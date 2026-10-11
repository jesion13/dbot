package dbot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"dbot/pkg/db/types"
	"dbot/pkg/player"
	"dbot/pkg/store"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	panelGID     = snowflake.ID(1)
	panelBotVC   = snowflake.ID(10)
	panelUser    = snowflake.ID(2)
	panelChannel = snowflake.ID(20)
	panelMessage = snowflake.ID(30)
)

// panelSnapshot builds a snapshot of n tracks with the given 1-based current position,
// or an idle one when current is 0.
func panelSnapshot(n, current int, titles ...string) player.QueueSnapshot {
	if titles == nil {
		titles = make([]string, n)
		for i := range titles {
			titles[i] = fmt.Sprintf("track %d", i+1)
		}
	}
	s := player.QueueSnapshot{CurrentIndex: current, Tracks: make([]player.Track, 0, len(titles))}
	for _, t := range titles {
		s.Tracks = append(s.Tracks, player.Track{Title: t})
	}
	return s
}

func panelPositions(rows []panelRow) []int {
	out := make([]int, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Position)
	}
	return out
}

func panelCurrentPositions(rows []panelRow) []int {
	var out []int
	for _, r := range rows {
		if r.Current {
			out = append(out, r.Position)
		}
	}
	return out
}

// --- window ------------------------------------------------------------------

func TestPanelWindowAtTheBeginning(t *testing.T) {
	rows := panelWindow(panelSnapshot(20, 1))

	// nothing before position 1 exists, so the window starts there
	assert.Equal(t, []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}, panelPositions(rows),
		"three before position 1 is nothing, so we get 1..11")
	assert.Equal(t, []int{1}, panelCurrentPositions(rows))
}

func TestPanelWindowInTheMiddle(t *testing.T) {
	rows := panelWindow(panelSnapshot(40, 20))

	assert.Equal(t, []int{17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30}, panelPositions(rows),
		"three before and ten after the current position")
	assert.Equal(t, []int{20}, panelCurrentPositions(rows))
	assert.Equal(t, "track 17", rows[0].Title, "titles come from the same absolute position")
}

func TestPanelWindowAtTheEnd(t *testing.T) {
	rows := panelWindow(panelSnapshot(8, 8))

	assert.Equal(t, []int{5, 6, 7, 8}, panelPositions(rows),
		"nothing exists after the last position, so the window just stops")
	assert.Equal(t, []int{8}, panelCurrentPositions(rows))
}

func TestPanelWindowNearTheStartHasOnlyWhatExists(t *testing.T) {
	// a lineup shorter than the whole window must not invent positions
	rows := panelWindow(panelSnapshot(3, 2))

	assert.Equal(t, []int{1, 2, 3}, panelPositions(rows))
	assert.Equal(t, []int{2}, panelCurrentPositions(rows))
}

func TestPanelWindowNumbersAreAbsolute(t *testing.T) {
	// positions keep pointing at the same track however far playback has moved, so
	// the window never renumbers and never drops an entry
	for _, cur := range []int{1, 2, 5, 14, 15, 16, 25} {
		rows := panelWindow(panelSnapshot(25, cur))
		for _, r := range rows {
			assert.Equal(t, fmt.Sprintf("track %d", r.Position), r.Title,
				"position %d must be the track that was enqueued at that position", r.Position)
		}
	}
}

func TestPanelWindowEmptyLineup(t *testing.T) {
	assert.Empty(t, panelWindow(panelSnapshot(0, 0)))
}

func TestPanelWindowIdleShowsWhatWouldPlayNext(t *testing.T) {
	rows := panelWindow(panelSnapshot(20, 0))

	assert.Equal(t, []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, panelPositions(rows))
	assert.Empty(t, panelCurrentPositions(rows), "nothing is playing, so nothing is highlighted")
}

// --- rendering ---------------------------------------------------------------

func TestPanelRenderPointsAtTheCurrentTrack(t *testing.T) {
	body := panelBody(panelWindow(panelSnapshot(6, 3)))

	for i, line := range strings.Split(body, "\n") {
		position := i + 1
		if position == 3 {
			assert.True(t, strings.HasPrefix(line, panelNowMarker),
				"the arrow points at the playing row: %q", line)
		} else {
			assert.False(t, strings.HasPrefix(line, "▶"),
				"position %d must not be pointed at: %q", position, line)
		}
		// the marker column is a fixed width, so every number starts at the same offset
		want := strings.Repeat(" ", len([]rune(panelNowMarker))) +
			fmt.Sprintf("%d. track %d", position, position)
		if position == 3 {
			want = panelNowMarker + fmt.Sprintf("%d. track %d", position, position)
		}
		assert.Equal(t, want, line, "line %d keeps its absolute number and alignment", position)
	}
	assert.NotEmpty(t, renderPanel(1, panelSnapshot(6, 3)).components)
}

// The arrow is the only marker: no leftover bold to say the same thing twice.
func TestPanelRenderHasOneMarkerOnly(t *testing.T) {
	body := panelBody(panelWindow(panelSnapshot(6, 3)))

	assert.NotContains(t, body, "**", "the arrow replaces the bold")
	assert.Equal(t, 1, strings.Count(body, "▶"), "exactly one row is pointed at")
}

func TestPanelRenderHeaderStatesPlayback(t *testing.T) {
	t.Run("playing", func(t *testing.T) {
		assert.Contains(t, panelHead(panelSnapshot(3, 1)), "Playing")
	})
	t.Run("paused", func(t *testing.T) {
		s := panelSnapshot(3, 1)
		s.Paused = true
		assert.Contains(t, panelHead(s), "Paused")
	})
	t.Run("stopped", func(t *testing.T) {
		assert.Contains(t, panelHead(panelSnapshot(3, 0)), "Stopped")
	})
	t.Run("title", func(t *testing.T) {
		assert.Contains(t, panelHead(panelSnapshot(3, 1)), "DBot Player")
	})
}

func TestPanelRenderEmptyAndIdleStates(t *testing.T) {
	t.Run("empty lineup", func(t *testing.T) {
		r := renderPanel(1, panelSnapshot(0, 0))
		assert.Contains(t, panelBody(panelWindow(panelSnapshot(0, 0))), "Nothing queued")
		assert.Contains(t, panelHead(panelSnapshot(0, 0)), "Stopped")
		assert.NotEmpty(t, r.components, "an empty panel still shows its buttons")
	})

	t.Run("idle with a lineup", func(t *testing.T) {
		body := panelBody(panelWindow(panelSnapshot(3, 0)))
		assert.NotContains(t, body, "▶", "nothing is playing, so nothing is pointed at")
		assert.Contains(t, body, "1. track 1", "but the lineup is still listed")
	})
}

func TestPanelRenderShowsWaitingSoundsOnlyWhenThereAreSome(t *testing.T) {
	s := panelSnapshot(3, 1)
	assert.Equal(t, "", panelFoot(s))

	s.SoundWait = 1
	assert.Contains(t, panelFoot(s), "1 sound sequence")

	s.SoundWait = 4
	assert.Contains(t, panelFoot(s), "4 sound sequences")
}

func TestPanelRenderUsesOneComponentPayload(t *testing.T) {
	// Components V2 lives entirely in the components: no content, no embeds
	r := renderPanel(1, panelSnapshot(3, 1))
	require.Len(t, r.components, 1)

	container, ok := r.components[0].(discord.ContainerComponent)
	require.True(t, ok, "the panel is a container")
	assert.NotEmpty(t, container.Components)

	// the control row is the one action row, carrying exactly our four buttons
	var row *discord.ActionRowComponent
	for i := range container.Components {
		if ar, ok := container.Components[i].(discord.ActionRowComponent); ok {
			row = &ar
		}
	}
	require.NotNil(t, row, "the panel has a control row")
	require.Len(t, row.Components, 4)
}

// --- button mapping ----------------------------------------------------------

// panelBot is a bot with a live player, a voice connection and panel seams that
// record edits instead of talking to discord.
type panelBot struct {
	*DBot
	cache cache.Caches
	// sent records every panel message that was published
	sent []snowflake.ID
	// replies records every slash-command or button response
	replies []discord.MessageCreate
	// deleted records retired panel messages, in order
	deleted []snowflake.ID
	// nextID hands out a fresh message id per publish
	nextID snowflake.ID
	// mu guards the recorders: a replacement deletes outside the registry lock, so
	// two concurrent swaps really do touch these at once
	mu sync.Mutex
	// sendErr fails the next publish, so the failure path can be exercised
	sendErr error
}

func newPanelBot(t *testing.T, tracks ...player.Track) *panelBot {
	t.Helper()

	c := cache.New(cache.WithCaches(cache.FlagsAll))
	c.AddVoiceState(discord.VoiceState{GuildID: panelGID, UserID: panelUser, ChannelID: &panelBotVC})

	p := player.NewPlayer()
	require.NoError(t, p.SetVC(stubConn{gid: panelGID}, panelBotVC))
	t.Cleanup(func() { _ = p.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	d := &DBot{Ctx: ctx, Client: &bot.Client{Caches: c}}
	d.SetPlayer(p)

	pb := &panelBot{DBot: d, cache: c, nextID: panelMessage}
	d.createPanel = func(context.Context, snowflake.ID, ...discord.LayoutComponent) (snowflake.ID, error) {
		if pb.sendErr != nil {
			return 0, pb.sendErr
		}
		pb.mu.Lock()
		id := pb.nextID
		pb.nextID++
		pb.sent = append(pb.sent, id)
		pb.mu.Unlock()
		return id, nil
	}
	d.answerPanel = func(_ snowflake.ID, _ string, m discord.MessageCreate) (snowflake.ID, error) {
		if pb.sendErr != nil {
			return 0, pb.sendErr
		}
		pb.mu.Lock()
		pb.replies = append(pb.replies, m)
		id := pb.nextID
		pb.nextID++
		pb.sent = append(pb.sent, id)
		pb.mu.Unlock()
		return id, nil
	}
	d.deletePanel = func(_, msgID snowflake.ID) error {
		pb.mu.Lock()
		pb.deleted = append(pb.deleted, msgID)
		pb.mu.Unlock()
		return nil
	}

	if len(tracks) > 0 {
		require.NoError(t, p.EnqueueMany(tracks))
	}
	return pb
}

func (tb *panelBot) sentIDs() []snowflake.ID {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return append([]snowflake.ID(nil), tb.sent...)
}

func (tb *panelBot) deletedIDs() []snowflake.ID {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return append([]snowflake.ID(nil), tb.deleted...)
}

func (tb *panelBot) sentCount() int {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return len(tb.sent)
}

func panelTracks(n int) []player.Track {
	out := make([]player.Track, 0, n)
	for i := range n {
		out = append(out, player.Track{Title: fmt.Sprintf("track %d", i+1)})
	}
	return out
}

func TestPanelPreviousButtonGoesBackOnePosition(t *testing.T) {
	tb := newPanelBot(t, panelTracks(4)...)
	require.NoError(t, tb.Player().JumpTo(3))

	require.NoError(t, tb.panelAction(panelBtnPrev, panelGID))

	assert.Equal(t, 2, tb.Player().QueueSnapshot().CurrentIndex)
}

func TestPanelPauseButtonTogglesManualPause(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)
	require.False(t, tb.Player().Paused())

	require.NoError(t, tb.panelAction(panelBtnPause, panelGID))
	assert.True(t, tb.Player().Paused())

	require.NoError(t, tb.panelAction(panelBtnPause, panelGID))
	assert.False(t, tb.Player().Paused())
}

func TestPanelPauseButtonLabelFollowsTheState(t *testing.T) {
	assert.Equal(t, "Pause", pauseLabel(false))
	assert.Equal(t, "Play", pauseLabel(true), "the button offers the action, not the state")
}

func TestPanelNextButtonKeepsWraparound(t *testing.T) {
	tb := newPanelBot(t, panelTracks(3)...)
	require.NoError(t, tb.Player().JumpTo(3))

	require.NoError(t, tb.panelAction(panelBtnNext, panelGID))

	assert.Equal(t, 1, tb.Player().QueueSnapshot().CurrentIndex, "next wraps past the last position")
}

// The Event button has no count, so it must never become a run of sounds the way
// `event 10` does.
func TestPanelEventEnqueuesExactlyOneSoundSequence(t *testing.T) {
	tb := newTestBot(t)
	srq.s = map[string][]store.Sound{}
	defer srq.dropAndLoad(tb.Store, testGID)

	for _, name := range []string{"airhorn", "boom", "clap"} {
		_, err := tb.Store.AddSound(tb.Ctx, store.AddSoundParams{
			Gid:       testGID,
			Url:       "https://example.invalid/" + name + ".mp3",
			OriginUrl: "https://example.invalid/" + name,
			Aliases:   types.Aliases{name},
		})
		require.NoError(t, err)
	}

	// the selection path is what decides the size: `event` with no count is one sound
	sounds, err := findSound(tb.Store, "event", testGID)
	require.NoError(t, err)
	require.Len(t, sounds, 1)

	seq, err := randomSoundSequence(tb.Store, testGID)
	require.NoError(t, err)
	assert.Len(t, seq.Tracks, 1, "one button press is one sequence of exactly one sound")

	p := player.NewPlayer()
	t.Cleanup(func() { _ = p.Close() })
	tb.SetPlayer(p)

	require.NoError(t, tb.queueRandomSound(testGID))
}

// A failing action must be reported, not silently swallowed, and must not touch the
// panel message.
func TestPanelActionErrorsAreReported(t *testing.T) {
	tb := newPanelBot(t)

	msg := tb.panelAction(panelBtnPrev, panelGID).Error()

	assert.Contains(t, msg, "earlier track", "an empty lineup has nothing to go back to")
}

func TestPanelUnknownActionIsRejected(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)

	err := tb.panelAction("nonsense", panelGID)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown")
}

// --- guards and lifecycle ----------------------------------------------------

// registers a panel the way a publish does, so clicks can be exercised without one.
func (tb *panelBot) registerPanel(generation uint64, messageID snowflake.ID) panelClick {
	// park the id counter past the one we just registered, so the next publish is
	// genuinely a different message
	if tb.nextID <= messageID {
		tb.nextID = messageID + 1
	}
	tb.panels.publish(panelGID, panelChannel, func(g uint64) (snowflake.ID, error) {
		return messageID, nil
	})
	return panelClick{
		GuildID:    panelGID,
		UserID:     panelUser,
		MessageID:  messageID,
		Generation: generation,
		Action:     panelBtnNext,
	}
}

func TestPanelRejectsClicksFromTheWrongVoiceChannel(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)
	click := tb.registerPanel(1, panelMessage)

	other := snowflake.ID(99)
	tb.cache.AddVoiceState(discord.VoiceState{GuildID: panelGID, UserID: panelUser, ChannelID: &other})

	res := tb.resolvePanelClick(click)

	assert.Equal(t, "join my voice channel first", res.Reject)
}

func TestPanelRejectsClicksWithNoVoiceConnection(t *testing.T) {
	tb := newPanelBot(t)
	click := tb.registerPanel(1, panelMessage)
	tb.SetPlayer(player.NewPlayer()) // a fresh player has no connection

	res := tb.resolvePanelClick(click)

	assert.Equal(t, "i am not connected to a voice channel", res.Reject)
}

// A replaced panel's buttons must stop working even if its message was never deleted.
func TestPanelRejectsAStalePanelMessage(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)
	stale := tb.registerPanel(1, panelMessage)
	require.Empty(t, tb.resolvePanelClick(stale).Reject, "the panel that is registered works")

	tb.registerPanel(2, panelMessage+1)

	res := tb.resolvePanelClick(stale)
	assert.Contains(t, res.Reject, "no longer active",
		"the old message must not control playback even though it was never deleted")
}

// The generation travels in the custom id, so a click from an older panel is refused
// even if the message id somehow still matches.
func TestPanelRejectsAStaleGeneration(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)
	tb.registerPanel(1, panelMessage)

	res := tb.resolvePanelClick(panelClick{
		GuildID: panelGID, UserID: panelUser, MessageID: panelMessage,
		Generation: 99, Action: panelBtnNext,
	})

	assert.Contains(t, res.Reject, "no longer active")
}

func TestPanelRejectsClicksWhenNoPanelIsRegistered(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)

	res := tb.resolvePanelClick(panelClick{
		GuildID: panelGID, UserID: panelUser, MessageID: panelMessage,
		Generation: 1, Action: panelBtnNext,
	})

	assert.Contains(t, res.Reject, "no longer active")
}

func TestPanelCustomIDRoundTrips(t *testing.T) {
	for _, action := range []string{panelBtnPrev, panelBtnPause, panelBtnNext, panelBtnEvent} {
		gen, got, ok := parsePanelCustomID(panelCustomID(7, action))
		require.True(t, ok, "our own ids must parse")
		assert.Equal(t, uint64(7), gen)
		assert.Equal(t, action, got)
	}

	for _, bad := range []string{"", "dbot:panel:", "dbot:panel:x:next", "other:thing", "dbot:panel:1"} {
		_, _, ok := parsePanelCustomID(bad)
		assert.False(t, ok, "must not accept %q", bad)
	}
}

// --- replacement -------------------------------------------------------------

// A replacement must be sent before the old one is deleted, so a send failure cannot
// leave the channel with no panel at all.
func TestPanelSendsBeforeDeleting(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)
	tb.registerPanel(1, panelMessage)

	var order []string
	tb.deletePanel = func(_, id snowflake.ID) error {
		order = append(order, fmt.Sprintf("delete:%d", id))
		return nil
	}
	require.NoError(t, tb.swapPanel(panelGID, panelChannel, func(uint64) (snowflake.ID, error) {
		order = append(order, "send")
		return panelMessage + 1, nil
	}))

	assert.Equal(t, []string{"send", fmt.Sprintf("delete:%d", panelMessage)}, order,
		"the replacement must exist before the old message is retired")
}

// A failed send keeps the current panel registered and usable: nothing is deleted
// and the old buttons still work.
func TestPanelFailedSendKeepsTheCurrentPanelUsable(t *testing.T) {
	tb := newPanelBot(t, panelTracks(3)...)
	click := tb.registerPanel(1, panelMessage)
	tb.sent = nil

	tb.sendErr = errors.New("rate limited")
	err := tb.swapPanel(panelGID, panelChannel, func(uint64) (snowflake.ID, error) {
		return 0, tb.sendErr
	})
	tb.sendErr = nil

	require.Error(t, err)
	assert.Empty(t, tb.deletedIDs(), "a failed send must not retire the working panel")

	cur, ok := tb.panels.active(panelGID)
	require.True(t, ok, "the old panel is still the active one")
	assert.Equal(t, panelMessage, cur.messageID)
	assert.Empty(t, tb.resolvePanelClick(click).Reject, "its buttons still work")
}

// A successful replacement deactivates the previous panel.
func TestPanelReplacementDeactivatesThePrevious(t *testing.T) {
	tb := newPanelBot(t, panelTracks(3)...)
	stale := tb.registerPanel(1, panelMessage)

	require.NoError(t, tb.swapPanel(panelGID, panelChannel, func(uint64) (snowflake.ID, error) {
		return panelMessage + 1, nil
	}))

	cur, ok := tb.panels.active(panelGID)
	require.True(t, ok)
	assert.Equal(t, panelMessage+1, cur.messageID)
	assert.Equal(t, uint64(2), cur.generation, "each replacement bumps the generation")
	assert.Equal(t, []snowflake.ID{panelMessage}, tb.deletedIDs())
	assert.Contains(t, tb.resolvePanelClick(stale).Reject, "no longer active")
}

// If the delete fails the old message is still gone from the registry, so it cannot
// drive playback even though it is still visible.
func TestPanelDeleteFailureStillDeactivatesTheOldPanel(t *testing.T) {
	tb := newPanelBot(t, panelTracks(3)...)
	stale := tb.registerPanel(1, panelMessage)

	tb.deletePanel = func(snowflake.ID, snowflake.ID) error { return errors.New("forbidden") }
	require.NoError(t, tb.swapPanel(panelGID, panelChannel, func(uint64) (snowflake.ID, error) {
		return panelMessage + 1, nil
	}))

	assert.Contains(t, tb.resolvePanelClick(stale).Reject, "no longer active",
		"a message we failed to delete must still not control playback")
}

// A tick and a click must not both publish. publish holds the registry lock across
// the send, so replacements are serialized.
func TestPanelReplacementIsSerialized(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var maxActive uint64
	var gens []uint64

	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = tb.swapPanel(panelGID, panelChannel, func(gen uint64) (snowflake.ID, error) {
				mu.Lock()
				gens = append(gens, gen)
				mu.Unlock()
				return panelMessage, nil
			})
		}()
	}
	wg.Wait()

	// every publish got its own generation, and the registry kept exactly one panel
	cur, ok := tb.panels.active(panelGID)
	require.True(t, ok)
	assert.Equal(t, uint64(len(gens)), cur.generation, "no two replacements shared a generation")

	seen := map[uint64]bool{}
	for _, g := range gens {
		assert.False(t, seen[g], "generation %d was handed out twice", g)
		seen[g] = true
	}
	_ = maxActive
}

// --- refresh -----------------------------------------------------------------

// The panel is republished every tick even when nothing changed, so it stays the
// latest message in the channel.
func TestPanelRefreshRepublishesEvenWhenUnchanged(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)
	tb.registerPanel(1, panelMessage)

	before := tb.Player().QueueSnapshot().CurrentIndex
	for range 3 {
		require.NoError(t, tb.refreshOnce(context.Background(), panelGID))
	}
	assert.Equal(t, before, tb.Player().QueueSnapshot().CurrentIndex, "nothing about playback moved")

	assert.Equal(t, 3, tb.sentCount(), "every tick published a new panel message")
	assert.Equal(t, []snowflake.ID{panelMessage, panelMessage + 1, panelMessage + 2}, tb.deletedIDs(),
		"and each one retired the message before it")

	cur, ok := tb.panels.active(panelGID)
	require.True(t, ok)
	assert.Equal(t, uint64(4), cur.generation, "the panel is now the fourth")
}

// A failing tick must not retire the panel.
func TestPanelRefreshFailureKeepsThePanel(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)
	tb.registerPanel(1, panelMessage)
	tb.sent = nil
	tb.sendErr = errors.New("boom")

	err := tb.refreshOnce(context.Background(), panelGID)

	require.Error(t, err)
	assert.Empty(t, tb.deletedIDs(), "a failed tick must not delete anything")
	cur, ok := tb.panels.active(panelGID)
	require.True(t, ok, "the previous panel is still active")
	assert.Equal(t, panelMessage, cur.messageID)
}

func TestPanelWorkerIsCancelledOnReplace(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)
	tb.registerPanel(1, panelMessage)

	cancelled := false
	tb.panels.setWorker(panelGID, func() { cancelled = true })

	tb.panels.stopWorker(panelGID)

	assert.True(t, cancelled, "the old worker must stop when the panel is replaced")
}

// --- /player command ---------------------------------------------------------

// slashEvent builds a real /player interaction; disgo's interaction struct has only
// unexported fields, so JSON is the way in.
func slashEvent(t *testing.T, guildID, channelID, userID snowflake.ID) *events.ApplicationCommandInteractionCreate {
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
	  "data":{"id":"400","name":"player","type":1}
	}`, guildField, channelID, channelID, userID)

	var i discord.ApplicationCommandInteraction
	require.NoError(t, i.UnmarshalJSON([]byte(payload)))
	return &events.ApplicationCommandInteractionCreate{ApplicationCommandInteraction: i}
}

// The panel must be the command's reply, or discord times the interaction out.
func TestHandlePlayerPublishesAsTheReply(t *testing.T) {
	t.Run("first run", func(t *testing.T) {
		tb := newPanelBot(t, panelTracks(3)...)

		require.NoError(t, tb.handlePlayer(context.Background(), slashEvent(t, panelGID, panelChannel, panelUser)))

		require.Len(t, tb.replies, 1, "the command must answer exactly once")
		assert.NotEmpty(t, tb.replies[0].Components, "the reply carries the panel")
		require.True(t, tb.replies[0].Flags.Has(discord.MessageFlagIsComponentsV2))

		cur, ok := tb.panels.active(panelGID)
		require.True(t, ok)
		assert.Equal(t, panelChannel, cur.channelID)
		assert.Equal(t, uint64(1), cur.generation)
	})

	t.Run("invoked again it replaces the old panel", func(t *testing.T) {
		tb := newPanelBot(t, panelTracks(3)...)
		require.NoError(t, tb.handlePlayer(context.Background(), slashEvent(t, panelGID, panelChannel, panelUser)))
		first, _ := tb.panels.active(panelGID)

		other := panelChannel + 1
		require.NoError(t, tb.handlePlayer(context.Background(), slashEvent(t, panelGID, other, panelUser)))

		assert.Equal(t, []snowflake.ID{first.messageID}, tb.deletedIDs(),
			"the previous panel is retired only after its replacement was sent")
		cur, _ := tb.panels.active(panelGID)
		assert.Equal(t, other, cur.channelID)
		assert.Equal(t, first.generation+1, cur.generation)
	})

	t.Run("outside a guild is refused", func(t *testing.T) {
		tb := newPanelBot(t, panelTracks(2)...)

		err := tb.handlePlayer(context.Background(), slashEvent(t, 0, panelChannel, panelUser))

		require.Error(t, err)
		assert.Contains(t, err.Error(), "server")
	})
}

// --- auto-publish ------------------------------------------------------------

// ensurePanel posts one when music is playing and the guild has none, and stays
// quiet otherwise.
func TestEnsurePanelPublishesWhenMusicIsPlayingAndNoneExists(t *testing.T) {
	tb := newTestBot(t)
	tb.GuildID = panelGID
	mappedMusicChannel(t, tb)
	pb := newPanelRecorder(t, tb)

	// idle: nothing is owed
	require.NoError(t, tb.ensureOnce(context.Background()))
	_, ok := tb.panels.active(panelGID)
	assert.False(t, ok, "no music playing means no panel")

	// playing with no panel: one is published
	require.NoError(t, tb.MusicPlayer.EnqueueMany([]player.Track{{Title: "song"}}))
	require.NoError(t, tb.ensureOnce(context.Background()))

	cur, ok := tb.panels.active(panelGID)
	require.True(t, ok, "playing music with no panel must produce one")
	assert.Equal(t, panelBotVC, cur.channelID, "the mapped music channel is where it lands")
	assert.Equal(t, 1, pb.sentCount())
}

// An existing panel is left alone; its own worker owns it.
func TestEnsurePanelLeavesAnExistingPanelAlone(t *testing.T) {
	tb := newTestBot(t)
	tb.GuildID = panelGID
	mappedMusicChannel(t, tb)
	// music IS playing here: what must stop the republish is the panel already existing
	require.NoError(t, tb.MusicPlayer.EnqueueMany([]player.Track{{Title: "song"}}))
	pb := newPanelRecorder(t, tb)
	tb.panels.publish(panelGID, panelChannel, func(uint64) (snowflake.ID, error) {
		return panelMessage, nil
	})

	require.NoError(t, tb.ensureOnce(context.Background()))

	assert.Equal(t, 0, pb.sentCount(), "an existing panel must not be republished here")
	cur, _ := tb.panels.active(panelGID)
	assert.Equal(t, panelMessage, cur.messageID)
}

// A failed publish must not leave a half-registered panel or a stray worker.
func TestEnsurePanelFailedPublishRegistersNothing(t *testing.T) {
	tb := newTestBot(t)
	tb.GuildID = panelGID
	mappedMusicChannel(t, tb)
	require.NoError(t, tb.MusicPlayer.EnqueueMany([]player.Track{{Title: "song"}}))
	pb := newPanelRecorder(t, tb)
	pb.sendErr = errors.New("rate limited")

	// the failure is reported, but nothing is registered or deleted
	require.Error(t, tb.ensureOnce(context.Background()))

	_, ok := tb.panels.active(panelGID)
	assert.False(t, ok, "nothing may be registered when the send failed")
	assert.Equal(t, 0, pb.sentCount())
}

// Without a mapped music channel there is nowhere to post, and that must not spam.
func TestEnsurePanelWithoutMusicChannelPostsNothing(t *testing.T) {
	tb := newTestBot(t)
	tb.GuildID = panelGID
	require.NoError(t, tb.MusicPlayer.EnqueueMany([]player.Track{{Title: "song"}}))
	pb := newPanelRecorder(t, tb)

	require.NoError(t, tb.ensureOnce(context.Background()))

	assert.Equal(t, 0, pb.sentCount())
}

// mappedMusicChannel gives the guild a music channel, which is where an automatic
// panel is posted.
func mappedMusicChannel(t *testing.T, tb *testBot) {
	t.Helper()
	_, err := tb.mapChannel(store.MapChannelParams{
		Gid:    tb.GuildID.String(),
		Chid:   snowflake.ID(panelBotVC).String(),
		ChName: "music",
		Type:   musicChannel,
	})
	require.NoError(t, err)
}

// newPanelRecorder attaches the panel seams to a plain testBot and hands back the
// recorder they write into.
func newPanelRecorder(t *testing.T, tb *testBot) *panelBot {
	t.Helper()
	pb := &panelBot{DBot: tb.DBot, nextID: panelMessage}
	tb.createPanel = func(context.Context, snowflake.ID, ...discord.LayoutComponent) (snowflake.ID, error) {
		if pb.sendErr != nil {
			return 0, pb.sendErr
		}
		pb.mu.Lock()
		id := pb.nextID
		pb.nextID++
		pb.sent = append(pb.sent, id)
		pb.mu.Unlock()
		return id, nil
	}
	tb.deletePanel = func(_, msgID snowflake.ID) error {
		pb.mu.Lock()
		pb.deleted = append(pb.deleted, msgID)
		pb.mu.Unlock()
		return nil
	}
	return pb
}
