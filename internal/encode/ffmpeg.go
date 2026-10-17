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
// settings say to and it has tracks that need telling apart.
func prepare(ctx context.Context, in string, s Settings, log func(string)) (*plan, error) {
	info, err := probe(ctx, in)
	if err != nil {
		return nil, err
	}
	p, err := newPlan(in, s, info)
	if err != nil {
		return nil, err
	}
	if p.prerollIn > 0 && p.action() != ActionNone && p.action() != ActionRename {
		if p.prerollIn, err = joinPoint(ctx, in, p.prerollIn); err != nil {
			return nil, err
		}
	}
	// Subtitles are stored among the video, so reading them reads the start
	// of the whole file: gigabytes for a film, which slows every other job
	// reading from the same disks. Only do it when the flags leave two
	// tracks in a language claiming the same role.
	if p.checkSubs {
		log("cav1ar: reading the subtitles")
		stats, err := readSubs(ctx, in, p.subs)
		if err != nil {
			return nil, err
		}
		p.tidySubs(stats)
	}
	return p, nil
}

// encodeFile brings in into line with s, encoding on d: encoding, remuxing
// or renaming it, or leaving it be if it already matches.
func encodeFile(ctx context.Context, in string, s Settings, d Device, u updates) error {
	u.stage("checking the file")
	p, err := prepare(ctx, in, s, u.log)
	if err != nil {
		return err
	}
	p.device = d
	for _, n := range p.notes {
		u.log("cav1ar: " + n)
	}
	action, out := p.action(), p.out
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

	plan := "plan: "
	if action == ActionRetag {
		plan = "plan, editing the file in place: "
	}
	u.log("cav1ar: " + plan + strings.Join(p.reasons(), " · "))
	switch {
	case out == in && !s.DeleteOriginal:
		return errors.New("the result would replace the original, which the settings keep; " +
			"choose to delete originals, or an output folder")
	case out != in && exists(out):
		return fmt.Errorf("%s already exists", filepath.Base(out))
	}
	if action == ActionRetag {
		u.stage("fixing the tags in place")
		err := retag(ctx, p)
		if err == nil && out != in {
			err = renameNoReplace(in, out)
		}
		if err == nil {
			u.log("cav1ar: fixed the tags in place, without rewriting the file")
			if out != in {
				u.log("cav1ar: renamed it to " + filepath.Base(out))
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		u.log("cav1ar: couldn't fix the tags in place, so remuxing it instead: " + err.Error())
		action = ActionRemux
		u.planned(action, out)
	}
	// The passes' files are only intermediates, so keep them off the
	// library's storage, which then only reads the source and writes the
	// result.
	work, err := workDir()
	if err != nil {
		return err
	}
	tmp := tempPath(out)
	if s.Preroll != "" { // the result is an intermediate too, which the preroll is joined onto
		tmp = filepath.Join(work, filepath.Base(tmp))
	}
	release, err := holdTemp(tmp)
	if err != nil {
		return err
	}
	defer release()
	defer os.Remove(tmp) // gone already, once the result is in place
	mainInfo, err := encodeAndRemux(ctx, p, action, tmp, work, u)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	if s.Preroll != "" {
		joined := tempPath(out)
		release, err := holdTemp(joined)
		if err != nil {
			return err
		}
		defer release()
		defer os.Remove(joined)
		length, err := addPreroll(ctx, s, d, tmp, mainInfo, joined, u)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return fmt.Errorf("adding the preroll: %w", err)
		}
		u.stage("checking the result with the preroll")
		if err := checkJoin(ctx, joined, mainInfo, length); err != nil {
			return fmt.Errorf("the result with the preroll failed its checks, so the original was kept: %w", err)
		}
		os.Remove(tmp)
		tmp = joined
	}
	if out == in {
		u.stage("replacing the original")
		if err := os.Rename(tmp, out); err != nil {
			return err
		}
		u.log("cav1ar: replaced the original with the result")
		return nil
	}
	u.stage("moving the result into place")
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

// assembling names the stage that copies the encoding pass's files into one,
// by what goes into it.
func (p *plan) assembling() string {
	parts := []string{"video"}
	if len(p.audio) > 0 {
		parts = append(parts, "audio")
	}
	if len(p.subs) > 0 {
		parts = append(parts, "subtitles")
	}
	if len(parts) == 1 {
		return "writing the file"
	}
	return "putting the " + joinAnd(parts) + " together"
}

// encodeAndRemux writes the result to tmp: an encoding pass writes the video
// and audio, and the subtitles and attachments, to files of their own in
// work, and a remuxing pass puts them together; see plan.encodeArgs. It
// returns what the result holds, once it checks out.
func encodeAndRemux(ctx context.Context, p *plan, action Action, tmp, work string, u updates) (mediaInfo, error) {
	// intermediate picks a file for a pass to write, held so no cav1ar takes
	// it for one left behind, and removed when done is called.
	intermediate := func() (path string, done func(), err error) {
		path = tempPath(filepath.Join(work, "intermediate"))
		release, err := holdTemp(path)
		if err != nil {
			return "", nil, err
		}
		return path, func() { release(); os.Remove(path) }, nil
	}
	// rate is the frame rate of the video a pass writes, or 0 for none.
	run := func(args []string, rate float64) error {
		u.log("cav1ar: ffmpeg " + strings.Join(args, " "))
		return runFFmpeg(ctx, args, p.info.Duration, rate, u)
	}

	av, done, err := intermediate()
	if err != nil {
		return mediaInfo{}, err
	}
	defer done()
	var side string
	if p.hasSide() {
		if side, done, err = intermediate(); err != nil {
			return mediaInfo{}, err
		}
		defer done()
	}
	hwDecode := []bool{false}
	if action == ActionEncode && p.device.GPU() {
		hwDecode = []bool{true, false}
	}
	for i, hw := range hwDecode {
		stage := p.encodingPass()
		if i > 0 {
			stage += " again, decoding on the CPU"
		}
		u.stage(stage)
		err = run(p.encodeArgs(av, side, hw), frameRate(p.video))
		if err == nil || ctx.Err() != nil || i == len(hwDecode)-1 {
			break
		}
		u.log("cav1ar: decoding on the GPU failed (" + err.Error() + "), so trying again on the CPU")
	}
	if err != nil {
		return mediaInfo{}, err
	}

	// The audio can still stop early, in which case encode it again on its
	// own. Video that stops early can't be helped that way, so leave it for
	// the result's checks to turn down.
	var audio string
	if len(p.audio) > 0 {
		info, err := probe(ctx, av)
		if err != nil {
			return mediaInfo{}, fmt.Errorf("the encoding pass's file couldn't be read: %w", err)
		}
		var short *shortStream
		if err := checkStreamLengths(info, p); errors.As(err, &short) && short.kind == "audio" {
			u.log("cav1ar: the encoding pass's " + strings.TrimPrefix(short.Error(), "its ") +
				", so encoding the audio again on its own")
			if audio, done, err = intermediate(); err != nil {
				return mediaInfo{}, err
			}
			defer done()
			u.step("encoding the audio again")
			u.stage("encoding the audio again, on its own")
			if err := run(p.audioArgs(audio), 0); err != nil {
				return mediaInfo{}, err
			}
		}
	}

	u.step("putting together")
	u.stage(p.assembling())
	if err := run(p.remuxArgs(tmp, av, audio, side), frameRate(p.video)); err != nil {
		return mediaInfo{}, err
	}
	u.step("")
	u.stage("checking the result")
	info, err := verify(ctx, tmp, p)
	if err != nil {
		return info, fmt.Errorf("the result failed its checks, so the original was kept: %w", err)
	}
	return info, nil
}

// runFFmpeg runs ffmpeg, reporting its progress and output as it goes. rate
// is the frame rate of the video it writes, or 0 if it writes none.
func runFFmpeg(ctx context.Context, args []string, duration time.Duration, rate float64, u updates) error {
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
	readProgress(stdout, duration, rate, u.progress)
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
func verify(ctx context.Context, out string, p *plan) (mediaInfo, error) {
	info, err := probe(ctx, out)
	if err != nil {
		return info, fmt.Errorf("it couldn't be read: %w", err)
	}
	count := map[string]int{}
	for _, st := range info.Streams {
		if st.CoverArt {
			continue
		}
		count[st.Type]++
		if st.Type == "video" && st.Codec != "av1" {
			return info, errors.New("its video isn't AV1")
		}
	}
	switch {
	case count["video"] != 1:
		return info, fmt.Errorf("it has %d video tracks, not 1", count["video"])
	case count["audio"] != len(p.audio):
		return info, fmt.Errorf("it has %d audio tracks, not %d", count["audio"], len(p.audio))
	case count["subtitle"] != len(p.subs):
		return info, fmt.Errorf("it has %d subtitle tracks, not %d", count["subtitle"], len(p.subs))
	}

	srcLen, outLen := longestPlayed(p.sources()), longestPlayed(info.Streams)
	if srcLen == 0 || outLen == 0 {
		srcLen, outLen = p.info.Duration, info.Duration
	}
	if srcLen > 0 && srcLen-outLen > tolerance(srcLen) {
		return info, fmt.Errorf("it runs %s but the source runs %s", outLen.Round(time.Second), srcLen.Round(time.Second))
	}
	if p.prerollIn > 0 && outLen-srcLen > p.prerollIn/2 {
		return info, errors.New("the source's preroll wasn't cut off")
	}
	return info, checkStreamLengths(info, p)
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
			return &shortStream{kind: st.Type, at: st.Duration, want: want}
		}
	}
	return nil
}

// shortStream is a video or audio stream that stops before it should.
type shortStream struct {
	kind     string // video or audio
	at, want time.Duration
}

func (e *shortStream) Error() string {
	return fmt.Sprintf("its %s stops at %s, but should run to %s",
		e.kind, e.at.Round(time.Second), e.want.Round(time.Second))
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
// that each end with a "progress" key. For a pass that writes video at rate
// frames per second, how far along it is and how fast it goes come from the
// frames written: out_time is as far as any stream has got, and audio, which
// encodes far faster than video, gets to the end long before it.
func readProgress(r io.Reader, dur time.Duration, rate float64, report func(progress)) {
	p := progress{duration: dur}
	frames := dur.Seconds() * rate // expected, or 0 if unknown
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "frame":
			if n, err := strconv.ParseFloat(v, 64); err == nil && frames > 0 {
				p.done = min(max(n/frames, 0), 1)
			}
		case "fps":
			p.fps, _ = strconv.ParseFloat(v, 64)
			if rate > 0 {
				p.speed = p.fps / rate
			}
		case "speed":
			if rate == 0 {
				p.speed, _ = strconv.ParseFloat(strings.TrimSuffix(v, "x"), 64)
			}
		case "total_size":
			p.size, _ = strconv.ParseInt(v, 10, 64)
		case "out_time_us":
			if us, err := strconv.ParseInt(v, 10, 64); err == nil && dur > 0 && frames == 0 {
				p.done = min(max(float64(us)/float64(dur.Microseconds()), 0), 1)
			}
		case "progress":
			report(p)
		}
	}
}
