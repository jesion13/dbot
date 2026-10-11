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

	"dbot/pkg/backup"
	"dbot/pkg/config"
	"dbot/pkg/store"
)

// Shared by playlist entries and individual /play videos, so a video is stored once.
const videoBackupDir = "play_history"

// must match the fail_count < 3 filter in db/queries/queue.sql
const jobAttempts = 3

// artefactFile resolves a stored artefact path to a real file on disk.
func artefactFile(path string) string {
	return filepath.Join(config.BACKUP_DIR, path)
}

// artefactCandidates is every url a video may already be stored under, best first.
// Legacy artefacts were indexed under whatever url yt-dlp handed us, so the raw url is
// a fallback rather than a duplicate.
func artefactCandidates(videoID, rawURL string) []string {
	candidates := make([]string, 0, 2)
	if videoID != "" {
		candidates = append(candidates, canonicalVideoURL(videoID))
	}
	if rawURL != "" && (len(candidates) == 0 || rawURL != candidates[0]) {
		candidates = append(candidates, rawURL)
	}
	return candidates
}

// A genuine backup-db error is returned, never swallowed into a cache miss.
func (d *DBot) cachedVideo(ctx context.Context, videoID, rawURL string) (path string, found bool, err error) {
	for _, c := range artefactCandidates(videoID, rawURL) {
		art, err := d.Backup.GetArtefact(ctx, c)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", false, fmt.Errorf("get artefact '%s': %w", c, err)
		}
		if _, err := os.Stat(artefactFile(art.Path)); err != nil {
			slog.Warn("artefact file missing, scheduling repair", slog.String("url", c), slog.Any("error", err))
			return "", false, nil
		}
		return art.Path, true, nil
	}
	return "", false, nil
}

// cachedVideoBatch is cachedVideo for a whole playlist: one query resolves every entry
// instead of one per entry. A video whose file vanished from disk counts as a miss, the
// same as the single lookup.
func (d *DBot) cachedVideoBatch(ctx context.Context, entries []store.PlaylistEntry) (map[string]backup.Artefact, error) {
	urls := make([]string, 0, len(entries)*2)
	for _, e := range entries {
		urls = append(urls, artefactCandidates(e.YoutubeVideoID.String, e.YoutubeUrl)...)
	}
	if len(urls) == 0 {
		return map[string]backup.Artefact{}, nil
	}

	arts, err := d.Backup.GetArtefactsByURLs(ctx, urls)
	if err != nil {
		return nil, fmt.Errorf("get artefacts: %w", err)
	}
	byURL := make(map[string]backup.Artefact, len(arts))
	for _, a := range arts {
		byURL[a.OriginUrl] = a
	}
	return byURL, nil
}

// cachedPathFor picks the cached path for one entry out of a batch, applying the same
// candidate order and missing-file rule as cachedVideo.
func cachedPathFor(byURL map[string]backup.Artefact, videoID, rawURL string) (string, bool) {
	for _, c := range artefactCandidates(videoID, rawURL) {
		art, ok := byURL[c]
		if !ok {
			continue
		}
		if _, err := os.Stat(artefactFile(art.Path)); err != nil {
			slog.Warn("artefact file missing, scheduling repair", slog.String("url", c), slog.Any("error", err))
			return "", false
		}
		return art.Path, true
	}
	return "", false
}

type backupState int

const (
	backupScheduled backupState = iota

	backupPending
	// youtube will never serve this one; stays in the snapshot but is unplayable
	backupUnavailable

	backupError
)

func backupJobMeta(gid, dir, url string) (string, error) {
	meta, err := json.Marshal(BackupFileParams{OriginUrl: url, GID: gid, Dirs: dir})
	if err != nil {
		return "", fmt.Errorf("marshal backup meta: %w", err)
	}
	return string(meta), nil
}

// backupStateFor decides what to do with a url from the newest job row for it. latest
// is nil when no job has ever run for that url. The second result is whether a new job
// still has to be enqueued.
func backupStateFor(latest *store.Queue) (backupState, bool) {
	if latest == nil {
		return backupScheduled, true
	}
	switch {
	case latest.Status == "unavailable":
		return backupUnavailable, false
	case latest.Status != "done" && latest.FailCount < jobAttempts:
		return backupPending, false
	}
	// done, or out of attempts: worth trying again
	return backupScheduled, true
}

func (d *DBot) scheduleBackup(ctx context.Context, gid, dir, url string) (backupState, error) {
	meta, err := backupJobMeta(gid, dir, url)
	if err != nil {
		return backupError, err
	}

	var latest *store.Queue
	switch row, err := d.Store.LatestJob(ctx, store.LatestJobParams{JobType: BackupJob, Meta: meta}); {
	case err == nil:
		latest = &row
	case errors.Is(err, sql.ErrNoRows):
	default:
		return backupError, fmt.Errorf("latest job for '%s': %w", url, err)
	}

	state, enqueue := backupStateFor(latest)
	if !enqueue {
		return state, nil
	}

	if _, err := d.Store.Enqueue(ctx, store.EnqueueParams{Meta: meta, JobType: BackupJob}); err != nil {
		return backupError, fmt.Errorf("enqueue backup for '%s': %w", url, err)
	}
	return backupScheduled, nil
}

// latestBackupJobs is the batch form: the newest job row per url for a whole playlist,
// in one query. planQueue needs the same decision scheduleBackup makes, for every entry
// at once.
func (d *DBot) latestBackupJobs(ctx context.Context, gid, dir string, urls []string) (map[string]store.Queue, error) {
	if len(urls) == 0 {
		return map[string]store.Queue{}, nil
	}

	metaToURL := make(map[string]string, len(urls))
	metas := make([]string, 0, len(urls))
	for _, u := range urls {
		meta, err := backupJobMeta(gid, dir, u)
		if err != nil {
			return nil, err
		}
		metaToURL[meta] = u
		metas = append(metas, meta)
	}

	rows, err := d.Store.LatestJobsByMeta(ctx, store.LatestJobsByMetaParams{
		JobType: BackupJob,
		Metas:   metas,
	})
	if err != nil {
		return nil, fmt.Errorf("latest jobs: %w", err)
	}

	// the query is ordered by id asc, so the last row for a meta is the newest one
	latest := make(map[string]store.Queue, len(rows))
	for _, row := range rows {
		latest[metaToURL[row.Meta]] = row
	}
	return latest, nil
}

func (d *DBot) saveVideoInDir(url string, gID string, dir string) {
	if _, err := d.scheduleBackup(d.Ctx, gID, dir, url); err != nil {
		slog.Error("failed to schedule backup", slog.Any("error", err), slog.String("url", url))
	}
}
