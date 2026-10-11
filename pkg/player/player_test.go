package player

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fake replaces the ffmpeg/yt-dlp pipelines so the queue state machine can be
// driven deterministically: a track plays until its channel is closed.
type fake struct {
	*Player
	t *testing.T

	mu       sync.Mutex
	release  map[uint64]chan struct{}
	errs     map[uint64]error
	srelease map[string]chan struct{}
	serrs    map[string]error

	started  chan uint64
	sstarted chan string
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{
		Player:   NewPlayer(),
		t:        t,
		release:  map[uint64]chan struct{}{},
		errs:     map[uint64]error{},
		srelease: map[string]chan struct{}{},
		serrs:    map[string]error{},
		started:  make(chan uint64, 256),
		sstarted: make(chan string, 256),
	}

	f.playTrackFn = func(ctx context.Context, gen uint64, tr Track) error {
		f.started <- tr.ID
		ch := make(chan struct{})
		f.mu.Lock()
		f.release[tr.ID] = ch
		f.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.errs[tr.ID]
	}
	f.playSoundFn = func(ctx context.Context, s SoundTrack) error {
		f.sstarted <- s.Title
		ch := make(chan struct{})
		f.mu.Lock()
		f.srelease[s.Title] = ch
		f.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.serrs[s.Title]
	}

	t.Cleanup(func() { _ = f.Close() })
	return f
}

// endTrack finishes a playing track. It waits for the track to register, so the test
// never races the player's goroutine.
func (f *fake) endTrack(id uint64) {
	f.mu.Lock()
	if ch, ok := f.release[id]; ok {
		delete(f.release, id)
		f.mu.Unlock()
		close(ch)
		return
	}
	f.mu.Unlock()
	await(f.t, fmt.Sprintf("track %d to start", id), func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		_, ok := f.release[id]
		return ok
	})
	f.endTrack(id)
}

func (f *fake) failTrack(id uint64, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[id] = err
}

func (f *fake) endSound(title string) {
	f.mu.Lock()
	if ch, ok := f.srelease[title]; ok {
		delete(f.srelease, title)
		f.mu.Unlock()
		close(ch)
		return
	}
	f.mu.Unlock()
	await(f.t, "sound "+title+" to start", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		_, ok := f.srelease[title]
		return ok
	})
	f.endSound(title)
}

func (f *fake) failSound(title string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.serrs[title] = err
}

func (f *fake) nextSound(t *testing.T) string {
	t.Helper()
	select {
	case s := <-f.sstarted:
		return s
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a sound to start")
		return ""
	}
}

func (f *fake) currentTitle() string {
	c := f.Current()
	if c == nil {
		return ""
	}
	return c.Title
}

func (f *fake) soundSuspended() bool {
	f.Player.mu.Lock()
	defer f.Player.mu.Unlock()
	return f.Player.soundActive
}

// await waits for cond, failing the test instead of hanging.
func await(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func track(title string) Track { return Track{Title: title, URL: "https://x/" + title} }

// --- snapshot helpers -------------------------------------------------------

// lineupTitles is every entry in absolute order.
func lineupTitles(s QueueSnapshot) []string {
	out := make([]string, 0, len(s.Tracks))
	for _, t := range s.Tracks {
		out = append(out, t.Title)
	}
	return out
}

// playing is the absolute position currently playing, 0 when idle.
func playing(p *fake) int { return p.QueueSnapshot().CurrentIndex }

func awaitPlaying(t *testing.T, p *fake, title string) {
	t.Helper()
	await(t, title, func() bool { return p.currentTitle() == title })
}

// --- lineup retention -------------------------------------------------------

func TestLineupKeepsEveryTrackForThePlayersLifetime(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b"), track("c")}))
	awaitPlaying(t, p, "a")

	// the full lineup is visible from the start, with the cursor on position 1
	s := p.QueueSnapshot()
	assert.Equal(t, []string{"a", "b", "c"}, lineupTitles(s))
	assert.Equal(t, 1, s.CurrentIndex)

	p.endTrack(1)
	awaitPlaying(t, p, "b")
	p.endTrack(2)
	awaitPlaying(t, p, "c")

	// a played track is still there, at its original absolute position
	s = p.QueueSnapshot()
	assert.Equal(t, []string{"a", "b", "c"}, lineupTitles(s), "playback never removes entries")
	assert.Equal(t, 3, s.CurrentIndex, "positions do not shift as playback advances")
}

func TestReachingTheEndKeepsTheLineup(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b")}))
	awaitPlaying(t, p, "a")
	p.endTrack(1)
	awaitPlaying(t, p, "b")
	p.endTrack(2)

	await(t, "idle at the end", func() bool { return playing(p) == 0 })
	s := p.QueueSnapshot()
	assert.Equal(t, []string{"a", "b"}, lineupTitles(s), "an idle player still shows its lineup")
}

func TestAppendingNeverRenumbers(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b")}))
	awaitPlaying(t, p, "a")
	p.endTrack(1)
	awaitPlaying(t, p, "b")

	require.NoError(t, p.EnqueueMany([]Track{track("c")}))
	s := p.QueueSnapshot()
	assert.Equal(t, []string{"a", "b", "c"}, lineupTitles(s))
	// a is still position 1
	assert.Equal(t, "a", s.Entry(1).Title)
	assert.Equal(t, "b", s.Entry(2).Title)
}

func TestDuplicateTracksAreSeparateEntries(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueMany([]Track{track("a"), track("a"), track("a")}))

	s := p.QueueSnapshot()
	require.Len(t, s.Tracks, 3)
	assert.Equal(t, "a", s.Entry(1).Title)
	assert.Equal(t, "a", s.Entry(2).Title)
	assert.Equal(t, "a", s.Entry(3).Title)
	assert.NotEqual(t, s.Entry(1).ID, s.Entry(2).ID, "each entry keeps its own identity")
	assert.NotEqual(t, s.Entry(2).ID, s.Entry(3).ID)
}

func TestEnqueueAfterTheEndPlaysTheNewEntries(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b")}))
	awaitPlaying(t, p, "a")
	p.endTrack(1)
	awaitPlaying(t, p, "b")
	p.endTrack(2)
	await(t, "idle", func() bool { return playing(p) == 0 })

	// appending after the lineup finished continues where it left off
	require.NoError(t, p.EnqueueMany([]Track{track("x"), track("y")}))
	awaitPlaying(t, p, "x")

	s := p.QueueSnapshot()
	assert.Equal(t, []string{"a", "b", "x", "y"}, lineupTitles(s))
	assert.Equal(t, 3, s.CurrentIndex, "playback resumes at the newly appended position, not position 1")
}

func TestLongSessionKeepsEveryLineupEntry(t *testing.T) {
	p := newFake(t)
	const total = 70

	tracks := make([]Track, 0, total)
	for i := range total {
		tracks = append(tracks, track(fmt.Sprint(i)))
	}
	require.NoError(t, p.EnqueueMany(tracks))

	for i := range total {
		awaitPlaying(t, p, fmt.Sprint(i))
		p.endTrack(uint64(i + 1))
	}
	await(t, "idle", func() bool { return playing(p) == 0 })

	s := p.QueueSnapshot()
	assert.Len(t, s.Tracks, total, "a long session keeps every entry")
	assert.Equal(t, fmt.Sprint(0), s.Entry(1).Title, "the oldest entry is still addressable")
}

// --- navigation -------------------------------------------------------------

// An explicit next always moves somewhere: it wraps around the lineup. That is
// deliberately different from a track finishing on its own, which stops at the end.
func TestSkipNext(t *testing.T) {
	t.Run("moves to the next absolute position", func(t *testing.T) {
		p := newFake(t)
		require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b"), track("c")}))
		awaitPlaying(t, p, "a")

		require.NoError(t, p.SkipNext())
		awaitPlaying(t, p, "b")
		assert.Equal(t, 2, playing(p))
		assert.Equal(t, []string{"a", "b", "c"}, lineupTitles(p.QueueSnapshot()))
	})

	t.Run("off the end it wraps to position 1", func(t *testing.T) {
		p := newFake(t)
		require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b")}))
		awaitPlaying(t, p, "a")
		require.NoError(t, p.SkipNext())
		awaitPlaying(t, p, "b")

		require.NoError(t, p.SkipNext())
		awaitPlaying(t, p, "a")
		assert.Equal(t, 1, playing(p))
		assert.Equal(t, []string{"a", "b"}, lineupTitles(p.QueueSnapshot()), "wrapping renumbers nothing")
	})

	t.Run("idle at the end starts position 1", func(t *testing.T) {
		p := newFake(t)
		require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b")}))
		awaitPlaying(t, p, "a")
		p.endTrack(1)
		awaitPlaying(t, p, "b")
		p.endTrack(2)
		await(t, "idle at the end", func() bool { return playing(p) == 0 })

		require.NoError(t, p.SkipNext())
		awaitPlaying(t, p, "a")
		assert.Equal(t, 1, playing(p))
	})

	t.Run("an empty lineup has nothing to wrap to", func(t *testing.T) {
		p := newFake(t)
		assert.ErrorIs(t, p.SkipNext(), ErrNoNextTrack)
	})
}

func TestPrevious(t *testing.T) {
	t.Run("selects the preceding absolute position", func(t *testing.T) {
		p := newFake(t)
		require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b"), track("c")}))
		awaitPlaying(t, p, "a")
		p.endTrack(1)
		awaitPlaying(t, p, "b")

		require.NoError(t, p.Previous())
		awaitPlaying(t, p, "a")
		assert.Equal(t, 1, playing(p))
		assert.Equal(t, []string{"a", "b", "c"}, lineupTitles(p.QueueSnapshot()), "nothing is removed")
	})

	t.Run("from position 1 it fails without changing state", func(t *testing.T) {
		p := newFake(t)
		require.NoError(t, p.Enqueue(track("a")))
		awaitPlaying(t, p, "a")

		assert.ErrorIs(t, p.Previous(), ErrNoPreviousTrack)
		assert.Equal(t, "a", p.currentTitle())
		assert.Equal(t, 1, playing(p))
	})

	t.Run("idle at the end resumes from the last entry", func(t *testing.T) {
		p := newFake(t)
		require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b")}))
		awaitPlaying(t, p, "a")
		p.endTrack(1)
		awaitPlaying(t, p, "b")
		p.endTrack(2)
		await(t, "idle", func() bool { return playing(p) == 0 })

		require.NoError(t, p.Previous())
		awaitPlaying(t, p, "b")
		assert.Equal(t, 2, playing(p))
	})

	t.Run("follows absolute adjacency after a jump", func(t *testing.T) {
		p := newFake(t)
		require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b"), track("c"), track("d")}))
		awaitPlaying(t, p, "a")

		// jump to 4, then previous must be 3 - not wherever playback came from
		require.NoError(t, p.JumpTo(4))
		awaitPlaying(t, p, "d")
		require.NoError(t, p.Previous())
		awaitPlaying(t, p, "c")
		assert.Equal(t, 3, playing(p))
	})
}

func TestJumpToAbsolutePositions(t *testing.T) {
	t.Run("backwards to a played track", func(t *testing.T) {
		p := newFake(t)
		require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b"), track("c")}))
		awaitPlaying(t, p, "a")
		p.endTrack(1)
		awaitPlaying(t, p, "b")
		p.endTrack(2)
		awaitPlaying(t, p, "c")

		require.NoError(t, p.JumpTo(1))
		awaitPlaying(t, p, "a")
		s := p.QueueSnapshot()
		assert.Equal(t, 1, s.CurrentIndex)
		assert.Equal(t, []string{"a", "b", "c"}, lineupTitles(s), "the lineup is intact")
	})

	t.Run("forwards past the end of the unplayed suffix", func(t *testing.T) {
		p := newFake(t)
		require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b"), track("c"), track("d"), track("e")}))
		awaitPlaying(t, p, "a")
		p.endTrack(1)
		p.endTrack(2)
		awaitPlaying(t, p, "c")

		require.NoError(t, p.JumpTo(5))
		awaitPlaying(t, p, "e")
		assert.Equal(t, 5, playing(p))
		assert.Equal(t, 5, p.QueueSnapshot().Len())
	})

	t.Run("current position is a no-op", func(t *testing.T) {
		p := newFake(t)
		require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b")}))
		awaitPlaying(t, p, "a")

		require.NoError(t, p.JumpTo(1))
		time.Sleep(20 * time.Millisecond)
		assert.Equal(t, "a", p.currentTitle())
		assert.Equal(t, 1, playing(p))
	})

	t.Run("out of range leaves everything intact", func(t *testing.T) {
		p := newFake(t)
		require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b")}))
		awaitPlaying(t, p, "a")

		for _, bad := range []int{0, -1, 3, 99} {
			assert.ErrorIs(t, p.JumpTo(bad), ErrInvalidPosition)
		}
		assert.Equal(t, "a", p.currentTitle(), "a failed jump does not interrupt playback")
		assert.Equal(t, []string{"a", "b"}, lineupTitles(p.QueueSnapshot()))
	})

	t.Run("the replaced generation cannot advance the cursor", func(t *testing.T) {
		p := newFake(t)
		require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b"), track("c")}))
		awaitPlaying(t, p, "a")

		// the old track ends right as the jump replaces it
		p.endTrack(1)
		require.NoError(t, p.JumpTo(3))
		awaitPlaying(t, p, "c")

		time.Sleep(50 * time.Millisecond)
		assert.Equal(t, 3, playing(p), "the stale completion did not advance the cursor again")
	})
}

func TestNaturalCompletionAdvancesExactlyOnce(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b"), track("c")}))
	awaitPlaying(t, p, "a")

	p.endTrack(1)
	awaitPlaying(t, p, "b")
	p.endTrack(2)
	awaitPlaying(t, p, "c")
	p.endTrack(3)
	await(t, "idle at the end", func() bool { return playing(p) == 0 })

	assert.Equal(t, 3, p.QueueSnapshot().Len())
}

func TestTrackFailureStillAdvances(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b")}))
	awaitPlaying(t, p, "a")
	p.failTrack(1, errors.New("boom"))

	p.endTrack(1)
	awaitPlaying(t, p, "b")
	assert.Equal(t, 2, playing(p), "the queue did not get stuck on the failed track")
}

// --- snapshot ---------------------------------------------------------------

func TestSnapshotIsACopy(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b")}))
	awaitPlaying(t, p, "a")

	s := p.QueueSnapshot()
	s.Tracks[0].Title = "hacked"
	s.Tracks = append(s.Tracks, track("injected"))

	fresh := p.QueueSnapshot()
	require.Len(t, fresh.Tracks, 2, "a snapshot cannot grow the live lineup")
	assert.Equal(t, "a", fresh.Entry(1).Title)
}

func TestCurrentIsACopy(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.Enqueue(track("a")))
	awaitPlaying(t, p, "a")

	c := p.Current()
	c.Title = "hacked"
	assert.Equal(t, "a", p.currentTitle())
}

func TestEntryOutOfRange(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.Enqueue(track("a")))
	s := p.QueueSnapshot()
	assert.Nil(t, s.Entry(0))
	assert.Nil(t, s.Entry(2))
	assert.NotNil(t, s.Entry(1))
	assert.Equal(t, 1, s.Len())
}

// --- pause ------------------------------------------------------------------

func TestEnqueuePreservesManualPause(t *testing.T) {
	t.Run("single track", func(t *testing.T) {
		p := newFake(t)
		require.NoError(t, p.TogglePause())
		require.NoError(t, p.Enqueue(track("a")))
		require.True(t, p.Paused(), "queueing a track must not resume paused music")
	})

	t.Run("batch", func(t *testing.T) {
		p := newFake(t)
		require.NoError(t, p.TogglePause())
		require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b")}))
		require.True(t, p.Paused())
		require.Equal(t, 2, p.QueueSnapshot().Len(), "the batch still landed")
	})

	t.Run("enqueue while sound suspended", func(t *testing.T) {
		p := newFake(t)
		require.NoError(t, p.Enqueue(track("a")))
		require.NoError(t, p.EnqueueSound(SoundTrack{Title: "boom"}))
		p.nextSound(t)
		require.True(t, p.soundSuspended())

		require.NoError(t, p.Enqueue(track("b")))
		require.True(t, p.soundSuspended(), "sound suspension survives an enqueue")
		require.False(t, p.Paused(), "an enqueue does not create a manual pause")

		p.endSound("boom")
		await(t, "sounds drained", func() bool { return p.QueueSnapshot().SoundWait == 0 })
		require.False(t, p.Paused())
	})

	t.Run("a drained sound backlog does not clear a manual pause", func(t *testing.T) {
		p := newFake(t)
		require.NoError(t, p.TogglePause())
		require.NoError(t, p.EnqueueSound(SoundTrack{Title: "boom"}))
		p.nextSound(t)
		p.endSound("boom")
		await(t, "sounds drained", func() bool { return !p.soundSuspended() })
		require.True(t, p.Paused())
	})

	t.Run("navigation does not toggle the pause", func(t *testing.T) {
		p := newFake(t)
		require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b")}))
		awaitPlaying(t, p, "a")

		require.NoError(t, p.TogglePause())
		require.NoError(t, p.SkipNext())
		require.NoError(t, p.Previous())
		require.Error(t, p.JumpTo(99))
		require.True(t, p.Paused())
	})

	t.Run("explicit resume clears it", func(t *testing.T) {
		p := newFake(t)
		require.NoError(t, p.Enqueue(track("a")))
		awaitPlaying(t, p, "a")

		require.NoError(t, p.TogglePause())
		require.NoError(t, p.TogglePause())
		require.False(t, p.Paused())
		require.False(t, p.soundSuspended())
	})
}

func TestManualPauseIsIndependentOfSounds(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.Enqueue(track("a")))
	awaitPlaying(t, p, "a")

	require.NoError(t, p.TogglePause())
	require.True(t, p.Paused())
	require.NoError(t, p.TogglePause())
	require.False(t, p.Paused())

	// a sound must not clear a manual pause
	require.NoError(t, p.TogglePause())
	require.NoError(t, p.EnqueueSound(SoundTrack{Title: "boom"}))
	p.nextSound(t)
	p.endSound("boom")
	await(t, "sound drained", func() bool { return p.QueueSnapshot().SoundWait == 0 })
	require.True(t, p.Paused())
}

// --- sounds -----------------------------------------------------------------

func TestSoundsDoNotTouchTheMusicQueue(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.Enqueue(track("song")))
	require.NoError(t, p.EnqueueSound(SoundTrack{Title: "boom"}))
	p.endSound("boom")

	s := p.QueueSnapshot()
	assert.Equal(t, []string{"song"}, lineupTitles(s), "sounds never enter the music lineup")
	assert.Equal(t, 1, s.CurrentIndex)
}

func TestSoundQueueFIFO(t *testing.T) {
	p := newFake(t)
	first := SoundSequence{Tracks: []SoundTrack{{Title: "a1"}, {Title: "a2"}}}
	second := SoundSequence{Tracks: []SoundTrack{{Title: "b1"}, {Title: "b2"}, {Title: "b3"}}}

	require.NoError(t, p.EnqueueSoundSequence(first))
	require.NoError(t, p.EnqueueSoundSequence(second))

	require.Equal(t, "a1", p.nextSound(t))
	p.endSound("a1")
	require.Equal(t, "a2", p.nextSound(t))
	p.endSound("a2")

	// nothing from the second sequence may start before the first one finished
	require.Equal(t, "b1", p.nextSound(t))
	p.endSound("b1")
	require.Equal(t, "b2", p.nextSound(t))
	p.endSound("b2")
	require.Equal(t, "b3", p.nextSound(t))
	p.endSound("b3")

	await(t, "sound backlog drained", func() bool { return !p.soundSuspended() })
	require.Zero(t, p.QueueSnapshot().SoundWait)
}

func TestMusicStaysSuspendedBetweenQueuedSequences(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.Enqueue(track("song")))
	require.NoError(t, p.EnqueueSoundSequence(SoundSequence{Tracks: []SoundTrack{{Title: "a"}}}))
	require.NoError(t, p.EnqueueSoundSequence(SoundSequence{Tracks: []SoundTrack{{Title: "b"}}}))

	require.True(t, p.soundSuspended())
	require.Equal(t, "a", p.nextSound(t))
	p.endSound("a")
	require.Equal(t, "b", p.nextSound(t))
	require.True(t, p.soundSuspended())
	p.endSound("b")

	await(t, "suspension released", func() bool { return !p.soundSuspended() })
	require.Equal(t, "song", p.currentTitle())
	require.False(t, p.Paused())
}

func TestSingleSoundIsAOneItemSequence(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueSound(SoundTrack{Title: "one"}))
	require.Equal(t, "one", p.nextSound(t))
	p.endSound("one")
	await(t, "drained", func() bool { return !p.soundSuspended() })
}

func TestEnqueueSoundSequenceValidation(t *testing.T) {
	p := newFake(t)
	require.ErrorIs(t, p.EnqueueSoundSequence(SoundSequence{}), ErrEmptySoundSequence)

	tooLong := make([]SoundTrack, MaxSoundsPerSequence+1)
	require.ErrorIs(t, p.EnqueueSoundSequence(SoundSequence{Tracks: tooLong}), ErrSoundSequenceTooLong)

	var rejected error
	for i := range maxSoundRequests + 8 {
		if err := p.EnqueueSound(SoundTrack{Title: fmt.Sprint(i)}); err != nil {
			rejected = err
		}
	}
	require.ErrorIs(t, rejected, ErrSoundQueueFull)
	require.LessOrEqual(t, p.QueueSnapshot().SoundWait, maxSoundRequests)
}

func TestEnqueueSoundSequenceCopiesInput(t *testing.T) {
	p := newFake(t)
	seq := SoundSequence{Tracks: []SoundTrack{{Title: "kept"}}}
	require.NoError(t, p.EnqueueSoundSequence(seq))

	seq.Tracks[0].Title = "changed"
	seq.Tracks = append(seq.Tracks, SoundTrack{Title: "injected"})

	require.Equal(t, "kept", p.nextSound(t))
	p.endSound("kept")
}

func TestSoundFailureDoesNotStrandMusic(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueSoundSequence(SoundSequence{Tracks: []SoundTrack{
		{Title: "bad"}, {Title: "good"},
	}}))
	p.failSound("bad", errors.New("no such file"))

	require.Equal(t, "bad", p.nextSound(t))
	p.endSound("bad")
	require.Equal(t, "good", p.nextSound(t))
	p.endSound("good")

	await(t, "suspension released", func() bool { return !p.soundSuspended() })
}

// --- lifecycle --------------------------------------------------------------

func TestCloseIsIdempotent(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.Enqueue(track("a")))
	require.NoError(t, p.EnqueueSound(SoundTrack{Title: "boom"}))
	p.nextSound(t)

	require.NoError(t, p.Close())
	require.NoError(t, p.Close())

	require.True(t, p.QueueSnapshot().Closed)
	require.ErrorIs(t, p.Enqueue(track("b")), ErrClosed)
	require.ErrorIs(t, p.EnqueueMany([]Track{track("b")}), ErrClosed)
	require.ErrorIs(t, p.SkipNext(), ErrClosed)
	require.ErrorIs(t, p.Previous(), ErrClosed)
	require.ErrorIs(t, p.JumpTo(1), ErrClosed)
	require.ErrorIs(t, p.TogglePause(), ErrClosed)
	require.ErrorIs(t, p.EnqueueSound(SoundTrack{Title: "x"}), ErrClosed)
	require.ErrorIs(t, p.EnqueueSoundSequence(SoundSequence{Tracks: []SoundTrack{{Title: "x"}}}), ErrClosed)
}

func TestCloseNeverDeletesCachedFiles(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "cached.opus")
	require.NoError(t, os.WriteFile(shared, []byte("media"), 0o644))

	p := newFake(t)
	require.NoError(t, p.EnqueueMany([]Track{{Title: "cached", Filepath: shared}, track("next")}))
	awaitPlaying(t, p, "cached")

	require.NoError(t, p.SkipNext())
	require.NoError(t, p.Close())

	_, err := os.Stat(shared)
	require.NoError(t, err)
}

func TestConcurrentEnqueueKeepsEveryTrack(t *testing.T) {
	p := newFake(t)
	const workers, each = 8, 20

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for range each {
				if err := p.Enqueue(track(fmt.Sprint(w))); err != nil {
					t.Errorf("enqueue: %v", err)
				}
			}
		}(w)
	}
	wg.Wait()

	assert.Equal(t, workers*each, p.QueueSnapshot().Len())
}

func TestConcurrentNavigationIsRaceFree(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueMany(make([]Track, 200)))
	require.NoError(t, p.Close())

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.Enqueue(track("x"))
			_ = p.JumpTo(2)
			_ = p.SkipNext()
			_ = p.Previous()
			_ = p.QueueSnapshot()
			_ = p.TogglePause()
		}()
	}
	wg.Wait()
}

func TestConcurrentCompletionVersusSkip(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueMany(make([]Track, 50)))

	var wg sync.WaitGroup
	for id := uint64(1); id <= 50; id++ {
		wg.Add(2)
		go func(id uint64) { defer wg.Done(); p.endTrack(id) }(id)
		go func() { defer wg.Done(); _ = p.SkipNext() }()
	}
	wg.Wait()

	// whatever the interleaving was, the lineup is still whole
	assert.Equal(t, 50, p.QueueSnapshot().Len())
}

func TestPlaybackWithoutVoiceConnectionStillAdvances(t *testing.T) {
	p := NewPlayer()
	t.Cleanup(func() { _ = p.Close() })
	require.NoError(t, p.EnqueueMany([]Track{{Title: "a"}, {Title: "b"}}))

	await(t, "advanced past both", func() bool {
		s := p.QueueSnapshot()
		return len(s.Tracks) == 2 && s.CurrentIndex == 0
	})
	assert.Equal(t, []string{"a", "b"}, lineupTitles(p.QueueSnapshot()))
	assert.Nil(t, p.Current(), "the player is idle, not stuck")
	assert.Equal(t, 2, len(p.ErrChan), "each failed track was reported once")
}

// --- navigation after a jump -----------------------------------------------

func TestNavigationAfterJumpUsesAbsoluteNeighbours(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueMany([]Track{
		track("a"), track("b"), track("c"), track("d"), track("e"),
	}))
	awaitPlaying(t, p, "a")

	require.NoError(t, p.JumpTo(5))
	awaitPlaying(t, p, "e")

	// previous from 5 is 4, then 3 - the lineup is the source of truth
	require.NoError(t, p.Previous())
	awaitPlaying(t, p, "d")
	assert.Equal(t, 4, playing(p))

	require.NoError(t, p.Previous())
	awaitPlaying(t, p, "c")
	assert.Equal(t, 3, playing(p))
	assert.Equal(t, []string{"a", "b", "c", "d", "e"}, lineupTitles(p.QueueSnapshot()))
}

func TestNextFromTheLastPositionWrapsToOne(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueMany([]Track{
		track("a"), track("b"), track("c"), track("d"), track("e"),
	}))
	awaitPlaying(t, p, "a")

	require.NoError(t, p.JumpTo(5))
	awaitPlaying(t, p, "e")

	// 5 is the last entry, so next wraps to position 1
	require.NoError(t, p.SkipNext())
	awaitPlaying(t, p, "a")
	assert.Equal(t, 1, playing(p))
	assert.Equal(t, 5, p.QueueSnapshot().Len(), "wrapping never removes an entry")
}

// Natural completion stops at the end; an explicit next starts the lineup over.
func TestNaturalCompletionStopsButNextRestarts(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b")}))
	awaitPlaying(t, p, "a")
	p.endTrack(1)
	awaitPlaying(t, p, "b")
	p.endTrack(2)

	// the last track finished on its own: playback stops, the lineup stays
	await(t, "idle at the end", func() bool { return playing(p) == 0 })
	assert.Equal(t, []string{"a", "b"}, lineupTitles(p.QueueSnapshot()),
		"the lineup is retained, not cleared or restarted")

	// asking for next is an explicit restart
	require.NoError(t, p.SkipNext())
	awaitPlaying(t, p, "a")
	assert.Equal(t, 1, playing(p))
}

func TestAppendingAfterCompletionStartsAtTheNewEntry(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b")}))
	awaitPlaying(t, p, "a")
	p.endTrack(1)
	awaitPlaying(t, p, "b")
	p.endTrack(2)
	await(t, "idle at the end", func() bool { return playing(p) == 0 })

	// appending resumes at the first new position, it does not replay the old list
	require.NoError(t, p.EnqueueMany([]Track{track("x"), track("y")}))
	awaitPlaying(t, p, "x")

	assert.Equal(t, []string{"a", "b", "x", "y"}, lineupTitles(p.QueueSnapshot()))
	assert.Equal(t, 3, playing(p))

	// and once that new track ends, playback continues at the one after it
	p.endTrack(3)
	awaitPlaying(t, p, "y")
	assert.Equal(t, 4, playing(p))
}

func TestFailedJumpChangesNothing(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b"), track("c")}))
	awaitPlaying(t, p, "a")

	require.NoError(t, p.JumpTo(2))
	awaitPlaying(t, p, "b")
	before := p.QueueSnapshot()

	for _, bad := range []int{0, -3, 4, 500} {
		require.ErrorIs(t, p.JumpTo(bad), ErrInvalidPosition)
	}

	assert.Equal(t, before, p.QueueSnapshot(), "a failed jump leaves cursor and lineup untouched")
	assert.Equal(t, "b", p.currentTitle())
}

func TestNavigationNeverRenumbers(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueMany([]Track{
		track("a"), track("b"), track("c"), track("d"),
	}))
	awaitPlaying(t, p, "a")

	// wander around the lineup in every direction
	require.NoError(t, p.JumpTo(4))
	awaitPlaying(t, p, "d")
	require.NoError(t, p.Previous())
	awaitPlaying(t, p, "c")
	require.NoError(t, p.SkipNext())
	awaitPlaying(t, p, "d")
	require.NoError(t, p.SkipNext())
	awaitPlaying(t, p, "a")

	s := p.QueueSnapshot()
	assert.Equal(t, []string{"a", "b", "c", "d"}, lineupTitles(s))
	for i, tr := range s.Tracks {
		assert.Equal(t, tr.Title, s.Entry(i+1).Title, "position %d still means the same track", i+1)
	}
}

// The queue display describes where playback is, not how it got there. Two players
// that landed on the same position by different routes must therefore expose the same
// snapshot - same lineup, same order, same numbers - because there is no per-entry
// state left that could record the difference.
func TestSamePositionViaDifferentRoutesHasTheSameSnapshot(t *testing.T) {
	tracks := []Track{track("a"), track("b"), track("c"), track("d"), track("e")}

	direct := newFake(t)
	require.NoError(t, direct.EnqueueMany(tracks))
	awaitPlaying(t, direct, "a")
	require.NoError(t, direct.JumpTo(3))
	awaitPlaying(t, direct, "c")

	wandered := newFake(t)
	require.NoError(t, wandered.EnqueueMany(tracks))
	awaitPlaying(t, wandered, "a")
	require.NoError(t, wandered.JumpTo(5))
	awaitPlaying(t, wandered, "e")
	require.NoError(t, wandered.Previous())
	awaitPlaying(t, wandered, "d")
	require.NoError(t, wandered.Previous())
	awaitPlaying(t, wandered, "c")

	a, b := direct.QueueSnapshot(), wandered.QueueSnapshot()
	require.Equal(t, 3, a.CurrentIndex)
	require.Equal(t, 3, b.CurrentIndex, "both routes land on the same position")
	require.Equal(t, a, b, "same lineup, same order, same cursor after two different routes")
}
