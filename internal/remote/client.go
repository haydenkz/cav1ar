package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/haydenkz/cav1ar/internal/encode"
)

// ErrNoDaemon is the error Dial gives when no daemon is running, as opposed
// to one that is but can't be shown.
var ErrNoDaemon = errors.New("no cav1ar daemon is running")

// errGone is why requests fail once the daemon has gone.
var errGone = errors.New("the cav1ar daemon has gone; reopen cav1ar once it's running again")

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
	devices  []encode.Device
	paused   bool
	holding  bool
	watching bool
	saved    int64
	results  int
	closed   bool
	err      error // why the latest request failed, until Err is called
}

// Dial connects to the daemon, if one is running, and reads what it has.
// With none running, the error is ErrNoDaemon.
func Dial() (*Client, error) {
	path := SocketPath()
	if err := checkOwner(path); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %w", ErrNoDaemon, err)
	} else if err != nil {
		return nil, err
	}
	nc, err := net.Dial("unix", path)
	if err != nil { // a daemon that stopped abruptly leaves its socket behind
		return nil, fmt.Errorf("%w: %w", ErrNoDaemon, err)
	}
	c := &Client{conn: nc, notify: make(chan struct{}, 1)}
	dec := json.NewDecoder(nc)
	var hello message
	nc.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err := dec.Decode(&hello); err != nil {
		nc.Close()
		return nil, fmt.Errorf("reading from the cav1ar daemon: %w", err)
	}
	if hello.Version != protocol {
		nc.Close()
		return nil, errors.New("the cav1ar daemon running is a different version from this cav1ar; " +
			"restart the daemon so it runs this one, or add -local to encode with a queue of the UI's own")
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
	if msg.Error != "" {
		c.err = errors.New(msg.Error)
	}
	c.paused, c.holding, c.watching = msg.Paused, msg.Holding, msg.Watching
	c.devices = msg.Devices
	c.saved, c.results = msg.Saved, msg.Results
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

// send makes a request of the daemon, reporting whether it went. When it
// didn't, Err says why.
func (c *Client) send(r request) bool {
	b, err := json.Marshal(r)
	if err == nil {
		c.wmu.Lock()
		if c.gone() {
			err = errGone
		} else if _, err = c.conn.Write(append(b, '\n')); err != nil {
			err = errGone
		}
		c.wmu.Unlock()
	}
	if err != nil {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		return false
	}
	return true
}

// gone reports whether the connection to the daemon has been lost.
func (c *Client) gone() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Err returns why the latest request failed, if one has since Err was last
// called: the daemon has gone, or it turned the request down.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.err
	c.err = nil
	return err
}

func (c *Client) Add(path string) { c.send(request{Op: "add", Path: path}) }
func (c *Client) Cancel(id int)   { c.send(request{Op: "cancel", ID: id}) }
func (c *Client) Retry(id int)    { c.send(request{Op: "retry", ID: id}) }

func (c *Client) SetPaused(paused bool) {
	if c.send(request{Op: "pause", Paused: paused}) {
		c.mu.Lock()
		c.paused = paused
		c.mu.Unlock()
	}
}

func (c *Client) Paused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paused
}

func (c *Client) SetHolding(holding bool) {
	if c.send(request{Op: "hold", Holding: holding}) {
		c.mu.Lock()
		c.holding = holding
		c.mu.Unlock()
	}
}

func (c *Client) Holding() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.holding
}

// Totals is what the daemon's watcher has recorded over every run.
func (c *Client) Totals() (saved int64, results int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saved, c.results
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

// Devices lists what the daemon encodes on.
func (c *Client) Devices() []encode.Device {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.devices)
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
