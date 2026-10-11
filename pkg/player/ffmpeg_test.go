package player

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests exercise the real ffmpeg subprocess paths, because that is where the
// bugs live: a blocked Wait, an unreaped process, a pipe nobody drains. They are
// skipped when ffmpeg is absent so the rest of the suite still runs.

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
}

// newPipelinePlayer returns a player wired to a recording connection.
func newPipelinePlayer(t *testing.T) (*Player, *fakeConn) {
	t.Helper()
	p := NewPlayer()
	conn := newFakeConn(1234)
	require.NoError(t, p.SetVC(conn, 99))
	t.Cleanup(func() { _ = p.Close() })
	return p, conn
}

// silence is a tiny real wav file, so ffmpeg has valid input to encode.
func silence(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/silence.wav"
	// 1s of 48kHz mono silence as a minimal RIFF wav
	require.NoError(t, os.WriteFile(path, wavSilence(), 0o644))
	return path
}

func wavSilence() []byte {
	const (
		sampleRate = 48000
		channels   = 1
		bits       = 16
		seconds    = 1
	)
	dataLen := sampleRate * channels * bits / 8 * seconds

	buf := make([]byte, 0, 44+dataLen)
	buf = append(buf, "RIFF"...)
	buf = appendLE32(buf, uint32(36+dataLen))
	buf = append(buf, "WAVEfmt "...)
	buf = appendLE32(buf, 16)
	buf = appendLE16(buf, 1) // PCM
	buf = appendLE16(buf, channels)
	buf = appendLE32(buf, sampleRate)
	buf = appendLE32(buf, sampleRate*channels*bits/8)
	buf = appendLE16(buf, channels*bits/8)
	buf = appendLE16(buf, bits)
	buf = append(buf, "data"...)
	buf = appendLE32(buf, uint32(dataLen))
	return append(buf, make([]byte, dataLen)...)
}

func appendLE32(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

func appendLE16(b []byte, v uint16) []byte {
	return append(b, byte(v), byte(v>>8))
}

func TestPlaySoundFromFileWritesFrames(t *testing.T) {
	requireFFmpeg(t)
	p, conn := newPipelinePlayer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- p.playSound(ctx, SoundTrack{Title: "s", Filepath: silence(t)}) }()

	select {
	case err := <-done:
		require.NoError(t, err, "a sound renders end to end")
	case <-time.After(20 * time.Second):
		t.Fatal("playSound did not finish")
	}

	assert.NotEmpty(t, conn.frames(), "the sound produced frames")
	assert.True(t, conn.speakingIsOn() || len(conn.speakLog()) > 0)
}

func TestPlaySoundStopsOnContextCancel(t *testing.T) {
	requireFFmpeg(t)
	p, conn := newPipelinePlayer(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.playSound(ctx, SoundTrack{Title: "s", Filepath: silence(t)}) }()

	// wait until it is really running, then pull the plug
	await(t, "frames", func() bool { return len(conn.frames()) > 0 })
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("playSound survived context cancellation")
	}
}

// A path that exists but is not audio makes ffmpeg fail on the reader.
func TestUnreadableInputFailsCleanly(t *testing.T) {
	requireFFmpeg(t)
	p, conn := newPipelinePlayer(t)

	notAudio := t.TempDir() + "/not-audio.mp4"
	require.NoError(t, os.WriteFile(notAudio, []byte("this is definitely not opus"), 0o644))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- p.playSound(ctx, SoundTrack{Title: "bad", Filepath: notAudio}) }()

	select {
	case err := <-done:
		assert.Error(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("a failing ffmpeg blocked")
	}
	assert.NotNil(t, conn)
}

// runFFmpeg kills ffmpeg before returning when the ogg reader cannot be built.
// Without that kill, ffmpeg stays blocked writing into a pipe nobody will read and
// the caller's Wait never returns.
func TestReaderFailureKillsFFmpegBeforeWait(t *testing.T) {
	requireFFmpeg(t)
	p, _ := newPipelinePlayer(t)

	// force the ogg reader to fail, which is the path where ffmpeg is still writing
	// into a pipe nobody will read
	restore := stubOggReader(func(io.Reader) (pageReader, error) {
		return nil, errors.New("bad ogg header")
	})
	defer restore()

	// a real ffmpeg, fed enough data that it is still writing when the reader
	// fails. runFFmpeg owns StdoutPipe and Start, so the test only takes stdin.
	cmd := exec.CommandContext(context.Background(), "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "s16le", "-ar", "48000", "-ac", "2", "-i", "pipe:0",
		"-c:a", "libopus", "-frame_duration", "20", "-vbr", "off",
		"-b:a", opusBitrate, "-f", "opus", "pipe:1")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)

	runDone := make(chan error, 1)
	go func() { runDone <- p.runFFmpeg(context.Background(), nil, 0, false, cmd) }()

	feederDone := make(chan struct{})
	go func() {
		defer close(feederDone)
		chunk := make([]byte, 48000*4)
		for range 500 {
			if _, err := stdin.Write(chunk); err != nil {
				return
			}
		}
	}()
	defer func() {
		stdin.Close()
		<-feederDone
	}()

	select {
	case err := <-runDone:
		assert.ErrorContains(t, err, "bad ogg header")
	case <-time.After(20 * time.Second):
		t.Fatal("runFFmpeg blocked: ffmpeg was not killed after the reader failed")
	}

	// the caller reaps the process, and that Wait must return: ffmpeg is already
	// dead, so it cannot still be filling the pipe nobody reads
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case <-waitDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait blocked: ffmpeg was never killed, so it is still filling the pipe")
	}
}

// stubOggReader replaces the reader constructor for one test.
func stubOggReader(fn func(io.Reader) (pageReader, error)) func() {
	prev := newOggReader
	newOggReader = fn
	return func() { newOggReader = prev }
}

func TestPlayTrackWithMissingFileFailsCleanly(t *testing.T) {
	p, _ := newPipelinePlayer(t)

	err := p.playTrack(context.Background(), 1, Track{Title: "gone", Filepath: "/nonexistent/nope.opus"})
	assert.Error(t, err)
}

func TestPlayTrackWithoutVoiceConnectionFails(t *testing.T) {
	p := NewPlayer()
	t.Cleanup(func() { _ = p.Close() })

	err := p.playTrack(context.Background(), 1, Track{Title: "a"})
	assert.ErrorIs(t, err, ErrNoVoiceConn)
}

func TestEncoderArgsAreSharedByBothPipelines(t *testing.T) {
	requireFFmpeg(t)

	// music and sounds encode identically, so a clip saved for one plays in the
	// other and neither can drift
	cmd := exec.CommandContext(context.Background(), "ffmpeg",
		append([]string{"-hide_banner", "-loglevel", "error", "-i", silence(t)}, opusArgs()...)...)
	require.NoError(t, cmd.Start())
	require.NoError(t, cmd.Wait())
	assert.Contains(t, opusArgs(), "pipe:1")
	assert.Contains(t, opusArgs(), "-b:a")
	assert.Contains(t, opusArgs(), opusBitrate)
}
