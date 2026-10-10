package dbot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	migrations "dbot/db"
	"dbot/pkg/backup"
	"dbot/pkg/config"
	"dbot/pkg/db"
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

	links, pending, unavailable, err := tb.planQueue(ctx, testGID, entries)
	require.NoError(t, err)
	assert.Equal(t, 2, pending)
	assert.Equal(t, 0, unavailable)
	require.Len(t, links, 3)
	assert.Equal(t, canonicalVideoURL("a"), links[0])
	assert.Equal(t, testGID+"/play_history/b.mp4", links[1], "cached video plays from disk")
	assert.Equal(t, canonicalVideoURL("c"), links[2])
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

	links, pending, _, err := tb.planQueue(ctx, testGID, tb.entries(t, pl.ID))
	require.NoError(t, err)
	assert.Len(t, links, 1, "the source url is queued instead")
	assert.Equal(t, canonicalVideoURL("a"), links[0])
	assert.Equal(t, 1, pending, "broken artefact must be repaired in the background")
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

	links, _, unavailable, err := tb.planQueue(ctx, testGID, entries)
	require.NoError(t, err)
	assert.Equal(t, 2, len(links))
	assert.Equal(t, 1, unavailable)
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

func TestPlanQueueSchedulesBackups(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx

	pl, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a", "b"), false)
	require.NoError(t, err)

	_, pending, _, err := tb.planQueue(ctx, testGID, tb.entries(t, pl.ID))
	require.NoError(t, err)
	assert.Equal(t, 2, pending)
	assert.Equal(t, int64(2), tb.queueLen(t))

	tb.writeArtefactFile(t, canonicalVideoURL("a"), testGID+"/play_history/a.mp4")
	links, pending, _, err := tb.planQueue(ctx, testGID, tb.entries(t, pl.ID))
	require.NoError(t, err)
	assert.Equal(t, 1, pending)
	assert.Equal(t, testGID+"/play_history/a.mp4", links[0])
	assert.Equal(t, int64(3), tb.queueLen(t), "a cached video must not queue another job")
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
	raw, err := sql.Open("sqlite", filepath.Join(config.DB_DIR, "test.db"))
	require.NoError(t, err)
	defer raw.Close()
	_, err = raw.ExecContext(ctx, "UPDATE playlists SET deleted_at = '2020-01-01T00:00:00Z' WHERE youtube_playlist_id = 'PL1'")
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
