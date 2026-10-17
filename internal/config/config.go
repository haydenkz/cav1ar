// Package config persists encode settings between runs.
package config

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/haydenkz/cav1ar/internal/encode"
)

// Path is where settings are stored, e.g. ~/.config/cav1ar/config.json.
func Path() (string, error) { return file("config.json") }

// StatePath is where the folder watcher keeps what it knows between runs:
// the videos it has seen, and the jobs that have finished.
func StatePath() (string, error) { return file("cav1ar.db") }

// WorkerToken is the token workers connect to a daemon with, which the
// daemon keeps in its settings folder and a worker in its own, along with
// where it is. With create, a daemon makes one there if there is none.
func WorkerToken(create bool) (token, path string, err error) {
	if path, err = file("worker-token"); err != nil {
		return "", "", err
	}
	if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) != "" {
		return strings.TrimSpace(string(data)), path, nil
	}
	if !create {
		return "", path, fmt.Errorf("no worker token at %s; copy the daemon's there, or set CAV1AR_WORKER_TOKEN", path)
	}
	token = rand.Text()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", path, err
	}
	return token, path, os.WriteFile(path, []byte(token+"\n"), 0o600)
}

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
	migrate(&s, data)
	if err := s.Validate(); err != nil {
		return encode.DefaultSettings(), fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// migrate carries over settings saved before each device got its own: one
// encoder for everything, running some number of files at once. Those apply
// to devices of that kind; the rest keep their defaults, unless the CPU was
// chosen, which then stays the only one.
func migrate(s *encode.Settings, data []byte) {
	var old struct {
		Encoder encode.Encoder `json:"encoder"`
		Workers int            `json:"workers"`
	}
	if json.Unmarshal(data, &old) != nil || old.Encoder == "" || s.Encoders != nil {
		return
	}
	s.Encoders = map[string]int{string(old.Encoder): min(max(old.Workers, 1), encode.MaxSlots)}
	if old.Encoder == encode.EncoderCPU {
		s.Encoders[string(encode.EncoderNVIDIA)] = 0
		s.Encoders[string(encode.EncoderVAAPI)] = 0
	}
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
