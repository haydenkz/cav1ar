package encode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
		planned: func(Action, string) {}, started: func(*os.Process) {}, progress: func(progress) {},
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
		"encoding the video and audio",
		"putting the video, audio and subtitles together",
		"checking the result",
		"moving the result into place",
	}
	if !slices.Equal(stages, want) {
		t.Errorf("stages logged:\n  %q\nwant:\n  %q", stages, want)
	}
}
