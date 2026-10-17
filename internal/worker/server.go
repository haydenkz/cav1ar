package worker

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/haydenkz/cav1ar/internal/encode"
)

const (
	// pendingLimit caps the connections that haven't yet proved they have
	// the token, so strangers can't tie the daemon up by opening many and
	// saying nothing. Each has handshakeTime to prove it.
	pendingLimit  = 16
	handshakeTime = 15 * time.Second

	// killWait is how long a pass that has been killed has to end on its
	// worker, before the worker is let go, which stops every pass on it.
	killWait = time.Minute

	nameLimit = 64
)

// Queue is the daemon's queue, as far as workers are concerned.
type Queue interface {
	Settings() encode.Settings
	Devices() []encode.Device
	SetWorkerDevices(name string, devices []encode.Device)
}

// Server takes workers for a daemon.
type Server struct {
	ln      net.Listener
	tls     *tls.Config
	token   string
	q       Queue
	pending chan struct{} // a slot for each connection still proving itself
	serving sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	conns   map[net.Conn]bool // every connection, joined or not
	workers map[string]*conn
}

// Listen starts taking workers on addr, such as ":7310", that have token.
func Listen(addr, token string, q Queue) (*Server, error) {
	if token == "" {
		return nil, errors.New("workers need a token to connect with")
	}
	cert, err := selfSigned()
	if err != nil {
		return nil, fmt.Errorf("making a certificate for workers' connections: %w", err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		ln:      ln,
		tls:     &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13},
		token:   token,
		q:       q,
		pending: make(chan struct{}, pendingLimit),
		conns:   map[net.Conn]bool{},
		workers: map[string]*conn{},
	}
	go s.accept()
	return s, nil
}

// selfSigned makes the certificate the daemon encrypts its connections with
// workers under. Workers don't check it, since the token is what proves the
// daemon is the daemon, so a new one each start does, with nothing to keep
// or renew.
func selfSigned() (tls.Certificate, error) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "cav1ar"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// Addr is the address the server listens on.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Close stops taking workers and lets go of those connected, returning once
// their devices have left the queue and the jobs on them have gone back in
// it.
func (s *Server) Close() {
	s.ln.Close()
	s.mu.Lock()
	s.closed = true
	for nc := range s.conns {
		nc.Close()
	}
	s.mu.Unlock()
	s.serving.Wait()
}

func (s *Server) accept() {
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			nc.Close()
			return
		}
		select {
		case s.pending <- struct{}{}:
		default: // too many proving themselves already
			s.mu.Unlock()
			nc.Close()
			continue
		}
		s.conns[nc] = true
		s.serving.Add(1)
		s.mu.Unlock()
		go s.serve(nc)
	}
}

// serve takes one worker, if it qualifies, and runs passes on it until it
// goes away.
func (s *Server) serve(nc net.Conn) {
	defer s.serving.Done()
	defer func() {
		s.mu.Lock()
		delete(s.conns, nc)
		s.mu.Unlock()
		nc.Close()
	}()
	from := nc.RemoteAddr().String()
	c, err := s.join(nc)
	if err != nil {
		log.Printf("cav1ar: a worker at %s couldn't join: %v", from, err)
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
	s.leave(c)
	log.Printf("cav1ar: worker %s left: %v", c.name, err)
}

// leave lets go of a worker that joined. Its devices leave the queue before
// its passes fail, so the jobs they belong to don't go straight back to it.
func (s *Server) leave(c *conn) {
	s.q.SetWorkerDevices(c.name, nil)
	encode.RemoveWorker(c.name)
	c.Close()
	s.forget(c)
	close(c.gone)
}

// forget takes c out of the workers, unless another has taken its place.
func (s *Server) forget(c *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workers[c.name] == c {
		delete(s.workers, c.name)
	}
}

// join has the worker on raw prove it has the token, proves the daemon
// has it too, then reads who the worker is and checks it can see the shared
// folder.
func (s *Server) join(raw net.Conn) (*conn, error) {
	proving := sync.OnceFunc(func() { <-s.pending })
	defer proving()

	raw.SetDeadline(time.Now().Add(handshakeTime))
	nc := tls.Server(raw, s.tls)
	if err := nc.Handshake(); err != nil {
		var old tls.RecordHeaderError
		if errors.As(err, &old) && old.Conn != nil && old.RecordHeader[0] == '{' {
			// A worker from before connections were encrypted, which sent
			// its hello as it is. Answer it so it can read why.
			why := "this daemon runs a newer cav1ar, which encrypts its connections with workers; update cav1ar on the worker, so both run the same version"
			json.NewEncoder(old.Conn).Encode(msg{Refused: why})
			return nil, errors.New("it runs an older cav1ar, from before connections were encrypted; update it")
		}
		return nil, fmt.Errorf("setting up encryption: %w", err)
	}
	in := newReader(nc, handshakeLimit)
	enc := json.NewEncoder(nc)
	refuse := func(why string) error {
		nc.SetWriteDeadline(time.Now().Add(5 * time.Second))
		enc.Encode(msg{Refused: why})
		return errors.New(why)
	}

	m, err := in.read()
	if err != nil || m.Hello == nil {
		return nil, errors.New("it didn't say hello")
	}
	switch {
	case m.Hello.Protocol != protocol:
		return nil, refuse(mismatch(m.Hello.Protocol, protocol))
	case len(m.Hello.Nonce) != nonceSize:
		return nil, refuse("the worker's hello has no nonce")
	}
	workerNonce, daemonNonce := m.Hello.Nonce, newNonce()
	if err := enc.Encode(msg{Hello: &hello{Protocol: protocol, Nonce: daemonNonce}}); err != nil {
		return nil, err
	}
	if m, err = in.read(); err != nil {
		return nil, err
	}
	cs := nc.ConnectionState()
	want, err := prove(s.token, "worker", cs, workerNonce, daemonNonce)
	if err != nil {
		return nil, err
	}
	if !hmac.Equal(m.Proof, want) {
		return nil, refuse("wrong token; copy the daemon's worker token to the worker")
	}
	proof, err := prove(s.token, "daemon", cs, workerNonce, daemonNonce)
	if err != nil {
		return nil, err
	}
	if err := enc.Encode(msg{Proof: proof}); err != nil {
		return nil, err
	}
	proving()
	in.limit = msgLimit

	if m, err = in.read(); err != nil {
		return nil, err
	}
	j := m.Join
	switch {
	case j == nil:
		return nil, refuse("the worker didn't say who it is")
	case !validName(j.Name):
		return nil, refuse(fmt.Sprintf("a worker's name is up to %d letters, digits, dots, dashes and underscores, which %.80q isn't; set one with -worker-name",
			nameLimit, j.Name))
	}
	dir := s.q.Settings().WorkerDir
	if dir == "" {
		return nil, refuse("this daemon has no folder to share with workers (worker_dir)")
	}
	c := &conn{nc: nc, raw: raw, name: j.Name, ffmpeg: j.FFmpeg, out: make(chan []byte, 256), runs: map[int]*pass{}, gone: make(chan struct{})}
	for _, d := range j.Devices {
		d.Worker = j.Name
		d.ID = j.Name + "/" + d.ID
		d.Name += " (" + j.Name + ")"
		c.devices = append(c.devices, d)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, refuse("the daemon is stopping")
	}
	old := s.workers[j.Name]
	s.workers[j.Name] = c
	s.mu.Unlock()
	ok := false
	defer func() {
		if !ok {
			s.forget(c)
			close(c.gone)
		}
	}()
	if old != nil {
		// Most likely the same worker, back after losing power or its
		// network, before the old connection was found dead.
		log.Printf("cav1ar: worker %s connected again, so letting its old connection go; if two workers have that name, give each its own with -worker-name", j.Name)
		old.raw.Close()
		<-old.gone
	}

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
	if m, err = in.read(); err != nil {
		return nil, err
	}
	if !m.Ready {
		return nil, errors.New(m.Error)
	}
	nc.SetDeadline(time.Time{})
	c.in = in
	go c.write()
	ok = true
	return c, nil
}

// validName reports whether name will do for a worker: short, and only
// letters, digits, dots, dashes and underscores, as host names are.
func validName(name string) bool {
	if name == "" || len(name) > nameLimit {
		return false
	}
	for _, r := range name {
		switch {
		case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9', r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// conn is one worker. It is the encode.Runner for its devices.
type conn struct {
	nc      *tls.Conn
	raw     net.Conn // under nc; closing it lets the worker go at once
	in      *reader
	name    string
	ffmpeg  string
	devices []encode.Device
	out     chan []byte   // messages waiting to be written
	gone    chan struct{} // closed once the worker has been let go

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

// FFmpegBuild is the build of ffmpeg the worker runs, so prerolls built on
// it are built again when it changes.
func (c *conn) FFmpegBuild() string { return c.ffmpeg }

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
		// Wait for the worker to say it has ended, so its files aren't
		// removed from under it. One that doesn't in time is let go, which
		// stops every pass it runs.
		select {
		case <-p.done:
		case <-time.After(killWait):
			log.Printf("cav1ar: worker %s didn't stop a pass within %s of being told to, so letting it go", c.name, killWait)
			c.raw.Close()
		}
		return ctx.Err()
	}
}

// read delivers what the worker sends to the passes it belongs to, until
// the worker goes away. Each pass's lines arrive in order, before its exit.
func (c *conn) read() error {
	for {
		m, err := c.in.read()
		if err != nil {
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
		c.raw.Close() // read then fails, which lets the worker go
	}
}

func (c *conn) write() {
	for b := range c.out {
		c.nc.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err := c.nc.Write(b); err != nil {
			c.raw.Close()
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
	c.raw.Close()
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
