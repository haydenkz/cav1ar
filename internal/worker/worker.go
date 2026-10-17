// Package worker lets a cav1ar daemon encode on the GPUs of other machines,
// or containers: workers. A worker connects to the daemon over TCP, the two
// prove to each other that they have the daemon's token, and the worker says
// what it can encode on. The daemon adds those devices to its queue, and
// runs the pass that encodes a job's video on the worker when one of them
// takes the job; see encode.Runner.
//
// Paths are the same on both sides: the worker reads the source where the
// daemon does, and writes the pass's files to a folder both share. When the
// worker joins, the daemon leaves a file in that folder for it to find, so a
// worker that doesn't see the same files never takes a job.
//
// The connection is encrypted with TLS, under a certificate the daemon makes
// each time it starts, which the worker doesn't check. The token is what
// proves each side to the other, and it never crosses the wire: each side
// sends a hello with a random nonce, then a proof, an HMAC keyed with the
// token of both nonces and of keying material exported from the TLS
// session; see prove. A proof is no good on any other connection, so a man
// in the middle, who has a session with each side, can't pass one on. The
// worker proves itself first, and sends nothing more until the daemon has
// proved itself too.
//
// Messages are JSON, one per line. After the proofs, the worker says who it
// is with join; the daemon answers with welcome or refused; the worker says
// ready once it has found the daemon's file, or error if it can't. After
// that the daemon sends run and signal, and the worker sends what each pass
// writes, with out, and how it ended, with exit. The daemon sends refused
// instead of whatever comes next whenever it won't take the worker.
package worker

import (
	"bufio"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/haydenkz/cav1ar/internal/encode"
)

// protocol changes when the messages do, so daemon and workers of
// different versions refuse each other rather than misunderstand. Version 1
// sent the token itself, unencrypted.
const protocol = 2

const (
	// handshakeLimit caps each message until the proofs are through, when
	// anyone could be sending them. A hello or a proof is far smaller.
	handshakeLimit = 4 << 10
	// msgLimit caps each message after that. The biggest are a pass's
	// command line and the lines it writes, which lineWriter keeps to
	// lineLimit.
	msgLimit  = 1 << 20
	lineLimit = 64 << 10

	nonceSize = 32
)

// msg is one message, either way. One field is set.
type msg struct {
	Hello   *hello   `json:"hello,omitempty"`
	Proof   []byte   `json:"proof,omitempty"`
	Join    *join    `json:"join,omitempty"`
	Welcome *welcome `json:"welcome,omitempty"`
	Refused string   `json:"refused,omitempty"` // why the daemon won't take the worker
	Ready   bool     `json:"ready,omitempty"`
	Error   string   `json:"error,omitempty"` // why the worker can't work for the daemon
	Run     *run     `json:"run,omitempty"`
	Signal  *signal  `json:"signal,omitempty"`
	Out     *out     `json:"out,omitempty"`
	Exit    *exit    `json:"exit,omitempty"`
}

// hello is the first message each side sends.
type hello struct {
	Protocol int    `json:"protocol"`
	Nonce    []byte `json:"nonce"` // random, for this connection's proofs
}

// join is who the worker is, which it says once the daemon has proved
// itself.
type join struct {
	Name    string          `json:"name"`
	FFmpeg  string          `json:"ffmpeg"` // its build, as encode.FFmpegBuild gives it
	Devices []encode.Device `json:"devices"`
}

type welcome struct {
	WorkDir string `json:"work_dir"` // the shared folder
	Check   string `json:"check"`    // the name of the file the daemon left in it
}

// run starts a pass: ffmpeg with Args.
type run struct {
	ID   int      `json:"id"`
	Args []string `json:"args"`
}

// signal pauses a pass ("stop"), carries it on ("cont"), or ends it ("kill").
type signal struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// out is a line a pass wrote.
type out struct {
	ID     int    `json:"id"`
	Stderr bool   `json:"stderr,omitempty"`
	Line   string `json:"line"`
}

// exit says a pass has ended, and why, if it failed.
type exit struct {
	ID  int    `json:"id"`
	Err string `json:"err,omitempty"`
}

// exporterLabel names the keying material that proofs are bound to.
const exporterLabel = "EXPORTER-cav1ar-worker-proof"

// prove is the proof that side, "worker" or "daemon", has token, on the TLS
// session cs, whose hellos carried the nonces given. Each side's proof is
// different, so neither can be sent back as the other's.
func prove(token, side string, cs tls.ConnectionState, workerNonce, daemonNonce []byte) ([]byte, error) {
	keying, err := cs.ExportKeyingMaterial(exporterLabel, nil, 32)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte("cav1ar " + side + "\x00"))
	mac.Write(keying)
	mac.Write(workerNonce)
	mac.Write(daemonNonce)
	return mac.Sum(nil), nil
}

func newNonce() []byte {
	b := make([]byte, nonceSize)
	rand.Read(b)
	return b
}

// mismatch explains a worker and a daemon that speak different versions of
// the protocol, saying which to update.
func mismatch(worker, daemon int) string {
	older := "worker"
	if daemon < worker {
		older = "daemon"
	}
	return fmt.Sprintf("the worker speaks version %d of the protocol, and the daemon %d; update cav1ar on the %s, so both run the same version",
		worker, daemon, older)
}

// reader reads messages, one per line, of no more than limit bytes each.
type reader struct {
	r     *bufio.Reader
	limit int
}

func newReader(r io.Reader, limit int) *reader { return &reader{bufio.NewReader(r), limit} }

var errTooLong = errors.New("a message was too long")

func (r *reader) read() (msg, error) {
	var line []byte
	for {
		part, err := r.r.ReadSlice('\n')
		if len(line)+len(part) > r.limit {
			return msg{}, errTooLong
		}
		line = append(line, part...)
		if err == nil {
			break
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return msg{}, err
		}
	}
	var m msg
	err := json.Unmarshal(line, &m)
	return m, err
}
