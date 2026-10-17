package encode

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
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
	Path     string     `json:"path"`
	Kind     FolderKind `json:"kind,omitempty"`
	Priority int        `json:"priority,omitempty"` // its videos run before those of folders with a lower one
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

// String is the folder as the settings form shows it, e.g. "shows: /tv", or
// "+1 movies: /films" with a priority.
func (f WatchFolder) String() string {
	s := f.Path
	if f.Kind != FolderAny {
		s = string(f.Kind) + ": " + s
	}
	if f.Priority != 0 {
		s = fmt.Sprintf("%+d %s", f.Priority, s)
	}
	return s
}

var priorityPrefix = regexp.MustCompile(`^([+-]\d+)\s+`)

// ParseWatchFolder reads a folder written as String does.
func ParseWatchFolder(s string) WatchFolder {
	var f WatchFolder
	s = strings.TrimSpace(s)
	if m := priorityPrefix.FindStringSubmatch(s); m != nil {
		f.Priority, _ = strconv.Atoi(m[1])
		s = s[len(m[0]):]
	}
	f.Path = s
	for _, kind := range []FolderKind{FolderMovies, FolderShows} {
		if rest, ok := strings.CutPrefix(s, string(kind)+":"); ok {
			f.Path, f.Kind = strings.TrimSpace(rest), kind
			break
		}
	}
	return f
}

// Settings control how jobs are encoded. A job uses the settings in effect
// when it starts.
type Settings struct {
	// Encoders says how many files a device encodes at once, by its ID or
	// by its kind (cpu, nvidia or vaapi); 0 turns it off. Devices it doesn't
	// mention get the default; see Slots.
	Encoders    map[string]int `json:"encoders,omitempty"`
	CRF         int            `json:"crf"`                    // quality, 1–63 on NVENC's scale, which the other encoders are set to match; lower is better
	Preset      int            `json:"preset"`                 // CPU speed, 0 (slowest, best) to 13 (fastest) like SVT-AV1's
	NVENCPreset int            `json:"nvenc_preset"`           // NVIDIA speed, 1 (fastest) to 7 (slowest, best) like NVENC's p1–p7
	VAAPIPreset int            `json:"vaapi_preset,omitempty"` // AMD/Intel speed, 1 (slowest, best) to 7 (fastest) as Intel's driver counts it; 0 leaves it to the driver
	MaxHeight   int            `json:"max_height,omitempty"`   // scale bigger video down to fit this height and its 16:9 width; 0 keeps the size

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

	Preroll string `json:"preroll,omitempty"` // a clip to put before every result; see preroll.go

	// Workers lend a daemon their GPUs; see Runner. WorkerListen is the
	// address the daemon takes them on, such as ":7310", or empty for none.
	// WorkerDir is a folder the daemon and its workers all reach at that
	// path, for the files workers write.
	WorkerListen string `json:"worker_listen,omitempty"`
	WorkerDir    string `json:"worker_dir,omitempty"`
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
		CRF:          30,
		Preset:       6,
		NVENCPreset:  6,
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
	s.Encoders, s.CRF, s.Preset, s.NVENCPreset, s.VAAPIPreset = nil, 30, 6, 6, 0
	s.AudioCodec, s.AudioKbps, s.Downmix = Opus, AudioAuto, false
	s.AudioTracks, s.Subtitles, s.TidySubs = KeepAll, KeepAll, true
	s.StripMetadata, s.Title = true, "{title} ({year}) {episode}"
	s.NameTemplate, s.DeleteOriginal = DefaultNameTemplate, false
	return s
}

var languageCode = regexp.MustCompile(`^[a-zA-Z]{2,3}$`)

func (s Settings) Validate() error {
	for id, n := range s.Encoders {
		if n < 0 || n > MaxSlots {
			return fmt.Errorf("%s: encodes at once must be between 0 and %d, not %d", id, MaxSlots, n)
		}
	}
	switch {
	case s.CRF < 1 || s.CRF > 63:
		return fmt.Errorf("crf must be between 1 and 63, not %d", s.CRF)
	case s.Preset < 0 || s.Preset > 13:
		return fmt.Errorf("preset must be between 0 and 13, not %d", s.Preset)
	case s.NVENCPreset < 1 || s.NVENCPreset > 7:
		return fmt.Errorf("NVIDIA preset must be between 1 and 7, not %d", s.NVENCPreset)
	case s.VAAPIPreset < 0 || s.VAAPIPreset > 7:
		return fmt.Errorf("AMD/Intel speed must be between 1 and 7, or 0 for the driver's own, not %d", s.VAAPIPreset)
	case s.MaxHeight != 0 && s.MaxHeight < 144:
		return fmt.Errorf("maximum height must be at least 144, or 0 to keep the size, not %d", s.MaxHeight)
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
	case s.WorkerDir != "" && !filepath.IsAbs(s.WorkerDir):
		return fmt.Errorf("the workers' folder must be a full path, not %q", s.WorkerDir)
	case s.WorkerListen != "" && s.WorkerDir == "":
		return errors.New("taking workers needs a folder the daemon and its workers share (worker_dir)")
	}
	for _, f := range s.Watch {
		if f.Kind != FolderAny && f.Kind != FolderMovies && f.Kind != FolderShows {
			return fmt.Errorf("%s: unknown folder kind %q", f.Path, f.Kind)
		}
	}
	return nil
}

// MaxSlots is the most files a device can be set to encode at once.
const MaxSlots = 16

// gpuSlots is how many files a GPU encodes at once by default. One encode
// keeps its encoder busy, apart from the gaps while a job does its work on
// the CPU, such as checking the result; a second fills those gaps.
const gpuSlots = 2

// Slots is how many files d encodes at once, given every device there is:
// what the settings say for d, or else for its kind, or else the default,
// which is 2 for a GPU, and for the CPU, none while a GPU encodes, since it
// is far slower. 0 means d is off.
func (s Settings) Slots(d Device, devices []Device) int {
	if n, ok := s.Encoders[d.ID]; ok {
		return n
	}
	if n, ok := s.Encoders[string(d.Kind)]; ok {
		return n
	}
	if d.GPU() {
		return gpuSlots
	}
	for _, other := range devices {
		if other.GPU() && s.Slots(other, devices) > 0 {
			return 0
		}
	}
	// One SVT-AV1 encode keeps about 16 threads busy, so more at once only
	// pays off on bigger CPUs.
	return max(1, runtime.NumCPU()/16)
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

// priorityOf is the priority of the watched folder path is in, or 0 for a
// video in none.
func (s Settings) priorityOf(path string) int {
	return s.folderOf(path).Priority
}

// outputPath is where the encode of in is written.
func (s Settings) outputPath(in string, r release) string {
	dir := s.OutDir
	if dir == "" {
		dir = filepath.Dir(in)
	}
	return filepath.Join(dir, fileName(s.NameTemplate, r))
}

// DeviceSummary says what each device does under s, e.g. "GeForce RTX 4070
// 2 at once p4 · CPU off".
func (s Settings) DeviceSummary(devices []Device) string {
	if len(devices) == 0 {
		return "nothing found that can encode AV1"
	}
	var parts []string
	for _, d := range devices {
		n := s.Slots(d, devices)
		switch {
		case n == 0:
			parts = append(parts, d.Name+" off")
		case d.Kind == EncoderNVIDIA:
			parts = append(parts, fmt.Sprintf("%s %d at once p%d", d.Name, n, s.NVENCPreset))
		case d.Kind == EncoderCPU:
			parts = append(parts, fmt.Sprintf("%s %d at once preset %d", d.Name, n, s.Preset))
		case d.Kind == EncoderVAAPI && s.VAAPIPreset > 0:
			parts = append(parts, fmt.Sprintf("%s %d at once speed %d", d.Name, n, s.VAAPIPreset))
		default:
			parts = append(parts, fmt.Sprintf("%s %d at once", d.Name, n))
		}
	}
	return strings.Join(parts, " · ")
}

// Summary is a one-line description, e.g. "crf 30 · ≤1080p · opus 128k".
func (s Settings) Summary() string {
	parts := []string{fmt.Sprintf("crf %d", s.CRF)}
	for _, id := range slices.Sorted(maps.Keys(s.Encoders)) {
		if n := s.Encoders[id]; n == 0 {
			parts = append(parts, id+" off")
		} else {
			parts = append(parts, fmt.Sprintf("%s %d at once", id, n))
		}
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
	if s.Preroll != "" {
		parts = append(parts, "preroll")
	}
	return strings.Join(parts, " · ")
}

func baseName(path string) string {
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}
