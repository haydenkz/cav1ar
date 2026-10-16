package encode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log"
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
	fileTodo   = "todo"   // queued; queued again on the next run without being read, unless it changes
)

// noteArrived marks a todo video that arrived since the watcher last looked,
// rather than being part of the library already there, so it goes ahead of
// the rest, including on later runs.
const noteArrived = "arrived"

// Watcher keeps the videos under a Queue's Settings.Watch folders, including
// in subfolders, matching the settings: it queues the ones that don't, as
// they arrive. Only one cav1ar watches at a time: the others wait and take
// over if it stops, so nothing is handled twice.
type Watcher struct {
	q           *Queue
	state       string   // the state database
	db          *stateDB // open while this Watcher is the one watching
	lock        *os.File // held while this Watcher is the one watching
	active      atomic.Bool
	retryFailed bool // retry videos whose jobs failed

	// Saved in the state database between runs.
	settings string               // fingerprint of the settings the files were checked against
	folders  map[string]bool      // watched folders scanned before
	files    map[string]fileState // what is known about each video
	dirty    map[string]bool      // files changed since they were last saved

	waiting  map[string]fileState // new or changed videos, as they looked on the last scan
	recorded map[int]time.Time    // jobs whose outcome is in files, and when they finished
	requeued map[string]bool      // videos found to need work on an earlier run, queued again on this one

	// Videos changed after this arrived since the watcher last looked, and
	// go ahead of the rest: the end of the last run's last scan, or the
	// start of this run if there wasn't one.
	arrivedAfter time.Time

	saved, results atomic.Int64 // totals of every finished job that rewrote a file; see stateDB.totals
}

func (w *Watcher) setFile(path string, f fileState) {
	w.files[path], w.dirty[path] = f, true
}

func (w *Watcher) forgetFile(path string) {
	delete(w.files, path)
	w.dirty[path] = true
}

// Totals says how much smaller the results of every job that rewrote a file
// came out than their sources, and how many there were, across every run of
// whichever cav1ar was watching. They're 0 when this Watcher isn't the one.
func (w *Watcher) Totals() (saved int64, results int) {
	return w.saved.Load(), int(w.results.Load())
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
// otherwise. What is known about each video is kept in the state database at
// state, so one that arrives while cav1ar is closed is still picked up next
// time, and one already checked isn't read again until it or the settings
// change. retryFailed gives videos whose jobs failed another go.
func Watch(ctx context.Context, q *Queue, state string, retryFailed bool) *Watcher {
	w := &Watcher{q: q, state: state, retryFailed: retryFailed}
	go func() {
		defer func() {
			if w.lock != nil {
				w.recordJobs(ctx) // those that finished since the last scan
			}
			w.release()
		}()
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
	// The lock keeps the name it had when the state was kept in watch.json,
	// so a cav1ar from before the database and one from after take turns.
	lock, err := os.OpenFile(filepath.Join(filepath.Dir(w.state), "watch.json.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false
	}
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		lock.Close()
		return false
	}
	db, err := openState(w.state)
	if err != nil {
		log.Printf("cav1ar: can't open %s, so not watching: %v", w.state, err)
		lock.Close()
		return false
	}
	st, err := db.load()
	if err != nil {
		log.Printf("cav1ar: can't read %s, so not watching: %v", w.state, err)
		db.Close()
		lock.Close()
		return false
	}
	w.lock, w.db = lock, db
	w.active.Store(true)
	w.settings, w.folders, w.files = st.settings, st.folders, st.files
	w.dirty, w.requeued = map[string]bool{}, map[string]bool{}
	if w.arrivedAfter = st.scanned; w.arrivedAfter.IsZero() {
		w.arrivedAfter = time.Now()
	}
	if saved, results, err := db.totals(); err == nil {
		w.saved.Store(saved)
		w.results.Store(int64(results))
	}
	w.waiting, w.recorded = map[string]fileState{}, map[int]time.Time{}
	for path, f := range w.files {
		if f.Status == fileFailed && w.retryFailed {
			w.forgetFile(path)
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
		w.save()
		w.db.Close()
		w.lock.Close()
		w.lock, w.db = nil, nil
		w.active.Store(false)
		w.saved.Store(0)
		w.results.Store(0)
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
				w.forgetFile(path)
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
				if known.Status == fileTodo && !w.requeued[v] {
					// Found to need work on an earlier run, and unchanged
					// since, so there's no need to read it again.
					w.queue(v, known.Note == noteArrived)
				}
				continue
			}
			if leaveAlone {
				now.Status = fileLeft
				w.setFile(v, now)
				changed = true
				continue
			}
			if last, ok := w.waiting[v]; !ok || !last.same(now) {
				w.waiting[v] = now // new or changed: wait until it stops changing
				continue
			}
			delete(w.waiting, v)
			now.Status = fileOK
			if needsWork(ctx, v, s) {
				now.Status = fileTodo
				if changedAt(v).After(w.arrivedAfter) {
					now.Note = noteArrived
				}
				w.queue(v, now.Note == noteArrived)
			}
			w.setFile(v, now)
			changed = true
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
			w.forgetFile(path)
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

// queue queues v, ahead of the rest if it has just arrived: its job is likely
// to be watched sooner than the backlog's.
func (w *Watcher) queue(v string, arrived bool) {
	if arrived {
		w.q.AddNext(v)
	} else {
		w.q.Add(v)
	}
	w.requeued[v] = true
}

// changedAt is when path was last changed as a file, rather than as content:
// when it was created, or moved or renamed into place. Unlike its
// modification time, unpacking it from an archive can't set that back.
func changedAt(path string) time.Time {
	var st syscall.Stat_t
	if syscall.Stat(path, &st) != nil {
		return time.Time{}
	}
	return time.Unix(st.Ctim.Unix())
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

// recordJobs notes what finished jobs did to the files they worked on, and
// adds them to the history.
func (w *Watcher) recordJobs(ctx context.Context) bool {
	changed := false
	mark := func(path, status, note string) {
		if f, ok := statFile(path); ok {
			f.Status, f.Note = status, note
			w.setFile(path, f)
			changed = true
		}
	}
	record := func(j Job, result string) {
		if err := w.db.addJob(j, result); err != nil {
			log.Printf("cav1ar: can't add a job to the history: %v", err)
			return
		}
		rewrote := j.Action == ActionRemux || j.Action == ActionEncode
		if result == "done" && rewrote && j.InSize > 0 && j.OutSize > 0 {
			w.saved.Add(j.InSize - j.OutSize)
			w.results.Add(1)
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
			record(j, "done")
		case Failed:
			mark(j.Input, fileFailed, j.Err.Error())
			record(j, "failed")
		case Canceled:
			if ctx.Err() == nil { // canceled by the user, not by cav1ar stopping
				mark(j.Input, fileLeft, "canceled")
				record(j, "canceled")
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

// matchRules goes up whenever what makes a file match its settings changes,
// such as stripping coming to take chapters too, so that files found to
// match before are checked again.
const matchRules = 2

// watchedPlace is a WatchFolder without its priority, encoded as one was
// before it had one, so adding priorities doesn't change a fingerprint.
type watchedPlace struct {
	Path string     `json:"path"`
	Kind FolderKind `json:"kind,omitempty"`
}

// fingerprint sums up the settings that decide whether a file matches them,
// so files are checked again when those change. How things are encoded
// doesn't count: it never makes a finished file not match.
func fingerprint(s Settings) string {
	key := struct {
		Rules                          int
		MaxHeight                      int
		AudioCodec                     AudioCodec
		CopyAudio, Downmix             bool
		Language                       string
		AudioTracks, Subtitles         TrackFilter
		TidySubs, StripMetadata        bool
		Title, ReleaseGroup, Name, Out string
		Watch                          []watchedPlace
		Preroll                        string
	}{matchRules, s.MaxHeight, s.AudioCodec, s.AudioKbps == AudioCopy, s.Downmix, s.Language, s.AudioTracks, s.Subtitles,
		s.TidySubs, s.StripMetadata, s.Title, s.ReleaseGroup, s.NameTemplate, s.OutDir, nil, s.Preroll}
	// Which folders, and how their videos are named, decide whether a file
	// matches; the order jobs run in doesn't.
	for _, f := range s.Watch {
		key.Watch = append(key.Watch, watchedPlace{f.Path, f.Kind})
	}
	if s.Preroll != "" {
		// A new clip at the same path counts as a change. One that can't be
		// read keeps its path, so it still differs from having none.
		if id, err := prerollID(s.Preroll); err == nil {
			key.Preroll = id
		}
	}
	data, _ := json.Marshal(key)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// save writes what has changed to the state database.
func (w *Watcher) save() {
	files := make(map[string]*fileState, len(w.dirty))
	for path := range w.dirty {
		if f, ok := w.files[path]; ok {
			files[path] = &f
		} else {
			files[path] = nil
		}
	}
	if err := w.db.save(w.settings, w.folders, files); err != nil {
		log.Printf("cav1ar: can't save what's known about the videos: %v", err)
		return
	}
	clear(w.dirty)
}

// removeStaleTemps deletes results left half-written under root by a cav1ar
// that stopped abruptly.
func removeStaleTemps(root string) {
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !IsTemp(path) {
			return nil
		}
		if fi, err := d.Info(); err == nil && time.Since(fi.ModTime()) > staleTemp && !tempInUse(path) {
			os.Remove(path)
		}
		return nil
	})
}

// holdTemp marks path, a result or intermediate a job is about to write, as
// in use until release is called. removeStaleTemps in any cav1ar then leaves
// it be, however long a pause leaves it untouched.
func holdTemp(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

// tempInUse reports whether a job holds path.
func tempInUse(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil
}
