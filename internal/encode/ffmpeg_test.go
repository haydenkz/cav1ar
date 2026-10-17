package encode

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// What ffmpeg writes when the disk fills up partway through a pass: the
// cause, then lines that don't say what went wrong.
const diskFull = `[out#0/matroska @ 0x55f994685c80] Task finished with error code: -28 (No space left on device)
[out#0/matroska @ 0x55f994685c80] Terminating thread with return code -28 (No space left on device)
[out#0/matroska @ 0x55f994685c80] Error writing trailer: No space left on device
[out#0/matroska @ 0x55f994685c80] video:312KiB audio:52KiB subtitle:0KiB other streams:0KiB global headers:0KiB muxing overhead: unknown
frame=  145 fps=0.0 q=2.0 Lsize=     320KiB time=00:00:06.17 bitrate= 424.1kbits/s speed=61.5x elapsed=0:00:00.10
[aac @ 0x55f99468d040] Qavg: 119.427
Conversion failed!`

func TestFFmpegFailureSkipsWhatFollowsTheCause(t *testing.T) {
	for _, c := range []struct{ out, want string }{
		{diskFull, "[out#0/matroska] Error writing trailer: No space left on device"},
		{"Error opening input file /nowhere.mkv.\nError opening input files: No such file or directory",
			"Error opening input files: No such file or directory"},
		{"[libsvtav1 @ 0x1] something odd\nConversion failed!", "[libsvtav1] something odd"},
		{"Conversion failed!", ""},
		{"[aac @ 0x1] " + strings.Repeat("error ", 100), "[aac] " + strings.Repeat("error ", 48) + "error…"},
	} {
		var f ffmpegFailure
		for line := range strings.Lines(c.out) {
			f.add(line)
		}
		if got := f.reason(); got != c.want {
			t.Errorf("for\n%s\ngot %q, want %q", c.out, got, c.want)
		}
	}
}

func TestEachLineCarriesOnPastALongLine(t *testing.T) {
	long := strings.Repeat("x", 3*maxLine)
	var got []string
	eachLine(strings.NewReader("first\r\n"+long+"\n\nnext\nlast"), func(line string) { got = append(got, line) })
	want := []string{"first", long[:maxLine], "", "next", "last"}
	if !slices.Equal(got, want) {
		t.Errorf("got %d lines, want %d: %.40q", len(got), len(want), got)
	}
}

// A failing ffmpeg is reported by the cause of its failure, and a line too
// long to read whole doesn't stop the rest of its output being read, which
// would leave it waiting for that forever.
func TestRunFFmpegReportsTheCause(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "out.txt"), []byte(diskFull+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\n" +
		"head -c 300000 /dev/zero | tr '\\0' x >&2\n" + // one long line, then plenty more to fill the pipe
		"i=0; while [ $i -lt 2000 ]; do echo \"[matroska @ 0x1] line $i of the output that follows\" >&2; i=$((i+1)); done\n" +
		"cat " + filepath.Join(dir, "out.txt") + " >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := FFmpeg
	FFmpeg = fake
	t.Cleanup(func() { FFmpeg = old })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := runFFmpeg(ctx, nil, 0, 0, noUpdates())
	if ctx.Err() != nil {
		t.Fatal("ffmpeg was left waiting for its output to be read")
	}
	if want := "[out#0/matroska] Error writing trailer: No space left on device"; err == nil || err.Error() != want {
		t.Errorf("got %v, want %q", err, want)
	}
}

func TestFFprobeGoesWithFFmpeg(t *testing.T) {
	old := FFmpeg
	t.Cleanup(func() { FFmpeg = old })
	for _, c := range []struct{ ffmpeg, want string }{
		{"ffmpeg", "ffprobe"},
		{"/opt/example/bin/ffmpeg", "/opt/example/bin/ffprobe"},
	} {
		FFmpeg = c.ffmpeg
		if got := FFprobe(); got != c.want {
			t.Errorf("with ffmpeg %q, got ffprobe %q, want %q", c.ffmpeg, got, c.want)
		}
	}
}

func TestExistsOnlyWhenThere(t *testing.T) {
	dir := t.TempDir()
	if !exists(dir) {
		t.Error("a folder that's there doesn't exist")
	}
	if exists(filepath.Join(dir, "missing.mkv")) {
		t.Error("a missing file exists")
	}
	if exists(filepath.Join(dir, strings.Repeat("x", 300)+".mkv")) {
		t.Error("a name too long to look up exists")
	}
}

// A second job planning a result another is writing fails at the start.
func TestReserveRefusesASecondJob(t *testing.T) {
	out := filepath.Join(t.TempDir(), "Example Movie (2020).mkv")
	release, err := reserve(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reserve(out); err == nil || !strings.Contains(err.Error(), "already writing") {
		t.Errorf("a second job got the same result: %v", err)
	}
	release()
	again, err := reserve(out)
	if err != nil {
		t.Fatalf("once the first job ended: %v", err)
	}
	again()
}

// With originals kept, a file whose only fault is its name is left as it
// is, with a note saying why; with them deleted, it's renamed.
func TestKeptOriginalsAreNotRenamed(t *testing.T) {
	if err := CheckEncoder(EncoderCPU); err != nil {
		t.Skip(err)
	}
	dir, outDir := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ctx := context.Background()
	first := testSettings(outDir)
	p, err := prepare(ctx, makeSource(t, dir), first, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if err := encodeFile(ctx, p.in, first, cpuDevice, noUpdates()); err != nil {
		t.Fatal(err)
	}
	encoded := p.out

	kept := first
	kept.NameTemplate = "{title} ({year}) [AV1]"
	var note string
	var action Action
	u := noUpdates()
	u.note = func(n string) { note = n }
	u.planned = func(a Action, out string) {
		action = a
		if out != encoded {
			t.Errorf("planned to put the result at %s", out)
		}
	}
	if err := encodeFile(ctx, encoded, kept, cpuDevice, u); err != nil {
		t.Fatal(err)
	}
	if action != ActionNone || note == "" {
		t.Errorf("want nothing done and a note, got %s and %q", action, note)
	}
	if !exists(encoded) || exists(filepath.Join(outDir, "Test Movie (2020) [AV1].mkv")) {
		t.Error("the original was renamed")
	}

	deleted := kept
	deleted.DeleteOriginal = true
	if err := encodeFile(ctx, encoded, deleted, cpuDevice, noUpdates()); err != nil {
		t.Fatal(err)
	}
	if exists(encoded) || !exists(filepath.Join(outDir, "Test Movie (2020) [AV1].mkv")) {
		t.Error("with originals deleted, the file wasn't renamed")
	}
}

// A source that something else changes while it's encoded is neither
// replaced nor deleted.
func TestAChangedOriginalIsKept(t *testing.T) {
	if err := CheckEncoder(EncoderCPU); err != nil {
		t.Skip(err)
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	for _, replace := range []bool{true, false} {
		dir := t.TempDir()
		src := makeSource(t, dir)
		s := testSettings(t.TempDir())
		s.DeleteOriginal = true
		if replace {
			s.OutDir, s.NameTemplate = "", "{name}"
		}
		var note string
		u := noUpdates()
		u.note = func(n string) { note = n }
		u.log = func(line string) {
			if stage, _ := Stage(line); stage == "checking the result" {
				later := time.Now().Add(time.Hour)
				if err := os.Chtimes(src, later, later); err != nil {
					t.Error(err)
				}
			}
		}
		err := encodeFile(context.Background(), src, s, cpuDevice, u)
		info, perr := probe(context.Background(), src)
		switch {
		case perr != nil:
			t.Fatalf("replacing %v: the original is gone: %v", replace, perr)
		case streamOf(info, "video").Codec == "av1":
			t.Errorf("replacing %v: the original was replaced", replace)
		case replace && (err == nil || !strings.Contains(err.Error(), "changed")):
			t.Errorf("replacing the original: want an error saying it changed, got %v", err)
		case !replace && (err != nil || note != "original changed, not deleted"):
			t.Errorf("beside the original: want the result kept with a note, got %v and %q", err, note)
		}
	}
}
