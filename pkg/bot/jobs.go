package dbot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"dbot/pkg/config"
	jobrunner "dbot/pkg/job_runner"
)

const BackupJob = "backup"

// Only messages with a confirmed cause qualify. "Private video", a region block, a
// sign-in or age prompt stay retryable: those can be temporary or resolved by a later
// attempt, and parking them as unavailable would block recovery forever.
// A bare "Video unavailable" is treated as dead too - it is what youtube answers for
// every video it will never serve, and retrying it just burns yt-dlp runs and log
// noise on every /playlist. A video parked this way can still come back by hand:
// delete the "unavailable" queue row.
//
// shortcut: matching yt-dlp's stderr text; if upstream rewords a message we fall back
// to retrying, which is noisy but not wrong.
func isUnavailableVideo(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())

	for _, msg := range []string{
		"removed for violating youtube's terms of service", // tos strike
		"removed for violating our terms of service",
		"has been removed by the uploader",
		"was removed by the uploader",
		"account associated with this video has been terminated",
		"this video is no longer available because the account",
		"video unavailable",
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

	// re-check the cache: the caller's preflight lookup can be minutes old and another
	// job for the same video may have finished since.
	// The video id matters here: rows written years ago are indexed under whatever url
	// form they arrived in (youtu.be, music.youtube.com), so looking up only the raw url
	// misses the copy already on disk and downloads the same video a second time.
	art, found, err := d.cachedVideo(ctx, videoIDForURL(bf.OriginUrl), bf.OriginUrl)
	if err != nil {
		return fmt.Errorf("backupFile: %w", err)
	}
	if found {
		slog.Debug("backup skipped, already cached", slog.String("url", bf.OriginUrl), slog.String("path", art.Path))
		return nil
	}

	// The download and the edit.* transcode output both land in this dir (ytdlp runs
	// with cmd.Dir from DirKey, convert() writes next to its input), so one RemoveAll
	// leaves nothing behind whatever the job does next - including a download or an
	// encode that dies halfway, which used to leave both files in a shared dir.
	dir, err := os.MkdirTemp(config.TMP_PATH, "backup")
	if err != nil {
		return fmt.Errorf("backupFile: %w", err)
	}
	defer os.RemoveAll(dir)
	ctx = context.WithValue(ctx, config.DirKey, dir)

	f, err := d.fetchVideo(ctx, bf.OriginUrl)
	if err != nil {
		if isUnavailableVideo(err) {
			// still a playlist member, but youtube will not serve it. Retrying burns
			// attempts and blocks every other job behind this one - the runner is a
			// single loop over `id asc`.
			slog.Info("backup skipped, video unavailable", slog.String("url", bf.OriginUrl))
			return fmt.Errorf("%w: %w", jobrunner.ErrPermanent, err)
		}
		return fmt.Errorf("backupFile: %w", err)
	}
	defer f.File.Close()
	// Nothing removes the yt-dlp download today, so every converted video leaves its
	// source behind and the temp dir grows by the size of the whole cache. The temp
	// dir and BACKUP_DIR are usually different mounts, so the store copies and this
	// deletes rather than renaming.
	defer func() {
		if err := os.Remove(f.SourcePath); err != nil && !os.IsNotExist(err) {
			slog.Warn("could not remove the download after storing it", slog.String("path", f.SourcePath), slog.Any("error", err))
		}
	}()

	bf.File = f.File
	// the stored name is the yt-dlp id plus a hash of the source, never the title: a
	// long title can exceed the filesystem's filename limit, and two different videos
	// can share a title (and, for direct links, an id). The extension is the real one,
	// so a stored webm is not called an mp4.
	bf.Name = artefactName(bf.OriginUrl, f.ID, f.Ext)
	bf.Title = f.Title
	bf.ContentType = mediaTypeForExt(f.Ext)

	slog.Debug("backupJob", slog.Any("bf", bf))
	_, err = d.backupArtefact(ctx, bf)
	if err != nil {
		return fmt.Errorf("backupFile: %w", err)
	}

	return nil
}
