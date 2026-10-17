package worker

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/tls"
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
	Allow   []string        // folders passes may read and write, as this worker sees them; empty allows any
	Paths   []PathMap       // where folders the daemon names are on this worker, if not at the same paths
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

// PathMap says a folder the daemon sees at From is at To on the worker.
type PathMap struct{ From, To string }

// ParsePathMap reads a folder mapping written as daemon-path=worker-path.
func ParsePathMap(s string) (PathMap, error) {
	from, to, ok := strings.Cut(s, "=")
	from, to = filepath.Clean(strings.TrimSpace(from)), filepath.Clean(strings.TrimSpace(to))
	if !ok || !filepath.IsAbs(from) || !filepath.IsAbs(to) {
		return PathMap{}, fmt.Errorf("%q isn't daemon-path=worker-path, both full paths", s)
	}
	return PathMap{from, to}, nil
}

// mapPath turns a path as the daemon sees it into the same path on this
// worker, by the longest mapping that takes it in. A path no mapping takes
// in is left as it is.
func mapPath(maps []PathMap, p string) string {
	best := -1
	for i, m := range maps {
		under := p == m.From || strings.HasPrefix(p, strings.TrimSuffix(m.From, "/")+"/")
		if under && (best < 0 || len(m.From) > len(maps[best].From)) {
			best = i
		}
	}
	if best < 0 {
		return p
	}
	m := maps[best]
	return filepath.Join(m.To, strings.TrimPrefix(p, m.From))
}

// connect is one connection to the daemon, which lasts until either side
// goes away. Passes still running when it ends are stopped.
func connect(ctx context.Context, o Options) error {
	// Running ffmpeg to see which it is can take a moment, which the daemon
	// won't wait for once connected.
	encode.FFmpegBuild()
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", o.Addr)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { <-ctx.Done(); raw.Close() }()

	nc, in, err := joinDaemon(raw, o)
	if err != nil {
		return err
	}
	o.Log("cav1ar: working for the daemon at %s", o.Addr)
	l := &link{nc: nc, raw: raw, enc: json.NewEncoder(nc), procs: map[int]*proc{}, allow: resolveAll(o.Allow), paths: o.Paths}

	var passes sync.WaitGroup
	defer passes.Wait()
	defer cancel() // stops every pass, before waiting for them
	for {
		m, err := in.read()
		if err != nil {
			return fmt.Errorf("lost the daemon: %w", err)
		}
		switch {
		case m.Run != nil:
			r := *m.Run
			l.begin(r.ID)
			passes.Go(func() { l.run(ctx, r) })
		case m.Signal != nil:
			l.signal(*m.Signal)
		}
	}
}

// joinDaemon proves to the daemon on raw that this worker has its token,
// has the daemon prove it has it too, then joins it, once this worker sees
// the folder they share. It returns the connection, and what reads from it.
func joinDaemon(raw net.Conn, o Options) (*tls.Conn, *reader, error) {
	raw.SetDeadline(time.Now().Add(handshakeTime))
	// The daemon's certificate is one it made when it started, which there's
	// nothing to check against. Its proof below is what shows it's the
	// daemon, and that no one sits between the two.
	nc := tls.Client(raw, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	if err := nc.Handshake(); err != nil {
		return nil, nil, fmt.Errorf("couldn't set up an encrypted connection with the daemon (%v); "+
			"if it runs an older cav1ar, update it and its workers to the same version", err)
	}
	in := newReader(nc, handshakeLimit)
	enc := json.NewEncoder(nc)
	refused := func(m msg, err error) error {
		switch {
		case err != nil:
			return fmt.Errorf("the daemon hung up: %w", err)
		case m.Refused != "":
			return errors.New("the daemon refused: " + m.Refused)
		}
		return nil
	}

	workerNonce := newNonce()
	if err := enc.Encode(msg{Hello: &hello{Protocol: protocol, Nonce: workerNonce}}); err != nil {
		return nil, nil, err
	}
	m, err := in.read()
	if err := refused(m, err); err != nil {
		return nil, nil, err
	}
	switch {
	case m.Hello == nil:
		return nil, nil, errors.New("the daemon didn't say hello")
	case m.Hello.Protocol != protocol:
		return nil, nil, errors.New(mismatch(protocol, m.Hello.Protocol))
	case len(m.Hello.Nonce) != nonceSize:
		return nil, nil, errors.New("the daemon's hello has no nonce")
	}
	daemonNonce := m.Hello.Nonce
	cs := nc.ConnectionState()
	proof, err := prove(o.Token, "worker", cs, workerNonce, daemonNonce)
	if err != nil {
		return nil, nil, err
	}
	if err := enc.Encode(msg{Proof: proof}); err != nil {
		return nil, nil, err
	}
	m, err = in.read()
	if err := refused(m, err); err != nil {
		return nil, nil, err
	}
	want, err := prove(o.Token, "daemon", cs, workerNonce, daemonNonce)
	if err != nil {
		return nil, nil, err
	}
	if !hmac.Equal(m.Proof, want) {
		return nil, nil, fmt.Errorf("what answered at %s couldn't prove it has the daemon's token, so this worker won't work for it", o.Addr)
	}
	raw.SetDeadline(time.Time{})
	in.limit = msgLimit

	raw.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if err := enc.Encode(msg{Join: &join{Name: o.Name, FFmpeg: encode.FFmpegBuild(), Devices: o.Devices}}); err != nil {
		return nil, nil, err
	}
	m, err = in.read()
	if err := refused(m, err); err != nil {
		return nil, nil, err
	}
	if m.Welcome == nil {
		return nil, nil, errors.New("the daemon didn't welcome this worker")
	}
	m.Welcome.WorkDir = mapPath(o.Paths, m.Welcome.WorkDir)
	err = checkShared(m.Welcome)
	raw.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if err != nil {
		enc.Encode(msg{Error: err.Error()})
		return nil, nil, err
	}
	if err := enc.Encode(msg{Ready: true}); err != nil {
		return nil, nil, err
	}
	raw.SetWriteDeadline(time.Time{})
	return nc, in, nil
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
	raw   net.Conn // under nc; closing it ends the connection at once
	allow []string // with their symlinks followed
	paths []PathMap

	wmu sync.Mutex // one message at a time
	enc *json.Encoder

	mu    sync.Mutex
	procs map[int]*proc // passes, by ID, from when the daemon asks for them until they end
}

// proc is a pass on this worker. Signals that arrive before its ffmpeg has
// started are kept until it has.
type proc struct {
	p       *os.Process // nil until it has started
	stopped bool        // paused
	killed  bool        // ended, so it is never started
}

// send writes m to the daemon. A write that fails closes the connection,
// which stops every pass and has the daemon put their jobs back in the
// queue, rather than leave each line to wait out the deadline in turn.
func (l *link) send(m msg) error {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	l.nc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	err := l.enc.Encode(m)
	if err != nil {
		l.raw.Close()
	}
	return err
}

// begin notes a pass the daemon has asked for, so signals for it are kept
// until it has started.
func (l *link) begin(id int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.procs[id] = &proc{}
}

// run runs one pass, sending what it writes as it goes, then how it ended.
func (l *link) run(ctx context.Context, r run) {
	defer func() {
		l.mu.Lock()
		delete(l.procs, r.ID)
		l.mu.Unlock()
	}()
	args, err := l.passArgs(r.Args)
	if err == nil {
		err = l.ffmpeg(ctx, r.ID, args)
	}
	e := &exit{ID: r.ID}
	if err != nil {
		e.Err = err.Error()
	}
	l.send(msg{Exit: e})
}

func (l *link) ffmpeg(ctx context.Context, id int, args []string) error {
	cmd := exec.CommandContext(ctx, encode.FFmpeg, args...)
	stdout := &lineWriter{line: func(line string) { l.send(msg{Out: &out{ID: id, Line: line}}) }}
	stderr := &lineWriter{line: func(line string) { l.send(msg{Out: &out{ID: id, Stderr: true, Line: line}}) }}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// Once ffmpeg is gone, don't wait long for anything it left holding its
	// output.
	cmd.WaitDelay = 5 * time.Second
	// Start and act on any signals that came first, with none coming in
	// between.
	l.mu.Lock()
	p := l.procs[id]
	if p == nil {
		p = &proc{}
		l.procs[id] = p
	}
	if p.killed {
		l.mu.Unlock()
		return errors.New("ffmpeg: killed before it started")
	}
	if err := cmd.Start(); err != nil {
		l.mu.Unlock()
		return err
	}
	p.p = cmd.Process
	if p.stopped {
		p.p.Signal(syscall.SIGSTOP)
	}
	l.mu.Unlock()
	err := cmd.Wait()
	l.mu.Lock()
	p.p = nil
	l.mu.Unlock()
	stdout.flush()
	stderr.flush()
	if err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	return nil
}

// lineWriter hands on what is written to it a line at a time. A line longer
// than lineLimit is handed on in pieces, so each fits in a message.
type lineWriter struct {
	line func(string)
	buf  []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		switch i := bytes.IndexByte(w.buf, '\n'); {
		case i >= 0 && i <= lineLimit:
			w.line(string(w.buf[:i]))
			w.buf = w.buf[i+1:]
		case len(w.buf) >= lineLimit:
			w.line(string(w.buf[:lineLimit]))
			w.buf = w.buf[lineLimit:]
		default:
			return len(p), nil
		}
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
	defer l.mu.Unlock()
	p := l.procs[s.ID]
	if p == nil {
		return // it has ended
	}
	switch s.Name {
	case "stop":
		p.stopped = true
	case "cont":
		p.stopped = false
	default:
		p.killed = true
	}
	if p.p == nil {
		return // ffmpeg acts on it once it starts
	}
	switch s.Name {
	case "stop":
		p.p.Signal(syscall.SIGSTOP)
	case "cont":
		p.p.Signal(syscall.SIGCONT)
	default:
		p.p.Signal(syscall.SIGCONT) // a stopped process can't act on being killed until it runs
		p.p.Kill()
	}
}
