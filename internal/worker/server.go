package worker

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/haydenkz/cav1ar/internal/encode"
)

// Queue is the daemon's queue, as far as workers are concerned.
type Queue interface {
	Settings() encode.Settings
	Devices() []encode.Device
	SetWorkerDevices(name string, devices []encode.Device)
}

// Server takes workers for a daemon.
type Server struct {
	ln    net.Listener
	token string
	q     Queue

	mu      sync.Mutex
	workers map[string]*conn
}

// Listen starts taking workers on addr, such as ":7310", that have token.
func Listen(addr, token string, q Queue) (*Server, error) {
	if token == "" {
		return nil, errors.New("workers need a token to connect with")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln, token: token, q: q, workers: map[string]*conn{}}
	go s.accept()
	return s, nil
}

// Addr is the address the server listens on.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Close stops taking workers and lets go of those connected. Their
// devices leave the queue, and jobs on them go back in it.
func (s *Server) Close() {
	s.ln.Close()
	s.mu.Lock()
	conns := make([]*conn, 0, len(s.workers))
	for _, c := range s.workers {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

func (s *Server) accept() {
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.serve(nc)
	}
}

// serve takes one worker, if it qualifies, and runs passes on it until it
// goes away.
func (s *Server) serve(nc net.Conn) {
	from := nc.RemoteAddr().String()
	c, err := s.join(nc)
	if err != nil {
		log.Printf("cav1ar: a worker at %s couldn't join: %v", from, err)
		nc.Close()
		return
	}
	var names []string
	for _, d := range c.devices {
		names = append(names, d.Name)
	}
	log.Printf("cav1ar: worker %s joined from %s, with %s", c.name, from, strings.Join(names, ", "))
	encode.AddWorker(c.name, c)
	s.q.SetWorkerDevices(c.name, c.devices)
	log.Printf("cav1ar: encoding on %s", s.q.Settings().DeviceSummary(s.q.Devices()))

	err = c.read()

	// Take its devices out of the queue before failing its passes, so the
	// jobs they belong to don't go straight back to it.
	s.q.SetWorkerDevices(c.name, nil)
	encode.RemoveWorker(c.name)
	c.Close()
	s.mu.Lock()
	delete(s.workers, c.name)
	s.mu.Unlock()
	log.Printf("cav1ar: worker %s left: %v", c.name, err)
}

// join reads the worker's hello and checks it can see the shared folder.
func (s *Server) join(nc net.Conn) (*conn, error) {
	dec := json.NewDecoder(nc)
	enc := json.NewEncoder(nc)
	refuse := func(why string) error {
		nc.SetWriteDeadline(time.Now().Add(5 * time.Second))
		enc.Encode(msg{Refused: why})
		return errors.New(why)
	}

	nc.SetReadDeadline(time.Now().Add(15 * time.Second))
	var m msg
	if err := dec.Decode(&m); err != nil || m.Hello == nil {
		return nil, errors.New("it didn't say hello")
	}
	h := m.Hello
	switch {
	case h.Protocol != protocol:
		return nil, refuse(fmt.Sprintf("the worker speaks version %d of the protocol, and this daemon %d; run the same cav1ar on both", h.Protocol, protocol))
	case subtle.ConstantTimeCompare([]byte(h.Token), []byte(s.token)) != 1:
		return nil, refuse("wrong token; copy the daemon's worker token to the worker")
	case h.Name == "" || strings.ContainsAny(h.Name, "/ "):
		return nil, refuse(fmt.Sprintf("a worker's name can't be empty or have spaces or slashes, as %q has", h.Name))
	}
	dir := s.q.Settings().WorkerDir
	if dir == "" {
		return nil, refuse("this daemon has no folder to share with workers (worker_dir)")
	}
	c := &conn{nc: nc, name: h.Name, out: make(chan []byte, 256), runs: map[int]*pass{}}
	for _, d := range h.Devices {
		d.Worker = h.Name
		d.ID = h.Name + "/" + d.ID
		d.Name += " on " + h.Name
		c.devices = append(c.devices, d)
	}
	s.mu.Lock()
	_, taken := s.workers[h.Name]
	if !taken {
		s.workers[h.Name] = c
	}
	s.mu.Unlock()
	if taken {
		return nil, refuse(fmt.Sprintf("a worker called %s is already connected; give each its own name", h.Name))
	}
	ok := false
	defer func() {
		if !ok {
			s.mu.Lock()
			delete(s.workers, h.Name)
			s.mu.Unlock()
		}
	}()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, refuse("the daemon can't make the shared folder: " + err.Error())
	}
	check, err := os.CreateTemp(dir, ".cav1ar-worker-check-*")
	if err != nil {
		return nil, refuse("the daemon can't write to the shared folder: " + err.Error())
	}
	check.Close()
	defer os.Remove(check.Name())
	nc.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := enc.Encode(msg{Welcome: &welcome{WorkDir: dir, Check: filepath.Base(check.Name())}}); err != nil {
		return nil, err
	}
	nc.SetReadDeadline(time.Now().Add(30 * time.Second))
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if !m.Ready {
		return nil, errors.New(m.Error)
	}
	nc.SetDeadline(time.Time{})
	c.dec = dec
	go c.write()
	ok = true
	return c, nil
}

// conn is one worker. It is the encode.Runner for its devices.
type conn struct {
	nc      net.Conn
	dec     *json.Decoder
	name    string
	devices []encode.Device
	out     chan []byte // messages waiting to be written

	mu     sync.Mutex
	runs   map[int]*pass
	next   int
	closed bool
}

// pass is a pass running on the worker.
type pass struct {
	out  encode.Output
	done chan error
}

// Run runs a pass on the worker.
func (c *conn) Run(ctx context.Context, args []string, o encode.Output) error {
	p := &pass{out: o, done: make(chan error, 1)}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return encode.ErrWorkerGone
	}
	id := c.next
	c.next++
	c.runs[id] = p
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.runs, id)
		c.mu.Unlock()
	}()

	c.send(msg{Run: &run{ID: id, Args: args}})
	if o.Started != nil {
		o.Started(process{c, id})
	}
	select {
	case err := <-p.done:
		return err
	case <-ctx.Done():
		c.send(msg{Signal: &signal{ID: id, Name: "kill"}})
		select { // let it clean up, so its files aren't removed from under it
		case <-p.done:
		case <-time.After(30 * time.Second):
		}
		return ctx.Err()
	}
}

// read delivers what the worker sends to the passes it belongs to, until
// the worker goes away. Each pass's lines arrive in order, before its exit.
func (c *conn) read() error {
	for {
		var m msg
		if err := c.dec.Decode(&m); err != nil {
			return err
		}
		switch {
		case m.Out != nil:
			if p := c.pass(m.Out.ID); p != nil {
				line := p.out.Stdout
				if m.Out.Stderr {
					line = p.out.Stderr
				}
				if line != nil {
					line(m.Out.Line)
				}
			}
		case m.Exit != nil:
			if p := c.pass(m.Exit.ID); p != nil {
				var err error
				if m.Exit.Err != "" {
					err = errors.New(m.Exit.Err)
				}
				select {
				case p.done <- err:
				default: // already failed, as the worker was let go
				}
			}
		case m.Error != "":
			return errors.New(m.Error)
		}
	}
}

func (c *conn) pass(id int) *pass {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs[id]
}

// send queues a message for the worker. One that falls far behind is let go
// rather than allowed to hold up the daemon.
func (c *conn) send(m msg) {
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	select {
	case c.out <- append(b, '\n'):
	default:
		c.nc.Close() // read then fails, which lets the worker go
	}
}

func (c *conn) write() {
	for b := range c.out {
		c.nc.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err := c.nc.Write(b); err != nil {
			c.nc.Close()
		}
	}
}

// Close lets the worker go, failing its passes with encode.ErrWorkerGone.
func (c *conn) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	close(c.out)
	c.nc.Close()
	for _, p := range c.runs {
		select {
		case p.done <- encode.ErrWorkerGone:
		default:
		}
	}
}

// process is a pass on a worker, as pausing sees it.
type process struct {
	c  *conn
	id int
}

func (p process) Signal(sig os.Signal) error {
	name := "kill"
	switch sig {
	case syscall.SIGSTOP:
		name = "stop"
	case syscall.SIGCONT:
		name = "cont"
	}
	p.c.send(msg{Signal: &signal{ID: p.id, Name: name}})
	return nil
}
