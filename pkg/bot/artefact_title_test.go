package dbot

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Paths copied verbatim from the real backup database, so these cases are the shapes
// that actually exist on disk rather than ones invented here.
func TestArtefactTitleRecoversFromPath(t *testing.T) {
	tests := []struct {
		name string
		url  string
		path string
		want string
	}{
		{
			name: "plain title",
			url:  "https://www.youtube.com/watch?v=0Rdg6GUv-wQ",
			path: "438758201916129281/play_history/edit.Hiszpańskie Dziewczyny [0Rdg6GUv-wQ].mp4",
			want: "Hiszpańskie Dziewczyny",
		},
		{
			name: "punctuation and symbols",
			url:  "https://www.youtube.com/watch?v=mOWuh6XQ9NQ",
			path: "438758201916129281/play_history/edit.Czarnobrody Kapitan (my own song) [mOWuh6XQ9NQ].mp4",
			want: "Czarnobrody Kapitan (my own song)",
		},
		{
			name: "brackets inside the title are kept",
			url:  "https://www.youtube.com/watch?v=EBxorz5MOxo",
			path: "438758201916129281/play_history/edit.Cztery Refy-Ballada o Botany Bay [Szanty] [EBxorz5MOxo].mp4",
			want: "Cztery Refy-Ballada o Botany Bay [Szanty]",
		},
		{
			name: "short url form",
			url:  "https://youtu.be/0Rdg6GUv-wQ",
			path: "438758201916129281/play_history/edit.Some Song [0Rdg6GUv-wQ].mp4",
			want: "Some Song",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, artefactTitle(tt.url, tt.path))
		})
	}
}

// Anything that is not a cached yt-dlp video must yield no title rather than a wrong one.
func TestArtefactTitleRefusesUnknownShapes(t *testing.T) {
	const url = "https://www.youtube.com/watch?v=abc12345678"
	tests := []struct {
		name string
		url  string
		path string
	}{
		{"sound, named after its alias", url, "1/sounds/honk.mp4"},
		{"attachment, time prefixed", url, "1/attachments/1791119508-shot.png"},
		{"truncated before the id suffix", url, "1/play_history/edit.A Very Long Title That Got Cut.mp4"},
		{"a different video's id", url, "1/play_history/edit.Song [zzz999].mp4"},
		{"doubled extension is not the shape we write", url, "1/play_history/edit.Song [abc12345678].mp4.mp4"},
		{"not youtube", "https://example.com/watch?v=abc12345678", "1/play_history/edit.Song [abc12345678].mp4"},
		{"no url at all", "", "1/play_history/edit.Song [abc12345678].mp4"},
		{"empty title before the id", url, "1/play_history/edit. [abc12345678].mp4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Empty(t, artefactTitle(tt.url, tt.path))
		})
	}
}

// The wiring the panel depends on, driven through the real /play path: a cached play
// must queue a titled track, so the panel renders a name instead of (untitled).
func TestPlayCachedArtefactQueuesTheRecoveredTitle(t *testing.T) {
	tb := newTestBot(t)
	// connectVoice returns early once the player owns a connection, so this exercises
	// play's caching branch without a real voice connection
	require.NoError(t, tb.MusicPlayer.SetVC(stubConn{gid: panelGID}, panelBotVC))

	url := canonicalVideoURL("0Rdg6GUv-wQ")
	tb.writeArtefactFile(t, url, testGID+"/play_history/edit.Hiszpańskie Dziewczyny [0Rdg6GUv-wQ].mp4")

	require.NoError(t, tb.play(tb.Ctx, testGID, panelUser.String(), url))

	tracks := tb.Player().QueueSnapshot().Tracks
	require.Len(t, tracks, 1)
	assert.Equal(t, "Hiszpańskie Dziewczyny", tracks[0].Title,
		"a cached play must not queue untitled")
	assert.Equal(t, url, tracks[0].URL, "the url stays the source")
	assert.Equal(t, artefactFile(testGID+"/play_history/edit.Hiszpańskie Dziewczyny [0Rdg6GUv-wQ].mp4"),
		tracks[0].Filepath, "and it still plays from disk")
}

// An artefact we cannot read a title from still plays, just untitled - never wrongly.
func TestPlayUnrecoverableArtefactStillQueues(t *testing.T) {
	tb := newTestBot(t)
	require.NoError(t, tb.MusicPlayer.SetVC(stubConn{gid: panelGID}, panelBotVC))

	url := canonicalVideoURL("0Rdg6GUv-wQ")
	tb.writeArtefactFile(t, url, testGID+"/play_history/edit.Truncated Before The Id.mp4")

	require.NoError(t, tb.play(tb.Ctx, testGID, panelUser.String(), url))

	tracks := tb.Player().QueueSnapshot().Tracks
	require.Len(t, tracks, 1)
	assert.Empty(t, tracks[0].Title)
	assert.Equal(t, url, tracks[0].URL)
}
