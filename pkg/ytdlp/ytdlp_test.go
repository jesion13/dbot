package ytdlp

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"dbot/pkg/config"
	"dbot/pkg/ffmpeg"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSpecialLinks(t *testing.T) {
	t.Run("non-jbzd links pass through unchanged", func(t *testing.T) {
		link := "https://www.youtube.com/watch?v=abc123"
		out, err := parseSpecialLinks(link)
		require.NoError(t, err)
		assert.Equal(t, link, out)
	})

	t.Run("invalid url", func(t *testing.T) {
		_, err := parseSpecialLinks("http://[::1]:namedport")
		assert.Error(t, err)
	})
}

func TestInjectCookies(t *testing.T) {
	tmpl := []string{"--no-simulate"}
	join := func(p string) string { return filepath.Join(must(os.Getwd()), "prod-data", p) }

	oldYT, oldReddit := config.YT_COOKIE_PATH, config.REDDIT_COOKIE_PATH
	t.Cleanup(func() { config.YT_COOKIE_PATH, config.REDDIT_COOKIE_PATH = oldYT, oldReddit })

	t.Run("youtube url gets yt cookies", func(t *testing.T) {
		config.YT_COOKIE_PATH, config.REDDIT_COOKIE_PATH = "./yt.txt", "./reddit.txt"
		out := injectCookies("https://www.youtube.com/watch?v=abc", tmpl)
		assert.Equal(t, []string{"--no-simulate", "--cookies", join("./yt.txt")}, out)
	})

	t.Run("youtu.be url gets yt cookies", func(t *testing.T) {
		config.YT_COOKIE_PATH, config.REDDIT_COOKIE_PATH = "./yt.txt", "./reddit.txt"
		out := injectCookies("https://youtu.be/abc", tmpl)
		assert.Equal(t, []string{"--no-simulate", "--cookies", join("./yt.txt")}, out)
	})

	t.Run("reddit url gets reddit cookies and firefox impersonation", func(t *testing.T) {
		config.YT_COOKIE_PATH, config.REDDIT_COOKIE_PATH = "./yt.txt", "./reddit.txt"
		out := injectCookies("https://www.reddit.com/r/x/comments/abc", tmpl)
		assert.Equal(t, []string{"--no-simulate", "--cookies", join("./reddit.txt"), "--impersonate", "firefox"}, out)
	})

	t.Run("non-youtube, non-reddit url gets no cookies", func(t *testing.T) {
		config.YT_COOKIE_PATH, config.REDDIT_COOKIE_PATH = "./yt.txt", "./reddit.txt"
		out := injectCookies("https://example.com/video.mp4", tmpl)
		assert.Equal(t, []string{"--no-simulate"}, out)
	})

	t.Run("unset cookie path gets no cookies", func(t *testing.T) {
		config.YT_COOKIE_PATH, config.REDDIT_COOKIE_PATH = "", ""
		out := injectCookies("https://old.reddit.com/r/x", tmpl)
		assert.Equal(t, []string{"--no-simulate"}, out)
	})

	t.Run("look-alike host gets no cookies", func(t *testing.T) {
		config.YT_COOKIE_PATH, config.REDDIT_COOKIE_PATH = "./yt.txt", "./reddit.txt"
		out := injectCookies("https://youtu.be.attacker.example/x", tmpl)
		assert.Equal(t, []string{"--no-simulate"}, out)
		out = injectCookies("https://reddit.com.attacker.example/x", tmpl)
		assert.Equal(t, []string{"--no-simulate"}, out)
	})

	t.Run("template slice is not mutated", func(t *testing.T) {
		config.YT_COOKIE_PATH, config.REDDIT_COOKIE_PATH = "./yt.txt", "./reddit.txt"
		injectCookies("https://youtube.com/x", tmpl)
		injectCookies("https://reddit.com/x", tmpl)
		assert.Equal(t, []string{"--no-simulate"}, tmpl)
	})
}

// The small download is uploaded to Discord as it came, so its size budget must come
// from the same constant ffmpeg re-encoding uses, not a hand-written number.
func TestSmallFormatTracksDiscordLimit(t *testing.T) {
	assert.Equal(t, ffmpeg.DiscordMaxFileSizeBytes>>20, maxMiB)
	assert.Equal(t, maxMiB, videoMiB+audioMiB, "video+audio budget must equal the limit")
	assert.Equal(t,
		fmt.Sprintf("bv*[filesize<%dM]+ba[filesize<%dM]/b[filesize<%dM]/bv*[height<=720]+ba/b[height<=720]/b", videoMiB, audioMiB, maxMiB),
		smallFormat)
}
