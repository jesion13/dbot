package dbot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"

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

// buildSnapshot validates playlist metadata and turns it into ordered entries.
//
// An entry with a nil Duration is kept: that usually means the video is unavailable,
// not that it left the playlist. An entry we cannot resolve to a video id/url at all
// makes the whole result unusable - dropping it would silently delete a member from
// the previous snapshot.
//
// KNOWN LIMITATION: `yt-dlp --flat-playlist` exits 0 and gives no entry count we can
// cross-check, so "this entry is missing from the response" and "this entry was removed
// from the playlist" are indistinguishable. Completeness here means "yt-dlp succeeded
// and every entry it returned was resolvable", nothing stronger.
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
			// an unavailable video has no title; keep the membership anyway
			Title:    orUnknown(e.Title, id),
			Position: len(entries),
		})
	}

	if len(entries) == 0 {
		return "", "", nil, ErrPlaylistEmpty
	}
	return meta.Title, canonicalPlaylistURL(meta.ID), entries, nil
}

// saveSnapshot writes the playlist and its whole entry list in one transaction and
// returns the stored playlist. On any failure the previous snapshot stays intact.
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
		// the upsert row predates TouchPlaylistSync
		pl, err = d.Store.GetPlaylist(ctx, store.GetPlaylistParams{ID: pl.ID, GuildID: gid})
		if err != nil {
			return store.Playlist{}, err
		}
	}
	return pl, nil
}

// syncPlaylist fetches the current playlist from YouTube and reconciles the saved
// snapshot. A failed or incomplete fetch leaves the previous snapshot untouched and
// returns an error - the caller must not fall back to it.
func (d *DBot) syncPlaylist(ctx context.Context, pl store.Playlist) (store.Playlist, error) {
	meta, err := d.playlistInfo(pl.YoutubeUrl)
	if err != nil {
		return store.Playlist{}, fmt.Errorf("sync '%s': failed getting playlist info: %w", pl.Title, err)
	}
	if meta.ID != pl.YoutubePlaylistID {
		return store.Playlist{}, fmt.Errorf("sync '%s': %w: got '%s'", pl.Title, ErrPlaylistIDMismatch, meta.ID)
	}

	// buildSnapshot rejects incomplete/empty metadata, so a truncated fetch can never
	// be mistaken for "these videos were removed".
	updated, err := d.saveSnapshot(ctx, pl.GuildID, meta, true)
	if err != nil {
		return store.Playlist{}, fmt.Errorf("sync '%s': %w", pl.Title, err)
	}

	slog.Info("playlist synced", slog.String("playlist", updated.Title))
	return updated, nil
}

// queueResult is what happened to one snapshot.
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

// planQueue resolves every snapshot entry to something playable, in snapshot order.
// It returns the links to append plus what the cache is doing about each entry.
func (d *DBot) planQueue(ctx context.Context, gid string, entries []store.PlaylistEntry) (links []string, res queueResult, err error) {
	for _, e := range entries {
		videoID := e.YoutubeVideoID.String
		url := e.YoutubeUrl

		// a local copy wins over any job history: the file is playable regardless of
		// what a past attempt concluded
		path, found, err := d.cachedVideo(ctx, videoID, url)
		if err != nil {
			return nil, res, fmt.Errorf("lookup '%s': %w", url, err)
		}
		if found {
			links = append(links, path)
			res.queued++
			res.cached++
			continue
		}

		if videoID != "" {
			url = canonicalVideoURL(videoID)
		}
		if url == "" {
			// still a snapshot member, just not playable right now
			res.unavailable++
			continue
		}

		state, err := d.scheduleBackup(ctx, gid, videoBackupDir, url)
		switch state {
		case backupUnavailable:
			// still a member of the snapshot, but youtube will not serve it and there is
			// no local copy - queueing the url would only fail at playback time
			res.unavailable++
			continue
		case backupError:
			// still play from source, but say so honestly
			slog.Error("backup scheduling failed", slog.Any("error", err), slog.String("url", url))
			res.unscheduled++
		case backupPending, backupScheduled:
			res.waiting++
		}

		links = append(links, url)
		res.queued++
	}
	return links, res, nil
}

// queueSnapshot appends a whole saved snapshot to the existing queue, in stored order.
func (d *DBot) queueSnapshot(ctx context.Context, gid string, playlistID int64) (queueResult, error) {
	entries, err := d.Store.ListPlaylistEntries(ctx, playlistID)
	if err != nil {
		return queueResult{}, fmt.Errorf("list entries: %w", err)
	}
	links, res, err := d.planQueue(ctx, gid, entries)
	if err != nil {
		return res, err
	}
	for _, l := range links {
		d.MusicPlayer.Add(l)
	}
	return res, nil
}
