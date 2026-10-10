package dbot

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"unicode/utf8"

	"dbot/pkg/player"

	"github.com/disgoorg/disgo/discord"
)

// queue commands are a plain text testing interface for the player queue. They are
// deliberately thin: everything they do goes through the same player API the slash
// commands use, and the queue itself only ever lives in the player.
const queueCmdPrefix = "!"

// maxQueueMsgLen is under discord's 2000 char limit so a title containing markdown
// or an embed still fits.
const maxQueueMsgLen = 1900

// queuePlayer is the part of the player these commands drive.
type queuePlayer interface {
	SkipNext() error
	Previous() error
	JumpTo(int) error
	QueueSnapshot() player.QueueSnapshot
}

type queueCommand struct {
	action string // next, prev, jump, queue
	pos    int
}

// parseQueueCommand recognizes the five text commands. ok is false for anything
// else, so unrelated messages (including sound aliases) are left alone.
func parseQueueCommand(content string) (c queueCommand, ok bool, err error) {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(content)))
	if len(fields) == 0 || !strings.HasPrefix(fields[0], queueCmdPrefix) {
		return c, false, nil
	}

	switch strings.TrimPrefix(fields[0], queueCmdPrefix) {
	case "next":
		return queueCommand{action: "next"}, true, nil
	case "prev":
		return queueCommand{action: "prev"}, true, nil
	case "queue", "list": // aliases, same display
		return queueCommand{action: "queue"}, true, nil
	case "jump":
		if len(fields) < 2 {
			return c, true, errors.New("usage: !jump <position>")
		}
		pos, convErr := strconv.Atoi(fields[1])
		if convErr != nil {
			return c, true, fmt.Errorf("'%s' is not a number", fields[1])
		}
		if pos < 1 {
			return c, true, errors.New("position must be 1 or more")
		}
		return queueCommand{action: "jump", pos: pos}, true, nil
	}
	return c, false, nil
}

// runQueueCommand executes a parsed command and returns the reply text. It never
// touches queue state itself - it only asks the player to.
func runQueueCommand(c queueCommand, p queuePlayer) string {
	switch c.action {
	case "next":
		if err := p.SkipNext(); err != nil {
			return queueErrText(err, "nothing to skip to")
		}
		return queueNowText(p.QueueSnapshot(), "skipped")
	case "prev":
		if err := p.Previous(); err != nil {
			return queueErrText(err, "nothing to go back to")
		}
		return queueNowText(p.QueueSnapshot(), "back to")
	case "jump":
		if err := p.JumpTo(c.pos); err != nil {
			return queueErrText(err, "invalid position")
		}
		return queueNowText(p.QueueSnapshot(), "jumped")
	default:
		return queueText(p.QueueSnapshot())
	}
}

func queueErrText(err error, fallback string) string {
	switch {
	case errors.Is(err, player.ErrNothingPlaying):
		return "nothing is playing"
	case errors.Is(err, player.ErrNoHistory):
		return "no previous track in history"
	case errors.Is(err, player.ErrInvalidPosition):
		return err.Error()
	case errors.Is(err, player.ErrClosed):
		return "the player is closed"
	case errors.Is(err, player.ErrSoundQueueFull):
		return "the sound queue is full"
	default:
		return fallback
	}
}

func queueNowText(s player.QueueSnapshot, verb string) string {
	if s.Current == nil {
		return "the queue is now empty"
	}
	return fmt.Sprintf("%s **%s**", verb, s.Current.Title)
}

// queueText renders the queue. Positions match JumpTo: the playing track is 1 and
// the first upcoming track is 2; when idle the first queued track is 1. No local
// paths and no sound requests are ever exposed.
func queueText(s player.QueueSnapshot) string {
	if s.Closed {
		return "the player is closed"
	}
	if s.Current == nil && len(s.Upcoming) == 0 {
		return "nothing is playing and the queue is empty"
	}

	var b strings.Builder
	if s.Current != nil {
		fmt.Fprintf(&b, "**Now:** %s\n", s.Current.Title)
	} else {
		b.WriteString("**Now:** idle\n")
	}
	if s.Paused {
		b.WriteString("*(paused)*\n")
	}

	pos := 1
	if s.Current != nil {
		pos = 2
	}
	if len(s.Upcoming) == 0 {
		b.WriteString("\nNothing queued after this.")
		return b.String()
	}
	b.WriteString("\n**Up next:**")
	for _, t := range s.Upcoming {
		fmt.Fprintf(&b, "\n%d. %s", pos, t.Title)
		pos++
	}
	return b.String()
}

// cutAt splits s into the longest prefix of at most limit bytes that ends on a
// rune boundary, and the remainder. Backing off to a rune boundary consumes fewer
// than limit bytes, so the caller must advance by len(head), not by limit.
func cutAt(s string, limit int) (head, rest string) {
	if len(s) <= limit {
		return s, ""
	}
	head = s[:limit]
	for len(head) > 0 && !utf8.ValidString(head) {
		head = head[:len(head)-1]
	}
	return head, s[len(head):]
}

// splitMessages breaks text into chunks that fit discord's limit, preferring line
// boundaries. Chunks concatenate back to the original text exactly: no character
// is dropped, duplicated or split mid-rune.
func splitMessages(text string, limit int) []string {
	if text == "" {
		return nil
	}
	if limit <= 0 {
		limit = maxQueueMsgLen
	}

	var out []string
	appendChunk := func(s string) {
		if len(out) > 0 && len(out[len(out)-1])+len(s) <= limit {
			out[len(out)-1] += s
			return
		}
		out = append(out, s)
	}

	for i, line := range strings.Split(text, "\n") {
		if i > 0 {
			// the separator belongs to the previous line
			appendChunk("\n")
		}
		// a line longer than the limit is split on rune boundaries
		for len(line) > limit {
			head, rest := cutAt(line, limit)
			appendChunk(head)
			line = rest
		}
		appendChunk(line)
	}
	return out
}

// queueCommandListener handles the five text queue commands. It reports whether the
// message was one of them, so the caller can leave sound matching alone.
func (d *DBot) queueCommandListener(m discord.Message) bool {
	text, ok := d.queueCommandReply(m)
	if !ok {
		return false
	}
	for _, chunk := range splitMessages(text, maxQueueMsgLen) {
		_, err := d.Client.Rest.CreateMessage(m.ChannelID, discord.MessageCreate{Content: chunk})
		if err != nil {
			slog.Error("queue command reply failed", slog.Any("error", err))
			return true
		}
	}
	return true
}

// queueCommandReply resolves one message into the text to answer with. ok is false
// when the message is not one of the five commands, so nothing is sent for it.
func (d *DBot) queueCommandReply(m discord.Message) (string, bool) {
	if m.Author.Bot {
		return "", false
	}
	cmd, ok, err := parseQueueCommand(m.Content)
	if !ok {
		return "", false
	}
	if err != nil {
		return err.Error(), true
	}
	if m.GuildID == nil {
		return "queue commands only work in a server", true
	}

	conn, botChanID := d.Player().Conn()
	if conn == nil {
		return "i am not connected to a voice channel", true
	}
	vs, vsErr := d.getUserVC(*m.GuildID, m.Author.ID)
	if vsErr != nil || vs.ChannelID == nil {
		return "join a voice channel first", true
	}
	if *vs.ChannelID != botChanID {
		return "join my voice channel first", true
	}
	return runQueueCommand(cmd, d.Player()), true
}

// the player must satisfy the interface the listener drives.
var _ queuePlayer = (*player.Player)(nil)
