package tui

import (
	"slices"
	"testing"
	"time"

	"github.com/haydenkz/cav1ar/internal/encode"
)

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
