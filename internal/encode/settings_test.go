package encode

import (
	"strings"
	"testing"
)

func TestValidateChecksWatchedFolders(t *testing.T) {
	for _, tc := range []struct {
		watch []WatchFolder
		ok    bool
	}{
		{nil, true},
		{[]WatchFolder{{Path: "/media/Shows", Kind: FolderShows}, {Path: "/media/Shows/Example Show"}}, true},
		{[]WatchFolder{{Path: ""}}, false},
		{[]WatchFolder{{Path: "media/Shows"}}, false},
		{[]WatchFolder{{Path: "/media/Shows/"}}, false},
		{[]WatchFolder{{Path: "/media/../Shows"}}, false},
		{[]WatchFolder{{Path: "/media/Shows"}, {Path: "/media/Shows", Kind: FolderShows}}, false},
		{[]WatchFolder{{Path: "/media/Shows", Kind: "cartoons"}}, false},
	} {
		s := DefaultSettings()
		s.Watch = tc.watch
		if err := s.Validate(); (err == nil) != tc.ok {
			t.Errorf("watching %+v: got %v", tc.watch, err)
		}
	}
}

func TestValidateChecksTheWebUIAddress(t *testing.T) {
	for addr, ok := range map[string]bool{
		"":               true, // no web UI
		":6767":          true,
		"127.0.0.1:6767": true,
		"[::1]:6767":     true,
		"localhost:80":   true,
		"6767":           false,
		":0":             false,
		":65536":         false,
		":http":          false,
		"localhost:":     false,
	} {
		s := DefaultSettings()
		s.WebListen = addr
		if err := s.Validate(); (err == nil) != ok {
			t.Errorf("web_listen %q: got %v", addr, err)
		}
	}
}

func TestValidateTakesEveryAudioBitrateChoice(t *testing.T) {
	for _, kbps := range []int{AudioAuto, AudioCopy, 128} {
		s := DefaultSettings()
		s.AudioKbps = kbps
		if err := s.Validate(); err != nil {
			t.Errorf("audio %d: %v", kbps, err)
		}
	}
	s := DefaultSettings()
	s.AudioKbps = -2
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "copy") {
		t.Errorf("audio -2: got %v, want an error that says 0 copies the audio", err)
	}
}
