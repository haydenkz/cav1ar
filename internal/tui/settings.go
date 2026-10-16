package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/haydenkz/cav1ar/internal/config"
	"github.com/haydenkz/cav1ar/internal/encode"
)

// settingsDraft holds the values the settings screen edits until they are
// saved. It lives behind a pointer because the forms keep pointers to its
// fields across updates.
type settingsDraft struct {
	encode.Settings
	crf   string      // typed as text, so it can be invalid while being edited
	audio audioChoice // codec and bitrate, picked together
}

// audioChoice is one entry in the audio list.
type audioChoice struct {
	codec encode.AudioCodec
	kbps  int
}

func newDraft(s encode.Settings) *settingsDraft {
	s.Watch = slices.Clone(s.Watch)
	return &settingsDraft{
		Settings: s,
		crf:      strconv.Itoa(s.CRF),
		audio:    audioChoice{s.AudioCodec, s.AudioKbps},
	}
}

// settings is the draft as settings to save, or why it can't be saved yet
// and which section to fix it in.
func (d *settingsDraft) settings() (encode.Settings, int, error) {
	s := d.Settings
	crf, err := parseCRF(d.crf)
	if err != nil {
		return s, sectionVideo, fmt.Errorf("quality: %w", err)
	}
	s.CRF = crf
	if err := encode.CheckEncoder(s.Encoder); err != nil {
		return s, sectionVideo, err
	}
	if d.audio.kbps != encode.AudioCopy { // copying keeps the codec chosen before
		s.AudioCodec = d.audio.codec
	}
	s.AudioKbps = d.audio.kbps
	if s.AudioKbps == encode.AudioCopy {
		s.Downmix = false // copied audio can't be changed
	}
	s.Language = strings.ToLower(strings.TrimSpace(s.Language))
	if err := validateLanguage(s.Language); err != nil {
		return s, sectionAudio, fmt.Errorf("language: %w", err)
	}
	s.Title = strings.TrimSpace(s.Title)
	s.ReleaseGroup = strings.TrimSpace(s.ReleaseGroup)
	if s.NameTemplate = strings.TrimSpace(s.NameTemplate); s.NameTemplate == "" {
		return s, sectionNaming, errors.New("the file name can't be empty; ctrl+r restores the recommended one")
	}
	if s.OutDir = expandHome(strings.TrimSpace(s.OutDir)); s.OutDir != "" {
		if err := validateDir(s.OutDir); err != nil {
			return s, sectionOutput, fmt.Errorf("output folder: %w", err)
		}
	}
	if s.Preroll = expandHome(strings.TrimSpace(s.Preroll)); s.Preroll != "" {
		if err := validateClip(s.Preroll); err != nil {
			return s, sectionOutput, fmt.Errorf("preroll: %w", err)
		}
		if abs, err := filepath.Abs(s.Preroll); err == nil {
			s.Preroll = abs
		}
	}
	for _, f := range s.Watch {
		if err := validateDir(f.Path); err != nil {
			return s, sectionFolders, fmt.Errorf("%s: %w", f.Path, err)
		}
	}
	if err := s.Validate(); err != nil {
		return s, sectionVideo, err
	}
	return s, 0, nil
}

// The settings sections, in the order the sidebar lists them. Each but the
// last is a form of its own; the last is the list of watched folders.
const (
	sectionVideo = iota
	sectionAudio
	sectionSubtitles
	sectionNaming
	sectionOutput
	sectionFolders
)

var sectionNames = []string{"Video", "Audio", "Subtitles", "Naming", "Output", "Folders"}

// openSettings shows the settings screen, editing d, at section.
func (m *Model) openSettings(d *settingsDraft, section int) tea.Cmd {
	m.draft = d
	m.screen = screenSettings
	m.confirmDiscard = false
	m.folderRow = min(m.folderRow, len(d.Watch))
	return m.showSection(section)
}

// showSection moves to section i, wrapping around at either end, and builds
// its form afresh from the draft.
func (m *Model) showSection(i int) tea.Cmd {
	m.section = (i + len(sectionNames)) % len(sectionNames)
	m.form = nil
	if m.section == sectionFolders {
		return nil
	}
	m.shownFor = m.dependsOn()
	m.form = huh.NewForm(huh.NewGroup(m.sectionFields(m.section)...)).
		WithTheme(m.st.formTheme()).
		WithKeyMap(settingsFormKeys()).
		WithShowHelp(false).
		WithShowErrors(true)
	m.resizeForm()
	return m.form.Init()
}

// dependsOn sums up the draft values that decide which fields are shown, so
// the form can be rebuilt when one of them changes.
func (m *Model) dependsOn() string {
	d := m.draft
	return fmt.Sprint(d.Encoder, d.audio.kbps == encode.AudioCopy, d.Subtitles == encode.KeepNone)
}

// settingsFormKeys moves between fields with the arrows and enter, leaving
// tab to switch sections and esc to the settings screen.
func settingsFormKeys() *huh.KeyMap {
	k := huh.NewDefaultKeyMap()
	off := key.NewBinding(key.WithDisabled())
	next := key.NewBinding(key.WithKeys("down", "enter"))
	prev := key.NewBinding(key.WithKeys("up"))
	k.Quit = off
	k.Input.Next, k.Input.Prev = next, prev
	k.Select.Next, k.Select.Prev = next, prev
	k.Text.Next, k.Text.Prev = next, prev
	return k
}

// sectionFields are the fields of a form section, leaving out any that
// don't apply to the draft as it stands.
func (m *Model) sectionFields(section int) []huh.Field {
	d := m.draft
	rec := d.Settings.Recommended()
	switch section {
	case sectionVideo:
		encoders := make([]huh.Option[encode.Encoder], 0, len(encode.Encoders))
		for _, e := range encode.Encoders {
			label := e.String()
			if encode.CheckEncoder(e) != nil {
				label += " (not found)"
			}
			encoders = append(encoders, option(label, e, rec.Encoder))
		}
		fields := []huh.Field{
			huh.NewSelect[encode.Encoder]().Key("encoder").Title("Encoder").
				Options(encoders...).Value(&d.Encoder).Inline(true).Validate(encode.CheckEncoder),
			huh.NewInput().Key("crf").Title("Quality (CRF)").CharLimit(2).Value(&d.crf).
				Validate(func(s string) error { _, err := parseCRF(s); return err }),
		}
		switch d.Encoder {
		case encode.EncoderCPU:
			presets := make([]huh.Option[int], 0, 14)
			for p := 0; p <= 13; p++ {
				presets = append(presets, option(presetLabel(p), p, rec.Preset))
			}
			fields = append(fields, huh.NewSelect[int]().Key("preset").Title("Speed preset").
				Options(presets...).Value(&d.Preset).Inline(true))
		case encode.EncoderNVIDIA:
			presets := make([]huh.Option[int], 0, 7)
			for p := 1; p <= 7; p++ {
				presets = append(presets, option(nvencPresetLabel(p), p, rec.NVENCPreset))
			}
			fields = append(fields, huh.NewSelect[int]().Key("nvencPreset").Title("Speed preset").
				Options(presets...).Value(&d.NVENCPreset).Inline(true))
		}
		maxHeights := []huh.Option[int]{option("keep original", 0, rec.MaxHeight)}
		for _, h := range encode.MaxHeights {
			maxHeights = append(maxHeights, option(fmt.Sprintf("scale down to %dp", h), h, rec.MaxHeight))
		}
		workers := make([]huh.Option[int], 0, runtime.NumCPU())
		for n := 1; n <= max(runtime.NumCPU(), d.Workers); n++ {
			workers = append(workers, option(strconv.Itoa(n), n, rec.Workers))
		}
		return append(fields,
			huh.NewSelect[int]().Key("maxHeight").Title("Resolution").
				Options(maxHeights...).Value(&d.MaxHeight).Inline(true),
			huh.NewSelect[int]().Key("workers").Title("Encodes at once").
				Options(workers...).Value(&d.Workers).Inline(true),
		)

	case sectionAudio:
		recAudio := audioChoice{rec.AudioCodec, rec.AudioKbps}
		audio := []huh.Option[audioChoice]{option("copy original", audioChoice{d.audio.codec, encode.AudioCopy}, recAudio)}
		for _, c := range []struct {
			codec encode.AudioCodec
			name  string
			rates []int
		}{
			{encode.Opus, "Opus", []int{64, 96, 128, 160, 192, 256, 384}},
			{encode.AAC, "AAC", []int{96, 128, 192, 256, 384, 512}},
		} {
			audio = append(audio, option(c.name+" · auto bitrate", audioChoice{c.codec, encode.AudioAuto}, recAudio))
			for _, kbps := range c.rates {
				audio = append(audio, option(fmt.Sprintf("%s %d kbps", c.name, kbps), audioChoice{c.codec, kbps}, recAudio))
			}
		}
		fields := []huh.Field{
			huh.NewSelect[audioChoice]().Key("audio").Title("Audio").
				Options(audio...).Value(&d.audio).Inline(true),
		}
		if d.audio.kbps != encode.AudioCopy {
			fields = append(fields, huh.NewSelect[bool]().Key("downmix").Title("Channels").
				Options(option("keep original", false, rec.Downmix), option("downmix to stereo", true, rec.Downmix)).
				Value(&d.Downmix).Inline(true))
		}
		return append(fields,
			huh.NewInput().Key("language").Title("Language").CharLimit(3).Value(&d.Language).Validate(validateLanguage),
			huh.NewSelect[encode.TrackFilter]().Key("audioTracks").Title("Audio tracks").
				Options(
					option("keep all", encode.KeepAll, rec.AudioTracks),
					option("only that language", encode.KeepLanguage, rec.AudioTracks),
					option("the best one in that language", encode.KeepBest, rec.AudioTracks),
				).Value(&d.AudioTracks).Inline(true),
		)

	case sectionSubtitles:
		fields := []huh.Field{
			huh.NewSelect[encode.TrackFilter]().Key("subtitles").Title("Subtitles").
				Options(
					option("keep all", encode.KeepAll, rec.Subtitles),
					option("only the language set under Audio", encode.KeepLanguage, rec.Subtitles),
					option("remove all", encode.KeepNone, rec.Subtitles),
				).Value(&d.Subtitles).Inline(true),
		}
		if d.Subtitles != encode.KeepNone {
			fields = append(fields, huh.NewSelect[bool]().Key("tidySubs").Title("Tidy subtitles").
				Options(option("keep as they are", false, rec.TidySubs), option("tidy", true, rec.TidySubs)).
				Value(&d.TidySubs).Inline(true))
		}
		return fields

	case sectionNaming:
		return []huh.Field{
			huh.NewInput().Key("name").Title("File name").Value(&d.NameTemplate),
			huh.NewInput().Key("title").Title("Title shown in players").
				Placeholder("keep the source's title").Value(&d.Title),
			huh.NewInput().Key("group").Title("Release group").Placeholder("none").Value(&d.ReleaseGroup),
			huh.NewSelect[bool]().Key("strip").Title("Original metadata").
				Options(option("keep", false, rec.StripMetadata), option("strip", true, rec.StripMetadata)).
				Value(&d.StripMetadata).Inline(true),
		}

	case sectionOutput:
		return []huh.Field{
			huh.NewInput().Key("outDir").Title("Output folder").Placeholder("next to the source").
				Value(&d.OutDir).Validate(validateDir),
			huh.NewSelect[bool]().Key("deleteOriginal").Title("Original file").
				Options(option("keep", false, rec.DeleteOriginal), option("delete after a checked encode", true, rec.DeleteOriginal)).
				Value(&d.DeleteOriginal).Inline(true),
			huh.NewInput().Key("preroll").Title("Preroll").Placeholder("none").
				Value(&d.Preroll).Validate(validateClip),
		}
	}
	return nil
}

// fieldHelp explains the focused field, below the form.
func (m *Model) fieldHelp(key string) string {
	d := m.draft
	rec := d.Settings.Recommended()
	switch key {
	case "encoder":
		return "GPUs are much faster, but make bigger files at the same quality."
	case "crf":
		return fmt.Sprintf("1–63. Lower is better quality and bigger files. Recommended: %d.", rec.CRF)
	case "preset", "nvencPreset":
		return "Slower makes smaller files at the same quality."
	case "maxHeight":
		return "Bigger video is scaled down to fit. AV1 video that already fits is copied, not re-encoded."
	case "workers":
		return "How many files are encoded at once. One encode already keeps a CPU or GPU busy."
	case "audio":
		return "Auto is 128 kbps for stereo and 256 for 5.1 with Opus, half as much again with AAC. " +
			"Tracks already in that codec and layout are copied."
	case "downmix":
		return "Downmixing makes stereo from surround, for TVs and headphones."
	case "language":
		return "A 2 or 3 letter code such as eng, used to pick audio and subtitle tracks."
	case "audioTracks":
		return "If no track is in the language, all are kept."
	case "subtitles":
		return "Which subtitle tracks to keep."
	case "tidySubs":
		return "When two tracks in a language look alike, reads them to drop duplicates and ones in the wrong " +
			"language, and to flag forced and SDH tracks the source didn't."
	case "name":
		return strings.Join(encode.NamePlaceholders, " ") + "\ne.g. " + d.Settings.PreviewName()
	case "title":
		s := "Stored inside the file, apart from its name. Recommended: " + rec.Title
		if preview := d.Settings.PreviewTitle(); preview != "" {
			s += "\ne.g. " + preview
		}
		return s
	case "group":
		return "Used for {group} in names, and saved in the ENCODED_BY tag."
	case "strip":
		return "Stripping removes old titles, tags and chapters. Track languages stay."
	case "outDir":
		return "Where results go. Empty puts each next to its source."
	case "deleteOriginal":
		return "Deleting is permanent, and only happens once the result checks out."
	case "preroll":
		return "A clip to play before every result, such as an intro, joined on without re-encoding. " +
			"Changing or clearing it rewrites every file that has it."
	}
	return ""
}

// option labels the recommended choice so it stands out while cycling.
func option[T comparable](label string, value, recommended T) huh.Option[T] {
	if value == recommended {
		label += " (recommended)"
	}
	return huh.NewOption(label, value)
}

// Settings screen geometry: the sidebar, and the help lines under the form.
const (
	sidebarW  = 14
	helpLines = 3
)

func (m *Model) resizeForm() {
	if m.form != nil {
		m.form.WithWidth(m.settingsContentWidth()).WithHeight(max(m.bodyHeight()-2-helpLines-1, 1))
	}
}

// settingsContentWidth is the width beside the sidebar.
func (m Model) settingsContentWidth() int {
	return max(m.width-4-sidebarW-2, 10)
}

func (m *Model) updateSettings(msg tea.Msg) tea.Cmd {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		if m.confirmDiscard && !key.Matches(k, m.keys.Back, m.keys.Save) {
			m.confirmDiscard = false
		}
		switch {
		case key.Matches(k, m.keys.Save):
			return m.saveDraft()
		case key.Matches(k, m.keys.Recommend):
			return tea.Batch(m.openSettings(newDraft(m.draft.Settings.Recommended()), m.section),
				m.setStatus("Filled in the recommended settings. Look them over, then ctrl+s to save.", false))
		case key.Matches(k, m.keys.Back):
			if m.confirmDiscard || !m.draftChanged() {
				m.closeSettings()
				return m.setStatus("Settings unchanged", false)
			}
			m.confirmDiscard = true
			return nil
		case key.Matches(k, m.keys.NextSection):
			return m.showSection(m.section + 1)
		case key.Matches(k, m.keys.PrevSection):
			return m.showSection(m.section - 1)
		}
		if m.section == sectionFolders {
			return m.foldersKey(k)
		}
	}
	if m.form == nil {
		return nil
	}

	f, cmd := m.form.Update(msg)
	m.form = f.(*huh.Form)
	if m.form.State == huh.StateCompleted { // enter on the last field
		return tea.Batch(cmd, m.showSection(m.section+1))
	}
	// A choice that shows or hides other fields, such as the encoder, which
	// decides the presets offered. It is the first field of its section, so
	// focus stays on it.
	if m.dependsOn() != m.shownFor {
		focused := m.form.GetFocusedField().GetKey()
		cmd = tea.Batch(cmd, m.showSection(m.section))
		for range 20 { // never more fields than this; a field that won't validate stops the walk
			if m.form.GetFocusedField().GetKey() == focused || m.form.State != huh.StateNormal {
				break
			}
			m.form.NextField()
		}
	}
	return cmd
}

// saveDraft saves the draft, or shows why it can't be saved in the section
// that needs fixing.
func (m *Model) saveDraft() tea.Cmd {
	s, section, err := m.draft.settings()
	if err != nil {
		cmd := tea.Cmd(nil)
		if section != m.section {
			cmd = m.showSection(section)
		}
		return tea.Batch(cmd, m.setStatus("Can't save yet: "+err.Error(), true))
	}
	m.closeSettings()
	return m.applySettings(s)
}

// draftChanged reports whether saving the draft would change the settings.
// One that can't be saved yet has been changed.
func (m *Model) draftChanged() bool {
	s, _, err := m.draft.settings()
	return err != nil || !reflect.DeepEqual(s, m.settings)
}

func (m *Model) closeSettings() {
	m.form, m.draft, m.screen, m.confirmDiscard = nil, nil, screenQueue, false
}

// foldersKey edits the list of watched folders. Its last row is the choice
// of what to do with videos already in a newly watched folder.
func (m *Model) foldersKey(k tea.KeyPressMsg) tea.Cmd {
	d := m.draft
	optionRow := m.folderRow == len(d.Watch)
	switch {
	case key.Matches(k, m.keys.Up):
		m.folderRow = max(m.folderRow-1, 0)
	case key.Matches(k, m.keys.Down):
		m.folderRow = min(m.folderRow+1, len(d.Watch))
	case key.Matches(k, m.keys.AddFolder):
		m.openPicker(pickWatch)
		return m.picker.Init()
	case optionRow && key.Matches(k, m.keys.Toggle):
		d.WatchExisting = !d.WatchExisting
	case optionRow:
	case key.Matches(k, m.keys.RemoveFolder):
		d.Watch = slices.Delete(d.Watch, m.folderRow, m.folderRow+1)
	case key.Matches(k, m.keys.FolderKind):
		f := &d.Watch[m.folderRow]
		switch f.Kind {
		case encode.FolderAny:
			f.Kind = encode.FolderMovies
		case encode.FolderMovies:
			f.Kind = encode.FolderShows
		default:
			f.Kind = encode.FolderAny
		}
	case key.Matches(k, m.keys.Raise):
		d.Watch[m.folderRow].Priority++
	case key.Matches(k, m.keys.Lower):
		d.Watch[m.folderRow].Priority--
	}
	return nil
}

// watchFolder adds dir to the draft's watched folders, or selects it if it
// is already there.
func (m *Model) watchFolder(dir string) {
	d := m.draft
	if i := slices.IndexFunc(d.Watch, func(f encode.WatchFolder) bool { return f.Path == dir }); i >= 0 {
		m.folderRow = i
		return
	}
	d.Watch = append(d.Watch, encode.WatchFolder{Path: dir})
	m.folderRow = len(d.Watch) - 1
}

// settingsView is the sidebar of sections beside the open section, with
// help for whatever is focused below it.
func (m Model) settingsView() string {
	var side []string
	for i, name := range sectionNames {
		if i == m.section {
			side = append(side, m.st.selected.Render("▸ "+name))
		} else {
			side = append(side, m.st.dim.Render("  "+name))
		}
	}
	sidebar := lipgloss.NewStyle().Width(sidebarW).Render(strings.Join(side, "\n"))

	w := m.settingsContentWidth()
	var content, help string
	if m.section == sectionFolders {
		content, help = m.foldersView(w)
	} else {
		content = m.form.View()
		if f := m.form.GetFocusedField(); f != nil {
			help = m.fieldHelp(f.GetKey())
		}
	}
	h := max(m.bodyHeight()-2, 1)
	content = lipgloss.NewStyle().Height(h - helpLines - 1).MaxHeight(h - helpLines - 1).Render(content)
	helpBox := lipgloss.NewStyle().Width(w).MaxHeight(helpLines).Render(m.st.dim.Render(wrap(help, w)))
	right := lipgloss.JoinVertical(lipgloss.Left, content, m.st.paneEdge.Render(strings.Repeat("─", w)), helpBox)
	return lipgloss.JoinHorizontal(lipgloss.Top, sidebar, "  ", right)
}

// foldersView lists the watched folders, and the help for the selected row.
func (m Model) foldersView(w int) (string, string) {
	d := m.draft
	kindW, prioW := 8, 9
	pathW := max(w-2-kindW-prioW-2, 10)
	lines := []string{
		m.st.title.Render("Watched folders"),
		m.st.dim.Render("  " + cell("folder", pathW) + " " + cell("named as", kindW) + " " + cell("priority", prioW)),
	}
	if len(d.Watch) == 0 {
		lines = append(lines, m.st.dim.Render("  None yet. Press a to add one."))
	}
	for i, f := range d.Watch {
		kind := string(f.Kind)
		if f.Kind == encode.FolderAny {
			kind = "files"
		}
		prio := strconv.Itoa(f.Priority)
		if f.Priority > 0 {
			prio = "+" + prio
		}
		row := cellLeft(shortPath(f.Path), pathW) + " " + cell(kind, kindW) + " " + cell(prio, prioW)
		row = cell(row, w-2)
		if i == m.folderRow {
			lines = append(lines, m.st.selected.Render("▌ "+row))
		} else {
			lines = append(lines, "  "+row)
		}
	}
	existing := "leave alone"
	if d.WatchExisting {
		existing = "handle them too"
	}
	if d.WatchExisting == d.Settings.Recommended().WatchExisting {
		existing += " (recommended)"
	}
	opt := cell("Videos already in a folder when it is first watched: "+existing, w-2)
	if m.folderRow == len(d.Watch) {
		opt = m.st.selected.Render("▌ " + opt)
	} else {
		opt = "  " + opt
	}
	lines = append(lines, "", opt)

	help := "Videos here or in subfolders that don't match the settings are queued. Named as movies or shows, " +
		"titles come from the folders, as media servers lay them out. A higher priority runs first."
	if m.folderRow == len(d.Watch) {
		help = "Handling them checks every video already in a folder you start watching, and queues those that " +
			"don't match. Leaving them alone only handles videos that arrive later."
	}
	return strings.Join(lines, "\n"), help
}

// wrap breaks s into lines of at most w cells.
func wrap(s string, w int) string {
	var out []string
	for line := range strings.SplitSeq(s, "\n") {
		out = append(out, ansi.Wordwrap(line, w, ""))
	}
	return strings.Join(out, "\n")
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

func nvencPresetLabel(p int) string {
	var speed string
	switch {
	case p <= 2:
		speed = "fastest, biggest files"
	case p <= 4:
		speed = "balanced"
	case p <= 6:
		speed = "slow"
	default:
		speed = "slowest, smallest files"
	}
	return fmt.Sprintf("p%d · %s", p, speed)
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

// validateClip accepts no clip, or a path to a file.
func validateClip(s string) error {
	s = expandHome(strings.TrimSpace(s))
	if s == "" {
		return nil
	}
	fi, err := os.Stat(s)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return errors.New("that file doesn't exist")
	case err != nil:
		return err
	case fi.IsDir():
		return errors.New("that's a folder, not a clip")
	}
	return nil
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
