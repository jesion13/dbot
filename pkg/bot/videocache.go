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

// enqueueBackup schedules a backup unless this exact job already ran.
// queued is false when the queue already knows about it.
func (d *DBot) enqueueBackup(ctx context.Context, gid, dir, url string) (queued bool, err error) {
	meta := BackupFileParams{
		OriginUrl:   url,
		GID:         gid,
		Dirs:        dir,
		PrependTime: false,
	}
	jsonMeta, err := json.Marshal(meta)
	if err != nil {
		return false, fmt.Errorf("marshal backup meta: %w", err)
	}

	// a dead video (private, TOS-removed, deleted) is still a playlist member and can
	// never be cached - the queue is where we already know we tried and failed
	seen, err := d.Store.QueueHasJob(ctx, store.QueueHasJobParams{
		JobType: BackupJob,
		Meta:    string(jsonMeta),
	})
	if err != nil {
		// can't tell; a duplicate job is harmless, a missed one is not
		slog.Error("queue lookup failed", slog.Any("error", err), slog.String("url", url))
	} else if seen > 0 {
		return false, nil
	}

	_, err = d.Store.Enqueue(ctx, store.EnqueueParams{
		Meta:    string(jsonMeta),
		JobType: BackupJob,
	})
	if err != nil {
		return false, fmt.Errorf("enqueue backup for '%s': %w", url, err)
	}
	return true, nil
}

func (d *DBot) saveVideoInDir(url string, gID string, dir string) {
	if _, err := d.enqueueBackup(d.Ctx, gID, dir, url); err != nil {
		slog.Error("failed to schedule backup", slog.Any("error", err), slog.String("url", url))
	}
}
