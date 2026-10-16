package encode

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// ffmpeg writes DURATION afresh for the file it makes, but copies a source's
// DURATION-eng as it was, so the plain tag is the one to believe, however
// the map happens to be ordered.
func TestDurationTagPrefersThePlainTag(t *testing.T) {
	tags := map[string]string{"DURATION-eng": "01:55:41.518000000", "DURATION": "00:00:02.586000000"}
	for range 50 {
		if got := durationTag(tags); got != 2586*time.Millisecond {
			t.Fatalf("durationTag = %s, want 2.586s", got)
		}
	}
	if got := durationTag(map[string]string{"DURATION-eng": "00:01:00.000000000"}); got != time.Minute {
		t.Errorf("with only DURATION-eng, durationTag = %s, want 1m", got)
	}
}

// A subtitle track the file says is empty is dropped: it has nothing to show,
// and mapping it stalls ffmpeg.
func TestEmptySubtitleTracksAreDropped(t *testing.T) {
	s := DefaultSettings()
	s.Subtitles, s.Language = KeepLanguage, "eng"
	info := mediaInfo{Streams: []streamInfo{
		{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080},
		{Index: 1, Type: "audio", Codec: "aac", Language: "eng", Channels: 2},
		{Index: 2, Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "eng"},
		{Index: 3, Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "eng", Empty: true},
	}}
	p, err := newPlan("/library/Test Movie (2020)/Test.Movie.2020.mkv", s, info)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.subs) != 1 || p.subs[0].Index != 2 {
		t.Errorf("kept subtitle tracks %v, want only 2", p.subs)
	}
	if !slices.ContainsFunc(p.notes, func(n string) bool { return strings.Contains(n, "track 3: it's empty") }) {
		t.Errorf("notes %q don't say track 3 was dropped as empty", p.notes)
	}
}
