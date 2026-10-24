package dbot

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/template"
	"time"

	"dbot/pkg/config"
	"dbot/pkg/ffmpeg"

	"github.com/disgoorg/snowflake/v2"
)

//go:embed web
var cropWeb embed.FS

const (
	// how long a crop link works, and with it how long its download sits on disk
	CropSessionTTL = 30 * time.Minute
	// after a finished encode the link stays open for another round for a while, so a
	// user can iterate without paying for the download twice
	cropIterationTTL = 10 * time.Minute
	// a watchdog for a wedged ffmpeg, not a cap on real work: a 2-pass encode of a long
	// trim legitimately runs for tens of minutes, and the Discord size budget rejects
	// anything over roughly 55 minutes long before we even start
	cropEncodeTimeout = time.Hour
	// each session holds a download, and each encode is a serial CPU hog
	CropMaxSessions = 4
	cropMaxStreams  = 2
	cropMaxBody     = 1 << 10
	minCropPx       = 16
	// the page reports fractions, so the axes cannot be mixed up by a window resize
	minCropFraction = 0.01
	cropGoneMessage = "this edit link is gone, run /to-mp4 again"
)

var (
	errCropGone = errors.New(cropGoneMessage)
	errCropBusy = errors.New("still encoding your last crop")

	// one bot per process, and the encode limit caps the box, not one bot's work, so
	// neither of these lives on DBot
	cropMu      sync.Mutex
	cropSess    = map[string]*cropSession{}
	cropEncodeC = make(chan struct{}, cropMaxStreams)

	cropPageTmpl = template.Must(template.ParseFS(cropWeb, "web/crop.html.tmpl"))
)

type cropState int

const (
	cropPending cropState = iota
	cropEncoding
	cropExpired
)

// everything the result post needs, captured while the interaction is still alive:
// by the time the user submits, its token is long dead
type transcodeTarget struct {
	ChannelID snowflake.ID
	Username  string
	AvatarURL string
}

type cropSession struct {
	token    string
	dir      string
	file     string
	width    int
	height   int
	duration time.Duration
	target   transcodeTarget
	// format and mute are the page's choice, sent with the submission;
	// the session starts with what the page shows by default
	format string
	mute   bool

	timer *time.Timer

	mu      sync.Mutex
	state   cropState
	lastErr string
	submits int
}

type cropSessionParams struct {
	Dir    string
	File   string
	Info   ffmpeg.Streams
	Target transcodeTarget
}

// CropLink is what the user gets: a URL that only this process knows, valid until the
// session expires. PUBLIC_URL has to be reachable from the user's browser, which is
// whoever exposes :58008, not the bot.
func cropLink(token string) string {
	return strings.TrimRight(config.PUBLIC_URL, "/") + "/edit/" + token
}

func cropToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to make a crop token: %w", err)
	}

	return hex.EncodeToString(b), nil
}

func (d *DBot) newCropSession(p cropSessionParams) (*cropSession, string, error) {
	if p.Dir == "" {
		return nil, "", errors.New("a crop session needs a directory of its own")
	}
	if config.PUBLIC_URL == "" {
		return nil, "", errors.New("PUBLIC_URL is not set, so /to-mp4 edit:true has no link to hand out")
	}

	width, height, err := ffmpeg.DisplaySizeOf(p.Info)
	if err != nil {
		return nil, "", err
	}

	token, err := cropToken()
	if err != nil {
		return nil, "", err
	}

	s := &cropSession{
		token:    token,
		dir:      p.Dir,
		file:     p.File,
		width:    width,
		height:   height,
		duration: p.Info.Format.Duration.Duration,
		target:   p.Target,
		format:   "mp4",
	}

	// armed before the session is reachable, so nothing can ever see one without a timer
	s.timer = time.AfterFunc(CropSessionTTL, func() { d.expireCrop(token) })

	cropMu.Lock()
	if len(cropSess) >= CropMaxSessions {
		cropMu.Unlock()
		return nil, "", fmt.Errorf("too many edits in flight (max %d), try again in a bit", CropMaxSessions)
	}
	cropSess[token] = s
	cropMu.Unlock()

	return s, token, nil
}

func (d *DBot) lookupCrop(token string) (*cropSession, bool) {
	cropMu.Lock()
	defer cropMu.Unlock()
	s, ok := cropSess[token]

	return s, ok
}

// expireCrop drops the session and its download. An encode that is still running owns
// the directory, so it is never pulled out from under it.
func (d *DBot) expireCrop(token string) {
	cropMu.Lock()
	s, ok := cropSess[token]
	if !ok {
		cropMu.Unlock()
		return
	}

	s.mu.Lock()
	encoding := s.state == cropEncoding
	if !encoding {
		s.state = cropExpired
	}
	s.mu.Unlock()

	if encoding {
		cropMu.Unlock()
		return
	}
	delete(cropSess, token)
	cropMu.Unlock()

	if err := os.RemoveAll(s.dir); err != nil {
		slog.Error("failed to remove crop dir", slog.String("dir", s.dir), slog.Any("error", err))
	}
}

// finishCrop hands the session back for another round, or records why this one failed.
func (d *DBot) finishCrop(token string, err error) {
	s, ok := d.lookupCrop(token)
	if !ok {
		return
	}

	s.mu.Lock()
	s.state = cropPending
	if err != nil {
		s.lastErr = err.Error()
	} else {
		s.lastErr = ""
	}
	// submit stopped the session timer; arm a shorter one now that the work is done
	s.timer = time.AfterFunc(cropIterationTTL, func() { d.expireCrop(token) })
	s.mu.Unlock()
}

// cropRect maps the page's fractions onto the frame ffmpeg crops, which is the displayed
// (rotation applied) frame. Sizes are not forced even here: normalizeCrop does that
// right before the encode, so both callers of the crop filters get the same rule.
func cropRect(width, height int, req cropRequest) (ffmpeg.Crop, error) {
	for _, v := range []float64{req.X, req.Y, req.W, req.H} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return ffmpeg.Crop{}, fmt.Errorf("crop values must be fractions between 0 and 1")
		}
	}
	if req.W < minCropFraction || req.H < minCropFraction {
		return ffmpeg.Crop{}, fmt.Errorf("selection is too small, drag a bigger area")
	}

	// a resized window can hand back 1.000001
	if req.X+req.W > 1 {
		req.W = 1 - req.X
	}
	if req.Y+req.H > 1 {
		req.H = 1 - req.Y
	}

	c := ffmpeg.Crop{
		X: int(math.Round(req.X * float64(width))),
		Y: int(math.Round(req.Y * float64(height))),
		W: int(math.Round(req.W * float64(width))),
		H: int(math.Round(req.H * float64(height))),
	}
	if c.X+c.W > width {
		c.W = width - c.X
	}
	if c.Y+c.H > height {
		c.H = height - c.Y
	}
	if c.W < minCropPx || c.H < minCropPx {
		return ffmpeg.Crop{}, fmt.Errorf("selection is too small, at least %d pixels", minCropPx)
	}

	return c, nil
}

// cropClip takes the trim the page settled on. End of 0 means the end of the whole
// video, which is also what an untouched page sends before it has a duration to work with.
func cropClip(req cropRequest, duration time.Duration) (ffmpeg.Clip, error) {
	// a stream ffprobe cannot price comes back with no duration, so the span needs a
	// ceiling of its own before it is turned into a time.Duration
	if !trimSeconds(req.Start) || !trimSeconds(req.End) {
		return ffmpeg.Clip{}, fmt.Errorf("trim must be a number of seconds between 0 and a day")
	}

	clip := ffmpeg.Clip{Start: time.Duration(req.Start * float64(time.Second))}
	if req.End <= 0 {
		return clip, nil
	}

	clip.End = time.Duration(req.End * float64(time.Second))
	if clip.End <= clip.Start {
		return ffmpeg.Clip{}, fmt.Errorf("trim end must be after trim start")
	}
	if clip.End < 500*time.Millisecond {
		return ffmpeg.Clip{}, fmt.Errorf("trim is too short")
	}
	// the browser and ffprobe round the duration differently
	if dur := duration.Seconds(); dur > 0 && req.End > dur+0.5 {
		return ffmpeg.Clip{}, fmt.Errorf("trim end is past the end of the video (%.1fs)", dur)
	}

	return clip, nil
}

func trimSeconds(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 24*60*60
}

// claimCrop validates a submission and takes ownership of the session for one encode,
// so a double click cannot start two.
func (d *DBot) claimCrop(token string, req cropRequest) (*cropSession, ffmpeg.Crop, ffmpeg.Clip, error) {
	s, ok := d.lookupCrop(token)
	if !ok {
		return nil, ffmpeg.Crop{}, ffmpeg.Clip{}, errCropGone
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == cropEncoding {
		return nil, ffmpeg.Crop{}, ffmpeg.Clip{}, errCropBusy
	}
	if s.state == cropExpired {
		return nil, ffmpeg.Crop{}, ffmpeg.Clip{}, errCropGone
	}

	rect, err := cropRect(s.width, s.height, req)
	if err != nil {
		return nil, ffmpeg.Crop{}, ffmpeg.Clip{}, err
	}
	clip, err := cropClip(req, s.duration)
	if err != nil {
		return nil, ffmpeg.Crop{}, ffmpeg.Clip{}, err
	}
	format, mute, err := cropOutput(req)
	if err != nil {
		return nil, ffmpeg.Crop{}, ffmpeg.Clip{}, err
	}

	s.state = cropEncoding
	s.lastErr = ""
	s.format, s.mute = format, mute
	// the encode owns the directory now: a 2-pass encode of a long trim outlives the
	// session timer, and losing the download mid-encode would be worse than a late link
	s.timer.Stop()
	s.submits++

	return s, rect, clip, nil
}

// runCrop encodes what the user picked and posts it. It runs detached from the HTTP
// request: the encode is the slow part and the browser only polls for the outcome.
func (d *DBot) runCrop(s *cropSession, rect ffmpeg.Crop, clip ffmpeg.Clip) {
	var err error
	defer func() { d.finishCrop(s.token, err) }()

	// the timeout covers the wait for a free slot as well, so a queue cannot pin a session
	// and its download with no end in sight
	ctx, cancel := context.WithTimeout(context.Background(), cropEncodeTimeout)
	defer cancel()

	select {
	case cropEncodeC <- struct{}{}:
		defer func() { <-cropEncodeC }()
	case <-ctx.Done():
		err = fmt.Errorf("too many edits queued: %w", ctx.Err())
		return
	}
	ctx = context.WithValue(ctx, config.DirKey, s.dir)

	var f *os.File
	if s.format == "gif" {
		f, err = ffmpeg.ToDiscordGIF(ctx, s.file, clip, rect)
	} else {
		f, err = ffmpeg.ToDiscordMP4(ctx, s.file, s.mute, clip, rect)
	}
	if err == nil {
		defer f.Close()
		if s.format == "gif" {
			var stat os.FileInfo
			if stat, err = f.Stat(); err == nil && stat.Size() >= ffmpeg.DiscordMaxFileSizeBytes {
				err = errors.New("file to big, shorten the clip or the crop")
			}
		}
		if err == nil {
			// discord infers how to render the file from its extension, same as the plain
			// /to-mp4 result
			fileName := "dupa.mp4"
			if s.format == "gif" {
				fileName = "dupa.webp"
			}
			err = d.postTranscodedFile(ctx, s.target, fileName, f)
		}
	}

	if err != nil {
		slog.Error("crop failed", slog.String("file", s.file), slog.Any("error", err))
		d.reportCropFailure(s, err)
	}
}

func (d *DBot) reportCropFailure(s *cropSession, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	note := "edit failed: " + truncate(err.Error())
	if postErr := d.postTranscodeNote(ctx, s.target, note); postErr != nil {
		slog.Error("failed to report a failed crop", slog.Any("error", postErr))
	}
}

// cropDirs are left behind when the bot dies with links still open
func cleanupCropDirs() {
	dirs, err := filepath.Glob(filepath.Join(config.TMP_PATH, "crop*"))
	if err != nil {
		return
	}
	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			slog.Error("failed to remove a stale crop dir", slog.String("dir", dir), slog.Any("error", err))
		}
	}
}

func (d *DBot) cropSessionOf(w http.ResponseWriter, r *http.Request) (*cropSession, bool) {
	s, ok := d.lookupCrop(r.PathValue("token"))
	if !ok {
		http.Error(w, cropGoneMessage, http.StatusNotFound)
		return nil, false
	}

	return s, true
}

func (d *DBot) CropPage(w http.ResponseWriter, r *http.Request) {
	s, ok := d.cropSessionOf(w, r)
	if !ok {
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := struct {
		Path   string
		Width  int
		Height int
	}{
		Path:   "/edit/" + s.token,
		Width:  s.width,
		Height: s.height,
	}
	if err := cropPageTmpl.ExecuteTemplate(w, "crop.html.tmpl", data); err != nil {
		slog.Error("failed to render the crop page", slog.Any("error", err))
	}
}

func (d *DBot) CropVideo(w http.ResponseWriter, r *http.Request) {
	s, ok := d.cropSessionOf(w, r)
	if !ok {
		return
	}

	f, err := os.Open(s.file)
	if err != nil {
		http.Error(w, cropGoneMessage, http.StatusNotFound)
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		http.Error(w, cropGoneMessage, http.StatusNotFound)
		return
	}

	// ServeContent answers Range, which is what lets the page seek
	http.ServeContent(w, r, filepath.Base(s.file), stat.ModTime(), f)
}

func (d *DBot) CropStatus(w http.ResponseWriter, r *http.Request) {
	s, ok := d.cropSessionOf(w, r)
	if !ok {
		return
	}

	s.mu.Lock()
	state, note := s.state, s.lastErr
	s.mu.Unlock()

	out := struct {
		State string `json:"state"`
		Error string `json:"error"`
	}{State: "pending", Error: note}
	if state == cropEncoding {
		out.State = "encoding"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

type cropRequest struct {
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	W     float64 `json:"w"`
	H     float64 `json:"h"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Mute  bool    `json:"mute"`
	// "" is mp4; a gif carries no audio, so mute is ignored for it
	Format string `json:"format"`
}

// cropOutput is what the encode will be: the page picks the format and whether to keep
// the audio, so both are only known when the user submits.
func cropOutput(req cropRequest) (string, bool, error) {
	switch req.Format {
	case "", "mp4":
		return "mp4", req.Mute, nil
	case "gif":
		return "gif", false, nil
	}

	return "", false, fmt.Errorf("unknown format %q, the page offers mp4 and gif", req.Format)
}

func (d *DBot) CropSubmit(w http.ResponseWriter, r *http.Request) {
	req, err := decodeCropRequest(w, r)
	if err != nil {
		http.Error(w, "send {x,y,w,h} as fractions of the frame, plus start, end, mute and format", http.StatusBadRequest)
		return
	}

	s, rect, clip, err := d.claimCrop(r.PathValue("token"), req)
	if err != nil {
		code := http.StatusBadRequest
		switch {
		case errors.Is(err, errCropGone):
			code = http.StatusNotFound
		case errors.Is(err, errCropBusy):
			code = http.StatusConflict
		}
		http.Error(w, err.Error(), code)
		return
	}

	go d.runCrop(s, rect, clip)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"state": "encoding"})
}

func decodeCropRequest(w http.ResponseWriter, r *http.Request) (cropRequest, error) {
	var req cropRequest
	r.Body = http.MaxBytesReader(w, r.Body, cropMaxBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return req, fmt.Errorf("bad crop body: %w", err)
	}

	return req, nil
}
