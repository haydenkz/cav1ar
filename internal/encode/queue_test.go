package encode

import (
	"context"
	"testing"
)

// cpuDevice stands in for whatever a test machine can encode on.
var cpuDevice = Device{ID: "cpu", Kind: EncoderCPU, Name: "CPU"}

func TestAddNextGoesAheadOfTheRest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := DefaultSettings()
	s.Encoders = map[string]int{"cpu": 1}
	q := newQueue(ctx, s, []Device{cpuDevice})
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
