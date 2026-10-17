package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/haydenkz/cav1ar/internal/encode"
)

// Options say whom a worker works for, and what it lets passes touch.
type Options struct {
	Addr    string          // the daemon's, such as "192.168.1.10:7310"
	Token   string          // the daemon's worker token
	Name    string          // how the daemon knows this worker; the host name will do
	Allow   []string        // folders passes may use, besides the GPUs; empty allows any
	Devices []encode.Device // what this worker lends the daemon
	Log     func(format string, args ...any)
}

// Work lends the daemon this machine's devices until ctx is done,
// connecting again whenever the connection drops.
func Work(ctx context.Context, o Options) error {
	if len(o.Devices) == 0 {
		return encode.ErrNoEncoder
	}
	wait := 5 * time.Second
	for {
		started := time.Now()
		err := connect(ctx, o)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(started) > time.Minute {
			wait = 5 * time.Second
		}
		o.Log("cav1ar: %v; trying again in %s", err, wait)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		wait = min(2*wait, time.Minute)
	}
}

// connect is one connection to the daemon, which lasts until either side
// goes away. Passes still running when it ends are stopped.
func connect(ctx context.Context, o Options) error {
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	nc, err := d.DialContext(ctx, "tcp", o.Addr)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { <-ctx.Done(); nc.Close() }()

	l := &link{nc: nc, enc: json.NewEncoder(nc), procs: map[int]*os.Process{}, allow: o.Allow}
	dec := json.NewDecoder(nc)
	if err := l.send(msg{Hello: &hello{Protocol: protocol, Token: o.Token, Name: o.Name, Devices: o.Devices}}); err != nil {
		return err
	}
	var m msg
	if err := dec.Decode(&m); err != nil {
		return fmt.Errorf("the daemon hung up: %w", err)
	}
	if m.Welcome == nil {
		return errors.New("the daemon refused: " + m.Refused)
	}
	if err := checkShared(m.Welcome); err != nil {
		l.send(msg{Error: err.Error()})
		return err
	}
	if err := l.send(msg{Ready: true}); err != nil {
		return err
	}
	o.Log("cav1ar: working for the daemon at %s", o.Addr)

	var passes sync.WaitGroup
	defer passes.Wait()
	defer cancel() // stops every pass, before waiting for them
	for {
		var m msg
		if err := dec.Decode(&m); err != nil {
			return fmt.Errorf("lost the daemon: %w", err)
		}
		switch {
		case m.Run != nil:
			r := *m.Run
			passes.Go(func() { l.run(ctx, r) })
		case m.Signal != nil:
			l.signal(*m.Signal)
		}
	}
}

// checkShared checks this worker sees the daemon's file in the shared
// folder, and can write there itself.
func checkShared(wl *welcome) error {
	if _, err := os.Stat(filepath.Join(wl.WorkDir, wl.Check)); err != nil {
		return fmt.Errorf("this worker doesn't see %s as the daemon does, so it can't share files with it: %v", wl.WorkDir, err)
	}
	f, err := os.CreateTemp(wl.WorkDir, ".cav1ar-worker-check-*")
	if err != nil {
		return fmt.Errorf("this worker can't write to %s: %v", wl.WorkDir, err)
	}
	f.Close()
	return os.Remove(f.Name())
}

// link is the worker's side of a connection.
type link struct {
	nc    net.Conn
	allow []string

	wmu sync.Mutex // one message at a time
	enc *json.Encoder

	mu    sync.Mutex
	procs map[int]*os.Process // running passes, by ID
}

func (l *link) send(m msg) error {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	l.nc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return l.enc.Encode(m)
}

// run runs one pass, sending what it writes as it goes, then how it ended.
func (l *link) run(ctx context.Context, r run) {
	err := l.allowed(r.Args)
	if err == nil {
		err = l.ffmpeg(ctx, r)
	}
	e := &exit{ID: r.ID}
	if err != nil {
		e.Err = err.Error()
	}
	l.send(msg{Exit: e})
}

func (l *link) ffmpeg(ctx context.Context, r run) error {
	cmd := exec.CommandContext(ctx, encode.FFmpeg, r.Args...)
	stdout := &lineWriter{line: func(line string) { l.send(msg{Out: &out{ID: r.ID, Line: line}}) }}
	stderr := &lineWriter{line: func(line string) { l.send(msg{Out: &out{ID: r.ID, Stderr: true, Line: line}}) }}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// Once ffmpeg is gone, don't wait long for anything it left holding its
	// output.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return err
	}
	l.mu.Lock()
	l.procs[r.ID] = cmd.Process
	l.mu.Unlock()
	err := cmd.Wait()
	l.mu.Lock()
	delete(l.procs, r.ID)
	l.mu.Unlock()
	stdout.flush()
	stderr.flush()
	if err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	return nil
}

// lineWriter hands on what is written to it a line at a time.
type lineWriter struct {
	line func(string)
	buf  []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		w.line(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
}

// flush hands on the last line, if it didn't end with a newline.
func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.line(string(w.buf))
		w.buf = nil
	}
}

func (l *link) signal(s signal) {
	l.mu.Lock()
	p := l.procs[s.ID]
	l.mu.Unlock()
	if p == nil {
		return
	}
	switch s.Name {
	case "stop":
		p.Signal(syscall.SIGSTOP)
	case "cont":
		p.Signal(syscall.SIGCONT)
	default:
		p.Signal(syscall.SIGCONT) // a stopped process can't act on being killed until it runs
		p.Kill()
	}
}

// allowed checks that every path a pass names is in a folder the worker
// allows, or is a GPU. The token is what keeps strangers out; this keeps a
// daemon from reaching further than its worker meant to let it.
func (l *link) allowed(args []string) error {
	if len(l.allow) == 0 {
		return nil
	}
	for _, a := range args {
		if !strings.HasPrefix(a, "/") || strings.HasPrefix(filepath.Clean(a), "/dev/dri/") {
			continue
		}
		ok := false
		for _, dir := range l.allow {
			if rel, err := filepath.Rel(dir, filepath.Clean(a)); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("this worker doesn't let passes use %s; see its -allow", a)
		}
	}
	return nil
}
