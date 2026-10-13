package remote

import (
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/haydenkz/cav1ar/internal/encode"
)

// Client is a daemon's queue as the UI sees it. It has the methods of
// encode.Queue the UI uses, so the UI can drive either.
type Client struct {
	conn   net.Conn
	wmu    sync.Mutex // one request at a time
	notify chan struct{}

	mu       sync.Mutex
	jobs     []encode.Job
	pending  []encode.Event
	settings encode.Settings
	paused   bool
	watching bool
	closed   bool
}

// Dial connects to the daemon, if one is running, and reads what it has.
func Dial() (*Client, error) {
	nc, err := net.Dial("unix", SocketPath())
	if err != nil {
		return nil, err
	}
	c := &Client{conn: nc, notify: make(chan struct{}, 1)}
	dec := json.NewDecoder(nc)
	var hello message
	nc.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err := dec.Decode(&hello); err != nil {
		nc.Close()
		return nil, fmt.Errorf("reading from the cav1ar daemon: %w", err)
	}
	nc.SetReadDeadline(time.Time{})
	c.apply(hello)
	go c.read(dec)
	return c, nil
}

func (c *Client) read(dec *json.Decoder) {
	for {
		var msg message
		if err := dec.Decode(&msg); err != nil {
			c.mu.Lock()
			c.closed = true
			c.mu.Unlock()
			c.signal()
			return
		}
		c.apply(msg)
	}
}

// apply folds a message from the daemon into the client's copy of the
// queue, and turns it into events for Next. What the daemon had when the
// client connected arrives as one event per job, then one per line of
// output, so the UI can treat it like any other news.
func (c *Client) apply(msg message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if msg.Settings != nil {
		c.settings = *msg.Settings
	}
	c.paused, c.watching = msg.Paused, msg.Watching
	for _, w := range msg.Jobs {
		j := w.job()
		c.jobs = append(c.jobs, j)
		c.pending = append(c.pending, encode.Event{Job: j})
		for _, line := range msg.Logs[j.ID] {
			c.pending = append(c.pending, encode.Event{Job: j, Line: line})
		}
	}
	for _, ev := range msg.Events {
		j := ev.Job.job()
		switch {
		case j.ID == len(c.jobs):
			c.jobs = append(c.jobs, j)
		case j.ID < len(c.jobs):
			c.jobs[j.ID] = j
		}
		c.pending = append(c.pending, encode.Event{Job: j, Line: ev.Line})
	}
	if len(c.pending) > 0 {
		c.signal()
	}
}

func (c *Client) signal() {
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

// Next blocks until there is news from the daemon and returns it. It
// returns nil once the connection is gone.
func (c *Client) Next() []encode.Event {
	for {
		c.mu.Lock()
		if len(c.pending) > 0 {
			evs := c.pending
			c.pending = nil
			c.mu.Unlock()
			return evs
		}
		closed := c.closed
		c.mu.Unlock()
		if closed {
			return nil
		}
		<-c.notify
	}
}

func (c *Client) send(r request) {
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.conn.Write(append(b, '\n')) // a lost connection shows up in read
}

func (c *Client) Add(path string) { c.send(request{Op: "add", Path: path}) }
func (c *Client) Cancel(id int)   { c.send(request{Op: "cancel", ID: id}) }
func (c *Client) Retry(id int)    { c.send(request{Op: "retry", ID: id}) }

func (c *Client) SetPaused(paused bool) {
	c.mu.Lock()
	c.paused = paused
	c.mu.Unlock()
	c.send(request{Op: "pause", Paused: paused})
}

func (c *Client) Paused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paused
}

// Watching reports whether the daemon is the cav1ar watching folders.
func (c *Client) Watching() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.watching
}

func (c *Client) Jobs() []encode.Job {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.jobs)
}

func (c *Client) Settings() encode.Settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.settings
}

// SetSettings shows s straight away. The daemon itself picks up settings
// from the file the UI saves them to.
func (c *Client) SetSettings(s encode.Settings) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.settings = s
}

// Daemon reports that the queue is a daemon's, which carries on once the UI
// closes.
func (c *Client) Daemon() bool { return true }

// Close disconnects from the daemon.
func (c *Client) Close() error { return c.conn.Close() }
