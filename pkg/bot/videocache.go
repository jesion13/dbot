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

	"dbot/pkg/config"
	"dbot/pkg/store"
)

// videoBackupDir is the shared backup subdir for downloaded videos. Playlist entries
// and individual /play videos use the same one, so a video is stored once and reused.
const videoBackupDir = "play_history"

// jobAttempts is the total attempt budget for one job record (first run included).
// Must match the fail_count < 3 filter in db/queries/queue.sql.
const jobAttempts = 3

// artefactFile resolves a stored artefact path to a real file on disk.
func artefactFile(path string) string {
	return filepath.Join(config.BACKUP_DIR, path)
}

// cachedVideo returns a usable local path for a youtube video.
// found is false when nothing usable is cached, which means a backup job should run.
// A genuine backup-db error is returned, never swallowed into a cache miss.
func (d *DBot) cachedVideo(ctx context.Context, videoID, rawURL string) (path string, found bool, err error) {
	candidates := make([]string, 0, 2)
	if videoID != "" {
		candidates = append(candidates, canonicalVideoURL(videoID))
	}
	if rawURL != "" && (len(candidates) == 0 || rawURL != candidates[0]) {
		// legacy artefacts were indexed under whatever url yt-dlp handed us
		candidates = append(candidates, rawURL)
	}

	for _, c := range candidates {
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

// backupState is the outcome of asking "does this video need a backup job?".
type backupState int

const (
	// backupScheduled means a new job row was just inserted.
	backupScheduled backupState = iota
	// backupPending means an existing job is still queued or retryable.
	backupPending
	// backupUnavailable means the last attempt proved youtube will never serve this
	// video. It stays in the snapshot; it just cannot be cached or played.
	backupUnavailable
	// backupError means the lookup or insert failed.
	backupError
)

func backupJobMeta(gid, dir, url string) (string, error) {
	meta, err := json.Marshal(BackupFileParams{OriginUrl: url, GID: gid, Dirs: dir})
	if err != nil {
		return "", fmt.Errorf("marshal backup meta: %w", err)
	}
	return string(meta), nil
}

// scheduleBackup decides whether this video needs a backup job, from the latest job
// record for it. Queue rows are never deleted, so only the newest one is current.
func (d *DBot) scheduleBackup(ctx context.Context, gid, dir, url string) (backupState, error) {
	meta, err := backupJobMeta(gid, dir, url)
	if err != nil {
		return backupError, err
	}
	params := store.LatestJobParams{JobType: BackupJob, Meta: meta}

	latest, err := d.Store.LatestJob(ctx, params)
	switch {
	case err == nil:
		switch {
		case latest.Status == "unavailable":
			// terminal: youtube refused the video, retrying will not help
			return backupUnavailable, nil
		case latest.Status != "done" && latest.FailCount < jobAttempts:
			// still queued or still retryable
			return backupPending, nil
		}
		// done without a usable file, or out of attempts: allow a fresh attempt
	case errors.Is(err, sql.ErrNoRows):
		// never tried
	default:
		// a broken lookup must not look like a cache hit
		return backupError, fmt.Errorf("latest job for '%s': %w", url, err)
	}

	if _, err := d.Store.Enqueue(ctx, store.EnqueueParams{Meta: meta, JobType: BackupJob}); err != nil {
		return backupError, fmt.Errorf("enqueue backup for '%s': %w", url, err)
	}
	return backupScheduled, nil
}

func (d *DBot) saveVideoInDir(url string, gID string, dir string) {
	if _, err := d.scheduleBackup(d.Ctx, gID, dir, url); err != nil {
		slog.Error("failed to schedule backup", slog.Any("error", err), slog.String("url", url))
	}
}
