package dbot

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dbot/pkg/config"
	"dbot/pkg/ffmpeg"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCropRectMapsFractionsOntoTheFrame(t *testing.T) {
	tests := []struct {
		name            string
		width, height   int
		req             cropRequest
		want            ffmpeg.Crop
		wantErrContains string
	}{
		{
			name: "half a 1920x1080 frame", width: 1920, height: 1080,
			req:  cropRequest{X: 0.25, Y: 0.25, W: 0.5, H: 0.5},
			want: ffmpeg.Crop{X: 480, Y: 270, W: 960, H: 540},
		},
		{
			name: "odd sizes are left for the encoder to round down", width: 1079, height: 721,
			req:  cropRequest{X: 0, Y: 0, W: 1, H: 1},
			want: ffmpeg.Crop{X: 0, Y: 0, W: 1079, H: 721},
		},
		{
			name: "a selection dragged off the edge is clamped", width: 100, height: 100,
			req:  cropRequest{X: 0.5, Y: 0.5, W: 0.9, H: 0.9},
			want: ffmpeg.Crop{X: 50, Y: 50, W: 50, H: 50},
		},
		{
			name: "a full frame", width: 640, height: 360,
			req:  cropRequest{X: 0, Y: 0, W: 1, H: 1},
			want: ffmpeg.Crop{X: 0, Y: 0, W: 640, H: 360},
		},
		{
			name: "a slice too thin to encode", width: 1920, height: 1080,
			req:             cropRequest{X: 0.1, Y: 0.1, W: 0.004, H: 0.5},
			wantErrContains: "too small",
		},
		{
			name: "a fraction out of range", width: 1920, height: 1080,
			req:             cropRequest{X: 1.4, Y: 0, W: 0.5, H: 0.5},
			wantErrContains: "fractions",
		},
		{
			name: "a fraction that is not a number", width: 1920, height: 1080,
			req:             cropRequest{X: 0, Y: 0, W: math.Inf(1), H: 0.5},
			wantErrContains: "fractions",
		},
		{
			name: "a height that lands a pixel over the frame", width: 100, height: 100,
			req:  cropRequest{X: 0, Y: 0.5, W: 0.5, H: 0.505},
			want: ffmpeg.Crop{X: 0, Y: 50, W: 50, H: 50},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cropRect(tt.width, tt.height, tt.req)
			if tt.wantErrContains != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrContains)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCropClip(t *testing.T) {
	t.Run("no end means from the start of the trim to the end of the video", func(t *testing.T) {
		clip, err := cropClip(cropRequest{Start: 12.5, End: 0}, time.Minute)
		require.NoError(t, err)
		assert.Equal(t, 12500*time.Millisecond, clip.Start)
		assert.Equal(t, time.Duration(0), clip.End)
	})

	t.Run("a trim longer than a day is refused", func(t *testing.T) {
		// ffprobe reports no duration for some streams, so nothing else bounds it
		_, err := cropClip(cropRequest{Start: 0, End: 1e18}, 0)
		require.ErrorContains(t, err, "between 0 and a day")
	})

	t.Run("seconds become a duration", func(t *testing.T) {
		clip, err := cropClip(cropRequest{Start: 1.5, End: 4.25}, time.Minute)
		require.NoError(t, err)
		assert.Equal(t, 1500*time.Millisecond, clip.Start)
		assert.Equal(t, 4250*time.Millisecond, clip.End)
	})

	t.Run("a trim the page has not touched is no trim", func(t *testing.T) {
		clip, err := cropClip(cropRequest{}, time.Minute)
		require.NoError(t, err)
		assert.Equal(t, ffmpeg.Clip{}, clip)
	})

	t.Run("a start after the end is refused", func(t *testing.T) {
		_, err := cropClip(cropRequest{Start: 30, End: 20}, time.Minute)
		require.ErrorContains(t, err, "after trim start")
	})

	t.Run("a short clip late in the video is refused", func(t *testing.T) {
		// the guard is clip length: ffmpeg gets -ss 30 -t 0.01, which encodes no frames
		_, err := cropClip(cropRequest{Start: 30, End: 30.01}, time.Minute)
		require.ErrorContains(t, err, "trim is too short")
	})

	t.Run("a trim past the end of the video is refused", func(t *testing.T) {
		_, err := cropClip(cropRequest{Start: 0, End: 70}, time.Minute)
		require.ErrorContains(t, err, "past the end")
	})

	t.Run("a trim rounding to nothing is refused", func(t *testing.T) {
		_, err := cropClip(cropRequest{Start: 0, End: 0.001}, time.Minute)
		require.Error(t, err)
	})

	t.Run("a negative start is refused", func(t *testing.T) {
		_, err := cropClip(cropRequest{Start: -5, End: 20}, time.Minute)
		require.Error(t, err)
	})
}

func newTestCropSession() (*DBot, *cropSession, func()) {
	s := &cropSession{
		token:    "t0",
		width:    640,
		height:   360,
		duration: time.Minute,
		format:   "mp4",
		timer:    time.AfterFunc(time.Hour, func() {}),
	}
	cropSess[s.token] = s

	return &DBot{}, s, func() { s.timer.Stop(); delete(cropSess, s.token) }
}

// the page is the only thing the user sees, so its numbers must be the bot's numbers
func TestCropCapCountsLiveSessions(t *testing.T) {
	tokens := []string{"t0", "t1", "t2", "t3"}
	for _, tok := range tokens {
		cropSess[tok] = &cropSession{token: tok, timer: time.AfterFunc(time.Hour, func() {})}
	}
	defer func() {
		for _, tok := range tokens {
			if s, ok := cropSess[tok]; ok {
				s.timer.Stop()
				delete(cropSess, tok)
			}
		}
	}()

	assert.True(t, cropCapReached(), "the cap must bite at max sessions")
	delete(cropSess, "t0")
	assert.False(t, cropCapReached(), "an expired session frees its slot")
}

func TestCropPageShowsTheFrameBeingCropped(t *testing.T) {
	d, _, done := newTestCropSession()
	defer done()

	req := httptest.NewRequest("GET", "/edit/t0", nil)
	req.SetPathValue("token", "t0")
	rec := httptest.NewRecorder()
	d.CropPage(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "640x360")
	assert.Contains(t, body, `src="/edit/t0/video"`)
	assert.Contains(t, body, `fetch("/edit/t0/status")`)

	// the trim, the audio and the format are all picked here now, the command says none of them
	assert.Contains(t, body, `id="mute" type="checkbox"`)
	assert.Contains(t, body, `id="format"`)
	// the label is the page's own copy, the value is what the bot encodes
	assert.Contains(t, body, `<option value="gif">`)
	assert.Contains(t, body, `mute: $("mute").checked`)

	// the crop preview is painted from the frame being played, so a smaller frame is
	// visible before the encode
	assert.Contains(t, body, `id="pv"`)
	assert.Contains(t, body, `drawImage(v, sx, sy, sw, sh`)
	for _, corner := range []string{"nw", "ne", "sw", "se"} {
		assert.Contains(t, body, `id="h-`+corner+`"`)
	}
	// every corner is the one being dragged, the opposite one holds still
	assert.Contains(t, body, "function cornerAt(p)")
	assert.Contains(t, body, `mode = "resize"`)
}

// the page, not the command, picks the format and whether to keep the audio
func TestCropOutputComesFromThePage(t *testing.T) {
	full := cropRequest{X: 0, Y: 0, W: 1, H: 1}
	tests := []struct {
		name     string
		req      cropRequest
		wantForm string
		wantMute bool
		wantErr  string
	}{
		{name: "what the page shows by default", req: full, wantForm: "mp4"},
		{name: "mp4 with the audio muted", req: cropRequest{X: 0, Y: 0, W: 1, H: 1, Mute: true}, wantForm: "mp4", wantMute: true},
		{name: "a gif has no audio to mute", req: cropRequest{X: 0, Y: 0, W: 1, H: 1, Format: "gif", Mute: true}, wantForm: "gif"},
		{name: "a format the page cannot offer", req: cropRequest{X: 0, Y: 0, W: 1, H: 1, Format: "avi"}, wantErr: "unknown format"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, s, done := newTestCropSession()
			defer done()

			_, _, _, err := d.claimCrop(s.token, tt.req)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				assert.Equal(t, cropPending, s.state, "a refused submission must leave the session usable")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantForm, s.format)
			assert.Equal(t, tt.wantMute, s.mute)
		})
	}
}

func TestCropSubmitRejectsWhatItCannotEncode(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		encode     bool
		wantStatus int
		wantErr    string
	}{
		{name: "a slice too thin", body: `{"x":0.1,"y":0,"w":0.001,"h":1}`, wantStatus: http.StatusBadRequest, wantErr: "too small"},
		{name: "a trim the wrong way round", body: `{"x":0,"y":0,"w":1,"h":1,"start":30,"end":20}`, wantStatus: http.StatusBadRequest, wantErr: "after trim start"},
		{name: "junk", body: `{"x":"dupa"}`, wantStatus: http.StatusBadRequest, wantErr: "fractions"},
		{name: "a format the page does not offer", body: `{"x":0,"y":0,"w":1,"h":1,"format":"avi"}`, wantStatus: http.StatusBadRequest, wantErr: "unknown format"},
		{name: "while still encoding the last one", body: `{"x":0,"y":0,"w":1,"h":1}`, encode: true, wantStatus: http.StatusConflict, wantErr: "still encoding"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, s, done := newTestCropSession()
			defer done()
			if tt.encode {
				s.state = cropEncoding
			}

			req := httptest.NewRequest("POST", "/edit/t0", strings.NewReader(tt.body))
			req.SetPathValue("token", "t0")
			rec := httptest.NewRecorder()
			d.CropSubmit(rec, req)

			require.Equal(t, tt.wantStatus, rec.Code)
			assert.Contains(t, rec.Body.String(), tt.wantErr)
			if !tt.encode {
				assert.Equal(t, cropPending, s.state, "a refused crop must leave the session usable")
			}
		})
	}
}

func TestCropRoutesAnswerGoneForAnExpiredLink(t *testing.T) {
	d := &DBot{}
	for _, tc := range []struct{ method, target string }{
		{"GET", "/edit/nope"},
		{"GET", "/edit/nope/video"},
		{"GET", "/edit/nope/status"},
		{"POST", "/edit/nope"},
	} {
		req := httptest.NewRequest(tc.method, tc.target, strings.NewReader(`{}`))
		req.SetPathValue("token", "nope")
		rec := httptest.NewRecorder()

		switch tc.method {
		case "GET":
			if strings.HasSuffix(tc.target, "/video") {
				d.CropVideo(rec, req)
			} else if strings.HasSuffix(tc.target, "/status") {
				d.CropStatus(rec, req)
			} else {
				d.CropPage(rec, req)
			}
		default:
			d.CropSubmit(rec, req)
		}

		require.Equal(t, http.StatusNotFound, rec.Code, tc.target)
		assert.Contains(t, rec.Body.String(), cropGoneMessage)
	}
}

// the link is the only thing a browser has to be able to open, so PUBLIC_URL is checked
// where the link is built; url.JoinPath drops the path of a base with no host
func TestEditLinkComesFromPublicURL(t *testing.T) {
	old := config.PUBLIC_URL
	defer func() { config.PUBLIC_URL = old }()

	info := ffmpeg.Streams{Streams: []ffmpeg.Stream{{CodecType: "video", Width: 640, Height: 360}}}

	for _, tc := range []struct{ pub, want string }{
		{"http://host:58008", "http://host:58008/edit/"},
		{"http://host/", "http://host/edit/"},
		{"https://ex.com/dbot", "https://ex.com/dbot/edit/"},
	} {
		config.PUBLIC_URL = tc.pub

		s, link, err := (&DBot{}).newCropSession(cropSessionParams{Dir: t.TempDir(), File: "v.mp4", Info: info})
		require.NoError(t, err)
		s.timer.Stop()
		defer func() { delete(cropSess, s.token) }()

		assert.True(t, strings.HasPrefix(link, tc.want), "%s gave %s", tc.pub, link)
		assert.Contains(t, link, s.token)
	}

	for _, pub := range []string{"", "localhost:58008", "dupa"} {
		config.PUBLIC_URL = pub

		_, _, err := (&DBot{}).newCropSession(cropSessionParams{Dir: t.TempDir(), File: "v.mp4", Info: info})
		assert.Error(t, err, pub)
	}
}
