package encode

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// encodeFile encodes in to AV1 with ffmpeg, using the encoder, audio,
// subtitle and metadata choices in s.
func encodeFile(ctx context.Context, in string, s Settings, u updates) error {
	if err := CheckEncoder(s.Encoder); err != nil {
		return err
	}
	info, err := probe(ctx, in)
	if err != nil {
		return err
	}
	p, err := newPlan(in, s, info)
	if err != nil {
		return err
	}
	out := p.output()
	u.output(out)
	if _, err := os.Stat(out); err == nil {
		return fmt.Errorf("%s already exists", filepath.Base(out))
	}
	for _, n := range p.notes {
		u.log("cav1ar: " + n)
	}
	args := p.args(out)
	u.log("cav1ar: ffmpeg " + strings.Join(args, " "))

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
	readProgress(stdout, info.Duration, u.progress)
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		os.Remove(out)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if last != "" {
			return errors.New(last)
		}
		return fmt.Errorf("ffmpeg: %w", err)
	}

	if s.DeleteOriginal {
		if err := checkEncode(ctx, out, info.Duration); err != nil {
			u.log("cav1ar: kept the original, because " + err.Error())
			u.note("original kept")
		} else if err := os.Remove(in); err != nil {
			u.log("cav1ar: couldn't delete the original: " + err.Error())
			u.note("original not deleted")
		} else {
			u.log("cav1ar: deleted the original, " + in)
		}
	}
	return nil
}

// checkEncode makes sure an encode is complete before its original is
// deleted: it must have AV1 video and run as long as the source.
func checkEncode(ctx context.Context, out string, want time.Duration) error {
	info, err := probe(ctx, out)
	if err != nil {
		return fmt.Errorf("the encode couldn't be read: %w", err)
	}
	if !slices.ContainsFunc(info.Streams, func(st streamInfo) bool { return st.Type == "video" && st.Codec == "av1" }) {
		return errors.New("the encode has no AV1 video")
	}
	if diff := (info.Duration - want).Abs(); want > 0 && diff > max(2*time.Second, want/100) {
		return fmt.Errorf("the encode runs %s but the source runs %s",
			info.Duration.Round(time.Second), want.Round(time.Second))
	}
	return nil
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
