package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"charm.land/bubbles/v2/filepicker"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/haydenkz/cav1ar/internal/encode"
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

func (m *Model) updatePicker(msg tea.Msg) tea.Cmd {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch {
		case key.Matches(k, m.keys.Back):
			m.screen = screenQueue
			return nil
		case key.Matches(k, m.keys.AddItem):
			return m.addPath(m.picker.HighlightedPath())
		case key.Matches(k, m.keys.AddAll):
			return m.addPath(m.picker.CurrentDirectory)
		case key.Matches(k, m.keys.Watch):
			dir := m.picker.CurrentDirectory
			if p := m.picker.HighlightedPath(); p != "" {
				if fi, err := os.Stat(p); err == nil && fi.IsDir() {
					dir = p
				}
			}
			return m.toggleWatch(dir)
		}
	}

	var cmd tea.Cmd
	m.picker, cmd = m.picker.Update(msg)
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

// toggleWatch moves dir through not watched, watched, watched as movies and
// watched as shows, and round again.
func (m *Model) toggleWatch(dir string) tea.Cmd {
	s := m.settings
	s.Watch = slices.Clone(s.Watch)
	i := slices.IndexFunc(s.Watch, func(f encode.WatchFolder) bool { return f.Path == dir })
	var msg string
	switch {
	case i < 0:
		s.Watch = append(s.Watch, encode.WatchFolder{Path: dir})
		msg = "Watching " + filepath.Base(dir) + " and its subfolders. Press w again to name videos after movie folders."
		if !s.WatchExisting {
			msg += " Videos already there are left alone; press a to add them."
		}
	case s.Watch[i].Kind == encode.FolderAny:
		s.Watch[i].Kind = encode.FolderMovies
		msg = "Watching " + filepath.Base(dir) + " as movies, named after their folders. Press w again for shows."
	case s.Watch[i].Kind == encode.FolderMovies:
		s.Watch[i].Kind = encode.FolderShows
		msg = "Watching " + filepath.Base(dir) + " as shows, named after their show folders. Press w again to stop."
	default:
		s.Watch = slices.Delete(s.Watch, i, i+1)
		msg = "Stopped watching " + filepath.Base(dir)
	}
	if err := m.saveSettings(s); err != nil {
		return m.setStatus("Couldn't save the watch list: "+err.Error(), true)
	}
	return m.setStatus(msg, false)
}

// shortPath abbreviates the home directory to ~.
func shortPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || (path != home && !strings.HasPrefix(path, home+string(filepath.Separator))) {
		return path
	}
	return "~" + strings.TrimPrefix(path, home)
}
