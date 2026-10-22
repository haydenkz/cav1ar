package encode

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestClipCuesEndsACueAtTheLength(t *testing.T) {
	got := clipCues(2*time.Hour + 18*time.Minute + 20*time.Second + 500*time.Millisecond)
	want := "setts=duration='if(gt(PTS+DURATION,8300.500000/TB),max(0,8300.500000/TB-PTS),DURATION)'"
	if got != want {
		t.Errorf("clipCues = %s, want %s", got, want)
	}
}

func TestTrimSideArgs(t *testing.T) {
	length := 90*time.Second + 500*time.Millisecond
	args := (&plan{}).trimSideArgs("/work/trimmed.mkv", "/work/side.mkv", length)
	for _, want := range [][2]string{
		{"-i", "/work/side.mkv"}, {"-map", "0"}, {"-c", "copy"}, {"-bsf:s", clipCues(length)},
		{"-t", "90.500000"}, {"-map_chapters", "-1"}, {"-f", "matroska"},
	} {
		if !hasArgs(args, want[0], want[1]) {
			t.Errorf("no %s %s in %q", want[0], want[1], args)
		}
	}
	if !slices.Contains(args, "-copyts") {
		t.Errorf("no -copyts in %q, so the cues would move", args)
	}
	if args[len(args)-1] != "/work/trimmed.mkv" {
		t.Errorf("%q doesn't end with the file to write", args)
	}
	// -t limits subtitles only as an output option.
	if slices.Index(args, "-t") < slices.Index(args, "-i") {
		t.Errorf("-t comes before the input in %q, where it doesn't limit the subtitles", args)
	}
}

// overrunSubs are subtitles for a video 3 seconds long: a cue inside it, one
// that runs an hour past its end, and one that starts well after it.
const overrunSubs = "1\n00:00:00,500 --> 00:00:01,500\nHello there.\n\n" +
	"2\n00:00:02,000 --> 01:00:02,000\nThis cue never ends.\n\n" +
	"3\n00:13:00,000 --> 00:13:01,000\nA stray marker.\n"

// makeOverrunSource writes a 3 second video with an audio track and
// overrunSubs.
func makeOverrunSource(t *testing.T, dir string) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("needs ffmpeg")
	}
	srt := filepath.Join(dir, "overrun.srt")
	if err := os.WriteFile(srt, []byte(overrunSubs), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "Long Subtitles (2020).mkv")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc=size=320x180:rate=24:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3:sample_rate=48000", "-i", srt,
		"-map", "0:v", "-map", "1:a", "-map", "2:s", "-c:v", "mpeg4", "-c:a", "aac", "-ac", "2", "-c:s", "srt",
		"-metadata:s:a:0", "language=eng", "-metadata:s:s:0", "language=eng", src)
	return src
}

// cuesOf lists the cues of the first subtitle track in path, each as when it
// starts and how long it shows, in seconds.
func cuesOf(t *testing.T, path string) [][2]float64 {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "s:0",
		"-show_entries", "packet=pts_time,duration_time", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	var cues [][2]float64
	for line := range strings.Lines(string(out)) {
		start, length, ok := strings.Cut(strings.TrimSpace(line), ",")
		s, err1 := strconv.ParseFloat(start, 64)
		l, err2 := strconv.ParseFloat(length, 64)
		if !ok || err1 != nil || err2 != nil {
			t.Fatalf("can't read the cue %q", line)
		}
		cues = append(cues, [2]float64{s, l})
	}
	return cues
}

func near(got, want float64) bool { return math.Abs(got-want) < 0.02 }

// The trimming pass drops a cue that starts after the length and shortens
// one that runs past it, leaving the others where they are, and keeps the
// tracks' tags and the attachments.
func TestTrimSideCutsCuesPastTheEnd(t *testing.T) {
	dir := t.TempDir()
	makeOverrunSource(t, dir) // to skip without ffmpeg
	srt, font, side := filepath.Join(dir, "overrun.srt"), filepath.Join(dir, "font.ttf"), filepath.Join(dir, "side.mkv")
	if err := os.WriteFile(font, []byte("not really a font"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Attachments are what make ffmpeg move the cues back by the first's
	// start, if it's let.
	ffmpeg(t, "-i", srt, "-attach", font, "-metadata:s:t:0", "mimetype=font/ttf", "-metadata:s:t:0", "filename=Test.ttf",
		"-c:s", "srt", "-metadata:s:s:0", "language=eng", "-metadata:s:s:0", "title=Test", "-disposition:s:0", "forced", side)

	trimmed := filepath.Join(dir, "trimmed.mkv")
	ffmpeg(t, (&plan{}).trimSideArgs(trimmed, side, 3*time.Second)...)

	cues := cuesOf(t, trimmed)
	if len(cues) != 2 || !near(cues[0][0], 0.5) || !near(cues[0][1], 1) || !near(cues[1][0], 2) || !near(cues[1][1], 1) {
		t.Errorf("cues (start, length) are %v, want (0.5, 1) and (2, 1), the second cut short and the third dropped", cues)
	}
	info, err := probe(context.Background(), trimmed)
	if err != nil {
		t.Fatal(err)
	}
	if info.Duration > 3100*time.Millisecond {
		t.Errorf("the file runs %s, want it to stop at 3s", info.Duration)
	}
	sub := streamOf(info, "subtitle")
	if sub.Language != "eng" || sub.Title != "Test" || !sub.Forced {
		t.Errorf("the subtitle track is %+v, want its language, title and flag kept", sub)
	}
	var fonts []streamInfo
	for _, st := range info.Streams {
		if st.Type == "attachment" {
			fonts = append(fonts, st)
		}
	}
	if len(fonts) != 1 || fonts[0].Filename != "Test.ttf" || fonts[0].Mimetype != "font/ttf" {
		t.Errorf("attachments are %+v, want the font with its name and type", fonts)
	}
}

// Subtitles that run on past the video are cut off at its end, whether or
// not a preroll is joined on, and the result runs as long as its video.
func TestEncodeFileCutsSubtitlesThatRunPastTheEnd(t *testing.T) {
	if err := CheckEncoder(EncoderCPU); err != nil {
		t.Skip(err)
	}
	for _, preroll := range []bool{false, true} {
		t.Run("preroll="+strconv.FormatBool(preroll), func(t *testing.T) {
			dir, outDir := t.TempDir(), t.TempDir()
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			src := makeOverrunSource(t, dir)
			ctx := context.Background()
			if in, err := probe(ctx, src); err != nil || in.Duration < time.Hour {
				t.Fatalf("the source runs %s (%v), want it stretched to over an hour by its subtitles", in.Duration, err)
			}
			s := testSettings(outDir)
			if preroll {
				intro := filepath.Join(dir, "intro.mkv")
				ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=2",
					"-f", "lavfi", "-i", "sine=frequency=880:duration=2:sample_rate=48000", "-c:v", "mpeg4", "-c:a", "aac", intro)
				s.Preroll = intro
			}
			p, err := prepare(ctx, src, s, func(string) {})
			if err != nil {
				t.Fatal(err)
			}
			var logged []string
			u := noUpdates()
			u.log = func(line string) { logged = append(logged, line) }
			if err := encodeFile(ctx, src, s, cpuDevice, u); err != nil {
				t.Fatalf("%v\n%s", err, strings.Join(logged, "\n"))
			}
			if !slices.ContainsFunc(logged, func(l string) bool { return strings.Contains(l, "so cut them off at") }) {
				t.Errorf("the log doesn't say the subtitles were cut:\n%s", strings.Join(logged, "\n"))
			}

			info, err := probe(ctx, p.out)
			if err != nil {
				t.Fatal(err)
			}
			_, added := prerollOf(info) // how far ahead of the video the preroll puts the cues
			offset := added.Seconds()
			if preroll == (added == 0) {
				t.Fatalf("the preroll runs %s, but the settings have preroll=%t", added, preroll)
			}
			if want := 3 + offset; math.Abs(info.Duration.Seconds()-want) > 0.2 {
				t.Errorf("the result runs %s, want about %gs, as long as its video", info.Duration, want)
			}
			if count(info, "subtitle") != 1 {
				t.Fatalf("want 1 subtitle track, got %d", count(info, "subtitle"))
			}
			cues := cuesOf(t, p.out)
			if len(cues) != 2 || !near(cues[0][0], 0.5+offset) || !near(cues[0][1], 1) || !near(cues[1][0], 2+offset) ||
				cues[1][0]+cues[1][1] > info.Duration.Seconds()+0.02 {
				t.Errorf("cues (start, length) are %v, want the first kept, and the second ending with the video at %s", cues, info.Duration)
			}
		})
	}
}

// Subtitles that end in time are left as they are, with no pass to trim them.
func TestEncodeFileLeavesSubtitlesThatEndInTime(t *testing.T) {
	if err := CheckEncoder(EncoderCPU); err != nil {
		t.Skip(err)
	}
	dir, outDir := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	src := makeSource(t, dir)
	var logged []string
	u := noUpdates()
	u.log = func(line string) { logged = append(logged, line) }
	if err := encodeFile(context.Background(), src, testSettings(outDir), cpuDevice, u); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(logged, func(l string) bool { return strings.Contains(l, "setts=") || strings.Contains(l, "cut them off") }) {
		t.Errorf("the subtitles were trimmed though they end before the video:\n%s", strings.Join(logged, "\n"))
	}
}

// The audio pass's file counts towards how long the picture and sound run,
// since it is the audio the result gets, and may run longer than the
// encoding pass's.
func TestTrimSideCountsTheAudioPassFile(t *testing.T) {
	dir := t.TempDir()
	makeOverrunSource(t, dir) // to skip without ffmpeg
	srt, side, audio := filepath.Join(dir, "overrun.srt"), filepath.Join(dir, "side.mkv"), filepath.Join(dir, "audio.mka")
	ffmpeg(t, "-i", srt, "-c:s", "srt", side)
	ffmpeg(t, "-f", "lavfi", "-i", "sine=frequency=440:duration=3:sample_rate=48000", "-c:a", "libopus", audio)
	// The encoding pass's file, whose audio stopped after a second.
	av := mediaInfo{Streams: []streamInfo{
		{Type: "video", Duration: 3 * time.Second}, {Type: "audio", Duration: time.Second},
	}}
	var made string
	intermediate := func() (string, func(), error) {
		made = filepath.Join(dir, "trimmed.mkv")
		return made, func() { os.Remove(made) }, nil
	}
	run := func(args []string, _ float64) error {
		ffmpeg(t, args...)
		return nil
	}
	got, done, err := trimSide(context.Background(), &plan{}, side, av, audio, intermediate, run, noUpdates())
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if got != made {
		t.Fatalf("trimSide kept %s, want the trimmed file", got)
	}
	if cues := cuesOf(t, got); len(cues) != 2 || !near(cues[1][0]+cues[1][1], 3) {
		t.Errorf("cues (start, length) are %v, want the second ending at 3s", cues)
	}

	// With the audio pass's file unreadable, the subtitles are kept as they
	// were, rather than the job failing.
	var logged []string
	u := noUpdates()
	u.log = func(line string) { logged = append(logged, line) }
	got, done2, err := trimSide(context.Background(), &plan{}, side, av, filepath.Join(dir, "missing.mka"), intermediate, run, u)
	if err != nil || got != side {
		t.Errorf("trimSide = %s, %v; want the subtitles as they were, without an error", got, err)
	}
	done2()
	if len(logged) != 1 || !strings.Contains(logged[0], "keeping them as they are") {
		t.Errorf("logged %q, want one line saying they were kept", logged)
	}
}
