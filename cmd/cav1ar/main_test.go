package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Flags that would be ignored with the others given are refused instead.
func TestFlagsThatCantApplyAreRefused(t *testing.T) {
	for _, c := range []struct {
		given []string
		ok    bool
	}{
		{[]string{"worker", "worker-name", "allow", "map", "ffmpeg"}, true},
		{[]string{"worker", "crf"}, false},
		{[]string{"ffmpeg"}, false},
		{[]string{"map", "crf"}, false},
		{[]string{"allow", "daemon"}, false},
		{[]string{"daemon", "local"}, false},
		{[]string{"local", "crf", "watch"}, true},
		{[]string{"daemon", "web"}, true},
		{[]string{"web"}, false},
	} {
		given := map[string]bool{}
		for _, name := range c.given {
			given[name] = true
		}
		if err := misplacedFlags(given); (err == nil) != c.ok {
			t.Errorf("%v: %v", c.given, err)
		}
	}
}

type adds []string

func (a *adds) Add(path string) { *a = append(*a, path) }

// Paths are queued in full, since a daemon would take a relative one to be
// in its own folder.
func TestAddQueuesFullPaths(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("a.mkv", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var got adds
	for _, path := range []string{"a.mkv", ".", "missing.mkv"} {
		if err := add(&got, path); err != nil {
			t.Fatal(err)
		}
	}
	want := adds{filepath.Join(dir, "a.mkv"), filepath.Join(dir, "a.mkv"), filepath.Join(dir, "missing.mkv")}
	if !slices.Equal(got, want) {
		t.Errorf("queued %q, want %q", got, want)
	}
}
