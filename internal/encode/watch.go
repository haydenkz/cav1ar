package encode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"syscall"
	"time"
)

// watchInterval is how often watched folders are scanned. A new or changed
// video is queued once it looks the same on two scans in a row, so a file
// still being copied in is never picked up half-finished.
const watchInterval = 10 * time.Second

// staleTemp is how long a result being written can go untouched before a
// watcher starting up takes it for one left behind and deletes it.
const staleTemp = time.Hour

// What a Watcher knows about a video.
const (
	fileOK     = "ok"     // matches the settings, or is the result of a job
	fileKept   = "kept"   // an original kept beside its result
	fileFailed = "failed" // its job failed; tried again once it changes, or when retrying failures
	fileLeft   = "left"   // left alone: there before its folder was watched, or canceled
)

// Watcher keeps the videos under a Queue's Settings.Watch folders, including
// in subfolders, matching the settings: it queues the ones that don't, as
// they arrive. Only one cav1ar watches at a time: the others wait and take
// over if it stops, so nothing is handled twice.
type Watcher struct {
	q           *Queue
	state       string
	lock        *os.File // held while this Watcher is the one watching
	active      atomic.Bool
	retryFailed bool // retry videos whose jobs failed

	// Saved in the state file between runs.
	settings string               // fingerprint of the settings the files were checked against
	folders  map[string]bool      // watched folders scanned before
	files    map[string]fileState // what is known about each video

	waiting  map[string]fileState // new or changed videos, as they looked on the last scan
	recorded map[int]time.Time    // jobs whose outcome is in files, and when they finished
}

type fileState struct {
	Size   int64  `json:"size"`
	Mod    int64  `json:"mod"` // modification time, in Unix nanoseconds
	Status string `json:"status,omitempty"`
	Note   string `json:"note,omitempty"`
}

func statFile(path string) (fileState, bool) {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return fileState{}, false
	}
	return fileState{Size: fi.Size(), Mod: fi.ModTime().UnixNano()}, true
}

func (f fileState) same(g fileState) bool { return f.Size == g.Size && f.Mod == g.Mod }

// Watch scans q's watched folders until ctx is done. Videos already in a
// folder when it is first watched are left alone, unless the settings say
// otherwise. What is known about each video is kept in the state file, so
// one that arrives while cav1ar is closed is still picked up next time, and
// one already checked isn't read again until it or the settings change.
// retryFailed gives videos whose jobs failed another go.
func Watch(ctx context.Context, q *Queue, state string, retryFailed bool) *Watcher {
	w := &Watcher{q: q, state: state, retryFailed: retryFailed}
	go func() {
		defer w.release()
		tick := time.NewTicker(watchInterval)
		defer tick.Stop()
		for {
			w.scan(ctx)
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
// what the last one knew.
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
	w.load()
	w.waiting, w.recorded = map[string]fileState{}, map[int]time.Time{}
	for path, f := range w.files {
		if f.Status == fileFailed && w.retryFailed {
			delete(w.files, path)
		}
	}
	for _, f := range w.q.Settings().Watch {
		removeStaleTemps(f.Path)
	}
	if dir, err := workDir(); err == nil {
		removeStaleTemps(dir)
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

func (w *Watcher) scan(ctx context.Context) {
	s := w.q.Settings()
	if len(s.Watch) == 0 {
		w.release() // leave watching to anyone else who has folders
		return
	}
	if !w.acquire() {
		return
	}
	changed := w.recordJobs(ctx)
	if fp := fingerprint(s); fp != w.settings {
		// Check everything against the new settings, apart from what is
		// meant to be left alone.
		for path, f := range w.files {
			if f.Status != fileLeft && f.Status != fileKept {
				delete(w.files, path)
			}
		}
		w.settings, changed = fp, true
	}
	busy := map[string]bool{}
	for _, j := range w.q.Jobs() {
		if !j.State.Finished() {
			busy[j.Input], busy[j.Output] = true, true
		}
	}

	scanned, present := map[string]bool{}, map[string]bool{}
	checked := 0
	for _, folder := range s.Watch {
		videos, err := FindVideos(folder.Path)
		if err != nil {
			continue // offline or missing; try again next scan
		}
		scanned[folder.Path] = true
		leaveAlone := !w.folders[folder.Path] && !s.WatchExisting
		for _, v := range videos {
			if ctx.Err() != nil {
				return
			}
			present[v] = true
			now, ok := statFile(v)
			if busy[v] || !ok {
				continue
			}
			if known, ok := w.files[v]; ok && known.same(now) {
				continue
			}
			if leaveAlone {
				now.Status = fileLeft
				w.files[v], changed = now, true
				continue
			}
			if last, ok := w.waiting[v]; !ok || !last.same(now) {
				w.waiting[v] = now // new or changed: wait until it stops changing
				continue
			}
			delete(w.waiting, v)
			if needsWork(ctx, v, s) {
				w.q.Add(v)
			} else {
				now.Status = fileOK
				w.files[v], changed = now, true
			}
			// Checking a big folder for the first time can take a while,
			// so save as it goes.
			if checked++; checked%100 == 0 {
				w.save()
			}
		}
		w.folders[folder.Path] = true
	}

	// Forget videos that are gone, and folders no longer watched, so the
	// state file doesn't grow forever.
	for path := range w.files {
		if !present[path] && (scanned[folderPath(s, path)] || folderPath(s, path) == "") {
			delete(w.files, path)
			changed = true
		}
	}
	for dir := range w.folders {
		if !slices.Contains(s.WatchPaths(), dir) {
			delete(w.folders, dir)
			changed = true
		}
	}
	for v := range w.waiting {
		if !present[v] {
			delete(w.waiting, v)
		}
	}
	if changed || checked > 0 {
		w.save()
	}
}

// needsWork reports whether a job would do anything to path. When it can't
// tell, it says yes, so the job fails and shows why.
func needsWork(ctx context.Context, path string, s Settings) bool {
	info, err := probe(ctx, path)
	if err != nil {
		return ctx.Err() == nil
	}
	p, err := newPlan(path, s, info)
	return err != nil || p.action() != ActionNone
}

// recordJobs notes what finished jobs did to the files they worked on.
func (w *Watcher) recordJobs(ctx context.Context) bool {
	changed := false
	mark := func(path, status, note string) {
		if f, ok := statFile(path); ok {
			f.Status, f.Note = status, note
			w.files[path], changed = f, true
		}
	}
	for _, j := range w.q.Jobs() {
		if !j.State.Finished() || w.recorded[j.ID].Equal(j.Finished) {
			continue
		}
		w.recorded[j.ID] = j.Finished
		switch j.State {
		case Done:
			mark(j.Output, fileOK, "")
			if j.Input != j.Output {
				mark(j.Input, fileKept, j.Output) // unless it was deleted
			}
		case Failed:
			mark(j.Input, fileFailed, j.Err.Error())
		case Canceled:
			if ctx.Err() == nil { // canceled by the user, not by cav1ar stopping
				mark(j.Input, fileLeft, "canceled")
			}
		}
	}
	return changed
}

// folderPath is the watched folder path is in, or "" if none.
func folderPath(s Settings, path string) string {
	f := s.folderOf(path)
	if !slices.Contains(s.Watch, f) {
		return ""
	}
	return f.Path
}

// fingerprint sums up the settings that decide whether a file matches them,
// so files are checked again when those change. How things are encoded
// doesn't count: it never makes a finished file not match.
func fingerprint(s Settings) string {
	key := struct {
		MaxHeight                      int
		AudioCodec                     AudioCodec
		CopyAudio, Downmix             bool
		Language                       string
		AudioTracks, Subtitles         TrackFilter
		TidySubs, StripMetadata        bool
		Title, ReleaseGroup, Name, Out string
		Watch                          []WatchFolder
		Preroll                        string
	}{s.MaxHeight, s.AudioCodec, s.AudioKbps == AudioCopy, s.Downmix, s.Language, s.AudioTracks, s.Subtitles,
		s.TidySubs, s.StripMetadata, s.Title, s.ReleaseGroup, s.NameTemplate, s.OutDir, s.Watch, s.Preroll}
	if s.Preroll != "" {
		key.Preroll, _ = prerollID(s.Preroll) // a new clip at the same path counts as a change
	}
	data, _ := json.Marshal(key)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// watchState is the state file's contents.
type watchState struct {
	Version  int                  `json:"version"`
	Settings string               `json:"settings"`
	Folders  []string             `json:"folders"`
	Files    map[string]fileState `json:"files"`
}

func (w *Watcher) load() {
	w.settings, w.folders, w.files = "", map[string]bool{}, map[string]fileState{}
	data, err := os.ReadFile(w.state)
	if err != nil {
		return
	}
	var st watchState
	if json.Unmarshal(data, &st) == nil && st.Version == 2 {
		w.settings = st.Settings
		w.folders = setOf(st.Folders)
		if st.Files != nil {
			w.files = st.Files
		}
		return
	}
	// The first version only listed the videos seen in each folder, all of
	// which were left alone.
	var seen map[string][]string
	if json.Unmarshal(data, &seen) == nil {
		for dir, paths := range seen {
			w.folders[dir] = true
			for _, p := range paths {
				if f, ok := statFile(p); ok {
					f.Status = fileLeft
					w.files[p] = f
				}
			}
		}
	}
}

func (w *Watcher) save() {
	st := watchState{Version: 2, Settings: w.settings, Files: w.files}
	for dir := range w.folders {
		st.Folders = append(st.Folders, dir)
	}
	slices.Sort(st.Folders)
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp := w.state + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		os.Rename(tmp, w.state)
	}
}

// removeStaleTemps deletes results left half-written under root by a cav1ar
// that stopped abruptly.
func removeStaleTemps(root string) {
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !IsTemp(path) {
			return nil
		}
		if fi, err := d.Info(); err == nil && time.Since(fi.ModTime()) > staleTemp {
			os.Remove(path)
		}
		return nil
	})
}
