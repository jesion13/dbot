package ytdlp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"dbot/pkg/config"

	"golang.org/x/net/html"
)

type YTDLP struct{}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

var ErrFailedToDownload = errors.New("yt-dlp: failed to download")

const (
	ytdlp          = "yt-dlp"
	printAfterMove = "after_move:%(.{id,title,vcodec,filepath,ext,original_url})j"
)

var playlistInfoCMD, videoDownloadCMD, videoDownloadSmallCMD, audioDownloadCMD, titleCMD []string

// Filenames are the id, never the title. A title can be long enough that the name
// exceeds the 255 byte limit every filesystem enforces, and it is not unique - two
// different Discord uploads both called Tramwaj.mp4 produced the same name and
// overwrote each other. The title travels in VideoMeta and is stored in the database.
//
// --trim-filenames is gone as a result: with no title in the name there is nothing to
// trim, and leaving it set would silently truncate ids instead.
const outputTemplate = "%(id)s.%(ext)s"

// AV1 first, then whatever else exists. AV1 is around 30% smaller than H.264 at the
// same visual quality, and the stored file is decoded to mono opus for playback, so
// the extra resolution of a non-AV1 alternative is never seen.
const formatBest = "bv*[vcodec=av01]+ba/bv*+ba/b"

func init() {
	go func() {
		<-config.Ctx.Done()
		audioDownloadCMD = []string{
			"--no-simulate",
			"--print", printAfterMove,
			"-x",
			"--no-playlist",
			"--audio-format",
			"opus",
			"-o", outputTemplate,
		}
		videoDownloadCMD = []string{
			"--no-simulate",
			"--print", printAfterMove,
			"--no-playlist",
			"-f",
			formatBest,
			"-o", outputTemplate,
		}
		videoDownloadSmallCMD = []string{
			"--no-simulate",
			"--print", printAfterMove,
			"--no-playlist",
			"-f",
			`bv*[filesize<8M]+ba[filesize<2M]/b[filesize<10M]/bv*[height<=720]+ba/b[height<=720]/b`,
			"--format-sort", "res,fps~30,codec:av01",
			"-o", outputTemplate,
		}

		// metadata only: one title per line, nothing downloaded
		titleCMD = []string{
			"--skip-download",
			"--no-playlist",
			"--no-colors",
			"--print", "%(title)s",
		}

		playlistInfoCMD = []string{
			"--skip-download",
			"--flat-playlist",
			"--dump-single-json",
			"--no-colors",
		}
	}()
}

type VideoMeta struct {
	// ID is yt-dlp's per-extractor id. It is a real, globally unique id for YouTube,
	// but for direct links the generic extractor derives it from the URL filename, so
	// it can collide between different videos and is not stable for the same one.
	ID string `json:"id"`
	// VCodec is the video codec of the downloaded file, e.g. "av01..." or "vp9".
	// The caller uses it to decide whether a re-encode is needed at all.
	VCodec      string `json:"vcodec"`
	Title       string `json:"title"`
	Ext         string `json:"ext"`
	Filepath    string `json:"filepath"`
	OriginalURL string `json:"original_url"`
}

// Cookies are domain-scoped, so match the parsed hostname, not a url substring:
// youtu.be.attacker.example must get no YouTube cookies.
func cookiesFor(link string) (string, []string) {
	u, err := url.Parse(link)
	if err != nil {
		return "", nil
	}
	host := strings.ToLower(u.Hostname())
	if host == "youtu.be" || strings.HasSuffix(host, ".youtu.be") ||
		host == "youtube.com" || strings.HasSuffix(host, ".youtube.com") ||
		host == "youtube-nocookie.com" || strings.HasSuffix(host, ".youtube-nocookie.com") {
		return config.YT_COOKIE_PATH, nil
	}
	if host == "reddit.com" || strings.HasSuffix(host, ".reddit.com") {
		// reddit 403s raw TLS fingerprints ("IP unable to access the API");
		// impersonate the browser the cookies come from — upstream thread
		// says the Chrome target gets a captcha, Firefox doesn't
		return config.REDDIT_COOKIE_PATH, []string{"--impersonate", "firefox"}
	}
	return "", nil
}

func injectCookies(url string, args []string) []string {
	p, extra := cookiesFor(url)
	if p == "" {
		return args
	}
	args = append(args, "--cookies", filepath.Join(must(os.Getwd()), "prod-data", p))
	return append(args, extra...)
}

func (YTDLP) DownloadAudio(link string) (VideoMeta, error) {
	link, err := parseSpecialLinks(link)
	if err != nil {
		return VideoMeta{}, errors.Join(errors.New("Special Links Parser"), err)
	}

	cmd := exec.Command(ytdlp, append(injectCookies(link,audioDownloadCMD), "--", link)...)
	cmd.Dir = config.TMP_PATH

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	var meta VideoMeta
	slog.Debug("DownloadAudio", slog.String("cmd", cmd.String()), slog.String("link", link))
	err = cmd.Run()
	if err != nil {
		b, _ := io.ReadAll(stderr)
		return meta, errors.Join(ErrFailedToDownload, errors.New(string(b)))
	}

	err = json.NewDecoder(stdout).Decode(&meta)
	if err != nil {
		return meta, err
	}

	return meta, nil
}

func (YTDLP) DownloadVideo(ctx context.Context, link string) (VideoMeta, error) {
	tmpDir, ok := ctx.Value(config.DirKey).(string)
	if !ok || len(tmpDir) == 0 {
		return VideoMeta{}, errors.New("nie dałeś temp dira debilu")
	}

	link, err := parseSpecialLinks(link)
	if err != nil {
		return VideoMeta{}, errors.Join(errors.New("Special Links Parser"), err)
	}

	cmd := exec.Command(ytdlp, append(injectCookies(link,videoDownloadCMD), "--", link)...)
	cmd.Dir = string(tmpDir)

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	var meta VideoMeta
	slog.Debug("DownloadVideo", slog.String("cmd", cmd.String()), slog.String("link", link))
	err = cmd.Run()
	if err != nil {
		b, _ := io.ReadAll(stderr)
		return meta, errors.Join(ErrFailedToDownload, errors.New(string(b)))
	}

	err = json.NewDecoder(stdout).Decode(&meta)
	if err != nil {
		return meta, err
	}

	return meta, nil
}

func (YTDLP) DownloadVideoSmall(ctx context.Context, link string) (VideoMeta, error) {
	tmpDir, ok := ctx.Value(config.DirKey).(string)
	if !ok || len(tmpDir) == 0 {
		return VideoMeta{}, errors.New("nie dałeś temp dira debilu")
	}

	link, err := parseSpecialLinks(link)
	if err != nil {
		return VideoMeta{}, errors.Join(errors.New("Special Links Parser"), err)
	}

	cmd := exec.Command(ytdlp, append(injectCookies(link,videoDownloadSmallCMD), "--", link)...)
	cmd.Dir = string(tmpDir)

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	var meta VideoMeta
	slog.Debug("DownloadVideo", slog.String("cmd", cmd.String()), slog.String("link", link))
	err = cmd.Run()
	if err != nil {
		b, _ := io.ReadAll(stderr)
		return meta, errors.Join(ErrFailedToDownload, errors.New(string(b)))
	}

	err = json.NewDecoder(stdout).Decode(&meta)
	if err != nil {
		return meta, err
	}

	return meta, nil
}

// Title asks yt-dlp for a video's title without downloading it. Nothing else knows the
// title of a video that is not in the artefact cache, so this is how an uncached /play
// stops queueing as (untitled).
func (YTDLP) Title(ctx context.Context, link string) (string, error) {
	link, err := parseSpecialLinks(link)
	if err != nil {
		return "", errors.Join(errors.New("Special Links Parser"), err)
	}

	cmd := exec.CommandContext(ctx, ytdlp, append(injectCookies(link,titleCMD), "--", link)...)
	cmd.Dir = config.TMP_PATH

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	slog.Debug("Title", slog.String("cmd", cmd.String()), slog.String("link", link))
	if err := cmd.Run(); err != nil {
		b, _ := io.ReadAll(stderr)
		return "", errors.Join(ErrFailedToDownload, errors.New(string(b)))
	}

	// --no-playlist keeps it to one line, but a newline can still sneak into a title
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	return strings.TrimSpace(lines[len(lines)-1]), nil
}

type PlaylistMeta struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	WebpageURL string `json:"webpage_url"`
	Entries    []struct {
		ID       string `json:"id"`
		URL      string `json:"url"`
		Title    string `json:"title"`
		Duration *int   `json:"duration"`
	} `json:"entries"`
}

func (YTDLP) PlaylistInfo(link string) (PlaylistMeta, error) {
	cmd := exec.Command(ytdlp, append(injectCookies(link,playlistInfoCMD), "--", link)...)
	cmd.Dir = config.TMP_PATH

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	var meta PlaylistMeta
	slog.Debug("PlaylistInfo", slog.String("cmd", cmd.String()), slog.String("link", link))
	err := cmd.Run()
	if err != nil {
		b, _ := io.ReadAll(stderr)
		return meta, errors.Join(ErrFailedToDownload, errors.New(string(b)))
	}

	err = json.NewDecoder(stdout).Decode(&meta)
	if err != nil {
		return meta, err
	}

	return meta, nil
}

func parseSpecialLinks(rawUrl string) (string, error) {
	u, err := url.Parse(rawUrl)
	if err != nil {
		return rawUrl, fmt.Errorf("Error parsing url(%s): %w", rawUrl, err)
	}
	if !strings.HasSuffix(u.Host, "jbzd.com.pl") {
		return rawUrl, nil
	}
	resp, err := http.Head(rawUrl)
	if err != nil {
		return rawUrl, fmt.Errorf("Error making web (HEAD) request url(%s): %w", rawUrl, err)
	}
	defer resp.Body.Close()
	// Get returns "" when the header is absent (a 204 or a bodiless redirect has
	// none), which the old resp.Header["Content-Type"][0] turned into a panic
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "text/html") {
		return rawUrl, nil
	}
	resp, err = http.Get(rawUrl)
	if err != nil {
		return rawUrl, fmt.Errorf("Error making web (GET) request url(%s): %w", rawUrl, err)
	}
	defer resp.Body.Close()

	doc, err := html.Parse(resp.Body)
	if err != nil {
		return rawUrl, fmt.Errorf("Error parsing body: %w", err)
	}
	for n := range doc.Descendants() {
		if n.Type == html.ElementNode && n.Data == "videoplyr" {
			for _, a := range n.Attr {
				if a.Key != "video_url" {
					continue
				}
				return a.Val, nil
			}
		}
	}

	return rawUrl, errors.New("Unknown error")
}
