package player

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- pacing -----------------------------------------------------------------

func TestOverlappingGenerationsDoNotRaceThePacer(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.EnqueueMany([]Track{{Title: "a"}, {Title: "b"}}))

	// park two streams together, so both are inside the pacing path at once
	parked, release := f.hookPacingFor(2)

	oldGen := f.currentGen()
	old := f.runMusicOn(oldGen, "old")
	<-parked

	// a replacement becomes authoritative while the old one is still in that wait,
	// and starts producing frames of its own
	require.NoError(t, f.SkipNext())
	newGen := f.currentGen()
	require.NotEqual(t, oldGen, newGen)
	next := f.runMusicOn(newGen, "new")
	defer next.stop()
	<-parked

	// both pacing waits finish together: two generations now touch the pacing state
	// at the same instant
	release()

	assert.ErrorIs(t, old.exit(), errOutputLost, "the replaced generation ends")
	assert.Zero(t, f.countFrames("old"), "a replaced generation writes nothing")
}

func TestCancelledGenerationDoesNotCommitPacing(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.Enqueue(Track{Title: "a", URL: "u"}))
	gen := f.currentGen()

	parked, release := f.hookPacing()
	s := f.runMusicOn(gen, "stale")
	<-parked

	// cancel only the stream, leaving the player alive
	require.NoError(t, f.TogglePause()) // wakes the gate waiter
	f.mu.Lock()
	f.gen = gen + 1 // and retire the generation
	f.mu.Unlock()

	before := f.lastFrameSentAt()
	release()

	assert.ErrorIs(t, s.exit(), errOutputLost)
	assert.Equal(t, before, f.lastFrameSentAt(), "a cancelled generation never advances the pacing clock")
	assert.Zero(t, f.countFrames("stale"))
}

func TestPacingIsPreservedAcrossMusicAndSoundHandoff(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.Enqueue(Track{Title: "a", URL: "u"}))

	music := f.runMusic("music")
	defer music.stop()

	// let the clock settle, then measure real spacing between consecutive frames
	f.awaitFrames("music", 3)
	start := f.frameTimeOf("music", 2)
	f.awaitFrames("music", 4)
	gap := f.frameTimeOf("music", 3).Sub(start)

	assert.GreaterOrEqual(t, gap, frameInterval/2, "music frames must stay ~20ms apart")
}

func TestFramesStaySpacedWhenSoundsTakeOver(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.Enqueue(Track{Title: "a", URL: "u"}))

	music := f.runMusic("music")
	defer music.stop()
	f.awaitFrames("music", 3)

	require.NoError(t, f.EnqueueSound(SoundTrack{Title: "s", Filepath: "/tmp/x"}))
	sound := f.runSound("sound")
	defer sound.stop()

	f.awaitFrames("sound", 1)
	// the sound takes the same clock over; it must not burst a backlog of frames
	first := f.frameTimeOf("sound", 0)
	f.awaitFrames("sound", 2)
	assert.GreaterOrEqual(t, f.frameTimeOf("sound", 1).Sub(first), frameInterval/2)
}

// --- frame preservation -----------------------------------------------------

// blockedAtPacing parks a music generation inside its pacing wait, which is exactly
// where a pause or a sound can arrive after the frame was decoded.
func blockedAtPacing(t *testing.T, f *outputFixture) (release func(), music *stream) {
	t.Helper()
	parked, release := f.hookPacing()
	music = f.runMusic("held")
	<-parked
	return release, music
}

// blockedSingleFrameAtPacing is the same, for a stream that carries one frame only.
func blockedSingleFrameAtPacing(t *testing.T, f *outputFixture) (release func(), music *stream) {
	t.Helper()
	parked, release := f.hookPacing()
	music = f.runSingleFrame(f.currentGen(), "held")
	<-parked
	return release, music
}

func TestFrameBlockedBySoundIsEmittedAfterTheBacklogDrains(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.Enqueue(Track{Title: "a", URL: "u"}))

	release, music := blockedAtPacing(t, f)
	defer music.stop()

	// a sound lands between decoding the frame and writing it
	require.NoError(t, f.EnqueueSound(SoundTrack{Title: "s", Filepath: "/tmp/x"}))
	release()

	// the frame must still be pending, not dropped
	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, f.countFrames("held"), "the frame is held, not lost")

	// draining the sound releases it, and it is the very same frame
	require.NoError(t, f.releaseSoundsNow())
	f.awaitFrames("held", 1)
}

func TestFrameBlockedByPauseIsEmittedAfterResume(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.Enqueue(Track{Title: "a", URL: "u"}))

	release, music := blockedAtPacing(t, f)
	defer music.stop()

	require.NoError(t, f.TogglePause())
	release()

	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, f.countFrames("held"))

	require.NoError(t, f.TogglePause())
	f.awaitFrames("held", 1)
}

func TestNoFrameIsDroppedAcrossASuspensionBoundary(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.Enqueue(Track{Title: "a", URL: "u"}))

	// a single-frame page: exactly one frame can be lost if the retry is wrong
	release, music := blockedSingleFrameAtPacing(t, f)
	defer music.stop()

	require.NoError(t, f.EnqueueSound(SoundTrack{Title: "s", Filepath: "/tmp/x"}))
	release()
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, f.releaseSoundsNow())

	f.awaitFrames("held", 1)
	assert.Equal(t, 1, f.countFrames("held"), "the frame was delayed, never dropped")
}

func TestSupersededFrameIsNeverEmitted(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.EnqueueMany([]Track{{Title: "a"}, {Title: "b"}}))

	release, music := blockedAtPacing(t, f)
	defer music.stop()

	// the track is replaced while its frame waits at the pacing boundary
	require.NoError(t, f.SkipNext())
	release()

	assert.ErrorIs(t, music.exit(), errOutputLost)
	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, f.countFrames("held"), "a superseded frame is never written")
}

func TestCancellingABlockedWorkerDoesNotDeadlockShutdown(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.Enqueue(Track{Title: "a", URL: "u"}))

	// park the worker on the gate (suspended by a sound), then shut down
	require.NoError(t, f.EnqueueSound(SoundTrack{Title: "s", Filepath: "/tmp/x"}))

	done := make(chan struct{})
	go func() {
		defer close(done)
		require.NoError(t, f.Close())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung on a worker blocked at the gate")
	}
}

// --- concurrent Close -------------------------------------------------------

func TestConcurrentCloseWaitsForTheSameShutdown(t *testing.T) {
	p := NewPlayer()

	started := make(chan struct{})
	release := make(chan struct{})
	// deliberately ignores cancellation: it models a subprocess that takes a moment
	// to die, which is exactly what Close has to wait for
	p.playTrackFn = func(ctx context.Context, gen uint64, tr Track) error {
		close(started)
		<-release
		return ctx.Err()
	}
	require.NoError(t, p.Enqueue(Track{Title: "a"}))
	<-started

	// first Close: coordinates the shutdown and blocks on the worker
	first := make(chan error, 1)
	go func() { first <- p.Close() }()

	// wait until the shutdown is under way
	await(t, "shutdown to start", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.closed
	})

	// second Close: must NOT return while the first is still waiting
	second := make(chan error, 1)
	go func() { second <- p.Close() }()

	select {
	case <-second:
		t.Fatal("a second Close returned while the shutdown was still in progress")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	require.NoError(t, <-first)
	require.NoError(t, <-second, "the second caller gets the same shutdown result")
}

func TestCloseAfterCompletionIsStillIdempotent(t *testing.T) {
	p := NewPlayer()
	require.NoError(t, p.Close())
	require.NoError(t, p.Close())
	require.NoError(t, p.Close())
}

func TestConcurrentCloseWhileAWorkerReportsAnError(t *testing.T) {
	p := NewPlayer()

	started := make(chan struct{})
	release := make(chan struct{})
	p.playTrackFn = func(ctx context.Context, gen uint64, tr Track) error {
		close(started)
		<-release
		return errors.New("boom")
	}
	require.NoError(t, p.Enqueue(Track{Title: "a"}))
	<-started

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			require.NoError(t, p.Close())
		}()
	}
	close(release)
	wg.Wait()

	// ErrChan closes exactly once, and nothing can send on it afterwards: the worker's
	// buffered error is drained, then the channel ends
	var sent int
	for {
		select {
		case _, ok := <-p.ErrChan:
			if !ok {
				assert.GreaterOrEqual(t, sent, 0)
				return
			}
			sent++
		case <-time.After(2 * time.Second):
			t.Fatal("ErrChan was never closed")
		}
	}
}

// --- speaking state ---------------------------------------------------------

func TestFailedSpeakingActivationIsRetried(t *testing.T) {
	f := newOutputFixture(t)
	f.conn.failSpeaking(1) // the first activation attempt fails

	require.NoError(t, f.Enqueue(Track{Title: "a", URL: "u"}))
	s := f.runMusic("music")
	defer s.stop()

	f.awaitFrames("music", 1)
	// the failure was not cached as applied, so the next reconcile tried again and
	// this time succeeded
	assert.Equal(t, 1, f.conn.speakingFailures(), "the first attempt failed")
	assert.GreaterOrEqual(t, f.conn.speakingAttempts(), 2, "and it was retried")
	assert.True(t, f.conn.speakingIsOn(), "the retry converged")
}

func TestFailedSpeakingDeactivationIsRetried(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.Enqueue(Track{Title: "a", URL: "u"}))
	s := f.runMusic("music")
	defer s.stop()

	f.awaitFrames("music", 1)
	require.True(t, f.conn.speakingIsOn())

	// the release fails; the cached state must not claim it went through
	f.conn.failSpeaking(1)
	require.NoError(t, f.SkipNext())
	await(t, "idle", func() bool { return f.Current() == nil })

	assert.True(t, f.conn.speakingIsOn(), "the flag is still on, so the release really failed")
	assert.Equal(t, 1, f.conn.speakingFailures())

	// the next reconcile retries and this time succeeds
	f.reconcileSpeaking()
	assert.False(t, f.conn.speakingIsOn())
}

func TestSpeakingConvergesAfterRetries(t *testing.T) {
	f := newOutputFixture(t)
	f.conn.failSpeaking(2) // two failures in a row
	require.NoError(t, f.Enqueue(Track{Title: "a", URL: "u"}))
	s := f.runMusic("music")
	defer s.stop()

	f.awaitFrames("music", 1)
	f.awaitFrames("music", 2)

	assert.Equal(t, 2, f.conn.speakingFailures())
	assert.True(t, f.conn.speakingIsOn(), "eventually the flag is applied")
}

func TestStaleGenerationCannotDisableNewerSpeaking(t *testing.T) {
	f := newOutputFixture(t)
	require.NoError(t, f.Enqueue(Track{Title: "a", URL: "u"}))
	s := f.runMusic("music")
	defer s.stop()
	f.awaitFrames("music", 1)
	require.True(t, f.conn.speakingIsOn())

	offs := f.conn.countSpeakingOff()
	gen := f.currentGen()
	f.mu.Lock()
	f.gen = gen + 1
	f.current = &Track{Title: "b"}
	f.mu.Unlock()

	assert.ErrorIs(t, s.exit(), errOutputLost)
	assert.Equal(t, offs, f.conn.countSpeakingOff())
}

// --- SetVC contract ---------------------------------------------------------

func TestSetVCLifecycle(t *testing.T) {
	t.Run("assignment before playback", func(t *testing.T) {
		p := NewPlayer()
		t.Cleanup(func() { _ = p.Close() })
		conn := newFakeConn(1)
		require.NoError(t, p.SetVC(conn, 42))
		got, id := p.Conn()
		assert.Same(t, voice.Conn(conn), got)
		assert.EqualValues(t, 42, id)
	})

	t.Run("replacement while idle", func(t *testing.T) {
		p := NewPlayer()
		t.Cleanup(func() { _ = p.Close() })
		require.NoError(t, p.SetVC(newFakeConn(1), 1))
		require.NoError(t, p.SetVC(newFakeConn(2), 2))
		_, id := p.Conn()
		assert.EqualValues(t, 2, id, "conn and vcid move together")
	})

	t.Run("refused after shutdown", func(t *testing.T) {
		p := NewPlayer()
		require.NoError(t, p.Close())
		assert.ErrorIs(t, p.SetVC(newFakeConn(1), 1), ErrClosed)
	})

	t.Run("no worker keeps writing after replacement", func(t *testing.T) {
		f := newOutputFixture(t)
		first := f.conn
		second := newFakeConn(4242)

		require.NoError(t, f.Enqueue(Track{Title: "a", URL: "u"}))
		s := f.runMusic("music")

		// the documented contract: a connection change is a player replacement, not
		// a field update, so the old player finishes on the old connection
		require.NoError(t, f.SetVC(second, 7))
		f.awaitFrames("music", 1)
		assert.NotEmpty(t, first.frames(), "the running stream keeps its own connection")
		assert.Empty(t, second.frames(), "and never writes to the new one mid-track")

		s.stop()
	})
}
