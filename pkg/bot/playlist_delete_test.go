package dbot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"dbot/pkg/backup"
	"dbot/pkg/config"
	"dbot/pkg/store"
	"dbot/pkg/ytdlp"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/rest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scanDeletedAt reads the soft-delete marker straight from the db.
func scanDeletedAt(t *testing.T, ctx context.Context, tb *testBot, id int64, out *sql.NullString) error {
	t.Helper()
	return rawStore(t, tb).QueryRowContext(ctx, `SELECT deleted_at FROM playlists WHERE id = ?`, id).Scan(out)
}

func TestDeletePlaylist(t *testing.T) {
	t.Run("soft deletes and drops entries", func(t *testing.T) {
		tb := newTestBot(t)
		ctx := tb.Ctx

		pl, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a", "b"), false)
		require.NoError(t, err)
		require.Len(t, tb.entries(t, pl.ID), 2)

		title, err := tb.deletePlaylist(ctx, pl.ID, testGID)
		require.NoError(t, err)
		assert.Equal(t, "dupa", title)

		// the row survives, marked deleted
		var deletedAt sql.NullString
		require.NoError(t, scanDeletedAt(t, ctx, tb, pl.ID, &deletedAt))
		assert.True(t, deletedAt.Valid, "deleted_at must be set")
		assert.NotEmpty(t, deletedAt.String)

		// entries are gone
		assert.Empty(t, tb.entries(t, pl.ID))

		// and it is invisible everywhere
		_, err = tb.Store.GetPlaylist(ctx, store.GetPlaylistParams{ID: pl.ID, GuildID: testGID})
		assert.ErrorIs(t, err, sql.ErrNoRows)
		_, err = tb.Store.GetPlaylistByYoutubeID(ctx, store.GetPlaylistByYoutubeIDParams{
			GuildID: testGID, YoutubePlaylistID: "PL1",
		})
		assert.ErrorIs(t, err, sql.ErrNoRows)
		found, err := tb.Store.SearchPlaylists(ctx, store.SearchPlaylistsParams{GuildID: testGID, Title: ""})
		require.NoError(t, err)
		assert.Empty(t, found, "deleted playlists must not appear in autocomplete")
	})

	t.Run("another guild cannot delete it", func(t *testing.T) {
		tb := newTestBot(t)
		ctx := tb.Ctx
		pl, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a"), false)
		require.NoError(t, err)

		_, err = tb.deletePlaylist(ctx, pl.ID, "someotherguild")
		assert.ErrorIs(t, err, sql.ErrNoRows)
		assert.Len(t, tb.entries(t, pl.ID), 1, "nothing may be deleted")
	})

	t.Run("already deleted cannot be deleted twice", func(t *testing.T) {
		tb := newTestBot(t)
		ctx := tb.Ctx
		pl, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a"), false)
		require.NoError(t, err)
		_, err = tb.deletePlaylist(ctx, pl.ID, testGID)
		require.NoError(t, err)

		_, err = tb.deletePlaylist(ctx, pl.ID, testGID)
		assert.ErrorIs(t, err, sql.ErrNoRows)
	})

	t.Run("keeps artefacts, files and queue rows", func(t *testing.T) {
		tb := newTestBot(t)
		ctx := tb.Ctx
		pl, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a", "b"), false)
		require.NoError(t, err)

		// a cached video, an artefact row and a queued backup job
		tb.writeArtefactFile(t, canonicalVideoURL("a"), testGID+"/play_history/a.mp4")
		_, err = tb.Store.Enqueue(ctx, store.EnqueueParams{
			Meta: `{"OriginUrl":"` + canonicalVideoURL("b") + `"}`, JobType: BackupJob,
		})
		require.NoError(t, err)

		_, err = tb.deletePlaylist(ctx, pl.ID, testGID)
		require.NoError(t, err)

		art, err := tb.Backup.GetArtefact(ctx, canonicalVideoURL("a"))
		require.NoError(t, err, "artefact records must survive")
		assert.FileExists(t, filepath.Join(config.BACKUP_DIR, art.Path), "cached media must survive")
		assert.Equal(t, int64(1), tb.queueLen(t), "backup jobs must survive")
	})
}

func TestRediscoveryOfDeletedPlaylist(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx

	old, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "old title", "a", "b"), false)
	require.NoError(t, err)
	tb.writeArtefactFile(t, canonicalVideoURL("b"), testGID+"/play_history/b.mp4")
	_, err = tb.deletePlaylist(ctx, old.ID, testGID)
	require.NoError(t, err)

	t.Run("rediscovering reuses the row with a fresh snapshot", func(t *testing.T) {
		tb.PlaylistInfoFunc = func(string) (ytdlp.PlaylistMeta, error) {
			return testPlaylistMeta(t, "PL1", "new title", "b", "c"), nil
		}
		require.NoError(t, tb.playPlaylistURL(ctx, testGID, "PL1"))

		all, err := tb.Store.SearchPlaylists(ctx, store.SearchPlaylistsParams{GuildID: testGID, Title: ""})
		require.NoError(t, err)
		require.Len(t, all, 1, "no duplicate row")
		assert.Equal(t, old.ID, all[0].ID, "the existing row must be reactivated")
		assert.Equal(t, "new title", all[0].Title)

		got := []string{}
		for _, e := range tb.entries(t, old.ID) {
			got = append(got, e.YoutubeVideoID.String)
		}
		assert.Equal(t, []string{"b", "c"}, got, "a stale snapshot must not come back")
	})

	t.Run("a failed rediscovery leaves the playlist deleted", func(t *testing.T) {
		other := newTestBot(t)
		ctx := other.Ctx
		pl, err := other.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "old", "a"), false)
		require.NoError(t, err)
		_, err = other.deletePlaylist(ctx, pl.ID, testGID)
		require.NoError(t, err)

		other.PlaylistInfoFunc = func(string) (ytdlp.PlaylistMeta, error) {
			return ytdlp.PlaylistMeta{}, errors.New("network down")
		}
		require.Error(t, other.playPlaylistURL(ctx, testGID, "PL1"))

		var deletedAt sql.NullString
		require.NoError(t, scanDeletedAt(t, ctx, other, pl.ID, &deletedAt))
		assert.True(t, deletedAt.Valid, "a failed fetch must not reactivate the playlist")

		// and an empty fetch is rejected rather than persisted
		other.PlaylistInfoFunc = func(string) (ytdlp.PlaylistMeta, error) {
			return testPlaylistMeta(t, "PL1", "old"), nil
		}
		require.Error(t, other.playPlaylistURL(ctx, testGID, "PL1"))
		require.NoError(t, scanDeletedAt(t, ctx, other, pl.ID, &deletedAt))
		assert.True(t, deletedAt.Valid, "an empty snapshot must not reactivate the playlist")
	})
}

// --- authorization ---------------------------------------------------------

func newDeleteEvent(t *testing.T, guild string, perms discord.Permissions, playlistID int64) (*events.ApplicationCommandInteractionCreate, *[]discord.InteractionResponseType) {
	t.Helper()
	guildField := ""
	if guild != "" {
		guildField = `"guild_id":"` + guild + `",`
	}
	raw := `{
		"id":"1","application_id":"2","type":2,"token":"tok","version":1,
		` + guildField + `"channel_id":"3",
		"member":{"user":{"id":"4","username":"u"},"permissions":"` + itoa(int64(perms)) + `"},
		"data":{"id":"5","type":1,"name":"playlist-delete",
			"options":[{"type":3,"name":"name","value":"` + itoa(playlistID) + `"}]}}`

	var aci discord.ApplicationCommandInteraction
	require.NoError(t, json.Unmarshal([]byte(raw), &aci))

	var types []discord.InteractionResponseType
	e := &events.ApplicationCommandInteractionCreate{ApplicationCommandInteraction: aci}
	e.Respond = func(t discord.InteractionResponseType, _ discord.InteractionResponseData, _ ...rest.RequestOpt) error {
		types = append(types, t)
		return nil
	}
	return e, &types
}

func TestHandlePlaylistDeleteAuthorization(t *testing.T) {
	t.Run("administrator can delete", func(t *testing.T) {
		tb := newTestBot(t)
		ctx := tb.Ctx
		pl, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a"), false)
		require.NoError(t, err)

		e, types := newDeleteEvent(t, testGID, discord.PermissionAdministrator, pl.ID)
		// answerDeferred needs a rest client; only the authorization path is asserted
		runHandler(t, func() error { return tb.handlePlaylistDelete(ctx, e) })

		require.NotEmpty(t, *types)
		assert.Equal(t, discord.InteractionResponseTypeDeferredCreateMessage, (*types)[0])
		assert.True(t, isGuildAdmin(e))
		_ = err
	})

	t.Run("non administrator is denied", func(t *testing.T) {
		tb := newTestBot(t)
		ctx := tb.Ctx
		pl, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a"), false)
		require.NoError(t, err)

		e, _ := newDeleteEvent(t, testGID, discord.PermissionSendMessages, pl.ID)
		assert.False(t, isGuildAdmin(e), "the runtime check must refuse a non-admin")

		// the handler dereferences a nil rest client only *after* deciding, so assert
		// the guard directly and confirm the playlist is untouched
		runHandler(t, func() error { return tb.handlePlaylistDelete(ctx, e) })
		assert.Len(t, tb.entries(t, pl.ID), 1, "a non-admin must not delete anything")
		_, err = tb.Store.GetPlaylist(ctx, store.GetPlaylistParams{ID: pl.ID, GuildID: testGID})
		require.NoError(t, err, "the playlist must still be active")
	})

	t.Run("dm is denied", func(t *testing.T) {
		e, _ := newDeleteEvent(t, "", discord.PermissionAdministrator, 1)
		assert.False(t, isGuildAdmin(e), "there is no guild to administer outside one")
	})
}

// TestPlaylistDeleteLeavesBackupDBAlone guards the rule structurally: the deletion
// path never references the backup db or the filesystem.
func TestPlaylistDeleteTouchesOnlyTheStore(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx
	pl, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a"), false)
	require.NoError(t, err)

	artefactPath := filepath.Join(config.BACKUP_DIR, testGID+"/play_history/keep.mp4")
	require.NoError(t, os.MkdirAll(filepath.Dir(artefactPath), 0o755))
	require.NoError(t, os.WriteFile(artefactPath, []byte("media"), 0o644))
	require.NoError(t, tb.Backup.InsertArtefact(ctx, backup.InsertArtefactParams{
		OriginUrl: canonicalVideoURL("a"), Path: testGID + "/play_history/keep.mp4", MediaType: "video/mp4",
	}))

	_, err = tb.deletePlaylist(ctx, pl.ID, testGID)
	require.NoError(t, err)

	assert.FileExists(t, artefactPath)
	_, err = tb.Backup.GetArtefact(ctx, canonicalVideoURL("a"))
	require.NoError(t, err)
}
