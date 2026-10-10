package dbot

import (
	"testing"

	"dbot/pkg/store"

	"github.com/disgoorg/disgo/discord"
	"github.com/stretchr/testify/assert"
)

func TestPlaylistChoices(t *testing.T) {
	playlists := []store.Playlist{
		{ID: 1, Title: "dupa"},
		{ID: 2, Title: "dupa"},
		{ID: 3, Title: "other"},
	}

	choices := playlistChoices(playlists)
	require := assert.New(t)
	require.Len(choices, 3)

	// values are internal ids, so duplicate titles stay selectable
	require.Equal("1", choices[0].(discord.AutocompleteChoiceString).Value)
	require.Equal("2", choices[1].(discord.AutocompleteChoiceString).Value)
	require.Equal("3", choices[2].(discord.AutocompleteChoiceString).Value)

	// duplicated titles get a suffix, unique ones stay clean
	require.Equal("dupa (1)", choices[0].(discord.AutocompleteChoiceString).Name)
	require.Equal("dupa (2)", choices[1].(discord.AutocompleteChoiceString).Name)
	require.Equal("other", choices[2].(discord.AutocompleteChoiceString).Name)

	require.Empty(playlistChoices(nil))
}
