package config

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/haydenkz/cav1ar/internal/encode"
)

// Settings saved when there was one encoder for everything apply it to
// devices of its kind, and choosing the CPU keeps the GPUs out.
func TestOldEncoderSettingsCarryOver(t *testing.T) {
	for _, c := range []struct {
		saved string
		want  map[string]int
	}{
		{`{"encoder": "nvidia", "workers": 1}`, map[string]int{"nvidia": 1}},
		{`{"encoder": "cpu", "workers": 2}`, map[string]int{"cpu": 2, "nvidia": 0, "vaapi": 0}},
		{`{"encoder": "nvidia", "workers": 1, "encoders": {"cpu": 0}}`, map[string]int{"cpu": 0}}, // already new
		{`{"crf": 30}`, nil},
	} {
		s := encode.DefaultSettings()
		if err := json.Unmarshal([]byte(c.saved), &s); err != nil {
			t.Fatal(err)
		}
		migrate(&s, []byte(c.saved))
		if !maps.Equal(s.Encoders, c.want) {
			t.Errorf("%s: got %v, want %v", c.saved, s.Encoders, c.want)
		}
	}
}

// The settings hold the web UI's password, so only their owner can read
// them, and saving them over old ones leaves nothing else behind.
func TestSettingsAreTheOwnersAlone(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := encode.DefaultSettings()
	s.WebPassword = "hash"
	for range 2 {
		if err := Save(s); err != nil {
			t.Fatal(err)
		}
	}
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700 | os.ModeDir} {
		if fi, err := os.Stat(p); err != nil {
			t.Error(err)
		} else if fi.Mode() != want {
			t.Errorf("%s is %v, want %v", p, fi.Mode(), want)
		}
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("the settings folder holds %d files, want only the settings", len(entries))
	}
	if got, err := Load(); err != nil || got.WebPassword != "hash" {
		t.Errorf("loaded %q, %v; want the password saved", got.WebPassword, err)
	}
}
