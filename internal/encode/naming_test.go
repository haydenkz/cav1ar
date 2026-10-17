package encode

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A season pack's folder names its show, not "Show S01".
func TestParseFolderLeavesTheSeasonOutOfTheTitle(t *testing.T) {
	for _, c := range []struct{ name, title, year string }{
		{"Show.Name.S01.1080p.BluRay.x265.10Bit-GRP", "Show Name", ""},
		{"Show.Name.2019.S03.720p.WEB-DL-GRP", "Show Name", "2019"},
		{"Show Name S02 Complete 1080p", "Show Name", ""},
		{"Show.Name.S01E02.1080p.WEB.h264-GRP", "Show Name", ""},
		{"Open Season 2 (2008)", "Open Season 2", "2008"},
		{"Show Name", "Show Name", ""},
	} {
		r := parseFolder(c.name)
		if r.Title != c.title || r.Year != c.year {
			t.Errorf("parseFolder(%q) = %q (%q), want %q (%q)", c.name, r.Title, r.Year, c.title, c.year)
		}
	}
}

// A name too long for the filesystem is cut short, keeping its extension
// and whole characters.
func TestFileNameFitsTheLimit(t *testing.T) {
	r := parseName("Example Movie 2020")
	for _, template := range []string{
		"{title} " + strings.Repeat("é", 200),
		"{title} " + strings.Repeat("x", 300),
		"{title} " + strings.Repeat("x", 235) + " - xxxx", // cut just after the dash
	} {
		name := fileName(template, r)
		switch {
		case len(name) > maxName:
			t.Errorf("%q is %d bytes", name, len(name))
		case !utf8.ValidString(name):
			t.Errorf("%q splits a character", name)
		case !strings.HasSuffix(name, ".mkv") || strings.HasSuffix(name, " .mkv") || strings.HasSuffix(name, "-.mkv"):
			t.Errorf("%q has lost its extension, or ends untidily", name)
		}
	}
	if name := fileName("{title} ({year})", r); name != "Example Movie (2020).mkv" {
		t.Errorf("a short name was changed: %q", name)
	}
}
