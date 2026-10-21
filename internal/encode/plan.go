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
	"time"
	"unicode"
)

// Action is what a file needs to match the settings.
type Action string

const (
	ActionNone   Action = "none"   // nothing: it already matches, or only its name doesn't and the settings keep originals
	ActionRename Action = "rename" // only its name is wrong, and the settings let the original change
	ActionRetag  Action = "retag"  // only its tags, track titles, flags or chapters are wrong; fixed in place
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
	device   Device // what encodes the video, once the job has one
	folder   WatchFolder
	info     mediaInfo

	video         streamInfo
	encodeVideo   bool
	tooBig        bool // bigger than Settings.MaxHeight allows
	width, height int  // output frame size
	audio         []audioOut
	subs          []subOut
	fonts         []streamInfo // attachments kept for styled subtitles

	out       string   // where the result goes
	notes     []string // choices worth showing the user, such as tracks left out
	checkSubs bool     // subtitles need tidying before the plan is final
	nameKept  bool     // the settings would rename the file, but keep originals as they are
	links     uint64   // how many hard links the source has, or 0 if unknown

	prerollHave string        // ID of the preroll the source starts with, if any
	prerollIn   time.Duration // how long that preroll runs, which the job cuts off
	prerollWant string        // ID of the preroll the settings want, if any
}

func newPlan(in string, s Settings, info mediaInfo) (*plan, error) {
	p := &plan{in: in, settings: s, folder: s.folderOf(in), info: info}
	_, p.links, _ = identify(in)
	if p.prerollHave, p.prerollIn = prerollOf(info); p.prerollIn > 0 {
		// Judge the source by what follows its preroll.
		p.info.Streams = slices.Clone(info.Streams)
		for i := range p.info.Streams {
			p.info.Streams[i].Duration = max(0, p.info.Streams[i].Duration-p.prerollIn)
		}
		p.info.Duration = max(0, p.info.Duration-p.prerollIn)
	}
	if s.Preroll != "" {
		id, err := prerollID(s.Preroll)
		if err != nil {
			return p, err
		}
		p.prerollWant = id
	}
	var haveVideo bool
	var audio, subs []streamInfo
	for _, st := range p.info.Streams {
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
	p.tooBig = p.width != p.video.Width || p.height != p.video.Height
	p.encodeVideo = p.video.Codec != "av1" || p.tooBig
	if p.encodeVideo && (p.video.Width <= 0 || p.video.Height <= 0) {
		return p, errors.New("the video stream has no size")
	}
	if p.encodeVideo { // some cropped sources have odd sizes
		p.width, p.height = even(float64(p.width)), even(float64(p.height))
	}

	picked := p.pickAudio(audio)
	for _, st := range picked {
		a := s.audioOut(st)
		if s.AudioTracks == KeepBest && len(picked) == 1 && untagged(st) {
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
		matched := filter(audio, func(st streamInfo) bool { return speaks(st, s.Language) })
		switch {
		case len(matched) == 0:
			p.note("No %s audio found, so keeping all %d audio tracks", s.Language, len(audio))
		case len(matched) < len(audio):
			p.note("Keeping %d of %d audio tracks (%s)", len(matched), len(audio), s.Language)
			return matched
		}
	case s.AudioTracks == KeepBest:
		kept := bestAudio(audio, s.Language)
		switch {
		case len(kept) > 1:
			p.note("No %s audio found, so keeping the best track in each language: %d of %d", s.Language, len(kept), len(audio))
		case len(audio) > 1:
			p.note("Keeping 1 of %d audio tracks: stream %d, %s", len(audio), kept[0].Index, describeAudio(kept[0]))
		}
		return kept
	}
	return audio
}

// bestAudio picks the tracks to keep when keeping the best: the one in lang
// that isn't commentary, or failing that an untagged one whose title doesn't
// name another language. When there's neither, it keeps the best track in
// each language rather than guess, since a dub in lang is sometimes tagged
// wrong; a film with no dub then keeps its original language. Among tracks
// it prefers the default, then the most channels, which make the best source
// for a downmix.
func bestAudio(audio []streamInfo, lang string) []streamInfo {
	usable := func(st streamInfo) bool { return !st.Commentary && !commentaryTitle.MatchString(st.Title) }
	best := func(pool []streamInfo) streamInfo {
		return slices.MaxFunc(pool, func(a, b streamInfo) int {
			return slices.Compare(
				[]int{b2i(a.Default), a.Channels, a.BitRate},
				[]int{b2i(b.Default), b.Channels, b.BitRate})
		})
	}
	if pool := filter(audio, func(st streamInfo) bool { return speaks(st, lang) && usable(st) }); len(pool) > 0 {
		return []streamInfo{best(pool)}
	}
	if pool := filter(audio, func(st streamInfo) bool {
		return untagged(st) && titleLanguage(st.Title) == "" && usable(st)
	}); len(pool) > 0 {
		return []streamInfo{best(pool)}
	}
	pool := filter(audio, usable)
	if len(pool) == 0 {
		return []streamInfo{best(audio)} // only commentary
	}
	byLanguage := map[string][]streamInfo{}
	var order []string
	for _, st := range pool {
		l := normLanguage(st.Language)
		if untagged(st) {
			l = titleLanguage(st.Title)
		}
		if _, ok := byLanguage[l]; !ok {
			order = append(order, l)
		}
		byLanguage[l] = append(byLanguage[l], st)
	}
	kept := make([]streamInfo, 0, len(order))
	for _, l := range order {
		kept = append(kept, best(byLanguage[l]))
	}
	slices.SortFunc(kept, func(a, b streamInfo) int { return a.Index - b.Index })
	return kept
}

// untagged reports whether a track doesn't say what language it's in: no tag,
// or one of the codes for undetermined, unknown or no language.
func untagged(st streamInfo) bool {
	switch normLanguage(st.Language) {
	case "", "und", "unk", "mis", "zxx":
		return true
	}
	return false
}

// speaks reports whether an audio track is in lang: tagged so, or untagged
// with a title that names lang, as in "English DD 5.1".
func speaks(st streamInfo, lang string) bool {
	if sameLanguage(st.Language, lang) {
		return true
	}
	return untagged(st) && titleLanguage(st.Title) == normLanguage(lang)
}

// languageNames are what a track title might call a language, keyed by its
// ISO 639-2/T code.
var languageNames = map[string][]string{
	"eng": {"english", "englisch", "anglais", "inglés", "ingles", "inglese", "engels"},
	"deu": {"german", "deutsch", "allemand", "alemán", "aleman", "tedesco", "duits"},
	"fra": {"french", "français", "francais", "französisch", "francés", "frances", "francese", "frans"},
	"spa": {"spanish", "español", "espanol", "castellano", "spanisch", "espagnol", "spagnolo"},
	"ita": {"italian", "italiano", "italienisch", "italien"},
	"jpn": {"japanese", "japanisch", "japonais", "japonés", "giapponese"},
	"kor": {"korean", "koreanisch", "coréen", "coreano"},
	"zho": {"chinese", "mandarin", "cantonese", "chinesisch", "chinois"},
	"rus": {"russian", "russisch", "russe", "ruso"},
	"por": {"portuguese", "português", "portugues", "portugiesisch"},
	"nld": {"dutch", "nederlands", "niederländisch"},
	"hin": {"hindi"},
}

// titleLanguage is the language a track's title names, by name or by a
// three-letter code standing as a word, or "" if it names none.
func titleLanguage(title string) string {
	for _, w := range strings.FieldsFunc(strings.ToLower(title), func(r rune) bool { return !unicode.IsLetter(r) }) {
		for code, names := range languageNames {
			if slices.Contains(names, w) || (len(w) == 3 && normLanguage(w) == code) {
				return code
			}
		}
	}
	return ""
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
		// Besides having nothing to show, a stream that never sends a packet
		// stalls ffmpeg: it holds back the other streams waiting for one, and
		// ends the encode at the end of the file with seconds of video done.
		if st.Empty {
			p.note("Dropping subtitle track %d: it's empty", st.Index)
			return false
		}
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

// reasons says what a job would change to bring the file into line with the
// settings, apart from its name, a few words for each. An empty list means its
// contents can stay as they are.
func (p *plan) reasons() []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	s := p.settings

	if p.video.Codec != "av1" {
		add("video %s → AV1", videoName(p.video.Codec))
	}
	if p.tooBig {
		add("scale %d×%d down to %dp", p.video.Width, p.video.Height, s.MaxHeight)
	}
	for _, a := range p.audio {
		if !a.copy {
			add("audio %s → %s", audioName(a.Codec, a.Channels), audioName(string(a.codec), a.channels))
		}
	}
	if slices.ContainsFunc(p.subs, func(sub subOut) bool { return sub.codec != "copy" }) {
		add("subtitles → SRT")
	}
	if p.checkSubs {
		add("read the subtitles to tell tracks apart")
	}
	if slices.ContainsFunc(p.subs, func(sub subOut) bool { return sub.forced != sub.Forced || sub.sdh != sub.HearingImpaired }) {
		add(reasonSubFlags)
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
			switch n := dropped[kind]; kind {
			case "cover art":
				parts = append(parts, "the cover art")
			case "attachment":
				parts = append(parts, fmt.Sprintf("%d %s", n, pick(n, "attachment", "attachments")))
			default:
				parts = append(parts, fmt.Sprintf("%d %s %s", n, kind, pick(n, "track", "tracks")))
			}
		}
		add("drop %s", joinAnd(parts))
	}

	if ext := filepath.Ext(p.in); !strings.EqualFold(ext, ".mkv") {
		add("%s → MKV", strings.ToUpper(strings.TrimPrefix(ext, ".")))
	}
	switch {
	case p.prerollWant == p.prerollHave:
	case p.prerollHave == "":
		add("add the preroll")
	case p.prerollWant == "":
		add("remove the preroll")
	default:
		add("replace the preroll")
	}
	if p.tagsDiffer() {
		add(reasonTags)
	}
	if s.StripMetadata && p.info.Chapters > 0 {
		add(reasonChapters)
	}
	return out
}

// videoName is how people write a video codec, e.g. H.264 for h264.
func videoName(codec string) string {
	switch codec {
	case "h264":
		return "H.264"
	case "hevc":
		return "HEVC"
	case "mpeg4":
		return "MPEG-4"
	case "mpeg2video":
		return "MPEG-2"
	case "vc1":
		return "VC-1"
	}
	return strings.ToUpper(codec)
}

// audioName is an audio format as release names write it, e.g. DDP 5.1.
func audioName(codec string, channels int) string {
	return strings.TrimSpace(codecTag(codec) + " " + channelTag(channels))
}

// joinAnd lists parts as a sentence would: "a", "a and b", "a, b and c".
func joinAnd(parts []string) string {
	if len(parts) < 2 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

func pick(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// Reasons that only concern a file's metadata, which a retag fixes without
// rewriting the file.
const (
	reasonSubFlags = "set subtitle flags"
	reasonTags     = "update tags"
	reasonChapters = "strip chapters"
)

// retaggable reports whether the file can be brought into line by editing
// its metadata in place: fixableInPlace, and nothing else links to it. A
// file with other hard links, as an import linked from a download that is
// still seeding has, would have them all edited too, so it's remuxed into
// a new file instead.
func (p *plan) retaggable() bool { return p.links <= 1 && p.fixableInPlace() }

// fixableInPlace reports whether editing the file's metadata in place would
// bring it into line: it's Matroska, everything wrong with it is metadata,
// MKVToolNix is installed, and the settings let the original be changed
// rather than kept beside a copy.
func (p *plan) fixableInPlace() bool {
	if !p.settings.DeleteOriginal || filepath.Dir(p.out) != filepath.Dir(p.in) ||
		!strings.EqualFold(filepath.Ext(p.in), ".mkv") || !haveMKVToolNix() {
		return false
	}
	reasons := p.reasons()
	return len(reasons) > 0 && !slices.ContainsFunc(reasons, func(r string) bool {
		return r != reasonSubFlags && r != reasonTags && r != reasonChapters
	})
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
			"MAJOR_BRAND", "MINOR_VERSION", "COMPATIBLE_BRANDS", prerollTag:
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
	case p.retaggable():
		return ActionRetag
	case len(p.reasons()) > 0:
		return ActionRemux
	case p.out == p.in:
		return ActionNone
	case filepath.Dir(p.out) != filepath.Dir(p.in):
		return ActionRemux // a copy in the output folder
	case !p.settings.DeleteOriginal:
		return ActionNone // renaming would change the original, which the settings keep as it is
	}
	return ActionRename
}

// A job that writes a new file, whether encoding the video or copying it,
// does so in at least two ffmpeg passes. ffmpeg has exited cleanly on
// encodes whose audio stopped an hour early: with the video, audio and
// subtitles in one pass, it can end an audio track partway through without
// saying so. So the encoding pass reads the source once and writes the
// video and audio to one file and the subtitles and attachments to another,
// and the remuxing pass puts them together with the flags and tags the
// settings want, or joins them onto the preroll. If the encoding pass still
// cuts the audio short, a pass of its own encodes the audio again first, and
// if the subtitles run on past the end of the video and audio, another cuts
// them off there.

// inputArgs opens the source for a pass that reads it, cutting off any
// preroll it starts with. beforeInput are the video decoder's options.
func (p *plan) inputArgs(beforeInput []string) []string {
	args := []string{"-hide_banner", "-nostdin", "-nostats", "-y", "-progress", "pipe:1",
		"-analyzeduration", "100M", "-probesize", "100M"}
	args = append(args, beforeInput...)
	if p.prerollIn > 0 { // cut off the preroll, which is added back afterwards if wanted
		args = append(args, "-ss", seconds6(p.prerollIn))
	}
	return append(args, "-i", p.in)
}

// audioCodecArgs encodes or copies each audio track the plan keeps.
func (p *plan) audioCodecArgs() []string {
	var args []string
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
		if p.settings.Downmix && a.Channels > 2 {
			layouts = "stereo"
		}
		args = append(args,
			fmt.Sprintf("-c:a:%d", i), codec,
			fmt.Sprintf("-b:a:%d", i), fmt.Sprintf("%dk", a.kbps),
			fmt.Sprintf("-filter:a:%d", i), "aformat=channel_layouts="+layouts)
	}
	return args
}

// encodingPass says what the encoding pass does with the video and audio,
// for the job's log.
func (p *plan) encodingPass() string {
	audio := false
	for _, a := range p.audio {
		audio = audio || !a.copy
	}
	switch {
	case p.encodeVideo && audio:
		return "encoding the video and audio"
	case p.encodeVideo:
		return "encoding the video, copying the audio"
	case audio:
		return "encoding the audio, copying the video"
	}
	return "copying the tracks"
}

// hasSide reports whether the plan keeps subtitles or attachments, which the
// encoding pass writes to a file of their own.
func (p *plan) hasSide() bool { return len(p.subs) > 0 || len(p.fonts) > 0 }

// encodeArgs is the ffmpeg command line for the encoding pass, which writes
// the video and audio to av, and the subtitles and attachments to side when
// the plan keeps any. hwDecode decodes the video on the GPU, when the encoder
// can use that.
func (p *plan) encodeArgs(av, side string, hwDecode bool) []string {
	var beforeInput, video []string
	if p.encodeVideo {
		beforeInput, video = videoArgs(p.settings, p.device, p.video, p.width, p.height, hwDecode)
	} else {
		video = []string{"-c:v", "copy"}
	}
	args := append(p.inputArgs(beforeInput), "-map", "0:"+strconv.Itoa(p.video.Index))
	for _, a := range p.audio {
		args = append(args, "-map", "0:"+strconv.Itoa(a.Index))
	}
	args = append(args, video...)
	args = append(args, p.audioCodecArgs()...)
	args = append(args, "-max_muxing_queue_size", "4096", "-f", "matroska", av)
	if !p.hasSide() {
		return args
	}
	for _, sub := range p.subs {
		args = append(args, "-map", "0:"+strconv.Itoa(sub.Index))
	}
	for _, f := range p.fonts {
		args = append(args, "-map", "0:"+strconv.Itoa(f.Index))
	}
	for i, sub := range p.subs {
		args = append(args, fmt.Sprintf("-c:s:%d", i), sub.codec)
	}
	if len(p.fonts) > 0 {
		args = append(args, "-c:t", "copy")
	}
	return append(args, "-map_chapters", "-1", "-max_muxing_queue_size", "4096", "-f", "matroska", side)
}

// audioArgs is the ffmpeg command line for a pass that encodes only the
// audio, to out.
func (p *plan) audioArgs(out string) []string {
	args := p.inputArgs(nil)
	for _, a := range p.audio {
		args = append(args, "-map", "0:"+strconv.Itoa(a.Index))
	}
	args = append(args, p.audioCodecArgs()...)
	return append(args, "-map_chapters", "-1", "-max_muxing_queue_size", "4096", "-f", "matroska", out)
}

// trimSideArgs is the ffmpeg command line for a pass that copies side, the
// encoding pass's subtitles and attachments, into out with the subtitles cut
// off at length, where the video and audio end. A source's subtitles can run
// far past that, which would stretch the result to match; see longestPlayed.
// Cues that start after length are dropped and ones that run past it are
// shortened. -t only limits subtitles as an output option, and -copyts keeps
// the cues where they are: without it ffmpeg moves them back by the time of
// the first, in a file that has attachments.
func (p *plan) trimSideArgs(out, side string, length time.Duration) []string {
	return []string{"-hide_banner", "-nostdin", "-nostats", "-y", "-progress", "pipe:1", "-copyts", "-i", side,
		"-map", "0", "-c", "copy", "-bsf:s", clipCues(length), "-t", seconds6(length),
		"-map_chapters", "-1", "-max_muxing_queue_size", "4096", "-f", "matroska", out}
}

// clipCues is the bitstream filter that ends a subtitle cue at length if it
// runs past it, and leaves the cues that don't. Times are in the stream's
// time base, TB seconds to the tick, so length is divided by it.
func clipCues(length time.Duration) string {
	end := seconds6(length) + "/TB"
	return "setts=duration='if(gt(PTS+DURATION," + end + "),max(0," + end + "-PTS),DURATION)'"
}

// remuxArgs is the ffmpeg command line for the remuxing pass, which copies
// the tracks from the encoding pass's files into out and gives them the
// flags and tags the settings want. audio, when not empty, is the audio-only
// pass's file, whose audio replaces av's. The video and audio files carry
// the source's tags and chapters, so those come from av.
func (p *plan) remuxArgs(out, av, audio, side string) []string {
	s := p.settings
	args := []string{"-hide_banner", "-nostdin", "-nostats", "-y", "-progress", "pipe:1", "-i", av}
	audioFrom, sideFrom, inputs := "0:", "", 1
	if audio != "" {
		args = append(args, "-i", audio)
		audioFrom = strconv.Itoa(inputs) + ":"
		inputs++
	}
	if side != "" {
		args = append(args, "-i", side)
		sideFrom = strconv.Itoa(inputs) + ":"
	}
	args = append(args, "-map", "0:v:0")
	for i := range p.audio {
		args = append(args, "-map", audioFrom+"a:"+strconv.Itoa(i))
	}
	for i := range p.subs {
		args = append(args, "-map", sideFrom+"s:"+strconv.Itoa(i))
	}
	for i := range p.fonts {
		args = append(args, "-map", sideFrom+"t:"+strconv.Itoa(i))
	}
	args = append(args, "-c", "copy")
	if len(p.audio) > 0 && p.firstAudioDefault() {
		args = append(args, "-disposition:a:0", "default")
	}
	args = append(args, p.tagArgs()...)
	if p.prerollIn > 0 && !s.StripMetadata {
		args = append(args, "-metadata", prerollTag+"=")
	}
	return append(args, "-max_muxing_queue_size", "4096", "-f", "matroska", out)
}

// joinArgs is the ffmpeg command line for putting the result together with
// the preroll ahead of it, in one pass: list is an ffconcat list of the
// preroll and main, a file of the video and audio, which the concat demuxer
// joins; main, read again shifted along by the preroll's length, gives the
// tags and chapters, and side the subtitles and attachments, shifted
// likewise. mainInfo is what main holds, and id the preroll's.
func (p *plan) joinArgs(out, list, main, side string, length time.Duration, id string, mainInfo mediaInfo) []string {
	s := p.settings
	shift := seconds6(length)
	args := []string{"-hide_banner", "-nostdin", "-nostats", "-y", "-progress", "pipe:1",
		"-f", "concat", "-safe", "0", "-i", list, "-itsoffset", shift, "-i", main}
	if side != "" {
		args = append(args, "-itsoffset", shift, "-i", side)
	}
	args = append(args, "-map", "0:v:0")
	for i := range p.audio {
		args = append(args, "-map", "0:a:"+strconv.Itoa(i))
	}
	for i := range p.subs {
		args = append(args, "-map", "2:s:"+strconv.Itoa(i))
	}
	for i := range p.fonts {
		args = append(args, "-map", "2:t:"+strconv.Itoa(i))
	}
	args = append(args, "-c", "copy")
	// The concat demuxer takes the video's and audio's tags and flags from
	// the preroll, so give them main's. Naming where any stream's tags come
	// from stops ffmpeg copying the rest's by itself, so name every one.
	if !s.StripMetadata {
		args = append(args, "-map_metadata", "1", "-map_chapters", "1", "-map_metadata:s:v:0", "1:s:v:0")
		for i := range p.audio {
			args = append(args, fmt.Sprintf("-map_metadata:s:a:%d", i), fmt.Sprintf("1:s:a:%d", i))
		}
		for i := range p.subs {
			args = append(args, fmt.Sprintf("-map_metadata:s:s:%d", i), fmt.Sprintf("2:s:s:%d", i))
		}
		for i := range p.fonts {
			args = append(args, fmt.Sprintf("-map_metadata:s:t:%d", i), fmt.Sprintf("2:s:t:%d", i))
		}
	}
	v, a := 0, 0
	for _, st := range mainInfo.Streams {
		switch {
		case st.Type == "video" && !st.CoverArt && v == 0:
			args = append(args, "-disposition:v:0", dispositionOf(st))
			v++
		case st.Type == "audio" && a < len(p.audio):
			flags := dispositionOf(st)
			if a == 0 && p.firstAudioDefault() {
				flags = "default"
			}
			args = append(args, fmt.Sprintf("-disposition:a:%d", a), flags)
			a++
		}
	}
	args = append(args, p.tagArgs()...)
	args = append(args, "-metadata", prerollTag+"="+id+" "+shift)
	return append(args, "-max_muxing_queue_size", "4096", "-f", "matroska", out)
}

// mergeArgs is the ffmpeg command line that puts the encoding pass's video
// and the audio pass's audio together in out, for the preroll to be joined
// onto, since the concat demuxer joins whole files.
func (p *plan) mergeArgs(out, av, audio string) []string {
	return []string{"-hide_banner", "-nostdin", "-nostats", "-y", "-progress", "pipe:1", "-i", av, "-i", audio,
		"-map", "0:v:0", "-map", "1:a", "-c", "copy", "-max_muxing_queue_size", "4096", "-f", "matroska", out}
}

// firstAudioDefault reports whether the first audio track kept is made the
// default, as it is when the settings keep tracks by language.
func (p *plan) firstAudioDefault() bool {
	return p.settings.AudioTracks == KeepLanguage || p.settings.AudioTracks == KeepBest
}

// tagArgs are the subtitle flags and titles, and the tags, that the settings
// give a result, whichever pass puts it together.
func (p *plan) tagArgs() []string {
	s := p.settings
	var args []string
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
		args = append(args, fmt.Sprintf("-disposition:s:%d", i), disposition)
		if label := sub.label(); label != "" && (s.StripMetadata || sub.Title == "") {
			args = append(args, fmt.Sprintf("-metadata:s:s:%d", i), "title="+label)
		}
	}

	if s.StripMetadata {
		args = append(args, "-map_metadata", "-1", "-map_chapters", "-1")
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
	return args
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
