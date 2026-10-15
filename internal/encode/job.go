// Package encode runs AV1 encodes and reports their progress. It has no
// knowledge of the TUI, so it can be driven headless as well.
package encode

import (
	"os"
	"strings"
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
	Output string // where the result goes, once decided; the same as Input when nothing needed doing
	Action Action // what the job decided the file needs, once it has
	Step   string // what it's doing beyond that, such as "adding the preroll"
	State  State
	Next   bool // queued ahead of the rest, such as a video that has just arrived

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
	if j.Speed > 0 && j.Duration > 0 {
		// Going by the encoder's speed counts only the pass it's on, where
		// time elapsed would count looking the file over first, and any
		// earlier pass, as if they were part of this one.
		return time.Duration(float64(j.Duration) * (1 - j.Progress) / j.Speed)
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

// A job's log mixes ffmpeg's output with lines cav1ar writes itself, which
// start with ownPrefix. Those marking the start of one of the job's stages,
// such as an ffmpeg pass or checking the result, start with stagePrefix.
const (
	ownPrefix   = "cav1ar: "
	stagePrefix = ownPrefix + "▸ "
)

// Own returns the message of a log line cav1ar wrote itself, and reports
// whether it was one, rather than a line of ffmpeg's output.
func Own(line string) (msg string, ok bool) { return strings.CutPrefix(line, ownPrefix) }

// Stage returns the name of the stage a log line marks the start of, and
// reports whether it marks one.
func Stage(line string) (name string, ok bool) { return strings.CutPrefix(line, stagePrefix) }

// updates is how a running encode reports back to the queue.
type updates struct {
	planned  func(Action, string) // what the job will do, and where the result will go
	started  func(*os.Process)    // the encoder process, so pausing can suspend it
	progress func(progress)
	log      func(line string)
	note     func(note string) // sets Job.Note
	step     func(step string) // sets Job.Step
}

// stage marks the start of one of the job's stages in its log.
func (u updates) stage(name string) { u.log(stagePrefix + name) }

type progress struct {
	done     float64 // 0..1
	fps      float64
	speed    float64
	duration time.Duration
	size     int64 // bytes written so far
}
