package encode

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

// workDir holds the intermediates that prerolls are joined onto, on local
// storage.
func workDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "cav1ar", "work")
	return dir, os.MkdirAll(dir, 0o755)
}

// joinPoint finds where the video after a file's preroll starts: the first
// keyframe at about length in. Cutting there with stream copy keeps all of
// the video after it and none of the preroll. The keyframe usually lands a
// few milliseconds after length, since files start a little in to make room
// for audio priming.
func joinPoint(ctx context.Context, path string, length time.Duration) (time.Duration, error) {
	// Read from the start: seeking would land on the preroll's only keyframe.
	out, err := exec.CommandContext(ctx, FFprobe(), "-v", "error", "-select_streams", "v:0",
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

// prerollFor returns a copy of the preroll encoded to match main, which d
// encoded, building it the first time each kind of main needs one, and how
// long it runs. A GPU encodes the preroll for its own results, with the
// encoder and settings it gave main, on a worker if that's where it is, so
// the two share an AV1 header and players see one continuous video. SVT-AV1
// encodes it for the CPU's, here.
func prerollFor(ctx context.Context, s Settings, d Device, mainPath string, main mediaInfo, u updates) (string, time.Duration, error) {
	nvenc, vaapi := d.Kind == EncoderNVIDIA, d.Kind == EncoderVAAPI
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
	args := []string{"-hide_banner", "-nostdin", "-nostats", "-v", "error", "-y"}
	if vaapi {
		args = append(args, "-vaapi_device", d.Path)
	}
	args = append(args, "-i", s.Preroll)
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
	case nvenc && tenBit:
		vf = append(vf, "format=p010le")
	case vaapi && tenBit:
		vf = append(vf, "format=p010")
	case vaapi:
		vf = append(vf, "format=nv12")
	case tenBit:
		vf = append(vf, "format=yuv420p10le")
	default:
		vf = append(vf, "format=yuv420p")
	}
	params := colorParams(v)
	// Where the chroma is sited is in the AV1 header too, and encoders take
	// it from the frames, so give the preroll's main's.
	if len(config) > 2 {
		switch config[2] & 3 {
		case 1:
			params += ":chroma_location=left"
		case 2:
			params += ":chroma_location=topleft"
		}
	}
	vf = append(vf, "setparams="+params)
	if vaapi {
		vf = append(vf, "hwupload")
	}
	args = append(args, "-vf", strings.Join(vf, ","), "-frames:v", strconv.Itoa(frames))
	// Matching the level and tier keeps the sequence header the same as
	// main's.
	var levelTier []string
	if v.Level >= 0 && v.Level < 24 {
		levelTier = append(levelTier, "-level", fmt.Sprintf("%d.%d", 2+v.Level/4, v.Level%4))
	}
	if len(config) > 2 {
		levelTier = append(levelTier, "-tier", strconv.Itoa(int(config[2]>>7)))
	}
	switch {
	case nvenc:
		maxRate := nvencMaxRate(resolution(v.Width, v.Height))
		args = append(args, "-c:v", "av1_nvenc")
		if d.Index > 0 { // left out for the first GPU, which keeps prerolls built before there could be more
			args = append(args, "-gpu", strconv.Itoa(d.Index))
		}
		args = append(args, "-preset", "p"+strconv.Itoa(s.NVENCPreset), "-tune", "hq",
			"-rc", "vbr", "-cq", strconv.Itoa(s.CRF), "-b:v", "0",
			"-maxrate", strconv.Itoa(maxRate)+"M", "-bufsize", strconv.Itoa(2*maxRate)+"M",
			"-rc-lookahead", "32", "-spatial-aq", "1", "-temporal-aq", "1", "-g", "240")
		args = append(args, levelTier...)
	case vaapi:
		args = append(args, "-c:v", "av1_vaapi")
		args = append(args, vaapiRate(s.CRF, resolution(v.Width, v.Height))...)
		args = append(args, "-g", "240")
		if s.VAAPIPreset > 0 {
			args = append(args, "-compression_level:v", strconv.Itoa(s.VAAPIPreset))
		}
		args = append(args, levelTier...)
	default:
		args = append(args, "-c:v", "libsvtav1", "-preset", strconv.Itoa(s.Preset), "-crf", strconv.Itoa(svtCRF(s.CRF)))
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

	// Another build of ffmpeg, or of the encoder in it, may encode the same
	// preroll differently, so each has prerolls of its own.
	key := sha256.Sum256([]byte(strings.Join(append([]string{id, hex.EncodeToString(config), buildFor(d)}, args...), "\x00")))
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
	removeStaleBuilds(dir)
	// Jobs running at once may build the same one; each writes its own
	// file, held so it isn't taken for one left behind, and whichever
	// finishes last wins.
	tmp := filepath.Join(dir, buildPrefix+rand.Text()+".mkv")
	release, err := holdTemp(tmp)
	if err != nil {
		return "", 0, err
	}
	defer release()
	defer os.Remove(tmp)
	where := "the CPU"
	if nvenc || vaapi {
		where = d.label()
	}
	u.log(fmt.Sprintf("cav1ar: encoding the preroll to match %dx%d %s %s, on %s", v.Width, v.Height, v.FrameRate, v.PixFmt, where))
	if err := encodePreroll(ctx, s, d, args, tmp, length, u); err != nil {
		return "", 0, err
	}
	if got, err := av1Config(ctx, tmp); err == nil && !bytes.Equal(got, config) {
		u.log("cav1ar: the preroll's AV1 header differs from the video's, so some players may stutter where they meet")
	}
	if err := os.Rename(tmp, out); err != nil {
		return "", 0, err
	}
	return out, length, nil
}

// buildFor is the build of ffmpeg that encodes d's prerolls: a worker's own,
// for a GPU on one, as it says when it joins, or else this cav1ar's.
func buildFor(d Device) string {
	if r, _ := runnerFor(d); r != nil && d.Kind != EncoderCPU {
		if b, ok := r.(builder); ok {
			return b.FFmpegBuild()
		}
	}
	return FFmpegBuild()
}

// buildPrefix starts the names of prerolls being built.
const buildPrefix = ".build-"

// removeStaleBuilds removes the prerolls in dir that a cav1ar which stopped
// abruptly left half built: those no job holds, leaving any made in the last
// hour, which a job may be about to hold.
func removeStaleBuilds(dir string) {
	builds, _ := filepath.Glob(filepath.Join(dir, buildPrefix+"*.mkv"))
	for _, b := range builds {
		if fi, err := os.Lstat(b); err == nil && time.Since(fi.ModTime()) > time.Hour && !tempInUse(b) {
			os.Remove(b)
		}
	}
}

// encodePreroll runs ffmpeg with args, which read the preroll clip, to write
// out. On a worker's device, it runs there: the clip is copied to the folder
// the worker shares with this cav1ar, and the worker writes the preroll there
// too, for it to be copied here.
func encodePreroll(ctx context.Context, s Settings, d Device, args []string, out string, length time.Duration, u updates) error {
	runner, err := runnerFor(d)
	if err != nil {
		return err
	}
	if runner == nil || d.Kind == EncoderCPU { // the CPU's are built here
		if b, err := exec.CommandContext(ctx, FFmpeg, append(args, "-f", "matroska", out)...).CombinedOutput(); err != nil {
			return fmt.Errorf("encoding the preroll: %s", lastLine(string(b), err))
		}
		return nil
	}
	if s.WorkerDir == "" {
		return errors.New("encoding the preroll on a worker needs a folder it shares with this cav1ar; set worker_dir")
	}
	shared := func() (string, func(), error) {
		path := tempPath(filepath.Join(s.WorkerDir, "preroll"))
		release, err := holdTemp(path)
		if err != nil {
			return "", nil, err
		}
		return path, func() { release(); os.Remove(path) }, nil
	}
	clip, done, err := shared()
	if err != nil {
		return err
	}
	defer done()
	if err := copyInto(s.Preroll, clip); err != nil {
		return err
	}
	result, done, err := shared()
	if err != nil {
		return err
	}
	defer done()
	args = slices.Clone(args)
	if i := slices.Index(args, s.Preroll); i > 0 && args[i-1] == "-i" {
		args[i] = clip
	}
	quiet := u
	quiet.progress = func(progress) {} // it's the job's main pass that shows progress
	if err := runRemote(ctx, runner, append(args, "-f", "matroska", result), length, 0, quiet); err != nil {
		if errors.Is(err, ErrWorkerGone) || ctx.Err() != nil {
			return err
		}
		return fmt.Errorf("encoding the preroll: %w", err)
	}
	return copyOut(result, out)
}

// A worker can write to the folder it shares with this cav1ar, so it could
// swap a file there for a symlink to one of this cav1ar's, which reading
// would copy into a result, and writing would overwrite. So a file there is
// only read or written if it is a regular file, and not through a link.

// openShared opens path, in the folder shared with workers, with flag, if
// it is a regular file. O_NONBLOCK keeps a pipe put in its place from
// holding up the open, and makes no difference to a regular file.
func openShared(path string, flag int) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%s isn't a regular file", path)
	}
	f, err := os.OpenFile(path, flag|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if now, err := f.Stat(); err != nil || !now.Mode().IsRegular() || !os.SameFile(before, now) {
		f.Close()
		return nil, fmt.Errorf("%s changed as it was opened", path)
	}
	return f, nil
}

// regularShared checks that path, which may be in the folder shared with
// workers, is a regular file, not a link to somewhere else.
func regularShared(path string) error {
	fi, err := os.Lstat(path)
	if err == nil && !fi.Mode().IsRegular() {
		err = fmt.Errorf("%s isn't a regular file", path)
	}
	return err
}

// copyInto copies from to to, a file this cav1ar made in the folder shared
// with workers, which it empties first.
func copyInto(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	w, err := openShared(to, os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return err
	}
	return copyFile(w, in)
}

// copyOut copies from, a file a worker wrote in the folder it shares with
// this cav1ar, to to, which it creates or empties.
func copyOut(from, to string) error {
	in, err := openShared(from, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer in.Close()
	w, err := os.Create(to)
	if err != nil {
		return err
	}
	return copyFile(w, in)
}

// copyFile copies in to w, then closes w.
func copyFile(w, in *os.File) error {
	if _, err := io.Copy(w, in); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

// unmatchedAudio is the codec of the first audio track p keeps as it was
// that a preroll can't be encoded to match, if there is one. Tracks p
// encodes are in codecs a preroll can match.
func unmatchedAudio(p *plan) string {
	for _, a := range p.audio {
		if _, ok := audioEncoders[a.Codec]; a.copy && !ok {
			return a.Codec
		}
	}
	return ""
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
	out, err := exec.CommandContext(ctx, FFprobe(), "-v", "error", "-select_streams", "v:0",
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

// joinPreroll puts the result together in out with the preroll ahead of it,
// in one pass: the encoding pass's video and audio from av, or its video and
// the audio pass's audio, joined onto the preroll, and the subtitles and
// attachments from side, moved along by the preroll's length. It returns
// that length. intermediate picks a file for the passes' work.
func joinPreroll(ctx context.Context, p *plan, out, av, audio, side string,
	intermediate func() (string, func(), error), u updates) (time.Duration, error) {
	run := func(args []string, dur time.Duration) error {
		u.log("cav1ar: ffmpeg " + strings.Join(args, " "))
		return runFFmpeg(ctx, args, dur, frameRate(p.video), u)
	}
	// A worker may have written the passes' files; see openShared.
	for _, f := range []string{av, audio, side} {
		if f == "" {
			continue
		}
		if err := regularShared(f); err != nil {
			return 0, err
		}
	}
	// The preroll's audio is encoded to match each audio track the result
	// keeps, which can't be done for some codecs kept as they were. Rather
	// than fail the job, leave the preroll off this file.
	if codec := unmatchedAudio(p); codec != "" {
		u.log("cav1ar: leaving the preroll off: its audio can't be encoded to match the " + codec + " audio kept from the source")
		u.note("preroll skipped")
		return 0, run(p.remuxArgs(out, av, audio, side), p.info.Duration)
	}
	main := av
	if audio != "" { // the concat demuxer joins whole files, so make one of the two
		merged, done, err := intermediate()
		if err != nil {
			return 0, err
		}
		defer done()
		if err := run(p.mergeArgs(merged, av, audio), p.info.Duration); err != nil {
			return 0, err
		}
		main = merged
	}
	mainInfo, err := probe(ctx, main)
	if err != nil {
		return 0, err
	}
	pre, length, err := prerollFor(ctx, p.settings, p.device, main, mainInfo, u)
	if err != nil {
		return 0, err
	}
	if err := seamFits(ctx, pre, main, length); err != nil {
		return 0, err
	}
	id, _ := prerollID(p.settings.Preroll)

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
	return length, run(p.joinArgs(out, list.Name(), main, side, length, id, mainInfo), mainInfo.Duration+length)
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

// joinDecodes checks that a result's video decodes cleanly across the join
// with the preroll, which runs for length.
func joinDecodes(ctx context.Context, joined string, length time.Duration) error {
	from := max(0, length-time.Second)
	b, err := exec.CommandContext(ctx, FFmpeg, "-v", "error", "-nostdin", "-ss", seconds6(from), "-t", "3",
		"-i", joined, "-map", "0:v:0", "-f", "null", "-").CombinedOutput()
	// Some sources give frames in pairs with the same timestamp, which
	// players take in their stride but the null muxer complains about.
	// They'd complain without a preroll too, so they're not counted, and
	// timestamps across the join are checked separately by seamFits.
	var complaints []string
	for line := range strings.Lines(string(b)) {
		if line = strings.TrimSpace(line); line != "" && !strings.Contains(line, "non monotonically increasing dts") {
			complaints = append(complaints, line)
		}
	}
	if err != nil || len(complaints) > 0 {
		return fmt.Errorf("its video doesn't decode cleanly where the preroll meets it: %s",
			lastLine(strings.Join(complaints, "\n"), err))
	}
	return nil
}

// seamFits checks that the preroll's video ends by length, where main's is
// put after it, and that main's starts no earlier than its own zero, so the
// two can't overlap. Once joined, an overlap is hard to see: the muxer turns
// timestamps that go back into a run of equal ones.
func seamFits(ctx context.Context, pre, main string, length time.Duration) error {
	packets := func(path string, intervals ...string) ([]string, error) {
		args := append([]string{"-v", "error", "-select_streams", "v:0"}, intervals...)
		out, err := exec.CommandContext(ctx, FFprobe(), append(args,
			"-show_entries", "packet=dts_time,pts_time,duration_time", "-of", "csv=p=0", path)...).Output()
		if err != nil {
			return nil, err
		}
		var lines []string
		for line := range strings.Lines(string(out)) {
			if line = strings.TrimSpace(line); line != "" {
				lines = append(lines, line)
			}
		}
		return lines, nil
	}
	const slack = time.Millisecond // timestamps are in whole milliseconds
	preEnd := time.Duration(0)
	lines, err := packets(pre)
	if err != nil {
		return fmt.Errorf("reading the preroll's timestamps: %w", err)
	}
	for _, line := range lines {
		f := strings.Split(line, ",")
		if len(f) >= 3 {
			preEnd = max(preEnd, seconds(f[0])+seconds(f[2]))
		}
	}
	if preEnd > length+slack {
		return fmt.Errorf("the preroll runs to %s, past %s where the video is put", seconds6(preEnd), seconds6(length))
	}
	if lines, err = packets(main, "-read_intervals", "%+#1"); err != nil {
		return fmt.Errorf("reading the video's timestamps: %w", err)
	}
	// ffprobe gives pts, dts and duration, in that order.
	if len(lines) > 0 {
		f := strings.Split(lines[0], ",")
		if len(f) < 2 {
			return nil
		}
		if dts, err := strconv.ParseFloat(f[1], 64); err == nil && time.Duration(dts*float64(time.Second)) < -slack {
			return fmt.Errorf("the video starts at %ss, so it would overlap the end of the preroll", f[1])
		}
	}
	return nil
}
