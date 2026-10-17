package tui

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/haydenkz/cav1ar/internal/config"
	"github.com/haydenkz/cav1ar/internal/encode"
)

// fakeQueue is a Backend that records the settings it is given.
type fakeQueue struct {
	settings encode.Settings
	devices  []encode.Device
	saves    int
	holding  bool
	fail     error // what the next request to hold fails with, as a daemon's might
	err      error // for Err
}

func (f *fakeQueue) Add(string)                    {}
func (f *fakeQueue) Cancel(int)                    {}
func (f *fakeQueue) Retry(int)                     {}
func (f *fakeQueue) SetPaused(bool)                {}
func (f *fakeQueue) Paused() bool                  { return false }
func (f *fakeQueue) Holding() bool                 { return f.holding }
func (f *fakeQueue) Totals() (int64, int)          { return 0, 0 }
func (f *fakeQueue) Jobs() []encode.Job            { return nil }
func (f *fakeQueue) Next() []encode.Event          { return nil }
func (f *fakeQueue) Settings() encode.Settings     { return f.settings }
func (f *fakeQueue) SetSettings(s encode.Settings) { f.settings = s; f.saves++ }
func (f *fakeQueue) Watching() bool                { return true }
func (f *fakeQueue) Daemon() bool                  { return false }
func (f *fakeQueue) Devices() []encode.Device      { return f.devices }
func (f *fakeQueue) Err() error                    { err := f.err; f.err = nil; return err }

func (f *fakeQueue) SetHolding(hold bool) {
	if f.err, f.fail = f.fail, nil; f.err == nil {
		f.holding = hold
	}
}

// newTestModel is a sized model whose settings are saved under a temporary
// folder, not the user's.
func newTestModel(t *testing.T) (Model, *fakeQueue) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	q := &fakeQueue{settings: encode.DefaultSettings()}
	m := New(q)
	m = update(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	return m, q
}

func update(m Model, msg tea.Msg) Model {
	next, _ := m.Update(msg)
	return next.(Model)
}

// press sends keys as the terminal names them: "tab", "ctrl+s", or a
// printable character.
func press(m Model, keys ...string) Model {
	for _, k := range keys {
		m = update(m, keyPress(k))
	}
	return m
}

func keyPress(k string) tea.KeyPressMsg {
	switch k {
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "ctrl+s":
		return tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	}
	return tea.KeyPressMsg{Code: []rune(k)[0], Text: k}
}

// Enter walks the fields and on through the sections, but only ctrl+s saves.
func TestEnterMovesOnAndNeverSaves(t *testing.T) {
	m, q := newTestModel(t)
	m = press(m, "s")
	if m.screen != screenSettings || m.section != sectionVideo {
		t.Fatalf("settings didn't open on Video: screen %d, section %d", m.screen, m.section)
	}
	m = press(m, "tab")
	if m.section != sectionEncoders {
		t.Fatalf("tab went to section %d, not Encoders", m.section)
	}
	m = press(m, "shift+tab", "shift+tab")
	if m.section != sectionFolders {
		t.Fatalf("shift+tab from Video went to section %d, not round to Folders", m.section)
	}
	m = press(m, "tab") // Video
	for range 30 {
		m = press(m, "enter")
	}
	if m.screen != screenSettings || q.saves != 0 {
		t.Errorf("enter left the settings or saved them: screen %d, %d saves", m.screen, q.saves)
	}
}

func TestFoldersAreEditedInTheDraftAndSavedWithIt(t *testing.T) {
	m, q := newTestModel(t)
	films, tv := t.TempDir(), t.TempDir()
	m = press(m, "s", "shift+tab")
	if m.section != sectionFolders {
		t.Fatalf("in section %d, not Folders", m.section)
	}
	m.watchFolder(films) // as the folder browser does
	m = press(m, "t", "+")
	m.watchFolder(tv)
	m = press(m, "t", "t", "-", "-")
	m.watchFolder("/elsewhere")
	m = press(m, "d")
	if len(q.settings.Watch) != 0 {
		t.Fatal("the folders were saved before ctrl+s")
	}

	m = press(m, "ctrl+s")
	want := []encode.WatchFolder{
		{Path: films, Kind: encode.FolderMovies, Priority: 1},
		{Path: tv, Kind: encode.FolderShows, Priority: -2},
	}
	if m.screen != screenQueue || !slices.Equal(q.settings.Watch, want) {
		t.Errorf("saved %+v (screen %d), want %+v", q.settings.Watch, m.screen, want)
	}
}

func TestEscAsksBeforeDiscardingChanges(t *testing.T) {
	m, q := newTestModel(t)
	m = press(m, "s", "shift+tab")
	m.watchFolder(t.TempDir())
	m = press(m, "esc")
	if m.screen != screenSettings || !m.confirmDiscard {
		t.Fatal("esc with a change closed the settings without asking")
	}
	m = press(m, "esc")
	if m.screen != screenQueue || q.saves != 0 {
		t.Errorf("a second esc didn't discard: screen %d, %d saves", m.screen, q.saves)
	}

	m = press(m, "s", "esc")
	if m.screen != screenQueue {
		t.Error("esc without changes asked anyway")
	}
}

// Saving something that can't be saved shows why, in the section to fix.
func TestSavingABadValueOpensItsSection(t *testing.T) {
	m, q := newTestModel(t)
	m = press(m, "s")
	m.draft.OutDir = "/nonexistent/folder"
	m = press(m, "ctrl+s")
	if m.screen != screenSettings || m.section != sectionOutput || q.saves != 0 {
		t.Errorf("screen %d, section %d, %d saves; want the Output section open and nothing saved",
			m.screen, m.section, q.saves)
	}
	if !strings.Contains(m.status, "output folder") {
		t.Errorf("status %q doesn't say what's wrong", m.status)
	}
}

// Fields that don't apply aren't shown, and come back with the choice that
// makes them apply.
func TestFieldsFollowTheChoicesTheyDependOn(t *testing.T) {
	m, q := newTestModel(t)
	q.devices = testDevices
	m = press(m, "s", "tab") // Encoders
	has := func(key string) bool {
		for _, f := range m.sectionFields(m.section) {
			if f.GetKey() == key {
				return true
			}
		}
		return false
	}
	if has("preset") || !has("nvencPreset") {
		t.Error("with the GPU encoding and the CPU left to its default, only the NVIDIA presets should show")
	}
	m.draft.slots[0] = 0 // the GPU off, so the CPU takes over
	if !has("preset") || has("nvencPreset") {
		t.Error("with the GPU off, only the CPU presets should show")
	}

	m = press(m, "tab") // Audio
	m.draft.audio.kbps = encode.AudioCopy
	if has("downmix") {
		t.Error("downmixing was offered for copied audio")
	}
	m.draft.audio = audioChoice{encode.Opus, encode.AudioAuto}
	if !has("downmix") {
		t.Error("downmixing wasn't offered for encoded audio")
	}
}

func TestStepsShowOnlyCav1arsOwnLines(t *testing.T) {
	m, _ := newTestModel(t)
	job := encode.Job{ID: 0, Input: "/films/a.mkv", State: encode.Running}
	m.apply([]encode.Event{
		{Job: job, Line: "cav1ar: ▸ encoding the video"},
		{Job: job, Line: "Stream #0:0 -> #0:0 (h264 -> av1_nvenc)"},
		{Job: job, Line: "cav1ar: plan: video H.264 → AV1"},
	})
	if got := len(m.logLines(0)); got != 2 {
		t.Errorf("the steps show %d lines, want cav1ar's 2", got)
	}
	m = press(m, "enter")
	if got := len(m.logLines(0)); got != 3 {
		t.Errorf("the full log shows %d lines, want all 3", got)
	}
}

var testDevices = []encode.Device{
	{ID: "nvidia:0000:01:00.0", Kind: encode.EncoderNVIDIA, Name: "Test GPU"},
	{ID: "cpu", Kind: encode.EncoderCPU, Name: "CPU"},
}

// Settings that name devices by kind, as older versions' are carried over,
// stay as they are until a device's number is changed, which then names
// each device by its ID.
func TestDeviceNumbersAreSavedByID(t *testing.T) {
	m, q := newTestModel(t)
	q.devices = testDevices
	q.settings.Encoders = map[string]int{"nvidia": 1}
	m.settings = q.settings
	m = press(m, "s")
	if m.draft.slots[0] != 1 || m.draft.slots[1] != slotsAuto {
		t.Fatalf("the form shows %v, want [1 auto]", m.draft.slots)
	}
	if m.draftChanged() {
		t.Fatal("opening the settings counted as a change")
	}
	m.draft.slots[1] = 2
	m = press(m, "ctrl+s")
	want := map[string]int{"nvidia:0000:01:00.0": 1, "cpu": 2}
	if !maps.Equal(q.settings.Encoders, want) {
		t.Errorf("saved %v, want %v", q.settings.Encoders, want)
	}
}

func TestEveryEncoderOffIsRefused(t *testing.T) {
	m, q := newTestModel(t)
	q.devices = testDevices
	m = press(m, "s")
	m.draft.slots = []int{0, 0}
	m = press(m, "ctrl+s")
	if q.saves != 0 || !strings.Contains(m.status, "every encoder is off") {
		t.Errorf("%d saves, status %q; want it refused", q.saves, m.status)
	}
}

// Encoders are listed by machine. The CPU of a worker with a GPU shows only
// once it's on; that of a worker with only a CPU always does.
func TestEncodersAreGroupedByMachine(t *testing.T) {
	m, q := newTestModel(t)
	q.devices = append(slices.Clone(testDevices),
		encode.Device{ID: "w/vaapi:x", Kind: encode.EncoderVAAPI, Name: "Test Arc (w)", Worker: "w"},
		encode.Device{ID: "w/cpu", Kind: encode.EncoderCPU, Name: "CPU (w)", Worker: "w"},
		encode.Device{ID: "c/cpu", Kind: encode.EncoderCPU, Name: "CPU (c)", Worker: "c"})
	m = press(m, "s", "tab")
	var got []string
	for _, g := range m.draft.encoderGroups() {
		got = append(got, g.title, fmt.Sprint(g.devices))
	}
	if want := []string{"── This machine", "[0 1]", "── w · worker", "[2]", "── c · worker", "[4]"}; !slices.Equal(got, want) {
		t.Errorf("groups %q, want %q", got, want)
	}
	m.draft.slots[3] = 1
	if g := m.draft.encoderGroups(); len(g) != 3 || len(g[1].devices) != 2 {
		t.Error("a worker's CPU that's on should show")
	}
}

// An AMD/Intel GPU's speed shows under it, as NVIDIA's does under theirs.
func TestVAAPISpeedShowsUnderItsGPU(t *testing.T) {
	m, q := newTestModel(t)
	q.devices = append(slices.Clone(testDevices),
		encode.Device{ID: "w/vaapi:x", Kind: encode.EncoderVAAPI, Name: "Test Arc (w)", Worker: "w"})
	m = press(m, "s", "tab")
	var keys []string
	for _, f := range m.sectionFields(m.section) {
		keys = append(keys, f.GetKey())
	}
	if i := slices.Index(keys, "slots2"); i < 0 || i+1 >= len(keys) || keys[i+1] != "vaapiPreset" {
		t.Errorf("fields %q; want vaapiPreset right after the Arc", keys)
	}
}

// Saving changes only what the screen changed, so what changed elsewhere
// while it was open, such as the web UI's password and watched folders,
// stays changed.
func TestSavingKeepsChangesMadeElsewhere(t *testing.T) {
	m, q := newTestModel(t)
	m = press(m, "s")
	elsewhere := q.settings
	elsewhere.WebPassword = "hash"
	elsewhere.Watch = []encode.WatchFolder{{Path: t.TempDir(), Kind: encode.FolderMovies}}
	if err := config.Save(elsewhere); err != nil {
		t.Fatal(err)
	}
	q.settings = elsewhere // as a daemon tells the UI

	m.draft.crf = "28"
	m = press(m, "ctrl+s")
	saved, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]encode.Settings{"saved": saved, "applied": q.settings} {
		if s.CRF != 28 || s.WebPassword != "hash" || !slices.Equal(s.Watch, elsewhere.Watch) {
			t.Errorf("%s CRF %d, password %q, folders %v; want 28 and what was changed elsewhere",
				name, s.CRF, s.WebPassword, s.Watch)
		}
	}
}

// Every setting is either edited in a section, or one the screen leaves
// alone, so none is left out of saving.
func TestEverySettingIsInASectionOrLeftAlone(t *testing.T) {
	leftAlone := []string{"WorkerListen", "WorkerDir", "WebListen", "WebPassword"}
	typ := reflect.TypeFor[encode.Settings]()
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		if _, ok := sectionOf[name]; ok == slices.Contains(leftAlone, name) {
			t.Errorf("%s should be in one of sectionOf and the settings the screen leaves alone", name)
		}
	}
}

// A value that only the settings' own checks catch opens its section.
func TestInvalidSettingsOpenTheirSection(t *testing.T) {
	m, q := newTestModel(t)
	q.devices = testDevices
	m = press(m, "s")
	m.draft.slots[1] = encode.MaxSlots + 1
	m = press(m, "ctrl+s")
	if m.screen != screenSettings || m.section != sectionEncoders || q.saves != 0 {
		t.Errorf("screen %d, section %d, %d saves; want the Encoders section open and nothing saved",
			m.screen, m.section, q.saves)
	}
}

// The screen takes the language codes the encoder does, and no others.
func TestLanguageCodesAreThoseTheEncoderTakes(t *testing.T) {
	for code, ok := range map[string]bool{"eng": true, "EN": true, " fr ": true,
		"é": false, "日本": false, "e1": false, "e": false, "engl": false} {
		s := encode.DefaultSettings()
		s.Language = strings.ToLower(strings.TrimSpace(code))
		if (validateLanguage(code) == nil) != ok || (s.Validate() == nil) != ok {
			t.Errorf("%q: the screen says %v and the encoder %v; want both to accept it: %v",
				code, validateLanguage(code), s.Validate(), ok)
		}
	}
}

// Recommended leaves what to do with videos already in a folder as it was,
// so neither choice is marked as the recommended one.
func TestWatchExistingIsntRecommendedBothWays(t *testing.T) {
	m, _ := newTestModel(t)
	m = press(m, "s", "shift+tab")
	marked := 0
	for _, existing := range []bool{false, true} {
		m.draft.WatchExisting = existing
		if view, _ := m.foldersView(200); strings.Contains(view, "(recommended)") {
			marked++
		}
	}
	if marked > 1 {
		t.Error("both choices are marked as recommended")
	}
}
