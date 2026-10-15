package encode

import (
	"slices"
	"testing"
)

func TestBestAudioNeverDropsTheWantedLanguage(t *testing.T) {
	track := func(index int, lang, title string, channels int, def bool) streamInfo {
		return streamInfo{Index: index, Type: "audio", Language: lang, Title: title, Channels: channels, Default: def}
	}
	tests := []struct {
		name  string
		audio []streamInfo
		want  []int // stream indexes kept
	}{
		{"tagged English over a default original",
			[]streamInfo{track(1, "ger", "", 8, true), track(2, "eng", "", 6, false)}, []int{2}},
		{"English tagged undetermined",
			[]streamInfo{track(1, "ger", "", 8, true), track(2, "und", "", 6, false)}, []int{2}},
		{"untagged English named in its title",
			[]streamInfo{track(1, "ger", "Deutsch DD+ 7.1", 8, true), track(2, "", "English DD 5.1", 6, false)}, []int{2}},
		{"untagged track named in another language isn't taken for English",
			[]streamInfo{track(1, "und", "Deutsch 7.1", 8, true), track(2, "", "", 2, false)}, []int{2}},
		{"no English: the best in each language, not a guess",
			[]streamInfo{track(1, "ger", "", 8, true), track(2, "ger", "", 2, false), track(3, "fre", "", 6, false)}, []int{1, 3}},
		{"English commentary doesn't count",
			[]streamInfo{track(1, "ger", "", 8, true), track(2, "eng", "Director's Commentary", 2, false)}, []int{1}},
		{"one track, kept",
			[]streamInfo{track(1, "jpn", "", 2, true)}, []int{1}},
	}
	for _, tt := range tests {
		var got []int
		for _, st := range bestAudio(tt.audio, "eng") {
			got = append(got, st.Index)
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("%s: kept %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestKeepBestTagsAnUntaggedPickInTheLanguageWanted(t *testing.T) {
	s := DefaultSettings()
	s.AudioTracks, s.Language, s.AudioKbps = KeepBest, "eng", AudioAuto
	info := mediaInfo{Streams: []streamInfo{
		{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080},
		{Index: 1, Type: "audio", Codec: "eac3", Language: "ger", Channels: 8, Default: true},
		{Index: 2, Type: "audio", Codec: "ac3", Language: "und", Channels: 6},
	}}
	p, err := newPlan("/library/Test Movie (2020)/Test.Movie.2020.mkv", s, info)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.audio) != 1 || p.audio[0].Index != 2 || p.audio[0].language != "eng" {
		t.Fatalf("kept %+v, want stream 2 tagged eng", p.audio)
	}
}
