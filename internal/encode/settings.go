package encode

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
)

// TrackFilter chooses which audio or subtitle tracks to keep.
type TrackFilter string

const (
	KeepAll      TrackFilter = "all"
	KeepLanguage TrackFilter = "language" // only tracks in Settings.Language
	KeepNone     TrackFilter = "none"
)

// Settings control how jobs are encoded. A job uses the settings in effect
// when it starts.
type Settings struct {
	Encoder Encoder `json:"encoder"`
	CRF     int     `json:"crf"`     // quality, 1–63 like SVT-AV1's CRF; lower is better
	Preset  int     `json:"preset"`  // speed, 0 (slowest, best) to 13 (fastest) like SVT-AV1's
	Workers int     `json:"workers"` // encodes to run at once

	AudioKbps   int         `json:"audio_kbps"`        // Opus bitrate, or AudioCopy or AudioAuto
	Downmix     bool        `json:"downmix,omitempty"` // mix audio down to stereo; needs Opus
	Language    string      `json:"language"`          // ISO 639 code used by KeepLanguage, e.g. "eng"
	AudioTracks TrackFilter `json:"audio_tracks"`      // KeepAll or KeepLanguage
	Subtitles   TrackFilter `json:"subtitles"`         // KeepAll, KeepLanguage or KeepNone

	StripMetadata bool   `json:"strip_metadata,omitempty"` // drop titles, comments and tags from the source
	Title         string `json:"title,omitempty"`          // a template, like NameTemplate; empty keeps the source's
	ReleaseGroup  string `json:"release_group,omitempty"`  // {group} in templates, and the ENCODED_BY tag

	NameTemplate   string   `json:"name_template"`             // output file name, without extension; see NamePlaceholders
	OutDir         string   `json:"out_dir,omitempty"`         // empty writes next to the source
	DeleteOriginal bool     `json:"delete_original,omitempty"` // remove the source once its encode checks out
	Watch          []string `json:"watch,omitempty"`           // folders whose new videos are queued; see Watch
}

// Special AudioKbps values.
const (
	AudioCopy = 0  // keep the original audio
	AudioAuto = -1 // Opus, at a bitrate suited to each track's channels
)

func DefaultSettings() Settings {
	return Settings{
		Encoder:      EncoderCPU,
		CRF:          30,
		Preset:       6,
		Workers:      1,
		Language:     "eng",
		AudioTracks:  KeepAll,
		Subtitles:    KeepAll,
		NameTemplate: DefaultNameTemplate,
	}
}

// Recommended returns s with the recommended choice for every setting that
// has one, including keeping originals. Personal settings are kept: language,
// release group, and where files go.
func (s Settings) Recommended() Settings {
	// One SVT-AV1 encode keeps about 16 threads busy, so more at once only
	// pays off on bigger CPUs.
	s.Encoder, s.CRF, s.Preset, s.Workers = EncoderCPU, 30, 6, max(1, runtime.NumCPU()/16)
	s.AudioKbps, s.Downmix = AudioAuto, false
	s.AudioTracks, s.Subtitles = KeepAll, KeepAll
	s.StripMetadata, s.Title = true, "{title} ({year})"
	s.NameTemplate, s.DeleteOriginal = DefaultNameTemplate, false
	return s
}

var languageCode = regexp.MustCompile(`^[a-zA-Z]{2,3}$`)

func (s Settings) Validate() error {
	switch {
	case !slices.Contains(Encoders, s.Encoder):
		return fmt.Errorf("unknown encoder %q", s.Encoder)
	case s.CRF < 1 || s.CRF > 63:
		return fmt.Errorf("crf must be between 1 and 63, not %d", s.CRF)
	case s.Preset < 0 || s.Preset > 13:
		return fmt.Errorf("preset must be between 0 and 13, not %d", s.Preset)
	case s.Workers < 1:
		return fmt.Errorf("workers must be at least 1, not %d", s.Workers)
	case s.AudioKbps < AudioAuto:
		return fmt.Errorf("audio bitrate must be positive, or %d for auto, not %d", AudioAuto, s.AudioKbps)
	case s.Downmix && s.AudioKbps == AudioCopy:
		return errors.New("downmixing needs Opus audio, since copied audio can't be changed")
	case s.AudioTracks != KeepAll && s.AudioTracks != KeepLanguage:
		return fmt.Errorf("unknown audio track filter %q", s.AudioTracks)
	case s.Subtitles != KeepAll && s.Subtitles != KeepLanguage && s.Subtitles != KeepNone:
		return fmt.Errorf("unknown subtitle filter %q", s.Subtitles)
	case !languageCode.MatchString(s.Language):
		return fmt.Errorf("language must be a 2 or 3 letter code like eng, not %q", s.Language)
	case strings.TrimSpace(s.NameTemplate) == "":
		return errors.New("the file name template can't be empty")
	}
	return nil
}

// outputPath is where the encode of in is written.
func (s Settings) outputPath(in string, r release) string {
	dir := s.OutDir
	if dir == "" {
		dir = filepath.Dir(in)
	}
	return filepath.Join(dir, fileName(s.NameTemplate, r))
}

// Summary is a one-line description, e.g. "crf 30 · preset 6 · opus 128k".
func (s Settings) Summary() string {
	parts := []string{fmt.Sprintf("crf %d", s.CRF), fmt.Sprintf("preset %d", s.Preset)}
	if s.Encoder != EncoderCPU {
		parts = append([]string{strings.ToLower(s.Encoder.String())}, parts...)
	}
	audio := "audio copy"
	switch {
	case s.AudioKbps == AudioAuto:
		audio = "opus auto"
	case s.AudioKbps > 0:
		audio = fmt.Sprintf("opus %dk", s.AudioKbps)
	}
	if s.Downmix {
		audio += " stereo"
	}
	parts = append(parts, audio)
	if s.AudioTracks == KeepLanguage {
		parts = append(parts, s.Language+" audio")
	}
	switch s.Subtitles {
	case KeepNone:
		parts = append(parts, "no subs")
	case KeepLanguage:
		parts = append(parts, s.Language+" subs")
	}
	if s.StripMetadata {
		parts = append(parts, "stripped")
	}
	if s.ReleaseGroup != "" {
		parts = append(parts, s.ReleaseGroup)
	}
	if s.DeleteOriginal {
		parts = append(parts, "deletes originals")
	}
	if s.Workers > 1 {
		parts = append(parts, fmt.Sprintf("%d at once", s.Workers))
	}
	return strings.Join(parts, " · ")
}

func baseName(path string) string {
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}
