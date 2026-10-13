package encode

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Action is what a file needs to match the settings.
type Action string

const (
	ActionNone   Action = "none"   // nothing: it already matches
	ActionRename Action = "rename" // only its name is wrong
	ActionRemux  Action = "remux"  // tracks to drop, audio to encode or tags to fix; the video is copied
	ActionEncode Action = "encode" // the video is encoded
)

// subtitle codecs that can be copied into Matroska as they are.
var copyableSubs = map[string]bool{
	"subrip": true, "ass": true, "ssa": true, "webvtt": true,
	"hdmv_pgs_subtitle": true, "dvd_subtitle": true, "dvb_subtitle": true,
}

var (
	commentaryTitle = regexp.MustCompile(`(?i)comment|descri|narrat`)
	forcedTitle     = regexp.MustCompile(`(?i)forced`)
	sdhTitle        = regexp.MustCompile(`(?i)\bSDH\b|\bCC\b|hearing`)
)

// audioOut is an audio track as it will be written.
type audioOut struct {
	streamInfo
	copy     bool
	codec    AudioCodec // when encoded
	channels int        // when encoded
	kbps     int        // when encoded
	language string     // the language tag to write
}

// subOut is a subtitle track as it will be written.
type subOut struct {
	streamInfo
	codec       string // "copy", or "srt" to convert
	forced, sdh bool
	stats       *subStats // what reading the track found, once tidying has
}

// role is what a subtitle track is for: the full dialogue, the dialogue
// with sound descriptions, or only the lines not in the audio's language.
func (s subOut) role() string {
	switch {
	case s.forced:
		return "forced"
	case s.sdh:
		return "SDH"
	}
	return "full"
}

// label is the track title written when the source's titles are stripped.
func (s subOut) label() string {
	switch {
	case s.forced:
		return "Forced"
	case s.sdh:
		return "SDH"
	}
	return ""
}

// plan is what one job will do: which tracks it keeps and how, what the
// result is called, and why.
type plan struct {
	in       string
	settings Settings
	folder   WatchFolder
	info     mediaInfo

	video         streamInfo
	encodeVideo   bool
	width, height int // output frame size
	audio         []audioOut
	subs          []subOut
	fonts         []streamInfo // attachments kept for styled subtitles

	out       string   // where the result goes
	notes     []string // choices worth showing the user, such as tracks left out
	checkSubs bool     // subtitles need tidying before the plan is final
}

func newPlan(in string, s Settings, info mediaInfo) (*plan, error) {
	p := &plan{in: in, settings: s, folder: s.folderOf(in), info: info}
	var haveVideo bool
	var audio, subs []streamInfo
	for _, st := range info.Streams {
		switch {
		case st.Type == "video" && !st.CoverArt && !haveVideo:
			p.video, haveVideo = st, true
		case st.Type == "audio":
			audio = append(audio, st)
		case st.Type == "subtitle":
			subs = append(subs, st)
		case st.Type == "attachment":
			p.fonts = append(p.fonts, st)
		}
	}
	if !haveVideo {
		return p, errors.New("no video stream found")
	}

	p.width, p.height = fitSize(p.video.Width, p.video.Height, s.MaxHeight)
	p.encodeVideo = p.video.Codec != "av1" || p.width != p.video.Width || p.height != p.video.Height
	if p.encodeVideo && (p.video.Width <= 0 || p.video.Height <= 0) {
		return p, errors.New("the video stream has no size")
	}

	for _, st := range p.pickAudio(audio) {
		a := s.audioOut(st)
		if s.AudioTracks == KeepBest && a.language == "" {
			a.language = s.Language // the best track, untagged, is assumed to be in the language wanted
		}
		p.audio = append(p.audio, a)
	}
	for _, st := range p.pickSubs(subs) {
		sub := subOut{
			streamInfo: st,
			codec:      "copy",
			forced:     st.Forced || forcedTitle.MatchString(st.Title),
			sdh:        st.HearingImpaired || sdhTitle.MatchString(st.Title),
		}
		if st.Codec == "mov_text" { // MP4's text subtitles; Matroska needs SRT
			sub.codec = "srt"
		}
		p.subs = append(p.subs, sub)
	}
	// Styled (ASS/SSA) subtitles need the fonts attached to the file.
	if !hasStyledSubs(p.subs) {
		p.fonts = nil
	}
	p.checkSubs = s.TidySubs && sharedRoles(p.subs)

	r, err := p.release()
	if err != nil {
		return p, err
	}
	p.out = s.outputPath(in, r)
	return p, nil
}

// pickAudio returns the audio tracks to keep.
func (p *plan) pickAudio(audio []streamInfo) []streamInfo {
	s := p.settings
	switch {
	case len(audio) == 0:
		return nil
	case s.AudioTracks == KeepLanguage:
		matched := inLanguage(audio, s.Language)
		switch {
		case len(matched) == 0:
			p.note("No %s audio found, so keeping all %d audio tracks", s.Language, len(audio))
		case len(matched) < len(audio):
			p.note("Keeping %d of %d audio tracks (%s)", len(matched), len(audio), s.Language)
			return matched
		}
	case s.AudioTracks == KeepBest:
		best := bestAudio(audio, s.Language)
		if len(audio) > 1 {
			p.note("Keeping 1 of %d audio tracks: stream %d, %s", len(audio), best.Index, describeAudio(best))
		}
		return []streamInfo{best}
	}
	return audio
}

// bestAudio picks the track to keep when keeping one: one in lang that isn't
// commentary, or failing that an untagged one, or failing that any that isn't
// commentary, such as the original language of a film with no dub. Among
// those it prefers the default track, then the most channels, which make the
// best source for a downmix.
func bestAudio(audio []streamInfo, lang string) streamInfo {
	usable := func(st streamInfo) bool { return !st.Commentary && !commentaryTitle.MatchString(st.Title) }
	pools := [][]streamInfo{
		filter(audio, func(st streamInfo) bool { return sameLanguage(st.Language, lang) && usable(st) }),
		filter(audio, func(st streamInfo) bool { return st.Language == "" && usable(st) }),
		filter(audio, usable),
		audio,
	}
	for _, pool := range pools {
		if len(pool) > 0 {
			return slices.MaxFunc(pool, func(a, b streamInfo) int {
				return slices.Compare(
					[]int{b2i(a.Default), a.Channels, a.BitRate},
					[]int{b2i(b.Default), b.Channels, b.BitRate})
			})
		}
	}
	return streamInfo{}
}

func describeAudio(st streamInfo) string {
	parts := []string{codecTag(st.Codec) + channelTag(st.Channels)}
	if st.Language != "" {
		parts = append(parts, st.Language)
	}
	if st.Title != "" {
		parts = append(parts, strconv.Quote(st.Title))
	}
	return strings.Join(parts, " ")
}

// audioOut decides how a track is written: copied when the settings say so,
// or when it already has the codec and channels they would give it.
func (s Settings) audioOut(st streamInfo) audioOut {
	a := audioOut{streamInfo: st, language: st.Language, codec: s.AudioCodec}
	if s.AudioKbps == AudioCopy {
		a.copy = true
		return a
	}
	a.channels = outChannels(st.Channels, s.Downmix)
	if st.Codec == string(s.AudioCodec) && st.Channels == a.channels {
		a.copy = true
		return a
	}
	a.kbps = s.AudioKbps
	if a.kbps == AudioAuto {
		a.kbps = autoBitrate(a.codec, a.channels)
	}
	return a
}

// pickSubs returns the subtitle tracks to keep.
func (p *plan) pickSubs(subs []streamInfo) []streamInfo {
	switch p.settings.Subtitles {
	case KeepNone:
		return nil
	case KeepLanguage:
		matched := inLanguage(subs, p.settings.Language)
		if len(matched) < len(subs) {
			p.note("Keeping %d of %d subtitle tracks (%s)", len(matched), len(subs), p.settings.Language)
		}
		subs = matched
	}
	return filter(subs, func(st streamInfo) bool {
		if copyableSubs[st.Codec] || st.Codec == "mov_text" {
			return true
		}
		p.note("Dropping subtitle track %d: %s can't be stored in MKV", st.Index, st.Codec)
		return false
	})
}

// sharedRoles reports whether two subtitle tracks in the same language claim
// the same role, going by their flags, so tidying needs to read them to tell
// which to keep.
func sharedRoles(subs []subOut) bool {
	seen := map[string]bool{}
	for _, s := range subs {
		key := normLanguage(s.Language) + "/" + s.role()
		if seen[key] {
			return true
		}
		seen[key] = true
	}
	return false
}

func (p *plan) note(format string, args ...any) {
	p.notes = append(p.notes, fmt.Sprintf(format, args...))
}

// release describes the result for naming it.
func (p *plan) release() (release, error) {
	r, err := nameSource(parseRelease(p.in), p.in, p.folder)
	if err != nil {
		return r, err
	}
	r.Resolution = resolution(p.width, p.height)
	if len(p.audio) > 0 {
		r.Audio = audioTag(p.audio[0])
	}
	r.Group = p.settings.ReleaseGroup
	return r, nil
}

// title is the title tag the result will have.
func (p *plan) title() string {
	switch {
	case p.settings.Title != "":
		r, _ := p.release()
		return r.expand(p.settings.Title)
	case p.settings.StripMetadata:
		return ""
	}
	return p.info.Tags["TITLE"]
}

// encodedBy is the ENCODED_BY tag the result will have.
func (p *plan) encodedBy() string {
	if p.settings.ReleaseGroup != "" || p.settings.StripMetadata {
		return p.settings.ReleaseGroup
	}
	return p.info.Tags["ENCODED_BY"]
}

// reasons says what about the file doesn't match the settings, apart from
// its name. An empty list means its contents can stay as they are.
func (p *plan) reasons() []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	s := p.settings

	if p.video.Codec != "av1" {
		add("video %s", p.video.Codec)
	}
	if p.width != p.video.Width || p.height != p.video.Height {
		add("%dx%d > %dp", p.video.Width, p.video.Height, s.MaxHeight)
	}
	for _, a := range p.audio {
		if !a.copy {
			add("audio %s %dch", a.Codec, a.Channels)
		}
	}
	if slices.ContainsFunc(p.subs, func(sub subOut) bool { return sub.codec != "copy" }) {
		add("subtitles to srt")
	}
	if p.checkSubs {
		add("subtitles to check")
	}
	if slices.ContainsFunc(p.subs, func(sub subOut) bool { return sub.forced != sub.Forced || sub.sdh != sub.HearingImpaired }) {
		add("subtitle flags")
	}

	// Streams left out.
	kept := map[int]bool{p.video.Index: true}
	for _, a := range p.audio {
		kept[a.Index] = true
	}
	for _, sub := range p.subs {
		kept[sub.Index] = true
	}
	for _, f := range p.fonts {
		kept[f.Index] = true
	}
	dropped := map[string]int{}
	for _, st := range p.info.Streams {
		if !kept[st.Index] {
			kind := st.Type
			if st.CoverArt {
				kind = "cover art"
			}
			dropped[kind]++
		}
	}
	if len(dropped) > 0 {
		var parts []string
		for _, kind := range slices.Sorted(maps.Keys(dropped)) {
			parts = append(parts, fmt.Sprintf("%d %s", dropped[kind], kind))
		}
		add("drop %s", strings.Join(parts, ", "))
	}

	if !strings.EqualFold(filepath.Ext(p.in), ".mkv") {
		add("to mkv")
	}
	if p.tagsDiffer() {
		add("tags")
	}
	return out
}

// tagsDiffer reports whether the file's titles, languages or other tags
// differ from what the settings would write.
func (p *plan) tagsDiffer() bool {
	s := p.settings
	tags := p.info.Tags
	if tags["TITLE"] != p.title() || tags["ENCODED_BY"] != p.encodedBy() {
		return true
	}
	for _, a := range p.audio {
		if a.language != a.Language {
			return true
		}
	}
	if !s.StripMetadata {
		return false
	}
	for k := range tags {
		switch k {
		case "TITLE", "ENCODED_BY", "ENCODER", "CREATION_TIME", "DURATION",
			"MAJOR_BRAND", "MINOR_VERSION", "COMPATIBLE_BRANDS":
		default:
			return true // something stripping would remove
		}
	}
	if p.video.Title != "" || slices.ContainsFunc(p.audio, func(a audioOut) bool { return a.Title != "" }) {
		return true
	}
	return slices.ContainsFunc(p.subs, func(sub subOut) bool { return sub.Title != sub.label() })
}

// action is what the job has to do.
func (p *plan) action() Action {
	switch {
	case p.encodeVideo:
		return ActionEncode
	case len(p.reasons()) > 0:
		return ActionRemux
	case p.out == p.in:
		return ActionNone
	case filepath.Dir(p.out) != filepath.Dir(p.in):
		return ActionRemux // a copy in the output folder
	}
	return ActionRename
}

// args is the ffmpeg command line that carries out the plan, writing to out.
// hwDecode decodes the video on the GPU, when the encoder can use that.
func (p *plan) args(out string, hwDecode bool) []string {
	s := p.settings
	args := []string{"-hide_banner", "-nostdin", "-nostats", "-y",
		"-analyzeduration", "100M", "-probesize", "100M"}
	var video []string
	if p.encodeVideo {
		var beforeInput []string
		beforeInput, video = videoArgs(s, p.video, p.width, p.height, hwDecode)
		args = append(args, beforeInput...)
	} else {
		video = []string{"-c:v", "copy"}
	}
	args = append(args, "-i", p.in, "-map", "0:"+strconv.Itoa(p.video.Index))
	for _, a := range p.audio {
		args = append(args, "-map", "0:"+strconv.Itoa(a.Index))
	}
	for _, sub := range p.subs {
		args = append(args, "-map", "0:"+strconv.Itoa(sub.Index))
	}
	for _, f := range p.fonts {
		args = append(args, "-map", "0:"+strconv.Itoa(f.Index))
	}
	args = append(args, video...)

	for i, a := range p.audio {
		if a.copy {
			args = append(args, fmt.Sprintf("-c:a:%d", i), "copy")
			continue
		}
		codec := "libopus"
		if a.codec == AAC {
			codec = "aac"
		}
		// The encoders reject some common layouts, such as 5.1(side), so
		// remap them to the nearest one they accept.
		layouts := "7.1|5.1|stereo|mono"
		if s.Downmix && a.Channels > 2 {
			layouts = "stereo"
		}
		args = append(args,
			fmt.Sprintf("-c:a:%d", i), codec,
			fmt.Sprintf("-b:a:%d", i), fmt.Sprintf("%dk", a.kbps),
			fmt.Sprintf("-filter:a:%d", i), "aformat=channel_layouts="+layouts)
	}
	if len(p.audio) > 0 && (s.AudioTracks == KeepLanguage || s.AudioTracks == KeepBest) {
		args = append(args, "-disposition:a:0", "default")
	}

	for i, sub := range p.subs {
		var flags []string
		if sub.Default {
			flags = append(flags, "default")
		}
		if sub.forced {
			flags = append(flags, "forced")
		}
		if sub.sdh {
			flags = append(flags, "hearing_impaired")
		}
		disposition := strings.Join(flags, "+")
		if disposition == "" {
			disposition = "0"
		}
		args = append(args, fmt.Sprintf("-c:s:%d", i), sub.codec, fmt.Sprintf("-disposition:s:%d", i), disposition)
		if label := sub.label(); label != "" && (s.StripMetadata || sub.Title == "") {
			args = append(args, fmt.Sprintf("-metadata:s:s:%d", i), "title="+label)
		}
	}
	if len(p.fonts) > 0 {
		args = append(args, "-c:t", "copy")
	}

	if s.StripMetadata {
		args = append(args, "-map_metadata", "-1")
		// Put back what players need: track languages and attachment details.
		if p.video.Language != "" {
			args = append(args, "-metadata:s:v:0", "language="+p.video.Language)
		}
		for i, sub := range p.subs {
			if sub.Language != "" {
				args = append(args, fmt.Sprintf("-metadata:s:s:%d", i), "language="+sub.Language)
			}
		}
		for i, st := range p.fonts {
			args = append(args,
				fmt.Sprintf("-metadata:s:t:%d", i), "filename="+st.Filename,
				fmt.Sprintf("-metadata:s:t:%d", i), "mimetype="+st.Mimetype)
		}
	}
	for i, a := range p.audio {
		if a.language != "" && (s.StripMetadata || a.language != a.Language) {
			args = append(args, fmt.Sprintf("-metadata:s:a:%d", i), "language="+a.language)
		}
	}
	if s.Title != "" {
		args = append(args, "-metadata", "title="+p.title())
	}
	if s.ReleaseGroup != "" {
		args = append(args, "-metadata", "ENCODED_BY="+s.ReleaseGroup)
	}

	return append(args, "-max_muxing_queue_size", "4096", "-f", "matroska", "-progress", "pipe:1", out)
}

// inLanguage returns the streams tagged with lang.
func inLanguage(streams []streamInfo, lang string) []streamInfo {
	return filter(streams, func(st streamInfo) bool { return sameLanguage(st.Language, lang) })
}

// languages maps ISO 639-1 and 639-2/B codes to 639-2/T, so "en", "eng"
// and "en-US" all match each other.
var languages = map[string]string{
	"ar": "ara", "bg": "bul", "cs": "ces", "cze": "ces", "da": "dan", "de": "deu", "ger": "deu",
	"el": "ell", "gre": "ell", "en": "eng", "es": "spa", "fa": "fas", "per": "fas",
	"fi": "fin", "fr": "fra", "fre": "fra", "he": "heb", "hi": "hin", "hu": "hun",
	"id": "ind", "it": "ita", "ja": "jpn", "ko": "kor", "nl": "nld", "dut": "nld",
	"no": "nor", "nb": "nob", "pl": "pol", "pt": "por", "ro": "ron", "rum": "ron",
	"ru": "rus", "sr": "srp", "sv": "swe", "th": "tha", "tr": "tur", "uk": "ukr", "vi": "vie",
	"zh": "zho", "chi": "zho",
}

// normLanguage writes a language code the way languages does, or returns ""
// for none.
func normLanguage(code string) string {
	code, _, _ = strings.Cut(strings.ToLower(code), "-")
	if full, ok := languages[code]; ok {
		return full
	}
	return code
}

func sameLanguage(a, b string) bool {
	return a != "" && normLanguage(a) == normLanguage(b)
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

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
