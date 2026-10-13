package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/haydenkz/cav1ar/internal/config"
	"github.com/haydenkz/cav1ar/internal/encode"
)

// settingsDraft holds the values the settings form edits. It lives behind a
// pointer because the form keeps pointers to its fields across updates.
type settingsDraft struct {
	encode.Settings
	crf   string // typed as text, so it can be invalid while being edited
	watch string // one folder per line
}

func newDraft(s encode.Settings) *settingsDraft {
	return &settingsDraft{Settings: s, crf: strconv.Itoa(s.CRF), watch: strings.Join(s.Watch, "\n")}
}

func (d *settingsDraft) settings() (encode.Settings, error) {
	s := d.Settings
	crf, err := parseCRF(d.crf)
	if err != nil {
		return s, err
	}
	s.CRF = crf
	s.Language = strings.ToLower(strings.TrimSpace(s.Language))
	s.Title = strings.TrimSpace(s.Title)
	s.ReleaseGroup = strings.TrimSpace(s.ReleaseGroup)
	s.NameTemplate = strings.TrimSpace(s.NameTemplate)
	s.OutDir = expandHome(strings.TrimSpace(s.OutDir))
	s.Watch = folders(d.watch)
	return s, s.Validate()
}

// folders reads one folder per line, ignoring blanks and repeats.
func folders(text string) []string {
	var out []string
	for line := range strings.Lines(text) {
		dir := expandHome(strings.TrimSpace(line))
		if dir == "" {
			continue
		}
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		if !slices.Contains(out, dir) {
			out = append(out, dir)
		}
	}
	return out
}

// The settings form has one group per page; fieldPage says which page each
// field is on, by key, so the tab strip can follow the focus.
var (
	settingsPages = []string{"Video", "Audio & subtitles", "Metadata", "Output", "Watch"}
	fieldPage     = map[string]int{
		"encoder": 0, "crf": 0, "preset": 0, "workers": 0,
		"audio": 1, "downmix": 1, "language": 1, "audioTracks": 1, "subtitles": 1,
		"strip": 2, "title": 2, "group": 2,
		"name": 3, "outDir": 3, "deleteOriginal": 3,
		"watch": 4,
	}
)

func (m *Model) openSettings(d *settingsDraft) tea.Cmd {
	m.draft = d
	rec := d.Settings.Recommended()

	presets := make([]huh.Option[int], 0, 14)
	for p := 0; p <= 13; p++ {
		presets = append(presets, option(presetLabel(p), p, rec.Preset))
	}
	audio := []huh.Option[int]{
		option("copy original", encode.AudioCopy, rec.AudioKbps),
		option("Opus · auto bitrate", encode.AudioAuto, rec.AudioKbps),
	}
	for _, kbps := range []int{64, 96, 128, 160, 192, 256, 384} {
		audio = append(audio, option(fmt.Sprintf("Opus %d kbps", kbps), kbps, rec.AudioKbps))
	}
	encoders := make([]huh.Option[encode.Encoder], 0, len(encode.Encoders))
	for _, e := range encode.Encoders {
		label := e.String()
		if encode.CheckEncoder(e) != nil {
			label += " (not found)"
		}
		encoders = append(encoders, option(label, e, rec.Encoder))
	}
	workers := make([]huh.Option[int], 0, runtime.NumCPU())
	for n := 1; n <= max(runtime.NumCPU(), d.Workers); n++ {
		workers = append(workers, option(strconv.Itoa(n), n, rec.Workers))
	}

	keys := huh.NewDefaultKeyMap()
	keys.Quit = key.NewBinding(key.WithKeys("esc"))

	video := huh.NewGroup(
		huh.NewSelect[encode.Encoder]().Key("encoder").
			Title("Encoder").
			Description("GPUs are much faster, but make bigger files at the same quality.").
			Options(encoders...).
			Value(&d.Encoder).
			Inline(true).
			Validate(encode.CheckEncoder),
		huh.NewInput().Key("crf").
			Title("Quality (CRF)").
			Description(fmt.Sprintf("1–63. Lower is better quality and bigger files. Recommended: %d.", rec.CRF)).
			CharLimit(2).
			Value(&d.crf).
			Validate(func(s string) error { _, err := parseCRF(s); return err }),
		huh.NewSelect[int]().Key("preset").
			Title("Speed preset").
			Description("Slower makes smaller files.").
			Options(presets...).
			Value(&d.Preset).
			Inline(true),
		huh.NewSelect[int]().Key("workers").
			Title("Encodes at once").
			Description("Each encode already uses many CPU threads.").
			Options(workers...).
			Value(&d.Workers).
			Inline(true),
	)
	tracks := huh.NewGroup(
		huh.NewSelect[int]().Key("audio").
			Title("Audio").
			Description("Opus auto uses 128 kbps for stereo, 256 for 5.1.").
			Options(audio...).
			Value(&d.AudioKbps).
			Inline(true),
		huh.NewSelect[bool]().Key("downmix").
			Title("Channels").
			Options(
				option("keep original", false, rec.Downmix),
				option("downmix to stereo", true, rec.Downmix),
			).
			Value(&d.Downmix).
			Inline(true).
			Validate(func(downmix bool) error {
				if downmix && d.AudioKbps == encode.AudioCopy {
					return errors.New("pick Opus above to downmix; copied audio can't be changed")
				}
				return nil
			}),
		huh.NewInput().Key("language").
			Title("Language").
			Description("For the filters below, e.g. eng, jpn, spa.").
			CharLimit(3).
			Value(&d.Language).
			Validate(validateLanguage),
		huh.NewSelect[encode.TrackFilter]().Key("audioTracks").
			Title("Audio tracks").
			Description("If none match, all are kept.").
			Options(
				option("keep all", encode.KeepAll, rec.AudioTracks),
				option("only that language", encode.KeepLanguage, rec.AudioTracks),
			).
			Value(&d.AudioTracks).
			Inline(true),
		huh.NewSelect[encode.TrackFilter]().Key("subtitles").
			Title("Subtitles").
			Options(
				option("keep all", encode.KeepAll, rec.Subtitles),
				option("only that language", encode.KeepLanguage, rec.Subtitles),
				option("remove all", encode.KeepNone, rec.Subtitles),
			).
			Value(&d.Subtitles).
			Inline(true),
	)
	metadata := huh.NewGroup(
		huh.NewSelect[bool]().Key("strip").
			Title("Original metadata").
			Description("Stripping removes old titles and tags. Track languages stay.").
			Options(
				option("keep", false, rec.StripMetadata),
				option("strip", true, rec.StripMetadata),
			).
			Value(&d.StripMetadata).
			Inline(true),
		huh.NewInput().Key("title").
			Title("Title shown in players").
			DescriptionFunc(func() string {
				desc := "Stored inside the file, separate from the file name. Recommended: " + rec.Title
				if preview := d.Settings.PreviewTitle(); preview != "" {
					desc += "\ne.g. " + preview
				}
				return desc
			}, &d.Settings).
			Placeholder("keep the source's title").
			Value(&d.Title),
		huh.NewInput().Key("group").
			Title("Release group").
			Description("Used for {group}, and saved in the ENCODED_BY tag.").
			Placeholder("none").
			Value(&d.ReleaseGroup),
	)
	output := huh.NewGroup(
		huh.NewInput().Key("name").
			Title("File name").
			DescriptionFunc(func() string {
				return strings.Join(encode.NamePlaceholders, " ") + "\n" +
					"e.g. " + d.Settings.PreviewName()
			}, &d.Settings).
			Value(&d.NameTemplate).
			Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					return errors.New("the file name can't be empty; ctrl+r restores the recommended one")
				}
				return nil
			}),
		huh.NewInput().Key("outDir").
			Title("Output folder").
			Placeholder("next to the source").
			Value(&d.OutDir).
			Validate(validateDir),
		huh.NewSelect[bool]().Key("deleteOriginal").
			Title("Original file").
			Description("Deleting is permanent, and only happens once the encode checks out.").
			Options(
				option("keep", false, rec.DeleteOriginal),
				option("delete after a checked encode", true, rec.DeleteOriginal),
			).
			Value(&d.DeleteOriginal).
			Inline(true),
	)

	watch := huh.NewGroup(
		huh.NewText().Key("watch").
			Title("Watched folders").
			Description("New videos here or in subfolders are queued. One per line (alt+enter).").
			Lines(6).
			Value(&d.watch).
			Validate(func(text string) error {
				for _, dir := range folders(text) {
					if err := validateDir(dir); err != nil {
						return fmt.Errorf("%s: %w", dir, err)
					}
				}
				return nil
			}),
	)

	m.form = huh.NewForm(video, tracks, metadata, output, watch).
		WithTheme(m.st.formTheme()).
		WithKeyMap(keys).
		WithShowHelp(false)

	m.screen = screenSettings
	m.resizeForm()
	return m.form.Init()
}

// option labels the recommended choice so it stands out while cycling.
func option[T comparable](label string, value, recommended T) huh.Option[T] {
	if value == recommended {
		label += " (recommended)"
	}
	return huh.NewOption(label, value)
}

// settingsChrome is the height of the tab strip and the space left below the
// fields, where huh shows validation errors.
const settingsChrome = 4

func (m *Model) resizeForm() {
	if m.form != nil {
		m.form.WithWidth(m.width - 4).WithHeight(max(m.bodyHeight()-2-settingsChrome, 1))
	}
}

func (m *Model) updateSettings(msg tea.Msg) tea.Cmd {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch {
		case key.Matches(k, m.keys.Save):
			s, err := m.draft.settings()
			if err != nil {
				return m.setStatus("Can't save yet: "+err.Error(), true)
			}
			m.form, m.screen = nil, screenQueue
			return m.applySettings(s)
		case key.Matches(k, m.keys.Recommend):
			d := newDraft(m.draft.Settings.Recommended())
			return tea.Batch(m.openSettings(d),
				m.setStatus("Filled in the recommended settings. Look them over, then ctrl+s to save.", false))
		}
	}

	f, cmd := m.form.Update(msg)
	m.form = f.(*huh.Form)

	switch m.form.State {
	case huh.StateCompleted:
		s, err := m.draft.settings()
		if err != nil {
			// A field changed after the one depending on it was checked, such
			// as audio set back to copy after choosing to downmix.
			return tea.Batch(m.openSettings(m.draft), m.setStatus("Can't save yet: "+err.Error(), true))
		}
		m.form, m.screen = nil, screenQueue
		return tea.Batch(cmd, m.applySettings(s))
	case huh.StateAborted:
		m.form, m.screen = nil, screenQueue
		return m.setStatus("Settings unchanged", false)
	}
	return cmd
}

// settingsPage is the index of the page holding the focused field.
func (m Model) settingsPage() int {
	if f := m.form.GetFocusedField(); f != nil {
		return fieldPage[f.GetKey()]
	}
	return 0
}

// applySettings saves s and says so in the footer.
func (m *Model) applySettings(s encode.Settings) tea.Cmd {
	if err := m.saveSettings(s); err != nil {
		return m.setStatus("Settings applied, but couldn't be saved: "+err.Error(), true)
	}
	return m.setStatus("Settings saved. Jobs that haven't started will use them.", false)
}

// saveSettings applies s to the queue and the folder watcher, and saves it
// for next time.
func (m *Model) saveSettings(s encode.Settings) error {
	m.settings = s
	m.queue.SetSettings(s)
	return config.Save(s)
}

func presetLabel(p int) string {
	var speed string
	switch {
	case p <= 2:
		speed = "very slow, smallest files"
	case p <= 5:
		speed = "slow"
	case p <= 8:
		speed = "balanced"
	case p <= 10:
		speed = "fast"
	default:
		speed = "fastest, biggest files"
	}
	return fmt.Sprintf("%d · %s", p, speed)
}

func validateLanguage(s string) error {
	s = strings.TrimSpace(s)
	if len(s) < 2 || strings.IndexFunc(s, func(r rune) bool { return !unicode.IsLetter(r) }) >= 0 {
		return errors.New("use a 2 or 3 letter code, like eng")
	}
	return nil
}

func parseCRF(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 || n > 63 {
		return 0, errors.New("enter a number from 1 to 63")
	}
	return n, nil
}

func validateDir(s string) error {
	s = expandHome(strings.TrimSpace(s))
	if s == "" {
		return nil
	}
	fi, err := os.Stat(s)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return errors.New("that folder doesn't exist")
	case err != nil:
		return err
	case !fi.IsDir():
		return errors.New("that's a file, not a folder")
	}
	return nil
}

func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~"))
}
