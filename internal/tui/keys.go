package tui

import "charm.land/bubbles/v2/key"

type keyMap struct {
	// Queue screen.
	Up, Down       key.Binding
	LogUp, LogDown key.Binding
	Cancel, Retry  key.Binding
	Add, Settings  key.Binding
	Pause          key.Binding
	Help, Quit     key.Binding

	// Add-files screen.
	AddItem, AddAll, Watch key.Binding

	// Settings screen.
	Save, Recommend key.Binding

	// Everywhere.
	Back key.Binding
	Kill key.Binding
}

func defaultKeys() keyMap {
	return keyMap{
		Up:        key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:      key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		LogUp:     key.NewBinding(key.WithKeys("pgup", "ctrl+u"), key.WithHelp("pgup", "scroll log up")),
		LogDown:   key.NewBinding(key.WithKeys("pgdown", "ctrl+d"), key.WithHelp("pgdn", "scroll log down")),
		Cancel:    key.NewBinding(key.WithKeys("x", "delete"), key.WithHelp("x", "cancel")),
		Retry:     key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "retry")),
		Add:       key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add files")),
		Settings:  key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "settings")),
		Pause:     key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "pause")),
		Help:      key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "more")),
		Quit:      key.NewBinding(key.WithKeys("q"), key.WithHelp("q", "quit")),
		AddItem:   key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add highlighted (folders too)")),
		AddAll:    key.NewBinding(key.WithKeys("A"), key.WithHelp("A", "add this whole folder")),
		Watch:     key.NewBinding(key.WithKeys("w"), key.WithHelp("w", "watch folder")),
		Save:      key.NewBinding(key.WithKeys("ctrl+s"), key.WithHelp("ctrl+s", "save")),
		Recommend: key.NewBinding(key.WithKeys("ctrl+r"), key.WithHelp("ctrl+r", "use recommended")),
		Back:      key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
		Kill:      key.NewBinding(key.WithKeys("ctrl+c")),
	}
}

func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Add, k.Settings, k.Pause, k.Cancel, k.Retry, k.Help, k.Quit}
}

func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down},
		{k.LogUp, k.LogDown},
		{k.Add, k.Settings, k.Pause},
		{k.Cancel, k.Retry},
		{k.Help, k.Quit},
	}
}

func (k keyMap) pickerHelp() []key.Binding {
	return []key.Binding{
		hint("↑/↓", "move"),
		hint("enter", "add file / open folder"),
		k.AddItem,
		k.AddAll,
		k.Watch,
		hint("←", "parent folder"),
		k.Back,
	}
}

func (k keyMap) settingsHelp() []key.Binding {
	return []key.Binding{
		hint("enter", "next"),
		hint("shift+tab", "previous"),
		hint("←/→", "change"),
		k.Recommend,
		k.Save,
		hint("esc", "cancel"),
	}
}

// hint is a binding that only appears in help. Its keys are handled by a
// component that has its own key map.
func hint(keys, desc string) key.Binding {
	return key.NewBinding(key.WithKeys(keys), key.WithHelp(keys, desc))
}
