package ffmpeg

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"dbot/pkg/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeCrop(t *testing.T) {
	frame := Stream{Width: 1920, Height: 1080}

	t.Run("no crop stays no crop", func(t *testing.T) {
		got, err := normalizeCrop(Crop{}, frame)
		require.NoError(t, err)
		assert.Equal(t, Crop{}, got)
	})

	t.Run("a full frame is dropped, the encoder does not need it", func(t *testing.T) {
		got, err := normalizeCrop(Crop{X: 0, Y: 0, W: 1920, H: 1080}, frame)
		require.NoError(t, err)
		assert.Equal(t, Crop{}, got)
	})

	t.Run("an odd size is rounded down", func(t *testing.T) {
		got, err := normalizeCrop(Crop{X: 3, Y: 5, W: 101, H: 51}, frame)
		require.NoError(t, err)
		assert.Equal(t, Crop{X: 3, Y: 5, W: 100, H: 50}, got)
	})

	t.Run("a rect off the frame is refused", func(t *testing.T) {
		_, err := normalizeCrop(Crop{X: 1900, Y: 0, W: 100, H: 100}, frame)
		require.ErrorContains(t, err, "does not fit")
	})

	t.Run("a negative offset is refused", func(t *testing.T) {
		_, err := normalizeCrop(Crop{X: -1, Y: 0, W: 100, H: 100}, frame)
		require.Error(t, err)
	})
}

// the fractions the page sends end up here as pixels, so this is the whole feature in
// one line: a third of a test frame must come back that size.
func TestToDiscordMP4Crops(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}

	dir := t.TempDir()
	input := filepath.Join(dir, "in.mp4")
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=15:duration=2",
		"-pix_fmt", "yuv420p", input).CombinedOutput()
	require.NoError(t, err, "make a test video: %s", out)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ctx = context.WithValue(ctx, config.DirKey, dir)

	f, err := ToDiscordMP4(ctx, input, true, Clip{}, Crop{X: 20, Y: 10, W: 201, H: 101})
	require.NoError(t, err)
	defer f.Close()

	info, err := Probe(f.Name())
	require.NoError(t, err)
	stream, err := firstVideoStream(info)
	require.NoError(t, err)
	assert.Equal(t, 200, stream.Width)
	assert.Equal(t, 100, stream.Height)
}

// the edit page re-submits into the same directory, where the previous encode already
// wrote its file; ffmpeg has to replace it, not refuse the existing output and exit 0
// with the old crop still on disk (which is what the bot then posts)
func TestToDiscordMP4ReplacesThePreviousCrop(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}

	dir := t.TempDir()
	input := filepath.Join(dir, "in.mp4")
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=15:duration=2",
		"-pix_fmt", "yuv420p", input).CombinedOutput()
	require.NoError(t, err, "make a test video: %s", out)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ctx = context.WithValue(ctx, config.DirKey, dir)

	clip := Clip{End: time.Second}

	first, err := ToDiscordMP4(ctx, input, true, clip, Crop{X: 20, Y: 10, W: 200, H: 100})
	require.NoError(t, err)
	first.Close()

	// a full-frame second submit: the old file is a 200x100 crop, so the un-cropped
	// size is what proves the second encode replaced it
	second, err := ToDiscordMP4(ctx, input, true, clip, Crop{})
	require.NoError(t, err)
	defer second.Close()

	info, err := Probe(second.Name())
	require.NoError(t, err)
	stream, err := firstVideoStream(info)
	require.NoError(t, err)
	assert.Equal(t, 320, stream.Width)
	assert.Equal(t, 240, stream.Height)
}
