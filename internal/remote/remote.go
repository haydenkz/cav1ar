// Package remote lets the UI show and control a daemon's queue, over a Unix
// socket only the user can open. The daemon sends what it has on connect,
// then every change as it happens; the UI sends back what the user does.
// Messages are JSON, one per line.
package remote

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/haydenkz/cav1ar/internal/encode"
)

// protocol is the version of the messages below. The daemon sends it when a
// UI connects, and the UI only carries on with a daemon that sends the same,
// so a UI and a daemon from different versions of cav1ar never misread each
// other. Raise it with any change the other side wouldn't understand.
const protocol = 1

// maxRequest is the longest request the daemon reads, plenty for a path.
const maxRequest = 64 << 10

// SocketPath is where a daemon listens.
func SocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "cav1ar.sock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("cav1ar-%d.sock", os.Getuid()))
}

// checkOwner checks that the socket at path is this user's, so one another
// user left in a folder anyone can write to, such as /tmp, is never taken
// for the daemon's.
func checkOwner(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || fi.Mode().Type() != fs.ModeSocket || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s isn't a socket of this user's, so it can't be the cav1ar daemon's; remove it", path)
	}
	return nil
}

// request is something the UI asks the daemon to do.
type request struct {
	Op      string `json:"op"`             // add, cancel, retry, pause or hold
	Path    string `json:"path,omitempty"` // for add: a full path, since the daemon's working folder isn't the UI's
	ID      int    `json:"id,omitempty"`
	Paused  bool   `json:"paused,omitempty"`
	Holding bool   `json:"holding,omitempty"`
}

// message is what the daemon sends: everything when the UI connects, then
// changes. The state fields are sent every time.
type message struct {
	Version  int              `json:"version,omitempty"` // when the UI connects: the protocol
	Error    string           `json:"error,omitempty"`   // to one UI: why the daemon didn't do what it asked
	Jobs     []job            `json:"jobs,omitempty"`    // when the UI connects
	Logs     map[int][]string `json:"logs,omitempty"`    // when the UI connects: recent output, by job
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
