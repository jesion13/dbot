package dbot

import (
	"context"
	"errors"
	"testing"
	"time"

	"dbot/pkg/player"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newLifecycleBot builds a bot with a real player and no voice connection.
func newLifecycleBot(t *testing.T) (*DBot, *player.Player) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	p := player.NewPlayer()
	d := &DBot{
		Ctx:        ctx,
		Client:     &bot.Client{Caches: cache.New(cache.WithCaches(cache.FlagsAll))},
		playerErrs: make(chan player.Err, 16),
	}
	d.SetPlayer(p)
	t.Cleanup(func() { _ = d.Player().Close() })
	return d, p
}

func TestPlayerReplacementKeepsErrorReporting(t *testing.T) {
	d, first := newLifecycleBot(t)

	// this is the loop the bot actually runs, so the test covers what it reads
	reported := make(chan player.Err, 4)
	go d.consumePlayerErrors(d.Ctx, func(e player.Err) { reported <- e })

	// the replacement path: close the old player, install a new one
	require.NoError(t, first.Close())
	second := player.NewPlayer()
	d.SetPlayer(second)
	t.Cleanup(func() { _ = second.Close() })

	second.ErrChan <- player.Err{Err: errors.New("from the replacement")}

	select {
	case got := <-reported:
		assert.EqualError(t, got.Err, "from the replacement")
	case <-time.After(2 * time.Second):
		t.Fatal("an error from the replacement player never reached reporting")
	}
}

func TestClosedPlayerErrorForwarderStops(t *testing.T) {
	_, first := newLifecycleBot(t)

	require.NoError(t, first.Close())

	// the forwarder exits when the player's channel closes, so replacements do not
	// pile up goroutines
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case _, ok := <-first.ErrChan:
			if !ok {
				return // drained and closed, which is what the forwarder sees
			}
		default:
		}
	}
	t.Fatal("the closed player's error channel was not closed")
}

func TestConcurrentPlayerReadsAndReplacement(t *testing.T) {
	d, first := newLifecycleBot(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 50 {
			_ = d.Player().QueueSnapshot()
		}
	}()

	require.NoError(t, first.Close())
	second := player.NewPlayer()
	d.SetPlayer(second)
	t.Cleanup(func() { _ = second.Close() })

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a reader blocked on a replaced player")
	}
}

func TestWypierdalajClosesPlayerBeforeItsConnection(t *testing.T) {
	d, p := newLifecycleBot(t)

	conn := &countingConn{
		alive: func() bool { return !p.QueueSnapshot().Closed },
	}
	require.NoError(t, p.SetVC(conn, 42))
	require.NoError(t, d.wypierdalajZVC())

	assert.Zero(t, conn.closedBefore, "the connection must not close while the player can still write to it")
	assert.Equal(t, 1, conn.closes, "the connection is closed exactly once")

	replacement := d.Player()
	require.NotNil(t, replacement)
	assert.False(t, replacement.QueueSnapshot().Closed, "the replacement is usable")
	t.Cleanup(func() { _ = replacement.Close() })
}

func TestWypierdalajWithNoConnectionStillReplacesThePlayer(t *testing.T) {
	d, p := newLifecycleBot(t)

	require.NoError(t, d.wypierdalajZVC())

	replacement := d.Player()
	require.NotNil(t, replacement)
	assert.False(t, replacement.QueueSnapshot().Closed)
	assert.NotSame(t, p, replacement)
	t.Cleanup(func() { _ = replacement.Close() })
}
