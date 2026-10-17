// Package remote lets the UI show and control a daemon's queue, over a Unix
// socket only the user can open. The daemon sends what it has on connect,
// then every change as it happens; the UI sends back what the user does.
// Messages are JSON, one per line.
package remote

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/haydenkz/cav1ar/internal/encode"
)

// SocketPath is where a daemon listens.
func SocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "cav1ar.sock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("cav1ar-%d.sock", os.Getuid()))
}

// request is something the UI asks the daemon to do.
type request struct {
	Op      string `json:"op"` // add, cancel, retry, pause or hold
	Path    string `json:"path,omitempty"`
	ID      int    `json:"id,omitempty"`
	Paused  bool   `json:"paused,omitempty"`
	Holding bool   `json:"holding,omitempty"`
}

// message is what the daemon sends: everything when the UI connects, then
// changes. The state fields are sent every time.
type message struct {
	Jobs     []job            `json:"jobs,omitempty"` // when the UI connects
	Logs     map[int][]string `json:"logs,omitempty"` // when the UI connects: recent output, by job
	Events   []event          `json:"events,omitempty"`
	Settings *encode.Settings `json:"settings,omitempty"` // when the UI connects, and when they change
	Devices  []encode.Device  `json:"devices"`            // what the daemon encodes on
	Paused   bool             `json:"paused"`
	Holding  bool             `json:"holding"`
	Watching bool             `json:"watching"`
	Saved    int64            `json:"saved"`   // over every run; see encode.Watcher.Totals
	Results  int              `json:"results"` // over every run
}

// job is an encode.Job with its error as text, which JSON can carry.
type job struct {
	encode.Job
	Err string `json:"err,omitempty"`
}

func toWire(j encode.Job) job {
	w := job{Job: j}
	if j.Err != nil {
		w.Err = j.Err.Error()
	}
	w.Job.Err = nil
	return w
}

func (w job) job() encode.Job {
	j := w.Job
	if w.Err != "" {
		j.Err = errors.New(w.Err)
	}
	return j
}

type event struct {
	Job  job    `json:"job"`
	Line string `json:"line,omitempty"`
}
