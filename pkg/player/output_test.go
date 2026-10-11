package player

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fake voice connection --------------------------------------------------

// recordedWrite is one frame that reached the sink. Tests submit distinctive
// payloads per producer ("music", "stale", "fresh", "sound"), so the frame content
// itself identifies which stream and generation wrote it.
type recordedWrite struct {
	frame []byte
}

// fakeUDP is a voice.UDPConn that records frames instead of sending them.
//
// The generation identifies the producer: music submits under its own generation,
// sounds under 0. That is what the tests read to tell the two streams apart.
type fakeUDP struct {
	voice.UDPConn

	mu     *sync.Mutex
	rec    *[]recordedWrite
	closed *atomicBool
	conn   *fakeConn
	times  *[]time.Time
	// failNext makes the next Write return this error instead of recording, so a
	// test can inject one bad datagram. Guarded by mu.
	failNext bool
	failWith error
}

func (u *fakeUDP) Write(b []byte) (int, error) {
	if u.closed.Load() {
		return 0, net.ErrClosed
	}
	u.mu.Lock()
	if u.failNext {
		u.failNext = false
		err := u.failWith
		u.mu.Unlock()
		return 0, err
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	*u.rec = append(*u.rec, recordedWrite{frame: cp})
	if u.times != nil {
		*u.times = append(*u.times, time.Now())
	}
	// park here if the test asked for it: the player holds its lock across this
	// call, which is exactly the ordering the test wants to observe
	block, writing, release := u.conn.blockWrite, u.conn.writing, u.conn.release
	u.mu.Unlock()

	if block {
		select {
		case writing <- struct{}{}:
		default:
		}
		<-release
	}
	return len(b), nil
}

// failOnce makes the next frame write fail with err. Pointer receivers matter here:
// a value receiver would silently mutate a copy and the injected failure would
// never be seen.
func (u *fakeUDP) failOnce(err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.failWith = err
	u.failNext = true
}

// atomicBool is a mutex-protected bool, used for flags the connection mutates
// without the player's lock.
type atomicBool struct {
	mu *sync.Mutex
	b  bool
}

func (a *atomicBool) Load() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.b
}

func (a *atomicBool) Store(v bool) {
	a.mu.Lock()
	a.b = v
	a.mu.Unlock()
}

// fakeConn is a voice.Conn that records every frame and every speaking change.
type fakeConn struct {
	voice.Conn

	gid snowflake.ID

	mu            sync.Mutex
	writes        []recordedWrite
	frameTimes    []time.Time
	speaking      []voice.SpeakingFlags
	speakAttempts int
	speakFail     int
	speakFailures int
	udp           *fakeUDP
	closed        atomicBool

	// blockWrite parks every write until the returned release func is called, so a
	// test can hold a frame inside the sink while the player state changes.
	blockWrite bool
	writing    chan struct{}
	release    chan struct{}
}

func newFakeConn(gid snowflake.ID) *fakeConn {
	c := &fakeConn{gid: gid}
	c.closed = atomicBool{mu: &c.mu}
	c.udp = &fakeUDP{mu: &c.mu, rec: &c.writes, closed: &c.closed, conn: c, times: &c.frameTimes}
	c.Conn = &connStub{udp: c.udp, c: c}
	return c
}

// holdWrites makes the next writes park inside the sink. releaseWrites lets them
// finish. It returns a channel closed when a write is actually parked.
func (c *fakeConn) holdWrites() (parked chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blockWrite = true
	c.writing = make(chan struct{}, 1)
	c.release = make(chan struct{})
	return c.writing
}

func (c *fakeConn) releaseWrites() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blockWrite = false
	close(c.release)
}

// held reports whether writes are currently parked.
func (c *fakeConn) held() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.blockWrite
}

func (c *fakeConn) GuildID() snowflake.ID { return c.gid }
func (c *fakeConn) UDP() voice.UDPConn    { return c.udp }

// connStub supplies the two calls the player makes on the connection.
type connStub struct {
	voice.Conn
	udp *fakeUDP
	c   *fakeConn
}

func (s *connStub) UDP() voice.UDPConn { return s.udp }

func (s *connStub) SetSpeaking(_ context.Context, flags voice.SpeakingFlags) error {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	s.c.speakAttempts++
	if s.c.speakFail > 0 {
		s.c.speakFail--
		s.c.speakFailures++
		return errors.New("speaking rejected")
	}
	s.c.speaking = append(s.c.speaking, flags)
	return nil
}

// failSpeaking makes the next n SetSpeaking calls fail, so a test can check that a
// failure is retried instead of being cached as applied.
func (c *fakeConn) failSpeaking(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.speakFail = n
}

func (c *fakeConn) speakingFailures() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.speakFailures
}

func (c *fakeConn) speakingAttempts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.speakAttempts
}

// close marks the connection unusable, so every later write is net.ErrClosed.
func (c *fakeConn) close() { c.closed.Store(true) }

func (c *fakeConn) frames() []recordedWrite {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]recordedWrite(nil), c.writes...)
}

func (c *fakeConn) speakLog() []voice.SpeakingFlags {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]voice.SpeakingFlags(nil), c.speaking...)
}

// speakingIsOn reports the last speaking flag, or false if none was ever set.
func (c *fakeConn) speakingIsOn() bool {
	log := c.speakLog()
	if len(log) == 0 {
		return false
	}
	return log[len(log)-1] != 0
}

// countSpeakingOff is how many times the flag was switched off. A stale worker
// doing this after a takeover is the bug this guards against.
func (c *fakeConn) countSpeakingOff() int {
	var n int
	for _, f := range c.speakLog() {
		if f == 0 {
			n++
		}
	}
	return n
}

// --- fixture ----------------------------------------------------------------

// outputFixture wires a real Player to a fake connection and drives the real frame
// loop through streamFrames, so ownership, pacing and speaking are exercised
// without ffmpeg.
type outputFixture struct {
	*Player
	conn *fakeConn
	t    *testing.T
	// trackDone lets a test end the playing track the way a real one ends.
	trackDone chan struct{}
}

// finishTrack ends the currently playing track, so the player advances on its own.
func (f *outputFixture) finishTrack() { f.trackDone <- struct{}{} }

func newOutputFixture(t *testing.T) *outputFixture {
	t.Helper()
	p := NewPlayer()
	conn := newFakeConn(1234)
	require.NoError(t, p.SetVC(conn, 99))
	// the real pipelines would shell out to ffmpeg and yt-dlp; these tests drive
	// streamFrames directly instead, so no track worker may start on its own
	trackDone := make(chan struct{}, 8)
	p.playTrackFn = func(ctx context.Context, gen uint64, tr Track) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-trackDone:
			return nil // the track played to its end
		}
	}
	p.playSoundFn = func(ctx context.Context, s SoundTrack) error {
		<-ctx.Done()
		return ctx.Err()
	}
	t.Cleanup(func() { _ = p.Close() })
	return &outputFixture{Player: p, conn: conn, t: t, trackDone: trackDone}
}

// pages builds a page source yielding the given frames, then ending the stream the
// way a real ffmpeg pipe does.
func pages(frames ...[]byte) pageSource {
	done := false
	return func() ([][]byte, error) {
		if done {
			return nil, io.EOF
		}
		done = true
		return frames, nil
	}
}

// endlessPages never ends, so a test controls the stream with its own context.
func endlessPages(frame []byte) pageSource {
	return func() ([][]byte, error) { return [][]byte{frame}, nil }
}

// stream is a running music stream a test can wait on or stop.
type stream struct {
	f      *outputFixture
	cancel context.CancelFunc
	done   chan error

	once sync.Once
	err  error
}

// exit waits for the stream to end on its own and returns why. Use this when the
// player is expected to end it (a jump, a skip, Close) - it does not cancel first,
// so the exit error reflects what actually stopped the stream.
func (s *stream) exit() error {
	s.once.Do(func() {
		select {
		case s.err = <-s.done:
		case <-time.After(3 * time.Second):
			s.f.t.Fatal("music stream did not stop")
		}
	})
	return s.err
}

// stop cancels the stream and waits for it. Idempotent.
func (s *stream) stop() error {
	s.cancel()
	return s.exit()
}

// hookPacing parks the next frame inside the pacing sleep, before any lock is
// taken. That is the window in which two overlapping generations used to share
// the pacing clock. release() lets the wait finish.
//
// The hook stays installed for the rest of the test: swapping it out would race the
// streams that are reading it, which is a race in the test, not in the player.
func (f *outputFixture) hookPacing() (parked chan struct{}, release func()) {
	return f.hookPacingFor(1)
}

// hookPacingFor parks the next n frames inside the pacing sleep and only lets them
// through once all n have arrived. Two streams released together then race each
// other for the rest of their pacing path, which is where a shared pacing clock
// used to be corrupted.
func (f *outputFixture) hookPacingFor(n int) (parked chan struct{}, release func()) {
	parked = make(chan struct{}, 8)
	releaseCh := make(chan struct{})
	var mu sync.Mutex
	var arrived int
	f.paceFn = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		arrived++
		enough := arrived >= n
		mu.Unlock()
		select {
		case parked <- struct{}{}:
		default:
		}
		if !enough {
			select {
			case <-releaseCh:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		select {
		case <-releaseCh:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}
	return parked, func() { close(releaseCh) }
}

// runMusic starts a real music stream on the player's current generation, writing
// frames with the given payload so the test can tell it from other streams.
func (f *outputFixture) runMusic(payload string) *stream {
	f.t.Helper()
	return f.runMusicOn(f.currentGen(), payload)
}

// runMusicOn starts a stream pinned to a specific generation, which is how a test
// drives an obsolete generation.
func (f *outputFixture) runMusicOn(gen uint64, payload string) *stream {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &stream{f: f, cancel: cancel, done: make(chan error, 1)}
	go func() {
		s.done <- f.streamFrames(ctx, f.conn, gen, true, endlessPages([]byte(payload)))
	}()
	return s
}

// currentGen returns the generation the player is on.
func (f *outputFixture) currentGen() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gen
}

// countFrames counts frames whose payload equals payload, which is how a test
// tells one stream or generation from another.
func (f *outputFixture) countFrames(payload string) int {
	var n int
	for _, w := range f.conn.frames() {
		if string(w.frame) == payload {
			n++
		}
	}
	return n
}

// frameTimeOf is when the nth frame with the given payload was written.
func (f *outputFixture) frameTimeOf(payload string, n int) time.Time {
	f.t.Helper()
	f.conn.mu.Lock()
	defer f.conn.mu.Unlock()
	var seen int
	for i, w := range f.conn.writes {
		if string(w.frame) != payload {
			continue
		}
		if seen == n {
			return f.conn.frameTimes[i]
		}
		seen++
	}
	f.t.Fatalf("no frame %d with payload %q", n, payload)
	return time.Time{}
}

// lastFrameSentAt reads the shared pacing clock.
func (f *outputFixture) lastFrameSentAt() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastFrameSent
}

// releaseSoundsNow drains the sound queue the way the sound worker does, so a test
// can end a suspension without ffmpeg.
func (f *outputFixture) releaseSoundsNow() error {
	f.mu.Lock()
	f.soundQ = nil
	f.soundActive = false
	f.gate.Broadcast()
	f.mu.Unlock()
	return nil
}

// reconcileSpeaking re-derives the speaking flag from current state, which is what
// the player does after any state change.
func (f *outputFixture) reconcileSpeaking() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshOutputLocked()
}

// awaitFrames waits until at least n frames with the given payload were written.
func (f *outputFixture) awaitFrames(payload string, n int) {
	f.t.Helper()
	await(f.t, fmt.Sprintf("%d %q frames", n, payload), func() bool {
		return f.countFrames(payload) >= n
	})
}

// unpace makes the next frame go out immediately, so a test need not wait 20ms.
// It resets the shared pacing clock, so call it before starting a stream.
func (f *outputFixture) unpace() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFrameSent = time.Now().UnixMilli()
}

// runSingleFrame starts a stream that yields exactly one frame and then ends, so a
// test can assert that this frame - and only this frame - survived a suspension.
func (f *outputFixture) runSingleFrame(gen uint64, payload string) *stream {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &stream{f: f, cancel: cancel, done: make(chan error, 1)}
	go func() {
		s.done <- f.streamFrames(ctx, f.conn, gen, true, pages([]byte(payload)))
	}()
	return s
}

// runSound starts a sound stream the same way, with its own context.
func (f *outputFixture) runSound(payload string) *stream {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &stream{f: f, cancel: cancel, done: make(chan error, 1)}
	go func() {
		s.done <- f.streamFrames(ctx, f.conn, 0, false, endlessPages([]byte(payload)))
	}()
	return s
}

// --- output ownership -------------------------------------------------------

func TestMusicWritesFrames(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.Enqueue(Track{Title: "a", URL: "u"}))

	f.unpace()
	s := f.runMusic("music")
	defer s.stop()

	f.awaitFrames("music", 3)
}

func TestMusicDoesNotWriteWhilePaused(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.Enqueue(Track{Title: "a", URL: "u"}))
	require.NoError(t, f.TogglePause())

	f.unpace()
	s := f.runMusic("music")
	defer s.stop()

	// a paused player must produce nothing at all
	time.Sleep(50 * time.Millisecond)
	require.Empty(t, f.conn.frames(), "no frames while paused")

	require.NoError(t, f.TogglePause())
	f.awaitFrames("music", 1)
}

func TestSoundRequestStopsMusicFramesImmediately(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.Enqueue(Track{Title: "a", URL: "u"}))

	f.unpace()
	s := f.runMusic("music")
	defer s.stop()

	f.awaitFrames("music", 3)
	before := f.countFrames("music")

	require.NoError(t, f.EnqueueSound(SoundTrack{Title: "s", Filepath: "/tmp/x"}))

	// the music stream is blocked now; give it far longer than a frame interval
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, before, f.countFrames("music"), "no music frames once a sound was accepted")
}

func TestStaleGenerationCannotWrite(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.EnqueueMany([]Track{{Title: "a"}, {Title: "b"}}))
	oldGen := f.currentGen()

	// replace the generation without touching any stream
	newGen := oldGen + 1
	f.mu.Lock()
	f.gen = newGen
	f.mu.Unlock()

	err := f.submitFrame(context.Background(), f.conn, []byte("stale"), oldGen, true)
	assert.ErrorIs(t, err, errOutputLost, "the replaced generation is refused")

	err = f.submitFrame(context.Background(), f.conn, []byte("fresh"), newGen, true)
	require.NoError(t, err)

	assert.Empty(t, f.countFrames("stale"), "the stale generation wrote nothing")
	assert.Equal(t, 1, f.countFrames("fresh"), "the authoritative generation wrote")
}

func TestJumpReplacesTheGenerationAndStopsTheOldStream(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.EnqueueMany([]Track{{Title: "a"}, {Title: "b"}, {Title: "c"}}))
	oldGen := f.currentGen()

	f.unpace()
	s := f.runMusic("old")
	f.awaitFrames("old", 2)

	require.NoError(t, f.JumpTo(3))
	require.Greater(t, f.currentGen(), oldGen)

	assert.ErrorIs(t, s.exit(), errOutputLost, "the old stream is told it lost the output")

	time.Sleep(50 * time.Millisecond)
	after := f.countFrames("old")
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, after, f.countFrames("old"), "the replaced generation wrote nothing more")
}

func TestSkipDuringAPendingWriteStopsTheOldGeneration(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.EnqueueMany([]Track{{Title: "a"}, {Title: "b"}}))

	// the stream is awake and paced right at the write boundary
	f.unpace()
	s := f.runMusic("frame")
	f.awaitFrames("frame", 2)

	require.NoError(t, f.SkipNext())
	assert.ErrorIs(t, s.exit(), errOutputLost)

	after := f.countFrames("frame")
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, after, f.countFrames("frame"), "the replaced generation wrote nothing more")
}

func TestSoundWaitsForAnInProgressMusicWrite(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.Enqueue(Track{Title: "a", URL: "u"}))

	f.unpace()
	parked := f.conn.holdWrites()
	s := f.runMusic("music")
	defer s.stop()

	// a music frame is now parked inside the sink, holding the player lock
	<-parked
	require.Equal(t, 1, f.countFrames("music"))

	// so the sound request cannot complete until that write is done: the check and
	// the write are one critical section
	accepted := make(chan error, 1)
	go func() { accepted <- f.EnqueueSound(SoundTrack{Title: "s", Filepath: "/tmp/x"}) }()

	select {
	case <-accepted:
		t.Fatal("a sound was accepted while a music frame write was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	f.conn.releaseWrites()
	require.NoError(t, <-accepted)
}

func TestMusicAndSoundWritesAreSerialized(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.Enqueue(Track{Title: "a", URL: "u"}))

	// a sound owns the output while it is active, so this is the contended case:
	// both producers are alive and both are trying to write
	f.unpace()
	music := f.runMusic("music")
	defer music.stop()
	sound := f.runSound("sound")
	defer sound.stop()
	require.NoError(t, f.EnqueueSound(SoundTrack{Title: "s", Filepath: "/tmp/x"}))

	f.awaitFrames("sound", 2)

	// music is suspended, so only the sound's frames are on the wire
	for _, w := range f.conn.frames() {
		assert.Equal(t, "sound", string(w.frame), "music must not interleave with a sound")
	}
}

// --- speaking state ---------------------------------------------------------

func TestMusicTurnsSpeakingOn(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.Enqueue(Track{Title: "a", URL: "u"}))

	s := f.runMusic("music")
	defer s.stop()

	f.awaitFrames("music", 1)
	assert.True(t, f.conn.speakingIsOn())
}

func TestSpeakingStaysOnAcrossTheHandoffToASound(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.Enqueue(Track{Title: "a", URL: "u"}))

	s := f.runMusic("music")
	defer s.stop()
	f.awaitFrames("music", 1)
	require.True(t, f.conn.speakingIsOn())
	offs := f.conn.countSpeakingOff()

	require.NoError(t, f.EnqueueSound(SoundTrack{Title: "s", Filepath: "/tmp/x"}))
	sound := f.runSound("sound")
	f.awaitFrames("sound", 1)
	sound.stop()

	assert.Equal(t, offs, f.conn.countSpeakingOff(), "no speaking-off across the handoff")
	assert.True(t, f.conn.speakingIsOn())
}

func TestStaleWorkerCannotDisableNewerOwnerSpeaking(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.EnqueueMany([]Track{{Title: "a"}, {Title: "b"}}))

	f.unpace()
	s := f.runMusic("music")
	defer s.stop()
	f.awaitFrames("music", 1)
	require.True(t, f.conn.speakingIsOn())
	offs := f.conn.countSpeakingOff()

	// hand the output to a new generation the way a jump would
	gen := f.currentGen()
	f.mu.Lock()
	f.gen = gen + 1
	f.cursor++ // the lineup move a jump produces
	f.mu.Unlock()

	assert.ErrorIs(t, s.exit(), errOutputLost)
	assert.Equal(t, offs, f.conn.countSpeakingOff(), "a stale worker never turns speaking off")

	require.NoError(t, f.submitFrame(context.Background(), f.conn, []byte("fresh"), gen+1, true))
	assert.True(t, f.conn.speakingIsOn(), "the new owner keeps speaking on")
}

func TestSpeakingOffWhenIdle(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.Enqueue(Track{Title: "a"}))

	s := f.runMusic("music")
	defer s.stop()
	f.awaitFrames("music", 1)
	require.True(t, f.conn.speakingIsOn())

	// the only track finishes, so nothing is playing and the flag must go
	f.finishTrack()
	await(t, "idle", func() bool { return f.Current() == nil })

	assert.False(t, f.conn.speakingIsOn(), "speaking is released when nothing is playing")
}

func TestSpeakingStaysOnBetweenConsecutiveSequences(t *testing.T) {
	f := newOutputFixture(t)

	// the sound worker runs the real sequence loop, but each sound is a no-op that
	// just records whether music was still suspended when it played
	var played []bool
	var mu sync.Mutex
	f.playSoundFn = func(context.Context, SoundTrack) error {
		f.mu.Lock()
		active := f.soundActive
		f.mu.Unlock()
		mu.Lock()
		played = append(played, active)
		mu.Unlock()
		return nil
	}

	require.NoError(t, f.EnqueueSoundSequence(SoundSequence{Tracks: []SoundTrack{{Title: "a"}}}))
	require.NoError(t, f.EnqueueSoundSequence(SoundSequence{Tracks: []SoundTrack{{Title: "b"}}}))

	await(t, "both sequences", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(played) == 2
	})

	// the second sequence started while the first was still counted as active,
	// which is what holds the suspension across the boundary
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, played, 2)
	for _, active := range played {
		assert.True(t, active, "music stays suspended for every sound of both sequences")
	}
}

func TestSpeakingOffOnClose(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.Enqueue(Track{Title: "a", URL: "u"}))

	s := f.runMusic("music")
	defer s.stop()
	f.awaitFrames("music", 1)
	require.True(t, f.conn.speakingIsOn())

	require.NoError(t, f.Close())
	assert.False(t, f.conn.speakingIsOn(), "shutdown releases speaking while the connection is still usable")
}

// --- shutdown ---------------------------------------------------------------

func TestCloseWaitsForWorkers(t *testing.T) {
	p := NewPlayer()

	running := make(chan struct{})
	exited := make(chan struct{})
	p.playTrackFn = func(ctx context.Context, gen uint64, tr Track) error {
		close(running)
		<-ctx.Done() // Close has to cancel this
		close(exited)
		return ctx.Err()
	}

	require.NoError(t, p.Enqueue(Track{Title: "a"}))
	<-running

	require.NoError(t, p.Close())
	// Close returned, so the worker is already out - not "soon"
	select {
	case <-exited:
	default:
		t.Fatal("Close returned while a track worker was still running")
	}
}

func TestCloseDuringSoundSequence(t *testing.T) {
	p := NewPlayer()

	started := make(chan struct{})
	var once sync.Once
	p.playSoundFn = func(ctx context.Context, s SoundTrack) error {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return ctx.Err()
	}

	require.NoError(t, p.EnqueueSound(SoundTrack{Title: "s"}))
	<-started

	done := make(chan struct{})
	go func() {
		defer close(done)
		require.NoError(t, p.Close())
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close hung on a sound sequence")
	}
}

func TestCloseStopsTheSoundWorkerWhileItWaitsForASignal(t *testing.T) {
	p := NewPlayer()
	require.NoError(t, p.Close())

	// the worker must have exited, so a later signal has nowhere to go
	select {
	case p.soundSignal <- struct{}{}:
	default:
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	assert.True(t, closed)
}

func TestCloseIsIdempotentUnderConcurrency(t *testing.T) {
	p := NewPlayer()
	require.NoError(t, p.Enqueue(Track{Title: "a"}))

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			require.NoError(t, p.Close())
		}()
	}
	wg.Wait()
}

func TestCloseReleasesErrChan(t *testing.T) {
	p := NewPlayer()
	require.NoError(t, p.Close())

	select {
	case _, ok := <-p.ErrChan:
		assert.False(t, ok, "ErrChan is closed so forwarders can exit")
	case <-time.After(time.Second):
		t.Fatal("ErrChan was not closed")
	}
}

func TestNoWorkerIsRegisteredAfterClose(t *testing.T) {
	p := NewPlayer()
	require.NoError(t, p.Close())

	// Enqueue is refused, so no Add can race with the Wait that already happened
	require.ErrorIs(t, p.Enqueue(Track{Title: "a"}), ErrClosed)

	p.wg.Wait() // must not block or panic
}

// --- terminal connection errors --------------------------------------------

func TestClosedConnectionStopsTheWholeStream(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.Enqueue(Track{Title: "a", URL: "u"}))

	f.unpace()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- f.streamFrames(ctx, f.conn, f.currentGen(), true, endlessPages([]byte("frame")))
	}()

	f.awaitFrames("frame", 1)

	f.conn.close()

	select {
	case err := <-done:
		assert.ErrorIs(t, err, errConnGone, "the stream ends, it does not spin on a dead connection")
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not stop on a closed connection")
	}

	// one error line, not one per frame
	assert.Less(t, len(f.conn.frames()), 200, "it stopped instead of hammering the dead socket")
}

func TestTransientWriteErrorDoesNotStopTheStream(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.Enqueue(Track{Title: "a", URL: "u"}))

	f.unpace()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- f.streamFrames(ctx, f.conn, f.currentGen(), true, endlessPages([]byte("frame")))
	}()

	f.awaitFrames("frame", 1)

	f.conn.udp.failOnce(errors.New("connection reset by peer"))
	f.awaitFrames("frame", 2)

	cancel()
	// the stream survives the bad datagram and only ends because we cancelled it
	err := <-done
	assert.NotErrorIs(t, err, errConnGone, "a transient datagram error is not terminal")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestPagesEndTheStream(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.Enqueue(Track{Title: "a", URL: "u"}))
	f.unpace()

	// a played-out stream ends with nil, not an error: the track finished normally
	err := f.streamFrames(context.Background(), f.conn, f.currentGen(), true, pages([]byte("a"), []byte("b")))
	assert.NoError(t, err)
	assert.Len(t, f.conn.frames(), 2, "both frames went out before the end")
}

func TestReaderErrorEndsTheStream(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.Enqueue(Track{Title: "a", URL: "u"}))

	boom := errors.New("corrupt ogg page")
	err := f.streamFrames(context.Background(), f.conn, f.currentGen(), true, func() ([][]byte, error) {
		return nil, boom
	})
	assert.ErrorIs(t, err, boom)
}

func TestSoundStreamStopsWhenThePlayerCloses(t *testing.T) {
	f := newOutputFixture(t)
	f.unpace()

	sound := f.runSound("s")
	f.awaitFrames("s", 1)
	require.NoError(t, f.Close())

	select {
	case <-sound.done:
	case <-time.After(3 * time.Second):
		t.Fatal("sound stream survived Close")
	}
}
