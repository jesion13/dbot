package ffmpeg

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTime(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		t.Run("parses seconds only", func(t *testing.T) {
			d, err := ParseTime("90")
			require.NoError(t, err)
			assert.Equal(t, 90*time.Second, d)
		})

		t.Run("parses with s suffix", func(t *testing.T) {
			d, err := ParseTime("60s")
			require.NoError(t, err)
			assert.Equal(t, 60*time.Second, d)
		})

		t.Run("parses standard go duration format", func(t *testing.T) {
			d, err := ParseTime("2m30s")
			require.NoError(t, err)
			assert.Equal(t, 150*time.Second, d)
		})

		t.Run("parses minutes only with s suffix", func(t *testing.T) {
			d, err := ParseTime("5m")
			require.NoError(t, err)
			assert.Equal(t, 5*time.Minute, d)
		})

		t.Run("parses zero", func(t *testing.T) {
			d, err := ParseTime("0")
			require.NoError(t, err)
			assert.Equal(t, time.Duration(0), d)
		})

		t.Run("parses decimal seconds", func(t *testing.T) {
			d, err := ParseTime("1.5")
			require.NoError(t, err)
			assert.Equal(t, 1500*time.Millisecond, d)
		})

		t.Run("parses MM:SS format", func(t *testing.T) {
			d, err := ParseTime("1:30")
			require.NoError(t, err)
			assert.Equal(t, 90*time.Second, d)
		})

		t.Run("parses HH:MM:SS format", func(t *testing.T) {
			d, err := ParseTime("1:30:45")
			require.NoError(t, err)
			assert.Equal(t, 1*time.Hour+30*time.Minute+45*time.Second, d)
		})
	})

	t.Run("fail", func(t *testing.T) {
		t.Run("invalid format", func(t *testing.T) {
			_, err := ParseTime("invalid")
			assert.Error(t, err)
		})

		t.Run("empty string", func(t *testing.T) {
			_, err := ParseTime("")
			assert.Error(t, err)
		})

		t.Run("invalid colon format", func(t *testing.T) {
			_, err := ParseTime("1:abc")
			assert.Error(t, err)
		})
	})
}

func TestFormatDurationForFFmpeg(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		t.Run("parses seconds only", func(t *testing.T) {
			d := 90 * time.Second
			s := formatDurationForFFmpeg(d)
			assert.Equal(t, "1:30", s)
		})

		t.Run("parses minutes and seconds", func(t *testing.T) {
			d := 3*time.Minute + 35*time.Second
			s := formatDurationForFFmpeg(d)
			assert.Equal(t, "3:35", s)
		})

		t.Run("parses hours", func(t *testing.T) {
			d := 1*time.Hour + 30*time.Minute + 45*time.Second
			s := formatDurationForFFmpeg(d)
			assert.Equal(t, "1:30:45", s)
		})

		t.Run("parses zero", func(t *testing.T) {
			d := time.Duration(0)
			s := formatDurationForFFmpeg(d)
			assert.Equal(t, "0:00", s)
		})
	})
}

func TestClip(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		t.Run("clip with start and end", func(t *testing.T) {
			clip := Clip{
				Start: 10 * time.Second,
				End:   30 * time.Second,
			}
			assert.Equal(t, 10*time.Second, clip.Start)
			assert.Equal(t, 30*time.Second, clip.End)
		})

		t.Run("clip with only start", func(t *testing.T) {
			clip := Clip{
				Start: 5 * time.Second,
			}
			assert.Equal(t, 5*time.Second, clip.Start)
			assert.Equal(t, time.Duration(0), clip.End)
		})

		t.Run("clip with only end", func(t *testing.T) {
			clip := Clip{
				End: 60 * time.Second,
			}
			assert.Equal(t, time.Duration(0), clip.Start)
			assert.Equal(t, 60*time.Second, clip.End)
		})

		t.Run("empty clip", func(t *testing.T) {
			clip := Clip{}
			assert.Equal(t, time.Duration(0), clip.Start)
			assert.Equal(t, time.Duration(0), clip.End)
		})

		t.Run("clip duration calculation", func(t *testing.T) {
			clip := Clip{
				Start: 10 * time.Second,
				End:   30 * time.Second,
			}
			duration := clip.End - clip.Start
			assert.Equal(t, 20*time.Second, duration)
		})
	})
}

func TestGifSettings(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		t.Run("settings with clip", func(t *testing.T) {
			settings := GifSettings{
				Height: 320,
				FPS:    15,
				Clip: Clip{
					Start: 5 * time.Second,
					End:   15 * time.Second,
				},
			}
			assert.Equal(t, 320, settings.Height)
			assert.Equal(t, 15, settings.FPS)
			assert.Equal(t, 5*time.Second, settings.Clip.Start)
			assert.Equal(t, 15*time.Second, settings.Clip.End)
		})

		t.Run("settings without clip", func(t *testing.T) {
			settings := GifSettings{
				Height: 240,
				FPS:    10,
			}
			assert.Equal(t, 240, settings.Height)
			assert.Equal(t, 10, settings.FPS)
			assert.Equal(t, time.Duration(0), settings.Clip.Start)
		})
	})
}

func TestFirstVideoStream(t *testing.T) {
	stream, err := firstVideoStream(Streams{
		Streams: []Stream{
			{CodecType: "audio"},
			{CodecType: "video", Width: 1280, Height: 720},
		},
	})

	require.NoError(t, err)
	assert.Equal(t, 1280, stream.Width)
	assert.Equal(t, 720, stream.Height)

	_, err = firstVideoStream(Streams{Streams: []Stream{{CodecType: "audio"}}})
	assert.Error(t, err)
}

func TestHasHEVCVideo(t *testing.T) {
	assert.True(t, HasHEVCVideo(Streams{Streams: []Stream{
		{CodecType: "video", CodecName: "h264"},
		{CodecType: "video", CodecName: "hevc"},
	}}))
	assert.True(t, HasHEVCVideo(Streams{Streams: []Stream{
		{CodecType: "video", CodecName: "H265"},
	}}))
	assert.False(t, HasHEVCVideo(Streams{Streams: []Stream{
		{CodecType: "audio", CodecName: "hevc"},
		{CodecType: "video", CodecName: "h264"},
	}}))
}

func TestDiscordVideoBudgetBPS(t *testing.T) {
	withAudio, err := discordVideoBudgetBPS(1.5, false)
	require.NoError(t, err)

	withoutAudio, err := discordVideoBudgetBPS(1.5, true)
	require.NoError(t, err)
	assert.Equal(t, float64(discordAudioBitrateBPS), withoutAudio-withAudio)

	_, err = discordVideoBudgetBPS(100_000, false)
	assert.Error(t, err)
}

func TestSelectDiscordVideoSettings(t *testing.T) {
	video := Stream{CodecType: "video", Width: 1280, Height: 720}

	settings, err := selectDiscordVideoSettings(video, 1_500_000)
	require.NoError(t, err)
	assert.Equal(t, 1280, settings.Width)
	assert.Equal(t, 720, settings.Height)
	assert.False(t, settings.Scale)

	settings, err = selectDiscordVideoSettings(video, 1_000_000)
	require.NoError(t, err)
	assert.Equal(t, 854, settings.Width)
	assert.Equal(t, 480, settings.Height)
	assert.True(t, settings.Scale)

	settings, err = selectDiscordVideoSettings(Stream{CodecType: "video", Width: 720, Height: 1280}, 1_000_000)
	require.NoError(t, err)
	assert.Equal(t, 480, settings.Width)
	assert.Equal(t, 854, settings.Height)

	settings, err = selectDiscordVideoSettings(video, 300_000)
	require.NoError(t, err)
	assert.Equal(t, 426, settings.Width)
	assert.Equal(t, 240, settings.Height)
}
