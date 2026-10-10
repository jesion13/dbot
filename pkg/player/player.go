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

// framePacer paces opus frame sends to real-time (20ms/frame), matching
// disgo's own voice.AudioSender timing. We can't use AudioSender/
// SetOpusFrameProvider directly: it assumes exclusive ownership of the
// Conn, but music and sounds are two independent producers that both write
// to the same Conn (a sound can interrupt a song mid-stream) - handing
// ownership to a second provider silently abandons the first one, which
// is why using it caused the song to never resume after a sound played.
//
// A pacer belongs to exactly one producer goroutine and is never touched under
// the player lock: the sleep has to stay outside it.
type framePacer struct {
	lastFrameSent int64
}

func (fp *framePacer) wait() {
	if fp.lastFrameSent == 0 {
		fp.lastFrameSent = time.Now().UnixMilli()
		return
	}

	sleepTime := time.Duration(int64(voice.OpusFrameSizeMs) - (time.Now().UnixMilli() - fp.lastFrameSent))
	if sleepTime > 0 {
		time.Sleep(sleepTime * time.Millisecond)
	}
	if time.Now().UnixMilli() < fp.lastFrameSent+int64(voice.OpusFrameSizeMs)*3 {
		fp.lastFrameSent += int64(voice.OpusFrameSizeMs)
	} else {
		fp.lastFrameSent = time.Now().UnixMilli()
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
	speaking     bool
	speakingConn voice.Conn
	musicPacer   framePacer
	soundPacer   framePacer

	// soundQ is the FIFO of accepted sound requests. soundSignal wakes the sound
	// worker without ever blocking the enqueuing goroutine.
	soundQ      []SoundSequence
	soundSignal chan struct{}

	// playback seams; overridden in tests to drive the state machine without
	// ffmpeg/yt-dlp and a real connection.
	playTrackFn func(context.Context, uint64, Track) error
	playSoundFn func(context.Context, SoundTrack) error
}

func NewPlayer() *Player {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Player{
		ErrChan:     make(chan Err, errBuf),
		soundSignal: make(chan struct{}, 1),
		baseCtx:     ctx,
		baseCancel:  cancel,
	}
	p.gate = sync.NewCond(&p.mu)
	p.playTrackFn = p.playTrack
	p.playSoundFn = p.playSound

	// registered before it can be waited on
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.soundLoop()
	}()
	return p
}

// Close stops every worker, joins them, and releases the output. It is idempotent,
// accepts no further work, and never touches cached or backed-up media: those files
// are shared between guilds, playlists and sounds.
//
// The voice connection is left open, because the workers that were writing to it
// have stopped by the time Close returns. The caller owns closing it.
func (p *Player) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
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

	p.mu.Lock()
	if conn != nil {
		p.setSpeakingLocked(conn, false)
	}
	p.mu.Unlock()

	// every sender was a worker, and all of them have exited
	close(p.ErrChan)
	return nil
}

// SetVC installs the voice connection the player writes to. Workers pick the new
// connection up on their next frame; nothing is closed here.
func (p *Player) SetVC(conn voice.Conn, vcid snowflake.ID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conn = conn
	p.vcid = vcid
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

// awaitMusicWrite blocks until this generation owns the output, and reports false
// once it has lost it for good (replaced generation, cancelled context, close).
func (p *Player) awaitMusicWrite(gen uint64, ctx context.Context) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for !p.musicEligibleLocked(gen, ctx) {
		if p.closed || gen != p.gen || ctx.Err() != nil {
			return false
		}
		p.gate.Wait()
	}
	return true
}

// awaitSoundWrite is the sound equivalent: sounds are not suspended by music, they
// only stop when the player goes away.
func (p *Player) awaitSoundWrite(ctx context.Context) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for !p.soundEligibleLocked(ctx) {
		if p.closed || ctx.Err() != nil {
			return false
		}
		p.gate.Wait()
	}
	return true
}

// submitMusicFrame paces, re-checks eligibility and writes one music frame. The
// check and the write share the lock, so no state change can slip between them.
// errOutputLost means the frame was dropped and the caller should await again.
func (p *Player) submitMusicFrame(gen uint64, ctx context.Context, conn voice.Conn, frame []byte) error {
	p.musicPacer.wait()

	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.musicEligibleLocked(gen, ctx) {
		return errOutputLost
	}
	return p.writeFrameLocked(conn, frame)
}

// submitSoundFrame is submitMusicFrame for sounds.
func (p *Player) submitSoundFrame(ctx context.Context, conn voice.Conn, frame []byte) error {
	p.soundPacer.wait()

	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.soundEligibleLocked(ctx) {
		return errOutputLost
	}
	return p.writeFrameLocked(conn, frame)
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

// setSpeakingLocked changes discord's speaking flag, once per transition. A stale
// worker computing this from current state can never switch it off for the stream
// that took over from it.
func (p *Player) setSpeakingLocked(conn voice.Conn, on bool) {
	if conn != p.speakingConn {
		// a new connection has no known flag state of ours
		p.speakingConn = conn
		p.speaking = on
	} else if p.speaking == on {
		return
	} else {
		p.speaking = on
	}

	flags := voice.SpeakingFlags(0)
	if on {
		flags = voice.SpeakingFlagMicrophone
	}
	ctx, cancel := context.WithTimeout(context.Background(), speakingTimeout)
	defer cancel()
	if err := conn.SetSpeaking(ctx, flags); err != nil {
		slog.Warn("failed setting speaking", slog.Bool("speaking", on), slog.Any("error", err))
	}
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
			var submitErr error
			if music {
				if !p.awaitMusicWrite(gen, ctx) {
					// distinguish "cancelled" from "replaced": the caller decides whether
					// a lost output is worth reporting
					if err := ctx.Err(); err != nil {
						return err
					}
					return errOutputLost
				}
				submitErr = p.submitMusicFrame(gen, ctx, conn, frame)
			} else {
				if !p.awaitSoundWrite(ctx) {
					if err := ctx.Err(); err != nil {
						return err
					}
					return errOutputLost
				}
				submitErr = p.submitSoundFrame(ctx, conn, frame)
			}

			switch {
			case errors.Is(submitErr, errOutputLost):
				// the state changed while we were pacing; re-await on the next frame
				continue
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
