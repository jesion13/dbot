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
		// video streams often carry neither duration nor bitrate
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

		// a phone video shot in portrait is stored landscape and carries a rotation
		// matrix; width/height stay the coded (unrotated) size
		SideData []struct {
			SideDataType string `json:"side_data_type"`
			Rotation     int    `json:"rotation"`
		} `json:"side_data_list,omitempty"`

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

// DisplaySize is the picture size the way it is shown, rotation applied. ffmpeg runs
// -vf crop/scale on the already rotated frame (verified on ffmpeg 9: a crop rect that
// only fits the rotated frame succeeds), so anything that picks coordinates or a scale
// target has to use these dims instead of the coded Width/Height.
func (s Stream) DisplaySize() (int, int) {
	for _, sd := range s.SideData {
		if sd.SideDataType != "Display Matrix" {
			continue
		}
		if r := ((sd.Rotation % 180) + 180) % 180; r != 0 {
			return s.Height, s.Width
		}
	}

	return s.Width, s.Height
}

// DisplaySizeOf is the displayed size of the picture a user picks a rect on.
func DisplaySizeOf(info Streams) (int, int, error) {
	stream, err := firstVideoStream(info)
	if err != nil {
		return 0, 0, err
	}

	return stream.Width, stream.Height, nil
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
