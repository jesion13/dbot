package dbot

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"dbot/pkg/player"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- serialized voice/player replacement ------------------------------------

func TestConcurrentVoiceReplacementInstallsExactlyOnePlayer(t *testing.T) {
	d, first := newLifecycleBot(t)

	conn := &countingConn{}
	require.NoError(t, first.SetVC(conn, 7))

	// two disconnect events land at the same time
	var wg sync.WaitGroup
	installed := make(chan *player.Player, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := d.ReplacePlayer()
			assert.NoError(t, err)
			installed <- p
		}()
	}
	wg.Wait()

	// both transitions ran, but they ran one after the other
	a, b := <-installed, <-installed
	require.NotSame(t, a, b, "each transition builds its own replacement")

	// the survivor is installed
	active := d.Player()
	require.NotNil(t, active)
	assert.True(t, active == a || active == b, "the active player is one of the replacements")
	require.Eventually(t, func() bool { return d.playerForwarders.Load() == 1 },
		2*time.Second, 10*time.Millisecond)

	// the original connection was closed exactly once, after its player was down
	assert.Equal(t, 1, conn.closes, "one transition owned the old connection")
	assert.Zero(t, conn.closedBefore, "and only after the player was closed")

	t.Cleanup(func() { _ = active.Close() })
}

func TestConcurrentReplacementDoesNotOrphanAPlayer(t *testing.T) {
	d, first := newLifecycleBot(t)
	first.SetVC(&countingConn{}, 3)

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = d.ReplacePlayer()
		}()
	}
	wg.Wait()

	active := d.Player()
	require.NotNil(t, active)
	t.Cleanup(func() { _ = active.Close() })

	// every retired player is closed, so its forwarder exits and only the active
	// player's remains
	require.Eventually(t, func() bool { return d.playerForwarders.Load() == 1 },
		3*time.Second, 10*time.Millisecond, "retired players left forwarders behind")
}

func TestReplacementKeepsVoiceStateConsistent(t *testing.T) {
	d, first := newLifecycleBot(t)
	first.SetVC(&countingConn{vcid: 7}, 7)

	replacement, err := d.ReplacePlayer()
	require.NoError(t, err)
	t.Cleanup(func() { _ = replacement.Close() })

	// a fresh player has no connection, and conn/vcid agree about that
	conn, vcid := replacement.Conn()
	assert.Nil(t, conn)
	assert.EqualValues(t, 0, vcid)

	// installing one sets both together
	require.NoError(t, replacement.SetVC(&countingConn{vcid: 9}, 9))
	conn, vcid = replacement.Conn()
	assert.NotNil(t, conn)
	assert.EqualValues(t, 9, vcid)
}

// countingConn reports how often it was closed and whether its player was still
// alive at that moment.
type countingConn struct {
	voice.Conn
	closes       int
	closedBefore int
	vcid         snowflake.ID
	alive        func() bool
}

func (c *countingConn) GuildID() snowflake.ID { return 1 }

func (c *countingConn) SetSpeaking(context.Context, voice.SpeakingFlags) error { return nil }

func (c *countingConn) Close(context.Context) {
	c.closes++
	if c.alive != nil && c.alive() {
		c.closedBefore++
	}
}

// --- error forwarding -------------------------------------------------------

func TestErrorConsumerExitsOnContextCancellation(t *testing.T) {
	d, _ := newLifecycleBot(t)

	ctx, cancel := context.WithCancel(d.botCtx())
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		d.consumePlayerErrors(ctx, func(player.Err) {})
	}()

	cancel()
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("the error consumer ignored context cancellation")
	}
}

func TestForwarderExitsWhenItsPlayerCloses(t *testing.T) {
	d, first := newLifecycleBot(t)
	require.Eventually(t, func() bool { return d.playerForwarders.Load() == 1 },
		2*time.Second, 10*time.Millisecond)

	require.NoError(t, first.Close())
	require.Eventually(t, func() bool { return d.playerForwarders.Load() == 0 },
		2*time.Second, 10*time.Millisecond, "the forwarder outlived its closed player")
}

func TestRepeatedReplacementDoesNotGrowForwarders(t *testing.T) {
	d, _ := newLifecycleBot(t)

	// one forwarder for the starting player
	require.Eventually(t, func() bool { return d.playerForwarders.Load() == 1 },
		2*time.Second, 10*time.Millisecond)

	for range 6 {
		_, err := d.ReplacePlayer()
		require.NoError(t, err)
		// the retired player's forwarder is gone, so exactly one is left
		require.Eventually(t, func() bool { return d.playerForwarders.Load() == 1 },
			2*time.Second, 10*time.Millisecond, "forwarders accumulate with every replacement")
	}

	active := d.Player()
	t.Cleanup(func() { _ = active.Close() })
}

func TestFullErrorStreamDropsInsteadOfBlocking(t *testing.T) {
	d, _ := newLifecycleBot(t)
	// a tiny stream with no consumer: the forwarder must not wedge
	d.playerErrs = make(chan player.Err, 1)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 50 {
			d.forwardPlayerErr(player.Err{Err: assertErr(i)})
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a full error stream blocked the forwarder")
	}
}

func TestPlayersDoNotCloseTheBotsErrorStream(t *testing.T) {
	d, first := newLifecycleBot(t)
	require.NoError(t, first.Close())

	// the shared stream belongs to the bot and is still usable
	select {
	case d.playerErrs <- player.Err{Err: assertErr(1)}:
	default:
		t.Fatal("the bot error stream was closed by a player")
	}

	active, err := d.ReplacePlayer()
	require.NoError(t, err)
	t.Cleanup(func() { _ = active.Close() })
}

type assertErr int

func (e assertErr) Error() string { return "err" + strings.Repeat("x", int(e)%3) }
