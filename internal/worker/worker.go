// Package worker lets a cav1ar daemon encode on the GPUs of other machines,
// or containers: workers. A worker connects to the daemon over TCP, proves
// it has the daemon's token, and says what it can encode on. The daemon
// adds those devices to its queue, and runs the pass that encodes a job's
// video on the worker when one of them takes the job; see encode.Runner.
//
// Paths are the same on both sides: the worker reads the source where the
// daemon does, and writes the pass's files to a folder both share. When the
// worker joins, the daemon leaves a file in that folder for it to find, so a
// worker that doesn't see the same files never takes a job.
//
// Messages are JSON, one per line. A worker tells the daemon who it is with
// hello; the daemon answers with welcome or refused; the worker says ready
// once it has found the daemon's file, or error if it can't. After that the
// daemon sends run and signal, and the worker sends what each pass writes,
// with out, and how it ended, with exit.
package worker

import (
	"github.com/haydenkz/cav1ar/internal/encode"
)

// protocol changes when the messages do, so daemon and workers of
// different versions refuse each other rather than misunderstand.
const protocol = 1

// msg is one message, either way. One field is set.
type msg struct {
	Hello   *hello   `json:"hello,omitempty"`
	Welcome *welcome `json:"welcome,omitempty"`
	Refused string   `json:"refused,omitempty"` // why the daemon won't take the worker
	Ready   bool     `json:"ready,omitempty"`
	Error   string   `json:"error,omitempty"` // why the worker can't work for the daemon
	Run     *run     `json:"run,omitempty"`
	Signal  *signal  `json:"signal,omitempty"`
	Out     *out     `json:"out,omitempty"`
	Exit    *exit    `json:"exit,omitempty"`
}

type hello struct {
	Protocol int             `json:"protocol"`
	Token    string          `json:"token"`
	Name     string          `json:"name"`
	Devices  []encode.Device `json:"devices"`
}

type welcome struct {
	WorkDir string `json:"work_dir"` // the shared folder
	Check   string `json:"check"`    // the name of the file the daemon left in it
}

// run starts a pass: ffmpeg with Args.
type run struct {
	ID   int      `json:"id"`
	Args []string `json:"args"`
}

// signal pauses a pass ("stop"), carries it on ("cont"), or ends it ("kill").
type signal struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// out is a line a pass wrote.
type out struct {
	ID     int    `json:"id"`
	Stderr bool   `json:"stderr,omitempty"`
	Line   string `json:"line"`
}

// exit says a pass has ended, and why, if it failed.
type exit struct {
	ID  int    `json:"id"`
	Err string `json:"err,omitempty"`
}
