// Package encode runs AV1 encodes and reports their progress. It has no
// knowledge of the TUI, so it can be driven headless as well.
package encode

import (
	"os"
	"time"
)

// State is where a job is in its lifecycle.
type State int

const (
	Queued State = iota
	Running
	Done
	Failed
	Canceled
)

func (s State) String() string {
	switch s {
	case Queued:
		return "queued"
	case Running:
		return "running"
	case Done:
		return "done"
	case Failed:
		return "failed"
	case Canceled:
		return "canceled"
	}
	return "unknown"
}

// Finished reports whether the job has stopped, successfully or not.
func (s State) Finished() bool { return s >= Done }

// Job is a snapshot of one encode. The Queue owns the live copy; every Job it
// hands out is a value copy that is safe to keep.
type Job struct {
	ID     int
	Input  string
	Output string
	State  State

	Progress float64       // 0..1; stays 0 when Duration is unknown
	FPS      float64       // encoder frames per second
	Speed    float64       // realtime multiplier, e.g. 1.5 = 1.5x
	Duration time.Duration // media duration, 0 if unknown
	InSize   int64
	OutSize  int64

	Started   time.Time
	Finished  time.Time
	PausedFor time.Duration // time spent paused, which doesn't count as elapsed
	Err       error
	Note      string // worth flagging about a finished job, e.g. "original kept"
}

// Elapsed is how long the job has been running, or ran for, not counting
// time paused.
func (j Job) Elapsed() time.Duration {
	switch {
	case j.Started.IsZero():
		return 0
	case j.Finished.IsZero():
		return time.Since(j.Started) - j.PausedFor
	}
	return j.Finished.Sub(j.Started) - j.PausedFor
}

// ETA estimates the time remaining from progress so far. It is 0 when there
// is not enough information to guess.
func (j Job) ETA() time.Duration {
	if j.State != Running || j.Progress <= 0 || j.Progress >= 1 {
		return 0
	}
	e := j.Elapsed()
	return time.Duration(float64(e) * (1 - j.Progress) / j.Progress)
}

// Event is emitted whenever a job changes. Line is set when the change is a
// new line of encoder output.
type Event struct {
	Job  Job
	Line string
}

// updates is how a running encode reports back to the queue.
type updates struct {
	output   func(path string) // where the encode is being written, once decided
	started  func(*os.Process) // the encoder process, so pausing can suspend it
	progress func(progress)
	log      func(line string)
	note     func(note string) // sets Job.Note
}

type progress struct {
	done     float64 // 0..1
	fps      float64
	speed    float64
	duration time.Duration
	size     int64 // bytes written so far
}
