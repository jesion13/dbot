package ffmpeg

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

var probeCMD = []string{
	"-v", "error",
	"-show_streams",
	"-show_format",
	"-print_format", "json",
}

type StringInt int

func (i *StringInt) UnmarshalJSON(b []byte) error {
	var s string
	err := json.Unmarshal(b, &s)
	if err != nil {
		return err
	}

	v, err := strconv.Atoi(s)
	if err != nil {
		return err
	}

	*i = StringInt(v)
	return nil
}

type MaybeTimeDuration struct {
	time.Duration
}

func (t *MaybeTimeDuration) UnmarshalJSON(b []byte) error {
	var s string
	err := json.Unmarshal(b, &s)
	if err != nil {
		return err
	}

	v, err := time.ParseDuration(s)
	if err != nil {
		// assume seconds
		v, err = time.ParseDuration(s + "s")
		if err != nil {
			return err
		}
	}

	*t = MaybeTimeDuration{v}
	return nil
}

type (
	Streams struct {
		Streams []Stream `json:"streams"`
		// getting format since video strams don't always have duration and bitrate for some reason
		Format struct {
			Filename string            `json:"filename"`
			Duration MaybeTimeDuration `json:"duration"`
			Size     StringInt         `json:"size"`
			BitRate  StringInt         `json:"bit_rate"`
		}
	}
	Stream struct {
		CodecName      string            `json:"codec_name"`
		Profile        string            `json:"profile"`
		CodecType      string            `json:"codec_type"`
		CodecTagString string            `json:"codec_tag_string"`
		Width          int               `json:"width,omitempty"`
		Height         int               `json:"height,omitempty"`
		CodedWidth     int               `json:"coded_width,omitempty"`
		CodedHeight    int               `json:"coded_height,omitempty"`
		PixFmt         string            `json:"pix_fmt,omitempty"`
		Level          int               `json:"level,omitempty"`
		ColorRange     string            `json:"color_range,omitempty"`
		StartTime      string            `json:"start_time"`
		DurationTs     int               `json:"duration_ts"`
		Duration       MaybeTimeDuration `json:"duration"`
		BitRate        StringInt         `json:"bit_rate"`

		SampleFmt      string `json:"sample_fmt,omitempty"`
		SampleRate     string `json:"sample_rate,omitempty"`
		Channels       int    `json:"channels,omitempty"`
		ChannelLayout  string `json:"channel_layout,omitempty"`
		BitsPerSample  int    `json:"bits_per_sample,omitempty"`
		InitialPadding int    `json:"initial_padding,omitempty"`
	}
)

func Probe(path string) (Streams, error) {
	cmd := exec.Command("ffprobe", append(probeCMD, path)...)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	var meta Streams
	slog.Debug("Probe", slog.String("cmd", cmd.String()), slog.String("link", path))
	err := cmd.Run()
	if err != nil {
		b, _ := io.ReadAll(stderr)
		return meta, errors.New(string(b))
	}

	err = json.NewDecoder(stdout).Decode(&meta)
	if err != nil {
		return meta, err
	}

	return meta, nil
}

func HasHEVCVideo(info Streams) bool {
	for _, stream := range info.Streams {
		if stream.CodecType != "video" {
			continue
		}

		if strings.EqualFold(stream.CodecName, "hevc") || strings.EqualFold(stream.CodecName, "h265") {
			return true
		}
	}

	return false
}
