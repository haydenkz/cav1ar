package encode

import (
	"fmt"
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
	Title      string // e.g. "Movie Title", parsed from Name
	Year       string // e.g. "2024", parsed from Name
	Episode    string // e.g. "S01E02", parsed from Name
	Source     string // e.g. "BRRip", parsed from Name
	Resolution string // e.g. "1080p", from the video stream
	Audio      string // the first audio track as encoded, e.g. "AAC2.0"
	Group      string // the release group setting
}

var (
	yearPattern    = regexp.MustCompile(`\b(19|20)\d\d\b`)
	episodePattern = regexp.MustCompile(`(?i)\bS(\d{1,2}) ?E(\d{1,3})\b`)
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
	leadingTag   = regexp.MustCompile(`^\[[^\]]*\]\s*`) // "[Group] Show - 01"
	emptyBracket = regexp.MustCompile(`\(\s*\)|\[\s*\]|\{\s*\}`)
	extraSpace   = regexp.MustCompile(`\s{2,}`)
	av1Word      = regexp.MustCompile(`(?i)(^|[^a-z0-9])av1([^a-z0-9]|$)`)
)

// parseRelease reads the title, year, episode and source out of a scene-style
// file name such as "Movie.Title.2024.1080p.BluRay.x264-GRP.mkv".
func parseRelease(path string) release {
	r := release{Name: baseName(path)}
	words := strings.NewReplacer(".", " ", "_", " ").Replace(r.Name)
	words = leadingTag.ReplaceAllString(words, "")

	end := len(words)
	if loc := techPattern.FindStringIndex(words); loc != nil {
		end = loc[0]
	}
	if m := episodePattern.FindStringSubmatchIndex(words); m != nil {
		season, _ := strconv.Atoi(words[m[2]:m[3]])
		episode, _ := strconv.Atoi(words[m[4]:m[5]])
		r.Episode = fmt.Sprintf("S%02dE%02d", season, episode)
		end = min(end, m[0])
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
// widescreen video that is shorter than its nominal height.
func resolution(width, height int) string {
	switch {
	case width == 0 && height == 0:
		return ""
	case width >= 3200 || height >= 1800:
		return "2160p"
	case width >= 1800 || height >= 1000:
		return "1080p"
	case width >= 1200 || height >= 700:
		return "720p"
	case height > 480:
		return "576p"
	}
	return "480p"
}

// audioTag describes an audio track as it will be encoded, e.g. "AAC2.0" or
// "Opus5.1".
func audioTag(st streamInfo, s Settings) string {
	if s.AudioKbps == AudioCopy {
		return codecTag(st.Codec) + channelTag(st.Channels)
	}
	return "Opus" + channelTag(opusChannels(st.Channels, s.Downmix))
}

// opusChannels is how many channels a track has once encoded to Opus, after
// downmixing or remapping to a layout libopus accepts.
func opusChannels(ch int, downmix bool) int {
	switch {
	case downmix:
		return 2
	case ch >= 7:
		return 8
	case ch >= 3:
		return 6
	}
	return ch
}

// opusBitrate is the auto bitrate for a track, in kbps: near-transparent for
// its number of channels.
func opusBitrate(ch int) int {
	switch {
	case ch == 1:
		return 64
	case ch <= 2:
		return 128
	case ch <= 6:
		return 256
	}
	return 384
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
	r.Audio = audioTag(streamInfo{Codec: "eac3", Channels: 6}, s)
	r.Group = strings.TrimSpace(s.ReleaseGroup)
	return r
}

// PreviewName shows what NameTemplate produces for a sample source.
func (s Settings) PreviewName() string { return fileName(s.NameTemplate, s.sample()) }

// PreviewTitle shows what the Title template produces for a sample source.
func (s Settings) PreviewTitle() string { return s.sample().expand(s.Title) }
