package encode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// mediaInfo is what ffprobe reports about a source file.
type mediaInfo struct {
	Duration time.Duration // 0 if unknown
	Streams  []streamInfo
	Tags     map[string]string // container tags, keys in upper case
}

type streamInfo struct {
	Index      int
	Type       string // video, audio, subtitle, attachment or data
	Codec      string
	Channels   int
	SampleRate int
	Width      int
	Height     int
	PixFmt     string // e.g. yuv420p10le
	FrameRate  string // e.g. 24000/1001
	Level      int    // for AV1, seq_level_idx
	BitRate    int
	Duration   time.Duration // how long this stream runs; 0 if the file doesn't say
	Language   string        // as tagged, e.g. "eng" or "en-US"; empty if untagged
	Title      string

	CoverArt        bool // a still image attached as a video stream
	Default         bool
	Forced          bool
	HearingImpaired bool
	Commentary      bool // flagged as commentary or audio description

	FieldOrder               string // "progressive", or tt, bb, tb or bt when interlaced
	ColorPrimaries, ColorTrc string
	ColorSpace, ColorRange   string
	Filename, Mimetype       string // attachments must keep these, or the file can't be written
}

// Interlaced reports whether the video needs deinterlacing.
func (st streamInfo) Interlaced() bool {
	switch st.FieldOrder {
	case "tt", "bb", "tb", "bt":
		return true
	}
	return false
}

func probe(ctx context.Context, in string) (mediaInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-show_entries", "format=duration:format_tags"+
			":stream=index,codec_type,codec_name,channels,sample_rate,width,height,pix_fmt,level,bit_rate,duration"+
			",r_frame_rate,avg_frame_rate,field_order,color_primaries,color_transfer,color_space,color_range"+
			":stream_tags:stream_disposition",
		"-of", "json",
		in,
	).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(bytes.TrimSpace(ee.Stderr)) > 0 {
			lines := strings.Split(strings.TrimSpace(string(ee.Stderr)), "\n")
			msg := strings.TrimSpace(lines[len(lines)-1])
			return mediaInfo{}, errors.New(strings.TrimPrefix(msg, in+": ")) // the UI already shows which file
		}
		if ctx.Err() != nil {
			return mediaInfo{}, errors.New("ffprobe took too long reading the file")
		}
		return mediaInfo{}, fmt.Errorf("ffprobe: %w", err)
	}

	var raw struct {
		Format struct {
			Duration string            `json:"duration"`
			Tags     map[string]string `json:"tags"`
		} `json:"format"`
		Streams []struct {
			Index          int               `json:"index"`
			CodecType      string            `json:"codec_type"`
			CodecName      string            `json:"codec_name"`
			Channels       int               `json:"channels"`
			SampleRate     string            `json:"sample_rate"`
			Width          int               `json:"width"`
			Height         int               `json:"height"`
			PixFmt         string            `json:"pix_fmt"`
			Level          int               `json:"level"`
			BitRate        string            `json:"bit_rate"`
			Duration       string            `json:"duration"`
			FrameRate      string            `json:"r_frame_rate"`
			AvgFrameRate   string            `json:"avg_frame_rate"`
			FieldOrder     string            `json:"field_order"`
			ColorPrimaries string            `json:"color_primaries"`
			ColorTransfer  string            `json:"color_transfer"`
			ColorSpace     string            `json:"color_space"`
			ColorRange     string            `json:"color_range"`
			Tags           map[string]string `json:"tags"`
			Disposition    map[string]int    `json:"disposition"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return mediaInfo{}, fmt.Errorf("reading ffprobe output: %w", err)
	}

	info := mediaInfo{Duration: seconds(raw.Format.Duration), Tags: map[string]string{}}
	for k, v := range raw.Format.Tags {
		info.Tags[strings.ToUpper(k)] = v
	}
	for _, s := range raw.Streams {
		tags := map[string]string{}
		for k, v := range s.Tags {
			tags[strings.ToUpper(k)] = v
		}
		lang := tags["LANGUAGE"]
		if lang == "und" {
			lang = ""
		}
		bitRate, _ := strconv.Atoi(s.BitRate)
		sampleRate, _ := strconv.Atoi(s.SampleRate)
		rate := s.FrameRate
		if fps, ok := ratio(rate); !ok || fps > 120 { // a timebase rather than a frame rate
			rate = s.AvgFrameRate
		}
		st := streamInfo{
			Index:           s.Index,
			Type:            s.CodecType,
			Codec:           s.CodecName,
			Channels:        s.Channels,
			SampleRate:      sampleRate,
			Width:           s.Width,
			Height:          s.Height,
			PixFmt:          s.PixFmt,
			FrameRate:       rate,
			Level:           s.Level,
			BitRate:         bitRate,
			Duration:        seconds(s.Duration),
			Language:        lang,
			Title:           tags["TITLE"],
			CoverArt:        s.Disposition["attached_pic"] == 1,
			Default:         s.Disposition["default"] == 1,
			Forced:          s.Disposition["forced"] == 1,
			HearingImpaired: s.Disposition["hearing_impaired"] == 1 || s.Disposition["captions"] == 1,
			Commentary:      s.Disposition["comment"] == 1 || s.Disposition["visual_impaired"] == 1 || s.Disposition["descriptions"] == 1,
			FieldOrder:      s.FieldOrder,
			ColorPrimaries:  s.ColorPrimaries,
			ColorTrc:        s.ColorTransfer,
			ColorSpace:      s.ColorSpace,
			ColorRange:      s.ColorRange,
			Filename:        tags["FILENAME"],
			Mimetype:        tags["MIMETYPE"],
		}
		if st.Duration == 0 {
			st.Duration = durationTag(tags)
		}
		info.Streams = append(info.Streams, st)
	}
	return info, nil
}

// ratio reads a frame rate such as "24000/1001".
func ratio(s string) (float64, bool) {
	num, den, ok := strings.Cut(s, "/")
	n, err1 := strconv.ParseFloat(num, 64)
	d, err2 := strconv.ParseFloat(den, 64)
	if !ok || err1 != nil || err2 != nil || n <= 0 || d <= 0 {
		return 0, false
	}
	return n / d, true
}

func seconds(s string) time.Duration {
	secs, err := strconv.ParseFloat(s, 64)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs * float64(time.Second))
}

// durationTag reads the DURATION tag Matroska muxers put on each stream, e.g.
// "01:48:06.147000000". mkvmerge may suffix its name with a language, as in
// DURATION-eng.
func durationTag(tags map[string]string) time.Duration {
	for k, v := range tags {
		if k != "DURATION" && !strings.HasPrefix(k, "DURATION-") {
			continue
		}
		h, rest, ok1 := strings.Cut(v, ":")
		m, sec, ok2 := strings.Cut(rest, ":")
		hh, err1 := strconv.Atoi(h)
		mm, err2 := strconv.Atoi(m)
		ss, err3 := strconv.ParseFloat(sec, 64)
		if ok1 && ok2 && err1 == nil && err2 == nil && err3 == nil {
			return time.Duration((float64(hh*3600+mm*60) + ss) * float64(time.Second))
		}
	}
	return 0
}
