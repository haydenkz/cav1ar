package encode

import (
	"context"
	"testing"
)

func TestAddNextGoesAheadOfTheRest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := DefaultSettings()
	s.Workers = 1
	q := NewQueue(ctx, s)
	defer func() { cancel(); q.Close() }()

	q.SetPaused(true)
	q.Add("/nonexistent/backlog-1.mkv")
	q.Add("/nonexistent/backlog-2.mkv")
	q.AddNext("/nonexistent/arrived.mkv")
	q.SetPaused(false)

	for {
		evs := q.Next()
		if evs == nil {
			t.Fatal("the queue closed before any job started")
		}
		for _, ev := range evs {
			if ev.Job.State == Running {
				if ev.Job.Input != "/nonexistent/arrived.mkv" {
					t.Fatalf("%s started first", ev.Job.Input)
				}
				return
			}
		}
	}
}
