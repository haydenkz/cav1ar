package encode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// subWindow is how much of a file tidying reads: enough to judge every
// subtitle track, without reading all of a long film.
const subWindow = 20 * time.Minute

// subStats is what reading the opening of a subtitle track found.
type subStats struct {
	events int  // lines shown, not counting repeats; for image subtitles, an estimate from their packets
	text   bool // whether the words below were read, as they are for text subtitles

	scripts             map[string]int // letters, by writing system
	words, englishWords int
	cues                int // lines describing sounds or naming who speaks, as SDH subtitles do
	drawings            int // vector drawings from styled subtitles, which aren't lines
}

func (st *subStats) letters() int {
	n := 0
	for _, c := range st.scripts {
		n += c
	}
	return n
}

// script is the writing system most of the letters are in.
func (st *subStats) script() string {
	best, most := "", 0
	for s, n := range st.scripts {
		if n > most {
			best, most = s, n
		}
	}
	return best
}

var textSubs = map[string]bool{"subrip": true, "ass": true, "ssa": true, "webvtt": true, "mov_text": true}

// readSubs reads the first subWindow of each subtitle track in subs.
func readSubs(ctx context.Context, in string, subs []subOut) (map[int]*subStats, error) {
	window := strconv.Itoa(int(subWindow.Seconds()))
	stats := map[int]*subStats{}

	// Image subtitles can't be read, but how many there are still says a lot.
	// Text tracks are counted as they are read below, so with no image
	// tracks there's nothing to count.
	if slices.ContainsFunc(subs, func(s subOut) bool { return !textSubs[s.Codec] }) {
		out, err := exec.CommandContext(ctx, FFprobe(), "-v", "error", "-read_intervals", "%+"+window,
			"-count_packets", "-select_streams", "s", "-show_entries", "stream=index,codec_name,nb_read_packets",
			"-of", "json", in).Output()
		if err != nil {
			return nil, fmt.Errorf("counting subtitles: %w", err)
		}
		var counts struct {
			Streams []struct {
				Index   int    `json:"index"`
				Codec   string `json:"codec_name"`
				Packets string `json:"nb_read_packets"`
			} `json:"streams"`
		}
		if err := json.Unmarshal(out, &counts); err != nil {
			return nil, fmt.Errorf("counting subtitles: %w", err)
		}
		for _, s := range counts.Streams {
			n, _ := strconv.Atoi(s.Packets)
			if s.Codec == "hdmv_pgs_subtitle" || s.Codec == "dvb_subtitle" {
				n /= 2 // one packet shows a line, the next clears it
			}
			stats[s.Index] = &subStats{events: n}
		}
	}

	dir, err := os.MkdirTemp("", "cav1ar-subs-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	args := []string{"-v", "error", "-nostdin", "-i", in}
	var text []subOut
	for _, sub := range subs {
		if textSubs[sub.Codec] {
			text = append(text, sub)
			// -t limits subtitles only as an output option.
			args = append(args, "-map", "0:"+strconv.Itoa(sub.Index), "-c:s", "srt", "-t", window, "-f", "srt",
				filepath.Join(dir, strconv.Itoa(sub.Index)+".srt"))
		}
	}
	if len(text) == 0 {
		return stats, nil
	}
	if out, err := exec.CommandContext(ctx, FFmpeg, args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("reading subtitles: %s", lastLine(string(out), err))
	}
	for _, sub := range text {
		data, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(sub.Index)+".srt"))
		if err != nil {
			return nil, fmt.Errorf("reading subtitles: %w", err)
		}
		stats[sub.Index] = readSRT(string(data))
	}
	return stats, nil
}

var (
	srtMarkup    = regexp.MustCompile(`<[^>]*>|\{[^}]*\}`)
	soundCue     = regexp.MustCompile(`\[[^\]]+\]|\([^)]+\)`)
	speakerLabel = regexp.MustCompile(`(?m)^\s*-?\s*[A-Z][A-Z0-9 .'#-]{1,24}:`)
	wordPattern  = regexp.MustCompile(`[\p{L}'’]+`)
)

// englishMarkers are common in English dialogue and rare in other languages
// written in Latin letters.
var englishMarkers = setOf(strings.Fields(`the you to and it of that is what this your don't it's have for not he
	are just with i'm know get can but here there she they him her like right okay yeah out now come did how why
	well about want got think gonna would could been from will were them then when who because really something
	sorry what's i'll you're can't let's thank good look need never tell going where back time`))

// readSRT counts an SRT file's lines and sizes up their words.
func readSRT(srt string) *subStats {
	st := &subStats{text: true, scripts: map[string]int{}}
	last := ""
	for block := range strings.SplitSeq(strings.ReplaceAll(srt, "\r\n", "\n"), "\n\n") {
		_, body, ok := strings.Cut(block, "-->")
		if !ok {
			continue
		}
		_, body, _ = strings.Cut(body, "\n") // the rest of the timing line
		body = strings.TrimSpace(srtMarkup.ReplaceAllString(body, ""))
		if isDrawing(body) {
			st.drawings++
			continue
		}
		// Karaoke-style lyrics repeat a line as each word lights up.
		if body == "" || body == last {
			continue
		}
		last = body
		st.events++
		if soundCue.MatchString(body) || speakerLabel.MatchString(body) {
			st.cues++
		}
		for _, r := range body {
			if s := scriptOf(r); s != "" {
				st.scripts[s]++
			}
		}
		for _, w := range wordPattern.FindAllString(strings.ToLower(body), -1) {
			st.words++
			if englishMarkers[strings.ReplaceAll(w, "’", "'")] {
				st.englishWords++
			}
		}
	}
	return st
}

// isDrawing reports whether a line is a vector drawing from styled (ASS)
// subtitles, such as "m 20 0 b 65 0 56 72", which signs are made of.
func isDrawing(body string) bool {
	fields := strings.Fields(body)
	if len(fields) < 3 || fields[0] != "m" {
		return false
	}
	numbers := 0
	for _, f := range fields {
		if _, err := strconv.ParseFloat(f, 64); err == nil {
			numbers++
		}
	}
	return numbers*2 > len(fields)
}

// scriptOf names the writing system a letter belongs to.
func scriptOf(r rune) string {
	switch {
	case !unicode.IsLetter(r):
		return ""
	case unicode.Is(unicode.Latin, r):
		return "Latin"
	case unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana):
		return "CJK"
	case unicode.Is(unicode.Hangul, r):
		return "Hangul"
	case unicode.Is(unicode.Cyrillic, r):
		return "Cyrillic"
	case unicode.Is(unicode.Arabic, r):
		return "Arabic"
	case unicode.Is(unicode.Hebrew, r):
		return "Hebrew"
	case unicode.Is(unicode.Greek, r):
		return "Greek"
	case unicode.Is(unicode.Thai, r):
		return "Thai"
	case unicode.Is(unicode.Devanagari, r):
		return "Devanagari"
	}
	return "other"
}

var scriptNames = map[string]string{
	"Latin": "Latin letters", "CJK": "Chinese or Japanese characters", "Hangul": "Korean",
	"Cyrillic": "Cyrillic", "Arabic": "Arabic script", "Hebrew": "Hebrew", "Greek": "Greek",
	"Thai": "Thai", "Devanagari": "Devanagari", "other": "another script",
}

// languageScripts is the writing system each language is written in, for
// the languages tidying can check.
var languageScripts = map[string]string{
	"eng": "Latin", "spa": "Latin", "fra": "Latin", "deu": "Latin", "ita": "Latin", "por": "Latin",
	"nld": "Latin", "swe": "Latin", "nor": "Latin", "nob": "Latin", "dan": "Latin", "fin": "Latin",
	"pol": "Latin", "ces": "Latin", "hun": "Latin", "ron": "Latin", "tur": "Latin", "vie": "Latin",
	"ind": "Latin", "rus": "Cyrillic", "ukr": "Cyrillic", "bul": "Cyrillic", "srp": "Cyrillic",
	"zho": "CJK", "jpn": "CJK", "kor": "Hangul", "ara": "Arabic", "fas": "Arabic", "heb": "Hebrew",
	"ell": "Greek", "tha": "Thai", "hin": "Devanagari",
}

// Tidying thresholds.
const (
	forcedShare  = 5   // a track with under 1/forcedShare the lines of the fullest one is forced
	minFullLines = 100 // ...when the fullest one has at least this many
	sdhCueShare  = 10  // a track with at least 1/sdhCueShare of its lines describing sounds is SDH
)

// tidySubs uses what readSubs found to drop subtitle tracks whose text isn't
// in the language they are tagged with, flag forced and SDH tracks the
// source didn't, and keep one track per language for each of those roles
// and the full dialogue.
func (p *plan) tidySubs(stats map[int]*subStats) {
	p.checkSubs = false
	for i := range p.subs {
		p.subs[i].stats = stats[p.subs[i].Index]
	}
	var kept []subOut
	for _, sub := range p.subs {
		if st := sub.stats; st != nil && st.drawings > 0 && st.events == 0 && sub.Codec != "ass" && sub.Codec != "ssa" {
			// Styled subtitles converted to plain text, losing what made
			// sense of them.
			p.note("Dropping subtitle track %d: it only holds drawings, which show as gibberish", sub.Index)
			continue
		}
		if why := wrongLanguage(sub, p.subs); why != "" {
			p.note("Dropping subtitle track %d: %s", sub.Index, why)
			continue
		}
		kept = append(kept, sub)
	}

	fullest := map[string]int{} // lines in the fullest track of each language
	for _, sub := range kept {
		lang := normLanguage(sub.Language)
		fullest[lang] = max(fullest[lang], lines(sub))
	}
	for i := range kept {
		sub := &kept[i]
		top := fullest[normLanguage(sub.Language)]
		if sub.stats == nil || sub.forced || top < minFullLines || lines(*sub)*forcedShare >= top {
			continue
		}
		sub.forced, sub.sdh = true, false
		p.note("Flagging subtitle track %d as forced: %d lines, against %d in the fullest %s track",
			sub.Index, lines(*sub), top, languageName(sub.Language))
	}
	for i := range kept {
		sub := &kept[i]
		if !sub.forced && !sub.sdh && describesSounds(*sub, kept) {
			sub.sdh = true
			p.note("Flagging subtitle track %d as SDH: %d of its %d lines describe sounds or name speakers",
				sub.Index, sub.stats.cues, sub.stats.events)
		}
	}

	// One track per language and role: text over images, which many players
	// can't show without re-encoding, then the one with the most lines. A
	// track with no lines read can't be judged, so it stays.
	best := map[string]int{}
	for i, sub := range kept {
		key := normLanguage(sub.Language) + "/" + sub.role()
		if j, ok := best[key]; !ok || betterSub(sub, kept[j]) {
			best[key] = i
		}
	}
	p.subs = nil
	for i, sub := range kept {
		if best[normLanguage(sub.Language)+"/"+sub.role()] != i && lines(sub) > 0 {
			p.note("Dropping subtitle track %d: a second %s %s track", sub.Index, languageName(sub.Language), sub.role())
			continue
		}
		p.subs = append(p.subs, sub)
	}
	if !hasStyledSubs(p.subs) {
		p.fonts = nil
	}
}

// wrongLanguage says why a track's text isn't in the language it is tagged
// with, or returns "" if it is, or can't tell. Telling English from other
// languages written in Latin letters is less certain than telling scripts
// apart, so a track is only judged not English when another track tagged
// English does read as English.
func wrongLanguage(sub subOut, all []subOut) string {
	st := sub.stats
	if st == nil || !st.text {
		return ""
	}
	lang := normLanguage(sub.Language)
	want, total := languageScripts[lang], st.letters()
	if script := st.script(); want != "" && total >= 200 && script != want && st.scripts[want]*3 < total {
		return fmt.Sprintf("tagged %s, but written in %s", sub.Language, scriptNames[script])
	}
	if lang == "eng" && !readsEnglish(st) && slices.ContainsFunc(all, func(o subOut) bool {
		return normLanguage(o.Language) == "eng" && o.stats != nil && o.stats.text && readsEnglish(o.stats)
	}) {
		return fmt.Sprintf("tagged %s, but doesn't read as English", sub.Language)
	}
	return ""
}

// readsEnglish reports whether a track's words look like English. Too few
// words to tell count as English.
func readsEnglish(st *subStats) bool {
	return st.words < 150 || st.englishWords*100 >= st.words*8
}

// describesSounds reports whether sub reads like SDH: many of its lines
// describe sounds or name speakers, and no other full track in its language
// does so as much, which would make them both the same kind.
func describesSounds(sub subOut, all []subOut) bool {
	st := sub.stats
	if st == nil || !st.text || st.events < 50 || st.cues*sdhCueShare < st.events {
		return false
	}
	for _, other := range all {
		if other.Index == sub.Index || other.forced || normLanguage(other.Language) != normLanguage(sub.Language) {
			continue
		}
		if o := other.stats; o != nil && o.text && o.events > 0 && o.cues*st.events*2 >= st.cues*o.events {
			return false // at least half as many cues: no plainer track to tell it from
		}
	}
	return true
}

func betterSub(a, b subOut) bool {
	at, bt := a.stats != nil && a.stats.text, b.stats != nil && b.stats.text
	if at != bt {
		return at
	}
	return lines(a) > lines(b)
}

func lines(sub subOut) int {
	if sub.stats == nil {
		return 0
	}
	return sub.stats.events
}

func hasStyledSubs(subs []subOut) bool {
	for _, s := range subs {
		if s.Codec == "ass" || s.Codec == "ssa" {
			return true
		}
	}
	return false
}

// languageName is how notes refer to a track's language.
func languageName(code string) string {
	if code == "" {
		return "untagged"
	}
	return code
}

func setOf(items []string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, s := range items {
		set[s] = true
	}
	return set
}

// lastLine is the last line of a command's output, which usually says why
// it failed, or err if it printed nothing.
func lastLine(out string, err error) string {
	lines := strings.FieldsFunc(strings.TrimSpace(out), func(r rune) bool { return r == '\n' || r == '\r' })
	if len(lines) == 0 {
		return err.Error()
	}
	return strings.TrimSpace(lines[len(lines)-1])
}
