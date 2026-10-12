package dbot

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"dbot/pkg/store"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newMapChannelEvent builds a /set-bot-channel interaction with the given permissions.
func newMapChannelEvent(t *testing.T, guild string, perms discord.Permissions) (*events.ApplicationCommandInteractionCreate, *[]discord.MessageCreate) {
	t.Helper()
	guildField := ""
	if guild != "" {
		guildField = `"guild_id":"` + guild + `",`
	}
	raw := `{
		"id":"1","application_id":"2","type":2,"token":"tok","version":1,
		` + guildField + `"channel_id":"3",
		"member":{"user":{"id":"4","username":"u"},"permissions":"` + itoa(int64(perms)) + `"},
		"data":{"id":"5","type":1,"name":"set-bot-channel",
			"options":[
				{"type":3,"name":"type","value":"music"},
				{"type":7,"name":"channel","value":"999"}
			],
			"resolved":{"channels":{"999":{"id":"999","type":0,"name":"music"}}}}}`

	var aci discord.ApplicationCommandInteraction
	require.NoError(t, json.Unmarshal([]byte(raw), &aci))

	var replies []discord.MessageCreate
	e := &events.ApplicationCommandInteractionCreate{ApplicationCommandInteraction: aci}
	e.Respond = func(_ discord.InteractionResponseType, data discord.InteractionResponseData, _ ...rest.RequestOpt) error {
		if m, ok := data.(discord.MessageCreate); ok {
			replies = append(replies, m)
		}
		return nil
	}
	return e, &replies
}

func TestHandleMapChannelAuthorization(t *testing.T) {
	t.Run("non administrator cannot remap a channel", func(t *testing.T) {
		tb := newTestBot(t)
		e, replies := newMapChannelEvent(t, testGID, discord.PermissionSendMessages)

		require.NoError(t, tb.handleMapChannel(tb.Ctx, e))

		require.Len(t, *replies, 1)
		assert.Equal(t, errNotAuthorized, (*replies)[0].Content)
		assert.True(t, (*replies)[0].Flags.Has(discord.MessageFlagEphemeral),
			"the refusal must be private")

		_, err := tb.Store.GetChannel(tb.Ctx, store.GetChannelParams{Gid: testGID, Type: musicChannel})
		assert.ErrorIs(t, err, sql.ErrNoRows, "the mapping must be untouched")
	})

	t.Run("administrator can remap a channel", func(t *testing.T) {
		tb := newTestBot(t)
		e, replies := newMapChannelEvent(t, testGID, discord.PermissionAdministrator)

		require.NoError(t, tb.handleMapChannel(tb.Ctx, e))

		require.Len(t, *replies, 1)
		assert.NotEqual(t, errNotAuthorized, (*replies)[0].Content)

		ch, err := tb.Store.GetChannel(tb.Ctx, store.GetChannelParams{Gid: testGID, Type: musicChannel})
		require.NoError(t, err)
		assert.Equal(t, "999", ch.Chid)
	})

	t.Run("dm is denied", func(t *testing.T) {
		tb := newTestBot(t)
		e, replies := newMapChannelEvent(t, "", discord.PermissionAdministrator)

		require.NoError(t, tb.handleMapChannel(tb.Ctx, e))

		require.Len(t, *replies, 1)
		assert.Equal(t, errNotAuthorized, (*replies)[0].Content)
	})
}

// --- the panel must not be refreshed for an empty queue ---------------------

// A refresh with nothing queued must not touch the panel at all: no edit, no republish.
func TestRefreshGuildSkipsAnEmptyQueue(t *testing.T) {
	tb := newPanelBot(t)
	tb.registerPanel(1, panelMessage)
	tb.edited = nil
	tb.sent = nil

	require.NoError(t, tb.refreshGuild(context.Background(), panelGID))

	assert.Empty(t, tb.editedIDs(), "an empty queue must not rewrite the panel")
	assert.Empty(t, tb.sentIDs(), "an empty queue must not publish a replacement")
}

// The other side of the same rule: a non-empty queue still refreshes, or this would
// break the panel entirely.
func TestRefreshGuildStillRefreshesANonEmptyQueue(t *testing.T) {
	tb := newPanelBot(t, panelTracks(3)...)
	tb.registerPanel(1, panelMessage)
	tb.edited = nil

	require.NoError(t, tb.refreshGuild(context.Background(), panelGID))

	assert.Equal(t, []snowflake.ID{panelMessage}, tb.editedIDs())
}

// The tick goes through refreshOnce, which is the path that actually runs every ten
// seconds, so the rule has to hold there too.
func TestRefreshOnceSkipsAnEmptyQueue(t *testing.T) {
	tb := newPanelBot(t)
	tb.registerPanel(1, panelMessage)
	tb.edited = nil
	tb.sent = nil

	require.NoError(t, tb.refreshOnce(context.Background(), panelGID))

	assert.Empty(t, tb.editedIDs())
	assert.Empty(t, tb.sentIDs())
}

// Skipping the refresh must not retire the panel: it stays the active one and its
// generation does not move, so a press still works after an empty tick.
func TestAnEmptyTickLeavesThePanelUsable(t *testing.T) {
	tb := newPanelBot(t)
	tb.registerPanel(1, panelMessage)

	require.NoError(t, tb.refreshGuild(context.Background(), panelGID))

	cur, ok := tb.panels.active(panelGID)
	require.True(t, ok, "the panel must still be the active one")
	assert.Equal(t, uint64(1), cur.generation, "the generation must not move")
	assert.Equal(t, panelMessage, cur.messageID)

	// registerPanel stamps its click with the generation argument but publish assigns
	// its own, so build the click from what the registry actually holds
	assert.Empty(t, tb.resolvePanelClick(panelClick{
		GuildID: panelGID, UserID: panelUser, ChannelID: panelChannel,
		MessageID: cur.messageID, Generation: cur.generation, Action: playerPanelBtnNext,
	}).Reject, "the panel must still accept clicks")
}
