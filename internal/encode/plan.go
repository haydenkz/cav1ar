package encode

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// subtitle codecs that can be copied into Matroska as they are.
var copyableSubs = map[string]bool{
	"subrip": true, "ass": true, "ssa": true, "webvtt": true,
	"hdmv_pgs_subtitle": true, "dvd_subtitle": true, "dvb_subtitle": true,
}

// plan is what one encode will do: which tracks it keeps, and why.
type plan struct {
	in       string
	settings Settings
	video    streamInfo
	audio    []streamInfo
	subs     []streamInfo
	fonts    []streamInfo // attachments kept for styled subtitles
	notes    []string     // choices worth showing the user, such as tracks left out
}

func newPlan(in string, s Settings, info mediaInfo) (plan, error) {
	p := plan{in: in, settings: s}
	var haveVideo bool
	for _, st := range info.Streams {
		switch {
		case st.Type == "video" && !st.CoverArt && !haveVideo:
			p.video, haveVideo = st, true
		case st.Type == "audio":
			p.audio = append(p.audio, st)
		case st.Type == "subtitle":
			p.subs = append(p.subs, st)
		case st.Type == "attachment":
			p.fonts = append(p.fonts, st)
		}
	}
	if !haveVideo {
		return p, errors.New("no video stream found")
	}

	if s.AudioTracks == KeepLanguage && len(p.audio) > 0 {
		matched := inLanguage(p.audio, s.Language)
		switch {
		case len(matched) == 0:
			p.note("No %s audio found, so keeping all %d audio tracks", s.Language, len(p.audio))
		case len(matched) < len(p.audio):
			p.note("Keeping %d of %d audio tracks (%s)", len(matched), len(p.audio), s.Language)
			p.audio = matched
		}
	}

	switch s.Subtitles {
	case KeepNone:
		p.subs = nil
	case KeepLanguage:
		matched := inLanguage(p.subs, s.Language)
		if len(matched) < len(p.subs) {
			p.note("Keeping %d of %d subtitle tracks (%s)", len(matched), len(p.subs), s.Language)
		}
		p.subs = matched
	}
	p.subs = filter(p.subs, func(st streamInfo) bool {
		if copyableSubs[st.Codec] || st.Codec == "mov_text" {
			return true
		}
		p.note("Dropping subtitle track %d: %s can't be stored in MKV", st.Index, st.Codec)
		return false
	})
	// Styled (ASS/SSA) subtitles need the fonts attached to the file.
	if !slices.ContainsFunc(p.subs, func(st streamInfo) bool { return st.Codec == "ass" || st.Codec == "ssa" }) {
		p.fonts = nil
	}
	return p, nil
}

func (p *plan) note(format string, args ...any) {
	p.notes = append(p.notes, fmt.Sprintf(format, args...))
}

// release describes the encode for naming it.
func (p plan) release() release {
	r := parseRelease(p.in)
	r.Resolution = resolution(p.video.Width, p.video.Height)
	if len(p.audio) > 0 {
		r.Audio = audioTag(p.audio[0], p.settings)
	}
	r.Group = p.settings.ReleaseGroup
	return r
}

// output is where the encode will be written.
func (p plan) output() string {
	return p.settings.outputPath(p.in, p.release())
}

// args is the ffmpeg command line that carries out the plan.
func (p plan) args(out string) []string {
	s := p.settings
	beforeInput, video := videoArgs(s)
	args := append([]string{"-hide_banner", "-nostdin", "-nostats", "-n"}, beforeInput...)
	args = append(args, "-i", p.in)
	for _, group := range [][]streamInfo{{p.video}, p.audio, p.subs, p.fonts} {
		for _, st := range group {
			args = append(args, "-map", "0:"+strconv.Itoa(st.Index))
		}
	}

	args = append(args, video...)

	if s.AudioKbps == AudioCopy {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "libopus")
		for i, st := range p.audio {
			kbps := s.AudioKbps
			if kbps == AudioAuto {
				kbps = opusBitrate(opusChannels(st.Channels, s.Downmix))
			}
			args = append(args, fmt.Sprintf("-b:a:%d", i), fmt.Sprintf("%dk", kbps))
		}
		if s.Downmix {
			args = append(args, "-ac", "2")
		} else {
			// libopus rejects some common layouts, such as 5.1(side), so
			// remap them to the nearest one it accepts.
			args = append(args, "-af", "aformat=channel_layouts=7.1|5.1|stereo|mono")
		}
	}
	if len(p.audio) > 0 && s.AudioTracks == KeepLanguage {
		args = append(args, "-disposition:a:0", "default")
	}

	for i, st := range p.subs {
		codec := "copy"
		if st.Codec == "mov_text" { // MP4's text subtitles; Matroska needs SRT
			codec = "srt"
		}
		args = append(args, fmt.Sprintf("-c:s:%d", i), codec)
	}
	if len(p.fonts) > 0 {
		args = append(args, "-c:t", "copy")
	}

	if s.StripMetadata {
		args = append(args, "-map_metadata", "-1")
		// Put back what players need: track languages and attachment details.
		for _, g := range []struct {
			kind    string
			streams []streamInfo
		}{{"v", []streamInfo{p.video}}, {"a", p.audio}, {"s", p.subs}} {
			for i, st := range g.streams {
				if st.Language != "" {
					args = append(args, fmt.Sprintf("-metadata:s:%s:%d", g.kind, i), "language="+st.Language)
				}
			}
		}
		for i, st := range p.fonts {
			args = append(args,
				fmt.Sprintf("-metadata:s:t:%d", i), "filename="+st.Filename,
				fmt.Sprintf("-metadata:s:t:%d", i), "mimetype="+st.Mimetype)
		}
	}
	if s.Title != "" {
		args = append(args, "-metadata", "title="+p.release().expand(s.Title))
	}
	if s.ReleaseGroup != "" {
		args = append(args, "-metadata", "ENCODED_BY="+s.ReleaseGroup)
	}

	return append(args, "-progress", "pipe:1", out)
}

// inLanguage returns the streams tagged with lang.
func inLanguage(streams []streamInfo, lang string) []streamInfo {
	return filter(streams, func(st streamInfo) bool { return sameLanguage(st.Language, lang) })
}

// languages maps ISO 639-1 and 639-2/B codes to 639-2/T, so "en", "eng"
// and "en-US" all match each other.
var languages = map[string]string{
	"ar": "ara", "cs": "ces", "cze": "ces", "da": "dan", "de": "deu", "ger": "deu",
	"el": "ell", "gre": "ell", "en": "eng", "es": "spa", "fa": "fas", "per": "fas",
	"fi": "fin", "fr": "fra", "fre": "fra", "he": "heb", "hi": "hin", "hu": "hun",
	"id": "ind", "it": "ita", "ja": "jpn", "ko": "kor", "nl": "nld", "dut": "nld",
	"no": "nor", "nb": "nob", "pl": "pol", "pt": "por", "ro": "ron", "rum": "ron",
	"ru": "rus", "sv": "swe", "th": "tha", "tr": "tur", "uk": "ukr", "vi": "vie",
	"zh": "zho", "chi": "zho",
}

func sameLanguage(a, b string) bool {
	norm := func(code string) string {
		code, _, _ = strings.Cut(strings.ToLower(code), "-")
		if full, ok := languages[code]; ok {
			return full
		}
		return code
	}
	return a != "" && norm(a) == norm(b)
}

func filter[T any](s []T, keep func(T) bool) []T {
	var out []T
	for _, v := range s {
		if keep(v) {
			out = append(out, v)
		}
	}
	return out
}
