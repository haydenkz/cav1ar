package encode

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// tempSuffix ends the name of a result while it is being written. With the
// leading dot that tempPath adds, media servers and cav1ar's own folder scans
// skip it.
const tempSuffix = ".cav1ar-tmp.mkv"

// tempPath is where the result for out is written until it checks out: a
// hidden file beside it, with a random name, since one built from out could
// run past the length limit for file names when out comes close to it.
func tempPath(out string) string {
	return filepath.Join(filepath.Dir(out), "."+rand.Text()+tempSuffix)
}

// IsTemp reports whether path is a result still being written, or left
// behind by a cav1ar that stopped abruptly.
func IsTemp(path string) bool {
	name := filepath.Base(path)
	return strings.HasPrefix(name, ".") && strings.HasSuffix(name, tempSuffix)
}

// Decision is what a job would do to a file.
type Decision struct {
	Action  Action
	Output  string   // where the result would go
	Reasons []string // what doesn't match the settings, apart from the name
	Notes   []string // choices made along the way, such as tracks left out
}

// Inspect works out what a job would do to path, without doing it. When
// subtitles need tidying, that means reading the start of the file.
func Inspect(ctx context.Context, path string, s Settings) (Decision, error) {
	p, err := prepare(ctx, path, s, func(string) {})
	if err != nil {
		return Decision{}, err
	}
	return Decision{Action: p.action(), Output: p.out, Reasons: p.reasons(), Notes: p.notes}, nil
}

// prepare plans what to do with in, tidying its subtitles first if the
// settings say to and it is going to be rewritten anyway, or has tracks
// that need telling apart.
func prepare(ctx context.Context, in string, s Settings, log func(string)) (*plan, error) {
	info, err := probe(ctx, in)
	if err != nil {
		return nil, err
	}
	p, err := newPlan(in, s, info)
	if err != nil {
		return nil, err
	}
	if s.TidySubs && len(p.subs) > 0 && (p.checkSubs || p.action() == ActionRemux || p.action() == ActionEncode) {
		log("cav1ar: reading the subtitles")
		stats, err := readSubs(ctx, in, p.subs)
		if err != nil {
			return nil, err
		}
		p.tidySubs(stats)
	}
	return p, nil
}

// encodeFile brings in into line with s: encoding, remuxing or renaming it,
// or leaving it be if it already matches.
func encodeFile(ctx context.Context, in string, s Settings, u updates) error {
	p, err := prepare(ctx, in, s, u.log)
	if err != nil {
		return err
	}
	for _, n := range p.notes {
		u.log("cav1ar: " + n)
	}
	action, out, info := p.action(), p.out, p.info
	u.planned(action, out)

	switch action {
	case ActionNone:
		u.log("cav1ar: nothing to do; it already matches the settings")
		return nil
	case ActionRename:
		if err := renameNoReplace(in, out); err != nil {
			return err
		}
		u.log("cav1ar: renamed it to " + filepath.Base(out))
		return nil
	}

	u.log(fmt.Sprintf("cav1ar: %s, because of: %s", action, strings.Join(p.reasons(), "; ")))
	switch {
	case out == in && !s.DeleteOriginal:
		return errors.New("the result would replace the original, which the settings keep; " +
			"choose to delete originals, or an output folder")
	case out != in && exists(out):
		return fmt.Errorf("%s already exists", filepath.Base(out))
	}
	if action == ActionEncode {
		if err := CheckEncoder(s.Encoder); err != nil {
			return err
		}
	}

	tmp := tempPath(out)
	defer os.Remove(tmp) // gone already, once the result is in place
	hwDecode := []bool{false}
	if action == ActionEncode && s.Encoder == EncoderNVIDIA {
		hwDecode = []bool{true, false}
	}
	for i, hw := range hwDecode {
		args := p.args(tmp, hw)
		u.log("cav1ar: ffmpeg " + strings.Join(args, " "))
		err = runFFmpeg(ctx, args, info.Duration, u)
		if err == nil || ctx.Err() != nil || i == len(hwDecode)-1 {
			break
		}
		u.log("cav1ar: decoding on the GPU failed (" + err.Error() + "), so trying again on the CPU")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}

	if err := verify(ctx, tmp, p); err != nil {
		return fmt.Errorf("the result failed its checks, so the original was kept: %w", err)
	}
	if out == in {
		if err := os.Rename(tmp, out); err != nil {
			return err
		}
		u.log("cav1ar: replaced the original with the result")
		return nil
	}
	if err := renameNoReplace(tmp, out); err != nil {
		return err
	}
	if s.DeleteOriginal {
		if err := os.Remove(in); err != nil {
			u.log("cav1ar: couldn't delete the original: " + err.Error())
			u.note("original not deleted")
		} else {
			u.log("cav1ar: deleted the original, " + in)
		}
	}
	return nil
}

// runFFmpeg runs ffmpeg, reporting its progress and output as it goes.
func runFFmpeg(ctx context.Context, args []string, duration time.Duration, u updates) error {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	u.started(cmd.Process)

	var (
		wg   sync.WaitGroup
		last string // last stderr line, which is usually the reason for a failure
	)
	wg.Go(func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if line := strings.TrimRight(sc.Text(), " \r"); line != "" {
				last = strings.TrimSpace(line)
				u.log(line)
			}
		}
	})
	readProgress(stdout, duration, u.progress)
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		if last != "" && ctx.Err() == nil {
			return errors.New(last)
		}
		return fmt.Errorf("ffmpeg: %w", err)
	}
	return nil
}

// verify checks a result before it replaces or joins anything: it must have
// AV1 video and the tracks planned, and its video and audio must run as long
// as the source's did. ffmpeg has exited cleanly on encodes whose audio
// stopped an hour early.
func verify(ctx context.Context, out string, p *plan) error {
	info, err := probe(ctx, out)
	if err != nil {
		return fmt.Errorf("it couldn't be read: %w", err)
	}
	count := map[string]int{}
	for _, st := range info.Streams {
		if st.CoverArt {
			continue
		}
		count[st.Type]++
		if st.Type == "video" && st.Codec != "av1" {
			return errors.New("its video isn't AV1")
		}
	}
	switch {
	case count["video"] != 1:
		return fmt.Errorf("it has %d video tracks, not 1", count["video"])
	case count["audio"] != len(p.audio):
		return fmt.Errorf("it has %d audio tracks, not %d", count["audio"], len(p.audio))
	case count["subtitle"] != len(p.subs):
		return fmt.Errorf("it has %d subtitle tracks, not %d", count["subtitle"], len(p.subs))
	}

	srcLen, outLen := longestPlayed(p.sources()), longestPlayed(info.Streams)
	if srcLen == 0 || outLen == 0 {
		srcLen, outLen = p.info.Duration, info.Duration
	}
	if srcLen > 0 && srcLen-outLen > tolerance(srcLen) {
		return fmt.Errorf("it runs %s but the source runs %s", outLen.Round(time.Second), srcLen.Round(time.Second))
	}
	return checkStreamLengths(info, p)
}

// sources are the source streams the plan keeps.
func (p *plan) sources() []streamInfo {
	out := []streamInfo{p.video}
	for _, a := range p.audio {
		out = append(out, a.streamInfo)
	}
	return out
}

// longestPlayed is the length of the longest video or audio stream, or 0 if
// the file doesn't say. Subtitles don't count: one that runs past the end
// can stretch a file by most of an hour.
func longestPlayed(streams []streamInfo) time.Duration {
	var longest time.Duration
	for _, st := range streams {
		if (st.Type == "video" && !st.CoverArt) || st.Type == "audio" {
			longest = max(longest, st.Duration)
		}
	}
	return longest
}

func tolerance(d time.Duration) time.Duration { return max(2*time.Second, d/100) }

// checkStreamLengths catches a stream that stops early, which the length of
// the whole file can't. Each stream must run as long as it did in the
// source, or, where the source doesn't say, nearly as long as the longest
// one in the result: some sources end on silent credits.
func checkStreamLengths(out mediaInfo, p *plan) error {
	longest := longestPlayed(out.Streams)
	audio := 0
	for _, st := range out.Streams {
		var src streamInfo
		switch {
		case st.Type == "video" && !st.CoverArt:
			src = p.video
		case st.Type == "audio" && audio < len(p.audio):
			src = p.audio[audio].streamInfo
			audio++
		default:
			continue
		}
		if st.Duration == 0 { // the muxer didn't record it
			continue
		}
		want, slack := longest, max(2*time.Minute, tolerance(longest))
		if src.Duration > 0 {
			want, slack = src.Duration, tolerance(src.Duration)
		}
		if want-st.Duration > slack {
			return fmt.Errorf("its %s stops at %s, but should run to %s",
				st.Type, st.Duration.Round(time.Second), want.Round(time.Second))
		}
	}
	return nil
}

// renameNoReplace moves from to to, unless something is already at to.
func renameNoReplace(from, to string) error {
	err := unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
	switch {
	case errors.Is(err, unix.EEXIST):
		return fmt.Errorf("%s already exists", filepath.Base(to))
	case errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS):
		// The filesystem can't do it atomically, so check first.
		if exists(to) {
			return fmt.Errorf("%s already exists", filepath.Base(to))
		}
		return os.Rename(from, to)
	case err != nil:
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

// readProgress parses ffmpeg's -progress output, a stream of key=value blocks
// that each end with a "progress" key.
func readProgress(r io.Reader, dur time.Duration, report func(progress)) {
	p := progress{duration: dur}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "fps":
			p.fps, _ = strconv.ParseFloat(v, 64)
		case "speed":
			p.speed, _ = strconv.ParseFloat(strings.TrimSuffix(v, "x"), 64)
		case "total_size":
			p.size, _ = strconv.ParseInt(v, 10, 64)
		case "out_time_us":
			if us, err := strconv.ParseInt(v, 10, 64); err == nil && dur > 0 {
				p.done = min(max(float64(us)/float64(dur.Microseconds()), 0), 1)
			}
		case "progress":
			report(p)
		}
	}
}
