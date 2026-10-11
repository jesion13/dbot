package dbot

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"dbot/pkg/player"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
)

// The queue view pages by absolute position, so the numbers here are the numbers
// /jump takes.
const queuePageSize = 20

// The page number lives in the custom id rather than a registry, so a click is answered
// from the live queue with nothing to keep in sync and nothing to expire.
const queueIDPrefix = "queue:"

func queuePageID(page int) string {
	return fmt.Sprintf("%s%d", queueIDPrefix, page)
}

func parseQueueCustomID(customID string) (int, bool) {
	rest, found := strings.CutPrefix(customID, queueIDPrefix)
	if !found {
		return 0, false
	}
	page, err := strconv.Atoi(rest)
	if err != nil || page < 1 {
		return 0, false
	}
	return page, true
}

// handleQueue answers privately, so browsing the queue does not push the player panel
// out of the newest-message slot and cost it a republish.
func (d *DBot) handleQueue(ctx context.Context, e *events.ApplicationCommandInteractionCreate) error {
	s := d.Player().QueueSnapshot()
	msg := buildQueuePageV2(queueLandingPage(s), s)
	// the V2 flag is set by the renderer; ephemeral has to be added on top or the
	// reply lands in the channel
	msg.Flags = msg.Flags.Add(discord.MessageFlagEphemeral)
	return e.CreateMessage(msg)
}

// queueLandingPage opens on what is playing, which is what someone asking for the
// queue wants to see first.
func queueLandingPage(s player.QueueSnapshot) int {
	if s.CurrentIndex < 1 {
		return 1
	}
	return (s.CurrentIndex-1)/queuePageSize + 1
}

func (d *DBot) onQueueComponentEvent(e *events.ComponentInteractionCreate) {
	d.onQueueComponent(e)
}

func (d *DBot) onQueueComponent(e *events.ComponentInteractionCreate) {
	page, ok := parseQueueCustomID(e.Data.CustomID())
	if !ok {
		return
	}

	// Response type 7 edits the message the button sits on, which is the ephemeral
	// reply /queue posted. One response, so discord sees the interaction answered.
	err := e.UpdateMessage(discord.NewMessageUpdateV2(buildQueuePageV2(page, d.Player().QueueSnapshot()).Components...))
	if err != nil {
		slog.Error("queue page update failed", slog.Int("page", page), slog.Any("error", err))
	}
}

// buildQueuePageV2 renders one page of the whole queue. The page is clamped, so a click
// on a stale button after the queue shrank lands on the last page rather than failing.
func buildQueuePageV2(page int, s player.QueueSnapshot) discord.MessageCreate {
	pages := max(1, (len(s.Tracks)+queuePageSize-1)/queuePageSize)
	page = min(max(page, 1), pages)

	var parts []discord.ContainerSubComponent
	if len(s.Tracks) == 0 {
		parts = append(parts, discord.NewTextDisplay("### 📋 QUEUE\nNothing queued yet."))
	} else {
		start := (page - 1) * queuePageSize
		end := min(start+queuePageSize, len(s.Tracks))
		parts = append(parts,
			discord.NewTextDisplay(fmt.Sprintf("### 📋 QUEUE · %d–%d of %d", start+1, end, len(s.Tracks))),
			discord.NewLargeSeparator(),
			// imported titles are unbounded, and discord rejects an oversized
			// text display outright
			discord.NewTextDisplay(truncate(renderQueueTracks(s.Tracks, start, end, s.CurrentIndex))),
			discord.NewLargeSeparator(),
			queuePagerRow(page, pages),
		)
	}

	container := discord.NewContainer(parts...).WithAccentColor(playerPanelAccent)
	return discord.NewMessageCreateV2(container)
}

// renderQueueTracks marks the playing track, so the marker is the only thing that has
// to move when the cursor does.
func renderQueueTracks(tracks []player.Track, start, end, current int) string {
	lines := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		mark := " "
		if i+1 == current {
			mark = "▶"
		}
		lines = append(lines, fmt.Sprintf("%s `%02d` %s", mark, i+1, escapePanelMarkdown(tracks[i].Title)))
	}
	return strings.Join(lines, "\n")
}

// Two controls only: a jump-to-first and a jump-to-last would carry the same custom id
// as prev and next on every interior page, and discord rejects a payload whose custom
// ids are duplicated.
func queuePagerRow(page, pages int) discord.ActionRowComponent {
	prev := playerPanelButton(discord.ButtonStyleSecondary, "◀", queuePageID(page-1))
	next := playerPanelButton(discord.ButtonStyleSecondary, "▶", queuePageID(page+1))

	if page == 1 {
		prev = prev.AsDisabled()
	}
	if page == pages {
		next = next.AsDisabled()
	}
	return discord.NewActionRow(prev, next)
}
