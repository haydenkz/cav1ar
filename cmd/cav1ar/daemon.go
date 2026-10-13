package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/haydenkz/cav1ar/internal/config"
	"github.com/haydenkz/cav1ar/internal/encode"
)

// runDaemon encodes without the UI, logging a line as each job starts and
// finishes. It picks up settings saved from the UI as they change. Given files
// to encode and no folders to watch, it exits once they are done; otherwise
// it runs until ctx is canceled.
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
	go followSettings(ctx, q)
	time.AfterFunc(2*time.Second, func() {
		if len(q.Settings().Watch) > 0 && !w.Active() {
			log.Print("cav1ar: another cav1ar is watching the folders; this one takes over if it stops")
		}
	})

	state := map[int]encode.State{}
	for {
		evs := q.Next()
		if evs == nil {
			log.Print("cav1ar: stopping")
			return nil
		}
		for _, ev := range evs {
			logEvent(ev, state)
		}
		if batch && len(q.Settings().Watch) == 0 && allFinished(q.Jobs()) {
			return nil
		}
	}
}

func logEvent(ev encode.Event, state map[int]encode.State) {
	j := ev.Job
	name := filepath.Base(j.Input)
	if strings.HasPrefix(ev.Line, "cav1ar: ") && !strings.HasPrefix(ev.Line, "cav1ar: ffmpeg ") {
		log.Printf("%s: %s", name, strings.TrimPrefix(ev.Line, "cav1ar: "))
	}
	prev, seen := state[j.ID]
	if seen && prev == j.State {
		return
	}
	state[j.ID] = j.State
	switch j.State {
	case encode.Queued:
		log.Printf("queued %s", j.Input)
	case encode.Running:
		log.Printf("encoding %s", name)
	case encode.Done:
		note := ""
		if j.Note != "" {
			note = ", " + j.Note
		}
		log.Printf("done %s → %s (%s%s)", name, filepath.Base(j.Output), j.Elapsed().Round(time.Second), note)
	case encode.Failed:
		log.Printf("failed %s: %v", name, j.Err)
	case encode.Canceled:
		log.Printf("canceled %s", name)
	}
}

// followSettings applies settings saved from the UI while the daemon runs.
func followSettings(ctx context.Context, q *encode.Queue) {
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
			log.Printf("cav1ar: settings changed: %s", s.Summary())
			logWatch(s)
		}
	}
}

func logWatch(s encode.Settings) {
	if len(s.Watch) == 0 {
		log.Print("cav1ar: no folders watched; add some in the cav1ar settings")
	}
	for _, dir := range s.Watch {
		log.Printf("cav1ar: watching %s", dir)
	}
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
