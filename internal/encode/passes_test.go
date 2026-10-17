package encode

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// makeSource writes a 3 second video with an audio track, an English
// subtitle track and two chapters, as a source to encode.
func makeSource(t *testing.T, dir string) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("needs ffmpeg")
	}
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	srt := write("subs.srt", "1\n00:00:00,500 --> 00:00:01,500\nHello there.\n\n2\n00:00:02,000 --> 00:00:02,800\nGoodbye.\n")
	meta := write("meta.txt", ";FFMETADATA1\n"+
		"[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=1500\ntitle=One\n"+
		"[CHAPTER]\nTIMEBASE=1/1000\nSTART=1500\nEND=3000\ntitle=Two\n")
	src := filepath.Join(dir, "Test Movie (2020).mkv")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc=size=320x180:rate=24:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3:sample_rate=48000", "-i", srt, "-i", meta,
		"-map", "0:v", "-map", "1:a", "-map", "2:s", "-map_chapters", "3",
		"-c:v", "mpeg4", "-c:a", "aac", "-ac", "2", "-c:s", "srt",
		"-metadata:s:a:0", "language=eng", "-metadata:s:s:0", "language=eng", src)
	return src
}

func ffmpeg(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("ffmpeg", append([]string{"-v", "error", "-nostdin", "-y"}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
}

func testSettings(out string) Settings {
	s := DefaultSettings()
	s.Preset = 12 // fastest, for a test clip
	s.AudioKbps = AudioAuto
	s.OutDir = out
	s.ReleaseGroup = "TEST"
	return s
}

func noUpdates() updates {
	return updates{
		planned: func(Action, string) {}, started: func(Process) {}, progress: func(progress) {},
		log: func(string) {}, note: func(string) {}, step: func(string) {},
	}
}

func count(info mediaInfo, kind string) int {
	n := 0
	for _, st := range info.Streams {
		if st.Type == kind && !st.CoverArt {
			n++
		}
	}
	return n
}

func streamOf(info mediaInfo, kind string) streamInfo {
	for _, st := range info.Streams {
		if st.Type == kind && !st.CoverArt {
			return st
		}
	}
	return streamInfo{}
}

func TestEncodeFileKeepsEveryTrackThroughBothPasses(t *testing.T) {
	if err := CheckEncoder(EncoderCPU); err != nil {
		t.Skip(err)
	}
	dir, outDir, cache := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache) // where the passes' files go
	src := makeSource(t, dir)
	s := testSettings(outDir)
	ctx := context.Background()
	p, err := prepare(ctx, src, s, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if err := encodeFile(ctx, src, s, cpuDevice, noUpdates()); err != nil {
		t.Fatal(err)
	}
	info, err := probe(ctx, p.out)
	if err != nil {
		t.Fatal(err)
	}
	if n := count(info, "video"); n != 1 || streamOf(info, "video").Codec != "av1" {
		t.Errorf("want 1 AV1 video track, got %d (%s)", n, streamOf(info, "video").Codec)
	}
	if n := count(info, "audio"); n != 1 || streamOf(info, "audio").Codec != "opus" {
		t.Errorf("want 1 Opus audio track, got %d (%s)", n, streamOf(info, "audio").Codec)
	}
	if sub := streamOf(info, "subtitle"); count(info, "subtitle") != 1 || sub.Language != "eng" {
		t.Errorf("want 1 English subtitle track, got %d (%q)", count(info, "subtitle"), sub.Language)
	}
	if info.Chapters != 2 {
		t.Errorf("want the source's 2 chapters, got %d", info.Chapters)
	}
	if got := streamOf(info, "audio").Duration; got < 2900*time.Millisecond {
		t.Errorf("the audio stops at %s, but the source runs 3s", got)
	}
	if got := info.Tags["ENCODED_BY"]; got != "TEST" {
		t.Errorf("ENCODED_BY is %q, want the release group", got)
	}
	work, _ := workDir()
	if left, _ := filepath.Glob(filepath.Join(work, "*")); len(left) > 0 {
		t.Errorf("the passes left files behind: %v", left)
	}
}

// When the encoding pass cuts the audio short, the remuxing pass takes the
// audio from the pass that encoded it again.
func TestRemuxTakesTheAudioFromItsOwnPass(t *testing.T) {
	dir := t.TempDir()
	src := makeSource(t, dir)
	ctx := context.Background()
	p, err := prepare(ctx, src, testSettings(dir), func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	av, audio, side, out := filepath.Join(dir, "av.mkv"), filepath.Join(dir, "audio.mka"),
		filepath.Join(dir, "side.mkv"), filepath.Join(dir, "out.mkv")
	// An encoding pass whose audio stopped after a second.
	ffmpeg(t, "-i", src, "-map", "0:v", "-map", "0:a", "-c:v", "copy", "-c:a", "libopus", "-af", "atrim=0:1", av)
	ffmpeg(t, "-i", src, "-map", "0:s", "-c", "copy", side)
	ffmpeg(t, p.audioArgs(audio)...)

	info, err := probe(ctx, av)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkStreamLengths(info, p); err == nil {
		t.Fatal("the cut-short audio passed its check")
	} else if short, ok := err.(*shortStream); !ok || short.kind != "audio" {
		t.Fatalf("want the audio reported short, got %v", err)
	}

	ffmpeg(t, p.remuxArgs(out, av, audio, side)...)
	got, err := probe(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	if d := streamOf(got, "audio").Duration; d < 2900*time.Millisecond {
		t.Errorf("the remux's audio stops at %s, so it didn't come from the audio pass", d)
	}
	if count(got, "subtitle") != 1 {
		t.Errorf("want the subtitle track from the side file, got %d", count(got, "subtitle"))
	}
}

// A job's log marks the start of each of its stages, in the order it goes
// through them.
func TestEncodeFileLogsItsStages(t *testing.T) {
	if err := CheckEncoder(EncoderCPU); err != nil {
		t.Skip(err)
	}
	dir, outDir := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	src := makeSource(t, dir)
	var stages []string
	u := noUpdates()
	u.log = func(line string) {
		if stage, ok := Stage(line); ok {
			stages = append(stages, stage)
		}
	}
	if err := encodeFile(context.Background(), src, testSettings(outDir), cpuDevice, u); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"checking the file",
		"encoding the video and audio on the CPU",
		"putting the video, audio and subtitles together",
		"checking the result",
		"moving the result into place",
	}
	if !slices.Equal(stages, want) {
		t.Errorf("stages logged:\n  %q\nwant:\n  %q", stages, want)
	}
}

// With a preroll, the result is put together in one pass with the preroll
// ahead of it: every track kept, the subtitles and chapters moved along by
// its length, the file tagged with it, and the whole checked once.
func TestEncodeFileJoinsThePrerollInOnePass(t *testing.T) {
	if err := CheckEncoder(EncoderCPU); err != nil {
		t.Skip(err)
	}
	for _, strip := range []bool{false, true} {
		t.Run(fmt.Sprint("strip=", strip), func(t *testing.T) { testPrerollJoin(t, strip) })
	}
}

func testPrerollJoin(t *testing.T, strip bool) {
	dir, outDir := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	src := makeSource(t, dir)
	intro := filepath.Join(dir, "intro.mkv")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=880:duration=2:sample_rate=48000", "-c:v", "mpeg4", "-c:a", "aac", intro)
	s := testSettings(outDir)
	s.Preroll, s.StripMetadata = intro, strip
	var stages []string
	u := noUpdates()
	u.log = func(line string) {
		if stage, ok := Stage(line); ok {
			stages = append(stages, stage)
		}
	}
	ctx := context.Background()
	p, err := prepare(ctx, src, s, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if err := encodeFile(ctx, src, s, cpuDevice, u); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"checking the file",
		"encoding the video and audio on the CPU",
		"putting the preroll, video, audio and subtitles together",
		"checking the result",
		"moving the result into place",
	}
	if !slices.Equal(stages, want) {
		t.Errorf("stages logged:\n  %q\nwant:\n  %q", stages, want)
	}

	info, err := probe(ctx, p.out)
	if err != nil {
		t.Fatal(err)
	}
	chapters := 2 // stripping drops them
	if strip {
		chapters = 0
	}
	if count(info, "video") != 1 || count(info, "audio") != 1 || count(info, "subtitle") != 1 || info.Chapters != chapters {
		t.Errorf("want 1 video, 1 audio and 1 subtitle track and %d chapters, got %d, %d, %d and %d",
			chapters, count(info, "video"), count(info, "audio"), count(info, "subtitle"), info.Chapters)
	}
	if v := streamOf(info, "video"); v.Codec != "av1" || v.Duration < 4900*time.Millisecond {
		t.Errorf("the video is %s and runs %s, want AV1 running 5s", v.Codec, v.Duration)
	}
	if a := streamOf(info, "audio").Duration; a < 4900*time.Millisecond {
		t.Errorf("the audio runs %s, want 5s", a)
	}
	if id, length := prerollOf(info); id == "" || length.Round(time.Second) != 2*time.Second {
		t.Errorf("the preroll tag says %q for %s, want a 2s preroll", id, length)
	}
	if a := streamOf(info, "audio"); a.Language != "eng" {
		t.Errorf("the audio's language is %q, want eng", a.Language)
	}
	if sub := streamOf(info, "subtitle"); sub.Language != "eng" {
		t.Errorf("the subtitle's language is %q, want eng", sub.Language)
	}
	first := func(args ...string) float64 { // the first value ffprobe gives
		out, err := exec.Command("ffprobe", append([]string{"-v", "error", "-of", "csv=p=0"}, args...)...).Output()
		if err != nil {
			t.Fatal(err)
		}
		v, _ := strconv.ParseFloat(strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]), 64)
		return v
	}
	if at := first("-show_entries", "chapter=start_time", p.out); !strip && at < 1.9 {
		t.Errorf("the first chapter starts at %.2fs, want it moved past the 2s preroll", at)
	}
	if at := first("-select_streams", "s:0", "-show_entries", "packet=pts_time", p.out); at < 2.4 {
		t.Errorf("the first subtitle shows at %.2fs, want 2.5s, after the preroll", at)
	}
	work, _ := workDir()
	if left, _ := filepath.Glob(filepath.Join(work, "*")); len(left) > 0 {
		t.Errorf("the passes left files behind: %v", left)
	}
}

// fakeWorker stands in for a worker, running the passes it's given here and
// noting each one.
type fakeWorker struct{ ran [][]string }

func (w *fakeWorker) Run(ctx context.Context, args []string, o Output) error {
	w.ran = append(w.ran, args)
	var stderr strings.Builder
	cmd := exec.CommandContext(ctx, FFmpeg, args...)
	cmd.Stderr = &stderr
	err := cmd.Run()
	for line := range strings.Lines(stderr.String()) {
		o.Stderr(strings.TrimSpace(line))
	}
	return err
}

// A GPU on a worker encodes the preroll for its own results, there, reading
// the clip from the folder it shares with the daemon.
func TestAWorkersGPUEncodesItsPreroll(t *testing.T) {
	if err := CheckEncoder(EncoderNVIDIA); err != nil {
		t.Skip(err)
	}
	dir, outDir, shared := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	src := makeSource(t, dir)
	intro := filepath.Join(dir, "intro.mkv")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=880:duration=2:sample_rate=48000", "-c:v", "mpeg4", "-c:a", "aac", intro)
	s := testSettings(outDir)
	s.Preroll, s.WorkerDir = intro, shared
	w := &fakeWorker{}
	AddWorker("w", w)
	defer RemoveWorker("w")
	gpu := Device{ID: "w/nvidia:x", Kind: EncoderNVIDIA, Name: "Test GPU (w)", Worker: "w"}
	var logged []string
	u := noUpdates()
	u.log = func(line string) { logged = append(logged, line) }
	if err := encodeFile(context.Background(), src, s, gpu, u); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(logged, "\n"))
	}
	var built []string
	for _, args := range w.ran {
		if slices.Contains(args, "-frames:v") { // the preroll's pass, which stops at its length
			built = args
		}
	}
	if built == nil {
		t.Fatalf("the worker never encoded the preroll; it ran %q", w.ran)
	}
	if slices.Contains(built, intro) {
		t.Error("the worker was given the clip's own path, not the shared folder's copy")
	}
	if !slices.ContainsFunc(logged, func(l string) bool { return strings.Contains(l, "encoding the preroll to match") && strings.HasSuffix(l, "on Test GPU (w)") }) {
		t.Errorf("the log doesn't say the worker's GPU encoded the preroll:\n%s", strings.Join(logged, "\n"))
	}
	if left, _ := os.ReadDir(shared); len(left) > 0 {
		t.Errorf("files were left in the shared folder: %v", left)
	}
}
