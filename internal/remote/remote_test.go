package remote

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/haydenkz/cav1ar/internal/encode"
)

// serve starts sharing a queue of its own, whose jobs stay queued, with its
// socket in a folder of the test's.
func serve(t *testing.T) (*Server, *encode.Queue) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	q := encode.NewQueueOn(ctx, encode.DefaultSettings(), []encode.Device{{ID: "cpu", Kind: encode.EncoderCPU, Name: "CPU"}})
	q.SetHolding(true)
	srv, err := Serve(q, new(encode.Watcher))
	if err != nil {
		t.Fatal(err)
	}
	go func() { // as the daemon hands the queue's events over
		for evs := q.Next(); evs != nil; evs = q.Next() {
			srv.Publish(evs)
		}
	}()
	t.Cleanup(func() { srv.Close(); cancel(); q.Close() })
	return srv, q
}

// turnedDown waits for the daemon to say why it didn't do what it was asked.
func turnedDown(t *testing.T, c *Client) error {
	t.Helper()
	for range 500 {
		if err := c.Err(); err != nil {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the daemon never said why it didn't do what it was asked")
	return nil
}

// A UI shows the daemon's queue and changes it, and is told why when the
// daemon won't do what it asks.
func TestUIDrivesTheDaemonsQueue(t *testing.T) {
	srv, q := serve(t)
	c, err := Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	const film = "/films/A Film (2001)/A Film (2001).mkv"
	c.Add(film)
	if evs := c.Next(); len(evs) != 1 || evs[0].Job.Input != film {
		t.Fatalf("the UI saw %+v, want the job it added", evs)
	}
	c.Add("A Film (2001).mkv")
	if err := turnedDown(t, c); !strings.Contains(err.Error(), "full path") {
		t.Errorf("a relative path was turned down with %q", err)
	}
	c.send(request{Op: "defragment"})
	if err := turnedDown(t, c); !strings.Contains(err.Error(), "different version") {
		t.Errorf("a request the daemon doesn't know was turned down with %q", err)
	}
	if n := len(q.Jobs()); n != 1 {
		t.Errorf("the daemon has %d jobs, want only the one added by its full path", n)
	}

	// A UI that opens later starts with the queue as it is.
	late, err := Dial()
	if err != nil {
		t.Fatal(err)
	}
	if jobs := late.Jobs(); len(jobs) != 1 || jobs[0].Input != film {
		t.Errorf("a UI opened later sees %+v, want the one job", jobs)
	}
	// One that sends more than any request could be is cut off.
	late.conn.Write(append(bytes.Repeat([]byte("x"), maxRequest+1), '\n'))
	for late.Next() != nil {
	}

	srv.Close()
	for c.Next() != nil {
	}
	c.SetPaused(true)
	if err := c.Err(); !errors.Is(err, errGone) || c.Paused() {
		t.Errorf("asked to pause once the daemon had gone, the UI got %v, and shows paused %v", err, c.Paused())
	}
}

// A UI won't show a daemon of another version, which it could misread,
// such as one from before the protocol had a version.
func TestDaemonOfAnotherVersionIsRefused(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	ln, err := net.Listen("unix", SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Write([]byte(`{"devices":null,"paused":false,"holding":false,"watching":false,"saved":0,"results":0}` + "\n"))
			c.Close()
		}
	}()
	if _, err := Dial(); err == nil || errors.Is(err, ErrNoDaemon) || !strings.Contains(err.Error(), "different version") {
		t.Errorf("Dial gave %v, want it to say the daemon is a different version", err)
	}
}

// Only a socket of this user's is taken for the daemon's.
func TestOnlyThisUsersSocketIsTrusted(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	if _, err := Dial(); !errors.Is(err, ErrNoDaemon) {
		t.Errorf("with no socket, Dial gave %v, want ErrNoDaemon", err)
	}
	if err := os.WriteFile(SocketPath(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Dial(); err == nil || errors.Is(err, ErrNoDaemon) {
		t.Errorf("with a file that isn't a socket in its place, Dial gave %v", err)
	}
	if srv, err := Serve(nil, nil); err == nil {
		srv.Close()
		t.Error("the daemon listened in place of a file that isn't a socket")
	}
}
