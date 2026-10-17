package encode

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSeamFitsCatchesAnOverlap(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("needs ffmpeg")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("ffmpeg", append([]string{"-v", "error", "-nostdin", "-y"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg: %v: %s", err, out)
		}
	}
	clip := func(name string, secs int) string {
		path := filepath.Join(dir, name)
		run("-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=160x90:rate=24:duration=%d", secs), "-c:v", "mpeg4", path)
		return path
	}
	pre, main := clip("pre.mkv", 2), clip("main.mkv", 3)
	ctx := context.Background()
	if err := seamFits(ctx, pre, main, 2*time.Second); err != nil {
		t.Errorf("a preroll that ends where the video starts failed: %v", err)
	}
	// Putting the video half a second early would overlap the two.
	if err := seamFits(ctx, pre, main, 1500*time.Millisecond); err == nil {
		t.Error("an overlapping join passed")
	}
}

// Audio kept as it was in a codec the preroll can't be encoded in, such as
// PCM, leaves the preroll off that file, rather than failing its job.
func TestAPrerollIsLeftOffAudioItCantMatch(t *testing.T) {
	if err := CheckEncoder(EncoderCPU); err != nil {
		t.Skip(err)
	}
	dir, outDir := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	src := filepath.Join(dir, "Test Movie (2020).mkv")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc=size=320x180:rate=24:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3:sample_rate=48000",
		"-c:v", "mpeg4", "-c:a", "pcm_s16le", "-metadata:s:a:0", "language=eng", src)
	intro := filepath.Join(dir, "intro.mkv")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=880:duration=2:sample_rate=48000", "-c:v", "mpeg4", "-c:a", "aac", intro)
	s := testSettings(outDir)
	s.Preroll, s.AudioKbps = intro, AudioCopy
	var logged []string
	note := ""
	u := noUpdates()
	u.log = func(line string) { logged = append(logged, line) }
	u.note = func(n string) { note = n }
	ctx := context.Background()
	p, err := prepare(ctx, src, s, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if err := encodeFile(ctx, src, s, cpuDevice, u); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(logged, "\n"))
	}
	info, err := probe(ctx, p.out)
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := prerollOf(info); id != "" {
		t.Errorf("the result is tagged with preroll %s", id)
	}
	if v := streamOf(info, "video"); v.Codec != "av1" || v.Duration > 3100*time.Millisecond {
		t.Errorf("the video is %s and runs %s, want AV1 running 3s, with no preroll", v.Codec, v.Duration)
	}
	if a := streamOf(info, "audio"); a.Codec != "pcm_s16le" {
		t.Errorf("the audio is %s, want it kept as PCM", a.Codec)
	}
	if note != "preroll skipped" {
		t.Errorf("the job's note is %q", note)
	}
	if !slices.ContainsFunc(logged, func(l string) bool { return strings.Contains(l, "leaving the preroll off") }) {
		t.Errorf("the log doesn't say why the preroll was left off:\n%s", strings.Join(logged, "\n"))
	}
}

func TestFFprobeGoesWithFFmpeg(t *testing.T) {
	old := FFmpeg
	defer func() { FFmpeg = old }()
	for ff, want := range map[string]string{"ffmpeg": "ffprobe", "/opt/ffmpeg/bin/ffmpeg": "/opt/ffmpeg/bin/ffprobe"} {
		FFmpeg = ff
		if got := ffprobePath(); got != want {
			t.Errorf("with %s, got %s, want %s", ff, got, want)
		}
	}
}

// builtRunner is a worker whose ffmpeg is the build given.
type builtRunner string

func (builtRunner) Run(context.Context, []string, Output) error { return nil }
func (b builtRunner) FFmpegBuild() string                       { return string(b) }

// A preroll is built again for each build of ffmpeg, a worker's GPU's with
// the worker's.
func TestPrerollsAreBuiltForEachFFmpeg(t *testing.T) {
	AddWorker("w", builtRunner("ffmpeg version on the worker"))
	defer RemoveWorker("w")
	if got := buildFor(Device{ID: "w/nvidia:0", Kind: EncoderNVIDIA, Worker: "w"}); got != "ffmpeg version on the worker" {
		t.Errorf("a worker's GPU's prerolls are built with %q", got)
	}
	if got := buildFor(cpuDevice); got != FFmpegBuild() {
		t.Errorf("the CPU's prerolls are built with %q, not this cav1ar's ffmpeg", got)
	}
}

func TestHalfBuiltPrerollsAreRemoved(t *testing.T) {
	dir := t.TempDir()
	build := func(name string, age time.Duration) string {
		path := filepath.Join(dir, buildPrefix+name+".mkv")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		then := time.Now().Add(-age)
		os.Chtimes(path, then, then)
		return path
	}
	stale, fresh, held := build("stale", 2*time.Hour), build("fresh", time.Minute), build("held", 2*time.Hour)
	release, err := holdTemp(held)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	removeStaleBuilds(dir)
	if exists(stale) {
		t.Error("a build left behind was kept")
	}
	if !exists(fresh) || !exists(held) {
		t.Error("a build that may be under way was removed")
	}
}

// Files in the folder shared with workers are only read or written when
// they are regular files, so a worker can't swap in a link to one of the
// daemon's.
func TestSharedFilesMustBeRegular(t *testing.T) {
	dir := t.TempDir()
	private := filepath.Join(dir, "private")
	if err := os.WriteFile(private, []byte("not for workers"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(dir, "local")
	if err := copyOut(link, local); err == nil {
		t.Error("a link to another file was copied out of the shared folder")
	}
	if err := copyInto(local, link); err == nil {
		t.Error("a link to another file was written through")
	}
	if b, _ := os.ReadFile(private); string(b) != "not for workers" {
		t.Errorf("the file the link led to was changed to %q", b)
	}
	if err := regularShared(link); err == nil {
		t.Error("a link was taken for a pass's file")
	}

	shared := filepath.Join(dir, "shared")
	os.WriteFile(shared, nil, 0o644)
	if err := copyInto(private, shared); err != nil {
		t.Fatal(err)
	}
	if err := copyOut(shared, local); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(local); string(b) != "not for workers" {
		t.Errorf("copied %q", b)
	}
}
