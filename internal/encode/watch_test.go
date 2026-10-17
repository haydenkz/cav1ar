package encode

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newTestWatcher returns a Watcher for q that keeps its state in a folder of
// its own, as Watch would, without scanning on its own.
func newTestWatcher(t *testing.T, q *Queue) *Watcher {
	t.Helper()
	// Taking the lock clears out cav1ar's work folder, which is kept here.
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	w := newWatcher(q, filepath.Join(t.TempDir(), "cav1ar.db"), false)
	t.Cleanup(w.release)
	return w
}

// writeVideo puts a stand-in for a video at path, which no job could read.
func writeVideo(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not really a video"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// historyLen is how many jobs w has added to the history.
func historyLen(t *testing.T, w *Watcher) int {
	t.Helper()
	var n int
	if err := w.db.db.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestScanKeepsVideosMissingFromAFolderThatComesBackEmpty(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	shows := filepath.Join(root, "Shows")
	episode := filepath.Join(shows, "Example Show", "Example Show S01E01.mkv")
	writeVideo(t, episode)
	s := DefaultSettings()
	s.Watch = []WatchFolder{{Path: shows}}
	q := newQueue(ctx, s, nil)
	w := newTestWatcher(t, q)

	w.scan(ctx)
	if got := w.files[episode].Status; got != fileLeft {
		t.Fatalf("a video there before its folder was watched is %q, not left alone", got)
	}

	// As if the folder were a network share that came unmounted, leaving
	// its mount point empty.
	if err := os.Rename(filepath.Join(shows, "Example Show"), filepath.Join(root, "Example Show")); err != nil {
		t.Fatal(err)
	}
	w.scan(ctx)
	if _, ok := w.files[episode]; !ok {
		t.Fatal("a video missing from a folder that came back empty was forgotten")
	}

	// Mounted again, the video is still left alone, rather than taken for
	// one that has just arrived.
	if err := os.Rename(filepath.Join(root, "Example Show"), filepath.Join(shows, "Example Show")); err != nil {
		t.Fatal(err)
	}
	w.scan(ctx)
	w.scan(ctx)
	if jobs := q.Jobs(); len(jobs) > 0 {
		t.Fatalf("queued %s", jobs[0].Input)
	}

	// One deleted from a folder that still has others is forgotten.
	writeVideo(t, filepath.Join(shows, "Another Show", "Another Show S01E01.mkv"))
	if err := os.Remove(episode); err != nil {
		t.Fatal(err)
	}
	w.scan(ctx)
	if _, ok := w.files[episode]; ok {
		t.Error("a deleted video wasn't forgotten")
	}
}

func TestScanKeepsVideosMissingFromAFolderNotReadInFull(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads folders whatever their permissions")
	}
	ctx := t.Context()
	movies := filepath.Join(t.TempDir(), "Movies")
	writeVideo(t, filepath.Join(movies, "Example Film (2001)", "Example Film (2001).mkv"))
	hidden := filepath.Join(movies, "Other Film (2002)", "Other Film (2002).mkv")
	writeVideo(t, hidden)
	lock := func(locked bool) {
		mode := os.FileMode(0o755)
		if locked {
			mode = 0
		}
		if err := os.Chmod(filepath.Dir(hidden), mode); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { lock(false) })
	s := DefaultSettings()
	s.Watch = []WatchFolder{{Path: movies}}
	q := newQueue(ctx, s, nil)
	w := newTestWatcher(t, q)

	// The first scan can't read every subfolder, so the folder isn't taken
	// as scanned, and what the next scan finds is still left alone.
	lock(true)
	w.scan(ctx)
	if w.folders[movies] {
		t.Error("a folder not read in full was taken as scanned")
	}
	lock(false)
	w.scan(ctx)
	if got := w.files[hidden].Status; got != fileLeft {
		t.Errorf("a video there before its folder was read in full is %q, not left alone", got)
	}

	// Once known, a video in a subfolder that can't be read isn't forgotten.
	lock(true)
	w.scan(ctx)
	if _, ok := w.files[hidden]; !ok {
		t.Error("a video in a subfolder that couldn't be read was forgotten")
	}
	lock(false)
	w.scan(ctx)
	w.scan(ctx)
	if jobs := q.Jobs(); len(jobs) > 0 {
		t.Fatalf("queued %s", jobs[0].Input)
	}
}

func TestScanWaitsForALaterScanUnderNestedFolders(t *testing.T) {
	ctx := t.Context()
	media := t.TempDir()
	tv := filepath.Join(media, "TV")
	if err := os.Mkdir(tv, 0o755); err != nil {
		t.Fatal(err)
	}
	s := DefaultSettings()
	s.Watch = []WatchFolder{{Path: media}, {Path: tv, Kind: FolderShows}}
	q := newQueue(ctx, s, nil)
	w := newTestWatcher(t, q)
	w.scan(ctx) // the folders are empty, so there's nothing to leave alone

	// Both folders' scans find the video, which still counts as one sighting.
	episode := filepath.Join(tv, "Example Show", "Example Show S01E02.mkv")
	writeVideo(t, episode)
	w.scan(ctx)
	if jobs := q.Jobs(); len(jobs) > 0 {
		t.Fatalf("queued %s after one scan, which may have caught it still being copied", jobs[0].Input)
	}
	w.scan(ctx)
	if jobs := q.Jobs(); len(jobs) != 1 || jobs[0].Input != episode {
		t.Fatalf("queued %+v after two scans, want just %s", jobs, episode)
	}
}

func TestRecordJobsRecordsJobsCanceledWhileQueued(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	film := filepath.Join(dir, "Example Film (2003).mkv")
	writeVideo(t, film)
	s := DefaultSettings()
	s.Watch = []WatchFolder{{Path: dir}}
	q := newQueue(ctx, s, nil) // with nothing to encode on, jobs stay queued
	w := newTestWatcher(t, q)
	if !w.acquire() {
		t.Fatal("couldn't take the lock")
	}

	q.Add(film)
	q.Cancel(0)
	w.recordJobs(ctx)
	w.recordJobs(ctx)
	if f := w.files[film]; f.Status != fileLeft || f.Note != "canceled" {
		t.Errorf("the video is %q (%s), not left alone as canceled", f.Status, f.Note)
	}
	if n := historyLen(t, w); n != 1 {
		t.Errorf("%d jobs in the history, want 1", n)
	}

	// cav1ar stopping leaves queued jobs be, so one canceled before it
	// started was canceled by the user, even when it's recorded as cav1ar
	// stops.
	q.Retry(0)
	q.Cancel(0)
	stopped, stop := context.WithCancel(ctx)
	stop()
	w.recordJobs(stopped)
	if n := historyLen(t, w); n != 2 {
		t.Errorf("%d jobs in the history after a retry was canceled, want 2", n)
	}

	// Stopping watching and starting again doesn't record them again.
	w.release()
	if !w.acquire() {
		t.Fatal("couldn't take the lock again")
	}
	w.recordJobs(ctx)
	if n := historyLen(t, w); n != 2 {
		t.Errorf("%d jobs in the history after watching again, want 2", n)
	}
}

func TestWaitReturnsOnceTheWatcherHasRecordedAndSaved(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	dir := t.TempDir()
	film := filepath.Join(dir, "Example Film (2004).mkv")
	writeVideo(t, film)
	s := DefaultSettings()
	s.Watch = []WatchFolder{{Path: dir}}
	q := newQueue(ctx, s, nil)
	q.Add(film)
	q.Cancel(0)
	state := filepath.Join(t.TempDir(), "cav1ar.db")

	w := Watch(ctx, q, state, false)
	cancel()
	q.Close()
	w.Wait()
	if w.Active() {
		t.Error("still watching once Wait returned")
	}
	h, err := OpenHistory(state)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	entries, err := h.Recent(context.Background(), 0, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Input != film || entries[0].Result != "canceled" {
		t.Errorf("history %+v, want the canceled job", entries)
	}
}

func TestAcquireRemovesStaleTempsFromTheOutputFolder(t *testing.T) {
	out := t.TempDir()
	temp := filepath.Join(out, ".job"+tempSuffix)
	if err := os.WriteFile(temp, []byte("half an encode"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * staleTemp)
	if err := os.Chtimes(temp, old, old); err != nil {
		t.Fatal(err)
	}
	s := DefaultSettings()
	s.Watch = []WatchFolder{{Path: t.TempDir()}}
	s.OutDir = out
	w := newTestWatcher(t, newQueue(t.Context(), s, nil))
	if !w.acquire() {
		t.Fatal("couldn't take the lock")
	}
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Errorf("a stale temp in the output folder was kept: %v", err)
	}
}

func TestRemoveStaleTempsLeavesHeldTemps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".job"+tempSuffix)
	release, err := holdTemp(path)
	if err != nil {
		t.Fatal(err)
	}
	// As if the job had been paused for longer than a temp is kept.
	old := time.Now().Add(-2 * staleTemp)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	removeStaleTemps(dir)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a held temp was removed: %v", err)
	}

	release()
	removeStaleTemps(dir)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a released stale temp was kept: %v", err)
	}
}
