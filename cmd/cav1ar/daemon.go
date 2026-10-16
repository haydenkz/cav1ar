package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/haydenkz/cav1ar/internal/config"
	"github.com/haydenkz/cav1ar/internal/encode"
	"github.com/haydenkz/cav1ar/internal/remote"
)

// runDaemon encodes without the UI, logging a line as each job starts and
// finishes, and sharing its queue with any UI that opens. It picks up settings
// saved from the UI as they change. Given files to encode and no folders to
// watch, it exits once they are done; otherwise it runs until ctx is canceled.
func runDaemon(ctx context.Context, q *encode.Queue, w *encode.Watcher, batch bool) error {
	if toJournal() {
		log.SetFlags(0) // the journal adds its own timestamps
	}
	s := q.Settings()
	if err := encode.CheckEncoder(s.Encoder); err != nil {
		return err
	}
	log.Printf("cav1ar: %s", s.Summary())
	logWatch(s)
	srv, err := remote.Serve(q, w)
	if err != nil {
		log.Printf("cav1ar: the UI can't show this daemon: %v", err)
	} else {
		defer srv.Close()
	}
	go followSettings(ctx, q, srv)
	time.AfterFunc(2*time.Second, func() {
		if len(q.Settings().Watch) > 0 && !w.Active() {
			log.Print("cav1ar: another cav1ar is watching the folders; this one takes over if it stops")
		}
	})

	jobs := map[int]*jobLog{}
	for {
		evs := q.Next()
		if evs == nil {
			log.Print("cav1ar: stopping")
			return nil
		}
		for _, ev := range evs {
			logEvent(ev, jobs)
		}
		if srv != nil {
			srv.Publish(evs)
		}
		if batch && len(q.Settings().Watch) == 0 && allFinished(q.Jobs()) {
			return nil
		}
	}
}

// jobLog is what the daemon remembers about a job it has logged.
type jobLog struct {
	state encode.State
	stage string // the stage it's in, which a failure is put down to
}

// logEvent logs a job's changes of state and the lines cav1ar writes about
// it, leaving out ffmpeg's output. Every line starts with the job's number,
// so jobs running side by side can be told apart, and the start of each
// stage is marked out, so a job's lines read as the stages it went through.
func logEvent(ev encode.Event, jobs map[int]*jobLog) {
	j := ev.Job
	name := filepath.Base(j.Input)
	id := fmt.Sprintf("#%d", j.ID)
	l, seen := jobs[j.ID]
	if !seen {
		l = &jobLog{}
		jobs[j.ID] = l
	}
	if stage, ok := encode.Stage(ev.Line); ok {
		l.stage = stage
		log.Printf("%s   ▸ %s", id, stage)
	} else if msg, ok := encode.Own(ev.Line); ok && !strings.HasPrefix(msg, "ffmpeg ") {
		log.Printf("%s     %s", id, msg)
	}
	if seen && l.state == j.State {
		return
	}
	l.state = j.State
	at := ""
	if l.stage != "" {
		at = " while " + l.stage
	}
	switch j.State {
	case encode.Queued:
		l.stage = "" // a retry starts over
		log.Printf("%s queued %s", id, j.Input)
	case encode.Running:
		log.Printf("%s started %s", id, name)
	case encode.Done:
		if j.Action == encode.ActionNone {
			log.Printf("%s done %s: nothing to do", id, name)
			break
		}
		note := ""
		if j.Note != "" {
			note = ", " + j.Note
		}
		log.Printf("%s done %s → %s (%s in %s%s)", id, name, filepath.Base(j.Output), j.Action, j.Elapsed().Round(time.Second), note)
	case encode.Failed:
		log.Printf("%s failed %s%s: %v", id, name, at, j.Err)
	case encode.Canceled:
		log.Printf("%s canceled %s%s", id, name, at)
	}
}

// followSettings applies settings saved from the UI while the daemon runs,
// and tells any UI showing the daemon.
func followSettings(ctx context.Context, q *encode.Queue, srv *remote.Server) {
	path, err := config.Path()
	if err != nil {
		return
	}
	modTime := func() time.Time {
		fi, err := os.Stat(path)
		if err != nil {
			return time.Time{}
		}
		return fi.ModTime()
	}
	last := modTime()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if mod := modTime(); !mod.Equal(last) {
			last = mod
			s, err := config.Load()
			if err == nil {
				err = encode.CheckEncoder(s.Encoder)
			}
			if err != nil {
				log.Printf("cav1ar: ignoring changed settings: %v", err)
				continue
			}
			q.SetSettings(s)
			if srv != nil {
				srv.SetSettings(s)
			}
			log.Printf("cav1ar: settings changed: %s", s.Summary())
			logWatch(s)
		}
	}
}

func logWatch(s encode.Settings) {
	if len(s.Watch) == 0 {
		log.Print("cav1ar: no folders watched; add some in the cav1ar settings")
	}
	for _, f := range s.Watch {
		log.Printf("cav1ar: watching %s", f)
	}
}

// runDryRun says what a job would do to each video in paths, or in the
// watched folders if none are given, without doing any of it.
func runDryRun(ctx context.Context, s encode.Settings, paths []string) error {
	if len(paths) == 0 {
		paths = s.WatchPaths()
	}
	if len(paths) == 0 {
		return errors.New("give files or folders to check, or watch some")
	}
	counts := map[encode.Action]int{}
	failed := 0
	for _, path := range paths {
		videos := []string{path}
		if fi, err := os.Stat(path); err == nil && fi.IsDir() {
			var err error
			if videos, err = encode.FindVideos(path); err != nil {
				return err
			}
		}
		for _, v := range videos {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			d, err := encode.Inspect(ctx, v, s)
			switch {
			case err != nil:
				failed++
				fmt.Printf("FAIL   %s\n       %v\n", v, err)
			case d.Action == encode.ActionNone:
				counts[d.Action]++
			default:
				counts[d.Action]++
				fmt.Printf("%-6s %s\n", strings.ToUpper(string(d.Action)), v)
				if d.Output != v {
					fmt.Printf("    -> %s\n", filepath.Base(d.Output))
				}
				if len(d.Reasons) > 0 {
					fmt.Printf("       %s\n", strings.Join(d.Reasons, " · "))
				}
				for _, n := range d.Notes {
					fmt.Printf("       %s\n", n)
				}
			}
		}
	}
	fmt.Printf("\n%d to encode, %d to remux, %d to retag, %d to rename, %d fine, %d failed\n",
		counts[encode.ActionEncode], counts[encode.ActionRemux], counts[encode.ActionRetag], counts[encode.ActionRename],
		counts[encode.ActionNone], failed)
	return nil
}

// toJournal reports whether stderr goes to the systemd journal.
func toJournal() bool {
	var dev, ino uint64
	if _, err := fmt.Sscanf(os.Getenv("JOURNAL_STREAM"), "%d:%d", &dev, &ino); err != nil {
		return false
	}
	var st syscall.Stat_t
	return syscall.Fstat(2, &st) == nil && st.Dev == dev && st.Ino == ino
}

func allFinished(jobs []encode.Job) bool {
	for _, j := range jobs {
		if !j.State.Finished() {
			return false
		}
	}
	return true
}
