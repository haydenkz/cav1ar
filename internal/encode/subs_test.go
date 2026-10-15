package encode

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Reading subtitles reads the start of the whole file, so a file whose
// tracks are told apart by their flags isn't read, even when it is encoded.
func TestPrepareReadsSubtitlesOnlyWhenTracksLookAlike(t *testing.T) {
	dir := t.TempDir()
	one := makeSource(t, dir) // one English track
	two := filepath.Join(dir, "Two Tracks (2020).mkv")
	ffmpeg(t, "-i", one, "-map", "0", "-map", "0:s", "-c", "copy", "-metadata:s:s:1", "language=eng", two)

	s := testSettings(t.TempDir())
	s.TidySubs = true
	ctx := context.Background()
	prepared := func(path string) (*plan, bool) {
		t.Helper()
		var read bool
		p, err := prepare(ctx, path, s, func(line string) { read = read || strings.Contains(line, "reading the subtitles") })
		if err != nil {
			t.Fatal(err)
		}
		return p, read
	}

	p, read := prepared(one)
	if p.action() != ActionEncode {
		t.Fatalf("want the source encoded, got %s", p.action())
	}
	if read {
		t.Error("read the subtitles of a file with one track")
	}

	p, read = prepared(two)
	if !read {
		t.Error("didn't read the subtitles of a file with two English tracks that look alike")
	}
	if len(p.subs) != 1 || !slices.ContainsFunc(p.notes, func(n string) bool { return strings.Contains(n, "a second eng full track") }) {
		t.Errorf("want the duplicate track dropped, got %d tracks and notes %q", len(p.subs), p.notes)
	}
}
