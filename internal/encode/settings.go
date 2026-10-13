package encode

import (
	"encoding/json"
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
	KeepBest     TrackFilter = "best"     // audio only: the one best track in Settings.Language
	KeepNone     TrackFilter = "none"
)

// AudioCodec is what audio is encoded to when it isn't copied.
type AudioCodec string

const (
	Opus AudioCodec = "opus"
	AAC  AudioCodec = "aac"
)

// FolderKind says how videos in a watched folder are laid out, which decides
// where their names come from.
type FolderKind string

const (
	FolderAny    FolderKind = ""       // names come from each file's own name
	FolderMovies FolderKind = "movies" // Movies/Title (Year)/file: titles come from the folder
	FolderShows  FolderKind = "shows"  // Shows/Show/[Season N/]file: titles come from the show's folder
)

// WatchFolder is a folder whose videos are kept encoded.
type WatchFolder struct {
	Path string     `json:"path"`
	Kind FolderKind `json:"kind,omitempty"`
}

// UnmarshalJSON also accepts a bare path, as older settings saved them.
func (f *WatchFolder) UnmarshalJSON(data []byte) error {
	var path string
	if json.Unmarshal(data, &path) == nil {
		*f = WatchFolder{Path: path}
		return nil
	}
	type plain WatchFolder
	return json.Unmarshal(data, (*plain)(f))
}

// String is the folder as the settings form shows it, e.g. "shows: /tv".
func (f WatchFolder) String() string {
	if f.Kind == FolderAny {
		return f.Path
	}
	return string(f.Kind) + ": " + f.Path
}

// ParseWatchFolder reads a folder written as String does.
func ParseWatchFolder(s string) WatchFolder {
	for _, kind := range []FolderKind{FolderMovies, FolderShows} {
		if rest, ok := strings.CutPrefix(s, string(kind)+":"); ok {
			return WatchFolder{Path: strings.TrimSpace(rest), Kind: kind}
		}
	}
	return WatchFolder{Path: strings.TrimSpace(s)}
}

// Settings control how jobs are encoded. A job uses the settings in effect
// when it starts.
type Settings struct {
	Encoder     Encoder `json:"encoder"`
	CRF         int     `json:"crf"`                  // quality, 1–63 like SVT-AV1's CRF; lower is better
	Preset      int     `json:"preset"`               // CPU speed, 0 (slowest, best) to 13 (fastest) like SVT-AV1's
	NVENCPreset int     `json:"nvenc_preset"`         // NVIDIA speed, 1 (fastest) to 7 (slowest, best) like NVENC's p1–p7
	MaxHeight   int     `json:"max_height,omitempty"` // scale bigger video down to fit this height and its 16:9 width; 0 keeps the size
	Workers     int     `json:"workers"`              // encodes to run at once

	AudioCodec  AudioCodec  `json:"audio_codec"`       // what audio is encoded to, unless AudioKbps is AudioCopy
	AudioKbps   int         `json:"audio_kbps"`        // bitrate, or AudioCopy or AudioAuto
	Downmix     bool        `json:"downmix,omitempty"` // mix surround down to stereo; needs encoded audio
	Language    string      `json:"language"`          // ISO 639 code used by KeepLanguage and KeepBest, e.g. "eng"
	AudioTracks TrackFilter `json:"audio_tracks"`      // KeepAll, KeepLanguage or KeepBest
	Subtitles   TrackFilter `json:"subtitles"`         // KeepAll, KeepLanguage or KeepNone
	TidySubs    bool        `json:"tidy_subs,omitempty"`

	StripMetadata bool   `json:"strip_metadata,omitempty"` // drop titles, comments and tags from the source
	Title         string `json:"title,omitempty"`          // a template, like NameTemplate; empty keeps the source's
	ReleaseGroup  string `json:"release_group,omitempty"`  // {group} in templates, and the ENCODED_BY tag

	NameTemplate   string        `json:"name_template"`             // output file name, without extension; see NamePlaceholders
	OutDir         string        `json:"out_dir,omitempty"`         // empty writes next to the source
	DeleteOriginal bool          `json:"delete_original,omitempty"` // remove the source once its encode checks out
	Watch          []WatchFolder `json:"watch,omitempty"`           // folders whose videos are queued; see Watch
	WatchExisting  bool          `json:"watch_existing,omitempty"`  // also queue videos already in a folder when it is first watched
}

// Special AudioKbps values.
const (
	AudioCopy = 0  // keep the original audio
	AudioAuto = -1 // a bitrate suited to each track's channels
)

// MaxHeights are the sizes offered for Settings.MaxHeight, after 0 (keep).
var MaxHeights = []int{2160, 1440, 1080, 720, 480}

func DefaultSettings() Settings {
	return Settings{
		Encoder:      EncoderCPU,
		CRF:          30,
		Preset:       6,
		NVENCPreset:  6,
		Workers:      1,
		AudioCodec:   Opus,
		Language:     "eng",
		AudioTracks:  KeepAll,
		Subtitles:    KeepAll,
		NameTemplate: DefaultNameTemplate,
	}
}

// Recommended returns s with the recommended choice for every setting that
// has one, including keeping originals. Personal settings are kept: language,
// release group, video size, and where files go.
func (s Settings) Recommended() Settings {
	// One SVT-AV1 encode keeps about 16 threads busy, so more at once only
	// pays off on bigger CPUs.
	s.Encoder, s.CRF, s.Preset, s.Workers = EncoderCPU, 30, 6, max(1, runtime.NumCPU()/16)
	s.NVENCPreset = 6
	s.AudioCodec, s.AudioKbps, s.Downmix = Opus, AudioAuto, false
	s.AudioTracks, s.Subtitles, s.TidySubs = KeepAll, KeepAll, true
	s.StripMetadata, s.Title = true, "{title} ({year}) {episode}"
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
	case s.NVENCPreset < 1 || s.NVENCPreset > 7:
		return fmt.Errorf("NVIDIA preset must be between 1 and 7, not %d", s.NVENCPreset)
	case s.MaxHeight != 0 && s.MaxHeight < 144:
		return fmt.Errorf("maximum height must be at least 144, or 0 to keep the size, not %d", s.MaxHeight)
	case s.Workers < 1:
		return fmt.Errorf("workers must be at least 1, not %d", s.Workers)
	case s.AudioCodec != Opus && s.AudioCodec != AAC:
		return fmt.Errorf("unknown audio codec %q", s.AudioCodec)
	case s.AudioKbps < AudioAuto:
		return fmt.Errorf("audio bitrate must be positive, or %d for auto, not %d", AudioAuto, s.AudioKbps)
	case s.Downmix && s.AudioKbps == AudioCopy:
		return errors.New("downmixing needs encoded audio, since copied audio can't be changed")
	case s.AudioTracks != KeepAll && s.AudioTracks != KeepLanguage && s.AudioTracks != KeepBest:
		return fmt.Errorf("unknown audio track filter %q", s.AudioTracks)
	case s.Subtitles != KeepAll && s.Subtitles != KeepLanguage && s.Subtitles != KeepNone:
		return fmt.Errorf("unknown subtitle filter %q", s.Subtitles)
	case !languageCode.MatchString(s.Language):
		return fmt.Errorf("language must be a 2 or 3 letter code like eng, not %q", s.Language)
	case strings.TrimSpace(s.NameTemplate) == "":
		return errors.New("the file name template can't be empty")
	}
	for _, f := range s.Watch {
		if f.Kind != FolderAny && f.Kind != FolderMovies && f.Kind != FolderShows {
			return fmt.Errorf("%s: unknown folder kind %q", f.Path, f.Kind)
		}
	}
	return nil
}

// WatchPaths lists the watched folders' paths.
func (s Settings) WatchPaths() []string {
	paths := make([]string, len(s.Watch))
	for i, f := range s.Watch {
		paths[i] = f.Path
	}
	return paths
}

// folderOf is the watched folder that path is in, the innermost if several
// are, or a FolderAny folder holding just path if none is.
func (s Settings) folderOf(path string) WatchFolder {
	best := WatchFolder{Path: filepath.Dir(path)}
	found := false
	for _, f := range s.Watch {
		if strings.HasPrefix(path, f.Path+string(filepath.Separator)) && (!found || len(f.Path) > len(best.Path)) {
			best, found = f, true
		}
	}
	return best
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
	parts := []string{fmt.Sprintf("crf %d", s.CRF)}
	switch s.Encoder {
	case EncoderCPU:
		parts = append(parts, fmt.Sprintf("preset %d", s.Preset))
	case EncoderNVIDIA:
		parts = append([]string{"nvidia gpu"}, append(parts, fmt.Sprintf("p%d", s.NVENCPreset))...)
	default:
		parts = append([]string{strings.ToLower(s.Encoder.String())}, parts...)
	}
	if s.MaxHeight > 0 {
		parts = append(parts, fmt.Sprintf("≤%dp", s.MaxHeight))
	}
	audio := "audio copy"
	switch {
	case s.AudioKbps == AudioAuto:
		audio = string(s.AudioCodec) + " auto"
	case s.AudioKbps > 0:
		audio = fmt.Sprintf("%s %dk", s.AudioCodec, s.AudioKbps)
	}
	if s.Downmix {
		audio += " stereo"
	}
	parts = append(parts, audio)
	switch s.AudioTracks {
	case KeepLanguage:
		parts = append(parts, s.Language+" audio")
	case KeepBest:
		parts = append(parts, "best "+s.Language+" audio")
	}
	switch s.Subtitles {
	case KeepNone:
		parts = append(parts, "no subs")
	case KeepLanguage:
		parts = append(parts, s.Language+" subs")
	}
	if s.TidySubs && s.Subtitles != KeepNone {
		parts = append(parts, "tidy subs")
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
