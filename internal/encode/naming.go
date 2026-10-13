package encode

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// DefaultNameTemplate names encodes like
// "Movie Title (2024) [1080p] [BRRip] [AV1] [AAC2.0] - Group.mkv".
const DefaultNameTemplate = "{title} ({year}) {episode} [{resolution}] [{source}] [AV1] [{audio}] - {group}"

// NamePlaceholders lists what name and title templates can contain.
var NamePlaceholders = []string{
	"{title}", "{year}", "{episode}", "{resolution}", "{source}", "{audio}", "{group}", "{name}",
}

// release describes a source in the terms output names are built from.
type release struct {
	Name       string // source file name without extension
	Title      string // e.g. "Movie Title", parsed from Name or the folder
	Year       string // e.g. "2024", parsed from Name or the folder
	Episode    string // e.g. "S01E02" or "S01E01-E02", parsed from Name
	Source     string // e.g. "BRRip", parsed from Name
	Resolution string // e.g. "1080p", from the video stream
	Audio      string // the first audio track as encoded, e.g. "AAC2.0"
	Group      string // the release group setting
}

var (
	yearPattern = regexp.MustCompile(`\b(19|20)\d\d\b`)
	// episodePattern matches S01E02, S01E01E02 and S01E01-E02; the first
	// group is the whole tag.
	episodePattern = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(S(\d{1,2})[ ._-]*E(\d{1,3})((?:[ ._-]*-?[ ._-]*E\d{1,3})*))`)
	extraEpisode   = regexp.MustCompile(`(?i)E(\d{1,3})`)
	crossEpisode   = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])((\d{1,2})x(\d{2,3}))(?:[^a-z0-9]|$)`) // 1x04
	// techPattern finds where a scene name's title ends and its technical
	// details begin.
	techPattern = regexp.MustCompile(`(?i)\b(\d{3,4}p|4k|uhd|hdr|bluray|blu-ray|bdrip|brrip|remux|web-?dl|webrip|web|hdtv|dvdrip|dvd|hdrip|x26[45]|h ?26[45]|hevc|avc|av1|xvid|aac|ac3|dts|repack|proper|extended|unrated|remastered)\b`)
	sources     = []struct {
		pattern *regexp.Regexp
		name    string
	}{
		{regexp.MustCompile(`(?i)\b(bluray|blu-ray|bdrip|brrip|bdremux|remux)\b`), "BRRip"},
		{regexp.MustCompile(`(?i)\b(web-?dl|webrip|web)\b`), "WEBRip"},
		{regexp.MustCompile(`(?i)\b(hdtv|pdtv)\b`), "HDTV"},
		{regexp.MustCompile(`(?i)\b(dvdrip|dvd)\b`), "DVDRip"},
		{regexp.MustCompile(`(?i)\bhdrip\b`), "HDRip"},
	}
	leadingTag   = regexp.MustCompile(`^\[[^\]]*\]\s*`)       // "[Group] Show - 01"
	folderTags   = regexp.MustCompile(`\{[^}]*\}|\[[^\]]*\]`) // "{tmdb-123}", "[imdbid-tt123]"
	emptyBracket = regexp.MustCompile(`\(\s*\)|\[\s*\]|\{\s*\}`)
	extraSpace   = regexp.MustCompile(`\s{2,}`)
)

// parseRelease reads the title, year, episode and source out of a file name,
// either scene-style such as "Movie.Title.2024.1080p.BluRay.x264-GRP.mkv" or
// already tidy such as "Movie Title (2024) [1080p].mkv".
func parseRelease(path string) release {
	return parseName(baseName(path))
}

func parseName(name string) release {
	r := release{Name: name}
	words := name
	if !strings.Contains(words, " ") { // dots and underscores separate words only in scene names
		words = strings.NewReplacer(".", " ", "_", " ").Replace(words)
	}
	words = leadingTag.ReplaceAllString(words, "")

	end := len(words)
	if loc := techPattern.FindStringIndex(words); loc != nil {
		end = loc[0]
	}
	if tag, start := episodeTag(words); tag != "" {
		r.Episode = tag
		end = min(end, start)
	}
	// The year is the last one before the technical details, skipping one
	// that starts the name, as in "1917.2019" or "2001.A.Space.Odyssey.1968".
	for _, loc := range yearPattern.FindAllStringIndex(words[:end], -1) {
		if loc[0] > 0 {
			r.Year = words[loc[0]:loc[1]]
			end = loc[0]
		}
	}
	r.Title = strings.TrimRight(strings.TrimSpace(words[:end]), " -([{")
	r.Title = extraSpace.ReplaceAllString(r.Title, " ")
	if r.Title == "" {
		r.Title = r.Name
	}

	for _, s := range sources {
		if s.pattern.MatchString(words) {
			r.Source = s.name
			break
		}
	}
	return r
}

// episodeTag finds an episode number in a name and writes it as S01E02, or
// S01E01-E02 for a file holding several. start is where it was found.
func episodeTag(name string) (tag string, start int) {
	if m := episodePattern.FindStringSubmatchIndex(name); m != nil {
		season, _ := strconv.Atoi(name[m[4]:m[5]])
		episode, _ := strconv.Atoi(name[m[6]:m[7]])
		tag = fmt.Sprintf("S%02dE%02d", season, episode)
		for _, extra := range extraEpisode.FindAllStringSubmatch(name[m[8]:m[9]], -1) {
			n, _ := strconv.Atoi(extra[1])
			tag += fmt.Sprintf("-E%02d", n)
		}
		return tag, m[2]
	}
	if m := crossEpisode.FindStringSubmatchIndex(name); m != nil {
		season, _ := strconv.Atoi(name[m[4]:m[5]])
		episode, _ := strconv.Atoi(name[m[6]:m[7]])
		return fmt.Sprintf("S%02dE%02d", season, episode), m[2]
	}
	return "", 0
}

// parseFolder reads a title and year from a folder name such as
// "Movie Title (2024) [tmdbid-123]" or "Show Name".
func parseFolder(name string) release {
	name = strings.TrimSpace(extraSpace.ReplaceAllString(folderTags.ReplaceAllString(name, ""), " "))
	return parseName(name)
}

// nameSource fills r's title and year from the folders path is in, following
// the layout of the watched folder it is under. Shows must have an episode
// number in the file name, since nothing else says which episode a file is.
func nameSource(r release, path string, folder WatchFolder) (release, error) {
	rel, err := filepath.Rel(folder.Path, path)
	if err != nil || rel == filepath.Base(path) { // directly in the watched folder
		if folder.Kind == FolderShows && r.Episode == "" {
			return r, fmt.Errorf("no episode number (like S01E02) in the file name")
		}
		return r, nil
	}
	var from release
	switch folder.Kind {
	case FolderMovies:
		from = parseFolder(filepath.Base(filepath.Dir(path)))
	case FolderShows:
		if r.Episode == "" {
			return r, fmt.Errorf("no episode number (like S01E02) in the file name")
		}
		show, _, _ := strings.Cut(rel, string(filepath.Separator))
		from = parseFolder(show)
	default:
		return r, nil
	}
	r.Title, r.Year = from.Title, from.Year
	return r, nil
}

// expand fills in a template's placeholders, then tidies away brackets and
// separators left empty by missing details.
func (r release) expand(template string) string {
	s := strings.NewReplacer(
		"{title}", r.Title, "{year}", r.Year, "{episode}", r.Episode,
		"{resolution}", r.Resolution, "{source}", r.Source, "{audio}", r.Audio,
		"{group}", r.Group, "{name}", r.Name,
	).Replace(template)
	s = emptyBracket.ReplaceAllString(s, "")
	s = extraSpace.ReplaceAllString(s, " ")
	return strings.Trim(s, " -._")
}

// unsafeInName are characters that are invalid or awkward in file names on
// some systems.
var unsafeInName = strings.NewReplacer(
	"/", "-", `\`, "-", ":", "", "*", "", "?", "", `"`, "", "<", "", ">", "", "|", "",
)

// fileName turns a name template into a file name for one encode.
func fileName(template string, r release) string {
	name := unsafeInName.Replace(r.expand(template))
	name = strings.TrimSpace(extraSpace.ReplaceAllString(name, " "))
	if name == "" {
		name = r.Name
	}
	return name + ".mkv"
}

// resolution names a frame size the way release names do, allowing for
// widescreen video that is shorter than its nominal height: a 1920x800 scope
// film is still 1080p.
func resolution(width, height int) string {
	switch {
	case width == 0 && height == 0:
		return ""
	case width >= 3200 || height >= 1800:
		return "2160p"
	case width >= 2400 || height >= 1300:
		return "1440p"
	case width >= 1600 || height >= 900:
		return "1080p"
	case width >= 1100 || height >= 650:
		return "720p"
	case height > 480:
		return "576p"
	}
	return "480p"
}

// fitSize scales width x height down to fit inside maxHeight and its 16:9
// width, keeping both even, as encoders need. maxHeight 0 keeps the size.
func fitSize(width, height, maxHeight int) (int, int) {
	if maxHeight <= 0 || width <= 0 || height <= 0 {
		return width, height
	}
	maxWidth := maxHeight * 16 / 9
	scale := min(1, float64(maxWidth)/float64(width), float64(maxHeight)/float64(height))
	even := func(n float64) int { return max(2, int(n/2+0.5)*2) }
	return even(float64(width) * scale), even(float64(height) * scale)
}

// audioTag describes an audio track as it will be encoded, e.g. "AAC2.0" or
// "Opus5.1".
func audioTag(a audioOut) string {
	if a.copy {
		return codecTag(a.Codec) + channelTag(a.Channels)
	}
	return codecTag(string(a.codec)) + channelTag(a.channels)
}

// outChannels is how many channels a track has once encoded, after
// downmixing or remapping to a layout the encoders accept.
func outChannels(ch int, downmix bool) int {
	switch {
	case downmix && ch > 2:
		return 2
	case ch >= 7:
		return 8
	case ch >= 3:
		return 6
	}
	return ch
}

// autoBitrate is the auto bitrate for a track, in kbps: near-transparent for
// its codec and number of channels.
func autoBitrate(codec AudioCodec, ch int) int {
	kbps := 384 // Opus 7.1
	switch {
	case ch == 1:
		kbps = 64
	case ch <= 2:
		kbps = 128
	case ch <= 6:
		kbps = 256
	}
	if codec == AAC { // AAC needs about half as much again for the same quality
		kbps = kbps * 3 / 2
	}
	return kbps
}

// codecTag names an audio codec the way release names do.
func codecTag(codec string) string {
	switch {
	case codec == "eac3":
		return "DDP"
	case codec == "ac3":
		return "DD"
	case codec == "truehd":
		return "TrueHD"
	case codec == "opus" || codec == "vorbis":
		return strings.ToUpper(codec[:1]) + codec[1:]
	case strings.HasPrefix(codec, "pcm_"):
		return "PCM"
	}
	return strings.ToUpper(codec)
}

func channelTag(n int) string {
	switch n {
	case 0:
		return ""
	case 3:
		return "2.1"
	case 6:
		return "5.1"
	case 7:
		return "6.1"
	case 8:
		return "7.1"
	}
	return fmt.Sprintf("%d.0", n)
}

// sample is a made-up source used to preview templates: a 1080p Blu-ray rip
// with 5.1 E-AC-3 audio.
func (s Settings) sample() release {
	r := parseRelease("Movie.Title.2024.1080p.BluRay.x264-GRP.mkv")
	r.Resolution = "1080p"
	r.Audio = audioTag(s.audioOut(streamInfo{Codec: "eac3", Channels: 6}))
	r.Group = strings.TrimSpace(s.ReleaseGroup)
	return r
}

// PreviewName shows what NameTemplate produces for a sample source.
func (s Settings) PreviewName() string { return fileName(s.NameTemplate, s.sample()) }

// PreviewTitle shows what the Title template produces for a sample source.
func (s Settings) PreviewTitle() string { return s.sample().expand(s.Title) }
