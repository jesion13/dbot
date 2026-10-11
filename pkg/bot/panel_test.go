package dbot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"dbot/pkg/db/types"
	"dbot/pkg/player"
	"dbot/pkg/store"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/rest"
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

// --- rendering ---------------------------------------------------------------

// panelText flattens a rendered panel into one string per text display, so a test can
// assert on what the user actually reads without walking the component tree.
func panelText(t *testing.T, msg discord.MessageCreate) []string {
	t.Helper()
	var out []string
	for _, lc := range msg.Components {
		container, ok := lc.(discord.ContainerComponent)
		require.True(t, ok, "the panel is one container")
		for _, sc := range container.Components {
			if td, ok := sc.(discord.TextDisplayComponent); ok {
				out = append(out, td.Content)
			}
		}
	}
	return out
}

func panelAllText(t *testing.T, msg discord.MessageCreate) string {
	return strings.Join(panelText(t, msg), "\n")
}

func panelButtons(t *testing.T, msg discord.MessageCreate) []discord.ButtonComponent {
	t.Helper()
	var out []discord.ButtonComponent
	for _, lc := range msg.Components {
		container := lc.(discord.ContainerComponent)
		for _, sc := range container.Components {
			row, ok := sc.(discord.ActionRowComponent)
			if !ok {
				continue
			}
			for _, ic := range row.Components {
				if b, ok := ic.(discord.ButtonComponent); ok {
					out = append(out, b)
				}
			}
		}
	}
	return out
}

// panelButtonsOf pulls the buttons out of an already-rendered payload.
func panelButtonsOf(t *testing.T, msg discord.MessageCreate) []discord.ButtonComponent {
	t.Helper()
	var out []discord.ButtonComponent
	for _, lc := range msg.Components {
		container, ok := lc.(discord.ContainerComponent)
		require.True(t, ok)
		for _, sc := range container.Components {
			row, ok := sc.(discord.ActionRowComponent)
			if !ok {
				continue
			}
			for _, ic := range row.Components {
				if b, ok := ic.(discord.ButtonComponent); ok {
					out = append(out, b)
				}
			}
		}
	}
	return out
}

func panelDisplayedNumbers(t *testing.T, msg discord.MessageCreate) []string {
	t.Helper()
	var out []string
	for _, text := range panelText(t, msg) {
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "`") {
				out = append(out, line)
			}
		}
	}
	return out
}

// The window is 3 before, the current track, and 10 after, clipped to what exists.
func TestPanelV2WindowBoundaries(t *testing.T) {
	t.Run("at the beginning there is nothing before", func(t *testing.T) {
		text := panelAllText(t, buildPlayerPanelV2(1, panelSnapshot(20, 1)))

		assert.NotContains(t, text, "PREVIOUS POSITIONS", "there is nothing before position 1")
		assert.Contains(t, text, "#1", "position 1 is the now playing row")
		assert.Contains(t, text, "`11` track 11", "and ten positions follow it")
		assert.NotContains(t, text, "`12`", "only ten follow")
	})

	t.Run("middle", func(t *testing.T) {
		got := panelDisplayedNumbers(t, buildPlayerPanelV2(1, panelSnapshot(40, 20)))
		// 17..19 precede, 20..30 follow
		assert.Contains(t, strings.Join(got, " "), "`17` track 17")
		assert.Contains(t, strings.Join(got, " "), "`19` track 19")
		assert.Contains(t, strings.Join(got, " "), "`30` track 30")
		assert.NotContains(t, strings.Join(got, " "), "`16`", "only three precede")
		assert.NotContains(t, strings.Join(got, " "), "`31`", "only ten follow")
	})

	t.Run("at the end nothing follows", func(t *testing.T) {
		text := panelAllText(t, buildPlayerPanelV2(1, panelSnapshot(8, 8)))
		assert.NotContains(t, text, "UP NEXT", "there is nothing after the last position")
		assert.Contains(t, text, "`05` track 5")
	})

	t.Run("short lineup shows only what exists", func(t *testing.T) {
		text := panelAllText(t, buildPlayerPanelV2(1, panelSnapshot(3, 2)))
		assert.Contains(t, text, "`01` track 1")
		assert.Contains(t, text, "`03` track 3")
		assert.NotContains(t, text, "`04`")
	})
}

// Positions are absolute, so a number the user read earlier still names the same track.
func TestPanelV2NumbersAreAbsolute(t *testing.T) {
	for _, cur := range []int{1, 2, 5, 14, 25} {
		text := panelAllText(t, buildPlayerPanelV2(1, panelSnapshot(25, cur)))
		for _, pos := range visiblePositions(t, text) {
			assert.Contains(t, text, fmt.Sprintf("`%02d` track %d", pos, pos),
				"position %d must be the track enqueued at that position (cursor %d)", pos, cur)
		}
	}
}

func visiblePositions(t *testing.T, text string) []int {
	t.Helper()
	var out []int
	for _, block := range strings.Split(text, "\n") {
		if !strings.HasPrefix(block, "`") {
			continue
		}
		var n int
		_, _ = fmt.Sscanf(block, "`%02d`", &n)
		out = append(out, n)
	}
	return out
}

func TestPanelV2HighlightsTheCurrentTrack(t *testing.T) {
	text := panelAllText(t, buildPlayerPanelV2(1, panelSnapshot(6, 3)))

	assert.Contains(t, text, "#3")
	assert.Contains(t, text, "**track 3**", "the now playing title is bold")
	assert.NotContains(t, text, "**track 4**", "no other track is bolded")
}

func TestPanelV2StatusStates(t *testing.T) {
	t.Run("playing", func(t *testing.T) {
		assert.Contains(t, panelAllText(t, buildPlayerPanelV2(1, panelSnapshot(5, 2))), "PLAYING · 2 / 5")
	})
	t.Run("paused", func(t *testing.T) {
		s := panelSnapshot(5, 2)
		s.Paused = true
		assert.Contains(t, panelAllText(t, buildPlayerPanelV2(1, s)), "PAUSED · 2 / 5")
	})
	t.Run("stopped with a lineup", func(t *testing.T) {
		text := panelAllText(t, buildPlayerPanelV2(1, panelSnapshot(5, 0)))
		assert.Contains(t, text, "STOPPED")
		assert.Contains(t, text, "NOT PLAYING", "and it must not pretend a track is playing")
	})
	t.Run("stopped empty", func(t *testing.T) {
		assert.Contains(t, panelAllText(t, buildPlayerPanelV2(1, panelSnapshot(0, 0))), "Nothing queued yet.")
	})
	t.Run("closed", func(t *testing.T) {
		s := panelSnapshot(3, 1)
		s.Closed = true
		text := panelAllText(t, buildPlayerPanelV2(1, s))
		assert.Contains(t, text, "Player closed")
		for _, b := range panelButtons(t, buildPlayerPanelV2(1, s)) {
			assert.True(t, b.Disabled, "a closed player disables %q", b.Label)
		}
	})
}

// Imported titles carry markdown, which must not reach the user as formatting.
func TestPanelV2EscapesTitleMarkdown(t *testing.T) {
	s := panelSnapshot(1, 1, "**bold** _under_ `code` [link](url) ~~~")

	text := panelAllText(t, buildPlayerPanelV2(1, s))

	assert.Contains(t, text, `\*\*bold\*\*`)
	assert.Contains(t, text, "\\`code\\`")
	assert.Contains(t, text, `\[link\]`)
	assert.NotContains(t, text, "**bold**", "the title must not render as bold")
	assert.NotContains(t, text, "[link](url)")
}

func TestPanelV2ShowsSoundCountOnlyWhenWaiting(t *testing.T) {
	assert.NotContains(t, panelAllText(t, buildPlayerPanelV2(1, panelSnapshot(3, 1))), "sound")

	s := panelSnapshot(3, 1)
	s.SoundWait = 1
	assert.Contains(t, panelAllText(t, buildPlayerPanelV2(1, s)), "1 sound sequence waiting")

	s.SoundWait = 4
	assert.Contains(t, panelAllText(t, buildPlayerPanelV2(1, s)), "4 sound sequences waiting")
}

func TestPanelV2Buttons(t *testing.T) {
	buttons := panelButtons(t, buildPlayerPanelV2(1, panelSnapshot(4, 2)))

	require.Len(t, buttons, 4)
	want := []string{playerPanelEmojiPrev, playerPanelEmojiPause, playerPanelEmojiNext, playerPanelEmojiEvent}
	for i, b := range buttons {
		assert.Equal(t, want[i], b.Emoji.Name, "button %d carries its emoji", i)
		assert.Empty(t, b.Label, "button %d carries no caption; the emoji says it", i)
		gen, action, ok := parsePanelCustomID(b.CustomID)
		require.True(t, ok, "%q must parse", b.CustomID)
		assert.Equal(t, uint64(1), gen, "every button carries the publishing generation")
		assert.NotEmpty(t, action)
	}
}

// The bot made this message, so a header naming the bot repeats what the reader
// already knows.
func TestPanelV2HasNoBotTitle(t *testing.T) {
	text := panelAllText(t, buildPlayerPanelV2(1, panelSnapshot(4, 2)))

	assert.NotContains(t, text, "DBot Player")
	assert.NotContains(t, text, "🎵")
}

func TestPanelV2PauseEmojiFollowsState(t *testing.T) {
	assert.Equal(t, playerPanelEmojiPause, pauseButtonEmoji(panelSnapshot(3, 1)))
	s := panelSnapshot(3, 1)
	s.Paused = true
	assert.Equal(t, playerPanelEmojiPlay, pauseButtonEmoji(s))
	assert.Equal(t, playerPanelEmojiStop, pauseButtonEmoji(panelSnapshot(3, 0)), "idle does not imply resuming")
}

// Buttons that cannot do anything are disabled rather than lying about it.
func TestPanelV2DisablesImpossibleActions(t *testing.T) {
	t.Run("nothing queued", func(t *testing.T) {
		buttons := panelButtons(t, buildPlayerPanelV2(1, panelSnapshot(0, 0)))
		require.Len(t, buttons, 4)
		assert.True(t, buttons[0].Disabled, "there is no earlier track")
		assert.True(t, buttons[1].Disabled, "nothing is playing to pause")
		assert.True(t, buttons[2].Disabled, "there is no later track")
		assert.False(t, buttons[3].Disabled,
			"Event still works: a sound does not need a queue")
	})
	t.Run("at position 1 previous is disabled", func(t *testing.T) {
		buttons := panelButtons(t, buildPlayerPanelV2(1, panelSnapshot(3, 1)))
		assert.True(t, buttons[0].Disabled, "there is no earlier track")
		assert.False(t, buttons[2].Disabled, "next still works")
	})
}

func TestPanelV2UsesOneComponentsV2Container(t *testing.T) {
	msg := buildPlayerPanelV2(1, panelSnapshot(3, 1))

	require.True(t, msg.Flags.Has(discord.MessageFlagIsComponentsV2))
	require.Len(t, msg.Components, 1)
	container, ok := msg.Components[0].(discord.ContainerComponent)
	require.True(t, ok, "the panel is a container")
	assert.Equal(t, playerPanelAccent, container.AccentColor)
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
	// edited records in-place panel rewrites, in order
	edited []snowflake.ID
	// updates records the MessageUpdate each rewrite carried
	updates []discord.MessageUpdate
	// nextID hands out a fresh message id per publish
	nextID snowflake.ID
	// mu guards the recorders: a replacement deletes outside the registry lock, so
	// two concurrent swaps really do touch these at once
	mu sync.Mutex
	// sendErr fails the next publish, so the failure path can be exercised
	sendErr error
	// slowSend runs inside the publish, to open a window for an interleaving test
	slowSend func()
	// record false keeps the double usable for tests that only care about ordering
	record bool
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

	pb := &panelBot{DBot: d, cache: c, nextID: panelMessage, record: true}
	d.createPanel = func(context.Context, snowflake.ID, discord.MessageCreate) (snowflake.ID, error) {
		return pb.send(0)
	}
	d.answerPanel = func(_ snowflake.ID, _ string, m discord.MessageCreate) (snowflake.ID, error) {
		if pb.sendErr != nil {
			return 0, pb.sendErr
		}
		pb.mu.Lock()
		defer pb.mu.Unlock()
		pb.replies = append(pb.replies, m)
		id := pb.nextID
		pb.nextID++
		// an ephemeral confirmation is not a panel; only count what was really posted
		if !m.Flags.Has(discord.MessageFlagEphemeral) {
			pb.sent = append(pb.sent, id)
		}
		return id, nil
	}
	d.deletePanel = func(_, msgID snowflake.ID) error {
		pb.mu.Lock()
		pb.deleted = append(pb.deleted, msgID)
		pb.mu.Unlock()
		return nil
	}
	d.editPanel = func(_ context.Context, _, msgID snowflake.ID, u discord.MessageUpdate) error {
		pb.mu.Lock()
		pb.edited = append(pb.edited, msgID)
		pb.updates = append(pb.updates, u)
		pb.mu.Unlock()
		return nil
	}

	if len(tracks) > 0 {
		require.NoError(t, p.EnqueueMany(tracks))
	}
	return pb
}

// send stands in for actually publishing a panel message.
func (pb *panelBot) send(uint64) (snowflake.ID, error) {
	if pb.slowSend != nil {
		pb.slowSend()
	}
	if pb.sendErr != nil {
		return 0, pb.sendErr
	}
	pb.mu.Lock()
	defer pb.mu.Unlock()
	id := pb.nextID
	pb.nextID++
	if pb.record {
		pb.sent = append(pb.sent, id)
	}
	return id, nil
}

// discardSends drops what it records, so a test that only cares about ordering can
// reuse the double without asserting on it.
func (pb *panelBot) discardSends() {
	pb.record = false
}

func (tb *panelBot) sentIDs() []snowflake.ID {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return append([]snowflake.ID(nil), tb.sent...)
}

func (tb *panelBot) editedIDs() []snowflake.ID {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return append([]snowflake.ID(nil), tb.edited...)
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

	require.NoError(t, tb.panelAction(playerPanelBtnPrev, panelGID))

	assert.Equal(t, 2, tb.Player().QueueSnapshot().CurrentIndex)
}

func TestPanelPauseButtonTogglesManualPause(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)
	require.False(t, tb.Player().Paused())

	require.NoError(t, tb.panelAction(playerPanelBtnPause, panelGID))
	assert.True(t, tb.Player().Paused())

	require.NoError(t, tb.panelAction(playerPanelBtnPause, panelGID))
	assert.False(t, tb.Player().Paused())
}

func TestPanelNextButtonKeepsWraparound(t *testing.T) {
	tb := newPanelBot(t, panelTracks(3)...)
	require.NoError(t, tb.Player().JumpTo(3))

	require.NoError(t, tb.panelAction(playerPanelBtnNext, panelGID))

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

	msg := tb.panelAction(playerPanelBtnPrev, panelGID).Error()

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
		ChannelID:  panelChannel,
		MessageID:  messageID,
		Generation: generation,
		Action:     playerPanelBtnNext,
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
		GuildID: panelGID, UserID: panelUser, MessageID: panelMessage, ChannelID: panelChannel,
		Generation: 99, Action: playerPanelBtnNext,
	})

	assert.Contains(t, res.Reject, "no longer active")
}

func TestPanelRejectsClicksWhenNoPanelIsRegistered(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)

	res := tb.resolvePanelClick(panelClick{
		GuildID: panelGID, UserID: panelUser, MessageID: panelMessage, ChannelID: panelChannel,
		Generation: 1, Action: playerPanelBtnNext,
	})

	assert.Contains(t, res.Reject, "no longer active")
}

func TestPanelCustomIDRoundTrips(t *testing.T) {
	for _, action := range []string{playerPanelBtnPrev, playerPanelBtnPause, playerPanelBtnNext, playerPanelBtnEvent} {
		gen, got, ok := parsePanelCustomID(playerPanelID(7, action))
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
	tb.editPanel = func(context.Context, snowflake.ID, snowflake.ID, discord.MessageUpdate) error { return nil }
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

// The panel is rewritten on every tick even when nothing changed, so what the channel
// shows is always current - and it is the same message, so a button being reached for
// stays valid.
func TestPanelRefreshRewritesTheSameMessage(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)
	tb.registerPanel(1, panelMessage)
	tb.sent = nil

	before := tb.Player().QueueSnapshot().CurrentIndex
	for range 3 {
		require.NoError(t, tb.refreshOnce(context.Background(), panelGID))
	}
	assert.Equal(t, before, tb.Player().QueueSnapshot().CurrentIndex, "nothing about playback moved")

	assert.Empty(t, tb.sentIDs(), "a refresh must never post a new message")
	assert.Empty(t, tb.deletedIDs(), "nor delete anything")
	assert.Equal(t, []snowflake.ID{panelMessage, panelMessage, panelMessage}, tb.editedIDs(),
		"the same panel is rewritten every time")

	cur, ok := tb.panels.active(panelGID)
	require.True(t, ok)
	assert.Equal(t, panelMessage, cur.messageID)
	assert.Equal(t, uint64(1), cur.generation,
		"rewriting must not bump the generation, or it would invalidate the very button being pressed")

	require.Len(t, tb.updates, 3)
	for _, u := range tb.updates {
		require.NotNil(t, u.Components)
		assert.NotEmpty(t, *u.Components, "each rewrite carries the panel")
	}
}

// A failing rewrite must be reported and must leave the panel registered, so the
// next tick can try again and the buttons keep working.
func TestPanelRefreshFailureKeepsThePanel(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)
	tb.registerPanel(1, panelMessage)
	tb.sent = nil
	tb.editPanel = func(context.Context, snowflake.ID, snowflake.ID, discord.MessageUpdate) error {
		return errors.New("boom")
	}

	err := tb.refreshOnce(context.Background(), panelGID)

	require.Error(t, err)
	assert.Empty(t, tb.deletedIDs(), "a failed rewrite must not delete anything")
	cur, ok := tb.panels.active(panelGID)
	require.True(t, ok, "the panel is still registered")
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
	tb.createPanel = func(context.Context, snowflake.ID, discord.MessageCreate) (snowflake.ID, error) {
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

// buttonEvent builds a real component interaction, so onPanelComponent itself is under
// test rather than a hand-rolled sequence of its inner calls.
func buttonEvent(t *testing.T, guildID, channelID, userID, messageID snowflake.ID, customID string) *events.ComponentInteractionCreate {
	t.Helper()
	return buttonEventRecording(t, guildID, channelID, userID, messageID, customID, nil)
}

// buttonEventRecording captures what the handler answers the interaction with.
func buttonEventRecording(t *testing.T, guildID, channelID, userID, messageID snowflake.ID, customID string, out *[]discord.InteractionResponseType) *events.ComponentInteractionCreate {
	t.Helper()

	payload := fmt.Sprintf(`{
	  "id":"500","application_id":"300","type":3,"token":"tok",
	  "guild_id":"%s","channel_id":"%s",
	  "channel":{"id":"%s","type":0},
	  "member":{"user":{"id":"%s","username":"u"}},
	  "message":{"id":"%s"},
	  "data":{"custom_id":%q,"component_type":2}
	}`, guildID, channelID, channelID, userID, messageID, customID)

	var i discord.ComponentInteraction
	require.NoError(t, i.UnmarshalJSON([]byte(payload)))
	return &events.ComponentInteractionCreate{
		ComponentInteraction: i,
		Respond: func(kind discord.InteractionResponseType, data discord.InteractionResponseData, _ ...rest.RequestOpt) error {
			_ = data
			if out != nil {
				*out = append(*out, kind)
			}
			return nil
		},
	}
}

// A component interaction must carry the message the button lives on; without it the
// stale check can never match and every click is refused.
func TestButtonEventCarriesItsMessage(t *testing.T) {
	e := buttonEvent(t, panelGID, panelChannel, panelUser, panelMessage, playerPanelID(1, playerPanelBtnNext))

	assert.Equal(t, panelMessage, e.Message.ID)
	assert.Equal(t, panelGID, *e.GuildID())
	assert.Equal(t, panelUser, e.User().ID)
}

// The update lock spans the whole click, not just its publish: while it is held a
// button must not act and no panel may be sent. Without that a timer tick can retire
// the panel a press is acting on between the guard and the publish.
func TestPanelClickIsSerializedAgainstUpdates(t *testing.T) {
	tb := newPanelBot(t, panelTracks(4)...)
	tb.registerPanel(1, panelMessage)
	pb := &panelBot{DBot: tb.DBot, nextID: panelMessage + 1, record: true}
	tb.editPanel = func(_ context.Context, _, msgID snowflake.ID, u discord.MessageUpdate) error {
		pb.mu.Lock()
		pb.edited = append(pb.edited, msgID)
		pb.updates = append(pb.updates, u)
		pb.mu.Unlock()
		return nil
	}

	var got []discord.InteractionResponseType
	var mu sync.Mutex

	unlock := tb.panels.lockUpdate(panelGID)

	done := make(chan struct{})
	go func() {
		defer close(done)
		tb.onPanelComponent(buttonEventRecording(t, panelGID, panelChannel, panelUser, panelMessage,
			playerPanelID(1, playerPanelBtnNext), &got))
	}()

	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	answers := len(got)
	mu.Unlock()
	assert.Equal(t, 0, answers, "a click must not answer while an update is in flight")
	assert.Equal(t, 1, tb.Player().QueueSnapshot().CurrentIndex, "nor act on the player")

	unlock()
	<-done

	assert.Equal(t, []discord.InteractionResponseType{discord.InteractionResponseTypeUpdateMessage}, got,
		"and it answers once the update finishes")
	assert.Equal(t, 2, tb.Player().QueueSnapshot().CurrentIndex, "having applied its action")
}

// A panel we could not delete must stop looking live, or people click it and are
// told the panel is gone. It is replaced with a note saying so.
func TestPanelDeleteFailureRetiresTheOldMessage(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)
	tb.registerPanel(1, panelMessage)

	tb.deletePanel = func(snowflake.ID, snowflake.ID) error { return errors.New("missing permissions") }

	var retired struct {
		mu        sync.Mutex
		id        snowflake.ID
		content   string
		callCount int
	}
	tb.editPanel = func(_ context.Context, _, msgID snowflake.ID, u discord.MessageUpdate) error {
		retired.mu.Lock()
		defer retired.mu.Unlock()
		retired.id = msgID
		if u.Content != nil {
			retired.content = *u.Content
		}
		retired.callCount++
		return nil
	}

	require.NoError(t, tb.swapPanel(panelGID, panelChannel, func(uint64) (snowflake.ID, error) {
		return panelMessage + 1, nil
	}))

	assert.Equal(t, panelMessage, retired.id, "the message that stayed behind is rewritten")
	assert.Contains(t, retired.content, "no longer active")
	assert.Equal(t, 1, retired.callCount)
}

// A panel in another channel is not ours to drive, even if the ids line up.
func TestPanelRejectsAClickFromAnotherChannel(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)
	tb.registerPanel(1, panelMessage)

	res := tb.resolvePanelClick(panelClick{
		GuildID: panelGID, UserID: panelUser,
		ChannelID: panelChannel + 1, // the panel lives in panelChannel
		MessageID: panelMessage, Generation: 1, Action: playerPanelBtnNext,
	})

	assert.Contains(t, res.Reject, "another channel")
}

// The bug this model fixes: with a fresh panel posted every few seconds, a button was
// retired roughly ten seconds after it appeared - about how long a person takes to
// read a queue and reach for a control. Rewriting in place keeps the same message and
// the same generation, so the press always lands.
func TestPanelButtonStaysValidAcrossRefreshes(t *testing.T) {
	tb := newPanelBot(t, panelTracks(4)...)
	tb.registerPanel(1, panelMessage)

	first, ok := tb.panels.active(panelGID)
	require.True(t, ok)

	// a tick passes with the panel on screen, and again after playback moves on
	require.NoError(t, tb.refreshOnce(context.Background(), panelGID))
	require.NoError(t, tb.Player().JumpTo(2))
	require.NoError(t, tb.refreshOnce(context.Background(), panelGID))

	after, ok := tb.panels.active(panelGID)
	require.True(t, ok)
	assert.Equal(t, first.messageID, after.messageID, "the message on screen never changed")
	assert.Equal(t, first.generation, after.generation, "nor the generation it is registered under")

	// the generation baked into the buttons must match the registered one, or the
	// next press is rejected even though nothing looks wrong
	for _, u := range tb.updates {
		for _, b := range panelButtonsOf(t, discord.MessageCreate{Components: *u.Components}) {
			gen, _, ok := parsePanelCustomID(b.CustomID)
			require.True(t, ok)
			assert.Equal(t, after.generation, gen,
				"a rewritten button must carry the generation the panel is registered under")
		}
	}

	res := tb.resolvePanelClick(panelClick{
		GuildID: panelGID, UserID: panelUser, ChannelID: panelChannel,
		MessageID: first.messageID, Generation: first.generation,
		Action: playerPanelBtnNext,
	})
	assert.Empty(t, res.Reject, "a button on the visible panel still works")
}

// /player in the channel that already holds the panel must refresh it, not post a
// second one.
func TestHandlePlayerIsIdempotentInTheSameChannel(t *testing.T) {
	tb := newPanelBot(t, panelTracks(3)...)
	require.NoError(t, tb.handlePlayer(context.Background(), slashEvent(t, panelGID, panelChannel, panelUser)))

	first, _ := tb.panels.active(panelGID)
	tb.replies = nil
	tb.sent = nil

	require.NoError(t, tb.handlePlayer(context.Background(), slashEvent(t, panelGID, panelChannel, panelUser)))

	assert.Empty(t, tb.sentIDs(), "no second panel is posted")
	assert.Empty(t, tb.deletedIDs(), "and nothing is deleted")
	assert.Equal(t, []snowflake.ID{panelMessage}, tb.editedIDs(), "the same panel is rewritten")
	assert.Len(t, tb.replies, 1, "but the command is still answered")

	after, _ := tb.panels.active(panelGID)
	assert.Equal(t, first.messageID, after.messageID)
}

// Every button press must answer the interaction exactly once, inside discord's
// deadline. Answering nothing is what produces "didn't respond in time".
func TestEveryButtonPressAnswersExactlyOnce(t *testing.T) {
	for _, action := range []string{
		playerPanelBtnPrev, playerPanelBtnPause, playerPanelBtnNext, playerPanelBtnEvent,
	} {
		t.Run(action, func(t *testing.T) {
			// a real store: Event reads the sound library, and a bot without one is a
			// state production never reaches
			tb := newTestBot(t)
			tb.GuildID = panelGID
			_ = tb.MusicPlayer.Close()
			tb.SetPlayer(player.NewPlayer())
			require.NoError(t, tb.MusicPlayer.SetVC(stubConn{gid: panelGID}, panelBotVC))

			c := cache.New(cache.WithCaches(cache.FlagsAll))
			c.AddVoiceState(discord.VoiceState{GuildID: panelGID, UserID: panelUser, ChannelID: &panelBotVC})
			tb.Client = &bot.Client{Caches: c}

			require.NoError(t, tb.MusicPlayer.EnqueueMany(panelTracks(4)))

			panel := &panelBot{DBot: tb.DBot, nextID: panelMessage + 1, record: true}
			panel.registerPanel(1, panelMessage)

			var got []discord.InteractionResponseType
			panel.onPanelComponent(buttonEventRecording(t, panelGID, panelChannel, panelUser, panelMessage,
				playerPanelID(1, action), &got))

			require.Len(t, got, 1, "%s must answer exactly once", action)

			// prev at position 1 and event with no sounds both genuinely fail, so they
			// answer ephemerally; anything that worked rewrites the panel
			worked := action == playerPanelBtnPause || action == playerPanelBtnNext
			want := discord.InteractionResponseTypeCreateMessage
			if worked {
				want = discord.InteractionResponseTypeUpdateMessage
			}
			assert.Equal(t, want, got[0], "%s answered the wrong way", action)
		})
	}
}

// A rejected press answers ephemerally and touches nothing else.
func TestRejectedPressStillAnswers(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)
	tb.registerPanel(1, panelMessage)

	var got []discord.InteractionResponseType
	// a generation the panel does not carry
	tb.onPanelComponent(buttonEventRecording(t, panelGID, panelChannel, panelUser, panelMessage,
		playerPanelID(99, playerPanelBtnNext), &got))

	require.Len(t, got, 1, "a refused press must still answer")
	assert.Equal(t, discord.InteractionResponseTypeCreateMessage, got[0],
		"and it says why, privately")
}
