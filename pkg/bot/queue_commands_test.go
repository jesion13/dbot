package dbot

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"dbot/pkg/player"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubPlayer records what the listener asked the player to do and answers with a
// canned snapshot.
type stubPlayer struct {
	snap player.QueueSnapshot

	skipErr, prevErr, jumpErr error

	skips, prevs, jumps int
	lastJump            int
}

func (s *stubPlayer) SkipNextTracks() (*player.Track, *player.Track, error) {
	s.skips++
	if s.skipErr != nil {
		return nil, nil, s.skipErr
	}
	return s.snap.Current(), nil, nil
}

func (s *stubPlayer) Previous() error {
	s.prevs++
	return s.prevErr
}

func (s *stubPlayer) JumpTo(pos int) error {
	s.jumps++
	s.lastJump = pos
	return s.jumpErr
}

func (s *stubPlayer) QueueSnapshot() player.QueueSnapshot { return s.snap }

// lineup builds a snapshot whose absolute position `playing` (1-based) is the current
// one, or an idle one when playing is 0.
func lineup(playing int, entries ...string) player.QueueSnapshot {
	s := player.QueueSnapshot{CurrentIndex: playing}
	for _, title := range entries {
		s.Tracks = append(s.Tracks, player.Track{Title: title})
	}
	return s
}

// snapshot is the common case: the first entry is playing, the rest are waiting.
func snapshot(current string, upcoming ...string) player.QueueSnapshot {
	return lineup(1, append([]string{current}, upcoming...)...)
}

func TestParseQueueCommand(t *testing.T) {
	tests := []struct {
		in     string
		action string
		pos    int
		err    bool
		ok     bool
	}{
		{in: "!next", action: "next", ok: true},
		{in: "  !NEXT  ", action: "next", ok: true},
		{in: "!prev", action: "prev", ok: true},
		{in: "!queue", action: "queue", ok: true},
		{in: "!list", action: "queue", ok: true},
		{in: "!jump 3", action: "jump", pos: 3, ok: true},
		{in: "!JUMP 12", action: "jump", pos: 12, ok: true},

		{in: "", ok: false},
		{in: "next", ok: false},
		{in: "next!", ok: false},
		{in: "event 13", ok: false},
		{in: "sound-all", ok: false},
		{in: "honk", ok: false},
		{in: "!unrelated", ok: false},

		{in: "!jump", err: true, ok: true},
		{in: "!jump abc", err: true, ok: true},
		{in: "!jump 0", err: true, ok: true},
		{in: "!jump -2", err: true, ok: true},
	}

	for _, tt := range tests {
		c, ok, err := parseQueueCommand(tt.in)
		require.Equal(t, tt.ok, ok, "input %q", tt.in)
		if !ok {
			continue
		}
		if tt.err {
			require.Error(t, err, "input %q", tt.in)
			continue
		}
		require.NoError(t, err, "input %q", tt.in)
		assert.Equal(t, tt.action, c.action, "input %q", tt.in)
		assert.Equal(t, tt.pos, c.pos, "input %q", tt.in)
	}
}

func TestQueueCommandsDispatchToThePlayer(t *testing.T) {
	t.Run("next", func(t *testing.T) {
		p := &stubPlayer{snap: snapshot("b", "c")}
		assert.Contains(t, runQueueCommand(queueCommand{action: "next"}, p), "b")
		assert.Equal(t, 1, p.skips)
	})

	t.Run("prev", func(t *testing.T) {
		p := &stubPlayer{snap: snapshot("a")}
		assert.Contains(t, runQueueCommand(queueCommand{action: "prev"}, p), "a")
		assert.Equal(t, 1, p.prevs)
	})

	t.Run("jump passes the position through", func(t *testing.T) {
		p := &stubPlayer{snap: snapshot("c")}
		runQueueCommand(queueCommand{action: "jump", pos: 4}, p)
		assert.Equal(t, 1, p.jumps)
		assert.Equal(t, 4, p.lastJump)
	})
}

func TestQueueCommandsReportInvalidState(t *testing.T) {
	tests := []struct {
		name string
		p    *stubPlayer
		cmd  queueCommand
		want string
	}{
		{"empty lineup", &stubPlayer{skipErr: player.ErrNoNextTrack}, queueCommand{action: "next"}, "the queue is empty"},
		{"no earlier track", &stubPlayer{prevErr: player.ErrNoPreviousTrack}, queueCommand{action: "prev"}, "there is no earlier track in the queue"},
		{"bad position", &stubPlayer{jumpErr: player.ErrInvalidPosition}, queueCommand{action: "jump", pos: 9}, "position out of range"},
		{"closed", &stubPlayer{skipErr: player.ErrClosed}, queueCommand{action: "next"}, "the player is closed"},
		{"skipped off the end", &stubPlayer{skipErr: errors.New("boom")}, queueCommand{action: "next"}, "nothing to skip to"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, runQueueCommand(tt.cmd, tt.p))
		})
	}
}

func TestListAndQueueAreTheSameOutput(t *testing.T) {
	snap := snapshot("song", "one", "two")
	p := &stubPlayer{snap: snap}

	listCmd, ok, err := parseQueueCommand("!list")
	require.True(t, ok)
	require.NoError(t, err)
	queueCmd, ok, err := parseQueueCommand("!queue")
	require.True(t, ok)
	require.NoError(t, err)

	assert.Equal(t, runQueueCommand(listCmd, p), runQueueCommand(queueCmd, p))
	assert.Equal(t, queueText(snap), runQueueCommand(listCmd, p))
}

func TestQueueTextShowsTheWholeLineup(t *testing.T) {
	// A B C D E with C playing: everything is listed at its absolute position
	got := queueText(lineup(3, "A", "B", "C", "D", "E"))
	lines := strings.Split(got, "\n")
	require.Len(t, lines, 6)
	assert.Contains(t, lines[0], "Music queue")
	assert.Contains(t, lines[1], "1. A")
	assert.Contains(t, lines[2], "2. B")
	assert.Contains(t, lines[3], "3. **C**", "the playing entry is the bolded one")
	assert.Contains(t, lines[4], "4. D")
	assert.Contains(t, lines[5], "5. E")

	// every line is just "N. Title" - no status column, nothing before the number
	for i, line := range lines[1:] {
		assert.Regexp(t, `^[0-9]+\. `, line, "line %d must start with its number", i+1)
		assert.False(t, strings.HasPrefix(line, " "), "line %d must not be indented: %q", i+1, line)
	}

	// the playing entry is called out by its title, so the numbers stay flush left
	assert.Contains(t, lines[3], "3. **C**")
	assert.NotContains(t, lines[1], "**")
}

func TestQueueTextPositionsAreAbsoluteAndStable(t *testing.T) {
	got := queueText(lineup(4, "A", "B", "C", "D", "E"))

	// every entry appears at its absolute index, none of them relative to the cursor
	for i, title := range []string{"A", "B", "C", "D", "E"} {
		assert.Equal(t, i+1, displayIndexOf(got, title), "%s keeps its absolute position", title)
	}
	assert.Equal(t, "D", displayCurrentTitle(got), "the cursor is still position 4, not renumbered to 1")
}

func TestQueueTextIdleStillShowsTheLineup(t *testing.T) {
	// everything played, nothing playing
	got := queueText(lineup(0, "A", "B", "C"))
	assert.Contains(t, got, "idle", "an idle player says so")
	assert.Contains(t, got, "1. A")
	assert.Contains(t, got, "3. C", "the lineup is not presented as empty")
}

// Imported titles routinely carry newlines, tabs and runs of spaces. Rendered raw,
// one entry would spill over several lines: the numbering, the state marker and the
// message splitting all stop lining up.
func TestQueueTextCollapsesWhitespaceInTitles(t *testing.T) {
	snap := lineup(2,
		"Hiszpańskie Dziewczyny\n\nSzanty - Bitwa + lyrics\nCzarnobrody Kapitan",
		"normal\tTitle",
		"  spaced   out  ",
	)
	lines := strings.Split(queueText(snap), "\n")

	require.Len(t, lines, 4, "one line per entry, however many lines the raw title had")
	for i, entry := range snap.Tracks {
		assert.Contains(t, lines[i+1], displayTitle(entry.Title), "entry %d keeps its number", i+1)
	}
	assert.Contains(t, lines[1], "Hiszpańskie Dziewczyny Szanty - Bitwa + lyrics Czarnobrody Kapitan",
		"newlines inside a title become spaces")
	assert.Contains(t, lines[2], "normal Title", "a tab becomes a space")
	assert.Contains(t, lines[3], "3. spaced out", "runs of spaces collapse")
}

func TestQueueTextNamesUntitledEntries(t *testing.T) {
	got := queueText(lineup(1, "   ", "\n\t", "real"))
	lines := strings.Split(got, "\n")
	require.Len(t, lines, 4)
	assert.Contains(t, lines[1], untitledTrack, "a blank title must not look like an empty row")
	assert.Contains(t, lines[2], untitledTrack)
	assert.Contains(t, lines[3], "real")
}

// A title that is long only because of its whitespace still has to fit the message
// limit, and the split must keep every entry.
func TestQueueTextWithMultilineTitlesSurvivesMessageSplitting(t *testing.T) {
	entries := make([]string, 0, 120)
	for i := range 120 {
		entries = append(entries, strings.Repeat("tytuł-\n", 6)+strconv.Itoa(i))
	}
	display := queueText(lineup(1, entries...))

	chunks := splitMessages(display, maxQueueMsgLen)
	require.Greater(t, len(chunks), 1)
	assert.Equal(t, display, strings.Join(chunks, ""), "nothing is lost across the split")

	// every entry still shows its absolute position, on its own line, in order
	lines := strings.Split(display, "\n")
	require.Len(t, lines, 121)
	for i := range 120 {
		assert.Contains(t, lines[i+1], strconv.Itoa(i+1)+". ", "entry %d keeps its number", i+1)
		assert.Equal(t, i+1, displayIndexOf(display, displayTitle(entries[i])), "entry %d", i+1)
	}
}

// The same position renders identically no matter how playback got there: there is
// no per-entry state left that could drift or contradict the cursor.
// The display contract in one place: absolute numbers, the current title bold and
// nothing else, and no per-entry status words - there is no per-entry state to show.
func TestQueueDisplayContract(t *testing.T) {
	got := queueText(lineup(3, "A", "B", "C", "D"))

	assert.Equal(t, []int{1, 2, 3, 4}, displayPositions(got),
		"every entry has a stable absolute number")
	assert.Equal(t, "C", displayCurrentTitle(got), "only the current track is highlighted")
	assert.Equal(t, 1, strings.Count(got, "**")/2, "exactly one entry is bold")

	for _, word := range []string{"played", "skipped", "upcoming", "now playing"} {
		assert.NotContains(t, strings.ToLower(got), word,
			"no per-entry status is displayed")
	}
	for _, mark := range []string{"\u2713", "\u25b6", "\u21b7", "\u00b7", "\u2192"} {
		assert.NotContains(t, got, mark, "no status marker column")
	}
}

func TestQueueTextPausedAndEmpty(t *testing.T) {
	snap := snapshot("song")
	snap.Paused = true
	assert.Contains(t, queueText(snap), "paused")

	assert.Contains(t, queueText(player.QueueSnapshot{}), "nothing has been queued yet")

	closed := lineup(1, "A")
	closed.Closed = true
	assert.Contains(t, queueText(closed), "closed")
}

func TestQueueTextHidesInternals(t *testing.T) {
	snap := player.QueueSnapshot{
		Tracks: []player.Track{
			{Title: "song", URL: "https://x", Filepath: "/tmp/secret.opus"},
			{Title: "next", Filepath: "/tmp/secret2.opus"},
		},
		CurrentIndex: 1,
		SoundWait:    3,
	}
	got := queueText(snap)
	assert.NotContains(t, got, "/tmp/", "no local paths")
	assert.NotContains(t, got, "https://x", "no source urls")
	assert.NotContains(t, got, "boom", "sound requests never appear in the music lineup")
}

func TestSplitMessages(t *testing.T) {
	t.Run("short text is one message", func(t *testing.T) {
		assert.Equal(t, []string{"a\nb"}, splitMessages("a\nb", 100))
	})

	t.Run("long queue keeps every line in order", func(t *testing.T) {
		var lines []string
		for i := range 200 {
			lines = append(lines, strings.Repeat("x", 40)+" #"+string(rune('a'+i%26)))
		}
		original := strings.Join(lines, "\n")
		chunks := splitMessages(original, 200)
		require.Greater(t, len(chunks), 1)

		for _, c := range chunks {
			assert.LessOrEqual(t, len(c), 200)
		}
		assert.Equal(t, original, strings.Join(chunks, ""), "chunks concatenate back to the original")
	})

	t.Run("a single huge line is cut, not dropped", func(t *testing.T) {
		original := strings.Repeat("y", 500)
		chunks := splitMessages(original, 200)
		require.Len(t, chunks, 3)
		for _, c := range chunks {
			assert.LessOrEqual(t, len(c), 200)
		}
		assert.Equal(t, original, strings.Join(chunks, ""))
	})

	t.Run("empty text", func(t *testing.T) {
		assert.Empty(t, splitMessages("", 100))
	})
}

func TestQueueCommandListenerIgnoresUnrelatedMessages(t *testing.T) {
	// no client, no player: an unhandled message must never reach either
	d := &DBot{}
	for _, content := range []string{"event 13", "event-all", "honk", "!nonsense", "plain chat"} {
		_, ok := d.queueCommandReply(msg(content, false))
		assert.False(t, ok, "content %q", content)
	}
}

func TestQueueCommandListenerIgnoresBotMessages(t *testing.T) {
	d := &DBot{}
	_, ok := d.queueCommandReply(msg("!next", true))
	assert.False(t, ok)
}

// stubConn is a voice connection the queue listener never writes to, but which
// still answers the calls a closing player makes.
type stubConn struct {
	voice.Conn
	gid snowflake.ID
}

func (s stubConn) GuildID() snowflake.ID { return s.gid }

func (s stubConn) SetSpeaking(context.Context, voice.SpeakingFlags) error { return nil }

// advancingPlayer is a stub whose cursor actually moves, so a reply can be checked
// against the track it was supposed to name rather than a frozen snapshot.
type advancingPlayer struct {
	tracks []player.Track
	cursor int // -1 when idle, like the real player
}

func (a *advancingPlayer) SkipNextTracks() (*player.Track, *player.Track, error) {
	if len(a.tracks) == 0 {
		return nil, nil, player.ErrNoNextTrack
	}
	var from *player.Track
	if a.cursor >= 0 {
		f := a.tracks[a.cursor]
		from = &f
	}
	a.cursor = (a.cursor + 1) % len(a.tracks)
	t := a.tracks[a.cursor]
	return from, &t, nil
}

func (a *advancingPlayer) Previous() error {
	if a.cursor == 0 {
		return player.ErrNoPreviousTrack
	}
	a.cursor--
	return nil
}

func (a *advancingPlayer) JumpTo(pos int) error {
	if pos < 1 || pos > len(a.tracks) {
		return player.ErrInvalidPosition
	}
	a.cursor = pos - 1
	return nil
}

func (a *advancingPlayer) QueueSnapshot() player.QueueSnapshot {
	return player.QueueSnapshot{Tracks: a.tracks, CurrentIndex: a.cursor + 1}
}

// starts playing at position 1, like a player that has just been handed a lineup
func lineup3() *advancingPlayer {
	return &advancingPlayer{
		tracks: []player.Track{{Title: "first"}, {Title: "second"}, {Title: "third"}},
	}
}

// nothing playing, but tracks are queued - the state after a lineup played out
func idleLineup3() *advancingPlayer {
	p := lineup3()
	p.cursor = -1
	return p
}

// An idle player is restarting, not skipping, and must say so.
func TestSkipFromIdleReportsAStart(t *testing.T) {
	p := idleLineup3()

	got := runQueueCommand(queueCommand{action: "next"}, p)

	assert.Equal(t, "started **first**", got, "nothing was playing, so nothing was skipped")
}

// An empty lineup has nothing to wrap to; the error path must survive the new method.
func TestSkipOnEmptyLineupReportsTheError(t *testing.T) {
	p := &advancingPlayer{}

	got := runQueueCommand(queueCommand{action: "next"}, p)

	assert.Equal(t, "the queue is empty", got)
}

// The double must hand back what the real player does, or these tests prove nothing.
func TestSkipUsesBothEndsOfTheMove(t *testing.T) {
	p := lineup3()
	p.cursor = 1

	from, to, err := p.SkipNextTracks()

	require.NoError(t, err)
	require.NotNil(t, from)
	require.NotNil(t, to)
	assert.Equal(t, "second", from.Title, "the track we left")
	assert.Equal(t, "third", to.Title, "the track we moved to")
	assert.Equal(t, 2, p.cursor)
}

// The reply has to name the track that was skipped, not the one that started playing.
func TestSkipRepliesWithTheTrackItLeft(t *testing.T) {
	p := lineup3()

	got := runQueueCommand(queueCommand{action: "next"}, p)

	assert.Equal(t, "skipped **first**", got)
	assert.Equal(t, 1, p.cursor, "playback really did move on")
	assert.NotContains(t, got, "second", "the track we skipped to must not be the one named")
}

// Same for prev and jump, which must name where we ended up - not where we came from.
func TestNavigationRepliesNameWherePlaybackWent(t *testing.T) {
	t.Run("prev", func(t *testing.T) {
		p := lineup3()
		p.cursor = 2
		assert.Equal(t, "back to **second**", runQueueCommand(queueCommand{action: "prev"}, p))
	})

	t.Run("jump", func(t *testing.T) {
		p := lineup3()
		assert.Equal(t, "jumped **third**", runQueueCommand(queueCommand{action: "jump", pos: 3}, p))
	})
}

// Wrapping off the end is still a skip: the last track is the one left behind.
func TestSkipWrappingNamesTheLastTrack(t *testing.T) {
	p := lineup3()
	p.cursor = 2

	assert.Equal(t, "skipped **third**", runQueueCommand(queueCommand{action: "next"}, p))
	assert.Equal(t, 0, p.cursor)
}

func TestQueueCommandReplyGuards(t *testing.T) {
	botGID := snowflake.ID(1)
	botVCID := snowflake.ID(10)
	otherVC := snowflake.ID(11)
	userID := snowflake.ID(2)

	// newBot puts the user in userChan's channel, or nowhere when it is nil.
	newBot := func(userChan *snowflake.ID) *DBot {
		c := cache.New(cache.WithCaches(cache.FlagsAll))
		if userChan != nil {
			c.AddVoiceState(discord.VoiceState{GuildID: botGID, UserID: userID, ChannelID: userChan})
		}
		p := player.NewPlayer()
		require.NoError(t, p.SetVC(stubConn{gid: botGID}, botVCID))
		t.Cleanup(func() { _ = p.Close() })
		d := &DBot{Client: &bot.Client{Caches: c}}
		d.SetPlayer(p)
		return d
	}

	t.Run("outside a guild", func(t *testing.T) {
		d := &DBot{}
		text, ok := d.queueCommandReply(msg("!next", false))
		assert.True(t, ok)
		assert.Contains(t, text, "server")
	})

	t.Run("bot not connected", func(t *testing.T) {
		p := player.NewPlayer()
		t.Cleanup(func() { _ = p.Close() })
		d := &DBot{Client: &bot.Client{Caches: cache.New(cache.WithCaches(cache.FlagsAll))}}
		d.SetPlayer(p)
		text, ok := d.queueCommandReply(guildMsg("!next", botGID))
		assert.True(t, ok)
		assert.Contains(t, text, "not connected")
	})

	t.Run("user not in a voice channel", func(t *testing.T) {
		text, ok := newBot(nil).queueCommandReply(guildMsg("!queue", botGID))
		assert.True(t, ok)
		assert.Contains(t, text, "join a voice channel")
	})

	t.Run("user in another voice channel", func(t *testing.T) {
		text, ok := newBot(&otherVC).queueCommandReply(guildMsg("!queue", botGID))
		assert.True(t, ok)
		assert.Contains(t, text, "my voice channel")
	})

	t.Run("same channel reaches the player", func(t *testing.T) {
		// an empty player has nothing to skip, which proves it was reached
		text, ok := newBot(&botVCID).queueCommandReply(guildMsg("!next", botGID))
		assert.True(t, ok)
		assert.Equal(t, "the queue is empty", text, "an empty lineup has nothing to wrap to")
	})

	t.Run("invalid jump is reported, not executed", func(t *testing.T) {
		text, ok := newBot(&botVCID).queueCommandReply(guildMsg("!jump abc", botGID))
		assert.True(t, ok)
		assert.Contains(t, text, "not a number")
	})
}

func TestQueueCommandsReachARealPlayer(t *testing.T) {
	botGID := snowflake.ID(1)
	botVCID := snowflake.ID(10)
	userID := snowflake.ID(2)

	c := cache.New(cache.WithCaches(cache.FlagsAll))
	c.AddVoiceState(discord.VoiceState{GuildID: botGID, UserID: userID, ChannelID: &botVCID})

	p := player.NewPlayer()
	require.NoError(t, p.SetVC(stubConn{gid: botGID}, botVCID))
	t.Cleanup(func() { _ = p.Close() })
	d := &DBot{Client: &bot.Client{Caches: c}, Ctx: context.Background()}
	d.SetPlayer(p)

	require.NoError(t, p.EnqueueMany([]player.Track{{Title: "a"}, {Title: "b"}}))

	text, ok := d.queueCommandReply(guildMsg("!list", botGID))
	require.True(t, ok)
	assert.Contains(t, text, "2. b")

	text, ok = d.queueCommandReply(guildMsg("!jump 2", botGID))
	require.True(t, ok)
	assert.Contains(t, text, "b")
}

// The queue is held in a controlled state rather than left to real playback: a
// track with no voice connection fails instantly and advances, which would make the
// displayed positions depend on timing.
func TestQueueTextNumbersAreJumpPositions(t *testing.T) {
	build := func() *player.Player {
		p := player.NewPlayer()
		t.Cleanup(func() { _ = p.Close() })
		require.NoError(t, p.EnqueueMany([]player.Track{
			{Title: "a"}, {Title: "b"}, {Title: "c"}, {Title: "d"},
		}))
		return p
	}

	// every number the display prints is a position the player accepts, and it is
	// the absolute index in the lineup - not an offset from the cursor
	positions := displayPositions(queueText(build().QueueSnapshot()))
	require.Equal(t, []int{1, 2, 3, 4}, positions, "every entry is listed, from position 1")
	for _, pos := range positions {
		require.NoError(t, build().JumpTo(pos), "displayed position %d", pos)
	}
}

// A lineup that has already advanced still reports positions the player accepts, and
// the oldest entry is still reachable.
func TestQueueTextNumbersStayValidAfterPlayback(t *testing.T) {
	p := player.NewPlayer()
	t.Cleanup(func() { _ = p.Close() })
	require.NoError(t, p.EnqueueMany([]player.Track{{Title: "a"}, {Title: "b"}, {Title: "c"}}))

	// no voice connection, so every track fails and the player advances to the end
	require.Eventually(t, func() bool { return p.QueueSnapshot().CurrentIndex == 0 },
		2*time.Second, 5*time.Millisecond)

	s := p.QueueSnapshot()
	assert.Len(t, s.Tracks, 3, "the lineup is intact")
	display := queueText(s)
	for i, e := range s.Tracks {
		assert.Equal(t, i+1, displayIndexOf(display, e.Title),
			"entry %d keeps its absolute position", i+1)
	}
	// and position 1 still resolves to the first track
	require.NoError(t, p.JumpTo(1))
	assert.Equal(t, "a", p.Current().Title)
}

// entryLine splits a rendered lineup line into its absolute position and title.
// Every line looks like "<position>. <title>", possibly with the title in bold.
func entryLine(line string) (pos int, title string, ok bool) {
	n, title, found := strings.Cut(line, ". ")
	if !found {
		return 0, "", false
	}
	pos, err := strconv.Atoi(n)
	if err != nil {
		return 0, "", false
	}
	return pos, strings.Trim(title, "*"), true
}

// displayPositions is the list of positions a rendered queue shows.
func displayPositions(display string) []int {
	var out []int
	for _, line := range strings.Split(display, "\n") {
		if pos, _, ok := entryLine(line); ok {
			out = append(out, pos)
		}
	}
	return out
}

// displayCurrentTitle is the title of the bolded (playing) entry.
func displayCurrentTitle(display string) string {
	for _, line := range strings.Split(display, "\n") {
		if strings.Contains(line, "**") {
			_, title, _ := entryLine(line)
			return title
		}
	}
	return ""
}

// displayIndexOf is the position a rendered queue gives to a title, 0 when absent.
func displayIndexOf(display, title string) int {
	for _, line := range strings.Split(display, "\n") {
		if pos, t, ok := entryLine(line); ok && t == title {
			return pos
		}
	}
	return 0
}

func TestQueueWithoutVoiceConnectionStaysConsistent(t *testing.T) {
	p := player.NewPlayer()
	t.Cleanup(func() { _ = p.Close() })
	require.NoError(t, p.EnqueueMany([]player.Track{{Title: "a"}, {Title: "b"}}))

	require.Eventually(t, func() bool { return p.Current() == nil }, 2*time.Second, 5*time.Millisecond)
	s := p.QueueSnapshot()
	assert.Len(t, s.Tracks, 2, "the lineup survives an idle player")
	assert.Zero(t, s.CurrentIndex)
}

// msg is the minimum a queue command needs: content, author and bot flag.
func msg(content string, fromBot bool) discord.Message {
	return discord.Message{
		Content: content,
		Author:  discord.User{Bot: fromBot, ID: 2},
	}
}

func guildMsg(content string, gid snowflake.ID) discord.Message {
	m := msg(content, false)
	m.GuildID = &gid
	return m
}
