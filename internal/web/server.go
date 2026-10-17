// Package web serves cav1ar's web UI: a page showing a daemon's queue,
// history, encoders and settings, and the JSON API behind it. Changes reach
// open pages as they happen, as server-sent events.
package web

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/haydenkz/cav1ar/internal/encode"
)

//go:embed static
var static embed.FS

// Output kept for pages that open a job's log, as the daemon keeps it for
// the terminal UI.
const (
	logLines = 300 // per job
	logJobs  = 50  // jobs whose output is kept, the most recent ones
)

// pushEvery is how often changes are sent to open pages, which batches the
// many progress updates a running job makes.
const pushEvery = 500 * time.Millisecond

// Options say what the web UI shows and how it changes things.
type Options struct {
	Addr      string // to listen on, such as ":6767"
	Queue     *encode.Queue
	Watcher   *encode.Watcher
	StatePath string                      // the state database, which holds the history
	KeyPath   string                      // where the key that signs logins is kept
	Apply     func(encode.Settings) error // saves settings and puts them into effect
	Version   string
}

// Server serves the web UI for a daemon. The daemon reads the queue's
// events, so it hands them over with Publish.
type Server struct {
	o       Options
	addr    string
	http    *http.Server
	key     []byte
	started time.Time
	history *encode.History // nil if the state database can't be read

	// Changes to the settings are made one at a time, so two pages saving at
	// once can't undo each other; logins are tried one at a time, to slow
	// guessing.
	settingsMu sync.Mutex
	loginMu    sync.Mutex

	mu       sync.Mutex
	logs     map[int][]string
	logOrder []int                         // jobs with output kept, oldest first
	states   map[int]encode.State          // each job's state as last published
	changed  map[int]encode.Job            // jobs changed since the last push
	status   []byte                        // the status last pushed, so it's sent only when it changes
	clients  map[chan []byte]*http.Request // open event streams, with the requests that opened them
	done     chan struct{}
}

// Listen starts serving the web UI.
func Listen(o Options) (*Server, error) {
	key, err := loadKey(o.KeyPath)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", o.Addr)
	if err != nil {
		return nil, err
	}
	s := &Server{o: o, addr: ln.Addr().String(), key: key, started: time.Now(), logs: map[int][]string{},
		states: map[int]encode.State{}, changed: map[int]encode.Job{}, clients: map[chan []byte]*http.Request{}, done: make(chan struct{})}
	if h, err := encode.OpenHistory(o.StatePath); err == nil {
		s.history = h
	}

	mux := http.NewServeMux()
	assets, _ := fs.Sub(static, "static")
	mux.Handle("GET /", http.FileServerFS(assets))
	s.routes(mux)
	// No write timeout: the event stream stays open, and sets its own for
	// each write.
	s.http = &http.Server{Handler: s.guard(mux), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute,
		MaxHeaderBytes: 64 << 10}
	go s.http.Serve(ln)
	go s.push()
	return s, nil
}

// Addr is the address the server listens on.
func (s *Server) Addr() string { return s.addr }

// Close stops serving.
func (s *Server) Close() {
	close(s.done)
	s.http.Close()
	if s.history != nil {
		s.history.Close()
	}
}

// Publish takes in the queue's events: jobs that changed, for open pages,
// and lines of output, for job logs.
func (s *Server) Publish(evs []encode.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range evs {
		id := ev.Job.ID
		if before, ok := s.states[id]; ok && before.Finished() && ev.Job.State == encode.Queued {
			delete(s.logs, id) // retried: its output starts over
			s.logOrder = slices.DeleteFunc(s.logOrder, func(j int) bool { return j == id })
		}
		s.states[id] = ev.Job.State
		s.changed[id] = ev.Job
		if ev.Line == "" {
			continue
		}
		if _, ok := s.logs[id]; !ok {
			s.logOrder = append(s.logOrder, id)
			if len(s.logOrder) > logJobs {
				delete(s.logs, s.logOrder[0])
				s.logOrder = s.logOrder[1:]
			}
		}
		s.logs[id] = encode.TrimLog(append(s.logs[id], ev.Line), logLines)
	}
}

// push sends open pages what changed, every pushEvery.
func (s *Server) push() {
	tick := time.NewTicker(pushEvery)
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-tick.C:
		}
		status, _ := json.Marshal(s.statusNow())
		hash := s.o.Queue.Settings().WebPassword
		s.mu.Lock()
		var msgs [][]byte
		if len(s.changed) > 0 {
			jobs := make([]job, 0, len(s.changed))
			for _, j := range s.changed {
				jobs = append(jobs, toWire(j))
			}
			slices.SortFunc(jobs, func(a, b job) int { return a.ID - b.ID })
			s.changed = map[int]encode.Job{}
			msgs = append(msgs, event("jobs", jobs))
		}
		if string(status) != string(s.status) {
			s.status = status
			msgs = append(msgs, eventRaw("status", status))
		}
		for c, r := range s.clients {
			// A stream opened before a password was set, or changed, ends,
			// and the page asks to log in.
			if hash != "" && !s.loggedIn(r, hash) {
				delete(s.clients, c)
				close(c)
				continue
			}
			for _, m := range msgs {
				select {
				case c <- m:
					continue
				default:
				}
				// A page that can't keep up is dropped, and reconnects.
				delete(s.clients, c)
				close(c)
				break
			}
		}
		s.mu.Unlock()
	}
}

// events streams what changes to a page: everything first, then changes.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	c := make(chan []byte, 64)
	s.mu.Lock()
	s.clients[c] = r
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if _, ok := s.clients[c]; ok {
			delete(s.clients, c)
			close(c)
		}
		s.mu.Unlock()
	}()

	// A page that stops reading is let go, rather than holding on forever.
	rc := http.NewResponseController(w)
	send := func(m []byte) bool {
		rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err := w.Write(m); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !send(event("snapshot", s.snapshot())) {
		return
	}
	alive := time.NewTicker(25 * time.Second)
	defer alive.Stop()
	for {
		var m []byte
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case msg, ok := <-c:
			if !ok {
				return
			}
			m = msg
		case <-alive.C:
			m = []byte(": still here\n\n")
		}
		if !send(m) {
			return
		}
	}
}

func event(name string, v any) []byte {
	data, _ := json.Marshal(v)
	return eventRaw(name, data)
}

func eventRaw(name string, data []byte) []byte {
	return []byte("event: " + name + "\ndata: " + string(data) + "\n\n")
}

// job is an encode.Job as pages see it.
type job struct {
	ID       int     `json:"id"`
	Input    string  `json:"input"`
	Output   string  `json:"output,omitempty"`
	Action   string  `json:"action,omitempty"`
	Step     string  `json:"step,omitempty"`
	Device   string  `json:"device,omitempty"`
	State    string  `json:"state"`
	Next     bool    `json:"next,omitempty"`
	Priority int     `json:"priority,omitempty"`
	Progress float64 `json:"progress"`
	FPS      float64 `json:"fps,omitempty"`
	Speed    float64 `json:"speed,omitempty"`
	ETA      float64 `json:"eta,omitempty"`      // seconds
	Elapsed  float64 `json:"elapsed,omitempty"`  // seconds
	Duration float64 `json:"duration,omitempty"` // of the video, in seconds
	InSize   int64   `json:"in_size,omitempty"`
	OutSize  int64   `json:"out_size,omitempty"`
	Started  int64   `json:"started,omitempty"` // Unix seconds
	Finished int64   `json:"finished,omitempty"`
	Err      string  `json:"error,omitempty"`
	Note     string  `json:"note,omitempty"`
}

func toWire(j encode.Job) job {
	w := job{ID: j.ID, Input: j.Input, Output: j.Output, Action: string(j.Action), Step: j.Step, Device: j.Device,
		State: j.State.String(), Next: j.Next, Priority: j.Priority, Progress: j.Progress, FPS: j.FPS, Speed: j.Speed,
		ETA: j.ETA().Seconds(), Elapsed: j.Elapsed().Seconds(), Duration: j.Duration.Seconds(),
		InSize: j.InSize, OutSize: j.OutSize, Note: j.Note}
	if !j.Started.IsZero() {
		w.Started = j.Started.Unix()
	}
	if !j.Finished.IsZero() {
		w.Finished = j.Finished.Unix()
	}
	if j.Err != nil {
		w.Err = j.Err.Error()
	}
	return w
}

// status is the state of the daemon as a whole.
type status struct {
	Version     string          `json:"version"`
	Started     int64           `json:"started"`
	Paused      bool            `json:"paused"`
	Holding     bool            `json:"holding"`
	Watching    bool            `json:"watching"`
	Folders     int             `json:"folders"` // how many are watched
	Saved       int64           `json:"saved"`
	Results     int             `json:"results"`
	Devices     []encode.Device `json:"devices"`
	Slots       map[string]int  `json:"slots"` // how many at once each device runs, by ID
	PasswordSet bool            `json:"password_set"`
}

func (s *Server) statusNow() status {
	q := s.o.Queue
	settings, devices := q.Settings(), q.Devices()
	st := status{Version: s.o.Version, Started: s.started.Unix(), Paused: q.Paused(), Holding: q.Holding(), Folders: len(settings.Watch),
		Devices: devices, Slots: map[string]int{}, PasswordSet: settings.WebPassword != ""}
	if s.o.Watcher != nil {
		st.Watching = s.o.Watcher.Active()
		st.Saved, st.Results = s.o.Watcher.Totals()
	}
	for _, d := range devices {
		st.Slots[d.ID] = settings.Slots(d, devices)
	}
	return st
}

// keepFinished is how many of the jobs done or canceled since the daemon
// started a page is sent when it opens, the most recent ones; the history
// has the rest.
const keepFinished = 200

// snapshot is everything a page needs to start.
func (s *Server) snapshot() any {
	jobs := s.o.Queue.Jobs()
	over := -keepFinished // failed jobs are all sent, as they want seeing to
	for _, j := range jobs {
		if j.State == encode.Done || j.State == encode.Canceled {
			over++
		}
	}
	wire := make([]job, 0, len(jobs))
	for _, j := range jobs {
		if over > 0 && (j.State == encode.Done || j.State == encode.Canceled) {
			over--
			continue
		}
		wire = append(wire, toWire(j))
	}
	return struct {
		Status status `json:"status"`
		Jobs   []job  `json:"jobs"`
	}{s.statusNow(), wire}
}

// errorJSON writes an error as the API returns them.
func errorJSON(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

var errNoHistory = errors.New("the history can't be read: this cav1ar isn't the one keeping it")
