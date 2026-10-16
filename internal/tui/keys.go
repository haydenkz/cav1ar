package tui

import "charm.land/bubbles/v2/key"

type keyMap struct {
	// Queue screen.
	Up, Down       key.Binding
	LogUp, LogDown key.Binding
	FullLog        key.Binding
	Cancel, Retry  key.Binding
	HideDone       key.Binding
	Add, Settings  key.Binding
	Pause          key.Binding // once the running jobs finish
	PauseNow       key.Binding // suspending the running jobs
	Help, Quit     key.Binding

	// File browser.
	AddItem, AddAll key.Binding
	WatchHere       key.Binding // choose a folder to watch

	// Settings screen.
	NextSection, PrevSection key.Binding
	Save, Recommend          key.Binding
	AddFolder, RemoveFolder  key.Binding
	FolderKind               key.Binding
	Raise, Lower             key.Binding // a folder's priority
	Toggle                   key.Binding

	// Everywhere.
	Back key.Binding
	Kill key.Binding
}

func defaultKeys() keyMap {
	return keyMap{
		Up:           key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/↓", "select")),
		Down:         key.NewBinding(key.WithKeys("down", "j")),
		LogUp:        key.NewBinding(key.WithKeys("pgup", "ctrl+u"), key.WithHelp("pgup/pgdn", "scroll the log")),
		LogDown:      key.NewBinding(key.WithKeys("pgdown", "ctrl+d")),
		FullLog:      key.NewBinding(key.WithKeys("enter", "l"), key.WithHelp("enter", "full log")),
		Cancel:       key.NewBinding(key.WithKeys("x", "delete"), key.WithHelp("x", "cancel")),
		Retry:        key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "retry")),
		HideDone:     key.NewBinding(key.WithKeys("h"), key.WithHelp("h", "hide done")),
		Add:          key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add")),
		Settings:     key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "settings")),
		Pause:        key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "pause")),
		PauseNow:     key.NewBinding(key.WithKeys("P"), key.WithHelp("P", "pause now")),
		Help:         key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "more")),
		Quit:         key.NewBinding(key.WithKeys("q"), key.WithHelp("q", "quit")),
		AddItem:      key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add highlighted")),
		AddAll:       key.NewBinding(key.WithKeys("A"), key.WithHelp("A", "add this folder")),
		WatchHere:    key.NewBinding(key.WithKeys("space", "w"), key.WithHelp("space", "watch this folder")),
		NextSection:  key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "section")),
		PrevSection:  key.NewBinding(key.WithKeys("shift+tab")),
		Save:         key.NewBinding(key.WithKeys("ctrl+s"), key.WithHelp("ctrl+s", "save")),
		Recommend:    key.NewBinding(key.WithKeys("ctrl+r"), key.WithHelp("ctrl+r", "recommended")),
		AddFolder:    key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add")),
		RemoveFolder: key.NewBinding(key.WithKeys("d", "x", "delete", "backspace"), key.WithHelp("d", "remove")),
		FolderKind:   key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "movies/shows")),
		Raise:        key.NewBinding(key.WithKeys("+", "="), key.WithHelp("+/-", "priority")),
		Lower:        key.NewBinding(key.WithKeys("-", "_")),
		Toggle:       key.NewBinding(key.WithKeys("left", "right", "h", "l", "space", "enter"), key.WithHelp("←/→", "change")),
		Back:         key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
		Kill:         key.NewBinding(key.WithKeys("ctrl+c")),
	}
}

func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Add, k.Settings, k.Pause, k.FullLog, k.Help}
}

func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.FullLog, k.LogUp},
		{k.Add, k.Settings},
		{k.Pause, k.PauseNow},
		{k.Cancel, k.Retry, k.HideDone},
		{k.Help, k.Quit},
	}
}

func (k keyMap) pickerHelp(mode pickMode) []key.Binding {
	if mode == pickWatch {
		return []key.Binding{hint("enter", "open folder"), k.WatchHere, hint("←", "parent"), hint("esc", "back to settings")}
	}
	return []key.Binding{hint("enter", "add file / open folder"), k.AddItem, k.AddAll, hint("←", "parent"), k.Back}
}

func (k keyMap) settingsHelp(folders bool) []key.Binding {
	if folders {
		return []key.Binding{hint("↑/↓", "select"), k.AddFolder, k.RemoveFolder, k.FolderKind, k.Raise,
			k.NextSection, k.Save, hint("esc", "close")}
	}
	return []key.Binding{hint("↑/↓", "field"), hint("←/→", "change"), k.NextSection, k.Save, k.Recommend, hint("esc", "close")}
}

// hint is a binding that only appears in help. Its keys are handled by a
// component that has its own key map.
func hint(keys, desc string) key.Binding {
	return key.NewBinding(key.WithKeys(keys), key.WithHelp(keys, desc))
}
