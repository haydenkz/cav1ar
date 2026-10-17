package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/haydenkz/cav1ar/internal/encode"
)

// setup starts a web UI on a queue of its own, which encodes on a stand-in
// CPU, and returns it with its address. Saved settings go into effect, as
// the daemon's do.
func setup(t *testing.T) (*Server, *encode.Queue, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	q := encode.NewQueueOn(ctx, encode.DefaultSettings(), []encode.Device{{ID: "cpu", Kind: encode.EncoderCPU, Name: "CPU"}})
	q.SetHolding(true) // jobs stay queued
	dir := t.TempDir()
	var mu sync.Mutex
	s, err := Listen(Options{Addr: "127.0.0.1:0", Queue: q, StatePath: filepath.Join(dir, "cav1ar.db"),
		KeyPath: filepath.Join(dir, "web-key"), Version: "test",
		Apply: func(st encode.Settings) error { mu.Lock(); defer mu.Unlock(); q.SetSettings(st); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	go func() { // as the daemon hands the queue's events over
		for evs := q.Next(); evs != nil; evs = q.Next() {
			s.Publish(evs)
		}
	}()
	t.Cleanup(func() { s.Close(); cancel(); q.Close() })
	return s, q, "http://" + s.Addr()
}

func client(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Timeout: 10 * time.Second}
}

func do(t *testing.T, c *http.Client, method, url, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v map[string]any
	json.NewDecoder(resp.Body).Decode(&v)
	return resp.StatusCode, v
}

func TestThePageAndItsStateAreServed(t *testing.T) {
	_, q, base := setup(t)
	c := client(t)
	resp, err := c.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(page), "app.js") {
		t.Errorf("/ didn't serve the page: %.200s", page)
	}
	q.Add("/nonexistent/a.mkv")
	code, state := do(t, c, "GET", base+"/api/state", "")
	jobs, _ := state["jobs"].([]any)
	if code != 200 || len(jobs) != 1 || state["status"].(map[string]any)["version"] != "test" {
		t.Errorf("state %d: %v", code, state)
	}
}

// Changes must come as JSON, which a page on another site can't send without
// the browser asking this one first.
func TestChangesMustBeJSON(t *testing.T) {
	_, _, base := setup(t)
	resp, err := http.Post(base+"/api/add", "application/x-www-form-urlencoded", strings.NewReader("path=/"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("a form post got %d", resp.StatusCode)
	}
}

func TestSettingsAreCheckedAndSaved(t *testing.T) {
	_, q, base := setup(t)
	c := client(t)
	_, view := do(t, c, "GET", base+"/api/settings", "")
	settings := view["settings"].(map[string]any)
	settings["crf"] = 99
	b, _ := json.Marshal(settings)
	if code, v := do(t, c, "PUT", base+"/api/settings", string(b)); code != 400 || !strings.Contains(v["error"].(string), "crf") {
		t.Errorf("a quality of 99 got %d %v", code, v)
	}
	settings["crf"] = 28
	settings["watch"] = []map[string]any{{"path": "/nonexistent/folder"}}
	b, _ = json.Marshal(settings)
	if code, _ := do(t, c, "PUT", base+"/api/settings", string(b)); code != 400 {
		t.Errorf("a folder that isn't there got %d", code)
	}
	settings["watch"] = []map[string]any{{"path": t.TempDir(), "kind": "shows"}}
	b, _ = json.Marshal(settings)
	if code, v := do(t, c, "PUT", base+"/api/settings", string(b)); code != 200 {
		t.Fatalf("good settings got %d %v", code, v)
	}
	if s := q.Settings(); s.CRF != 28 || len(s.Watch) != 1 || s.Watch[0].Kind != encode.FolderShows {
		t.Errorf("saved %+v", s)
	}
}

func TestAPasswordGuardsEverything(t *testing.T) {
	_, q, base := setup(t)
	c := client(t)
	if code, _ := do(t, c, "PUT", base+"/api/password", `{"password":"short"}`); code != 400 {
		t.Errorf("a short password got %d", code)
	}
	if code, _ := do(t, c, "PUT", base+"/api/password", `{"password":"correct horse"}`); code != 200 {
		t.Fatalf("setting the password got %d", code)
	}
	hash := q.Settings().WebPassword
	if hash == "" || strings.Contains(hash, "correct") {
		t.Fatalf("the password was kept as %q", hash)
	}
	if code, _ := do(t, c, "GET", base+"/api/state", ""); code != 200 {
		t.Errorf("the page that set the password was logged out: %d", code)
	}
	if _, view := do(t, c, "GET", base+"/api/settings", ""); view["settings"].(map[string]any)["web_password"] != nil {
		t.Error("the password's hash was sent to the page")
	}

	stranger := client(t)
	if code, _ := do(t, stranger, "GET", base+"/api/state", ""); code != 401 {
		t.Errorf("no login got %d", code)
	}
	if code, _ := do(t, stranger, "POST", base+"/api/login", `{"password":"wrong"}`); code != 401 {
		t.Errorf("a wrong password got %d", code)
	}
	if code, _ := do(t, stranger, "POST", base+"/api/login", `{"password":"correct horse"}`); code != 200 {
		t.Errorf("the right password got %d", code)
	}
	if code, _ := do(t, stranger, "GET", base+"/api/state", ""); code != 200 {
		t.Errorf("logged in, got %d", code)
	}

	// A new password ends every login but the page's that set it.
	if code, _ := do(t, c, "PUT", base+"/api/password", `{"password":"battery staple"}`); code != 200 {
		t.Fatal("changing the password failed")
	}
	if code, _ := do(t, stranger, "GET", base+"/api/state", ""); code != 401 {
		t.Errorf("an old login still worked: %d", code)
	}
}

func TestChangesArePushedToOpenPages(t *testing.T) {
	_, q, base := setup(t)
	resp, err := client(t).Get(base + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	events := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			if name, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
				events <- name
			}
		}
	}()
	want := func(name string) {
		t.Helper()
		for {
			select {
			case got := <-events:
				if got == name {
					return
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("no %s event", name)
			}
		}
	}
	want("snapshot")
	q.Add("/nonexistent/a.mkv")
	want("jobs")
}

func TestBrowseListsFoldersAndVideos(t *testing.T) {
	_, _, base := setup(t)
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "Show"), 0o755)
	os.Mkdir(filepath.Join(dir, ".hidden"), 0o755)
	os.WriteFile(filepath.Join(dir, "film.mkv"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644)
	_, v := do(t, client(t), "GET", base+"/api/browse?path="+dir, "")
	var names []string
	for _, e := range v["entries"].([]any) {
		names = append(names, e.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "Show,film.mkv" {
		t.Errorf("listed %v", names)
	}
}
