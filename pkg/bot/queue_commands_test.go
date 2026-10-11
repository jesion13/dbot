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

func (s *stubPlayer) SkipNext() error {
	s.skips++
	return s.skipErr
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

func snapshot(current string, upcoming ...string) player.QueueSnapshot {
	s := player.QueueSnapshot{}
	if current != "" {
		s.Current = &player.Track{Title: current}
	}
	for _, u := range upcoming {
		s.Upcoming = append(s.Upcoming, player.Track{Title: u})
	}
	return s
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
		{"nothing playing", &stubPlayer{skipErr: player.ErrNothingPlaying}, queueCommand{action: "next"}, "nothing is playing"},
		{"no history", &stubPlayer{prevErr: player.ErrNoHistory}, queueCommand{action: "prev"}, "no previous track in history"},
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

func TestQueueTextPositions(t *testing.T) {
	// playing: the current track is 1, so the upcoming ones start at 2
	got := queueText(snapshot("song", "one", "two"))
	assert.Contains(t, got, "**Now:** song")
	assert.Contains(t, got, "\n2. one")
	assert.Contains(t, got, "\n3. two")
	assert.NotContains(t, got, "1. song", "the playing track is not repeated in the list")

	t.Run("idle: the first queued track is 1", func(t *testing.T) {
		got := queueText(snapshot("", "one", "two"))
		assert.Contains(t, got, "idle")
		assert.Contains(t, got, "\n1. one")
		assert.Contains(t, got, "\n2. two")
	})

	t.Run("paused is visible", func(t *testing.T) {
		snap := snapshot("song")
		snap.Paused = true
		assert.Contains(t, queueText(snap), "(paused)")
	})

	t.Run("empty", func(t *testing.T) {
		assert.Contains(t, queueText(player.QueueSnapshot{}), "empty")
	})

	t.Run("nothing after the current track", func(t *testing.T) {
		got := queueText(snapshot("song"))
		assert.Contains(t, got, "Nothing queued after this.")
	})
}

func TestQueueTextNumbersMatchJumpPositions(t *testing.T) {
	// the numbers the display prints are exactly the positions JumpTo accepts:
	// playing track is 1, upcoming start at 2
	got := queueText(snapshot("song", "one", "two"))

	var positions []string
	for _, line := range strings.Split(got, "\n") {
		if n, _, ok := strings.Cut(line, ". "); ok && n != "" {
			positions = append(positions, n)
		}
	}
	require.Len(t, positions, 2)
	for i, n := range positions {
		assert.Equal(t, strconv.Itoa(i+2), n, "upcoming entry %d", i)
	}
}

func TestQueueTextHidesInternals(t *testing.T) {
	snap := player.QueueSnapshot{
		Current:   &player.Track{Title: "song", URL: "https://x", Filepath: "/tmp/secret.opus"},
		Upcoming:  []player.Track{{Title: "next", Filepath: "/tmp/secret2.opus"}},
		History:   []player.Track{{Title: "old"}},
		SoundWait: 3,
	}
	got := queueText(snap)
	assert.NotContains(t, got, "/tmp/")
	assert.NotContains(t, got, "https://x")
	assert.NotContains(t, got, "old", "history is not displayed")
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
		assert.Equal(t, "nothing is playing", text)
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
// track with no voice connection fails instantly and advances into history, which
// would make the displayed positions depend on timing.
func TestQueueTextNumbersAreJumpPositions(t *testing.T) {
	build := func() *player.Player {
		p := player.NewPlayer()
		t.Cleanup(func() { _ = p.Close() })
		require.NoError(t, p.EnqueueMany([]player.Track{{Title: "a"}, {Title: "b"}, {Title: "c"}, {Title: "d"}}))
		return p
	}

	// read the display and jump every position it prints, each on a fresh queue
	// because a jump drops the tracks it skipped
	displayed := build().QueueSnapshot()
	var positions []int
	for _, line := range strings.Split(queueText(displayed), "\n") {
		n, _, ok := strings.Cut(line, ". ")
		if !ok || n == "" {
			continue
		}
		pos, err := strconv.Atoi(n)
		require.NoError(t, err)
		positions = append(positions, pos)
	}

	require.Equal(t, []int{2, 3, 4}, positions, "upcoming entries are numbered from 2 while playing")
	for _, pos := range positions {
		require.NoError(t, build().JumpTo(pos), "displayed position %d", pos)
	}
}

// The player advances on its own when it has no connection, so this covers the
// contract the display relies on: it stays correct without anyone driving it.
func TestQueueWithoutVoiceConnectionStaysConsistent(t *testing.T) {
	p := player.NewPlayer()
	t.Cleanup(func() { _ = p.Close() })
	require.NoError(t, p.EnqueueMany([]player.Track{{Title: "a"}, {Title: "b"}}))

	require.Eventually(t, func() bool { return p.Current() == nil }, 2*time.Second, 5*time.Millisecond)
	s := p.QueueSnapshot()
	assert.Empty(t, s.Upcoming)
	assert.Len(t, s.History, 2)
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
