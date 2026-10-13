package encode

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"syscall"
	"time"
)

// watchInterval is how often watched folders are scanned. A new video is
// queued once it looks the same on two scans in a row, so a file still being
// copied in is never picked up half-finished.
const watchInterval = 10 * time.Second

// Watcher queues videos that appear under a Queue's Settings.Watch folders,
// including in subfolders. Only one cav1ar watches at a time: the others wait
// and take over if it stops, so nothing is encoded twice.
type Watcher struct {
	q      *Queue
	state  string
	lock   *os.File // held while this Watcher is the one watching
	active atomic.Bool

	seen    map[string]map[string]bool // watched folder → videos already handled
	waiting map[string]fileSig         // new videos, as they looked on the last scan
}

type fileSig struct {
	size int64
	mod  time.Time
}

// Watch scans q's watched folders until ctx is done. Videos already in a
// folder when it is first watched are left alone. What has been seen is kept
// in the state file, so videos that arrive while cav1ar is closed are still
// picked up next time.
func Watch(ctx context.Context, q *Queue, state string) *Watcher {
	w := &Watcher{q: q, state: state, seen: map[string]map[string]bool{}, waiting: map[string]fileSig{}}
	go func() {
		defer w.release()
		tick := time.NewTicker(watchInterval)
		defer tick.Stop()
		for {
			w.scan()
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return w
}

// Active reports whether this Watcher is the one watching folders, as
// opposed to another cav1ar, such as a daemon.
func (w *Watcher) Active() bool { return w.active.Load() }

// acquire takes the lock that says this Watcher is the one watching, and loads
// what the last one saw.
func (w *Watcher) acquire() bool {
	if w.lock != nil {
		return true
	}
	if os.MkdirAll(filepath.Dir(w.state), 0o755) != nil {
		return false
	}
	lock, err := os.OpenFile(w.state+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false
	}
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		lock.Close()
		return false
	}
	w.lock = lock
	w.active.Store(true)

	w.seen = map[string]map[string]bool{}
	if data, err := os.ReadFile(w.state); err == nil {
		var saved map[string][]string
		if json.Unmarshal(data, &saved) == nil {
			for dir, paths := range saved {
				w.seen[dir] = setOf(paths)
			}
		}
	}
	return true
}

func (w *Watcher) release() {
	if w.lock != nil {
		w.lock.Close()
		w.lock = nil
		w.active.Store(false)
	}
}

func (w *Watcher) scan() {
	folders := w.q.Settings().Watch
	if len(folders) == 0 {
		w.release() // leave watching to anyone else who has folders
		return
	}
	if !w.acquire() {
		return
	}
	outputs := map[string]bool{}
	for _, j := range w.q.Jobs() {
		outputs[j.Output] = true
	}

	changed := false
	present := map[string]bool{}
	for dir := range w.seen {
		if !slices.Contains(folders, dir) {
			delete(w.seen, dir)
			changed = true
		}
	}
	for _, dir := range folders {
		videos, err := FindVideos(dir)
		if err != nil {
			continue // offline or missing; try again next scan
		}
		seen, known := w.seen[dir]
		if !known { // newly watched: everything already here is old
			w.seen[dir] = setOf(videos)
			changed = true
			continue
		}
		now := map[string]bool{}
		for _, v := range videos {
			now[v], present[v] = true, true
			if seen[v] {
				continue
			}
			if outputs[v] { // one of our own encodes
				seen[v], changed = true, true
				continue
			}
			fi, err := os.Stat(v)
			if err != nil {
				continue
			}
			sig := fileSig{fi.Size(), fi.ModTime()}
			if w.waiting[v] == sig {
				w.q.Add(v)
				seen[v], changed = true, true
				delete(w.waiting, v)
			} else {
				w.waiting[v] = sig
			}
		}
		// Forget videos that are gone, so the state file doesn't grow forever.
		for v := range seen {
			if !now[v] {
				delete(seen, v)
				changed = true
			}
		}
	}
	for v := range w.waiting {
		if !present[v] {
			delete(w.waiting, v)
		}
	}
	if changed {
		w.save()
	}
}

func (w *Watcher) save() {
	saved := map[string][]string{}
	for dir, seen := range w.seen {
		saved[dir] = slices.Sorted(maps.Keys(seen))
	}
	data, err := json.Marshal(saved)
	if err != nil {
		return
	}
	tmp := w.state + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		os.Rename(tmp, w.state)
	}
}

func setOf(paths []string) map[string]bool {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	return set
}
