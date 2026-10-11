package dbot

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// queueEvent builds a real /queue interaction, recording what it is answered with.
func queueEvent(t *testing.T, guildID snowflake.ID, out *[]discord.MessageCreate) *events.ApplicationCommandInteractionCreate {
	t.Helper()
	guildField := ""
	if guildID != 0 {
		guildField = fmt.Sprintf(`"guild_id":"%s",`, guildID)
	}
	payload := fmt.Sprintf(`{
	  "id":"500","application_id":"300","type":2,"token":"tok",
	  %s"channel_id":"%s",
	  "channel":{"id":"%s","type":0},
	  "member":{"user":{"id":"%s","username":"u"}},
	  "data":{"id":"400","name":"queue","type":1}
	}`, guildField, panelChannel, panelChannel, panelUser)

	var i discord.ApplicationCommandInteraction
	require.NoError(t, i.UnmarshalJSON([]byte(payload)))
	return &events.ApplicationCommandInteractionCreate{
		ApplicationCommandInteraction: i,
		Respond: func(_ discord.InteractionResponseType, data discord.InteractionResponseData, _ ...rest.RequestOpt) error {
			if m, ok := data.(discord.MessageCreate); ok {
				*out = append(*out, m)
			}
			return nil
		},
	}
}

// queuePageEvent builds a button press on a queue page control.
func queuePageEvent(t *testing.T, customID string, out *[]discord.InteractionResponseData) *events.ComponentInteractionCreate {
	t.Helper()
	payload := fmt.Sprintf(`{
	  "id":"600","application_id":"300","type":3,"token":"tok",
	  "guild_id":"%s","channel_id":"%s",
	  "channel":{"id":"%s","type":0},
	  "member":{"user":{"id":"%s","username":"u"}},
	  "message":{"id":"%s","channel_id":"%s"},
	  "data":{"custom_id":"%s","component_type":2}
	}`, panelGID, panelChannel, panelChannel, panelUser, panelMessage, panelChannel, customID)

	var i discord.ComponentInteraction
	require.NoError(t, i.UnmarshalJSON([]byte(payload)))
	return &events.ComponentInteractionCreate{
		ComponentInteraction: i,
		Respond: func(_ discord.InteractionResponseType, data discord.InteractionResponseData, _ ...rest.RequestOpt) error {
			*out = append(*out, data)
			return nil
		},
	}
}

func queueTextOf(t *testing.T, msg discord.MessageCreate) string {
	t.Helper()
	return strings.Join(panelText(t, msg), "\n")
}

// messageCreateFromUpdate rewraps an update as the create payload the render helpers
// read, so a page rendered through Respond is asserted on with the same helpers.
func messageCreateFromUpdate(update discord.MessageUpdate) discord.MessageCreate {
	return discord.NewMessageCreateV2(*update.Components...)
}

func TestQueueViewPaginates(t *testing.T) {
	body := queueTextOf(t, buildQueuePageV2(1, panelSnapshot(45, 1)))

	assert.Contains(t, body, "`01`", "the first page starts at position 1")
	assert.Contains(t, body, fmt.Sprintf("`%02d`", queuePageSize), "and holds a full page")
	assert.NotContains(t, body, fmt.Sprintf("`%02d`", queuePageSize+1), "track 21 belongs to page 2")
	assert.Contains(t, body, "1–20 of 45")
}

func TestQueueViewLastPageIsShort(t *testing.T) {
	body := queueTextOf(t, buildQueuePageV2(3, panelSnapshot(45, 1)))

	assert.Contains(t, body, "41–45 of 45")
	assert.Contains(t, body, "`45`")
}

// Page 2 must be numbered absolutely, or the numbers stop matching what /jump takes.
func TestQueueViewNumbersTheWholeQueueNotJustThePage(t *testing.T) {
	body := queueTextOf(t, buildQueuePageV2(2, panelSnapshot(45, 1)))

	assert.Contains(t, body, "`21`")
	assert.NotContains(t, body, "`01`")
}

func TestQueueViewClampsOutOfRangePages(t *testing.T) {
	s := panelSnapshot(45, 1)

	assert.Equal(t, queueTextOf(t, buildQueuePageV2(3, s)), queueTextOf(t, buildQueuePageV2(99, s)))
	assert.Equal(t, queueTextOf(t, buildQueuePageV2(1, s)), queueTextOf(t, buildQueuePageV2(0, s)))
	assert.Equal(t, queueTextOf(t, buildQueuePageV2(1, s)), queueTextOf(t, buildQueuePageV2(-5, s)))
}

func TestQueueViewMarksThePlayingTrack(t *testing.T) {
	body := queueTextOf(t, buildQueuePageV2(1, panelSnapshot(3, 2)))

	assert.Contains(t, body, "▶ `02`")
	assert.NotContains(t, body, "▶ `01`")
}

func TestQueueViewEmptyQueueHasNoPager(t *testing.T) {
	msg := buildQueuePageV2(1, panelSnapshot(0, 0))

	assert.Contains(t, queueTextOf(t, msg), "Nothing queued yet")
	assert.Empty(t, panelButtons(t, msg), "there is nothing to page through")
}

func TestQueueViewDisablesPagerAtTheEdges(t *testing.T) {
	first := panelButtons(t, buildQueuePageV2(1, panelSnapshot(45, 1)))
	last := panelButtons(t, buildQueuePageV2(3, panelSnapshot(45, 1)))
	middle := panelButtons(t, buildQueuePageV2(2, panelSnapshot(45, 1)))

	require.Len(t, first, 2)
	assert.True(t, first[0].Disabled, "the first page has nowhere back to go")
	assert.False(t, first[1].Disabled)

	assert.False(t, last[0].Disabled)
	assert.True(t, last[1].Disabled, "the last page has nowhere forward to go")

	for i, b := range middle {
		assert.False(t, b.Disabled, "button %d on a middle page can go both ways", i)
	}
}

// Discord rejects the whole payload when two components share a custom id, which is
// what a first/last control colliding with prev/next on an interior page looks like.
// Every page, not just the edges, has to survive.
func TestQueueViewNeverDuplicatesACustomID(t *testing.T) {
	for pages := 1; pages <= 5; pages++ {
		for page := 1; page <= pages; page++ {
			buttons := panelButtons(t, buildQueuePageV2(page, panelSnapshot(pages*queuePageSize+1, 1)))
			seen := make(map[string]bool, len(buttons))
			for _, b := range buttons {
				assert.False(t, seen[b.CustomID],
					"page %d of %d reuses custom id %q", page, pages, b.CustomID)
				seen[b.CustomID] = true
			}
		}
	}
}

func TestQueueViewOnePageDisablesBothWays(t *testing.T) {
	buttons := panelButtons(t, buildQueuePageV2(1, panelSnapshot(5, 1)))

	require.Len(t, buttons, 2)
	for i, b := range buttons {
		assert.True(t, b.Disabled, "button %d on a single page queue", i)
	}
}

// Imported titles are unbounded, and discord rejects an oversized text display outright.
// The limit is per text display, so the body is checked on its own.
func TestQueueViewClampsOversizedText(t *testing.T) {
	long := strings.Repeat("x", 4000)
	displays := panelText(t, buildQueuePageV2(1, panelSnapshot(20, 1, long)))

	require.Len(t, displays, 2)
	assert.LessOrEqual(t, len(displays[1]), maxMessageLen, "the track list is clamped")
	assert.NotEmpty(t, displays[0], "the header survives the clamp")
}

func TestQueueViewEscapesMarkdownInTitles(t *testing.T) {
	body := queueTextOf(t, buildQueuePageV2(1, panelSnapshot(1, 1, `evil *title* [x](y)`)))

	assert.Contains(t, body, `\*title\*`)
	assert.NotContains(t, body, `evil *title*`)
}

func TestQueueCustomIDRoundTrip(t *testing.T) {
	page, ok := parseQueueCustomID(queuePageID(7))
	assert.True(t, ok)
	assert.Equal(t, 7, page)

	for _, bad := range []string{"", "queue:", "queue:abc", "queue:0", "queue:-1", "player:3", "7"} {
		_, ok := parseQueueCustomID(bad)
		assert.False(t, ok, "must not accept %q", bad)
	}
}

// A queue pager must never be mistaken for a player panel control.
func TestQueueButtonsAreNotPanelButtons(t *testing.T) {
	_, _, ok := parsePanelCustomID(queuePageID(2))
	assert.False(t, ok, "a queue pager is not a player panel button")
}

func TestQueueLandingPageOpensOnWhatIsPlaying(t *testing.T) {
	assert.Equal(t, 1, queueLandingPage(panelSnapshot(45, 1)))
	assert.Equal(t, 2, queueLandingPage(panelSnapshot(45, 25)))
	assert.Equal(t, 3, queueLandingPage(panelSnapshot(45, 45)))
	assert.Equal(t, 1, queueLandingPage(panelSnapshot(45, 0)), "an idle queue opens at the start")
}

func TestHandleQueueAnswersPrivately(t *testing.T) {
	tb := newPanelBot(t, panelTracks(3)...)

	var replies []discord.MessageCreate
	require.NoError(t, tb.handleQueue(context.Background(), queueEvent(t, panelGID, &replies)))

	require.Len(t, replies, 1)
	assert.True(t, replies[0].Flags.Has(discord.MessageFlagEphemeral),
		"browsing the queue must not push the player panel out of the newest slot")
}

func TestHandleQueueOpensOnTheCurrentPage(t *testing.T) {
	tb := newPanelBot(t, panelTracks(45)...)
	require.NoError(t, tb.Player().JumpTo(25))

	var replies []discord.MessageCreate
	require.NoError(t, tb.handleQueue(context.Background(), queueEvent(t, panelGID, &replies)))

	require.Len(t, replies, 1)
	assert.Contains(t, panelAllText(t, replies[0]), "21–40", "opens on the page holding track 25")
}

// /queue is read only: it must neither rewrite nor publish a player panel.
func TestHandleQueueTouchesNoPanel(t *testing.T) {
	tb := newPanelBot(t, panelTracks(3)...)
	tb.registerPanel(1, panelMessage)
	tb.edited = nil
	tb.sent = nil

	var replies []discord.MessageCreate
	require.NoError(t, tb.handleQueue(context.Background(), queueEvent(t, panelGID, &replies)))

	assert.Empty(t, tb.editedIDs(), "/queue must not rewrite the player panel")
	assert.Empty(t, tb.sentIDs(), "/queue must not publish a panel either")
}

func TestOnQueueComponentEditsTheMessageInPlace(t *testing.T) {
	tb := newPanelBot(t, panelTracks(45)...)

	var responses []discord.InteractionResponseData
	tb.onQueueComponent(queuePageEvent(t, queuePageID(2), &responses))

	require.Len(t, responses, 1, "exactly one response")
	update, ok := responses[0].(discord.MessageUpdate)
	require.True(t, ok, "the page is edited in place, not replaced by a new message")

	assert.Contains(t, panelAllText(t, messageCreateFromUpdate(update)), "`21`")
}

func TestOnQueueComponentAnswersEveryClickOnce(t *testing.T) {
	tb := newPanelBot(t, panelTracks(45)...)

	for _, id := range []string{queuePageID(1), queuePageID(2), queuePageID(3), queuePageID(99)} {
		var responses []discord.InteractionResponseData
		tb.onQueueComponent(queuePageEvent(t, id, &responses))
		assert.Len(t, responses, 1, "custom id %q", id)
	}
}

func TestOnQueueComponentIgnoresForeignIDs(t *testing.T) {
	tb := newPanelBot(t, panelTracks(3)...)

	for _, id := range []string{"", playerPanelID(1, playerPanelBtnNext), "queue:abc", "unrelated"} {
		var responses []discord.InteractionResponseData
		tb.onQueueComponent(queuePageEvent(t, id, &responses))
		assert.Empty(t, responses, "must ignore %q", id)
	}
}

// A click shows the queue as it is now, not as it was when the page was drawn.
func TestOnQueueComponentReflectsTheLiveQueue(t *testing.T) {
	tb := newPanelBot(t, panelTracks(45)...)

	var before []discord.InteractionResponseData
	tb.onQueueComponent(queuePageEvent(t, queuePageID(3), &before))
	require.Len(t, before, 1)

	require.NoError(t, tb.Player().JumpTo(2))

	var after []discord.InteractionResponseData
	tb.onQueueComponent(queuePageEvent(t, queuePageID(1), &after))
	require.Len(t, after, 1)

	assert.Contains(t, panelAllText(t, messageCreateFromUpdate(after[0].(discord.MessageUpdate))),
		"▶ `02`", "the marker follows the cursor")
}
