package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/haydenkz/cav1ar/internal/encode"
)

// fakeFFmpeg pretends to be ffmpeg: it writes progress and a line on
// stderr, unless its arguments ask it to fail or to take its time.
const fakeFFmpeg = `#!/bin/sh
for a in "$@"; do
	[ "$a" = fail ] && { echo "fake failure" >&2; exit 1; }
	[ "$a" = slow ] && sleep 30
done
printf 'frame=5\nprogress=continue\nframe=10\nprogress=end\n'
echo "fake ffmpeg ran" >&2
echo "args: $*" >&2
`

// fakeQueue records the devices workers bring.
type fakeQueue struct {
	dir string

	mu      sync.Mutex
	devices map[string][]encode.Device
	changed chan struct{}
}

func (q *fakeQueue) Settings() encode.Settings {
	s := encode.DefaultSettings()
	s.WorkerDir = q.dir
	return s
}

func (q *fakeQueue) Devices() []encode.Device {
	q.mu.Lock()
	defer q.mu.Unlock()
	var all []encode.Device
	for _, ds := range q.devices {
		all = append(all, ds...)
	}
	return all
}

func (q *fakeQueue) SetWorkerDevices(name string, devices []encode.Device) {
	q.mu.Lock()
	q.devices[name] = devices
	q.mu.Unlock()
	q.changed <- struct{}{}
}

// setup starts a server, and a worker called "test" for it, returning the
// server once the worker has joined, and a function that stops the worker.
func setup(t *testing.T, allow ...string) (*Server, *fakeQueue, context.CancelFunc) {
	return setupWith(t, Options{Allow: allow})
}

// setupWith is setup with the worker's options as given, apart from those
// that connect it.
func setupWith(t *testing.T, o Options) (*Server, *fakeQueue, context.CancelFunc) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(bin, []byte(fakeFFmpeg), 0o755); err != nil {
		t.Fatal(err)
	}
	old := encode.FFmpeg
	encode.FFmpeg = bin
	t.Cleanup(func() { encode.FFmpeg = old }) // runs last, once the worker has stopped

	dir := o.Addr // the shared folder, as the daemon sees it, if the test chose one
	if dir == "" {
		dir = t.TempDir()
	}
	q := &fakeQueue{dir: dir, devices: map[string][]encode.Device{}, changed: make(chan struct{}, 10)}
	s, err := Listen("127.0.0.1:0", "secret", q)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ctx, stop := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	t.Cleanup(func() { stop(); <-stopped })
	go func() {
		defer close(stopped)
		o.Addr, o.Token, o.Name, o.Log = s.Addr().String(), "secret", "test", t.Logf
		o.Devices = []encode.Device{{ID: "vaapi:0000:0c:00.0", Kind: encode.EncoderVAAPI, Name: "Test GPU"}}
		Work(ctx, o)
	}()
	select {
	case <-q.changed:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker didn't join")
	}
	return s, q, stop
}

func runner(t *testing.T, s *Server) *conn {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.workers["test"]
	if c == nil {
		t.Fatal("no worker called test")
	}
	return c
}

// lines collects what a pass writes.
type lines struct {
	mu             sync.Mutex
	stdout, stderr []string
}

func (l *lines) output() encode.Output {
	add := func(to *[]string) func(string) {
		return func(line string) { l.mu.Lock(); *to = append(*to, line); l.mu.Unlock() }
	}
	return encode.Output{Stdout: add(&l.stdout), Stderr: add(&l.stderr), Started: func(encode.Process) {}}
}

func TestAWorkerJoinsWithItsDevices(t *testing.T) {
	_, q, _ := setup(t)
	got := q.Devices()
	if len(got) != 1 || got[0].ID != "test/vaapi:0000:0c:00.0" || got[0].Name != "Test GPU (test)" || got[0].Worker != "test" {
		t.Errorf("the queue got %+v", got)
	}
	if entries, _ := os.ReadDir(q.dir); len(entries) != 0 {
		t.Errorf("the check files were left in the shared folder: %v", entries)
	}
}

func TestPassesRunOnTheWorker(t *testing.T) {
	s, _, _ := setup(t)
	c := runner(t, s)

	var l lines
	if err := c.Run(context.Background(), []string{"-i", "in.mkv"}, l.output()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(l.stdout, "frame=10") || !slices.Contains(l.stderr, "fake ffmpeg ran") {
		t.Errorf("stdout %q, stderr %q", l.stdout, l.stderr)
	}

	var failed lines
	err := c.Run(context.Background(), []string{"fail"}, failed.output())
	if err == nil || !slices.Contains(failed.stderr, "fake failure") {
		t.Errorf("a failing pass gave %v, stderr %q", err, failed.stderr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	if err := c.Run(ctx, []string{"slow"}, (&lines{}).output()); !errors.Is(err, context.Canceled) || time.Since(start) > 10*time.Second {
		t.Errorf("a canceled pass gave %v after %s", err, time.Since(start))
	}
}

// A pass whose worker goes away fails with ErrWorkerGone, after its
// devices have left the queue.
func TestAWorkerGoingAwayFailsItsPasses(t *testing.T) {
	s, q, stop := setup(t)
	c := runner(t, s)
	time.AfterFunc(200*time.Millisecond, stop)
	if err := c.Run(context.Background(), []string{"slow"}, (&lines{}).output()); !errors.Is(err, encode.ErrWorkerGone) {
		t.Errorf("got %v, want ErrWorkerGone", err)
	}
	<-q.changed
	if len(q.Devices()) != 0 {
		t.Errorf("the worker's devices stayed: %+v", q.Devices())
	}
}

func TestAWrongTokenIsRefused(t *testing.T) {
	q := &fakeQueue{dir: t.TempDir(), devices: map[string][]encode.Device{}, changed: make(chan struct{}, 10)}
	s, err := Listen("127.0.0.1:0", "secret", q)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = connect(context.Background(), Options{Addr: s.Addr().String(), Token: "guess", Name: "test",
		Devices: []encode.Device{{ID: "cpu", Kind: encode.EncoderCPU, Name: "CPU"}}, Log: t.Logf})
	if err == nil || !strings.Contains(err.Error(), "wrong token") {
		t.Errorf("got %v, want a refusal for the token", err)
	}
	if len(q.Devices()) != 0 {
		t.Error("a worker with the wrong token joined")
	}
}

func TestPassesStayInTheFoldersAllowed(t *testing.T) {
	s, q, _ := setup(t, "/media")
	c := runner(t, s)
	err := c.Run(context.Background(), []string{"-vaapi_device", "/dev/dri/renderD128", "-i", "/media/a.mkv", "/etc/passwd"}, (&lines{}).output())
	if err == nil || !strings.Contains(err.Error(), "/etc/passwd") {
		t.Errorf("got %v, want /etc/passwd refused", err)
	}
	if err := c.Run(context.Background(), []string{"-i", "/media/a.mkv", "/media/../media/b.mkv"}, (&lines{}).output()); err != nil {
		t.Errorf("paths in /media were refused: %v", err)
	}
	_ = q
}

func TestAWorkerMustSeeTheSharedFolder(t *testing.T) {
	dir := t.TempDir()
	if err := checkShared(&welcome{WorkDir: dir, Check: ".cav1ar-worker-check-missing"}); err == nil {
		t.Error("a worker that doesn't see the daemon's file was let through")
	}
	os.WriteFile(filepath.Join(dir, "seen"), nil, 0o644)
	if err := checkShared(&welcome{WorkDir: dir, Check: "seen"}); err != nil {
		t.Errorf("a worker that sees it was turned away: %v", err)
	}
}

func TestPathsMapOntoTheWorkers(t *testing.T) {
	maps := []PathMap{{"/mnt/media", "/media"}, {"/mnt/media/tv", "/tv"}, {"/", "/root-of-all"}}
	for in, want := range map[string]string{
		"/mnt/media":           "/media",
		"/mnt/media/film.mkv":  "/media/film.mkv",
		"/mnt/media/tv/ep.mkv": "/tv/ep.mkv", // the longest mapping wins
		"/mnt/mediaserver/x":   "/root-of-all/mnt/mediaserver/x",
		"-c:v":                 "-c:v",
		"vaapi=gpu:/dev/dri/x": "vaapi=gpu:/dev/dri/x",
	} {
		if got := mapPath(maps, in); got != want {
			t.Errorf("mapPath(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := ParsePathMap("relative=/abs"); err == nil {
		t.Error("a relative path was accepted")
	}
}

// A worker that sees the daemon's folders somewhere else joins, and its
// passes get the paths as it sees them.
func TestAWorkerWithOtherPathsJoinsAndRuns(t *testing.T) {
	real := t.TempDir()
	daemonView := filepath.Join(t.TempDir(), "daemon-view")
	if err := os.Symlink(real, daemonView); err != nil {
		t.Fatal(err)
	}
	s, _, _ := setupWith(t, Options{Addr: daemonView, Paths: []PathMap{{daemonView, real}}, Allow: []string{real}})
	var l lines
	if err := runner(t, s).Run(context.Background(), []string{"-i", daemonView + "/in.mkv"}, l.output()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(l.stderr, "args: -i "+real+"/in.mkv") {
		t.Errorf("the pass was given %q", l.stderr)
	}
}
