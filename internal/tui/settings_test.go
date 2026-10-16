package tui

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/haydenkz/cav1ar/internal/encode"
)

// fakeQueue is a Backend that records the settings it is given.
type fakeQueue struct {
	settings encode.Settings
	saves    int
}

func (f *fakeQueue) Add(string)                    {}
func (f *fakeQueue) Cancel(int)                    {}
func (f *fakeQueue) Retry(int)                     {}
func (f *fakeQueue) SetPaused(bool)                {}
func (f *fakeQueue) Paused() bool                  { return false }
func (f *fakeQueue) SetHolding(bool)               {}
func (f *fakeQueue) Holding() bool                 { return false }
func (f *fakeQueue) Totals() (int64, int)          { return 0, 0 }
func (f *fakeQueue) Jobs() []encode.Job            { return nil }
func (f *fakeQueue) Next() []encode.Event          { return nil }
func (f *fakeQueue) Settings() encode.Settings     { return f.settings }
func (f *fakeQueue) SetSettings(s encode.Settings) { f.settings = s; f.saves++ }
func (f *fakeQueue) Watching() bool                { return true }
func (f *fakeQueue) Daemon() bool                  { return false }

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

// needEncoder skips a test that saves, since saving checks the encoder works.
func needEncoder(t *testing.T) {
	t.Helper()
	if err := encode.CheckEncoder(encode.DefaultSettings().Encoder); err != nil {
		t.Skip(err)
	}
}

func update(m Model, msg tea.Msg) Model {
	next, _ := m.Update(msg)
	return next.(Model)
}

// press sends keys as the terminal names them: "tab", "ctrl+s", or a
// printable character.
func press(m Model, keys ...string) Model {
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "tab":
			msg = tea.KeyPressMsg{Code: tea.KeyTab}
		case "shift+tab":
			msg = tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "up":
			msg = tea.KeyPressMsg{Code: tea.KeyUp}
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		case "ctrl+s":
			msg = tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}
		default:
			r := []rune(k)[0]
			msg = tea.KeyPressMsg{Code: r, Text: k}
		}
		m = update(m, msg)
	}
	return m
}

// Enter walks the fields and on through the sections, but only ctrl+s saves.
func TestEnterMovesOnAndNeverSaves(t *testing.T) {
	m, q := newTestModel(t)
	m = press(m, "s")
	if m.screen != screenSettings || m.section != sectionVideo {
		t.Fatalf("settings didn't open on Video: screen %d, section %d", m.screen, m.section)
	}
	m = press(m, "tab")
	if m.section != sectionAudio {
		t.Fatalf("tab went to section %d, not Audio", m.section)
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
	needEncoder(t)
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
	needEncoder(t)
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
	m, _ := newTestModel(t)
	m = press(m, "s")
	has := func(key string) bool {
		for _, f := range m.sectionFields(m.section) {
			if f.GetKey() == key {
				return true
			}
		}
		return false
	}
	m.draft.Encoder = encode.EncoderCPU
	if !has("preset") || has("nvencPreset") {
		t.Error("the CPU encoder should offer only the CPU presets")
	}
	m.draft.Encoder = encode.EncoderNVIDIA
	if has("preset") || !has("nvencPreset") {
		t.Error("the NVIDIA encoder should offer only its presets")
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
		{Job: job, Line: "cav1ar: encode, because of: video h264"},
	})
	if got := len(m.logLines(0)); got != 2 {
		t.Errorf("the steps show %d lines, want cav1ar's 2", got)
	}
	m = press(m, "enter")
	if got := len(m.logLines(0)); got != 3 {
		t.Errorf("the full log shows %d lines, want all 3", got)
	}
}
