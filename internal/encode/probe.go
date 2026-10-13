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
}

type streamInfo struct {
	Index    int
	Type     string // video, audio, subtitle, attachment or data
	Codec    string
	Channels int
	Width    int
	Height   int
	Language string // as tagged, e.g. "eng" or "en-US"; empty if untagged
	CoverArt bool   // a still image attached as a video stream

	// Attachments must keep these, or the file can't be written.
	Filename, Mimetype string
}

func probe(ctx context.Context, in string) (mediaInfo, error) {
	out, err := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-show_entries", "format=duration"+
			":stream=index,codec_type,codec_name,channels,width,height"+
			":stream_tags=language,filename,mimetype"+
			":stream_disposition=attached_pic",
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
		return mediaInfo{}, fmt.Errorf("ffprobe: %w", err)
	}

	var raw struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			Index       int    `json:"index"`
			CodecType   string `json:"codec_type"`
			CodecName   string `json:"codec_name"`
			Channels    int    `json:"channels"`
			Width       int    `json:"width"`
			Height      int    `json:"height"`
			Tags        map[string]string
			Disposition struct {
				AttachedPic int `json:"attached_pic"`
			} `json:"disposition"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return mediaInfo{}, fmt.Errorf("reading ffprobe output: %w", err)
	}

	var info mediaInfo
	if secs, err := strconv.ParseFloat(raw.Format.Duration, 64); err == nil {
		info.Duration = time.Duration(secs * float64(time.Second))
	}
	for _, s := range raw.Streams {
		lang := s.Tags["language"]
		if lang == "und" {
			lang = ""
		}
		info.Streams = append(info.Streams, streamInfo{
			Index:    s.Index,
			Type:     s.CodecType,
			Codec:    s.CodecName,
			Channels: s.Channels,
			Width:    s.Width,
			Height:   s.Height,
			Language: lang,
			CoverArt: s.Disposition.AttachedPic == 1,
			Filename: s.Tags["filename"],
			Mimetype: s.Tags["mimetype"],
		})
	}
	return info, nil
}
