package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/filepicker"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/haydenkz/cav1ar/internal/encode"
)

// pickMode is what the file browser is open for.
type pickMode int

const (
	pickAdd   pickMode = iota // queuing files
	pickWatch                 // choosing a folder to watch, for the settings
)

func newPicker() filepicker.Model {
	p := filepicker.New()
	if wd, err := os.Getwd(); err == nil {
		p.CurrentDirectory = wd
	}
	for _, ext := range encode.VideoExts {
		p.AllowedTypes = append(p.AllowedTypes, ext, strings.ToUpper(ext))
	}
	p.ShowPermissions = false
	p.AutoHeight = false
	// esc leaves the picker instead of going up a folder.
	p.KeyMap.Back = key.NewBinding(key.WithKeys("h", "backspace", "left"))
	return p
}

// openPicker shows the file browser for mode.
func (m *Model) openPicker(mode pickMode) {
	m.pick = mode
	m.screen = screenAdd
}

func (m *Model) updatePicker(msg tea.Msg) tea.Cmd {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch {
		case key.Matches(k, m.keys.Back):
			if m.pick == pickWatch {
				m.screen = screenSettings
			} else {
				m.screen = screenQueue
			}
			return nil
		case m.pick == pickWatch && key.Matches(k, m.keys.WatchHere):
			dir := m.picker.CurrentDirectory
			if p := m.picker.HighlightedPath(); p != "" {
				if fi, err := os.Stat(p); err == nil && fi.IsDir() {
					dir = p
				}
			}
			m.watchFolder(dir)
			m.screen = screenSettings
			return m.setStatus("Watching "+shortPath(dir)+" once you save. Press t to name videos as movies or shows.", false)
		case m.pick == pickAdd && key.Matches(k, m.keys.AddItem):
			return m.addPath(m.picker.HighlightedPath())
		case m.pick == pickAdd && key.Matches(k, m.keys.AddAll):
			return m.addPath(m.picker.CurrentDirectory)
		}
	}

	var cmd tea.Cmd
	m.picker, cmd = m.picker.Update(msg)
	if m.pick != pickAdd {
		return cmd
	}
	if ok, path := m.picker.DidSelectFile(msg); ok {
		return tea.Batch(cmd, m.addFiles(path))
	}
	if ok, path := m.picker.DidSelectDisabledFile(msg); ok {
		return tea.Batch(cmd, m.setStatus(filepath.Base(path)+" isn't a video file", true))
	}
	return cmd
}

// addPath queues a file, or every video in a folder and its subfolders.
func (m *Model) addPath(path string) tea.Cmd {
	if path == "" {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return m.setStatus(err.Error(), true)
	}
	if !fi.IsDir() {
		if !encode.IsVideo(path) {
			return m.setStatus(filepath.Base(path)+" isn't a video file", true)
		}
		return m.addFiles(path)
	}
	paths, err := encode.FindVideos(path)
	if err != nil {
		return m.setStatus(err.Error(), true)
	}
	if len(paths) == 0 {
		return m.setStatus("No videos in "+filepath.Base(path), true)
	}
	return m.addFiles(paths...)
}

// addFiles queues paths, skipping any already in the list. Finished jobs are
// rerun with retry instead, so the list never holds the same file twice.
func (m *Model) addFiles(paths ...string) tea.Cmd {
	// Ask the queue rather than m.jobs, which lags until its events arrive.
	listed := map[string]bool{}
	for _, j := range m.queue.Jobs() {
		listed[j.Input] = true
	}
	var added []string
	for _, p := range paths {
		if listed[p] {
			continue
		}
		listed[p] = true
		m.queue.Add(p)
		added = append(added, filepath.Base(p))
	}
	switch len(added) {
	case 0:
		return m.setStatus(plural(len(paths), "That file is", "Those files are")+" already in the list", true)
	case 1:
		return m.setStatus("Added "+added[0], false)
	}
	return m.setStatus(fmt.Sprintf("Added %d files", len(added)), false)
}

// shortPath abbreviates the home directory to ~.
func shortPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || (path != home && !strings.HasPrefix(path, home+string(filepath.Separator))) {
		return path
	}
	return "~" + strings.TrimPrefix(path, home)
}
