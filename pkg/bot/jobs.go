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

// isUnavailableVideo reports a yt-dlp failure that will not fix itself on retry.
// shortcut: matching yt-dlp's stderr text; if upstream rewords it we just fall back to
// retrying like before, which is noisy but not wrong.
//
// Sign-in / age / cookie errors are deliberately NOT matched: those are configuration
// problems, so a retry with working cookies would succeed. They stay retryable and burn
// the ordinary three attempts instead of being parked as unavailable forever.
func isUnavailableVideo(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, msg := range []string{
		"Video unavailable",   // deleted / private / region blocked
		"Private video",       // not public
		"has been removed",    // TOS strike / removed by uploader
		"no longer available", // terminated account
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
