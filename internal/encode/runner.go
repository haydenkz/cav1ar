package encode

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A worker is another machine, or a container, that lends a daemon its
// GPUs. The daemon plans each job as usual and runs the pass that encodes
// the video on the worker, which reads the source and writes the pass's
// files at the same paths the daemon sees; see package worker. Everything
// else stays with the daemon.

// FFmpeg is the ffmpeg cav1ar runs to test devices and encode.
var FFmpeg = "ffmpeg"

// ffprobePath is the ffprobe that goes with FFmpeg: the one beside it, when
// FFmpeg is a path, or else whichever is on the PATH.
func ffprobePath() string {
	if strings.ContainsRune(FFmpeg, filepath.Separator) {
		return filepath.Join(filepath.Dir(FFmpeg), "ffprobe")
	}
	return "ffprobe"
}

// ffmpegBuilds holds what FFmpegBuild found for each ffmpeg.
var ffmpegBuilds sync.Map

// FFmpegBuild is what FFmpeg says of itself with -version: its version, how
// it was built, and its libraries' versions, or nothing if it won't run. It
// is worked out once for each FFmpeg.
func FFmpegBuild() string {
	ff := FFmpeg
	if b, ok := ffmpegBuilds.Load(ff); ok {
		return b.(string)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, ff, "-version").Output()
	b := strings.TrimSpace(string(out))
	ffmpegBuilds.Store(ff, b)
	return b
}

// Process is a running pass, which pausing stops and starts again.
type Process interface {
	Signal(os.Signal) error
}

// Runner runs ffmpeg passes on a worker. Run starts ffmpeg there with args
// and returns once it has exited, with an error if it failed. It returns
// ErrWorkerGone if the worker goes away first.
type Runner interface {
	Run(ctx context.Context, args []string, out Output) error
}

// A Runner can also have a method FFmpegBuild() string, which says what
// FFmpegBuild says on the worker, so that prerolls built there are built
// again when its ffmpeg changes.
type builder interface{ FFmpegBuild() string }

// Output is where a pass on a worker sends what it writes, line by line.
type Output struct {
	Stdout, Stderr func(line string)
	Started        func(Process) // once it is running
}

// ErrWorkerGone is returned for a pass whose worker went away. The job
// goes back in the queue, for whichever device has room next.
var ErrWorkerGone = errors.New("its worker went away")

var (
	workersMu sync.Mutex
	workers   = map[string]Runner{}
)

// AddWorker makes passes for devices on the worker called name run with r.
func AddWorker(name string, r Runner) {
	workersMu.Lock()
	defer workersMu.Unlock()
	workers[name] = r
}

// RemoveWorker forgets the worker called name.
func RemoveWorker(name string) {
	workersMu.Lock()
	defer workersMu.Unlock()
	delete(workers, name)
}

// runnerFor is what runs passes for d: nil for a device here, or its
// worker's Runner. A worker that has gone gives ErrWorkerGone.
func runnerFor(d Device) (Runner, error) {
	if d.Worker == "" {
		return nil, nil
	}
	workersMu.Lock()
	defer workersMu.Unlock()
	if r, ok := workers[d.Worker]; ok {
		return r, nil
	}
	return nil, ErrWorkerGone
}

// runRemote is runFFmpeg for a pass on a worker.
func runRemote(ctx context.Context, r Runner, args []string, duration time.Duration, rate float64, u updates) error {
	pr, pw := io.Pipe()
	read := make(chan struct{})
	go func() {
		readProgress(pr, duration, rate, u.progress)
		io.Copy(io.Discard, pr) // in case it stopped early
		close(read)
	}()
	var last string // last stderr line, which is usually the reason for a failure
	err := r.Run(ctx, args, Output{
		Stdout: func(line string) { io.WriteString(pw, line+"\n") },
		Stderr: func(line string) {
			if line = strings.TrimRight(line, " \r"); line != "" {
				last = strings.TrimSpace(line)
				u.log(line)
			}
		},
		Started: u.started,
	})
	pw.Close()
	<-read
	switch {
	case err == nil || errors.Is(err, ErrWorkerGone) || ctx.Err() != nil:
		return err
	case last != "":
		return errors.New(last)
	}
	return err
}
