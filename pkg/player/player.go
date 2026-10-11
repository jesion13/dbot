package player

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"

	"dbot/pkg/ytdlp"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/pion/opus/pkg/oggreader"
)

const (
	// historyMax bounds the playback history.
	historyMax = 50
	// errBuf keeps an undrained ErrChan from blocking playback.
	errBuf = 16
	// speakingTimeout bounds the gateway call that updates discord's speaking flag,
	// which happens while the player lock is held.
	speakingTimeout = 5 * time.Second
)

var (
	ErrClosed          = errors.New("player is closed")
	ErrNothingPlaying  = errors.New("nothing is playing")
	ErrNoHistory       = errors.New("no previous track in history")
	ErrInvalidPosition = errors.New("position out of range")
	ErrNoVoiceConn     = errors.New("no voice connection")

	ErrEmptySoundSequence   = errors.New("sound sequence is empty")
	ErrSoundSequenceTooLong = errors.New("sound sequence too long")
	ErrSoundQueueFull       = errors.New("sound queue is full")
)

var (
	// errOutputLost means the submitter no longer owns the output: its generation
	// was replaced, the player closed, or its context was cancelled. The frame was
	// not written and the caller must stop or re-await.
	errOutputLost = errors.New("player: output is no longer owned by this stream")
	// errConnGone means the voice connection itself is gone, which is terminal for
	// the whole stream - not just for the frame that failed.
	errConnGone = errors.New("player: voice connection is closed")
)

// framePacer used to be per-producer mutable state, which two overlapping music
// generations could touch at once. Pacing is now a single clock owned by the
// player: lastFrameSent lives under mu and is advanced only when a frame is really
// written, so a generation that loses the output never reserves pacing time and
// cannot disturb its replacement. The sleep itself happens outside mu.
//
// The clock keeps opus frame sends at real time (20ms/frame), matching disgo's own
// voice.AudioSender timing. We can't use AudioSender/SetOpusFrameProvider directly:
// it assumes exclusive ownership of the Conn, but music and sounds are two
// independent producers that both write to the same Conn (a sound can interrupt a
// song mid-stream) - handing ownership to a second provider silently abandons the
// first one, which is why using it caused the song to never resume after a sound
// played.
const frameInterval = voice.OpusFrameSizeMs * time.Millisecond

// frameDelayLocked is how long the caller must wait before the next frame, so
// frames stay ~20ms apart no matter which producer is sending.
func (p *Player) frameDelayLocked(now int64) time.Duration {
	if p.lastFrameSent == 0 {
		return 0
	}
	d := frameInterval - time.Duration(now-p.lastFrameSent)*time.Millisecond
	if d < 0 {
		return 0
	}
	return d
}

// commitFrameLocked advances the pacing clock after a frame actually went out. A
// long gap (a sound, a pause) resets the clock instead of catching up, so
// playback never bursts to make up for time it lost.
func (p *Player) commitFrameLocked(now int64) {
	ms := int64(frameInterval / time.Millisecond)
	if p.lastFrameSent != 0 && now < p.lastFrameSent+3*ms {
		p.lastFrameSent += ms
		return
	}
	p.lastFrameSent = now
}

// sleepCtx waits for d, or returns early if the context is done. A pacing sleep
// must never outlive the generation that started it.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type Err struct {
	GID string
	Err error
}

// Player owns the music queue and the sound queue, and is the only writer to the
// voice connection both of them play into.
//
// Every mutable field below is guarded by mu, including the output state: a frame
// write happens under mu so that "may this stream write?" and "write it" cannot be
// separated by a state change. The pacer sleep and any worker wait happen outside mu.
//
// Lifecycle: Close marks the player closed under mu, cancels playback, then waits
// for every worker it started before returning. The bot must therefore close the
// player before closing the voice connection it handed over.
type Player struct {
	ytdlp.YTDLP

	ErrChan chan Err

	mu   sync.Mutex
	gate *sync.Cond
	// wg tracks every goroutine that can touch playback resources. Workers are
	// registered under mu, and Close stops registering them before it waits.
	wg sync.WaitGroup

	queue []*Track
	// current is the track the player is working on, whether it is streaming or
	// suspended behind a pause or a sound. nil means idle.
	current     *Track
	history     []*Track
	paused      bool
	soundActive bool
	closed      bool
	// gen invalidates the running track. It is what makes a stale worker's frames
	// and its completion event both no-ops.
	gen        uint64
	nextID     uint64
	cancel     context.CancelFunc
	baseCtx    context.Context
	baseCancel context.CancelFunc

	// voice connection state; only read through Conn and written through SetVC.
	conn voice.Conn
	vcid snowflake.ID

	// output ownership, only touched under mu
	// lastFrameSent is the shared pacing clock: the unix milli of the last frame
	// that actually went out. See the pacing comment above.
	lastFrameSent int64
	// speakingApplied is the flag state discord has confirmed. It is recorded only
	// after SetSpeaking succeeds, so a failure is retried instead of cached.
	speakingApplied bool
	speakingConn    voice.Conn
	speakWarned     bool

	// shutdown: the first Close caller coordinates, everyone else waits on
	// closeDone for that same shutdown to finish.
	closeDone chan struct{}
	closeErr  error

	// soundQ is the FIFO of accepted sound requests. soundSignal wakes the sound
	// worker without ever blocking the enqueuing goroutine.
	soundQ      []SoundSequence
	soundSignal chan struct{}

	// playback seams; overridden in tests to drive the state machine without
	// ffmpeg/yt-dlp and a real connection.
	playTrackFn func(context.Context, uint64, Track) error
	playSoundFn func(context.Context, SoundTrack) error
	// paceFn is the pacing wait. It is a seam so a test can hold a frame inside the
	// pacing window, which is where generations used to overlap.
	paceFn func(context.Context, time.Duration) error
}

func NewPlayer() *Player {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Player{
		ErrChan:     make(chan Err, errBuf),
		soundSignal: make(chan struct{}, 1),
		closeDone:   make(chan struct{}),
		baseCtx:     ctx,
		baseCancel:  cancel,
	}
	p.gate = sync.NewCond(&p.mu)
	p.playTrackFn = p.playTrack
	p.playSoundFn = p.playSound
	p.paceFn = sleepCtx

	// registered before it can be waited on
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.soundLoop()
	}()
	return p
}

// Close stops every worker, joins them, and releases the output. It never touches
// cached or backed-up media: those files are shared between guilds, playlists and
// sounds.
//
// Exactly one caller performs the shutdown; every other caller - concurrent or
// later - waits for that same shutdown to finish before returning. That matters
// because the caller owns the voice connection: a second Close returning early
// would let it close a connection that live workers are still writing to.
//
// The voice connection is left open, because the workers that were writing to it
// have stopped by the time Close returns.
func (p *Player) Close() error {
	p.mu.Lock()
	if p.closed {
		// somebody else is already shutting down: wait for their shutdown, not just
		// for the flag to be set
		done := p.closeDone
		p.mu.Unlock()
		<-done
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.closeErr
	}

	// this caller owns the shutdown from here on
	p.closed = true
	p.abortLocked()
	p.queue = nil
	p.history = nil
	p.current = nil
	p.soundQ = nil
	p.soundActive = false
	p.gate.Broadcast()
	conn := p.conn
	p.mu.Unlock()

	// cancel first so blocked reads and the sound worker wake up, then join them
	// without holding the lock they need to exit
	p.baseCancel()
	p.wg.Wait()

	// the connection is still ours to talk to, so release the speaking flag here
	p.mu.Lock()
	if conn != nil {
		p.setSpeakingLocked(conn, false)
	}
	p.mu.Unlock()

	// every sender was a worker and all of them have exited, so this is the only
	// close and nothing can send on it afterwards
	close(p.ErrChan)

	p.mu.Lock()
	p.closeErr = nil
	p.mu.Unlock()
	close(p.closeDone)
	return nil
}

// SetVC installs the voice connection the player writes to.
//
// Contract: one player uses one connection for its whole lifetime. SetVC is meant
// for a player that is not streaming yet - a worker captures the connection when
// its track starts and keeps it for that track, so changing the field mid-playback
// would not move it. Reconnecting therefore goes through replacing the player (see
// the bot's lifecycle operation), which stops the workers first.
//
// After Close it is refused: a shut-down player owns no output.
func (p *Player) SetVC(conn voice.Conn, vcid snowflake.ID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	p.conn = conn
	p.vcid = vcid
	return nil
}

// Conn returns a consistent snapshot of the connection and its channel. A non-nil
// conn is only guaranteed to stay open while the player is not being closed.
func (p *Player) Conn() (voice.Conn, snowflake.ID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn, p.vcid
}

// Current returns a copy of the playing track, nil when idle.
func (p *Player) Current() *Track {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil {
		return nil
	}
	c := *p.current
	return &c
}

// Enqueue appends one track. An idle player starts playing it immediately.
// It does not clear a manual pause: only TogglePause does that.
func (p *Player) Enqueue(t Track) error {
	return p.EnqueueMany([]Track{t})
}

// EnqueueMany appends tracks atomically, in the given order, and starts playback
// if the player was idle. An empty batch changes nothing.
func (p *Player) EnqueueMany(tracks []Track) error {
	if len(tracks) == 0 {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}

	for _, t := range tracks {
		p.nextID++
		t.ID = p.nextID
		p.queue = append(p.queue, &t)
	}
	if p.current == nil {
		p.startNextLocked()
	}
	return nil
}

// SkipNext stops the current track and starts the next one. No upcoming track
// leaves the player idle.
func (p *Player) SkipNext() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	if p.current == nil {
		return ErrNothingPlaying
	}
	p.pushHistoryLocked(*p.current)
	p.startNextLocked()
	return nil
}

// Previous returns to the most recently played track. The track that was playing
// goes back to the front of the queue, so it can be replayed with SkipNext.
func (p *Player) Previous() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	if len(p.history) == 0 {
		return ErrNoHistory
	}

	prev := p.history[len(p.history)-1]
	p.history = p.history[:len(p.history)-1]

	upcoming := p.queue
	if p.current != nil {
		p.pushHistoryLocked(*p.current)
		upcoming = append([]*Track{p.current}, p.queue...)
	}
	p.beginLocked(prev, upcoming)
	return nil
}

// JumpTo moves to a 1-based position, where 1 is the playing track while playing
// and the next queued track while idle. Tracks skipped over leave the queue; their
// cached files are untouched.
func (p *Player) JumpTo(position int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}

	seq := p.orderedLocked()
	if position < 1 || position > len(seq) {
		return fmt.Errorf("%w: %d (1-%d)", ErrInvalidPosition, position, len(seq))
	}
	if p.current != nil && position == 1 {
		return nil
	}

	target := seq[position-1]
	// full slice expression: the result must not share storage with seq's tail
	upcoming := append([]*Track(nil), seq[position:]...)
	if p.current != nil {
		p.pushHistoryLocked(*p.current)
	}
	p.beginLocked(target, upcoming)
	return nil
}

// TogglePause flips the user's own pause. Sound suspension is separate and never
// clears it.
func (p *Player) TogglePause() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	p.paused = !p.paused
	// wake the streams that are parked on the pause before re-deriving the flag
	p.gate.Broadcast()
	p.refreshOutputLocked()
	return nil
}

// Paused reports the user's own pause, ignoring sound suspension.
func (p *Player) Paused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paused
}

// QueueSnapshot returns a consistent copy of the whole queue state.
func (p *Player) QueueSnapshot() QueueSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snapshotLocked()
}

func (p *Player) snapshotLocked() QueueSnapshot {
	s := QueueSnapshot{
		Upcoming:  make([]Track, 0, len(p.queue)),
		History:   make([]Track, 0, len(p.history)),
		Paused:    p.paused,
		SoundWait: len(p.soundQ),
		Closed:    p.closed,
	}
	if p.current != nil {
		c := *p.current
		s.Current = &c
	}
	for _, t := range p.queue {
		s.Upcoming = append(s.Upcoming, *t)
	}
	for _, t := range p.history {
		s.History = append(s.History, *t)
	}
	return s
}

// startNextLocked promotes the head of the queue, or goes idle when it is empty.
func (p *Player) startNextLocked() {
	if len(p.queue) == 0 {
		p.abortLocked()
		p.current = nil
		p.queue = nil
		p.refreshOutputLocked()
		return
	}
	next := p.queue[0]
	// full slice expression so later appends cannot write into the old backing array
	p.beginLocked(next, p.queue[1:len(p.queue):len(p.queue)])
}

// beginLocked is the single entry point into playback: it invalidates whatever was
// playing and starts t with the given upcoming queue.
func (p *Player) beginLocked(t *Track, upcoming []*Track) {
	p.abortLocked()
	p.current = t
	p.queue = upcoming
	if t == nil {
		p.refreshOutputLocked()
		return
	}

	p.gen++
	gen := p.gen
	ctx, cancel := context.WithCancel(p.baseCtx)
	p.cancel = cancel
	p.refreshOutputLocked()

	// registered under mu, so Close cannot start waiting before this is visible
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.runTrack(gen, ctx, *t)
	}()
}

// abortLocked invalidates the running track. Bumping gen is what makes the old
// worker's frames and its completion event no-ops; cancel is what unblocks it.
func (p *Player) abortLocked() {
	p.gen++
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.gate.Broadcast()
}

// runTrack is the authoritative completion path of one track: whatever happens -
// end of file, decoder error, cancellation - the queue advances at most once.
func (p *Player) runTrack(gen uint64, ctx context.Context, t Track) {
	err := p.playTrackFn(ctx, gen, t)

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || gen != p.gen {
		return
	}
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, errOutputLost) {
		p.reportErr(p.playerErr("failed to play '%s'", t.Title, err))
	}
	p.pushHistoryLocked(t)
	p.startNextLocked()
}

func (p *Player) playerErr(msg string, vars ...any) Err {
	return Err{
		Err: fmt.Errorf("player: "+msg+": %w", vars...),
		GID: p.guildIDLocked(),
	}
}

func (p *Player) guildIDLocked() string {
	if p.conn == nil {
		return ""
	}
	return p.conn.GuildID().String()
}

// reportErr never blocks: the reader may be gone (the bot swaps the player out)
// and stalling the playback worker over a lost error report is worse than the log.
// Callers hold mu, which is also why Close can close the channel once the workers
// are gone.
func (p *Player) reportErr(e Err) {
	select {
	case p.ErrChan <- e:
	default:
		slog.Error("player error buffer full, dropping", slog.Any("error", e.Err))
	}
}

// --- output ownership -------------------------------------------------------
//
// Every opus frame and every speaking flag change happens under mu, so the two
// producers on one connection can never interleave a 20ms slice, and a replaced
// generation cannot emit a frame or clear the flag afterwards.

func (p *Player) musicEligibleLocked(gen uint64, ctx context.Context) bool {
	return !p.closed && gen == p.gen && ctx.Err() == nil &&
		p.current != nil && !p.paused && !p.soundActive
}

func (p *Player) soundEligibleLocked(ctx context.Context) bool {
	return !p.closed && ctx.Err() == nil
}

// streamAliveLocked reports whether this stream is still the authoritative one. It
// separates the two reasons a write cannot happen right now:
//
//	not alive  -> the generation was replaced, the context was cancelled or the
//	              player shut down: the frame is gone for good, drop it;
//	alive but  -> a pause or a sound is in the way: the frame is only temporarily
//	not eligible  blocked and must be preserved and retried.
func (p *Player) streamAliveLocked(gen uint64, ctx context.Context) bool {
	return !p.closed && gen == p.gen && ctx.Err() == nil
}

// submitFrame paces and writes one frame.
//
// It loops rather than dropping the frame, because losing it means losing audio:
// a sound or a pause that lands between decoding and writing parks this loop on the
// gate and retries the very same frame when the output becomes eligible again. The
// pacing sleep is cancelled with the context, and the pacing clock is only advanced
// after a frame really went out, so an obsolete generation never reserves a slot.
func (p *Player) submitFrame(ctx context.Context, conn voice.Conn, frame []byte, gen uint64, music bool) error {
	for {
		p.mu.Lock()
		eligible := p.frameEligibleLocked(gen, ctx, music)
		var delay time.Duration
		if eligible {
			delay = p.frameDelayLocked(time.Now().UnixMilli())
		}
		if !eligible {
			if !p.streamAliveLocked(gen, ctx) {
				p.mu.Unlock()
				return errOutputLost
			}
			// parked, not spinning: Wait releases mu and returns with it held
			p.gate.Wait()
			p.mu.Unlock()
			continue
		}
		p.mu.Unlock()

		if err := p.paceFn(ctx, delay); err != nil {
			return err
		}

		p.mu.Lock()
		if !p.frameEligibleLocked(gen, ctx, music) {
			// the state moved under us during the pacing sleep; decide again whether
			// this frame is still worth keeping
			if !p.streamAliveLocked(gen, ctx) {
				p.mu.Unlock()
				return errOutputLost
			}
			p.gate.Wait()
			p.mu.Unlock()
			continue
		}
		// eligibility and the write are one critical section, and only a frame that
		// really goes out advances the pacing clock
		p.commitFrameLocked(time.Now().UnixMilli())
		err := p.writeFrameLocked(conn, frame)
		p.mu.Unlock()
		return err
	}
}

// frameEligibleLocked is the single gate for both producers. music additionally
// depends on its generation; sounds only depend on the player still being alive.
func (p *Player) frameEligibleLocked(gen uint64, ctx context.Context, music bool) bool {
	if music {
		return p.musicEligibleLocked(gen, ctx)
	}
	return p.soundEligibleLocked(ctx)
}

// writeFrameLocked is the only place a frame reaches the voice connection, and the
// only place that turns speaking on. Callers must hold mu and must have checked
// eligibility.
func (p *Player) writeFrameLocked(conn voice.Conn, frame []byte) error {
	p.setSpeakingLocked(conn, true)
	_, err := conn.UDP().Write(frame)
	return err
}

// refreshOutputLocked re-derives the speaking flag after a state change, so a track
// that ends while suspended does not leave the bot talking over silence.
func (p *Player) refreshOutputLocked() {
	if p.conn == nil {
		return
	}
	p.setSpeakingLocked(p.conn, p.outputActiveLocked())
}

// outputActiveLocked is the single source of truth for "is anything playing right
// now". A sound keeps the output active across the handoff, so the flag never
// flickers between a sound and the music that resumes after it.
func (p *Player) outputActiveLocked() bool {
	if p.closed {
		return false
	}
	if p.soundActive {
		return true
	}
	return p.current != nil && !p.paused
}

// setSpeakingLocked reconciles discord's speaking flag with what the output is
// doing. Two properties matter:
//
//   - the cached state records only what discord confirmed, so a failed call is
//     retried on the next reconcile instead of being remembered as applied;
//   - the desired state is derived from current player state, so a stale worker
//     computing it can never switch the flag off for the stream that took over.
//
// Callers must hold mu, which also keeps two workers from racing the same flag.
func (p *Player) setSpeakingLocked(conn voice.Conn, on bool) {
	if conn == p.speakingConn && p.speakingApplied == on {
		return
	}

	flags := voice.SpeakingFlags(0)
	if on {
		flags = voice.SpeakingFlagMicrophone
	}
	ctx, cancel := context.WithTimeout(context.Background(), speakingTimeout)
	defer cancel()

	if err := conn.SetSpeaking(ctx, flags); err != nil {
		// a dead connection fails every time; warn once instead of on every frame
		if !p.speakWarned {
			p.speakWarned = true
			slog.Warn("failed setting speaking, will retry", slog.Bool("speaking", on), slog.Any("error", err))
		}
		return
	}

	p.speakingConn = conn
	p.speakingApplied = on
	p.speakWarned = false
}

// --- audio pipelines --------------------------------------------------------

// pageSource hands out one ogg page worth of opus frames.
type pageSource func() ([][]byte, error)

// streamFrames is the one frame loop both pipelines use. gen identifies the music
// generation; music decides whether it may still write, sounds only check that the
// player is alive.
func (p *Player) streamFrames(ctx context.Context, conn voice.Conn, gen uint64, music bool, next pageSource) error {
	// A stream parked on the gate only wakes on a broadcast, so cancellation needs
	// one too. The watcher lives exactly as long as this call.
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-watchCtx.Done():
			p.mu.Lock()
			p.gate.Broadcast()
			p.mu.Unlock()
		case <-watchDone:
		}
	}()

	for {
		frames, err := next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// the stream played out: a normal end, not a failure
				return nil
			}
			if !errors.Is(err, context.Canceled) {
				slog.Error("failed to parse page", slog.Any("error", err))
			}
			return err
		}

		for _, frame := range frames {
			submitErr := p.submitFrame(ctx, conn, frame, gen, music)

			switch {
			case errors.Is(submitErr, errOutputLost), errors.Is(submitErr, context.Canceled):
				// this generation lost the output: the frame is dropped and the stream
				// ends. A frame held back by a pause or a sound never gets here - it is
				// retried inside submitFrame.
				return submitErr
			case errors.Is(submitErr, net.ErrClosed):
				// the connection is gone: stop the whole stream, not just this page
				slog.Error("voice connection closed, stopping stream", slog.Any("error", submitErr))
				return errConnGone
			case submitErr != nil:
				// a single bad datagram must not cost us the whole track
				slog.Warn("dropped opus frame", slog.Any("error", submitErr))
			}
		}
	}
}

func (p *Player) fetch(link string) (Track, error) {
	meta, err := p.YTDLP.DownloadAudio(link)
	if err != nil {
		return Track{}, err
	}
	return Track{URL: meta.OriginalURL, Filepath: meta.Filepath, Title: meta.Title}, nil
}

// 128k measured fine offline (single 20ms frames, 320B) but sounded broken in
// discord; 64k is what shipped before. Change one knob at a time.
const opusBitrate = "64k"

// opusArgs is the single encoder configuration shared by music and sounds.
func opusArgs() []string {
	return []string{
		"-ar", "48000",
		"-ac", "2",
		// "-af", dynaudnorm,
		"-c:a", "libopus",
		"-frame_duration", "20",
		"-vbr", "off",
		"-b:a", opusBitrate,
		"-application", "audio",
		"-packet_loss", "0",
		"-f", "opus",
		"pipe:1",
	}
}

// playTrack streams one track into the voice connection. It reads the whole track
// even while suspended - suspending only stops the writes, so ffmpeg and yt-dlp
// stay alive and playback continues from the same byte on resume. gen is the
// generation this stream belongs to: frames are submitted under it and nowhere else.
func (p *Player) playTrack(ctx context.Context, gen uint64, t Track) error {
	conn, _ := p.Conn()
	if conn == nil {
		return ErrNoVoiceConn
	}

	src, cleanup, err := p.trackSource(ctx, t)
	if err != nil {
		return err
	}
	defer cleanup()

	ffmpegCMD := exec.CommandContext(ctx, "ffmpeg", append([]string{"-hide_banner", "-loglevel", "error", "-i", "-"}, opusArgs()...)...)
	ffmpegCMD.Stdin = src
	ffmpegCMD.Stderr = os.Stderr

	err = p.runFFmpeg(ctx, conn, gen, true, ffmpegCMD)
	waitErr := ffmpegCMD.Wait()
	if err == nil && waitErr != nil && ctx.Err() == nil {
		return fmt.Errorf("ffmpeg: %w", waitErr)
	}
	return err
}

// playSound renders one sound file into the same connection.
func (p *Player) playSound(ctx context.Context, s SoundTrack) error {
	conn, _ := p.Conn()
	if conn == nil {
		return ErrNoVoiceConn
	}

	filepath := s.Filepath
	if filepath == "" {
		fetched, err := p.fetch(s.URL)
		if err != nil {
			return fmt.Errorf("failed to download '%s': %w", s.URL, err)
		}
		filepath = fetched.Filepath
	}

	cmd := exec.CommandContext(ctx, "ffmpeg", append([]string{"-hide_banner", "-loglevel", "error", "-i", filepath}, opusArgs()...)...)
	cmd.Stderr = os.Stderr
	slog.Info("playSound", slog.String("cmd", cmd.String()))

	err := p.runFFmpeg(ctx, conn, 0, false, cmd)
	if waitErr := cmd.Wait(); err == nil && waitErr != nil && ctx.Err() == nil {
		return fmt.Errorf("ffmpeg: %w", waitErr)
	}
	return err
}

// trackSource opens the track's audio, downloading it first when there is no local
// file. The returned cleanup closes the pipe and reaps the downloader.
func (p *Player) trackSource(ctx context.Context, t Track) (io.ReadCloser, func(), error) {
	if t.Filepath == "" {
		dl := exec.CommandContext(ctx, "yt-dlp", "--no-playlist", "-f", "bestaudio", "-o", "-", t.URL)
		dl.Stderr = os.Stderr
		pipe, err := dl.StdoutPipe()
		if err != nil {
			return nil, nil, fmt.Errorf("yt-dlp stdout pipe: %w", err)
		}
		if err := dl.Start(); err != nil {
			return nil, nil, fmt.Errorf("yt-dlp start: %w", err)
		}
		return pipe, func() {
			pipe.Close()
			dl.Wait()
		}, nil
	}

	f, err := os.Open(t.Filepath)
	if err != nil {
		return nil, nil, fmt.Errorf("open '%s': %w", t.Filepath, err)
	}
	return f, func() { f.Close() }, nil
}

// newOggReader is the ogg reader constructor, indirected so a test can force the
// failure path - which is the one that has to kill ffmpeg - without having to find
// real input that produces it.
var newOggReader = func(r io.Reader) (pageReader, error) {
	reader, _, err := oggreader.NewWith(r)
	if err != nil {
		return nil, err
	}
	return reader, nil
}

type pageReader interface {
	ParseNextPage() ([][]byte, *oggreader.OggPageHeader, error)
}

// runFFmpeg starts ffmpeg and streams its opus frames. It never Waits: the caller
// reaps the process exactly once, on every path.
func (p *Player) runFFmpeg(ctx context.Context, conn voice.Conn, gen uint64, music bool, cmd *exec.Cmd) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg start: %w", err)
	}

	reader, err := newOggReader(stdout)
	if err != nil {
		// ffmpeg is still writing into a pipe nobody will read; killing it first is
		// what keeps Wait from blocking forever.
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		stdout.Close()
		return fmt.Errorf("oggreader: %w", err)
	}

	next := func() ([][]byte, error) {
		page, _, err := reader.ParseNextPage()
		if err != nil {
			return nil, err
		}
		return page, nil
	}
	return p.streamFrames(ctx, conn, gen, music, next)
}
