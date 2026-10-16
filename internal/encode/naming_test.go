package encode

import "testing"

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
