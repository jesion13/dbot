package dbot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"dbot/pkg/player"
	"dbot/pkg/store"
	"dbot/pkg/ytdlp"
)

var (
	ErrPlaylistEmpty      = errors.New("playlist has no entries")
	ErrPlaylistIncomplete = errors.New("playlist metadata is incomplete")
	ErrPlaylistIDMismatch = errors.New("youtube returned a different playlist id")
	ErrPlaylistNotFound   = errors.New("playlist not found")
	ErrNoPlayableEntries  = errors.New("no entries could be played")
)

func orUnknown(title, videoID string) string {
	if title == "" {
		return videoID
	}
	return title
}

// snapshotEntry is one validated playlist membership row.
type snapshotEntry struct {
	VideoID  string
	URL      string
	Title    string
	Position int
}

// An entry with a nil Duration is kept: that usually means the video is unavailable,
// not that it left the playlist. One we cannot resolve to a video id/url at all makes
// the whole result unusable - dropping it would silently delete a member of the
// previous snapshot.
//
// KNOWN LIMITATION: `yt-dlp --flat-playlist` exits 0 and gives no entry count to
// cross-check, so "missing from the response" and "removed from the playlist" are
// indistinguishable. Completeness means "yt-dlp succeeded and every entry it returned
// was resolvable", nothing stronger.
func buildSnapshot(meta ytdlp.PlaylistMeta) (title string, url string, entries []snapshotEntry, err error) {
	if meta.ID == "" || strings.TrimSpace(meta.Title) == "" {
		return "", "", nil, ErrPlaylistIncomplete
	}

	entries = make([]snapshotEntry, 0, len(meta.Entries))
	for i := range meta.Entries {
		e := meta.Entries[i]
		id := strings.TrimSpace(e.ID)
		if id == "" {
			if link, ok := parseYTLink(strings.TrimSpace(e.URL)); ok && link.VideoID != "" {
				id = link.VideoID
			}
		}
		if id == "" {
			return "", "", nil, fmt.Errorf("%w: entry %d ('%s') has no video id or url", ErrPlaylistIncomplete, i, e.Title)
		}

		entries = append(entries, snapshotEntry{
			VideoID: id,
			URL:     canonicalVideoURL(id),

			Title:    orUnknown(e.Title, id),
			Position: len(entries),
		})
	}

	if len(entries) == 0 {
		return "", "", nil, ErrPlaylistEmpty
	}
	return meta.Title, canonicalPlaylistURL(meta.ID), entries, nil
}

// One transaction, so any failure leaves the previous snapshot intact.
func (d *DBot) saveSnapshot(ctx context.Context, gid string, meta ytdlp.PlaylistMeta, synced bool) (store.Playlist, error) {
	title, url, entries, err := buildSnapshot(meta)
	if err != nil {
		return store.Playlist{}, err
	}

	var pl store.Playlist
	err = d.StoreDB.WithTx(ctx, func(q *store.Queries) error {
		p, err := q.UpsertPlaylist(ctx, store.UpsertPlaylistParams{
			GuildID:           gid,
			YoutubePlaylistID: meta.ID,
			Title:             title,
			YoutubeUrl:        url,
		})
		if err != nil {
			return fmt.Errorf("upsert playlist: %w", err)
		}
		pl = p

		err = q.DeletePlaylistEntries(ctx, p.ID)
		if err != nil {
			return fmt.Errorf("delete playlist entries: %w", err)
		}

		for _, e := range entries {
			var vid sql.NullString
			if e.VideoID != "" {
				vid = sql.NullString{String: e.VideoID, Valid: true}
			}
			err = q.AddPlaylistEntry(ctx, store.AddPlaylistEntryParams{
				PlaylistID:     p.ID,
				YoutubeVideoID: vid,
				YoutubeUrl:     e.URL,
				Title:          e.Title,
				Position:       int64(e.Position),
			})
			if err != nil {
				return fmt.Errorf("insert playlist entry: %w", err)
			}
		}

		if synced {
			err = q.TouchPlaylistSync(ctx, p.ID)
			if err != nil {
				return fmt.Errorf("touch playlist sync: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return store.Playlist{}, err
	}
	if synced {

		pl, err = d.Store.GetPlaylist(ctx, store.GetPlaylistParams{ID: pl.ID, GuildID: gid})
		if err != nil {
			return store.Playlist{}, err
		}
	}
	return pl, nil
}

// A failed or incomplete fetch leaves the previous snapshot untouched and returns an
// error - the caller must not fall back to it.
func (d *DBot) syncPlaylist(ctx context.Context, pl store.Playlist) (store.Playlist, error) {
	meta, err := d.playlistInfo(pl.YoutubeUrl)
	if err != nil {
		return store.Playlist{}, fmt.Errorf("sync '%s': failed getting playlist info: %w", pl.Title, err)
	}
	if meta.ID != pl.YoutubePlaylistID {
		return store.Playlist{}, fmt.Errorf("sync '%s': %w: got '%s'", pl.Title, ErrPlaylistIDMismatch, meta.ID)
	}

	updated, err := d.saveSnapshot(ctx, pl.GuildID, meta, true)
	if err != nil {
		return store.Playlist{}, fmt.Errorf("sync '%s': %w", pl.Title, err)
	}

	slog.Info("playlist synced", slog.String("playlist", updated.Title))
	return updated, nil
}

type queueResult struct {
	queued      int // appended to the playback queue
	cached      int // played from local media
	waiting     int // no local media, a backup/repair job covers it
	unscheduled int // no local media and we could not schedule a job
	unavailable int // nothing playable right now
}

func (r queueResult) total() int {
	return r.cached + r.waiting + r.unscheduled + r.unavailable
}

// Every entry is resolved from two queries in total, not two per entry: a 500 track
// playlist was 1000 sequential lookups on a connection capped at one.
func (d *DBot) planQueue(ctx context.Context, gid string, entries []store.PlaylistEntry) (tracks []player.Track, res queueResult, err error) {
	// a local copy wins over any job history, so collect the urls a job could exist for
	// before deciding anything
	urls := make([]string, 0, len(entries))
	for _, e := range entries {
		urls = append(urls, canonicalEntryURL(e))
	}

	byURL, err := d.cachedVideoBatch(ctx, entries)
	if err != nil {
		return nil, res, err
	}
	latest, err := d.latestBackupJobs(ctx, gid, videoBackupDir, urls)
	if err != nil {
		return nil, res, err
	}

	for _, e := range entries {
		videoID := e.YoutubeVideoID.String
		url := e.YoutubeUrl

		// a local copy wins over any job history
		path, found := cachedPathFor(byURL, videoID, url)
		if found {
			tracks = append(tracks, player.Track{Title: e.Title, URL: url, Filepath: artefactFile(path)})
			res.queued++
			res.cached++
			continue
		}

		url = canonicalEntryURL(e)
		if url == "" {
			res.unavailable++
			continue
		}

		row, hasRow := latest[url]
		var latestPtr *store.Queue
		if hasRow {
			latestPtr = &row
		}
		state, enqueue := backupStateFor(latestPtr)
		if enqueue {
			meta, err := backupJobMeta(gid, videoBackupDir, url)
			if err != nil {
				slog.Error("backup scheduling failed", slog.Any("error", err), slog.String("url", url))
				res.unscheduled++
				state = backupError
			} else if _, err := d.Store.Enqueue(ctx, store.EnqueueParams{Meta: meta, JobType: BackupJob}); err != nil {
				slog.Error("backup scheduling failed", slog.Any("error", err), slog.String("url", url))
				res.unscheduled++
				state = backupError
			} else {
				state = backupScheduled
			}
		}

		switch state {
		case backupUnavailable:
			// unplayable: youtube refuses it and there is no local copy, so queueing
			// the url would only fail at playback time
			res.unavailable++
			continue
		case backupError:
			res.unscheduled++
		case backupPending, backupScheduled:
			res.waiting++
		}

		tracks = append(tracks, player.Track{Title: e.Title, URL: url})
		res.queued++
	}
	return tracks, res, nil
}

// canonicalEntryURL is the url a playlist entry is cached and scheduled under.
func canonicalEntryURL(e store.PlaylistEntry) string {
	if e.YoutubeVideoID.String != "" {
		return canonicalVideoURL(e.YoutubeVideoID.String)
	}
	return e.YoutubeUrl
}

// Only touches the store DB: cached media lives in the shared backup DB, so artefacts
// and files survive. The row stays, so re-discovering the same playlist reactivates it
// instead of creating a duplicate.
func (d *DBot) deletePlaylist(ctx context.Context, playlistID int64, guildID string) (title string, err error) {

	pl, err := d.Store.GetPlaylist(ctx, store.GetPlaylistParams{ID: playlistID, GuildID: guildID})
	if err != nil {
		return "", err
	}

	err = d.StoreDB.WithTx(ctx, func(q *store.Queries) error {
		res, err := q.SoftDeletePlaylist(ctx, store.SoftDeletePlaylistParams{ID: playlistID, GuildID: guildID})
		if err != nil {
			return fmt.Errorf("soft delete playlist: %w", err)
		}
		if res == 0 {

			return ErrPlaylistNotFound
		}
		if err := q.DeletePlaylistEntries(ctx, playlistID); err != nil {
			return fmt.Errorf("delete playlist entries: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return pl.Title, nil
}

// One batch, so a playlist can never interleave with another enqueue.
func (d *DBot) queueSnapshot(ctx context.Context, gid string, playlistID int64) (queueResult, error) {
	entries, err := d.Store.ListPlaylistEntries(ctx, playlistID)
	if err != nil {
		return queueResult{}, fmt.Errorf("list entries: %w", err)
	}
	tracks, res, err := d.planQueue(ctx, gid, entries)
	if err != nil {
		return res, err
	}

	if err := d.Player().EnqueueMany(tracks); err != nil {
		return res, err
	}
	return res, nil
}
