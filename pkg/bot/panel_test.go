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
	"github.com/disgoorg/disgo/voice"
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

// stubConn is a voice connection the queue listener never writes to, but which
// still answers the calls a closing player makes.
type stubConn struct {
	voice.Conn
	gid snowflake.ID
}

func (s stubConn) GuildID() snowflake.ID { return s.gid }

func (s stubConn) SetSpeaking(context.Context, voice.SpeakingFlags) error { return nil }

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
	// followups records post-acknowledgement replies to the user
	followups []discord.MessageCreate
	// superseded makes panelIsNewest report that something was posted after the panel
	superseded bool
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
	d.newestPanel = func(_, msgID snowflake.ID) (bool, error) {
		pb.mu.Lock()
		defer pb.mu.Unlock()
		return !pb.superseded, nil
	}
	d.followupPanel = func(_ string, m discord.MessageCreate) error {
		pb.mu.Lock()
		pb.followups = append(pb.followups, m)
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

	require.NoError(t, tb.panelAction(playerPanelBtnPrev, panelGID, panelUser))

	assert.Equal(t, 2, tb.Player().QueueSnapshot().CurrentIndex)
}

func TestPanelPauseButtonTogglesManualPause(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)
	require.False(t, tb.Player().Paused())

	require.NoError(t, tb.panelAction(playerPanelBtnPause, panelGID, panelUser))
	assert.True(t, tb.Player().Paused())

	require.NoError(t, tb.panelAction(playerPanelBtnPause, panelGID, panelUser))
	assert.False(t, tb.Player().Paused())
}

func TestPanelNextButtonKeepsWraparound(t *testing.T) {
	tb := newPanelBot(t, panelTracks(3)...)
	require.NoError(t, tb.Player().JumpTo(3))

	require.NoError(t, tb.panelAction(playerPanelBtnNext, panelGID, panelUser))

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

	msg := tb.panelAction(playerPanelBtnPrev, panelGID, panelUser).Error()

	assert.Contains(t, msg, "earlier track", "an empty lineup has nothing to go back to")
}

func TestPanelUnknownActionIsRejected(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)

	err := tb.panelAction("nonsense", panelGID, panelUser)

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

// The event button is the one action that may be pressed with no connection: it
// joins the clicker's channel itself. A panel the bot has already left must therefore
// still take the press.
func TestPanelEventClickSurvivesNoVoiceConnection(t *testing.T) {
	tb := newPanelBot(t)
	click := tb.registerPanel(1, panelMessage)
	click.Action = playerPanelBtnEvent
	tb.SetPlayer(player.NewPlayer()) // a fresh player has no connection

	assert.Empty(t, tb.resolvePanelClick(click).Reject)
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
	require.NoError(t, tb.swapPanel(context.Background(), panelGID, panelChannel, func(uint64) (snowflake.ID, error) {
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
	err := tb.swapPanel(context.Background(), panelGID, panelChannel, func(uint64) (snowflake.ID, error) {
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

	require.NoError(t, tb.swapPanel(context.Background(), panelGID, panelChannel, func(uint64) (snowflake.ID, error) {
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
	require.NoError(t, tb.swapPanel(context.Background(), panelGID, panelChannel, func(uint64) (snowflake.ID, error) {
		return panelMessage + 1, nil
	}))

	assert.Contains(t, tb.resolvePanelClick(stale).Reject, "no longer active",
		"a message we failed to delete must still not control playback")
}

// Serialization is the per-guild update lock every caller holds; publish itself only
// guarantees a generation is never handed out twice under that lock.
func TestPanelReplacementIsSerialized(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)

	var mu sync.Mutex
	var gens []uint64

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := tb.panels.lockUpdate(panelGID)
			defer unlock()

			_ = tb.swapPanel(context.Background(), panelGID, panelChannel, func(gen uint64) (snowflake.ID, error) {
				mu.Lock()
				gens = append(gens, gen)
				mu.Unlock()
				return panelMessage, nil
			})
		}()
	}
	wg.Wait()

	cur, ok := tb.panels.active(panelGID)
	require.True(t, ok)
	assert.Equal(t, uint64(len(gens)), cur.generation,
		"every replacement advanced the generation exactly once")

	seen := map[uint64]bool{}
	for _, g := range gens {
		assert.False(t, seen[g], "generation %d was handed out twice", g)
		seen[g] = true
	}
}

// --- refresh -----------------------------------------------------------------

// While the panel is still the newest message in the channel a tick rewrites it in
// place: the message id and generation do not move, so every button stays valid.
func TestPanelRefreshEditsWhileThePanelIsNewest(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)
	tb.registerPanel(1, panelMessage)
	tb.sent = nil
	tb.deleted = nil

	first, _ := tb.panels.active(panelGID)

	require.NoError(t, tb.refreshOnce(context.Background(), panelGID))

	after, ok := tb.panels.active(panelGID)
	require.True(t, ok)
	assert.Equal(t, first.messageID, after.messageID, "no new message was posted")
	assert.Equal(t, first.generation, after.generation, "nor did the generation move")
	assert.Empty(t, tb.sentIDs(), "nothing sent")
	assert.Empty(t, tb.deletedIDs(), "nothing deleted")
	assert.Equal(t, []snowflake.ID{panelMessage}, tb.editedIDs(), "the panel was rewritten in place")
}

// Once something else is posted after the panel, it is no longer the newest message,
// so a tick moves it back to the bottom.
func TestPanelRefreshRepublishesOnceSuperseded(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)
	tb.registerPanel(1, panelMessage)
	tb.sent = nil
	tb.deleted = nil

	first, _ := tb.panels.active(panelGID)
	tb.superseded = true

	require.NoError(t, tb.refreshOnce(context.Background(), panelGID))

	after, ok := tb.panels.active(panelGID)
	require.True(t, ok)
	assert.NotEqual(t, first.messageID, after.messageID, "a new message was posted")
	assert.Equal(t, first.generation+1, after.generation, "and the generation moved on")
	assert.Equal(t, 1, tb.sentCount())
	assert.Equal(t, []snowflake.ID{panelMessage}, tb.deletedIDs(), "retiring the old one")
}

// A failed publish keeps the previous panel registered and usable; nothing is deleted.
func TestPanelRefreshFailureKeepsThePanel(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)
	tb.registerPanel(1, panelMessage)
	tb.sent = nil
	tb.deleted = nil
	tb.superseded = true // force the republish path
	tb.sendErr = errors.New("boom")

	err := tb.refreshOnce(context.Background(), panelGID)
	tb.sendErr = nil

	require.Error(t, err)
	assert.Empty(t, tb.deletedIDs(), "a failed publish must not retire anything")
	cur, ok := tb.panels.active(panelGID)
	require.True(t, ok, "the previous panel is still active")
	assert.Equal(t, panelMessage, cur.messageID)
	assert.Equal(t, uint64(1), cur.generation, "and keeps its generation")
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
	pb := newPanelRecorder(t, tb)
	pb.sendErr = errors.New("rate limited")

	// A track with no media fails to play and is gone within a few milliseconds, so
	// ensureOnce's "is anything playing" guard can return before the failing send is
	// ever reached. Queue a fresh one per attempt and stop as soon as the send is
	// actually exercised, rather than relying on the window being short.
	var err error
	for range 50 {
		require.NoError(t, tb.MusicPlayer.EnqueueMany([]player.Track{{Title: "song"}}))
		if err = tb.ensureOnce(context.Background()); err != nil {
			break
		}
	}

	// the failure is reported, but nothing is registered or deleted
	require.Error(t, err, "the failing publish must be reported")

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
	tb.followupPanel = func(_ string, m discord.MessageCreate) error {
		pb.mu.Lock()
		defer pb.mu.Unlock()
		pb.followups = append(pb.followups, m)
		return nil
	}
	tb.newestPanel = func(_, _ snowflake.ID) (bool, error) { return true, nil }
	tb.editPanel = func(_ context.Context, _, msgID snowflake.ID, u discord.MessageUpdate) error {
		pb.mu.Lock()
		pb.edited = append(pb.edited, msgID)
		pb.updates = append(pb.updates, u)
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
	pb := tb

	var got []discord.InteractionResponseType
	var mu sync.Mutex

	unlock := tb.panels.lockUpdate(panelGID)

	done := make(chan struct{})
	go func() {
		defer close(done)
		tb.onPanelComponent(context.Background(), buttonEventRecording(t, panelGID, panelChannel, panelUser, panelMessage,
			playerPanelID(1, playerPanelBtnNext), &got))
	}()

	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	answers := len(got)
	mu.Unlock()
	assert.Equal(t, 0, answers, "a click must not answer while an update is in flight")
	assert.Equal(t, 1, tb.Player().QueueSnapshot().CurrentIndex, "nor act on the player")
	assert.Empty(t, pb.editedIDs(), "nor rewrite the panel")

	unlock()
	<-done

	assert.Equal(t, []discord.InteractionResponseType{discord.InteractionResponseTypeDeferredUpdateMessage}, got,
		"and it acknowledges exactly once when the update finishes")
	assert.Equal(t, 2, tb.Player().QueueSnapshot().CurrentIndex, "having applied its action")
	// the panel is still the newest message, so the press rewrites it rather than
	// retiring it - which is what keeps the next press working
	assert.Equal(t, []snowflake.ID{panelMessage}, pb.editedIDs())
	assert.Empty(t, pb.sentIDs(), "and publishes nothing new")
}

// A panel we could not delete must stop looking live, or people click it and are
// told the panel is gone. It is replaced with a note saying so.
func TestPanelDeleteFailureRetiresTheOldMessage(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)
	stale := tb.registerPanel(1, panelMessage)

	tb.deletePanel = func(snowflake.ID, snowflake.ID) error { return errors.New("missing permissions") }

	var retired struct {
		mu      sync.Mutex
		id      snowflake.ID
		updates []discord.MessageUpdate
	}
	tb.editPanel = func(_ context.Context, _, msgID snowflake.ID, u discord.MessageUpdate) error {
		retired.mu.Lock()
		defer retired.mu.Unlock()
		retired.id = msgID
		retired.updates = append(retired.updates, u)
		return nil
	}

	require.NoError(t, tb.swapPanel(context.Background(), panelGID, panelChannel, func(uint64) (snowflake.ID, error) {
		return panelMessage + 1, nil
	}))

	require.NotEmpty(t, retired.updates, "the message that stayed behind is rewritten")
	assert.Equal(t, panelMessage, retired.id)

	u := retired.updates[0]
	require.NotNil(t, u.Flags)
	assert.True(t, u.Flags.Has(discord.MessageFlagIsComponentsV2),
		"a Components V2 message rejects a plain content update, so the retirement must be V2 too")
	assert.Nil(t, u.Content, "no plain content may be sent alongside V2 components")
	require.NotNil(t, u.Components)

	rendered := discord.MessageCreate{Components: *u.Components}
	assert.Contains(t, strings.Join(panelText(t, rendered), "\n"), "no longer active")
	for _, b := range panelButtonsOf(t, rendered) {
		assert.True(t, b.Disabled, "a retired panel must not look clickable")
	}

	assert.Contains(t, tb.resolvePanelClick(stale).Reject, "no longer active",
		"and its buttons are refused whether or not the retirement edit worked")
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

// Every /player publishes a replacement, including in the channel that already holds
// one, so the panel is always the newest message there.
func TestHandlePlayerAlwaysPublishesAReplacement(t *testing.T) {
	tb := newPanelBot(t, panelTracks(3)...)
	require.NoError(t, tb.handlePlayer(context.Background(), slashEvent(t, panelGID, panelChannel, panelUser)))

	first, _ := tb.panels.active(panelGID)
	require.Equal(t, panelMessage, first.messageID)

	tb.replies = nil
	require.NoError(t, tb.handlePlayer(context.Background(), slashEvent(t, panelGID, panelChannel, panelUser)))

	after, ok := tb.panels.active(panelGID)
	require.True(t, ok)
	assert.NotEqual(t, first.messageID, after.messageID, "a replacement was published")
	assert.Equal(t, first.generation+1, after.generation)
	assert.Len(t, tb.replies, 1, "and the command still answers the interaction")
	assert.Equal(t, []snowflake.ID{first.messageID}, tb.deletedIDs(), "retiring the one it replaced")
}

// second one.

// Every button press must answer the interaction exactly once. disgo's Acknowledge
// sends response type 2, which discord no longer accepts, so the silent deferred
// update is the ack that actually works - and it must be the only initial response.
func TestEveryButtonPressAnswersExactlyOnce(t *testing.T) {
	for _, action := range []string{
		playerPanelBtnPrev, playerPanelBtnPause, playerPanelBtnNext, playerPanelBtnEvent,
	} {
		t.Run(action, func(t *testing.T) {
			tb := newTestBot(t)
			tb.GuildID = panelGID
			_ = tb.MusicPlayer.Close()
			tb.SetPlayer(player.NewPlayer())
			require.NoError(t, tb.MusicPlayer.SetVC(stubConn{gid: panelGID}, panelBotVC))

			c := cache.New(cache.WithCaches(cache.FlagsAll))
			c.AddVoiceState(discord.VoiceState{GuildID: panelGID, UserID: panelUser, ChannelID: &panelBotVC})
			tb.Client = &bot.Client{Caches: c}
			require.NoError(t, tb.MusicPlayer.EnqueueMany(panelTracks(4)))

			panel := newPanelRecorder(t, tb)
			panel.registerPanel(1, panelMessage)

			var got []discord.InteractionResponseType
			panel.onPanelComponent(context.Background(), buttonEventRecording(t, panelGID, panelChannel,
				panelUser, panelMessage, playerPanelID(1, action), &got))

			require.Len(t, got, 1, "%s must send exactly one initial response", action)
			assert.Equal(t, discord.InteractionResponseTypeDeferredUpdateMessage, got[0],
				"%s must acknowledge with the deferred update", action)
		})
	}
}

// An action that fails after the acknowledgement must not send a second initial
// response; the user is told through the follow-up instead.
func TestFailedActionUsesAFollowupNotASecondResponse(t *testing.T) {
	tb := newPanelBot(t, panelTracks(4)...)
	pb := tb
	pb.registerPanel(1, panelMessage)

	var got []discord.InteractionResponseType
	// prev at position 1 has nowhere to go back to
	tb.onPanelComponent(context.Background(), buttonEventRecording(t, panelGID, panelChannel,
		panelUser, panelMessage, playerPanelID(1, playerPanelBtnPrev), &got))

	assert.Equal(t, []discord.InteractionResponseType{discord.InteractionResponseTypeDeferredUpdateMessage}, got,
		"only the acknowledgement")
	require.NotEmpty(t, pb.followups, "the failure is reported through the follow-up")
	assert.Contains(t, pb.followups[0].Content, "earlier track")
	assert.True(t, pb.followups[0].Flags.Has(discord.MessageFlagEphemeral), "privately")
}

// A rejected press answers ephemerally and touches nothing else.
func TestRejectedPressStillAnswers(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)
	tb.registerPanel(1, panelMessage)

	var got []discord.InteractionResponseType
	// a generation the panel does not carry
	tb.onPanelComponent(context.Background(), buttonEventRecording(t, panelGID, panelChannel, panelUser, panelMessage,
		playerPanelID(99, playerPanelBtnNext), &got))

	require.Len(t, got, 1, "a refused press must still answer")
	assert.Equal(t, discord.InteractionResponseTypeCreateMessage, got[0],
		"and it says why, privately")
}

// A slow Discord request for one guild must not hold the registry's global mutex and
// block every other guild. publish releases it around the send.
func TestSlowGuildDoesNotBlockAnother(t *testing.T) {
	var r panelRegistry
	slow, fast := snowflake.ID(1), snowflake.ID(2)

	started := make(chan struct{})
	release := make(chan struct{})
	slowDone := make(chan struct{})
	go func() {
		defer close(slowDone)
		_, _, _ = r.publish(slow, panelChannel, func(uint64) (snowflake.ID, error) {
			close(started)
			<-release
			return 100, nil
		})
	}()
	<-started

	acquired := make(chan struct{})
	go func() {
		unlock := r.lockUpdate(fast)
		close(acquired)
		unlock()
	}()

	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		close(release)
		<-slowDone
		t.Fatal("another guild was blocked by a slow request for a different guild")
	}

	// the slow guild still completes and registers normally
	close(release)
	<-slowDone

	cur, ok := r.active(slow)
	require.True(t, ok)
	assert.Equal(t, snowflake.ID(100), cur.messageID)

	// and its own update lock serializes it, as before
	unlock := r.lockUpdate(slow)
	unlock()
}

// --- persistence -------------------------------------------------------------

// The panel's identity has to outlive the process, or a restart leaves an orphan no
// code will ever delete.
func TestPanelIdentitySurvivesARestart(t *testing.T) {
	tb := newTestBot(t)
	tb.GuildID = panelGID
	tb.bindPanelStore(tb.Ctx)

	pb := newPanelRecorder(t, tb)
	ctx := context.Background()

	require.NoError(t, tb.swapPanel(ctx, panelGID, panelChannel, func(uint64) (snowflake.ID, error) {
		return pb.send(0)
	}))

	row, err := tb.Store.GetPlayerPanel(tb.Ctx, panelGID.String())
	require.NoError(t, err, "the published panel must be recorded")
	assert.Equal(t, panelChannel.String(), row.ChannelID)
	assert.Equal(t, pb.nextID-1, snowflake.MustParse(row.MessageID))

	// a fresh bot against the same database, as after a restart
	restarted := &DBot{Ctx: tb.Ctx, Store: tb.Store, GuildID: panelGID}
	restarted.bindPanelStore(tb.Ctx)
	var deleted []snowflake.ID
	restarted.deletePanel = func(_, msgID snowflake.ID) error {
		deleted = append(deleted, msgID)
		return nil
	}

	restarted.cleanupOrphanPanel(tb.Ctx, panelGID)

	assert.Equal(t, []snowflake.ID{pb.nextID - 1}, deleted,
		"the new process must find and delete the panel the old one left")

	_, err = tb.Store.GetPlayerPanel(tb.Ctx, panelGID.String())
	assert.Error(t, err, "and the record is cleared, so it is not chased again")
}

// A panel that was already deleted by hand must not turn into a startup error, and
// must not be chased on every restart.
func TestOrphanCleanupToleratesAMissingPanel(t *testing.T) {
	tb := newTestBot(t)
	tb.GuildID = panelGID
	tb.bindPanelStore(tb.Ctx)
	tb.deletePanel = func(snowflake.ID, snowflake.ID) error {
		return errors.New("unknown message")
	}
	tb.cleanupOrphanPanel(tb.Ctx, panelGID) // nothing recorded: a no-op
	tb.cleanupOrphanPanel(tb.Ctx, panelGID)

	var attempts int
	tb.deletePanel = func(snowflake.ID, snowflake.ID) error {
		attempts++
		return errors.New("unknown message")
	}
	_, err := tb.Store.UpsertPlayerPanel(tb.Ctx, store.UpsertPlayerPanelParams{
		GuildID: panelGID.String(), ChannelID: panelChannel.String(),
		MessageID: panelMessage.String(), Generation: 1,
	})
	require.NoError(t, err)

	tb.cleanupOrphanPanel(tb.Ctx, panelGID)

	assert.Equal(t, 1, attempts, "one attempt, no retry loop")
}

// Recording must never fail a publish that already succeeded.
func TestPanelRecordFailureDoesNotBreakThePublish(t *testing.T) {
	tb := newPanelBot(t, panelTracks(2)...)
	tb.rememberPanelRow = func(context.Context, snowflake.ID, panelEntry) error {
		return errors.New("db down")
	}

	require.NoError(t, tb.swapPanel(context.Background(), panelGID, panelChannel,
		func(uint64) (snowflake.ID, error) { return panelMessage, nil }))

	cur, ok := tb.panels.active(panelGID)
	require.True(t, ok, "the panel is live regardless")
	assert.Equal(t, panelMessage, cur.messageID)
}

// --- retained helpers --------------------------------------------------------

// displayTitle keeps an imported title on one line. This used to serve the text queue
// commands; the panel renderer relies on it now.
func TestDisplayTitleCollapsesWhitespace(t *testing.T) {
	assert.Equal(t,
		"Hiszpańskie Dziewczyny Szanty - Bitwa + lyrics Czarnobrody Kapitan",
		displayTitle("Hiszpańskie Dziewczyny\n\nSzanty - Bitwa + lyrics\nCzarnobrody Kapitan"))
	assert.Equal(t, "normal Title", displayTitle("normal\tTitle"))
	assert.Equal(t, "spaced out", displayTitle("  spaced   out  "))
	assert.Equal(t, untitledTrack, displayTitle("   "), "a blank title must not look like an empty row")
	assert.Equal(t, untitledTrack, displayTitle("\n\t"))
}

func TestQueueErrTextCoversThePlayerErrors(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{player.ErrNoPreviousTrack, "there is no earlier track in the queue"},
		{player.ErrNoNextTrack, "the queue is empty"},
		{player.ErrInvalidPosition, player.ErrInvalidPosition.Error()},
		{player.ErrClosed, "the player is closed"},
		{player.ErrSoundQueueFull, "the sound queue is full"},
		{errors.New("boom"), "fallback"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, queueErrText(tt.err, "fallback"))
	}
}

// --- /jump -------------------------------------------------------------------

// jumpEvent builds a real /jump interaction carrying an integer option.
func jumpEvent(t *testing.T, guildID, channelID, userID snowflake.ID, position int) *events.ApplicationCommandInteractionCreate {
	t.Helper()
	return jumpEventRecording(t, guildID, channelID, userID, position, nil, nil)
}

// jumpEventRecording captures what the handler answers the interaction with.
func jumpEventRecording(t *testing.T, guildID, channelID, userID snowflake.ID, position int, onReply func(), out *[]discord.MessageCreate) *events.ApplicationCommandInteractionCreate {
	t.Helper()
	_ = onReply

	guildField := ""
	if guildID != 0 {
		guildField = fmt.Sprintf(`"guild_id":"%s",`, guildID)
	}
	payload := fmt.Sprintf(`{
	  "id":"500","application_id":"300","type":2,"token":"tok",
	  %s"channel_id":"%s",
	  "channel":{"id":"%s","type":0},
	  "member":{"user":{"id":"%s","username":"u"}},
	  "data":{"id":"400","name":"jump","type":1,
	    "options":[{"name":"position","type":4,"value":%d}]}
	}`, guildField, channelID, channelID, userID, position)

	var i discord.ApplicationCommandInteraction
	require.NoError(t, i.UnmarshalJSON([]byte(payload)))
	return &events.ApplicationCommandInteractionCreate{
		ApplicationCommandInteraction: i,
		Respond: func(_ discord.InteractionResponseType, data discord.InteractionResponseData, _ ...rest.RequestOpt) error {
			if onReply != nil {
				onReply()
			}
			if out != nil {
				if m, ok := data.(discord.MessageCreate); ok {
					*out = append(*out, m)
				}
			}
			return nil
		},
	}
}

func TestHandleJumpPlaysTheRequestedPosition(t *testing.T) {
	tb := newPanelBot(t, panelTracks(5)...)

	require.NoError(t, tb.handleJump(context.Background(), jumpEvent(t, panelGID, panelChannel, panelUser, 3)))

	assert.Equal(t, 3, tb.Player().QueueSnapshot().CurrentIndex)
	assert.Equal(t, "track 3", tb.Player().Current().Title)
}

func TestHandleJumpOutOfRangeLeavesTheCursorAlone(t *testing.T) {
	tb := newPanelBot(t, panelTracks(3)...)
	before := tb.Player().QueueSnapshot()

	require.NoError(t, tb.handleJump(context.Background(), jumpEvent(t, panelGID, panelChannel, panelUser, 9)))

	assert.Equal(t, before.CurrentIndex, tb.Player().QueueSnapshot().CurrentIndex,
		"a rejected jump must not move anything")
}

// Jumping to the position already playing is a no-op that still reports the track,
// so the user gets a reply rather than silence.
func TestHandleJumpToCurrentPositionStillReports(t *testing.T) {
	tb := newPanelBot(t, panelTracks(3)...)

	require.NoError(t, tb.handleJump(context.Background(), jumpEvent(t, panelGID, panelChannel, panelUser, 1)))

	assert.Equal(t, 1, tb.Player().QueueSnapshot().CurrentIndex)
}

// A finished queue can be restarted by jumping into it, which is what asking for a
// position means.
func TestHandleJumpRestartsAFinishedLineup(t *testing.T) {
	tb := newPanelBot(t, panelTracks(3)...)
	require.NoError(t, tb.Player().JumpTo(3))

	require.NoError(t, tb.handleJump(context.Background(), jumpEvent(t, panelGID, panelChannel, panelUser, 2)))

	assert.Equal(t, 2, tb.Player().QueueSnapshot().CurrentIndex,
		"an idle queue is jumped into, not refused")
}

func TestHandleJumpNeedsTheVoiceChannel(t *testing.T) {
	tb := newPanelBot(t, panelTracks(4)...)
	other := snowflake.ID(99)
	tb.cache.AddVoiceState(discord.VoiceState{
		GuildID: panelGID, UserID: panelUser, ChannelID: &other,
	})

	require.NoError(t, tb.handleJump(context.Background(), jumpEvent(t, panelGID, panelChannel, panelUser, 3)))

	assert.Equal(t, 1, tb.Player().QueueSnapshot().CurrentIndex,
		"someone outside the bot's channel must not steer playback")
}

func TestHandleJumpOutsideAGuildIsRefused(t *testing.T) {
	tb := newPanelBot(t, panelTracks(3)...)

	require.NoError(t, tb.handleJump(context.Background(), jumpEvent(t, 0, panelChannel, panelUser, 2)))

	assert.Equal(t, 1, tb.Player().QueueSnapshot().CurrentIndex)
}

// The panel must show the jump straight away rather than at the next tick.
func TestHandleJumpRefreshesThePanel(t *testing.T) {
	tb := newPanelBot(t, panelTracks(4)...)
	tb.registerPanel(1, panelMessage)
	tb.edited = nil

	require.NoError(t, tb.handleJump(context.Background(), jumpEvent(t, panelGID, panelChannel, panelUser, 3)))

	assert.Equal(t, []snowflake.ID{panelMessage}, tb.editedIDs(),
		"the panel is still the newest message, so it is rewritten in place")
}

func TestHandleJumpWithNoPanelDoesNothingVisible(t *testing.T) {
	tb := newPanelBot(t, panelTracks(4)...)
	tb.sent = nil

	require.NoError(t, tb.handleJump(context.Background(), jumpEvent(t, panelGID, panelChannel, panelUser, 3)))

	assert.Equal(t, 3, tb.Player().QueueSnapshot().CurrentIndex)
	assert.Empty(t, tb.sentIDs(), "jump must not conjure a panel that was never asked for")
}

// Every path answers the interaction exactly once, or discord reports "did not
// respond" after three seconds.
func TestHandleJumpAnswersExactlyOnce(t *testing.T) {
	tests := []struct {
		name     string
		position int
		truncate int
	}{
		{"valid position", 2, 0},
		{"out of range", 99, 0},
		{"outside the voice channel", 2, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tb := newPanelBot(t, panelTracks(4)...)
			if tt.truncate < 0 {
				other := snowflake.ID(99)
				tb.cache.AddVoiceState(discord.VoiceState{
					GuildID: panelGID, UserID: panelUser, ChannelID: &other,
				})
			}

			var replies []discord.MessageCreate
			require.NoError(t, tb.handleJump(context.Background(),
				jumpEventRecording(t, panelGID, panelChannel, panelUser, tt.position, nil, &replies)))

			require.Len(t, replies, 1, "exactly one response")
			assert.True(t, replies[0].Flags.Has(discord.MessageFlagEphemeral),
				"and it is private")
		})
	}
}

// A jump must answer before it touches the panel: refreshGuild makes REST calls, and
// a slow one would eat discord's three second deadline.
func TestHandleJumpRepliesBeforeRefreshing(t *testing.T) {
	tb := newPanelBot(t, panelTracks(4)...)
	tb.registerPanel(1, panelMessage)

	var answered bool
	var replies []discord.MessageCreate
	tb.editPanel = func(context.Context, snowflake.ID, snowflake.ID, discord.MessageUpdate) error {
		assert.True(t, answered, "the panel must not be touched before the command answered")
		return nil
	}

	require.NoError(t, tb.handleJump(context.Background(),
		jumpEventRecording(t, panelGID, panelChannel, panelUser, 3,
			func() { answered = true }, &replies)))

	assert.Len(t, replies, 1)
}
