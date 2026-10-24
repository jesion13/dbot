package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"dbot/pkg/config"
	"dbot/pkg/ffmpeg"
)

func main() {
	inputPath := flag.String("input", "", "path to the input video (required)")
	mute := flag.Bool("mute", false, "remove audio from the output")
	start := flag.Duration("start", 0, "clip start, for example 10s or 1m30s")
	end := flag.Duration("end", 0, "clip end, for example 30s or 2m")
	outputDir := flag.String("output-dir", filepath.Join("tmp", "ffmpeg-debug"), "directory for output and FFmpeg pass logs")
	flag.Parse()

	if *inputPath == "" {
		fmt.Fprintln(os.Stderr, "-input is required")
		flag.Usage()
		os.Exit(2)
	}

	if err := os.MkdirAll(*outputDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "create output directory: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx = context.WithValue(ctx, config.DirKey, *outputDir)

	output, err := ffmpeg.ToDiscordMP4(ctx, *inputPath, *mute, ffmpeg.Clip{
		Start: *start,
		End:   *end,
	}, ffmpeg.Crop{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "convert video: %v\n", err)
		os.Exit(1)
	}
	defer output.Close()

	info, err := output.Stat()
	if err != nil {
		fmt.Fprintf(os.Stderr, "read output metadata: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Created %s (%d bytes)\n", output.Name(), info.Size())
}
