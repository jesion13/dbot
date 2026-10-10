package dbot

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"

	"dbot/pkg/store"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
)

// playlistAutocomplete suggests saved playlists for the focused `name` option.
// The option value is the internal playlist id, never the title: titles collide.
func (d *DBot) playlistAutocomplete(e *events.AutocompleteInteractionCreate) {
	if e.Data.CommandName != "playlist" {
		return
	}

	var (
		input  string
		guild  string
		option = e.Data.Focused()
	)
	if option.Type == discord.ApplicationCommandOptionTypeString {
		// AutocompleteOption.String() panics on malformed values; autocomplete must
		// never take the event loop down
		if err := json.Unmarshal(option.Value, &input); err != nil {
			input = ""
		}
	}
	if e.GuildID() != nil {
		guild = e.GuildID().String()
	}

	// filtering happens in SQL, before the limit, so a partial title can still match
	// playlists beyond the first page
	playlists, err := d.Store.SearchPlaylists(d.Ctx, store.SearchPlaylistsParams{
		GuildID: guild,
		Title:   input,
	})
	if err != nil {
		slog.Error("playlist autocomplete failed", slog.Any("error", err))
		// answer with nothing rather than leaving the interaction hanging
		if err := e.AutocompleteResult([]discord.AutocompleteChoice{}); err != nil {
			slog.Error("autocomplete respond failed", slog.Any("error", err))
		}
		return
	}

	choices := playlistChoices(playlists)
	if err := e.AutocompleteResult(choices); err != nil {
		slog.Error("autocomplete respond failed", slog.Any("error", err))
	}
}

// playlistChoices renders suggestions. The value is the internal id, never the title:
// duplicate titles are valid and the user must be able to tell them apart.
func playlistChoices(playlists []store.Playlist) []discord.AutocompleteChoice {
	titles := make(map[string]int, len(playlists))
	for _, p := range playlists {
		titles[p.Title]++
	}

	choices := make([]discord.AutocompleteChoice, 0, len(playlists))
	for _, p := range playlists {
		name := p.Title
		if titles[p.Title] > 1 {
			name = fmt.Sprintf("%s (%d)", name, p.ID)
		}
		choices = append(choices, discord.AutocompleteChoiceString{
			Name:  name,
			Value: strconv.FormatInt(p.ID, 10),
		})
	}
	return choices
}
