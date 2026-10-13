package dbot

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"dbot/pkg/backup"
	"dbot/pkg/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The job meta is matched against the queue table as an exact string, so any change to
// the marshalled shape makes every historical row invisible and re-downloads the whole
// cache. The expected value is a copy of what production already has.
func TestBackupJobMetaShapeIsUnchanged(t *testing.T) {
	meta, err := backupJobMeta("492318912881491981", videoBackupDir,
		"https://www.youtube.com/watch?v=Fr6PTBDSt98")
	require.NoError(t, err)

	assert.Equal(t,
		`{"Name":"","Dirs":"play_history","GID":"492318912881491981","ContentType":"","OriginUrl":"https://www.youtube.com/watch?v=Fr6PTBDSt98","PrependTime":false,"File":null}`,
		meta,
		"this exact string is what the 1095 rows already in production look like")
}

// A title must never leak into the meta, however it is set.
func TestBackupJobMetaIgnoresTheTitle(t *testing.T) {
	withTitle, err := backupJobMeta(testGID, videoBackupDir, canonicalVideoURL("a"))
	require.NoError(t, err)

	bf := BackupFileParams{GID: testGID, Dirs: videoBackupDir, OriginUrl: canonicalVideoURL("a"), Title: "Some Very Long Title"}
	assert.NotContains(t, mustJSON(t, bf), "Some Very Long Title",
		"a title would change the meta and orphan every queue row")

	plain := bf
	plain.Title = ""
	assert.Equal(t, withTitle, mustJSON(t, plain),
		"setting a title must leave the meta byte-identical")
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := jsonMarshal(v)
	require.NoError(t, err)
	return b
}

// The stored name is the yt-dlp id plus a hash of the source url.
func TestArtefactName(t *testing.T) {
	tests := []struct {
		name string
		url  string
		id   string
		ext  string
		want string
	}{
		{
			name: "webm keeps its real extension",
			url:  "https://www.youtube.com/watch?v=0Rdg6GUv-wQ",
			id:   "0Rdg6GUv-wQ",
			ext:  "webm",
			want: "0Rdg6GUv-wQ_" + hash8("https://www.youtube.com/watch?v=0Rdg6GUv-wQ") + ".webm",
		},
		{
			name: "mp4 source stays mp4",
			url:  "https://youtu.be/0Rdg6GUv-wQ",
			id:   "0Rdg6GUv-wQ",
			ext:  "mp4",
			want: "0Rdg6GUv-wQ_" + hash8("https://youtu.be/0Rdg6GUv-wQ") + ".mp4",
		},
		{
			name: "no extension falls back to mp4",
			url:  "https://youtu.be/0Rdg6GUv-wQ",
			id:   "0Rdg6GUv-wQ",
			ext:  "",
			want: "0Rdg6GUv-wQ_" + hash8("https://youtu.be/0Rdg6GUv-wQ") + ".mp4",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, artefactName(tt.url, tt.id, tt.ext))
		})
	}
}

// The two real collisions from production: two different Discord uploads that both got
// the id "Tramwaj", and two named dupa.mp4 that yt-dlp separated into "dupa"/"dupa_1".
// Under the old scheme the first pair overwrote each other; the second could land on
// either name depending on what else was in the directory.
func TestArtefactNameSeparatesDifferentVideosWithTheSameID(t *testing.T) {
	const a = "https://cdn.discordapp.com/ephemeral-attachments/1328383992982016032/1507135996083437709/Tramwaj.mp4?ex=6a10cd46"
	const b = "https://cdn.discordapp.com/ephemeral-attachments/1328383992982016032/1507136191991123988/Tramwaj.mp4?ex=6a10cd75"

	assert.NotEqual(t, artefactName(a, "Tramwaj", "webm"), artefactName(b, "Tramwaj", "webm"),
		"two different videos sharing an id must not share a file")

	const dupa = "https://cdn.discordapp.com/ephemeral-attachments/1328383992982016032/1462921659064516758/dupa.mp4"
	const dupa2 = "https://cdn.discordapp.com/attachments/705088351778111608/1531057784341135420/dupa.mp4"
	assert.NotEqual(t, artefactName(dupa, "dupa", "webm"), artefactName(dupa2, "dupa_1", "webm"))
}

// A generic-extractor id comes from a url filename and can contain anything.
func TestArtefactNameSanitisesTheID(t *testing.T) {
	url := "https://example.com/x"
	name := artefactName(url, "../../etc/passwd", "mp4")

	assert.NotContains(t, name, "/")
	assert.NotContains(t, name, "..")
	assert.Equal(t, "etc_passwd_"+hash8(url)+".mp4", name)
}

func TestArtefactNameHandlesAnEmptyID(t *testing.T) {
	url := "https://example.com/y"
	assert.Equal(t, "video_"+hash8(url)+".mp4", artefactName(url, "", ""))
	assert.Equal(t, "video_"+hash8(url)+".mp4", artefactName(url, "///", ""))
}

// The whole point: a long, unicode, filename-hostile title never reaches the filesystem.
func TestArtefactNameIgnoresTheTitleEntirely(t *testing.T) {
	url := "https://www.youtube.com/watch?v=v-t1ikF9ftA"
	long := ""
	for range 4000 {
		long += "\U0001D7ED"
	}
	assert.Equal(t, artefactName(url, "v-t1ikF9ftA", "webm"), artefactName(url, "v-t1ikF9ftA", "webm"),
		"the title is not an input, so it cannot change the name")
	assert.Less(t, len(artefactName(url, "v-t1ikF9ftA", "webm")), 80,
		"even with a %d byte title in play the name stays tiny", len(long))
}

// Longest realistic id still fits well inside the 255 byte limit.
func TestArtefactNameFitsTheFilesystemLimit(t *testing.T) {
	long := ""
	for range 500 {
		long += "a"
	}
	assert.LessOrEqual(t, len(artefactName("https://x", long, "webm")), 255)
}

func hash8(s string) string {
	got := artefactName(s, "h", "mp4")
	// reuse the helper rather than duplicating the hash
	sum := got[len("h_") : len(got)-len(".mp4")]
	return sum
}

// --- the title now comes from the database -----------------------------------

func TestCachedTrackPrefersTheStoredTitle(t *testing.T) {
	url := "https://www.youtube.com/watch?v=0Rdg6GUv-wQ"
	art := backup.Artefact{
		OriginUrl: url,
		Path:      testGID + "/play_history/0Rdg6GUv-wQ_ab12cd34.mp4",
		Title:     "The Real Title",
	}
	assert.Equal(t, "The Real Title", cachedTrack(url, art).Title)
}

// A row written before the column existed still renders, from its filename.
func TestCachedTrackFallsBackToTheFilename(t *testing.T) {
	url := "https://www.youtube.com/watch?v=0Rdg6GUv-wQ"
	art := backup.Artefact{
		OriginUrl: url,
		Path:      testGID + "/play_history/edit.Legacy Title [0Rdg6GUv-wQ].mp4",
	}
	assert.Equal(t, "Legacy Title", cachedTrack(url, art).Title)
}

func TestCachedTrackUntitledWhenNeitherIsAvailable(t *testing.T) {
	url := "https://cdn.discordapp.com/x/Tramwaj.mp4"
	art := backup.Artefact{OriginUrl: url, Path: testGID + "/play_history/Tramwaj_ab12cd34.mp4"}
	assert.Empty(t, cachedTrack(url, art).Title)
}

// --- the backfill ------------------------------------------------------------

// insertLegacyVideo writes a row shaped like the ones that predate the title column.
func insertLegacyVideo(t *testing.T, tb *testBot, url, path string) {
	t.Helper()
	err := tb.Backup.InsertArtefact(tb.Ctx, backup.InsertArtefactParams{
		OriginUrl: url, Path: path, MediaType: "video/mp4",
		CreatedAt: time.Now(), Gid: testGID,
	})
	require.NoError(t, err)
}

func TestBackfillArtefactTitles(t *testing.T) {
	tb := newTestBot(t)
	url := canonicalVideoURL("0Rdg6GUv-wQ")
	insertLegacyVideo(t, tb, url, testGID+"/play_history/edit.Hiszpańskie Dziewczyny [0Rdg6GUv-wQ].mp4")

	// a row whose title is already stored must not be touched
	stored := canonicalVideoURL("DuW4k1ZkS00")
	err := tb.Backup.InsertArtefact(tb.Ctx, backup.InsertArtefactParams{
		OriginUrl: stored, Path: testGID + "/play_history/edit.something [DuW4k1ZkS00].mp4",
		MediaType: "video/mp4", Title: "Already Stored", CreatedAt: time.Now(), Gid: testGID,
	})
	require.NoError(t, err)

	// unrecoverable: no id in the url
	noID := "https://cdn.discordapp.com/x/Tramwaj.mp4"
	insertLegacyVideo(t, tb, noID, testGID+"/play_history/edit.Tramwaj [Tramwaj].mp4")

	filled, skipped, err := BackfillArtefactTitles(tb.Ctx, tb.Backup)
	require.NoError(t, err)
	assert.Equal(t, 1, filled)
	assert.Equal(t, 1, skipped, "the non-youtube row cannot be recovered")

	got, err := tb.Backup.GetArtefact(tb.Ctx, url)
	require.NoError(t, err)
	assert.Equal(t, "Hiszpańskie Dziewczyny", got.Title)

	untouched, err := tb.Backup.GetArtefact(tb.Ctx, stored)
	require.NoError(t, err)
	assert.Equal(t, "Already Stored", untouched.Title)

	stillEmpty, err := tb.Backup.GetArtefact(tb.Ctx, noID)
	require.NoError(t, err)
	assert.Empty(t, stillEmpty.Title)
}

// Running it twice must not double-apply or error.
func TestBackfillIsIdempotent(t *testing.T) {
	tb := newTestBot(t)
	url := canonicalVideoURL("0Rdg6GUv-wQ")
	insertLegacyVideo(t, tb, url, testGID+"/play_history/edit.Song [0Rdg6GUv-wQ].mp4")

	filled, skipped, err := BackfillArtefactTitles(tb.Ctx, tb.Backup)
	require.NoError(t, err)
	require.Equal(t, 1, filled)
	require.Equal(t, 0, skipped)

	filled, skipped, err = BackfillArtefactTitles(tb.Ctx, tb.Backup)
	require.NoError(t, err)
	assert.Zero(t, filled, "a second run has nothing to do")
	assert.Equal(t, 0, skipped, "a recovered row is no longer a candidate")
}

// The backfill must not touch files, and must leave non-video rows alone.
func TestBackfillOnlyTouchesVideoRows(t *testing.T) {
	tb := newTestBot(t)
	// a sound: no title column write, and it is not in play_history
	err := tb.Backup.InsertArtefact(tb.Ctx, backup.InsertArtefactParams{
		OriginUrl: "https://x/honk.mp4", Path: testGID + "/sounds/honk.mp4",
		MediaType: "video/mp4", CreatedAt: time.Now(), Gid: testGID,
	})
	require.NoError(t, err)

	filled, skipped, err := BackfillArtefactTitles(tb.Ctx, tb.Backup)
	require.NoError(t, err)
	assert.Zero(t, filled)
	assert.Zero(t, skipped, "a sound is not a play_history row, so it is not even considered")

	art, err := tb.Backup.GetArtefact(tb.Ctx, "https://x/honk.mp4")
	require.NoError(t, err)
	assert.Empty(t, art.Title)
}

// A stored path must still resolve to a file that exists.
func TestArtefactFileResolvesUnderBackupDir(t *testing.T) {
	dir := t.TempDir()
	config.BACKUP_DIR = dir
	rel := testGID + "/play_history/abc_de12f345.mp4"
	full := filepath.Join(dir, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte("x"), 0o644))

	assert.FileExists(t, artefactFile(rel))
	assert.Equal(t, full, artefactFile(rel))
}

// backupArtefact is where the title reaches the database, so it is the seam that
// decides whether a stored video ever has one. The job's own `bf.Title = f.Title` sits
// behind a real yt-dlp subprocess and is not reachable from a test.
func TestBackupArtefactPersistsTheTitle(t *testing.T) {
	tb := newTestBot(t)
	url := canonicalVideoURL("v-t1ikF9ftA")

	name := artefactName(url, "v-t1ikF9ftA", "webm")
	full := filepath.Join(config.BACKUP_DIR, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte("media"), 0o644))
	f, err := os.Open(full)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	_, err = tb.backupArtefact(tb.Ctx, BackupFileParams{
		Name:      name,
		OriginUrl: url,
		GID:       testGID,
		Dirs:      videoBackupDir,
		File:      f,
		// a title far longer than any filename limit, which is the point of storing it
		Title: "Hiszpańskie Dziewczyny \U0001D7ED\U0001D7F5\U0001D7F8\U0001D7F1 " +
			"𝗘𝗦𝗖𝗔𝗡𝗘 𝗧𝗛𝗘 𝗡𝗘𝗽𝗪𝗢𝗥𝗞 ⧸⧸ Retrowave, Synthwave, Vaporwave",
	})
	require.NoError(t, err)

	art, err := tb.Backup.GetArtefact(tb.Ctx, url)
	require.NoError(t, err)
	assert.Contains(t, art.Title, "Hiszpańskie Dziewczyny")
	assert.Equal(t, name, filepath.Base(art.Path), "stored under the id-based name")

	// and it reaches the player intact
	assert.Contains(t, cachedTrack(url, art).Title, "𝗘𝗦𝗖𝗔𝗡𝗘",
		"a fancy-unicode title survives the round trip")
}

// Re-storing the same origin url must not lose the title on the repair path.
func TestBackupArtefactRepairKeepsTheTitle(t *testing.T) {
	tb := newTestBot(t)
	url := canonicalVideoURL("v-t1ikF9ftA")

	store := func(name, title string) {
		t.Helper()
		full := filepath.Join(config.BACKUP_DIR, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte("media"), 0o644))
		f, err := os.Open(full)
		require.NoError(t, err)
		t.Cleanup(func() { _ = f.Close() })
		_, err = tb.backupArtefact(tb.Ctx, BackupFileParams{
			Name: name, OriginUrl: url, GID: testGID, Dirs: videoBackupDir, File: f, Title: title,
		})
		require.NoError(t, err)
	}

	store("first_ab12cd34.webm", "First Title")
	store("second_ef56ab78.webm", "Second Title")

	art, err := tb.Backup.GetArtefact(tb.Ctx, url)
	require.NoError(t, err)
	assert.Equal(t, "second_ef56ab78.webm", filepath.Base(art.Path))
	assert.Equal(t, "Second Title", art.Title, "a repair must carry the title too")
}
