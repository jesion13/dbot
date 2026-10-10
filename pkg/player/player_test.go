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

	// the gate is registered before the wait, so a test that ends the track right
	// after seeing it start can never close a channel nobody waits on yet
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

// endTrack finishes a playing track. It waits for the track to register, so the
// test never races the player's goroutine.
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

// ids returns the ids of the tracks that began playing, in play order.
func (f *fake) ids() []uint64 {
	var out []uint64
	for {
		select {
		case id := <-f.started:
			out = append(out, id)
		default:
			return out
		}
	}
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

func titlesOf(ts []Track) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Title)
	}
	return out
}

func TestEnqueuePlaysInFIFOOrder(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.Enqueue(track("a")))
	require.NoError(t, p.EnqueueMany([]Track{track("b"), track("c")}))
	await(t, "a playing", func() bool { return p.currentTitle() == "a" })

	s := p.QueueSnapshot()
	require.Equal(t, "a", s.Current.Title)
	// playing track is position 1, so b is 2 and c is 3
	require.Equal(t, []string{"b", "c"}, titlesOf(s.Upcoming))
	require.NoError(t, p.JumpTo(2))
	await(t, "b playing", func() bool { return p.currentTitle() == "b" })
}

func TestEnqueueManyEmptyIsNoop(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueMany(nil))

	s := p.QueueSnapshot()
	require.Nil(t, s.Current)
	require.Empty(t, s.Upcoming)
	require.Empty(t, p.ids())
}

func TestEnqueueManyAssignsStableIDs(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueMany([]Track{track("a"), track("a"), track("b")}))

	s := p.QueueSnapshot()
	require.Equal(t, uint64(1), s.Current.ID)
	// duplicates are kept and never share an id
	require.Equal(t, []uint64{2, 3}, []uint64{s.Upcoming[0].ID, s.Upcoming[1].ID})
}

func TestNaturalCompletionAdvancesOnce(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b"), track("c")}))
	await(t, "a playing", func() bool { return p.currentTitle() == "a" })

	p.endTrack(1)
	await(t, "b playing", func() bool { return p.currentTitle() == "b" })
	await(t, "b in history", func() bool { return len(p.QueueSnapshot().History) == 1 })

	p.endTrack(2)
	await(t, "c playing", func() bool { return p.currentTitle() == "c" })
	p.endTrack(3)

	await(t, "idle", func() bool { return p.Current() == nil })
	s := p.QueueSnapshot()
	require.Empty(t, s.Upcoming)
	require.Equal(t, []string{"a", "b", "c"}, titlesOf(s.History))
}

func TestTrackFailureStillAdvances(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b")}))
	await(t, "a playing", func() bool { return p.currentTitle() == "a" })
	p.failTrack(1, errors.New("boom"))

	p.endTrack(1)
	await(t, "b playing", func() bool { return p.currentTitle() == "b" })
	require.Equal(t, []string{"a"}, titlesOf(p.QueueSnapshot().History))
}

func TestSkip(t *testing.T) {
	t.Run("skips to the next track", func(t *testing.T) {
		p := newFake(t)
		require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b")}))
		await(t, "a playing", func() bool { return p.currentTitle() == "a" })

		require.NoError(t, p.SkipNext())
		await(t, "b playing", func() bool { return p.currentTitle() == "b" })
		require.Equal(t, []string{"a"}, titlesOf(p.QueueSnapshot().History))
	})

	t.Run("last track leaves the player idle", func(t *testing.T) {
		p := newFake(t)
		require.NoError(t, p.Enqueue(track("a")))
		require.NoError(t, p.SkipNext())
		await(t, "idle", func() bool { return p.Current() == nil })
		require.Empty(t, p.QueueSnapshot().Upcoming)
	})

	t.Run("idle player", func(t *testing.T) {
		p := newFake(t)
		require.ErrorIs(t, p.SkipNext(), ErrNothingPlaying)
	})
}

func TestPrevious(t *testing.T) {
	t.Run("returns to history and keeps the current track queued", func(t *testing.T) {
		p := newFake(t)
		require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b"), track("c")}))
		await(t, "a", func() bool { return p.currentTitle() == "a" })
		p.endTrack(1)
		await(t, "b", func() bool { return p.currentTitle() == "b" })

		require.NoError(t, p.Previous())
		await(t, "a again", func() bool { return p.currentTitle() == "a" })
		require.Equal(t, []string{"b", "c"}, titlesOf(p.QueueSnapshot().Upcoming))

		// the track we came from is playable again
		require.NoError(t, p.SkipNext())
		await(t, "b again", func() bool { return p.currentTitle() == "b" })
	})

	t.Run("empty history", func(t *testing.T) {
		p := newFake(t)
		require.NoError(t, p.Enqueue(track("a")))
		await(t, "a", func() bool { return p.currentTitle() == "a" })
		require.ErrorIs(t, p.Previous(), ErrNoHistory)
		require.Equal(t, "a", p.currentTitle())
	})

	t.Run("idle player with history", func(t *testing.T) {
		p := newFake(t)
		require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b")}))
		await(t, "a", func() bool { return p.currentTitle() == "a" })
		p.endTrack(1)
		await(t, "b", func() bool { return p.currentTitle() == "b" })
		p.endTrack(2)
		await(t, "idle", func() bool { return p.Current() == nil })

		require.NoError(t, p.Previous())
		await(t, "b again", func() bool { return p.currentTitle() == "b" })
	})
}

func TestJump(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b"), track("c"), track("d")}))
	await(t, "a playing", func() bool { return p.currentTitle() == "a" })

	t.Run("out of range", func(t *testing.T) {
		require.ErrorIs(t, p.JumpTo(0), ErrInvalidPosition)
		require.ErrorIs(t, p.JumpTo(-1), ErrInvalidPosition)
		require.ErrorIs(t, p.JumpTo(99), ErrInvalidPosition)
		// a failed jump left the queue exactly as it was
		require.Equal(t, "a", p.currentTitle())
		require.Len(t, p.QueueSnapshot().Upcoming, 3)
	})

	t.Run("current position is a noop", func(t *testing.T) {
		require.NoError(t, p.JumpTo(1))
		time.Sleep(20 * time.Millisecond)
		require.Equal(t, "a", p.currentTitle())
		require.Len(t, p.QueueSnapshot().Upcoming, 3)
	})

	t.Run("jumps forward and drops the skipped tracks", func(t *testing.T) {
		require.NoError(t, p.JumpTo(3))
		await(t, "c playing", func() bool { return p.currentTitle() == "c" })
		require.Equal(t, []string{"d"}, titlesOf(p.QueueSnapshot().Upcoming))
		require.Equal(t, []string{"a"}, titlesOf(p.QueueSnapshot().History))

		// the stale track's goroutine must not advance the queue when it unwinds
		time.Sleep(20 * time.Millisecond)
		require.Equal(t, "c", p.currentTitle())
	})
}

func TestJumpWhileIdle(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b")}))
	require.ErrorIs(t, p.JumpTo(3), ErrInvalidPosition)

	await(t, "a", func() bool { return p.currentTitle() == "a" })
	p.endTrack(1)
	await(t, "b", func() bool { return p.currentTitle() == "b" })
	p.endTrack(2)
	await(t, "idle", func() bool { return p.Current() == nil })

	// idle: the first queued track is position 1
	require.NoError(t, p.EnqueueMany([]Track{track("x"), track("y")}))
	await(t, "x", func() bool { return p.currentTitle() == "x" })
	require.NoError(t, p.JumpTo(2))
	await(t, "y", func() bool { return p.currentTitle() == "y" })
}

func TestSnapshotIsACopy(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.EnqueueMany([]Track{track("a"), track("b"), track("c")}))
	await(t, "a", func() bool { return p.currentTitle() == "a" })

	s := p.QueueSnapshot()
	s.Upcoming[0].Title = "hacked"
	s.Upcoming = append(s.Upcoming, track("injected"))
	*s.Current = track("hacked")

	fresh := p.QueueSnapshot()
	require.Equal(t, []string{"b", "c"}, titlesOf(fresh.Upcoming))
	require.Equal(t, "a", fresh.Current.Title)
}

func TestCurrentIsACopy(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.Enqueue(track("a")))
	await(t, "a", func() bool { return p.currentTitle() == "a" })

	c := p.Current()
	c.Title = "hacked"
	require.Equal(t, "a", p.currentTitle())
}

func TestHistoryIsBounded(t *testing.T) {
	p := newFake(t)
	total := historyMax + 10
	tracks := make([]Track, 0, total)
	for i := range total {
		tracks = append(tracks, track(fmt.Sprint(i)))
	}
	require.NoError(t, p.EnqueueMany(tracks))

	for i := range total {
		require.NoError(t, awaitPlaying(t, p, fmt.Sprint(i)))
		p.endTrack(uint64(i + 1))
	}
	await(t, "idle", func() bool { return p.Current() == nil })

	s := p.QueueSnapshot()
	require.Len(t, s.History, historyMax)
	require.Equal(t, fmt.Sprint(total-historyMax), s.History[0].Title)
}

// awaitPlaying waits until title is the playing track, skipping any that started
// while we were not looking.
func awaitPlaying(t *testing.T, p *fake, title string) error {
	t.Helper()
	await(t, title, func() bool { return p.currentTitle() == title })
	return nil
}

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
		require.True(t, p.Paused(), "queueing a batch must not resume paused music")
		// the queue still grew
		require.Len(t, p.QueueSnapshot().Upcoming, 1)
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
		require.False(t, p.Paused(), "draining sounds does not create a pause either")
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
		await(t, "a", func() bool { return p.currentTitle() == "a" })

		require.NoError(t, p.TogglePause())
		require.NoError(t, p.SkipNext())
		require.NoError(t, p.Previous())
		require.Error(t, p.JumpTo(99)) // rejected, and still no toggle
		require.True(t, p.Paused(), "no queue operation clears the manual pause")
	})

	t.Run("explicit resume clears it", func(t *testing.T) {
		p := newFake(t)
		require.NoError(t, p.Enqueue(track("a")))
		await(t, "a", func() bool { return p.currentTitle() == "a" })

		require.NoError(t, p.TogglePause())
		require.NoError(t, p.TogglePause())
		require.False(t, p.Paused())
		require.False(t, p.soundSuspended())
	})
}

func TestManualPauseIsIndependentOfSounds(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.Enqueue(track("a")))
	await(t, "a", func() bool { return p.currentTitle() == "a" })

	require.NoError(t, p.TogglePause())
	require.True(t, p.Paused())
	require.NoError(t, p.TogglePause())
	require.False(t, p.Paused())

	// a sound must not clear a manual pause
	require.NoError(t, p.TogglePause())
	require.NoError(t, p.EnqueueSound(SoundTrack{Title: "boom"}))
	require.Equal(t, "boom", p.nextSound(t))
	p.endSound("boom")
	await(t, "sounds drained", func() bool { return p.QueueSnapshot().SoundWait == 0 })
	require.True(t, p.Paused())
}

func TestClose(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.Enqueue(track("a")))
	require.NoError(t, p.EnqueueSound(SoundTrack{Title: "boom"}))
	p.nextSound(t)

	require.NoError(t, p.Close())
	require.NoError(t, p.Close()) // idempotent

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
	require.NoError(t, p.Enqueue(Track{Title: "cached", Filepath: shared}))
	await(t, "playing", func() bool { return p.currentTitle() == "cached" })

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

	// one is playing, the rest are queued
	require.Len(t, p.QueueSnapshot().Upcoming, workers*each-1)
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

	// whatever the interleaving was, the queue is still consistent
	s := p.QueueSnapshot()
	require.LessOrEqual(t, len(s.History)+len(s.Upcoming), 50)
	if s.Current != nil {
		require.NotEmpty(t, s.Upcoming)
	}
}

func TestSoundQueueFIFO(t *testing.T) {
	p := newFake(t)
	first := SoundSequence{Tracks: []SoundTrack{{Title: "a1"}, {Title: "a2"}}}
	second := SoundSequence{Tracks: []SoundTrack{{Title: "b1"}, {Title: "b2"}, {Title: "b3"}}}

	require.NoError(t, p.EnqueueSoundSequence(first))
	require.NoError(t, p.EnqueueSoundSequence(second))
	// the worker may already have taken the first one; the ordering below is what
	// proves the two requests were kept apart
	require.LessOrEqual(t, p.QueueSnapshot().SoundWait, 2)

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

	// suspended from the first enqueue, never released between the two
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

	// once the backlog is full, new requests are rejected - never silently swapped
	// for one that was already accepted. The worker drains as we go, so we only
	// assert the bound and the rejection.
	var rejected error
	for i := range maxSoundRequests + 8 {
		err := p.EnqueueSound(SoundTrack{Title: fmt.Sprint(i)})
		if err != nil {
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

func TestSoundsDoNotTouchTheMusicQueue(t *testing.T) {
	p := newFake(t)
	require.NoError(t, p.Enqueue(track("song")))
	require.NoError(t, p.EnqueueSound(SoundTrack{Title: "boom"}))
	p.endSound("boom")

	s := p.QueueSnapshot()
	require.Equal(t, "song", s.Current.Title)
	require.Empty(t, s.History)
	require.Empty(t, s.Upcoming)
}

// The real playTrack refuses to write anywhere when there is no connection. It must
// still advance the queue rather than wedge on the first track.
func TestPlaybackWithoutVoiceConnectionStillAdvances(t *testing.T) {
	p := NewPlayer()
	t.Cleanup(func() { _ = p.Close() })
	require.NoError(t, p.EnqueueMany([]Track{{Title: "a"}, {Title: "b"}}))

	await(t, "advanced past both", func() bool { return len(p.QueueSnapshot().History) == 2 })
	assert.Equal(t, []string{"a", "b"}, titlesOf(p.QueueSnapshot().History))
	assert.Nil(t, p.Current(), "the player is idle, not stuck")
	assert.Equal(t, 2, len(p.ErrChan), "each failed track was reported once")
}
