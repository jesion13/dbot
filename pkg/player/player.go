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
	// errBuf keeps an undrained ErrChan from blocking playback.
	errBuf = 16
	// speakingTimeout bounds the gateway call that updates discord's speaking flag,
	// which happens while the player lock is held.
	speakingTimeout = 5 * time.Second
)

var (
	ErrClosed          = errors.New("player is closed")
	ErrNoPreviousTrack = errors.New("no earlier track in the lineup")
	// ErrNoNextTrack is only returned for an empty lineup. A non-empty lineup always
	// has somewhere to go, because an explicit next wraps around.
	ErrNoNextTrack     = errors.New("the queue is empty")
	ErrInvalidPosition = errors.New("position out of range")
	ErrNoVoiceConn     = errors.New("no voice connection")
	// ErrConnectionTaken means this player already owns a different connection and
	// will not swap it out from under its workers.
	ErrConnectionTaken = errors.New("player already has a voice connection")

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

// We can't use voice.AudioSender/SetOpusFrameProvider: it assumes exclusive ownership
// of the Conn, but music and sounds are two independent producers writing to the same
// Conn, and a second provider silently abandons the first - which is why the song
// never resumed after a sound played. So pacing is one player-owned clock instead.
const frameInterval = voice.OpusFrameSizeMs * time.Millisecond

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

// A long gap (a sound, a pause) resets the clock instead of catching up, so playback
// never bursts to make up for time it lost.
func (p *Player) commitFrameLocked(now int64) {
	ms := int64(frameInterval / time.Millisecond)
	if p.lastFrameSent != 0 && now < p.lastFrameSent+3*ms {
		p.lastFrameSent += ms
		return
	}
	p.lastFrameSent = now
}

// A pacing sleep must never outlive the generation that started it.
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

// Every mutable field below is guarded by mu, including the output state: a frame
// write happens under mu so that "may this stream write?" and "write it" cannot be
// separated by a state change. The pacer sleep and any worker wait happen outside mu.
//
// Close cancels playback and waits for every worker before returning, so the bot must
// close the player before closing the voice connection it handed over.
type Player struct {
	ytdlp.YTDLP

	ErrChan chan Err

	mu   sync.Mutex
	gate *sync.Cond
	// registered under mu, so Close stops registering workers before it waits
	wg sync.WaitGroup

	// appended only, so a 1-based position always means the same track for the life of
	// the player - including tracks that already played
	lineup []*Track
	cursor int // 0-based index of the playing entry, or -1 when idle
	// equals len(lineup) once played out, which is how "finished" is told apart from
	// "never started" - the only difference between resuming at appended entries and
	// replaying the old ones
	resumeIndex int
	paused      bool
	soundActive bool
	closed      bool
	// makes a stale worker's frames and its completion event no-ops
	gen        uint64
	nextID     uint64
	cancel     context.CancelFunc
	baseCtx    context.Context
	baseCancel context.CancelFunc

	// read through Conn, written through SetVC
	conn voice.Conn
	vcid snowflake.ID

	// output ownership, only touched under mu
	// unix milli of the last frame that actually went out; see the pacing note above
	lastFrameSent int64
	// only what discord confirmed, so a failed SetSpeaking is retried, not cached
	speakingApplied bool
	speakingConn    voice.Conn
	speakWarned     bool

	// shutdown: the first Close caller coordinates, everyone else waits on closeDone
	closeDone chan struct{}
	closeErr  error

	// soundSignal wakes the worker without ever blocking the enqueuing goroutine
	soundQ      []SoundSequence
	soundSignal chan struct{}

	// seams, overridden in tests to drive the state machine without ffmpeg/yt-dlp
	playTrackFn func(context.Context, uint64, Track) error
	playSoundFn func(context.Context, SoundTrack) error
	// lets a test hold a frame in the pacing window, where generations used to overlap
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
	p.cursor = -1
	p.gate = sync.NewCond(&p.mu)
	p.playTrackFn = p.playTrack
	p.playSoundFn = p.playSound
	p.paceFn = sleepCtx

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.soundLoop()
	}()
	return p
}

// Never touches cached or backed-up media: those files are shared between guilds,
// playlists and sounds. The voice connection is left open, because its writers have
// stopped by the time Close returns.
//
// Every caller but the first waits for that same shutdown before returning, or the
// owner of the voice connection could close one that live workers still write to.
func (p *Player) Close() error {
	p.mu.Lock()
	if p.closed {
		// wait for their shutdown, not just for the flag to be set
		done := p.closeDone
		p.mu.Unlock()
		<-done
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.closeErr
	}

	p.closed = true
	p.abortLocked()
	p.lineup = nil
	p.cursor = -1
	p.soundQ = nil
	p.soundActive = false
	p.gate.Broadcast()
	conn := p.conn
	p.mu.Unlock()

	// cancel first so blocked reads and the sound worker wake up, then join them
	// without holding the lock they need to exit
	p.baseCancel()
	p.wg.Wait()

	p.mu.Lock()
	if conn != nil {
		p.setSpeakingLocked(conn, false)
	}
	p.mu.Unlock()

	// every sender was a worker and all have exited, so nothing can send afterwards
	close(p.ErrChan)

	p.mu.Lock()
	p.closeErr = nil
	p.mu.Unlock()
	close(p.closeDone)
	return nil
}

// Setting the same connection again is a no-op, so a retry is harmless. A different
// one is refused with ErrConnectionTaken: a worker captures the connection when its
// track starts, so moving the field would leave playback writing to a connection this
// player no longer owns. Reconnecting goes through the bot's lifecycle, which closes
// the old player and its connection first.
func (p *Player) SetVC(conn voice.Conn, vcid snowflake.ID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	if p.conn != nil {
		if p.conn == conn {
			return nil // idempotent
		}
		return ErrConnectionTaken
	}
	p.conn = conn
	p.vcid = vcid
	return nil
}

// A non-nil conn is only guaranteed to stay open while the player is not closing.
func (p *Player) Conn() (voice.Conn, snowflake.ID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn, p.vcid
}

// Current returns a copy of the playing track, nil when idle.
func (p *Player) Current() *Track {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cursor < 0 || p.cursor >= len(p.lineup) {
		return nil
	}
	c := *p.lineup[p.cursor]
	return &c
}

// EnqueueMany without the atomic batch. Does not clear a manual pause.
func (p *Player) Enqueue(t Track) error {
	return p.EnqueueMany([]Track{t})
}

// Appends atomically and starts playback if the player is idle. Never renumbers what
// is already there, and never restarts a lineup already played to the end.
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
		p.lineup = append(p.lineup, &t)
	}
	if p.cursor < 0 {
		// position 1 on a fresh player, the first appended entry on one that already
		// played its lineup out - never a replay of position 1
		if p.resumeIndex < len(p.lineup) {
			p.cursor = p.resumeIndex
		}
		p.startLocked()
	}
	return nil
}

// SkipNext moves to the next absolute position, wrapping from the last entry back to
// position 1. Nothing is ever removed: the entry it leaves stays at its own position.
func (p *Player) SkipNext() error {
	_, _, err := p.SkipNextTracks()
	return err
}

// SkipNextTracks is SkipNext plus the two tracks involved, captured under the same
// lock as the cursor move. A caller that wants to report what it did must not read
// the lineup separately around SkipNext: playback can advance on its own in between,
// and it would then report the wrong track. From is nil when the player was idle, so
// nothing was actually skipped.
func (p *Player) SkipNextTracks() (from, to *Track, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, nil, ErrClosed
	}
	if len(p.lineup) == 0 {
		return nil, nil, ErrNoNextTrack
	}

	if p.cursor >= 0 && p.cursor < len(p.lineup) {
		f := *p.lineup[p.cursor]
		from = &f
	}
	// deliberately different from a track finishing on its own, which stops
	p.cursor = (p.cursor + 1) % len(p.lineup)
	d := *p.lineup[p.cursor]
	to = &d

	p.startLocked()
	return from, to, nil
}

// Idle at the end of a lineup resumes from the last entry. Entries are never removed,
// reordered or renumbered.
func (p *Player) Previous() error {
	_, err := p.PreviousTrack()
	return err
}

// PreviousTrack is Previous plus the track it selected, captured under the same lock
// as the cursor move. A caller that reports the move must use this rather than read
// the lineup afterwards: playback can advance on its own in between.
func (p *Player) PreviousTrack() (*Track, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrClosed
	}

	switch {
	case p.cursor > 0:
		p.cursor--
	case p.cursor == 0:
		return nil, ErrNoPreviousTrack
	case len(p.lineup) > 0:
		p.cursor = len(p.lineup) - 1
	default:
		return nil, ErrNoPreviousTrack
	}

	t := *p.lineup[p.cursor]
	p.startLocked()
	return &t, nil
}

// 1-based, the same numbering QueueSnapshot reports and the text commands display.
// Entries before and after it are left exactly where they are.
func (p *Player) JumpTo(position int) error {
	_, err := p.JumpToTrack(position)
	return err
}

// JumpToTrack is JumpTo plus the track it selected, captured under the same lock as
// the cursor move. A caller that reports the move must use this rather than read the
// lineup afterwards: playback can advance on its own in between.
func (p *Player) JumpToTrack(position int) (*Track, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrClosed
	}
	if position < 1 || position > len(p.lineup) {
		return nil, fmt.Errorf("%w: %d (1-%d)", ErrInvalidPosition, position, len(p.lineup))
	}
	if p.cursor == position-1 {
		t := *p.lineup[p.cursor]
		return &t, nil // already there
	}

	p.cursor = position - 1
	t := *p.lineup[p.cursor]
	p.startLocked()
	return &t, nil
}

// The lineup itself is never touched.
func (p *Player) startLocked() {
	if p.cursor < 0 || p.cursor >= len(p.lineup) {
		p.abortLocked()
		p.cursor = -1
		p.refreshOutputLocked()
		return
	}

	entry := *p.lineup[p.cursor]
	p.abortLocked()
	p.gen++
	gen := p.gen
	ctx, cancel := context.WithCancel(p.baseCtx)
	p.cancel = cancel
	p.refreshOutputLocked()

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.runTrack(gen, ctx, entry)
	}()
}

// The finished entry stays where it is.
func (p *Player) advanceLocked() {
	if p.cursor+1 < len(p.lineup) {
		p.cursor++
		p.resumeIndex = p.cursor
	} else {
		p.cursor = -1
		p.resumeIndex = len(p.lineup)
	}
	p.startLocked()
}

// Sound suspension is separate and never clears it.
func (p *Player) TogglePause() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	p.paused = !p.paused
	p.gate.Broadcast()
	p.refreshOutputLocked()
	return nil
}

// Ignoring sound suspension.
func (p *Player) Paused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paused
}

// A consistent copy of the complete lineup and where playback sits in it.
func (p *Player) QueueSnapshot() QueueSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()

	snap := QueueSnapshot{
		Tracks:    make([]Track, 0, len(p.lineup)),
		Paused:    p.paused,
		SoundWait: len(p.soundQ),
		Closed:    p.closed,
	}
	if p.cursor >= 0 && p.cursor < len(p.lineup) {
		snap.CurrentIndex = p.cursor + 1
	}
	for _, t := range p.lineup {
		snap.Tracks = append(snap.Tracks, *t)
	}
	return snap
}

// The authoritative completion path: whatever happens, the cursor advances at most
// once.
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
	p.advanceLocked()
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

// Never blocks: the reader may be gone (the bot swaps the player out) and stalling
// the playback worker over a lost error report is worse than the log.
func (p *Player) reportErr(e Err) {
	select {
	case p.ErrChan <- e:
	default:
		slog.Error("player error buffer full, dropping", slog.Any("error", e.Err))
	}
}

// --- output ownership -------------------------------------------------------
// Frames and speaking-flag changes happen under mu, so the two producers on one
// connection can never interleave a 20ms slice, and a replaced generation cannot emit
// a frame or clear the flag afterwards.

func (p *Player) musicEligibleLocked(gen uint64, ctx context.Context) bool {
	return !p.closed && gen == p.gen && ctx.Err() == nil &&
		p.cursor >= 0 && p.cursor < len(p.lineup) && !p.paused && !p.soundActive
}

func (p *Player) soundEligibleLocked(ctx context.Context) bool {
	return !p.closed && ctx.Err() == nil
}

// Bumping gen makes the old worker a no-op; cancel is what unblocks it.
func (p *Player) abortLocked() {
	p.gen++
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.gate.Broadcast()
}

// Separates the two reasons a write cannot happen: not alive means the frame is gone
// for good (drop it), alive-but-ineligible means only a pause or sound is in the way
// (keep it and retry).
func (p *Player) streamAliveLocked(gen uint64, ctx context.Context) bool {
	return !p.closed && gen == p.gen && ctx.Err() == nil
}

// Loops rather than dropping the frame, because losing it means losing audio: a sound
// or pause landing between decoding and writing parks this loop and retries the same
// frame. The clock advances only after a frame really went out, so an obsolete
// generation never reserves a slot.
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
			// Wait releases mu and returns with it held
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
			// state moved under us during the sleep; decide again
			if !p.streamAliveLocked(gen, ctx) {
				p.mu.Unlock()
				return errOutputLost
			}
			p.gate.Wait()
			p.mu.Unlock()
			continue
		}
		// eligibility and the write are one critical section
		err := p.writeFrameLocked(conn, frame)
		if err == nil {
			// a failed write must not reserve the slot or the next frames would burst
			p.commitFrameLocked(time.Now().UnixMilli())
		}
		p.mu.Unlock()
		return err
	}
}

// The single gate for both producers.
func (p *Player) frameEligibleLocked(gen uint64, ctx context.Context, music bool) bool {
	if music {
		return p.musicEligibleLocked(gen, ctx)
	}
	return p.soundEligibleLocked(ctx)
}

// The only place a frame reaches the connection. Callers must hold mu and must have
// checked eligibility.
func (p *Player) writeFrameLocked(conn voice.Conn, frame []byte) error {
	p.setSpeakingLocked(conn, true)
	_, err := conn.UDP().Write(frame)
	return err
}

// Re-derives the flag after a state change, so a track that ends while suspended does
// not leave the bot talking over silence.
func (p *Player) refreshOutputLocked() {
	if p.conn == nil {
		return
	}
	p.setSpeakingLocked(p.conn, p.outputActiveLocked())
}

// A sound keeps the output active across the handoff, so the flag never flickers
// between a sound and the music resuming after it.
func (p *Player) outputActiveLocked() bool {
	if p.closed {
		return false
	}
	if p.soundActive {
		return true
	}
	return p.cursor >= 0 && p.cursor < len(p.lineup) && !p.paused
}

// Reconciles discord's speaking flag with the output. The cache records only what
// discord confirmed, so a failure is retried; the desired state is derived from
// current player state, so a stale worker cannot switch the flag off for the stream
// that took over. Callers must hold mu, which also keeps two workers off the flag.
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

func (p *Player) streamFrames(ctx context.Context, conn voice.Conn, gen uint64, music bool, next pageSource) error {
	// a parked stream only wakes on a broadcast, so cancellation needs one too
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
				// frame dropped and the stream ends; a frame held back by a pause or a
				// sound never gets here - it is retried inside submitFrame
				return submitErr
			case errors.Is(submitErr, net.ErrClosed):
				slog.Error("voice connection closed, stopping stream", slog.Any("error", submitErr))
				return errConnGone
			case submitErr != nil:
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

// 128k measured fine offline but sounded broken in discord; 64k is what shipped.
// Change one knob at a time.
const opusBitrate = "64k"
const loudnorm = "loudnorm=I=-16:TP=-1.5:LRA=11"

func opusArgs() []string {
	return []string{
		"-ar", "48000",
		"-ac", "1",
		"-af", loudnorm,
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

// Reads the whole track even while suspended - suspending only stops the writes, so
// ffmpeg and yt-dlp stay alive and playback resumes from the same byte.
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

// Indirected so a test can force the failure path, which has to kill ffmpeg, without
// finding real input that produces it.
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

// It never Waits: the caller reaps the process exactly once, on every path.
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
		// ffmpeg still writes into a pipe nobody reads; killing it first keeps Wait from
		// blocking forever
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
