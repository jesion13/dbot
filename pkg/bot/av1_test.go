package dbot

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"dbot/pkg/config"
	"dbot/pkg/ytdlp"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The decision the whole AV1 change rests on.
func TestNeedsAV1Encode(t *testing.T) {
	tests := []struct {
		vcodec string
		want   bool
	}{
		{"av01.0.12M.08", false}, // what youtube actually reports
		{"av01", false},
		{"av1", true}, // not the profile name youtube uses; treat as unknown
		{"vp09.00.50.08", true},
		{"vp9", true},
		{"avc1.640028", true}, // h264
		{"", true},            // audio only, as SaveSound downloads
	}
	for _, tt := range tests {
		t.Run(tt.vcodec, func(t *testing.T) {
			assert.Equal(t, tt.want, needsAV1Encode(tt.vcodec))
		})
	}
}

// stubDownload returns a context carrying a scratch directory, with a file already in
// it and downloadVideo reporting it. yt-dlp is never invoked.
func stubDownload(t *testing.T, tb *testBot, name string, meta ytdlp.VideoMeta) context.Context {
	t.Helper()
	dir := t.TempDir()
	full := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(full, []byte("media"), 0o644))

	meta.Filepath = full
	tb.DownloadVideoFunc = func(context.Context, string) (ytdlp.VideoMeta, error) {
		return meta, nil
	}
	return context.WithValue(tb.Ctx, config.DirKey, dir)
}

// The point of the change: an av1 download is stored untouched. Re-encoding it made the
// file bigger (a 4K av1 source is 441MB; the same video through the h264 path came to
// 678MB) and cost minutes of hardware encoding.
func TestFetchVideoKeepsAnAV1Download(t *testing.T) {
	tb := newTestBot(t)
	ctx := stubDownload(t, tb, "FlWII9xFabo.webm", ytdlp.VideoMeta{
		ID: "FlWII9xFabo", Title: "It’s not just you",
		VCodec: "av01.0.12M.08", Ext: "webm",
	})

	got, err := tb.fetchVideo(ctx, "https://www.youtube.com/watch?v=FlWII9xFabo")
	require.NoError(t, err)

	assert.False(t, got.Transcoded, "an av1 download must not be re-encoded")
	assert.Equal(t, filepath.Base(got.Name), "FlWII9xFabo.webm", "the download is stored as it came")
	assert.Equal(t, got.Filepath, got.SourcePath,
		"the stored file is the download, so that is the path the job deletes afterwards")
	assert.Equal(t, "It’s not just you", got.Title)
	assert.Equal(t, "av01.0.12M.08", got.VCodec)
	assert.Equal(t, "webm", got.Ext)
	require.NotNil(t, got.File)
	t.Cleanup(func() { _ = got.File.Close() })
}

// A non-av1 source must reach the av1 encoder. The encode needs hardware ffmpeg, so the
// assertion is that it failed *there* rather than that it silently returned something.
func TestFetchVideoSendsNonAV1ThroughTheAV1Encoder(t *testing.T) {
	for _, vcodec := range []string{"vp09.00.50.08", "avc1.640028", ""} {
		t.Run("vcodec="+vcodec, func(t *testing.T) {
			tb := newTestBot(t)
			ctx := stubDownload(t, tb, "clip.mp4", ytdlp.VideoMeta{
				ID: "clip", Title: "Some Song", VCodec: vcodec, Ext: "mp4",
			})

			_, err := tb.fetchVideo(ctx, "https://example.com/x")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "convertToAV1",
				"only the av1 encoder may run for a non-av1 source")
		})
	}
}

func TestMediaTypeForExt(t *testing.T) {
	tests := map[string]string{
		"webm":  "video/webm",
		"WEBM":  "video/webm",
		".webm": "video/webm",
		"mkv":   "video/x-matroska",
		"mp4":   "video/mp4",
		"":      "video/mp4",
	}
	for ext, want := range tests {
		assert.Equal(t, want, mediaTypeForExt(ext), "ext %q", ext)
	}
}

// The stored name must not claim mp4 for a webm, now that the download is kept as-is.
func TestStoredNameCarriesTheRealExtension(t *testing.T) {
	url := "https://www.youtube.com/watch?v=FlWII9xFabo"
	assert.True(t, hasSuffix(artefactName(url, "FlWII9xFabo", "webm"), ".webm"))
	assert.True(t, hasSuffix(artefactName(url, "FlWII9xFabo", "mkv"), ".mkv"))
	assert.True(t, hasSuffix(artefactName(url, "FlWII9xFabo", "mp4"), ".mp4"))
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// The job is the only place the download can be deleted, and nothing else covers it.
// With the download stubbed and an av1 source (no encode needed) the whole job runs.
func TestBackupJobStoresAndThenDeletesTheDownload(t *testing.T) {
	tb := newTestBot(t)
	config.TMP_PATH = t.TempDir()

	url := canonicalVideoURL("FlWII9xFabo")
	downloadDir := filepath.Join(config.TMP_PATH, "backup")
	require.NoError(t, os.MkdirAll(downloadDir, 0o755))
	download := filepath.Join(downloadDir, "FlWII9xFabo.webm")
	require.NoError(t, os.WriteFile(download, []byte("media"), 0o644))

	tb.DownloadVideoFunc = func(context.Context, string) (ytdlp.VideoMeta, error) {
		return ytdlp.VideoMeta{
			ID: "FlWII9xFabo", Title: "It’s not just you",
			VCodec: "av01.0.12M.08", Ext: "webm", Filepath: download,
		}, nil
	}

	meta, err := backupJobMeta(testGID, videoBackupDir, url)
	require.NoError(t, err)
	require.NoError(t, tb.backupVideoJob(meta))

	// stored under the id-based name, with the title
	art, err := tb.Backup.GetArtefact(tb.Ctx, url)
	require.NoError(t, err)
	assert.Equal(t, "It’s not just you", art.Title)
	assert.Equal(t, "FlWII9xFabo_"+hash8(url)+".webm", filepath.Base(art.Path))
	assert.Equal(t, "video/webm", art.MediaType, "a stored webm is not recorded as an mp4")
	assert.FileExists(t, artefactFile(art.Path), "the stored copy must exist")

	// and the download is gone, which is what used to leak
	assert.NoFileExists(t, download,
		"the yt-dlp download must be removed once it has been stored")
}
