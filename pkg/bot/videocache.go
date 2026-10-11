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

// Shared by playlist entries and individual /play videos, so a video is stored once.
const videoBackupDir = "play_history"

// must match the fail_count < 3 filter in db/queries/queue.sql
const jobAttempts = 3

// artefactFile resolves a stored artefact path to a real file on disk.
func artefactFile(path string) string {
	return filepath.Join(config.BACKUP_DIR, path)
}

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

			return backupUnavailable, nil
		case latest.Status != "done" && latest.FailCount < jobAttempts:

			return backupPending, nil
		}

	case errors.Is(err, sql.ErrNoRows):

	default:

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
