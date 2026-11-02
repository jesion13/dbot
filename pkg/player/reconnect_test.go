package player

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// When the connection dies the lineup must hold at the entry that died: advancing
// would churn the rest through the same dead socket, one fresh ffmpeg each. A
// replacement connection (SetVC after ClearVC) replays that entry.
func TestConnGoneHoldsTheLineupAndReplayResumesIt(t *testing.T) {
	p := NewPlayer()
	t.Cleanup(func() { _ = p.Close() })

	var mu sync.Mutex
	var started []string
	p.playTrackFn = func(ctx context.Context, gen uint64, tr Track) error {
		mu.Lock()
		started = append(started, tr.Title)
		n := len(started)
		mu.Unlock()
		if n == 1 {
			return errConnGone // the first attempt loses the connection
		}
		<-ctx.Done()
		return ctx.Err()
	}

	require.NoError(t, p.SetVC(newFakeConn(1), 10))
	require.NoError(t, p.EnqueueMany([]Track{{Title: "a"}, {Title: "b"}}))
	await(t, "the dead connection's attempt ends", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(started) == 1
	})
	snap := p.QueueSnapshot()
	assert.Equal(t, 1, snap.CurrentIndex, "the entry the connection died on stays current")
	assert.Len(t, snap.Tracks, 2, "the lineup survived the dead connection")

	p.ClearVC()
	conn, _ := p.Conn()
	assert.Nil(t, conn, "ClearVC released the connection without closing it")

	require.NoError(t, p.SetVC(newFakeConn(2), 10))
	await(t, "the replacement connection replays the held entry", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(started) == 2
	})

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"a", "a"}, started,
		"the held entry replays and the rest of the lineup waits its turn")
}
