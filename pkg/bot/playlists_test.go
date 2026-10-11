package dbot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	migrations "dbot/db"
	"dbot/pkg/backup"
	"dbot/pkg/config"
	"dbot/pkg/db"
	jobrunner "dbot/pkg/job_runner"
	"dbot/pkg/player"
	"dbot/pkg/store"
	"dbot/pkg/ytdlp"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// testPlaylistMeta builds a PlaylistMeta without touching yt-dlp.
func testPlaylistMeta(t *testing.T, id, title string, ids ...string) ytdlp.PlaylistMeta {
	t.Helper()
	meta := ytdlp.PlaylistMeta{ID: id, Title: title}
	for _, vid := range ids {
		dur := 10
		meta.Entries = append(meta.Entries, struct {
			ID       string `json:"id"`
			URL      string `json:"url"`
			Title    string `json:"title"`
			Duration *int   `json:"duration"`
		}{ID: vid, URL: canonicalVideoURL(vid), Title: "vid " + vid, Duration: &dur})
	}
	return meta
}

type testBot struct {
	*DBot
	store  *db.Store
	backup *backup.Queries
	dir    string
}

// newTestBot wires a DBot to fresh temp store/backup dbs. No network, no discord.
func newTestBot(t *testing.T) *testBot {
	t.Helper()
	dir := t.TempDir()
	config.DB_DIR = dir
	config.BACKUP_DIR = filepath.Join(dir, "backup")
	config.TMP_PATH = filepath.Join(dir, "tmp")
	require.NoError(t, os.MkdirAll(config.TMP_PATH, 0o755))
	require.NoError(t, os.MkdirAll(config.BACKUP_DIR, 0o755))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	st, err := db.ConnectStore(ctx, "test.db", "")
	require.NoError(t, err)
	bk, err := db.ConnectBackup(ctx, "backup.db", "")
	require.NoError(t, err)

	d := &DBot{
		Ctx:         ctx,
		Store:       st.Queries,
		StoreDB:     st,
		Backup:      bk,
		MusicPlayer: player.NewPlayer(),
	}
	return &testBot{DBot: d, store: st, backup: bk, dir: dir}
}

func (tb *testBot) queueLen(t *testing.T) int64 {
	t.Helper()
	n, err := tb.Store.CountQueueByJobType(tb.Ctx, BackupJob)
	require.NoError(t, err)
	return n
}

func jsonMarshal(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

func (tb *testBot) entries(t *testing.T, playlistID int64) []store.PlaylistEntry {
	t.Helper()
	e, err := tb.Store.ListPlaylistEntries(tb.Ctx, playlistID)
	require.NoError(t, err)
	return e
}

// writeArtefactFile creates a real file in the backup dir and indexes it.
func (tb *testBot) writeArtefactFile(t *testing.T, originURL, rel string) {
	t.Helper()
	full := filepath.Join(config.BACKUP_DIR, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte("media"), 0o644))
	err := tb.Backup.InsertArtefact(tb.Ctx, backup.InsertArtefactParams{
		OriginUrl: originURL,
		Path:      rel,
		MediaType: "video/mp4",
	})
	require.NoError(t, err)
}

func TestMigrationsFreshDB(t *testing.T) {
	tb := newTestBot(t)

	pl, err := tb.Store.UpsertPlaylist(tb.Ctx, store.UpsertPlaylistParams{
		GuildID:           testGID,
		YoutubePlaylistID: "PL1",
		Title:             "dupa",
		YoutubeUrl:        canonicalPlaylistURL("PL1"),
	})
	require.NoError(t, err)
	assert.NotZero(t, pl.ID)
	assert.True(t, pl.LastSyncedAt.Valid == false)

	// identity is (guild, youtube id), not the title
	_, err = tb.Store.UpsertPlaylist(tb.Ctx, store.UpsertPlaylistParams{
		GuildID:           testGID,
		YoutubePlaylistID: "PL1",
		Title:             "renamed",
		YoutubeUrl:        canonicalPlaylistURL("PL1"),
	})
	require.NoError(t, err)

	all, err := tb.Store.SearchPlaylists(tb.Ctx, store.SearchPlaylistsParams{GuildID: testGID, Title: ""})
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, "renamed", all[0].Title)
}

func TestMigrationsFromLegacySchema(t *testing.T) {
	// old install sitting at migration 007, i.e. still holding legacy playlist rows
	dir := t.TempDir()
	legacy := filepath.Join(dir, "legacy.db")
	w, err := sql.Open("sqlite", legacy)
	require.NoError(t, err)

	goose.SetBaseFS(migrations.DBmigrations)
	goose.SetDialect("sqlite3")
	require.NoError(t, goose.UpToContext(context.Background(), w, "migrations", 7))

	_, err = w.Exec(`
INSERT INTO playlists (guild_id, name, youtube_url) VALUES ('1', 'legacy', 'https://youtube.com/playlist?list=PLOLD');
INSERT INTO playlist_entries (playlist_id, youtube_url, minio_url, name) VALUES (1, 'https://youtube.com/watch?v=old', 'dbot,1/playlists/old.mp4', 'old');
INSERT INTO sounds (gid, url, aliases, origin_url) VALUES ('1', 'x.mp4', 'x', 'https://example.com/x.mp4');
`)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	config.DB_DIR = dir
	ctx := context.Background()
	st, err := db.ConnectStore(ctx, "legacy.db", "")
	require.NoError(t, err)

	// the new schema works against the upgraded file
	_, err = st.UpsertPlaylist(ctx, store.UpsertPlaylistParams{
		GuildID: testGID, YoutubePlaylistID: "PLNEW", Title: "new", YoutubeUrl: canonicalPlaylistURL("PLNEW"),
	})
	require.NoError(t, err)

	all, err := st.SearchPlaylists(ctx, store.SearchPlaylistsParams{GuildID: testGID, Title: ""})
	require.NoError(t, err)
	require.Len(t, all, 1)

	// unrelated data survives
	sounds, err := st.SelectSounds(ctx, "1")
	require.NoError(t, err)
	require.Len(t, sounds, 1)
}

func TestSaveSnapshotPositionsAndDuplicates(t *testing.T) {
	tb := newTestBot(t)

	// the same video twice is legal
	pl, err := tb.saveSnapshot(tb.Ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a", "b", "a"), false)
	require.NoError(t, err)

	entries := tb.entries(t, pl.ID)
	require.Len(t, entries, 3)
	assert.Equal(t, int64(0), entries[0].Position)
	assert.Equal(t, "a", entries[0].YoutubeVideoID.String)
	assert.Equal(t, int64(2), entries[2].Position)
	assert.Equal(t, "a", entries[2].YoutubeVideoID.String)
}

func TestSaveSnapshotIncomplete(t *testing.T) {
	tb := newTestBot(t)

	_, err := tb.saveSnapshot(tb.Ctx, testGID, testPlaylistMeta(t, "", "no id", "a"), false)
	assert.ErrorIs(t, err, ErrPlaylistIncomplete)

	_, err = tb.saveSnapshot(tb.Ctx, testGID, testPlaylistMeta(t, "PL1", "empty"), false)
	assert.ErrorIs(t, err, ErrPlaylistEmpty)
}

func TestSnapshotKeepsUnavailableEntries(t *testing.T) {
	tb := newTestBot(t)

	meta := testPlaylistMeta(t, "PL1", "dupa", "a", "b")
	// a nil duration usually means private/deleted, not removed from the playlist
	meta.Entries[1].Duration = nil
	meta.Entries[1].Title = ""

	pl, err := tb.saveSnapshot(tb.Ctx, testGID, meta, false)
	require.NoError(t, err)
	assert.Len(t, tb.entries(t, pl.ID), 2)
}

func TestSnapshotReconciliation(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx

	pl, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "old title", "a", "b", "c"), true)
	require.NoError(t, err)

	// membership changed, order changed, title changed, 'b' removed,
	// 'b' is still cached locally
	tb.writeArtefactFile(t, canonicalVideoURL("b"), testGID+"/play_history/b.mp4")

	pl2, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "new title", "c", "d", "a"), true)
	require.NoError(t, err)
	assert.Equal(t, pl.ID, pl2.ID, "same youtube id must not create a new playlist")
	assert.Equal(t, "new title", pl2.Title)

	entries := tb.entries(t, pl.ID)
	require.Len(t, entries, 3)
	assert.Equal(t, "c", entries[0].YoutubeVideoID.String)
	assert.Equal(t, "d", entries[1].YoutubeVideoID.String)
	assert.Equal(t, "a", entries[2].YoutubeVideoID.String)

	// removing a video from a playlist never touches its backup
	_, err = tb.Backup.GetArtefact(ctx, canonicalVideoURL("b"))
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(config.BACKUP_DIR, testGID+"/play_history/b.mp4"))
}

func TestSnapshotUpdateIsAtomic(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx

	pl, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a", "b"), false)
	require.NoError(t, err)

	// violate the unique (playlist_id, position) index halfway through the snapshot
	meta := testPlaylistMeta(t, "PL1", "dupa", "c", "d")
	_, err = tb.saveSnapshot(ctx, testGID, meta, false)
	require.NoError(t, err)
	assert.Len(t, tb.entries(t, pl.ID), 2)

	// a failure inside the transaction rolls the whole thing back
	err = tb.store.WithTx(ctx, func(q *store.Queries) error {
		if err := q.DeletePlaylistEntries(ctx, pl.ID); err != nil {
			return err
		}
		return errors.New("boom")
	})
	require.Error(t, err)
	assert.Len(t, tb.entries(t, pl.ID), 2, "rolled back transaction must not delete entries")
}

func TestLastSyncedAtOnlyOnSync(t *testing.T) {
	tb := newTestBot(t)

	pl, err := tb.saveSnapshot(tb.Ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a"), false)
	require.NoError(t, err)
	assert.False(t, pl.LastSyncedAt.Valid)

	pl, err = tb.saveSnapshot(tb.Ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a", "b"), true)
	require.NoError(t, err)
	assert.True(t, pl.LastSyncedAt.Valid)
}

func TestSyncPlaylistRejectsIDMismatch(t *testing.T) {
	tb := newTestBot(t)

	pl, err := tb.saveSnapshot(tb.Ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a", "b"), false)
	require.NoError(t, err)

	tb.PlaylistInfoFunc = func(string) (ytdlp.PlaylistMeta, error) {
		return testPlaylistMeta(t, "PLOTHER", "other", "a"), nil
	}
	_, err = tb.syncPlaylist(tb.Ctx, pl)
	require.ErrorIs(t, err, ErrPlaylistIDMismatch)

	// the old snapshot survives a failed sync
	assert.Len(t, tb.entries(t, pl.ID), 2)
}

func TestSyncPlaylistFailedFetchKeepsSnapshot(t *testing.T) {
	tb := newTestBot(t)

	pl, err := tb.saveSnapshot(tb.Ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a", "b"), false)
	require.NoError(t, err)

	tb.PlaylistInfoFunc = func(string) (ytdlp.PlaylistMeta, error) {
		return ytdlp.PlaylistMeta{}, errors.New("network down")
	}
	_, err = tb.syncPlaylist(tb.Ctx, pl)
	require.Error(t, err)
	assert.Len(t, tb.entries(t, pl.ID), 2, "failed sync must not clear the snapshot")

	// an empty/incomplete fetch is not evidence of removal
	tb.PlaylistInfoFunc = func(string) (ytdlp.PlaylistMeta, error) {
		return testPlaylistMeta(t, "PL1", "dupa"), nil
	}
	_, err = tb.syncPlaylist(tb.Ctx, pl)
	require.ErrorIs(t, err, ErrPlaylistEmpty)
	assert.Len(t, tb.entries(t, pl.ID), 2)
}

func TestSyncPlaylistAppliesNewSnapshot(t *testing.T) {
	tb := newTestBot(t)

	pl, err := tb.saveSnapshot(tb.Ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a", "b"), false)
	require.NoError(t, err)

	tb.PlaylistInfoFunc = func(string) (ytdlp.PlaylistMeta, error) {
		return testPlaylistMeta(t, "PL1", "dupa 2", "b", "a", "c"), nil
	}
	updated, err := tb.syncPlaylist(tb.Ctx, pl)
	require.NoError(t, err)
	assert.Equal(t, "dupa 2", updated.Title)

	entries := tb.entries(t, pl.ID)
	require.Len(t, entries, 3)
	assert.Equal(t, "b", entries[0].YoutubeVideoID.String)
	assert.True(t, entries[0].YoutubeVideoID.Valid)
}

func TestPlaylistDiscoveryDoesNotResync(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx

	meta := testPlaylistMeta(t, "PL1", "dupa", "a", "b")
	pl, err := tb.saveSnapshot(ctx, testGID, meta, false)
	require.NoError(t, err)

	// queueing a known playlist must not touch youtube
	tb.PlaylistInfoFunc = func(string) (ytdlp.PlaylistMeta, error) {
		t.Fatal("known playlist must not be re-fetched")
		return ytdlp.PlaylistMeta{}, nil
	}
	require.NoError(t, tb.playPlaylistURL(ctx, testGID, "PL1"))
	assert.Len(t, tb.entries(t, pl.ID), 2)
}

func TestPlaylistDiscoveryRegistersNewPlaylist(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx

	tb.PlaylistInfoFunc = func(link string) (ytdlp.PlaylistMeta, error) {
		assert.Equal(t, canonicalPlaylistURL("PLNEW"), link)
		return testPlaylistMeta(t, "PLNEW", "dupa", "a", "b", "c"), nil
	}
	require.NoError(t, tb.playPlaylistURL(ctx, testGID, "PLNEW"))

	pl, err := tb.Store.GetPlaylistByYoutubeID(ctx, store.GetPlaylistByYoutubeIDParams{
		GuildID: testGID, YoutubePlaylistID: "PLNEW",
	})
	require.NoError(t, err)
	assert.Equal(t, "dupa", pl.Title)
	assert.Equal(t, canonicalPlaylistURL("PLNEW"), pl.YoutubeUrl)
	assert.Len(t, tb.entries(t, pl.ID), 3)

	// replaying the same url does not duplicate the record or refetch
	require.NoError(t, tb.playPlaylistURL(ctx, testGID, "PLNEW"))
	all, err := tb.Store.SearchPlaylists(ctx, store.SearchPlaylistsParams{GuildID: testGID, Title: ""})
	require.NoError(t, err)
	assert.Len(t, all, 1)
}

func TestSearchPlaylistsScopedAndFiltered(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx

	_, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "Rock and roll", "a"), false)
	require.NoError(t, err)
	_, err = tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL2", "rock metal", "b"), false)
	require.NoError(t, err)
	_, err = tb.saveSnapshot(ctx, "otherguild", testPlaylistMeta(t, "PL3", "rock", "c"), false)
	require.NoError(t, err)

	// guild scoped
	all, err := tb.Store.SearchPlaylists(ctx, store.SearchPlaylistsParams{GuildID: testGID, Title: ""})
	require.NoError(t, err)
	assert.Len(t, all, 2)

	// case insensitive partial, and never truncates before filtering
	filtered, err := tb.Store.SearchPlaylists(ctx, store.SearchPlaylistsParams{GuildID: testGID, Title: "ROCK"})
	require.NoError(t, err)
	assert.Len(t, filtered, 2)

	none, err := tb.Store.SearchPlaylists(ctx, store.SearchPlaylistsParams{GuildID: testGID, Title: "zzz"})
	require.NoError(t, err)
	assert.Empty(t, none)
}

func TestDuplicateTitlesAreDistinctPlaylists(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx

	a, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "same", "a"), false)
	require.NoError(t, err)
	b, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL2", "same", "b"), false)
	require.NoError(t, err)
	assert.NotEqual(t, a.ID, b.ID)

	got, err := tb.Store.GetPlaylist(ctx, store.GetPlaylistParams{ID: b.ID, GuildID: testGID})
	require.NoError(t, err)
	assert.Equal(t, "PL2", got.YoutubePlaylistID)
}

func TestGetPlaylistIsGuildScoped(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx

	pl, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a"), false)
	require.NoError(t, err)

	_, err = tb.Store.GetPlaylist(ctx, store.GetPlaylistParams{ID: pl.ID, GuildID: "otherguild"})
	assert.ErrorIs(t, err, sql.ErrNoRows)
}

func TestAppendEntriesOrderAndCache(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx

	pl, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a", "b", "c"), false)
	require.NoError(t, err)
	entries := tb.entries(t, pl.ID)

	// 'b' is already cached and must be queued as a local file
	tb.writeArtefactFile(t, canonicalVideoURL("b"), testGID+"/play_history/b.mp4")

	tracks, res, err := tb.planQueue(ctx, testGID, entries)
	require.NoError(t, err)
	assert.Equal(t, 3, res.queued)
	assert.Equal(t, 1, res.cached, "b plays from local media")
	assert.Equal(t, 2, res.waiting)
	assert.Equal(t, 0, res.unscheduled)
	assert.Equal(t, 0, res.unavailable)
	require.Len(t, tracks, 3)
	// the source url is always kept, the local file only when one exists
	assert.Equal(t, canonicalVideoURL("a"), tracks[0].URL)
	assert.Empty(t, tracks[0].Filepath)
	assert.Equal(t, canonicalVideoURL("b"), tracks[1].URL)
	assert.Equal(t, artefactFile(testGID+"/play_history/b.mp4"), tracks[1].Filepath, "cached video plays from disk")
	assert.Equal(t, canonicalVideoURL("c"), tracks[2].URL)
}

func TestAppendEntriesSkipsMissingFile(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx

	pl, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a"), false)
	require.NoError(t, err)

	// artefact row points at a file that no longer exists
	err = tb.Backup.InsertArtefact(ctx, backup.InsertArtefactParams{
		OriginUrl: canonicalVideoURL("a"), Path: testGID + "/play_history/gone.mp4", MediaType: "video/mp4",
	})
	require.NoError(t, err)

	tracks, res, err := tb.planQueue(ctx, testGID, tb.entries(t, pl.ID))
	require.NoError(t, err)
	assert.Len(t, tracks, 1, "the source url is queued instead")
	assert.Equal(t, canonicalVideoURL("a"), tracks[0].URL)
	assert.Empty(t, tracks[0].Filepath)
	assert.Equal(t, 1, res.waiting, "broken artefact must be repaired in the background")
	assert.Equal(t, 0, res.cached)
}

func TestAppendEntriesOneBadEntryDoesNotBlock(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx

	pl, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a", "b", "c"), false)
	require.NoError(t, err)

	entries := tb.entries(t, pl.ID)
	// simulate an entry with no usable source at all
	entries[0].YoutubeUrl = ""
	entries[0].YoutubeVideoID.String = ""

	tracks, res, err := tb.planQueue(ctx, testGID, entries)
	require.NoError(t, err)
	assert.Equal(t, 2, len(tracks))
	assert.Equal(t, 1, res.unavailable)
	assert.Equal(t, 2, res.waiting)
}

func TestCachedVideoLookup(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx

	_, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a"), false)
	require.NoError(t, err)

	t.Run("miss", func(t *testing.T) {
		_, found, err := tb.cachedVideo(ctx, "a", "")
		require.NoError(t, err)
		assert.False(t, found)
	})

	t.Run("usable artefact reused", func(t *testing.T) {
		tb.writeArtefactFile(t, canonicalVideoURL("a"), testGID+"/play_history/a.mp4")
		path, found, err := tb.cachedVideo(ctx, "a", "")
		require.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, testGID+"/play_history/a.mp4", path)
	})

	t.Run("legacy url candidate reused", func(t *testing.T) {
		// old artefacts were indexed under whatever url yt-dlp returned
		tb.writeArtefactFile(t, "https://youtu.be/legacy1", testGID+"/play_history/legacy.mp4")
		path, found, err := tb.cachedVideo(ctx, "legacy1", "https://youtu.be/legacy1")
		require.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, testGID+"/play_history/legacy.mp4", path)
	})

	t.Run("missing file is a miss", func(t *testing.T) {
		_, found, err := tb.cachedVideo(ctx, "zzz", "https://www.youtube.com/watch?v=zzz")
		require.NoError(t, err)
		assert.False(t, found)
	})
}

func TestBackupJobSchedulingByJobState(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx

	pl, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a", "b"), false)
	require.NoError(t, err)
	entries := tb.entries(t, pl.ID)

	_, res, err := tb.planQueue(ctx, testGID, entries)
	require.NoError(t, err)
	assert.Equal(t, 2, res.waiting)
	require.Equal(t, int64(2), tb.queueLen(t))

	t.Run("active job is not duplicated", func(t *testing.T) {
		// both jobs are still pending, so a second /playlist must not add more
		_, res, err := tb.planQueue(ctx, testGID, entries)
		require.NoError(t, err)
		assert.Equal(t, 2, res.waiting, "still covered by the active jobs")
		assert.Equal(t, int64(2), tb.queueLen(t))
	})

	t.Run("completed job allows a new attempt when the file is missing", func(t *testing.T) {
		markQueueStatus(t, tb, canonicalVideoURL("a"), "done")

		_, res, err := tb.planQueue(ctx, testGID, entries)
		require.NoError(t, err)
		assert.Equal(t, 0, res.cached, "no artefact file exists for a")
		assert.Equal(t, 2, res.waiting)
		assert.Equal(t, int64(3), tb.queueLen(t), "a finished job with no file must be re-queued")
	})

	t.Run("exhausted job allows a new attempt", func(t *testing.T) {
		markQueueStatus(t, tb, canonicalVideoURL("b"), "failing")
		bumpQueueFailCount(t, tb, canonicalVideoURL("b"), 3)

		_, _, err := tb.planQueue(ctx, testGID, entries)
		require.NoError(t, err)
		assert.Equal(t, int64(4), tb.queueLen(t), "a job that gave up must be retried")
	})

	t.Run("unavailable job is never retried", func(t *testing.T) {
		markQueueStatus(t, tb, canonicalVideoURL("b"), "unavailable")

		_, res, err := tb.planQueue(ctx, testGID, entries)
		require.NoError(t, err)
		assert.Equal(t, int64(4), tb.queueLen(t), "a dead video must not get another job")
		// it is now reported as unavailable rather than queued for playback
		assert.Equal(t, 1, res.unavailable)
		assert.Equal(t, 1, res.waiting)
		assert.Equal(t, res.queued, res.cached+res.waiting+res.unscheduled)
	})
}

// rawStore opens a second connection to a test bot's db so a test can write queue
// state the bot itself only writes through the job runner. It uses tb's own directory
// rather than the global config, which sibling test bots overwrite.
func rawStore(t *testing.T, tb *testBot) *sql.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", filepath.Join(tb.dir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { raw.Close() })
	return raw
}

// markQueueStatus rewrites the status of every backup job for a video, the way the
// job runner does.
func markQueueStatus(t *testing.T, tb *testBot, videoURL, status string) {
	t.Helper()
	res, err := rawStore(t, tb).ExecContext(tb.Ctx,
		`UPDATE queue SET status = ? WHERE job_type = ? AND meta LIKE ?`,
		status, BackupJob, `%"OriginUrl":"`+videoURL+`"%`)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.NotZero(t, n)
}

func bumpQueueFailCount(t *testing.T, tb *testBot, videoURL string, n int64) {
	t.Helper()
	res, err := rawStore(t, tb).ExecContext(tb.Ctx,
		`UPDATE queue SET fail_count = ? WHERE job_type = ? AND meta LIKE ?`,
		n, BackupJob, `%"OriginUrl":"`+videoURL+`"%`)
	require.NoError(t, err)
	affected, err := res.RowsAffected()
	require.NoError(t, err)
	require.NotZero(t, affected)
}

// Realistic yt-dlp stderr fragments. Only a confirmed cause is permanent. A generic
// "Video unavailable" counts as dead: youtube answers it for every video it will never
// serve, and retrying it only burns yt-dlp runs and log noise on each /playlist.
func TestIsUnavailableVideo(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{
			"tos strike",
			errors.New("backupFile: 'u': yt-dlp: failed to download\nERROR: [youtube] x: This video has been removed for violating YouTube's Terms of Service\n"),
			true,
		},
		{
			"removed by uploader",
			errors.New("ERROR: [youtube] x: This video has been removed by the uploader"),
			true,
		},
		{
			"terminated account",
			errors.New("ERROR: [youtube] x: This video is no longer available because the account associated with this video has been terminated."),
			true,
		},
		{"generic unavailable", errors.New("ERROR: [youtube] x: Video unavailable"), true},
		// everything below must stay retryable
		{
			"private",
			errors.New("ERROR: [youtube] x: Private video. Sign in if you've been granted access"),
			false,
		},
		{
			"region block",
			errors.New("ERROR: [youtube] x: The uploader has not made this video available in your country"),
			false,
		},
		{
			"sign in required",
			errors.New("ERROR: [youtube] x: Sign in to confirm you’re not a bot"),
			false,
		},
		{
			"age confirmation",
			errors.New("ERROR: [youtube] x: Sign in to confirm your age. This video may be inappropriate for some users."),
			false,
		},
		{
			"transient network",
			errors.New("ERROR: unable to download webpage: <urlopen error timed out>"),
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isUnavailableVideo(tt.err))
		})
	}
}

func TestBackupJobIsIdempotent(t *testing.T) {
	tb := newTestBot(t)

	tb.writeArtefactFile(t, canonicalVideoURL("a"), testGID+"/play_history/a.mp4")

	meta := BackupFileParams{OriginUrl: canonicalVideoURL("a"), GID: testGID, Dirs: videoBackupDir}
	raw, err := jsonMarshal(meta)
	require.NoError(t, err)

	// a duplicate job must succeed without re-downloading
	require.NoError(t, tb.backupVideoJob(raw))
}

func TestBackupJobStillDownloadsWhenMissing(t *testing.T) {
	tb := newTestBot(t)

	raw, err := jsonMarshal(BackupFileParams{OriginUrl: canonicalVideoURL("zzz"), GID: testGID, Dirs: videoBackupDir})
	require.NoError(t, err)

	// no yt-dlp download possible in tests: the job must still fail loudly
	require.Error(t, tb.backupVideoJob(raw))
}

func TestArtefactRepairDoesNotConflict(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx

	tb.writeArtefactFile(t, canonicalVideoURL("a"), testGID+"/play_history/a.mp4")

	// plain insert conflicts on the primary key
	err := tb.Backup.InsertArtefact(ctx, backup.InsertArtefactParams{
		OriginUrl: canonicalVideoURL("a"), Path: "other.mp4", MediaType: "video/mp4",
	})
	require.Error(t, err)

	require.NoError(t, tb.Backup.RepairArtefact(ctx, backup.RepairArtefactParams{
		OriginUrl: canonicalVideoURL("a"),
		Path:      testGID + "/play_history/repaired.mp4",
		MediaType: "video/mp4",
	}))

	art, err := tb.Backup.GetArtefact(ctx, canonicalVideoURL("a"))
	require.NoError(t, err)
	assert.Equal(t, testGID+"/play_history/repaired.mp4", art.Path)
}

func TestBackupFilePathResolvesToRealFile(t *testing.T) {
	tb := newTestBot(t)

	info, err := tb.backupFile(BackupFileParams{
		Name: "clip.mp4",
		GID:  testGID,
		Dirs: videoBackupDir,
		File: strings.NewReader("media bytes"),
	})
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(config.BACKUP_DIR, info.Name))
	assert.Equal(t, filepath.Join(testGID, videoBackupDir, "clip.mp4"), info.Name)
}

func TestSearchPlaylistsExcludesSoftDeleted(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx

	_, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a"), false)
	require.NoError(t, err)

	// reactivating a soft deleted row must reuse it, not duplicate it
	_, err = rawStore(t, tb).ExecContext(ctx,
		"UPDATE playlists SET deleted_at = '2020-01-01T00:00:00Z' WHERE youtube_playlist_id = 'PL1'")
	require.NoError(t, err)

	none, err := tb.Store.SearchPlaylists(ctx, store.SearchPlaylistsParams{GuildID: testGID, Title: ""})
	require.NoError(t, err)
	assert.Empty(t, none)

	_, err = tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa again", "a"), false)
	require.NoError(t, err)

	all, err := tb.Store.SearchPlaylists(ctx, store.SearchPlaylistsParams{GuildID: testGID, Title: ""})
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, "dupa again", all[0].Title)
}

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}

func TestSnapshotRejectsUnresolvedEntries(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx

	old, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a", "b", "c"), false)
	require.NoError(t, err)
	require.Len(t, tb.entries(t, old.ID), 3)

	// a fetch that lost track of one member must not delete it
	tb.PlaylistInfoFunc = func(string) (ytdlp.PlaylistMeta, error) {
		meta := testPlaylistMeta(t, "PL1", "dupa", "a", "b")
		meta.Entries = append(meta.Entries, struct {
			ID       string `json:"id"`
			URL      string `json:"url"`
			Title    string `json:"title"`
			Duration *int   `json:"duration"`
		}{Title: "???"})
		return meta, nil
	}

	_, err = tb.syncPlaylist(ctx, old)
	require.ErrorIs(t, err, ErrPlaylistIncomplete)
	assert.Len(t, tb.entries(t, old.ID), 3, "the unresolved entry must not be read as a removal")
}

func TestPlaylistQueuedMsgReportsEveryOutcome(t *testing.T) {
	msg := playlistQueuedMsg(queueResult{
		queued: 5, cached: 2, waiting: 2, unscheduled: 1, unavailable: 1,
	})
	assert.Equal(t, "queued 5 entries, 2 from cache, 2 waiting for cache, 1 could not be cached, 1 unavailable", msg)

	// nothing queued is still worth explaining
	assert.Equal(t, "queued 0 entries, 3 unavailable", playlistQueuedMsg(queueResult{unavailable: 3}))
}

// A permanently failed job keeps its fail_count, so the runner query has to exclude
// the terminal 'unavailable' status explicitly - otherwise the same row is picked up
// again immediately and the runner loops forever.
func TestQueueSkipsUnavailableJobs(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx

	enqueue := func(url string) int64 {
		res, err := tb.Store.Enqueue(ctx, store.EnqueueParams{Meta: `{"OriginUrl":"` + url + `"}`, JobType: BackupJob})
		require.NoError(t, err)
		return res.ID
	}
	id := enqueue("https://www.youtube.com/watch?v=dead")

	next, err := tb.Store.NextInQueue(ctx)
	require.NoError(t, err)
	assert.Equal(t, id, next.ID, "a pending job must be picked up")

	err = tb.Store.UpdateQueueEntry(ctx, store.UpdateQueueEntryParams{
		ID: id, Status: "unavailable", FailCount: 0,
	})
	require.NoError(t, err)

	_, err = tb.Store.NextInQueue(ctx)
	assert.ErrorIs(t, err, sql.ErrNoRows, "an unavailable job must not stay in the run queue")

	failed, err := tb.Store.FindFailedTasksInQueue(ctx)
	require.NoError(t, err)
	assert.Empty(t, failed)

	// a done job is skipped too, and only a live one comes back
	doneID := enqueue("https://www.youtube.com/watch?v=done")
	err = tb.Store.UpdateQueueEntry(ctx, store.UpdateQueueEntryParams{
		ID: doneID, Status: "done", FailCount: 0,
	})
	require.NoError(t, err)
	_, err = tb.Store.NextInQueue(ctx)
	assert.ErrorIs(t, err, sql.ErrNoRows)

	liveID := enqueue("https://www.youtube.com/watch?v=live")
	next, err = tb.Store.NextInQueue(ctx)
	require.NoError(t, err)
	assert.Equal(t, liveID, next.ID)
}

// A video youtube refuses to serve stays in the snapshot, but it must not be queued
// for playback - doing so only produces a yt-dlp/ffmpeg failure later.
func TestPlanQueueSkipsKnownUnavailableVideos(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx

	pl, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a", "b"), false)
	require.NoError(t, err)
	entries := tb.entries(t, pl.ID)

	// 'a' is already known to be unavailable, 'b' is not
	ins, err := rawStore(t, tb).ExecContext(ctx,
		`INSERT INTO queue (job_type, meta, status, fail_count) VALUES (?, ?, 'unavailable', 0)`,
		BackupJob, `{"Name":"","Dirs":"play_history","GID":"`+testGID+`","CHID":"","MSGID":"","ContentType":"","OriginUrl":"`+canonicalVideoURL("a")+`","PrependTime":false,"File":null}`)
	require.NoError(t, err)
	require.NotZero(t, ins)
	before := tb.queueLen(t)

	tracks, qr, err := tb.planQueue(ctx, testGID, entries)
	require.NoError(t, err)
	require.Len(t, tracks, 1, "only the playable entry may be queued")
	assert.Equal(t, canonicalVideoURL("b"), tracks[0].URL)
	assert.Equal(t, 1, qr.unavailable)
	assert.Equal(t, 1, qr.waiting)
	assert.Equal(t, before+1, tb.queueLen(t), "only 'b' may get a job; the dead video must not")

	// the entry itself is untouched - membership is not downloadability
	assert.Len(t, tb.entries(t, pl.ID), 2)
}

// --- backup state detection -------------------------------------------------

func TestScheduleBackupStates(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx
	url := canonicalVideoURL("a")

	t.Run("no history schedules", func(t *testing.T) {
		state, err := tb.scheduleBackup(ctx, testGID, videoBackupDir, url)
		require.NoError(t, err)
		assert.Equal(t, backupScheduled, state)
	})

	t.Run("pending job is not duplicated", func(t *testing.T) {
		state, err := tb.scheduleBackup(ctx, testGID, videoBackupDir, url)
		require.NoError(t, err)
		assert.Equal(t, backupPending, state)
		assert.Equal(t, int64(1), tb.queueLen(t))
	})

	t.Run("retryable failure stays pending", func(t *testing.T) {
		markQueueStatus(t, tb, url, "failing")
		bumpQueueFailCount(t, tb, url, 2)
		state, err := tb.scheduleBackup(ctx, testGID, videoBackupDir, url)
		require.NoError(t, err)
		assert.Equal(t, backupPending, state)
	})

	t.Run("done job allows a new attempt", func(t *testing.T) {
		markQueueStatus(t, tb, url, "done")
		before := tb.queueLen(t)
		state, err := tb.scheduleBackup(ctx, testGID, videoBackupDir, url)
		require.NoError(t, err)
		assert.Equal(t, backupScheduled, state, "a missing file behind a done job must be repairable")
		assert.Equal(t, before+1, tb.queueLen(t))
	})

	t.Run("exhausted job allows a new attempt", func(t *testing.T) {
		markQueue(t, tb, url, "failing", jobAttempts)
		before := tb.queueLen(t)
		state, err := tb.scheduleBackup(ctx, testGID, videoBackupDir, url)
		require.NoError(t, err)
		assert.Equal(t, backupScheduled, state)
		assert.Equal(t, before+1, tb.queueLen(t))
	})

	t.Run("unavailable is terminal", func(t *testing.T) {
		markQueue(t, tb, url, "unavailable", 0)
		before := tb.queueLen(t)
		state, err := tb.scheduleBackup(ctx, testGID, videoBackupDir, url)
		require.NoError(t, err)
		assert.Equal(t, backupUnavailable, state)
		assert.Equal(t, before, tb.queueLen(t), "no new job for a dead video")
	})

	t.Run("an older unavailable row does not outvote a newer one", func(t *testing.T) {
		markQueue(t, tb, url, "unavailable", 0)
		// a later attempt was made and is still running: that is the current state
		markQueue(t, tb, url, "", 0)
		state, err := tb.scheduleBackup(ctx, testGID, videoBackupDir, url)
		require.NoError(t, err)
		assert.Equal(t, backupPending, state, "only the latest job decides")
	})

	t.Run("a newer done row beats an older unavailable one", func(t *testing.T) {
		markQueue(t, tb, url, "unavailable", 0)
		markQueue(t, tb, url, "done", 0)
		before := tb.queueLen(t)
		state, err := tb.scheduleBackup(ctx, testGID, videoBackupDir, url)
		require.NoError(t, err)
		assert.Equal(t, backupScheduled, state)
		assert.Equal(t, before+1, tb.queueLen(t))
	})
}

func TestScheduleBackupReportsDbErrors(t *testing.T) {
	tb := newTestBot(t)

	// close the connection so the lookup fails for real
	require.NoError(t, tb.store.Close())

	state, err := tb.scheduleBackup(tb.Ctx, testGID, videoBackupDir, canonicalVideoURL("a"))
	assert.Equal(t, backupError, state, "a db error must never look like a cache hit or a fresh schedule")
	require.Error(t, err)
}

// markQueue inserts a job row for a video with an explicit outcome.
func markQueue(t *testing.T, tb *testBot, videoURL, status string, failCount int64) {
	t.Helper()
	meta, err := backupJobMeta(testGID, videoBackupDir, videoURL)
	require.NoError(t, err)
	_, err = rawStore(t, tb).ExecContext(tb.Ctx,
		`INSERT INTO queue (job_type, meta, status, fail_count) VALUES (?, ?, ?, ?)`,
		BackupJob, meta, status, failCount)
	require.NoError(t, err)
}

// --- playback resolution ---------------------------------------------------

// A terminal 'unavailable' job must not stop a usable local copy from playing.
func TestPlanQueuePlaysCachedFileOfUnavailableVideo(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx

	pl, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a"), false)
	require.NoError(t, err)
	markQueue(t, tb, canonicalVideoURL("a"), "unavailable", 0)
	tb.writeArtefactFile(t, canonicalVideoURL("a"), testGID+"/play_history/a.mp4")

	tracks, res, err := tb.planQueue(ctx, testGID, tb.entries(t, pl.ID))
	require.NoError(t, err)
	require.Len(t, tracks, 1)
	assert.Equal(t, artefactFile(testGID+"/play_history/a.mp4"), tracks[0].Filepath, "the local file wins over job history")
	assert.Equal(t, canonicalVideoURL("a"), tracks[0].URL, "and the source url is still kept")
	assert.Equal(t, 1, res.cached)
	assert.Equal(t, 0, res.unavailable)
}

func TestPlanQueueSkipsUnavailableAndKeepsTheRest(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx

	pl, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a", "b", "c"), false)
	require.NoError(t, err)
	markQueue(t, tb, canonicalVideoURL("b"), "unavailable", 0)

	tracks, res, err := tb.planQueue(ctx, testGID, tb.entries(t, pl.ID))
	require.NoError(t, err)
	require.Len(t, tracks, 2, "one dead entry must not stop the others")
	assert.NotContains(t, trackURLs(tracks), canonicalVideoURL("b"))
	assert.Equal(t, 1, res.unavailable)
	assert.Equal(t, 2, res.waiting)
	assert.Equal(t, 2, res.queued, "queued counts what was appended, not what was inspected")

	// membership is untouched by playback decisions
	assert.Len(t, tb.entries(t, pl.ID), 3)
}

func TestPlanQueueNeverQueuesMissingPaths(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx

	pl, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a"), false)
	require.NoError(t, err)
	require.NoError(t, tb.Backup.InsertArtefact(ctx, backup.InsertArtefactParams{
		OriginUrl: canonicalVideoURL("a"), Path: testGID + "/play_history/gone.mp4", MediaType: "video/mp4",
	}))

	tracks, _, err := tb.planQueue(ctx, testGID, tb.entries(t, pl.ID))
	require.NoError(t, err)
	require.Len(t, tracks, 1)
	assert.Equal(t, canonicalVideoURL("a"), tracks[0].URL, "a nonexistent path is never queued")
	assert.Empty(t, tracks[0].Filepath, "and no filepath is invented for it")
}

// trackURLs is what planQueue hands to the player, reduced to the source urls.
func trackURLs(tracks []player.Track) []string {
	out := make([]string, 0, len(tracks))
	for _, t := range tracks {
		out = append(out, t.URL)
	}
	return out
}

// --- snapshot validation ---------------------------------------------------

func TestSnapshotValidation(t *testing.T) {
	t.Run("unavailable video with a valid id stays", func(t *testing.T) {
		tb := newTestBot(t)
		meta := testPlaylistMeta(t, "PL1", "dupa", "a")
		meta.Entries[0].Duration = nil
		pl, err := tb.saveSnapshot(tb.Ctx, testGID, meta, false)
		require.NoError(t, err)
		require.Len(t, tb.entries(t, pl.ID), 1)
		assert.Equal(t, "a", tb.entries(t, pl.ID)[0].YoutubeVideoID.String)
	})

	t.Run("positions follow yt-dlp order", func(t *testing.T) {
		tb := newTestBot(t)
		pl, err := tb.saveSnapshot(tb.Ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "c", "a", "b", "a"), false)
		require.NoError(t, err)
		got := []string{}
		for _, e := range tb.entries(t, pl.ID) {
			got = append(got, e.YoutubeVideoID.String)
		}
		assert.Equal(t, []string{"c", "a", "b", "a"}, got)
	})

	t.Run("a failed fetch changes nothing", func(t *testing.T) {
		tb := newTestBot(t)
		ctx := tb.Ctx
		pl, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a", "b"), false)
		require.NoError(t, err)

		tb.PlaylistInfoFunc = func(string) (ytdlp.PlaylistMeta, error) {
			return ytdlp.PlaylistMeta{}, errors.New("network down")
		}
		_, err = tb.syncPlaylist(ctx, pl)
		require.Error(t, err)

		again, err := tb.Store.GetPlaylist(ctx, store.GetPlaylistParams{ID: pl.ID, GuildID: testGID})
		require.NoError(t, err)
		assert.Equal(t, "dupa", again.Title)
		assert.False(t, again.LastSyncedAt.Valid, "last_synced_at must not move on a failed sync")
		assert.Len(t, tb.entries(t, pl.ID), 2)
	})

	t.Run("syncing does not delete backup artefacts of removed entries", func(t *testing.T) {
		tb := newTestBot(t)
		ctx := tb.Ctx
		pl, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a", "b"), false)
		require.NoError(t, err)
		tb.writeArtefactFile(t, canonicalVideoURL("b"), testGID+"/play_history/b.mp4")

		tb.PlaylistInfoFunc = func(string) (ytdlp.PlaylistMeta, error) {
			return testPlaylistMeta(t, "PL1", "dupa", "a"), nil
		}
		_, err = tb.syncPlaylist(ctx, pl)
		require.NoError(t, err)
		assert.Len(t, tb.entries(t, pl.ID), 1)

		_, err = tb.Backup.GetArtefact(ctx, canonicalVideoURL("b"))
		require.NoError(t, err, "removing a video from a playlist must not delete its backup")
	})
}

// --- deferred interactions -------------------------------------------------

func TestIsPlaylistRequest(t *testing.T) {
	assert.True(t, isPlaylistRequest("https://www.youtube.com/playlist?list=PL1"))
	// known or new, the url shape is what decides
	assert.True(t, isPlaylistRequest("https://m.youtube.com/playlist?list=PL2"))
	// single video work is not deferred
	assert.False(t, isPlaylistRequest("https://www.youtube.com/watch?v=abc"))
	assert.False(t, isPlaylistRequest("https://youtu.be/abc"))
	assert.False(t, isPlaylistRequest("not a url"))
	assert.False(t, isPlaylistRequest("https://example.com/x"))
}

func TestTruncateKeepsRunesIntact(t *testing.T) {
	assert.Equal(t, "short", truncate("short"))

	long := strings.Repeat("ą", 3000)
	got := truncate(long)
	assert.LessOrEqual(t, len(got), maxMessageLen)
	assert.True(t, utf8.ValidString(got), "must not split a utf-8 character")

	// cuts exactly at the boundary, not mid-rune
	assert.Equal(t, strings.Repeat("ą", 0)+got, got)
	assert.False(t, strings.HasSuffix(got, "\ufffd"))
}

// Three attempts total, first run included - and rows written under the old
// five-attempt policy must still be recognised as exhausted.
func TestRetryBudget(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx

	row, err := tb.Store.Enqueue(ctx, store.EnqueueParams{
		Meta: `{"OriginUrl":"https://www.youtube.com/watch?v=a"}`, JobType: BackupJob,
	})
	require.NoError(t, err)
	entry := store.Queue{ID: row.ID}

	for want := int64(1); want <= jobAttempts; want++ {
		got := jobResultForTest(entry, errors.New("boom"))
		entry.FailCount = got.FailCount
		assert.Equal(t, want, entry.FailCount, "failure %d", want)
		require.NoError(t, tb.Store.UpdateQueueEntry(ctx, store.UpdateQueueEntryParams{
			ID: entry.ID, Status: got.Status, FailCount: got.FailCount,
		}))

		next, err := tb.Store.NextInQueue(ctx)
		if want < jobAttempts {
			require.NoError(t, err, "still retryable after %d failures", want)
			assert.Equal(t, entry.ID, next.ID)
			continue
		}
		// the third attempt exhausts the budget
		assert.ErrorIs(t, err, sql.ErrNoRows, "fail_count == 3 must not be selected again")
	}

	t.Run("legacy counts above the new limit stay exhausted", func(t *testing.T) {
		other := newTestBot(t)
		for _, n := range []int64{3, 4, 5} {
			markQueue(t, other, canonicalVideoURL(fmt.Sprintf("v%d", n)), "failing", n)
		}
		markQueue(t, other, canonicalVideoURL("done"), "done", 5)
		markQueue(t, other, canonicalVideoURL("gone"), "unavailable", 0)

		failed, err := other.Store.FindFailedTasksInQueue(ctx)
		require.NoError(t, err)
		require.Len(t, failed, 3, "fail_count 3, 4 and 5 must all be reported")
		got := map[int64]bool{}
		for _, f := range failed {
			got[f.FailCount] = true
		}
		assert.True(t, got[3] && got[4] && got[5])
	})

	t.Run("a fresh job can be created after an exhausted one", func(t *testing.T) {
		before := tb.queueLen(t)
		state, err := tb.scheduleBackup(ctx, testGID, videoBackupDir, canonicalVideoURL("a"))
		require.NoError(t, err)
		assert.Equal(t, backupScheduled, state)
		assert.Equal(t, before+1, tb.queueLen(t))

		// the exhausted row is left exactly as it was, untouched
		var (
			status    string
			failCount int64
		)
		require.NoError(t, rawStore(t, tb).QueryRowContext(ctx,
			`SELECT status, fail_count FROM queue WHERE id = ?`, entry.ID).Scan(&status, &failCount))
		assert.Equal(t, "failing", status)
		assert.Equal(t, int64(jobAttempts), failCount)
	})
}

func jobResultForTest(entry store.Queue, err error) store.UpdateQueueEntryParams {
	return jobrunner.JobResultForTest(entry, err)
}
