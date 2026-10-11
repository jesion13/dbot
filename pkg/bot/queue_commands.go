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

// A plain text testing interface, deliberately thin: everything goes through the same
// player API the slash commands use, and the queue only ever lives in the player.
const queueCmdPrefix = "!"

// under discord's 2000 char limit, so a title containing markdown still fits
const maxQueueMsgLen = 1900

type queuePlayer interface {
	SkipNextTracks() (from, to *player.Track, err error)
	PreviousTrack() (*player.Track, error)
	JumpToTrack(int) (*player.Track, error)
	QueueSnapshot() player.QueueSnapshot
}

type queueCommand struct {
	action string // next, prev, jump, queue
	pos    int
}

// ok is false for anything else, so unrelated messages (including sound aliases) are
// left alone.
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

// Never touches queue state itself - it only asks the player to.
func runQueueCommand(c queueCommand, p queuePlayer) string {
	switch c.action {
	case "next":
		// the player returns both ends of the move under one lock; reading the lineup
		// around SkipNext instead would race playback advancing on its own
		from, to, err := p.SkipNextTracks()
		if err != nil {
			return queueErrText(err, "nothing to skip to")
		}
		if from == nil {
			// nothing was playing, so there is nothing that got skipped
			return trackText("started", to)
		}
		return trackText("skipped", from)
	case "prev":
		// the player returns the track it selected under the same lock as the move, so
		// playback advancing on its own cannot make us name the wrong one
		to, err := p.PreviousTrack()
		if err != nil {
			return queueErrText(err, "nothing to go back to")
		}
		return trackText("back to", to)
	case "jump":
		to, err := p.JumpToTrack(c.pos)
		if err != nil {
			return queueErrText(err, "invalid position")
		}
		return trackText("jumped", to)
	default:
		return queueText(p.QueueSnapshot())
	}
}

func queueErrText(err error, fallback string) string {
	switch {
	case errors.Is(err, player.ErrNoPreviousTrack):
		return "there is no earlier track in the queue"
	case errors.Is(err, player.ErrNoNextTrack):
		return "the queue is empty"
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

func trackText(verb string, t *player.Track) string {
	if t == nil {
		return "the queue is finished"
	}
	return fmt.Sprintf("%s **%s**", verb, displayTitle(t.Title))
}

// Imported metadata carries newlines, tabs and space runs; rendered raw, one entry
// spills over several lines and the numbering and message splitting stop lining up.
func displayTitle(title string) string {
	one := strings.Join(strings.Fields(title), " ")
	if one == "" {
		return untitledTrack
	}
	return one
}

// stands in for a title that is empty or whitespace only
const untitledTrack = "(untitled)"

// Every entry is shown, including tracks that already played: a position has to keep
// meaning the same track, otherwise a number the user read minutes ago would point
// somewhere else. The playing track is marked by its title rather than by a column in
// front of the number, which read as stray formatting and broke the flush-left
// alignment. Nothing here exposes a filepath, a URL, or sound requests.
func queueText(s player.QueueSnapshot) string {
	if s.Closed {
		return "the player is closed"
	}
	if len(s.Tracks) == 0 {
		return "nothing has been queued yet"
	}

	var b strings.Builder
	b.WriteString("Music queue")
	if s.CurrentIndex == 0 {
		b.WriteString(" (idle)")
	}
	if s.Paused {
		b.WriteString(" (paused)")
	}
	for i, t := range s.Tracks {
		title := displayTitle(t.Title)
		if i+1 == s.CurrentIndex {
			title = "**" + title + "**"
		}
		fmt.Fprintf(&b, "\n%d. %s", i+1, title)
	}
	return b.String()
}

// so the caller must advance by len(head), not by limit
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

// Prefers line boundaries. Chunks concatenate back to the original exactly: no
// character dropped, duplicated or split mid-rune.
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

			appendChunk("\n")
		}

		for len(line) > limit {
			head, rest := cutAt(line, limit)
			appendChunk(head)
			line = rest
		}
		appendChunk(line)
	}
	return out
}

// Reports whether the message was one of them, so the caller can leave sound
// matching alone.
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

// ok is false when the message is not one of the five commands.
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

	if reason := d.voiceGuard(*m.GuildID, m.Author.ID); reason != "" {
		return reason, true
	}
	return runQueueCommand(cmd, d.Player()), true
}

// the player must satisfy the interface the listener drives.
var _ queuePlayer = (*player.Player)(nil)
