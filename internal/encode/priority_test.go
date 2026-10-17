package encode

import (
	"context"
	"testing"
)

func TestWatchFolderPriorityRoundTrips(t *testing.T) {
	for _, s := range []string{"+1 movies: /media/Movies", "shows: /media/TV", "-2 /media/Other", "/media/Plain"} {
		if got := ParseWatchFolder(s).String(); got != s {
			t.Errorf("ParseWatchFolder(%q).String() = %q", s, got)
		}
	}
	if f := ParseWatchFolder("+3 shows: /tv"); f.Priority != 3 || f.Kind != FolderShows || f.Path != "/tv" {
		t.Errorf("got %+v", f)
	}
}

// A priority doesn't change which files match the settings, so it mustn't
// make the watcher check the whole library again.
func TestPriorityLeavesTheFingerprintAlone(t *testing.T) {
	s := DefaultSettings()
	s.Watch = []WatchFolder{{Path: "/media/Movies", Kind: FolderMovies}, {Path: "/media/TV", Kind: FolderShows}}
	before := fingerprint(s)
	s.Watch[0].Priority = 1
	if after := fingerprint(s); after != before {
		t.Errorf("giving a folder a priority changed the fingerprint from %s to %s", before, after)
	}
}

// Videos in a higher-priority folder run first, ahead even of a video that
// has just arrived in a lower one, and a priority set later reorders what
// is waiting.
func TestHigherPriorityFoldersRunFirst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := DefaultSettings()
	s.Encoders = map[string]int{"cpu": 1}
	s.Watch = []WatchFolder{{Path: "/nonexistent/Movies", Kind: FolderMovies, Priority: 1}, {Path: "/nonexistent/TV", Kind: FolderShows}}
	q := newQueue(ctx, s, []Device{cpuDevice})
	defer func() { cancel(); q.Close() }()

	started := map[int]bool{}
	first := func() string { // the next job to start
		t.Helper()
		for {
			evs := q.Next()
			if evs == nil {
				t.Fatal("the queue closed before any job started")
			}
			for _, ev := range evs {
				if ev.Job.State == Running && !started[ev.Job.ID] {
					started[ev.Job.ID] = true
					return ev.Job.Input
				}
			}
		}
	}

	q.SetPaused(true)
	q.AddNext("/nonexistent/TV/Show/arrived.mkv")
	q.Add("/nonexistent/Movies/Film (2020)/backlog.mkv")
	q.SetPaused(false)
	if got := first(); got != "/nonexistent/Movies/Film (2020)/backlog.mkv" {
		t.Fatalf("%s started first, not the movie", got)
	}

	q.SetPaused(true)
	q.Add("/nonexistent/Movies/Other (2021)/backlog.mkv")
	s.Watch[1].Priority = 2 // TV now outranks movies
	q.SetSettings(s)
	q.SetPaused(false)
	if got := first(); got != "/nonexistent/TV/Show/arrived.mkv" {
		t.Fatalf("%s started next, not the episode whose folder now ranks higher", got)
	}
}
