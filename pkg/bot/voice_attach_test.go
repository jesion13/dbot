package dbot

import (
	"context"
	"sync"
	"testing"
	"time"

	"dbot/pkg/player"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockingConn is a connection whose Close can be held, so a test can observe the
// window in which a replacement is between "player closed" and "connection closed".
type blockingConn struct {
	voice.Conn
	closes       int
	closedBefore int
	alive        func() bool

	releaseClose chan struct{}
	closeEntered chan struct{}
	once         sync.Once
}

func (c *blockingConn) GuildID() snowflake.ID { return 1 }

func (c *blockingConn) SetSpeaking(context.Context, voice.SpeakingFlags) error { return nil }

func (c *blockingConn) Close(context.Context) {
	c.closes++
	if c.alive != nil && c.alive() {
		c.closedBefore++
	}
	if c.closeEntered != nil {
		c.once.Do(func() { close(c.closeEntered) })
	}
	if c.releaseClose != nil {
		<-c.releaseClose
	}
}

// TestAttachmentCannotRaceReplacement covers the interleaving the shared lifecycle
// lock exists for. An attachment that arrives while a replacement is tearing the old
// player down must wait for that transition and land on the replacement - never on
// the player that is on its way out, and never left without an owner.
func TestAttachmentCannotRaceReplacement(t *testing.T) {
	d, first := newLifecycleBot(t)

	old := &blockingConn{}
	require.NoError(t, first.SetVC(old, 1))

	// the replacement stalls with the old connection half closed
	old.closeEntered = make(chan struct{})
	old.releaseClose = make(chan struct{})
	old.alive = func() bool { return !first.QueueSnapshot().Closed }

	replaced := make(chan *player.Player, 1)
	go func() {
		p, _ := d.ReplacePlayer()
		replaced <- p
	}()

	<-old.closeEntered // the replacement is closing the old connection
	require.True(t, first.QueueSnapshot().Closed, "the old player is already shut down")

	// an attachment that arrives now must wait rather than reach into the teardown
	late := &countingConn{}
	attached := make(chan error, 1)
	go func() { attached <- d.AttachVoiceConnection(late, 2) }()

	select {
	case <-attached:
		t.Fatal("an attachment ran while a replacement was still tearing the player down")
	case <-time.After(50 * time.Millisecond):
	}

	close(old.releaseClose)
	next := <-replaced
	t.Cleanup(func() { _ = next.Close() })

	select {
	case err := <-attached:
		require.NoError(t, err, "the attachment lands on the replacement player")
	case <-time.After(3 * time.Second):
		t.Fatal("the attachment never completed")
	}

	// the connection every player owned, was closed only after its player stopped
	assert.Equal(t, 1, old.closes)
	assert.Zero(t, old.closedBefore, "the connection closed after its player")

	conn, vcid := next.Conn()
	assert.Same(t, voice.Conn(late), conn, "the replacement owns the new connection")
	assert.EqualValues(t, 2, vcid, "conn and vcid agree")
	assert.Zero(t, late.closes, "the accepted connection stays open")
}

func TestTwoAttachmentsRaceWithReplacement(t *testing.T) {
	d, first := newLifecycleBot(t)
	require.NoError(t, first.SetVC(&countingConn{}, 1))

	connA := &countingConn{}
	connB := &countingConn{}

	var wg sync.WaitGroup
	results := make(chan error, 3)
	wg.Add(3)
	go func() { defer wg.Done(); _, err := d.ReplacePlayer(); results <- err }()
	go func() { defer wg.Done(); results <- d.AttachVoiceConnection(connA, 10) }()
	go func() { defer wg.Done(); results <- d.AttachVoiceConnection(connB, 11) }()
	wg.Wait()
	close(results)

	active := d.Player()
	require.NotNil(t, active)
	t.Cleanup(func() { _ = active.Close() })

	owned, _ := active.Conn()
	// every connection is either owned by the active player or explicitly closed -
	// none is silently abandoned
	for _, c := range []*countingConn{connA, connB} {
		if voice.Conn(c) == owned {
			assert.Zero(t, c.closes, "the owned connection is still open")
			continue
		}
		assert.Equal(t, 1, c.closes, "a connection the active player does not own was closed")
	}

	// the active player holds one connection at most, and its vcid agrees with it
	if owned != nil {
		_, vcid := active.Conn()
		if owned == voice.Conn(connA) {
			assert.EqualValues(t, 10, vcid)
		}
		if owned == voice.Conn(connB) {
			assert.EqualValues(t, 11, vcid)
		}
	}
}

func TestAttachmentRejectedWhenThePlayerIsClosed(t *testing.T) {
	d, p := newLifecycleBot(t)
	require.NoError(t, p.Close())

	c := &countingConn{}
	err := d.AttachVoiceConnection(c, 5)
	require.ErrorIs(t, err, player.ErrClosed)
	assert.Equal(t, 1, c.closes, "the rejected connection is closed by the bot, not left open")
}

func TestReconnectSwapsTheConnectionSafely(t *testing.T) {
	d, first := newLifecycleBot(t)

	old := &countingConn{}
	require.NoError(t, first.SetVC(old, 1))

	fresh := &countingConn{}
	next, err := d.Reconnect(fresh, 2)
	require.NoError(t, err)
	t.Cleanup(func() { _ = next.Close() })

	assert.Equal(t, 1, old.closes, "the old connection was closed")
	assert.Zero(t, old.closedBefore, "and only after its player stopped writing")

	conn, vcid := next.Conn()
	assert.Same(t, voice.Conn(fresh), conn, "the new player owns the new connection")
	assert.EqualValues(t, 2, vcid, "conn and vcid agree")

	// the old player is shut down and takes no work
	assert.True(t, first.QueueSnapshot().Closed)
	assert.ErrorIs(t, first.Enqueue(player.Track{Title: "x"}), player.ErrClosed)
}
