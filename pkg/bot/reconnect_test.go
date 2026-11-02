package dbot

import (
	"context"
	"errors"
	"testing"

	"dbot/pkg/player"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newVoiceBot builds a bot whose VoiceManager has no gateway behind it: a conn.Open
// only gets as far as the voice state update, which the test controls.
func newVoiceBot(t *testing.T, stateUpdate voice.StateUpdateFunc) (*DBot, *player.Player, voice.Manager) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	mgr := voice.NewManager(stateUpdate, snowflake.MustParse("1"))
	p := player.NewPlayer()
	d := &DBot{
		Ctx:        ctx,
		Client:     &bot.Client{Caches: cache.New(cache.WithCaches(cache.FlagsAll)), VoiceManager: mgr},
		playerErrs: make(chan player.Err, 16),
	}
	d.SetPlayer(p)
	t.Cleanup(func() { _ = p.Close() })
	return d, p, mgr
}

// disgo destroyed the connection (session invalidated, close 4006): it is gone from
// its manager and its socket is closed. connectVoice must not treat the leftover
// pointer as connected; it releases it and rejoins the same channel.
func TestConnectVoiceRejoinsAfterConnectionWasDestroyed(t *testing.T) {
	noGateway := errors.New("no gateway")
	d, p, _ := newVoiceBot(t, func(context.Context, snowflake.ID, *snowflake.ID, bool, bool) error {
		return noGateway
	})

	dead := &countingConn{vcid: 42}
	require.NoError(t, p.SetVC(dead, 42))

	err := d.connectVoice(snowflake.MustParse("1"), snowflake.MustParse("2"))
	assert.ErrorContains(t, err, "failed to rejoin VC")
	assert.ErrorIs(t, err, noGateway)

	conn, _ := p.Conn()
	assert.Nil(t, conn, "the dead connection stayed attached")
	assert.Zero(t, dead.closes, "closing the dead connection would send a leave update that ejects the rejoin")
}

// A connection disgo still holds is live: connectVoice does nothing at all.
func TestConnectVoiceKeepsLiveConnection(t *testing.T) {
	d, p, mgr := newVoiceBot(t, func(context.Context, snowflake.ID, *snowflake.ID, bool, bool) error {
		t.Error("a live connection must not trigger a new voice state update")
		return nil
	})

	live := mgr.CreateConn(snowflake.MustParse("1"))
	require.NoError(t, p.SetVC(live, 42))

	require.NoError(t, d.connectVoice(snowflake.MustParse("1"), snowflake.MustParse("2")))

	conn, _ := p.Conn()
	assert.Equal(t, live, conn)
}
