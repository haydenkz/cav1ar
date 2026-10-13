package encode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A preroll is a short clip played before every result, such as an intro.
// It is encoded to match each result's video and audio, down to the AV1
// sequence header where the encoder allows, then joined on without
// re-encoding the result. The result is tagged with which preroll it starts
// with and how long it runs, so it is never added twice, and can be taken
// off again or swapped for another.

// prerollTag is the container tag recording a file's preroll, as "<id> <seconds>".
const prerollTag = "CAV1AR_PREROLL"

// prerollIDs caches each preroll file's ID by path, size and modification time.
var prerollIDs sync.Map

// prerollID identifies a preroll file by its contents.
func prerollID(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("preroll: %w", err)
	}
	key := fmt.Sprintf("%s|%d|%d", path, fi.Size(), fi.ModTime().UnixNano())
	if id, ok := prerollIDs.Load(key); ok {
		return id.(string), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("preroll: %w", err)
	}
	sum := sha256.Sum256(data)
	id := hex.EncodeToString(sum[:6])
	prerollIDs.Store(key, id)
	return id, nil
}

// prerollOf reads the preroll a file starts with from its tags.
func prerollOf(info mediaInfo) (id string, length time.Duration) {
	id, secs, ok := strings.Cut(info.Tags[prerollTag], " ")
	s, err := strconv.ParseFloat(secs, 64)
	if !ok || err != nil || s <= 0 {
		return "", 0
	}
	return id, time.Duration(s * float64(time.Second))
}

// joinPoint finds where the video after a file's preroll starts: the first
// keyframe at about length in. Cutting there with stream copy keeps all of
// the video after it and none of the preroll. The keyframe usually lands a
// few milliseconds after length, since files start a little in to make room
// for audio priming.
func joinPoint(ctx context.Context, path string, length time.Duration) (time.Duration, error) {
	// Read from the start: seeking would land on the preroll's only keyframe.
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0",
		"-read_intervals", "%"+seconds6(length+3*time.Second), "-show_entries", "packet=pts_time,flags", "-of", "csv=p=0", path).Output()
	if err != nil {
		return 0, fmt.Errorf("finding the end of the preroll: %w", err)
	}
	for line := range strings.Lines(string(out)) {
		pts, flags, _ := strings.Cut(strings.TrimSpace(line), ",")
		at := seconds(pts)
		if strings.HasPrefix(flags, "K") && at >= length-100*time.Millisecond {
			return at, nil
		}
	}
	return 0, errors.New("couldn't find where the preroll ends")
}

// prerollFor returns a copy of the preroll encoded to match main, building
// it the first time each kind of main needs one, and how long it runs.
func prerollFor(ctx context.Context, s Settings, mainPath string, main mediaInfo, u updates) (string, time.Duration, error) {
	id, err := prerollID(s.Preroll)
	if err != nil {
		return "", 0, err
	}
	src, err := probe(ctx, s.Preroll)
	if err != nil {
		return "", 0, fmt.Errorf("preroll: %w", err)
	}
	var v streamInfo
	var audio []streamInfo
	for _, st := range main.Streams {
		switch {
		case st.Type == "video" && !st.CoverArt && v.Type == "":
			v = st
		case st.Type == "audio":
			audio = append(audio, st)
		}
	}
	fps, ok := ratio(v.FrameRate)
	if !ok {
		v.FrameRate, fps = "24000/1001", 24000.0/1001
	}
	// Whole frames, so the video and audio of the preroll end together.
	frames := max(1, int(math.Round(src.Duration.Seconds()*fps)))
	length := time.Duration(float64(frames) / fps * float64(time.Second))

	config, err := av1Config(ctx, mainPath)
	if err != nil {
		return "", 0, err
	}
	args := []string{"-hide_banner", "-nostdin", "-nostats", "-v", "error", "-y", "-i", s.Preroll}
	if !hasAudio(src) {
		args = append(args, "-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo")
	}
	args = append(args, "-map", "0:v:0")
	vf := []string{
		"fps=" + v.FrameRate,
		fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=increase:flags=lanczos", v.Width, v.Height),
		fmt.Sprintf("crop=%d:%d", v.Width, v.Height),
	}
	tenBit := strings.Contains(v.PixFmt, "10")
	switch {
	case s.Encoder == EncoderNVIDIA && tenBit:
		vf = append(vf, "format=p010le")
	case tenBit:
		vf = append(vf, "format=yuv420p10le")
	default:
		vf = append(vf, "format=yuv420p")
	}
	vf = append(vf, "setparams="+colorParams(v))
	args = append(args, "-vf", strings.Join(vf, ","), "-frames:v", strconv.Itoa(frames))
	if s.Encoder == EncoderNVIDIA {
		maxRate := nvencMaxRate(resolution(v.Width, v.Height))
		args = append(args, "-c:v", "av1_nvenc", "-preset", "p"+strconv.Itoa(s.NVENCPreset), "-tune", "hq",
			"-rc", "vbr", "-cq", strconv.Itoa(s.CRF), "-b:v", "0",
			"-maxrate", strconv.Itoa(maxRate)+"M", "-bufsize", strconv.Itoa(2*maxRate)+"M",
			"-rc-lookahead", "32", "-spatial-aq", "1", "-temporal-aq", "1", "-g", "240")
		// Matching the level and tier keeps the sequence header the same as
		// main's, so players see one continuous video.
		if v.Level >= 0 && v.Level < 24 {
			args = append(args, "-level", fmt.Sprintf("%d.%d", 2+v.Level/4, v.Level%4))
		}
		if len(config) > 2 {
			args = append(args, "-tier", strconv.Itoa(int(config[2]>>7)))
		}
	} else {
		args = append(args, "-c:v", "libsvtav1", "-preset", strconv.Itoa(s.Preset), "-crf", strconv.Itoa(s.CRF))
	}

	audioIn := "0:a:0"
	if !hasAudio(src) {
		audioIn = "1:a:0"
	}
	for i, a := range audio {
		codec, ok := audioEncoders[a.Codec]
		if !ok {
			return "", 0, fmt.Errorf("preroll: can't encode %s audio to match", a.Codec)
		}
		rate := a.SampleRate
		if rate <= 0 {
			rate = 48000
		}
		kbps := a.BitRate / 1000
		if kbps <= 0 {
			kbps = autoBitrate(AudioCodec(a.Codec), a.Channels)
		}
		args = append(args, "-map", audioIn,
			fmt.Sprintf("-c:a:%d", i), codec,
			fmt.Sprintf("-b:a:%d", i), fmt.Sprintf("%dk", kbps),
			fmt.Sprintf("-filter:a:%d", i), fmt.Sprintf("aresample=%d,aformat=channel_layouts=%s,apad,atrim=end=%s",
				rate, channelLayout(a.Channels), seconds6(length)))
	}

	key := sha256.Sum256([]byte(strings.Join(append([]string{id, hex.EncodeToString(config)}, args...), "\x00")))
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", 0, err
	}
	dir = filepath.Join(dir, "cav1ar", "preroll")
	out := filepath.Join(dir, hex.EncodeToString(key[:8])+".mkv")
	if exists(out) {
		return out, length, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, err
	}
	tmp := out + ".tmp"
	defer os.Remove(tmp)
	u.log(fmt.Sprintf("cav1ar: encoding the preroll to match %dx%d %s %s", v.Width, v.Height, v.FrameRate, v.PixFmt))
	if b, err := exec.CommandContext(ctx, "ffmpeg", append(args, "-f", "matroska", tmp)...).CombinedOutput(); err != nil {
		return "", 0, fmt.Errorf("encoding the preroll: %s", lastLine(string(b), err))
	}
	if got, err := av1Config(ctx, tmp); err == nil && !bytes.Equal(got, config) {
		u.log("cav1ar: the preroll's AV1 header differs from the video's, so some players may stutter where they meet")
	}
	if err := os.Rename(tmp, out); err != nil {
		return "", 0, err
	}
	return out, length, nil
}

// audioEncoders are the encoders that can match each audio codec.
var audioEncoders = map[string]string{
	"opus": "libopus", "aac": "aac", "ac3": "ac3", "eac3": "eac3", "flac": "flac", "mp3": "libmp3lame",
}

func channelLayout(ch int) string {
	switch ch {
	case 1:
		return "mono"
	case 3:
		return "2.1"
	case 6:
		return "5.1"
	case 8:
		return "7.1"
	case 0, 2:
		return "stereo"
	}
	return fmt.Sprintf("%dc", ch)
}

// colorParams describes v's colours for the setparams filter. Colours v
// leaves unspecified are set unspecified rather than left as the preroll's,
// since whether a sequence header describes its colours at all has to match.
func colorParams(v streamInfo) string {
	known := func(c string) bool { return c != "" && c != "unknown" && c != "reserved" }
	var parts []string
	for _, c := range []struct{ name, val string }{
		{"color_primaries", v.ColorPrimaries}, {"color_trc", v.ColorTrc}, {"colorspace", v.ColorSpace},
	} {
		if !known(c.val) {
			c.val = "unknown"
		}
		parts = append(parts, c.name+"="+c.val)
	}
	if known(v.ColorRange) {
		parts = append(parts, "range="+v.ColorRange)
	}
	return strings.Join(parts, ":")
}

func hasAudio(info mediaInfo) bool {
	for _, st := range info.Streams {
		if st.Type == "audio" {
			return true
		}
	}
	return false
}

func seconds6(d time.Duration) string { return strconv.FormatFloat(d.Seconds(), 'f', 6, 64) }

// av1Config reads the AV1 codec configuration (av1C) of a file's video,
// which holds its sequence header.
func av1Config(ctx context.Context, path string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_streams", "-show_data", path).Output()
	if err != nil {
		return nil, fmt.Errorf("reading the AV1 header: %w", err)
	}
	var data []byte
	in := false
	for line := range strings.Lines(string(out)) {
		switch {
		case strings.HasPrefix(line, "extradata="):
			in = true
		case in && len(line) > 10 && line[8] == ':':
			// "00000000: 8108 0c00 0a0e ...  ascii"
			hexPart, _, _ := strings.Cut(line[10:], "  ")
			b, err := hex.DecodeString(strings.ReplaceAll(hexPart, " ", ""))
			if err != nil {
				return nil, fmt.Errorf("reading the AV1 header: %w", err)
			}
			data = append(data, b...)
		case in:
			return data, nil
		}
	}
	if len(data) == 0 {
		return nil, errors.New("the video has no AV1 header")
	}
	return data, nil
}

// addPreroll writes main, with the preroll ahead of it, to out.
func addPreroll(ctx context.Context, s Settings, main string, mainInfo mediaInfo, out string, u updates) (time.Duration, error) {
	pre, length, err := prerollFor(ctx, s, main, mainInfo, u)
	if err != nil {
		return 0, err
	}
	id, _ := prerollID(s.Preroll)

	list, err := os.CreateTemp("", "cav1ar-concat-*.txt")
	if err != nil {
		return 0, err
	}
	defer os.Remove(list.Name())
	quote := func(p string) string { return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'" }
	fmt.Fprintf(list, "ffconcat version 1.0\nfile %s\nduration %s\nfile %s\n", quote(pre), seconds6(length), quote(main))
	if err := list.Close(); err != nil {
		return 0, err
	}

	// The preroll and main's video and audio are joined by the concat
	// demuxer; main's subtitles, attachments and chapters come from main
	// itself, shifted along by the preroll's length.
	args := []string{"-hide_banner", "-nostdin", "-nostats", "-y", "-v", "error",
		"-f", "concat", "-safe", "0", "-i", list.Name(), "-itsoffset", seconds6(length), "-i", main,
		"-map", "0:v:0"}
	count := map[string]int{}
	for _, st := range mainInfo.Streams {
		if !st.CoverArt {
			count[st.Type]++
		}
	}
	for i := range count["audio"] {
		args = append(args, "-map", "0:a:"+strconv.Itoa(i))
	}
	args = append(args, "-map", "1:s?", "-map", "1:t?", "-map_chapters", "1", "-map_metadata", "1", "-c", "copy")
	for _, k := range []struct{ name, spec string }{{"video", "v"}, {"audio", "a"}, {"subtitle", "s"}, {"attachment", "t"}} {
		for i := range count[k.name] {
			spec := fmt.Sprintf("s:%s:%d", k.spec, i)
			args = append(args, "-map_metadata:"+spec, "1:"+spec)
		}
	}
	// Video and audio come through the concat demuxer, which takes their
	// flags from the preroll, so put main's back.
	v, a := 0, 0
	for _, st := range mainInfo.Streams {
		switch {
		case st.Type == "video" && !st.CoverArt:
			args = append(args, fmt.Sprintf("-disposition:v:%d", v), dispositionOf(st))
			v++
		case st.Type == "audio":
			args = append(args, fmt.Sprintf("-disposition:a:%d", a), dispositionOf(st))
			a++
		}
	}
	args = append(args, "-metadata", prerollTag+"="+id+" "+seconds6(length),
		"-max_muxing_queue_size", "4096", "-f", "matroska", "-progress", "pipe:1", out)
	u.log("cav1ar: adding the preroll")
	if err := runFFmpeg(ctx, args, mainInfo.Duration+length, u); err != nil {
		return 0, err
	}
	return length, nil
}

func dispositionOf(st streamInfo) string {
	var flags []string
	if st.Default {
		flags = append(flags, "default")
	}
	if st.Forced {
		flags = append(flags, "forced")
	}
	if st.HearingImpaired {
		flags = append(flags, "hearing_impaired")
	}
	if len(flags) == 0 {
		return "0"
	}
	return strings.Join(flags, "+")
}

// checkJoin checks a file with a preroll against main, the file it was
// joined onto: the same tracks, each longer by the preroll's length, and
// video that decodes cleanly across the join.
func checkJoin(ctx context.Context, joined string, main mediaInfo, length time.Duration) error {
	info, err := probe(ctx, joined)
	if err != nil {
		return fmt.Errorf("it couldn't be read: %w", err)
	}
	count := func(m mediaInfo) map[string]int {
		c := map[string]int{}
		for _, st := range m.Streams {
			if !st.CoverArt {
				c[st.Type]++
			}
		}
		return c
	}
	if got, want := count(info), count(main); fmt.Sprint(got) != fmt.Sprint(want) {
		return fmt.Errorf("it has tracks %v, not %v", got, want)
	}
	want, got := longestPlayed(main.Streams)+length, longestPlayed(info.Streams)
	if want-got > tolerance(want) {
		return fmt.Errorf("it runs %s, not %s", got.Round(time.Second), want.Round(time.Second))
	}
	from := max(0, length-time.Second)
	b, err := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-nostdin", "-ss", seconds6(from), "-t", "3",
		"-i", joined, "-map", "0:v:0", "-f", "null", "-").CombinedOutput()
	if err != nil || len(bytes.TrimSpace(b)) > 0 {
		return fmt.Errorf("its video doesn't decode cleanly where the preroll meets it: %s", lastLine(string(b), err))
	}
	return nil
}
