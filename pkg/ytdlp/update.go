package ytdlp

import (
	"context"
	"log/slog"
	"os/exec"
	"time"
)

func StartUpdater(ctx context.Context) {
	tic := time.NewTicker(time.Hour)
	version, err := exec.Command("yt-dlp", "--version").CombinedOutput()
	if err != nil {
		slog.Error("StartUpdater.yt-dlp -U",
			slog.String("timestamp", time.Now().String()),
			slog.Any("error", err),
			slog.String("out", string(version)))
	}
	slog.Info("yt-dlp", slog.String("version", string(version)))
	go func() {
		for {
			out, err := exec.Command("yt-dlp", "-U").CombinedOutput()
			if err != nil {
				slog.Error("StartUpdater.yt-dlp -U",
					slog.String("timestamp", time.Now().String()),
					slog.Any("error", err),
					slog.String("out", string(out)))
			}

			versionNew, err := exec.Command("yt-dlp", "--version").CombinedOutput()
			if err != nil {
				slog.Error("StartUpdater.yt-dlp -U",
					slog.String("timestamp", time.Now().String()),
					slog.Any("error", err),
					slog.String("out", string(version)))
			}
			if string(version) != string(versionNew) {
				slog.Info("updated yt-dlp", slog.String("old", string(version)), slog.String("new", string(versionNew)))
				version = versionNew
			}

			select {
			case <-tic.C:
			case <-ctx.Done():
				return
			}
		}
	}()
}
