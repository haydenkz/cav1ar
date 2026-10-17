package worker

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/haydenkz/cav1ar/internal/encode"
)

// listen starts a server for workers with the token "secret", which no
// worker has joined yet.
func listen(t *testing.T) (*Server, *fakeQueue) {
	t.Helper()
	q := &fakeQueue{dir: t.TempDir(), devices: map[string][]encode.Device{}, changed: make(chan struct{}, 100)}
	s, err := Listen("127.0.0.1:0", "secret", q)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, q
}

// testOptions are a worker called "test"'s, apart from where it connects.
func testOptions(t *testing.T) Options {
	return Options{Token: "secret", Name: "test", Log: t.Logf,
		Devices: []encode.Device{{ID: "vaapi:0000:0c:00.0", Kind: encode.EncoderVAAPI, Name: "Test GPU"}}}
}

// dial joins the daemon at addr as a worker with options o, without
// running passes for it.
func dial(t *testing.T, addr string, o Options) (net.Conn, error) {
	t.Helper()
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	_, _, err = joinDaemon(raw, o)
	return raw, err
}

// proved goes through a worker's side of the handshake with the daemon at
// addr, as far as the daemon's proof.
func proved(t *testing.T, addr string) (*tls.Conn, *reader) {
	t.Helper()
	nc, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	in, enc := newReader(nc, msgLimit), json.NewEncoder(nc)
	workerNonce := newNonce()
	enc.Encode(msg{Hello: &hello{Protocol: protocol, Nonce: workerNonce}})
	m, err := in.read()
	if err != nil || m.Hello == nil {
		t.Fatalf("no hello from the daemon: %v %+v", err, m)
	}
	cs := nc.ConnectionState()
	proof, _ := prove("secret", "worker", cs, workerNonce, m.Hello.Nonce)
	want, _ := prove("secret", "daemon", cs, workerNonce, m.Hello.Nonce)
	enc.Encode(msg{Proof: proof})
	if m, err := in.read(); err != nil || !hmac.Equal(m.Proof, want) {
		t.Fatalf("the daemon didn't prove itself: %v %+v", err, m)
	}
	return nc, in
}

// The connection is encrypted, and the token is only ever proved, so
// someone watching the network never sees it.
func TestTheTokenNeverCrossesTheWire(t *testing.T) {
	s, q := listen(t)
	// A relay that keeps a copy of everything either side sends.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var (
		mu   sync.Mutex
		seen bytes.Buffer
	)
	go func() {
		w, err := ln.Accept()
		if err != nil {
			return
		}
		d, err := net.Dial("tcp", s.Addr().String())
		if err != nil {
			w.Close()
			return
		}
		relay := func(to, from net.Conn) {
			defer to.Close()
			buf := make([]byte, 4096)
			for {
				n, err := from.Read(buf)
				mu.Lock()
				seen.Write(buf[:n])
				mu.Unlock()
				if _, werr := to.Write(buf[:n]); err != nil || werr != nil {
					return
				}
			}
		}
		go relay(d, w)
		relay(w, d)
	}()
	if _, err := dial(t, ln.Addr().String(), testOptions(t)); err != nil {
		t.Fatal(err)
	}
	<-q.changed
	mu.Lock()
	defer mu.Unlock()
	for _, secret := range []string{"secret", "Test GPU", `"hello"`} {
		if bytes.Contains(seen.Bytes(), []byte(secret)) {
			t.Errorf("%s crossed the wire as it is", secret)
		}
	}
}

// Someone in the middle, with an encrypted connection to each side, can't
// pass the worker's proof on to the daemon, since each proof is bound to
// its own connection.
func TestAManInTheMiddleIsFound(t *testing.T) {
	s, q := listen(t)
	cert, err := selfSigned()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		w, err := ln.Accept()
		if err != nil {
			return
		}
		defer w.Close()
		d, err := tls.Dial("tcp", s.Addr().String(), &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer d.Close()
		go io.Copy(d, w)
		io.Copy(w, d)
	}()
	_, err = dial(t, ln.Addr().String(), testOptions(t))
	if err == nil || !strings.Contains(err.Error(), "wrong token") {
		t.Errorf("got %v, want the daemon to refuse the proof passed on", err)
	}
	if len(q.Devices()) != 0 {
		t.Error("a worker joined through a man in the middle")
	}
}

// A worker doesn't say who it is, or take passes, from whatever answers at
// the daemon's address until it has proved it has the token.
func TestAWorkerWontJoinADaemonWithoutTheToken(t *testing.T) {
	cert, err := selfSigned()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	told := make(chan bool, 1)
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		defer nc.Close()
		in, enc := newReader(nc, msgLimit), json.NewEncoder(nc)
		in.read()
		enc.Encode(msg{Hello: &hello{Protocol: protocol, Nonce: newNonce()}})
		in.read()
		enc.Encode(msg{Proof: make([]byte, 32)}) // a guess
		m, err := in.read()
		told <- err == nil && m.Join != nil
	}()
	raw, err := dial(t, ln.Addr().String(), testOptions(t))
	if err == nil || !strings.Contains(err.Error(), "couldn't prove") {
		t.Errorf("got %v, want the daemon turned down", err)
	}
	raw.Close()
	if <-told {
		t.Error("the worker said who it is to a daemon that didn't prove itself")
	}
}

// A worker from before connections were encrypted gets an answer it can
// read, saying to update it.
func TestAnOldWorkerIsToldToUpdate(t *testing.T) {
	s, _ := listen(t)
	nc, err := net.Dial("tcp", s.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	fmt.Fprintln(nc, `{"hello":{"protocol":1,"token":"secret","name":"old","devices":[]}}`)
	nc.SetReadDeadline(time.Now().Add(10 * time.Second))
	var m msg
	if err := json.NewDecoder(nc).Decode(&m); err != nil || !strings.Contains(m.Refused, "update cav1ar on the worker") {
		t.Errorf("got %v %+v, want a refusal saying to update the worker", err, m)
	}
}

// A worker that finds a daemon from before connections were encrypted, which
// hangs up on what it can't read, says the daemon may need updating.
func TestAWorkerSaysToUpdateAnOldDaemon(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		var m msg
		json.NewDecoder(nc).Decode(&m)
		nc.Close()
	}()
	if _, err := dial(t, ln.Addr().String(), testOptions(t)); err == nil || !strings.Contains(err.Error(), "older cav1ar") {
		t.Errorf("got %v, want a hint that the daemon is older", err)
	}
}

func TestWorkersOfOtherVersionsAreRefused(t *testing.T) {
	s, _ := listen(t)
	for v, want := range map[int]string{protocol - 1: "update cav1ar on the worker", protocol + 1: "update cav1ar on the daemon"} {
		nc, err := tls.Dial("tcp", s.Addr().String(), &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			t.Fatal(err)
		}
		json.NewEncoder(nc).Encode(msg{Hello: &hello{Protocol: v, Nonce: newNonce()}})
		if m, err := newReader(nc, msgLimit).read(); err != nil || !strings.Contains(m.Refused, want) {
			t.Errorf("version %d: got %v %+v, want a refusal saying to %s", v, err, m, want)
		}
		nc.Close()
	}
}

// Strangers can't tie the daemon up with a hello too long to read, or with
// many connections that say nothing.
func TestStrangersCantTieTheDaemonUp(t *testing.T) {
	s, _ := listen(t)
	addr := s.Addr().String()

	nc, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	nc.Write(bytes.Repeat([]byte("x"), 2*handshakeLimit))
	nc.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := nc.Read(make([]byte, 1)); err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("a hello too long to be one was read for %s, ending with %v", time.Since(start), err)
	}
	nc.Close()

	var silent []net.Conn
	for range pendingLimit {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		silent = append(silent, c)
	}
	extra, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	extra.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := extra.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("a connection past the limit got %v, want it closed", err)
	}

	// Once they've gone, workers join again.
	for _, c := range silent {
		c.Close()
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		_, err := dial(t, addr, testOptions(t))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a worker couldn't join once the silent connections had gone: %v", err)
		}
	}
}

// A worker that connects again before the daemon has found its old
// connection dead, as after losing power, takes the old one's place, and
// the passes on the old one fail so their jobs go back in the queue.
func TestAWorkerThatComesBackTakesItsOwnPlace(t *testing.T) {
	s, q := listen(t)
	if _, err := dial(t, s.Addr().String(), testOptions(t)); err != nil {
		t.Fatal(err)
	}
	<-q.changed
	old := runner(t, s)
	failed := make(chan error, 1)
	go func() { failed <- old.Run(context.Background(), []string{"slow"}, (&lines{}).output()) }()

	if _, err := dial(t, s.Addr().String(), testOptions(t)); err != nil {
		t.Fatalf("the worker was turned away when it came back: %v", err)
	}
	select {
	case err := <-failed:
		if !errors.Is(err, encode.ErrWorkerGone) {
			t.Errorf("the pass on the old connection got %v, want ErrWorkerGone", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the pass on the old connection never ended")
	}
	<-q.changed // the old connection's devices leave
	<-q.changed // and the new one's join
	if runner(t, s) == old || len(q.Devices()) != 1 {
		t.Errorf("the new connection didn't take the old one's place: devices %+v", q.Devices())
	}
}

// Closing the server lets go of workers part way through joining, which
// then can't join.
func TestClosingLetsGoOfWorkersStillJoining(t *testing.T) {
	s, _ := listen(t)
	nc, in := proved(t, s.Addr().String())
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("closing waited on a worker still joining")
	}
	json.NewEncoder(nc).Encode(msg{Join: &join{Name: "late"}})
	if m, err := in.read(); err == nil {
		t.Errorf("a worker joining a closed server got %+v", m)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.workers) != 0 {
		t.Errorf("workers joined a closed server: %v", s.workers)
	}
}
