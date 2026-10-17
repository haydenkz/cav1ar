package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/haydenkz/cav1ar/internal/encode"
)

// Quitting while the UI's own queue encodes asks first, by ctrl+c as by q,
// and pressing the key again quits.
func TestQuittingWhileEncodingAsksFirst(t *testing.T) {
	for _, k := range []string{"q", "ctrl+c"} {
		m, _ := newTestModel(t)
		m.apply([]encode.Event{{Job: encode.Job{ID: 0, Input: "/films/a.mkv", State: encode.Running}}})
		next, cmd := m.Update(keyPress(k))
		if m = next.(Model); cmd != nil || m.confirmQuit != k {
			t.Fatalf("%s quit with an encode running, without asking", k)
		}
		if _, cmd = m.Update(keyPress(k)); cmd == nil {
			t.Fatalf("%s pressed again didn't quit", k)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Errorf("%s pressed again didn't quit", k)
		}
	}
}

// A request that fails says why rather than seeming to work, and once the
// daemon has gone, requests are refused.
func TestRequestsThatFailSaySo(t *testing.T) {
	m, q := newTestModel(t)
	q.fail = errors.New("the cav1ar daemon has gone")
	m = press(m, "p")
	if m.holding || !strings.Contains(m.status, "has gone") || !m.statusBad {
		t.Errorf("holding %v, status %q; want the failure shown and nothing changed", m.holding, m.status)
	}

	m.lost = true
	for _, k := range []string{"p", "P", "a", "x", "r"} {
		m.status = ""
		if m = press(m, k); m.status != lostStatus || m.screen != screenQueue || q.holding {
			t.Errorf("%s with the daemon gone: status %q, screen %d; want it refused", k, m.status, m.screen)
		}
	}
}

func TestRowsAreInTheOrderJobsRun(t *testing.T) {
	at := time.Now()
	m := Model{jobs: []encode.Job{
		{ID: 0, State: encode.Done, Started: at},
		{ID: 1, State: encode.Queued},
		{ID: 2, State: encode.Queued},
		{ID: 3, State: encode.Running, Next: true, Started: at.Add(2 * time.Second)},
		{ID: 4, State: encode.Running, Started: at.Add(time.Second)},
		{ID: 5, State: encode.Queued, Next: true},
		{ID: 6, State: encode.Canceled},
		{ID: 7, State: encode.Queued, Next: true},
	}}
	if got, want := m.rows(), []int{0, 4, 3, 5, 7, 1, 2, 6}; !slices.Equal(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}

	m.hideDone = true
	if got, want := m.rows(), []int{4, 3, 5, 7, 1, 2}; !slices.Equal(got, want) {
		t.Errorf("rows with finished hidden = %v, want %v", got, want)
	}
}

func TestHidingTheSelectedJobSelectsTheNextOneShown(t *testing.T) {
	at := time.Now()
	m := Model{jobs: []encode.Job{
		{ID: 0, State: encode.Queued},
		{ID: 1, State: encode.Done, Started: at},
		{ID: 2, State: encode.Running, Started: at.Add(time.Second)},
	}}
	m.cursor, m.hideDone = 1, true
	m.layout()
	if m.cursor != 2 {
		t.Errorf("selected job %d, want 2, the one that ran after it", m.cursor)
	}
}
