package dbot

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseYTLink(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantOK   bool
		videoID  string
		playlist string
		isList   bool
	}{
		{name: "playlist www", input: "https://www.youtube.com/playlist?list=PL123", wantOK: true, playlist: "PL123", isList: true},
		{name: "playlist bare", input: "https://youtube.com/playlist?list=PL123", wantOK: true, playlist: "PL123", isList: true},
		{name: "playlist mobile", input: "https://m.youtube.com/playlist?list=PL123", wantOK: true, playlist: "PL123", isList: true},
		{name: "playlist music", input: "https://music.youtube.com/playlist?list=PL123", wantOK: true, playlist: "PL123", isList: true},
		{name: "watch video", input: "https://www.youtube.com/watch?v=abc123", wantOK: true, videoID: "abc123"},
		{name: "short link", input: "https://youtu.be/abc123", wantOK: true, videoID: "abc123"},
		{name: "shorts", input: "https://www.youtube.com/shorts/abc123", wantOK: true, videoID: "abc123"},
		// v= wins over list= even when both are present
		{name: "video with list", input: "https://www.youtube.com/watch?v=abc123&list=PL123", wantOK: true, videoID: "abc123", playlist: "PL123"},
		{name: "video with extra params", input: "https://www.youtube.com/watch?feature=share&v=abc123&t=30", wantOK: true, videoID: "abc123"},
		{name: "not youtube", input: "https://example.com/playlist?list=PL123"},
		{name: "no scheme", input: "youtube.com/playlist?list=PL123"},
		{name: "garbage", input: "not a url"},
		{name: "lookalike host", input: "https://notyoutube.com/playlist?list=PL123"},
		{name: "youtube without id", input: "https://www.youtube.com/watch"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseYTLink(tt.input)
			require.Equal(t, tt.wantOK, ok)
			if !tt.wantOK {
				return
			}
			assert.Equal(t, tt.videoID, got.VideoID)
			assert.Equal(t, tt.playlist, got.PlaylistID)
			assert.Equal(t, tt.isList, got.Playlist)
		})
	}
}

func TestCanonicalURLs(t *testing.T) {
	assert.Equal(t, "https://www.youtube.com/watch?v=abc", canonicalVideoURL("abc"))
	assert.Equal(t, "https://www.youtube.com/playlist?list=PL1", canonicalPlaylistURL("PL1"))
}
