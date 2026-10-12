package dbot

import (
	"fmt"
	"strings"

	"dbot/pkg/player"
	"github.com/disgoorg/disgo/discord"
)

const playerPanelAccent = 0x5865F2

// Button ids carry the generation the panel was published with: a click from a panel
// that has since been replaced names an older one, so it is refused even if the old
// message was never deleted.
const (
	playerPanelBtnPrefix = "player:"
	playerPanelBtnPrev   = playerPanelBtnPrefix + "prev"
	playerPanelBtnPause  = playerPanelBtnPrefix + "pause"
	playerPanelBtnNext   = playerPanelBtnPrefix + "next"
	playerPanelBtnEvent  = playerPanelBtnPrefix + "event"
)

// playerPanelID is one button's custom id for a given panel generation.
func playerPanelID(generation uint64, action string) string {
	return fmt.Sprintf("%s%d:%s", playerPanelBtnPrefix, generation, action)
}

// The controls carry no text: the bot made this message, so the user can already see
// what each glyph means, and a caption beside it just repeats itself.
const (
	playerPanelEmojiPrev  = "⏮"
	playerPanelEmojiPause = "⏸"
	playerPanelEmojiPlay  = "▶"
	playerPanelEmojiStop  = "⏹"
	playerPanelEmojiNext  = "⏭"
	playerPanelEmojiEvent = "🇪"
)

func playerPanelButton(style discord.ButtonStyle, emoji, customID string) discord.ButtonComponent {
	return discord.NewSecondaryButton("", customID).
		WithStyle(style).
		WithEmoji(discord.ComponentEmoji{Name: emoji})
}

// buildPlayerPanelV2 renders the absolute queue window:
// up to 3 preceding tracks, the current track, and up to 10 following tracks.
// It deliberately does not inspect playback history.
func buildPlayerPanelV2(generation uint64, s player.QueueSnapshot) discord.MessageCreate {
	// no title: the bot made this message, so a header naming the bot says nothing the
	// reader does not already know
	var parts []discord.ContainerSubComponent

	current := s.CurrentIndex
	validCurrent := current >= 1 && current <= len(s.Tracks) && !s.Closed

	switch {
	case s.Closed:
		parts = append(parts, discord.NewTextDisplay("### ⛔ Player closed"))
	case validCurrent && s.Paused:
		parts = append(parts, discord.NewTextDisplay(fmt.Sprintf("### ⏸ PAUSED · %d / %d", current, len(s.Tracks))))
	case validCurrent:
		parts = append(parts, discord.NewTextDisplay(fmt.Sprintf("### ▶ PLAYING · %d / %d", current, len(s.Tracks))))
	case len(s.Tracks) == 0:
		parts = append(parts,
			discord.NewTextDisplay("### ⏹ STOPPED\nNothing queued yet."),
		)
	default:
		parts = append(parts, discord.NewTextDisplay("### ⏹ STOPPED"))
	}

	if validCurrent {
		cursor := current - 1
		prevStart := max(0, cursor-3)
		if prevStart < cursor {
			parts = append(parts,
				discord.NewSmallSeparator(),
				discord.NewTextDisplay("**⏮ PREVIOUS POSITIONS**\n"+renderPanelTracks(s.Tracks, prevStart, cursor)),
			)
		}

		parts = append(parts,
			discord.NewLargeSeparator(),
			discord.NewTextDisplay(fmt.Sprintf("### 🔊 NOW PLAYING · #%d\n**%s**", current, escapePanelMarkdown(s.Tracks[cursor].Title))),
		)

		nextEnd := min(len(s.Tracks), current+10)
		if current < nextEnd {
			parts = append(parts,
				discord.NewSmallSeparator(),
				discord.NewTextDisplay("**⏭ UP NEXT**\n"+renderPanelTracks(s.Tracks, current, nextEnd)),
			)
		}
	} else if len(s.Tracks) > 0 {
		// With no current cursor, show the tail of the stable lineup without
		// falsely marking any track as playing.
		start := max(0, len(s.Tracks)-14)
		parts = append(parts,
			discord.NewSmallSeparator(),
			discord.NewTextDisplay("**LAST POSITIONS · NOT PLAYING**\n"+renderPanelTracks(s.Tracks, start, len(s.Tracks))),
		)
	}

	if s.SoundWait > 0 {
		word := "sequences"
		if s.SoundWait == 1 {
			word = "sequence"
		}
		parts = append(parts,
			discord.NewSmallSeparator(),
			discord.NewTextDisplay(fmt.Sprintf("🔉 **%d sound %s waiting**", s.SoundWait, word)),
		)
	}

	previous := playerPanelButton(discord.ButtonStyleSecondary, playerPanelEmojiPrev, playerPanelID(generation, playerPanelBtnPrev))
	pause := playerPanelButton(discord.ButtonStyleDanger, pauseButtonEmoji(s), playerPanelID(generation, playerPanelBtnPause))
	next := playerPanelButton(discord.ButtonStyleSecondary, playerPanelEmojiNext, playerPanelID(generation, playerPanelBtnNext))
	event := playerPanelButton(discord.ButtonStyleSecondary, playerPanelEmojiEvent, playerPanelID(generation, playerPanelBtnEvent))

	if s.Closed {
		previous = previous.AsDisabled()
		pause = pause.AsDisabled()
		next = next.AsDisabled()
		event = event.AsDisabled()
	} else {
		if len(s.Tracks) == 0 || (validCurrent && current == 1) {
			previous = previous.AsDisabled()
		}
		if len(s.Tracks) == 0 {
			next = next.AsDisabled()
		}
		if !validCurrent {
			pause = pause.AsDisabled()
		}
	}
	controls := discord.NewActionRow(previous, pause, next, event)
	parts = append(parts, discord.NewLargeSeparator(), controls)

	container := discord.NewContainer(parts...).WithAccentColor(playerPanelAccent)
	return discord.NewMessageCreateV2(container)
}

func pauseButtonEmoji(s player.QueueSnapshot) string {
	if s.Closed || s.CurrentIndex == 0 {
		return playerPanelEmojiStop
	}
	if s.Paused {
		return playerPanelEmojiPlay
	}
	return playerPanelEmojiPause
}

// renderPanelTracks uses absolute 1-based positions. start is inclusive and end
// is exclusive, matching Go slice conventions.
func renderPanelTracks(tracks []player.Track, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end > len(tracks) {
		end = len(tracks)
	}
	if start >= end {
		return "_No tracks_"
	}

	lines := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		lines = append(lines, fmt.Sprintf("`%02d` %s", i+1, escapePanelMarkdown(tracks[i].Title)))
	}
	return strings.Join(lines, "\n")
}

func escapePanelMarkdown(title string) string {
	title = displayTitle(title)
	return strings.NewReplacer(
		"\\", "\\\\",
		"*", "\\*",
		"_", "\\_",
		"~", "\\~",
		"`", "\\`",
		"|", "\\|",
		">", "\\>",
		"[", "\\[",
		"]", "\\]",
		"(", "\\(",
		")", "\\)",
	).Replace(title)
}

// retiredPanelMessage is what a panel we could not delete is rewritten to. A Components
// V2 message can only be updated with another V2 payload, so this is a full container
// again - the notice, and the controls disabled so nothing looks clickable.
func retiredPanelMessage() discord.MessageCreate {
	disabled := func(e string, id string) discord.ButtonComponent {
		return discord.NewSecondaryButton("", id).WithEmoji(discord.ComponentEmoji{Name: e}).AsDisabled()
	}
	row := discord.NewActionRow(
		disabled(playerPanelEmojiPrev, playerPanelBtnPrefix+"retired-prev"),
		disabled(playerPanelEmojiPause, playerPanelBtnPrefix+"retired-pause"),
		disabled(playerPanelEmojiNext, playerPanelBtnPrefix+"retired-next"),
		disabled(playerPanelEmojiEvent, playerPanelBtnPrefix+"retired-event"),
	)
	container := discord.NewContainer(
		discord.NewTextDisplay("### This player panel is no longer active"),
		discord.NewSmallSeparator(),
		row,
	)
	return discord.NewMessageCreateV2(container)
}

// Imported metadata carries newlines, tabs and space runs; rendered raw, one entry
// spills over several lines and the layout stops lining up.
func displayTitle(title string) string {
	one := strings.Join(strings.Fields(title), " ")
	if one == "" {
		return untitledTrack
	}
	return one
}

// stands in for a title that is empty or whitespace only
const untitledTrack = "(untitled)"
