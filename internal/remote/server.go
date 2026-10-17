package remote

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/haydenkz/cav1ar/internal/encode"
)

// Output kept for UIs that connect later.
const (
	logLines = 300 // per job
	logJobs  = 50  // jobs whose output is kept, the most recent ones
)

// Server shares a daemon's queue with UIs. The daemon is the one reading
// the queue's events, so it hands them over with Publish.
type Server struct {
	q  *encode.Queue
	w  *encode.Watcher
	ln net.Listener

	mu       sync.Mutex
	jobs     []encode.Job // as of the events published so far
	logs     map[int][]string
	logOrder []int // jobs with output kept, oldest first
	settings encode.Settings
	clients  map[*conn]bool
}

// conn is one connected UI.
type conn struct {
	net.Conn
	out chan []byte // messages waiting to be written
}

// Serve starts sharing q, whose folders w watches.
func Serve(q *encode.Queue, w *encode.Watcher) (*Server, error) {
	path := SocketPath()
	switch err := checkOwner(path); {
	case err == nil:
		if c, err := net.Dial("unix", path); err == nil {
			c.Close()
			return nil, errors.New("another cav1ar daemon is already listening at " + path)
		}
		os.Remove(path) // left behind by a daemon that stopped abruptly
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	s := &Server{q: q, w: w, ln: ln, logs: map[int][]string{}, settings: q.Settings(), clients: map[*conn]bool{}}
	go s.accept()
	return s, nil
}

// Close stops listening and disconnects every UI.
func (s *Server) Close() {
	s.ln.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		s.dropLocked(c)
	}
}

func (s *Server) accept() {
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return
		}
		c := &conn{Conn: nc, out: make(chan []byte, 1024)}
		s.mu.Lock()
		jobs := make([]job, len(s.jobs))
		for i, j := range s.jobs {
			jobs[i] = toWire(j)
		}
		settings := s.settings
		s.queueLocked(c, message{Version: protocol, Jobs: jobs, Logs: s.logs, Settings: &settings})
		s.clients[c] = true
		s.mu.Unlock()
		go s.write(c)
		go s.read(c)
	}
}

func (s *Server) write(c *conn) {
	for b := range c.out {
		if _, err := c.Write(b); err != nil {
			s.drop(c)
			return
		}
	}
}

// read carries out what a UI asks for, telling it why when it can't. A UI
// that sends more than a request could be is disconnected.
func (s *Server) read(c *conn) {
	sc := bufio.NewScanner(c)
	sc.Buffer(nil, maxRequest)
	for sc.Scan() {
		var r request
		err := json.Unmarshal(sc.Bytes(), &r)
		if err == nil {
			err = s.do(r)
		} else {
			err = fmt.Errorf("the cav1ar daemon can't read that request: %w", err)
		}
		if err != nil {
			s.tell(c, err)
		}
	}
	s.drop(c)
}

// do carries out a request, or says why it can't.
func (s *Server) do(r request) error {
	switch r.Op {
	case "add":
		if !filepath.IsAbs(r.Path) {
			return fmt.Errorf("the cav1ar daemon needs a full path, not %q, since it doesn't know which folder that's in", r.Path)
		}
		s.q.Add(r.Path)
	case "cancel":
		s.q.Cancel(r.ID)
	case "retry":
		s.q.Retry(r.ID)
	case "pause":
		s.q.SetPaused(r.Paused)
		s.mu.Lock()
		s.sendLocked(message{}) // tell every UI
		s.mu.Unlock()
	case "hold":
		s.q.SetHolding(r.Holding)
		s.mu.Lock()
		s.sendLocked(message{})
		s.mu.Unlock()
	default:
		return fmt.Errorf("the cav1ar daemon doesn't know how to %q, so it must be a different version; restart it", r.Op)
	}
	return nil
}

// tell sends one UI why the daemon didn't do what it asked, unless the UI
// has gone.
func (s *Server) tell(c *conn, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clients[c] {
		s.queueLocked(c, message{Error: err.Error()})
	}
}

// Publish passes the queue's events on to every UI.
func (s *Server) Publish(evs []encode.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg := message{Events: make([]event, len(evs))}
	for i, ev := range evs {
		s.recordLocked(ev)
		msg.Events[i] = event{Job: toWire(ev.Job), Line: ev.Line}
	}
	s.sendLocked(msg)
}

// SetSettings tells every UI the daemon's settings changed.
func (s *Server) SetSettings(settings encode.Settings) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings = settings
	s.sendLocked(message{Settings: &settings})
}

// recordLocked keeps what a UI connecting later needs to catch up.
func (s *Server) recordLocked(ev encode.Event) {
	j := ev.Job
	switch {
	case j.ID == len(s.jobs):
		s.jobs = append(s.jobs, j)
	case j.ID < len(s.jobs):
		if s.jobs[j.ID].State.Finished() && j.State == encode.Queued { // retried: its output starts over
			delete(s.logs, j.ID)
			s.logOrder = slices.DeleteFunc(s.logOrder, func(id int) bool { return id == j.ID })
		}
		s.jobs[j.ID] = j
	}
	if ev.Line == "" {
		return
	}
	if _, ok := s.logs[j.ID]; !ok {
		s.logOrder = append(s.logOrder, j.ID)
		if len(s.logOrder) > logJobs {
			delete(s.logs, s.logOrder[0])
			s.logOrder = s.logOrder[1:]
		}
	}
	s.logs[j.ID] = encode.TrimLog(append(s.logs[j.ID], ev.Line), logLines)
}

func (s *Server) sendLocked(msg message) {
	for c := range s.clients {
		s.queueLocked(c, msg)
	}
}

// queueLocked queues a message for one UI, adding the state every message
// carries. A UI that falls far behind is disconnected rather than allowed to
// hold up the daemon.
func (s *Server) queueLocked(c *conn, msg message) {
	msg.Paused, msg.Holding, msg.Watching = s.q.Paused(), s.q.Holding(), s.w.Active()
	msg.Devices = s.q.Devices()
	msg.Saved, msg.Results = s.w.Totals()
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	select {
	case c.out <- append(b, '\n'):
	default:
		s.dropLocked(c)
	}
}

func (s *Server) drop(c *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropLocked(c)
}

func (s *Server) dropLocked(c *conn) {
	c.Close()
	if s.clients[c] {
		delete(s.clients, c)
		close(c.out)
	}
}
