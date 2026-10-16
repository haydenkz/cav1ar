package encode

import (
	"cmp"
	"context"
	"errors"
	"os"
	"slices"
	"sync"
	"syscall"
	"time"
)

// Queue encodes files in the order they were added, keeping up to
// Settings.Workers of them running at once.
type Queue struct {
	ctx    context.Context
	wg     sync.WaitGroup
	notify chan struct{} // signals Next that events are pending

	mu       sync.Mutex
	settings Settings
	paused   bool
	pausedAt time.Time
	holding  bool // new jobs are held back, while running ones finish
	closed   bool
	running  int
	jobs     []*Job
	cancels  map[int]context.CancelFunc
	procs    map[int]*os.Process // running encoder processes, by job ID
	pending  []Event
}

// NewQueue returns a queue that runs jobs until ctx is done. Canceling ctx
// stops every running encode; call Close afterwards to let them clean up.
func NewQueue(ctx context.Context, s Settings) *Queue {
	return &Queue{
		ctx:      ctx,
		settings: s,
		notify:   make(chan struct{}, 1),
		cancels:  make(map[int]context.CancelFunc),
		procs:    make(map[int]*os.Process),
	}
}

// Add queues an encode of in. Its output path is chosen when it starts.
// Jobs run in order of the priority of the watched folder their video is
// in, highest first.
func (q *Queue) Add(in string) { q.add(in, false) }

// AddNext queues an encode of in ahead of everything of its priority queued
// with Add, behind anything of its priority queued with AddNext before it.
func (q *Queue) AddNext(in string) { q.add(in, true) }

func (q *Queue) add(in string, next bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j := &Job{ID: len(q.jobs), Input: in, State: Queued, Next: next, Priority: q.settings.priorityOf(in)}
	if fi, err := os.Stat(in); err == nil {
		j.InSize = fi.Size()
	}
	q.jobs = append(q.jobs, j)
	q.emitLocked(*j, "")
	q.dispatchLocked()
}

// Cancel stops a running job or removes a queued one from the line.
func (q *Queue) Cancel(id int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j := q.jobLocked(id)
	switch {
	case j == nil:
	case j.State == Queued:
		j.State = Canceled
		q.emitLocked(*j, "")
	case j.State == Running:
		q.cancels[id]()
	}
}

// Retry puts a failed or canceled job back in the queue.
func (q *Queue) Retry(id int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j := q.jobLocked(id)
	if j == nil || (j.State != Failed && j.State != Canceled) {
		return
	}
	*j = Job{ID: j.ID, Input: j.Input, InSize: j.InSize, State: Queued, Next: j.Next, Priority: q.settings.priorityOf(j.Input)}
	q.emitLocked(*j, "")
	q.dispatchLocked()
}

// Settings returns the settings that new jobs will start with.
func (q *Queue) Settings() Settings {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.settings
}

// SetSettings changes the settings for jobs that have not started yet,
// including their priorities. Running jobs keep the settings they started
// with.
func (q *Queue) SetSettings(s Settings) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.settings = s
	for _, j := range q.jobs {
		if p := s.priorityOf(j.Input); j.State == Queued && j.Priority != p {
			j.Priority = p
			q.emitLocked(*j, "")
		}
	}
	q.dispatchLocked()
}

// SetPaused suspends running encodes and holds back new ones, or carries on
// where they left off.
func (q *Queue) SetPaused(paused bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if paused == q.paused {
		return
	}
	q.paused = paused
	sig := syscall.SIGCONT
	if paused {
		sig, q.pausedAt = syscall.SIGSTOP, time.Now()
	}
	for id, p := range q.procs {
		p.Signal(sig)
		if !paused {
			j := q.jobs[id]
			j.PausedFor += time.Since(q.pausedAt)
			q.emitLocked(*j, "")
		}
	}
	q.dispatchLocked()
}

// Paused reports whether the queue is paused.
func (q *Queue) Paused() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.paused
}

// SetHolding holds back new jobs while letting running ones finish, or lets
// new ones start again.
func (q *Queue) SetHolding(holding bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.holding = holding
	q.dispatchLocked()
}

// Holding reports whether new jobs are held back.
func (q *Queue) Holding() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.holding
}

// Jobs returns a snapshot of every job, in the order they were added.
func (q *Queue) Jobs() []Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Job, len(q.jobs))
	for i, j := range q.jobs {
		out[i] = *j
	}
	return out
}

// Next blocks until events are available and returns all of them in order.
// It returns nil once the queue's context is done. Events accumulate until
// Next is called, so whoever owns the queue should keep calling it.
func (q *Queue) Next() []Event {
	for {
		q.mu.Lock()
		if len(q.pending) > 0 {
			evs := q.pending
			q.pending = nil
			q.mu.Unlock()
			return evs
		}
		q.mu.Unlock()

		select {
		case <-q.notify:
		case <-q.ctx.Done():
			return nil
		}
	}
}

// Close stops new jobs from starting and waits for running ones to exit.
// Cancel the queue's context first to make them exit promptly.
func (q *Queue) Close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.wg.Wait()
}

// dispatchLocked starts queued jobs in the order they run until the worker
// limit is reached. The caller must hold q.mu.
func (q *Queue) dispatchLocked() {
	if q.paused || q.holding || q.closed || q.ctx.Err() != nil {
		return
	}
	var waiting []*Job
	for _, j := range q.jobs {
		if j.State == Queued {
			waiting = append(waiting, j)
		}
	}
	slices.SortStableFunc(waiting, func(a, b *Job) int { return RunOrder(*a, *b) })
	for _, j := range waiting {
		if q.running >= q.settings.Workers {
			return
		}
		ctx, cancel := context.WithCancel(q.ctx)
		q.cancels[j.ID] = cancel
		q.running++
		j.State = Running
		j.Started = time.Now()
		q.emitLocked(*j, "")
		s := q.settings
		q.wg.Go(func() { q.run(ctx, j, s) })
	}
}

// RunOrder compares two queued jobs by the order they run in: higher
// priority first, then those queued with AddNext, then the order they were
// added (which a stable sort keeps).
func RunOrder(a, b Job) int {
	return cmp.Or(cmp.Compare(b.Priority, a.Priority), cmp.Compare(b2i(b.Next), b2i(a.Next)))
}

func (q *Queue) run(ctx context.Context, j *Job, s Settings) {
	// Each update edits j under the lock, then tells Next about it.
	update := func(line string, edit func()) {
		q.mu.Lock()
		defer q.mu.Unlock()
		edit()
		q.emitLocked(*j, line)
	}
	err := encodeFile(ctx, j.Input, s, updates{
		planned: func(a Action, out string) { update("", func() { j.Action, j.Output = a, out }) },
		started: func(p *os.Process) {
			q.mu.Lock()
			defer q.mu.Unlock()
			q.procs[j.ID] = p
			if q.paused { // paused while the job was getting ready
				p.Signal(syscall.SIGSTOP)
			}
		},
		progress: func(p progress) {
			update("", func() {
				j.Progress, j.FPS, j.Speed = p.done, p.fps, p.speed
				j.Duration, j.OutSize = p.duration, p.size
			})
		},
		log:  func(line string) { update(line, func() {}) },
		note: func(note string) { update("", func() { j.Note = note }) },
		step: func(step string) { update("", func() { j.Step = step }) },
	})

	q.mu.Lock()
	defer q.mu.Unlock()
	j.Finished = time.Now()
	switch {
	case ctx.Err() != nil || errors.Is(err, context.Canceled):
		j.State = Canceled
	case err != nil:
		j.State, j.Err = Failed, err
	default:
		j.State, j.Progress = Done, 1
		if fi, err := os.Stat(j.Output); err == nil {
			j.OutSize = fi.Size()
		}
	}
	q.cancels[j.ID]() // release the context only after checking whether it was canceled
	delete(q.cancels, j.ID)
	delete(q.procs, j.ID)
	q.running--
	q.emitLocked(*j, "")
	q.dispatchLocked()
}

// emitLocked records an event for Next. The caller must hold q.mu, which keeps
// events in the same order as the changes they describe.
func (q *Queue) emitLocked(j Job, line string) {
	q.pending = append(q.pending, Event{Job: j, Line: line})
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *Queue) jobLocked(id int) *Job {
	if id < 0 || id >= len(q.jobs) {
		return nil
	}
	return q.jobs[id]
}
