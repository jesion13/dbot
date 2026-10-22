package ffmpeg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"dbot/pkg/config"
)

var ErrFfmpegError = errors.New("ffmpeg error")

const (
	DiscordMaxFileSizeBytes = 20 << 20
	discordOutputFPS        = 24
	discordAudioBitrateBPS  = 48_000
	discordSafetyMargin     = 0.97
	discordTargetBPP        = 0.06
)

type videoSettings struct {
	BitrateKbps int
	Width       int
	Height      int
	Scale       bool
}

// the returned file is closed when the context is canceled
func ToDiscordMP4(ctx context.Context, file string, mute bool, clip Clip) (*os.File, error) {
	tmpDir, ok := ctx.Value(config.DirKey).(string)
	if !ok || len(tmpDir) == 0 {
		return nil, errors.New("nie dałeś temp dira debilu")
	}

	mp4Path := filepath.Join(tmpDir, "discord.dupa.mp4")
	info, err := Probe(file)
	if err != nil {
		return nil, err
	}

	slog.Debug("ToDiscordMP4", slog.String("dir", tmpDir))
	duration := info.Format.Duration.Seconds()
	if clip.End > 0 {
		duration = clip.End.Seconds()
	}
	if clip.Start > 0 {
		duration -= clip.Start.Seconds()
	}
	if duration <= 0 {
		return nil, errors.New("invalid clip duration")
	}
	video, err := firstVideoStream(info)
	if err != nil {
		return nil, err
	}

	videoBudgetBPS, err := discordVideoBudgetBPS(duration, mute)
	if err != nil {
		return nil, err
	}
	settings, err := selectDiscordVideoSettings(video, videoBudgetBPS)
	if err != nil {
		return nil, err
	}
	slog.Debug("ToDiscordMP4 settings",
		slog.Int("bitrateKbps", settings.BitrateKbps),
		slog.Int("width", settings.Width),
		slog.Int("height", settings.Height))
	base := []string{
		"-hide_banner",
	}
	if clip.Start > 0 {
		base = append(base, "-ss", formatDurationForFFmpeg(clip.Start))
	}
	base = append(base, "-i", file)
	if clip.End > 0 {
		base = append(base, "-t", fmt.Sprintf("%.2f", clip.End.Seconds()-clip.Start.Seconds()))
	}
	base = append(base, "-c:v", "libx264")
	if settings.Scale {
		base = append(base, "-vf", fmt.Sprintf("scale=%d:%d", settings.Width, settings.Height))
	}
	base = append(
		base,
		"-preset", "veryslow",
		"-r", fmt.Sprintf("%d", discordOutputFPS),
		"-b:v", fmt.Sprintf("%dK", settings.BitrateKbps),
		"-passlogfile", filepath.Join(tmpDir, "discord-pass"),
	)

	cmd := exec.CommandContext(ctx, "ffmpeg")
	cmd.Args = append(cmd.Args, base...)
	cmd.Args = append(cmd.Args,
		"-an", "-pass", "1", "-f", "mp4", "-y", "/dev/null")

	slog.Info("convertToDiscordMP4 first pass", slog.String("cmd", cmd.String()))
	err = runCmd(cmd)
	if err != nil {
		return nil, err
	}

	cmd = exec.CommandContext(ctx, "ffmpeg")
	cmd.Args = append(cmd.Args, base...)
	if mute {
		cmd.Args = append(cmd.Args,
			"-pass", "2",
			"-an",
			"-movflags", "+faststart",
			mp4Path)
	} else {
		cmd.Args = append(cmd.Args,
			"-pass", "2",
			"-c:a", "libopus",
			"-b:a", "48k",
			"-movflags", "+faststart",
			mp4Path)
	}

	slog.Info("convertToDiscordMP4 second pass", slog.String("cmd", cmd.String()))
	err = runCmd(cmd)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(mp4Path)
	if err != nil {
		return nil, err
	}
	stat, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if stat.Size() >= DiscordMaxFileSizeBytes {
		f.Close()
		return nil, fmt.Errorf("Discord MP4 is too large: %d bytes", stat.Size())
	}
	slog.Debug("convertToDiscordMP4",
		slog.String("mp4Path", mp4Path),
		slog.String("file", f.Name()), slog.Int64("size", stat.Size()))

	go func() {
		<-ctx.Done()
		f.Close()
	}()

	return f, nil
}

func firstVideoStream(info Streams) (Stream, error) {
	for _, stream := range info.Streams {
		if stream.CodecType == "video" && stream.Width > 0 && stream.Height > 0 {
			return stream, nil
		}
	}

	return Stream{}, errors.New("input does not contain a video stream with valid dimensions")
}

func discordVideoBudgetBPS(duration float64, mute bool) (float64, error) {
	audioBitrateBPS := 0
	if !mute {
		audioBitrateBPS = discordAudioBitrateBPS
	}

	totalBudgetBPS := float64(DiscordMaxFileSizeBytes*8) * discordSafetyMargin / duration
	videoBudgetBPS := totalBudgetBPS - float64(audioBitrateBPS)
	if videoBudgetBPS < 1_000 {
		return 0, errors.New("clip is too long for the Discord size limit")
	}

	return videoBudgetBPS, nil
}

func selectDiscordVideoSettings(video Stream, videoBudgetBPS float64) (videoSettings, error) {
	if video.Width <= 0 || video.Height <= 0 {
		return videoSettings{}, errors.New("video stream has invalid dimensions")
	}

	bitrateKbps := int(math.Floor(videoBudgetBPS / 1_000))
	if bitrateKbps < 1 {
		return videoSettings{}, errors.New("video bitrate budget is too small")
	}

	for _, maxShortSide := range []int{720, 480, 360, 240} {
		width, height := scaledDimensions(video.Width, video.Height, maxShortSide)
		requiredBPS := float64(width*height*discordOutputFPS) * discordTargetBPP
		if videoBudgetBPS >= requiredBPS {
			return videoSettings{
				BitrateKbps: bitrateKbps,
				Width:       width,
				Height:      height,
				Scale:       width != video.Width || height != video.Height,
			}, nil
		}
	}

	width, height := scaledDimensions(video.Width, video.Height, 240)
	return videoSettings{
		BitrateKbps: bitrateKbps,
		Width:       width,
		Height:      height,
		Scale:       width != video.Width || height != video.Height,
	}, nil
}

func scaledDimensions(width, height, maxShortSide int) (int, int) {
	shortSide := min(width, height)
	if shortSide <= maxShortSide {
		return width, height
	}

	scale := float64(maxShortSide) / float64(shortSide)
	return evenDimension(float64(width) * scale), evenDimension(float64(height) * scale)
}

func evenDimension(value float64) int {
	return max(int(math.Round(value/2))*2, 2)
}

type Clip struct {
	Start time.Duration
	End   time.Duration
}

type GifSettings struct {
	Height int
	FPS    int
	Clip   Clip
}

func formatDurationForFFmpeg(d time.Duration) string {
	totalSecs := int(d.Seconds())
	hours := totalSecs / 3600
	minutes := (totalSecs % 3600) / 60
	seconds := totalSecs % 60

	if hours > 0 {
		return fmt.Sprintf("%d:%02d:%02d", hours, minutes, seconds)
	}
	return fmt.Sprintf("%d:%02d", minutes, seconds)
}

// Accepts "90", "90s", "2m30s", "1:30" and "1:30:45".
func ParseTime(s string) (time.Duration, error) {
	if strings.Contains(s, ":") {
		parts := strings.Split(s, ":")
		var total time.Duration
		for i, p := range parts {
			v, err := strconv.Atoi(p)
			if err != nil {
				return 0, err
			}
			switch len(parts) - i {
			case 3:
				total += time.Duration(v) * time.Hour
			case 2:
				total += time.Duration(v) * time.Minute
			case 1:
				total += time.Duration(v) * time.Second
			}
		}
		return total, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		d, err = time.ParseDuration(s + "s")
		if err != nil {
			return 0, err
		}
	}
	return d, nil
}

// the returned file is closed when the context is canceled
func ToDiscordGIF(ctx context.Context, file string, clip Clip) (*os.File, error) {
	tmpDir, ok := ctx.Value(config.DirKey).(string)
	if !ok || len(tmpDir) == 0 {
		return nil, errors.New("nie dałeś temp dira debilu")
	}

	gifPath := filepath.Join(tmpDir, "discord.dupa.webp")
	info, err := Probe(file)
	if err != nil {
		return nil, err
	}

	slog.Debug("ToDiscordGIF", slog.String("dir", tmpDir))

	duration := info.Format.Duration.Seconds()
	if clip.End > 0 {
		duration = clip.End.Seconds()
	}
	if clip.Start > 0 {
		duration -= clip.Start.Seconds()
	}

	cmd := exec.CommandContext(ctx, "ffmpeg")
	cmd.Args = append(cmd.Args, "-hide_banner")
	if clip.Start > 0 {
		cmd.Args = append(cmd.Args, "-ss", formatDurationForFFmpeg(clip.Start))
	}
	cmd.Args = append(
		cmd.Args,
		"-i", file,
		"-t", fmt.Sprintf("%.2f", duration),
		"-vcodec", "libwebp",
		"-filter:v", "fps=24,scale=480:-1:flags=lanczos",
		"-lossless", "0", // Use lossy compression for video source efficiency
		"-q:v", "75", // Quality factor (75-80 is the sweet spot for webp)
		"-loop", "0", // Infinite loop play state
		"-an", // Strip audio track
		"-y",
		gifPath,
	)

	slog.Info("convertToDiscordGIF", slog.String("cmd", cmd.String()))
	err = runCmd(cmd)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(gifPath)
	if err != nil {
		return nil, err
	}
	stat, err := f.Stat()
	slog.Debug("convertToDiscordGIF",
		slog.String("gifPath", gifPath),
		slog.String("file", f.Name()), slog.Int64("size", stat.Size()))

	go func() {
		<-ctx.Done()
		f.Close()
	}()

	return f, nil
}

func runCmd(cmd *exec.Cmd) error {
	buf := bytes.NewBuffer(nil)
	cmd.Stdout = buf
	cmd.Stderr = buf
	err := cmd.Start()
	if err != nil {
		return fmt.Errorf("%w: cmd.Start failed: %w", ErrFfmpegError, err)
	}

	err = cmd.Wait()
	if err != nil {
		fmt.Println(buf.String())
		return fmt.Errorf("%w: cmd.Wait failed: %w,\n%s", ErrFfmpegError, err, buf.String())
	}

	return nil
}

// the returned file is closed when the context is canceled
// The stored cache is decoded to mono opus for playback, so the picture is discarded
// during playback either way and AV1 is the right target: it is roughly 30% smaller
// than H.264 at the same quality, and on this hardware it also encodes faster than the
// H.264 path.
//
// gq25, not something cheaper: videos get taken down, and the cache is the only copy
// left. Measured over the same 30s of 4K, gq25 came to 9.7MB against 9.6MB for the
// untouched source - it reproduces what youtube itself encodes at, so a fallback
// transcode is indistinguishable from having downloaded an av1 original. gq30 was
// 5.5MB and gq35 3.3MB; the savings are not worth losing the picture.
//
// Benchmarks are kept in data/bench-av1 (gitignored) if this ever needs re-tuning.
const av1Quality = "25"

// ConvertToMP4 is for files that leave the process and have to be playable by
// something other than ffmpeg - discord rejects AV1 and VP9 on upload, so /to-mp4
// still needs H.264.
func ConvertToMP4(ctx context.Context, file string, clip Clip) (*os.File, error) {
	return convert(ctx, file, clip, "h264_qsv", "23", "veryslow")
}

// ConvertToAV1 is for the playback cache, which only ffmpeg ever reads.
func ConvertToAV1(ctx context.Context, file string, clip Clip) (*os.File, error) {
	return convert(ctx, file, clip, "av1_qsv", av1Quality, "")
}

func convert(ctx context.Context, file string, clip Clip, encoder, quality, preset string) (*os.File, error) {
	tmpDir, ok := ctx.Value(config.DirKey).(string)
	if !ok || len(tmpDir) == 0 {
		return nil, errors.New("nie dałeś temp dira debilu")
	}

	buf := bytes.NewBuffer(nil)
	name := strings.ReplaceAll(file, filepath.Ext(file), "")
	mp4Path := filepath.Join(tmpDir, fmt.Sprintf("edit.%s.mp4", filepath.Base(name)))
	cmd := exec.CommandContext(ctx, "ffmpeg")
	cmd.Args = append(cmd.Args,
		"-hide_banner",
		// without this ffmpeg prompts when the output already exists, and since the
		// backup job runner is one serial loop a prompt stalls every other job
		"-y",
		"-init_hw_device", "qsv=hw",
		"-filter_hw_device", "hw")
	if clip.Start > 0 {
		cmd.Args = append(cmd.Args, "-ss", formatDurationForFFmpeg(clip.Start))
	}
	cmd.Args = append(cmd.Args, "-i", file)
	if clip.End > 0 {
		cmd.Args = append(cmd.Args, "-t", fmt.Sprintf("%.2f", clip.End.Seconds()-clip.Start.Seconds()))
	}
	cmd.Args = append(cmd.Args, "-c:v", encoder, "-global_quality", quality)
	if preset != "" {
		cmd.Args = append(cmd.Args, "-preset", preset)
	}
	cmd.Args = append(cmd.Args, "-movflags", "+faststart", mp4Path)
	cmd.Stdout = buf
	cmd.Stderr = buf

	slog.Info("convert", slog.String("encoder", encoder), slog.String("cmd", cmd.String()))
	err := cmd.Start()
	if err != nil {
		return nil, fmt.Errorf("cmd.Start failed: %w", err)
	}

	err = cmd.Wait()
	if err != nil {
		return nil, fmt.Errorf("cmd.Wait failed: %w,\n%s", err, buf.String())
	}

	f, err := os.Open(mp4Path)
	if err != nil {
		return nil, err
	}

	go func() {
		<-ctx.Done()
		f.Close()
	}()

	return f, nil
}
