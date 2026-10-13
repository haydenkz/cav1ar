// Package config persists encode settings between runs.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/haydenkz/cav1ar/internal/encode"
)

// Path is where settings are stored, e.g. ~/.config/cav1ar/config.json.
func Path() (string, error) { return file("config.json") }

// WatchStatePath is where the folder watcher remembers the videos it has seen.
func WatchStatePath() (string, error) { return file("watch.json") }

func file(name string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cav1ar", name), nil
}

// Load reads the saved settings, falling back to defaults for anything not
// saved yet.
func Load() (encode.Settings, error) {
	s := encode.DefaultSettings()
	path, err := Path()
	if err != nil {
		return s, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return encode.DefaultSettings(), fmt.Errorf("%s: %w", path, err)
	}
	if err := s.Validate(); err != nil {
		return encode.DefaultSettings(), fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Save writes settings so the next run starts with them.
func Save(s encode.Settings) error {
	path, err := Path()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Write then rename, so a crash never leaves a half-written file.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
