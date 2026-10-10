package dbot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"dbot/pkg/config"
	jobrunner "dbot/pkg/job_runner"
)

const BackupJob = "backup"

// isUnavailableVideo reports a yt-dlp failure that retrying cannot fix.
//
// Only messages with a confirmed cause qualify. Anything generic - "Video
// unavailable", "Private video", a region block, a sign-in or age prompt - stays
// retryable: those can be temporary, cookie/config related, or resolved by a later
// attempt, and parking them as unavailable would block recovery forever.
//
// shortcut: matching yt-dlp's stderr text; if upstream rewords a message we fall back
// to retrying, which is noisy but not wrong.
func isUnavailableVideo(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	// confirmed, specific causes only - never a bare "has been removed"
	for _, msg := range []string{
		"removed for violating youtube's terms of service", // tos strike
		"removed for violating our terms of service",
		"has been removed by the uploader",
		"was removed by the uploader",
		"account associated with this video has been terminated",
		"this video is no longer available because the account",
	} {
		if strings.Contains(s, msg) {
			return true
		}
	}
	return false
}

func (d *DBot) backupVideoJob(meta string) error {
	var bf BackupFileParams
	err := json.Unmarshal([]byte(meta), &bf)
	if err != nil {
		return fmt.Errorf("backupFile: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// re-check the cache here: the caller's preflight lookup can be minutes old and
	// another job for the same video may have finished in the meantime.
	path, found, err := d.cachedVideo(ctx, "", bf.OriginUrl)
	if err != nil {
		return fmt.Errorf("backupFile: %w", err)
	}
	if found {
		slog.Debug("backup skipped, already cached", slog.String("url", bf.OriginUrl), slog.String("path", path))
		return nil
	}

	dir := filepath.Join(config.TMP_PATH, "backup")
	os.MkdirAll(dir, 0o755)
	ctx = context.WithValue(ctx, config.DirKey, dir)

	f, err := d.downloadAsMP4(ctx, bf.OriginUrl)
	if err != nil {
		if isUnavailableVideo(err) {
			// still a playlist member, but youtube will not serve it. Retrying burns
			// attempts and, worse, blocks every other job behind this one - the runner
			// is a single loop over `id asc`.
			slog.Info("backup skipped, video unavailable", slog.String("url", bf.OriginUrl))
			return fmt.Errorf("%w: %w", jobrunner.ErrPermanent, err)
		}
		return fmt.Errorf("backupFile: %w", err)
	}
	defer f.File.Close()
	defer os.Remove(f.Name)
	bf.File = f.File
	bf.Name = f.Name
	bf.ContentType = "video/mp4"

	slog.Debug("backupJob", slog.Any("bf", bf))
	_, err = d.backupArtefact(ctx, bf)
	if err != nil {
		return fmt.Errorf("backupFile: %w", err)
	}

	return nil
}
