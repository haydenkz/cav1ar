package encode

import (
	"context"
	"database/sql"
	"time"
)

// Entry is a finished job, as the history in the state database records it.
type Entry struct {
	ID       int64     `json:"id"`
	Input    string    `json:"input"`
	Output   string    `json:"output"`
	Action   Action    `json:"action"`
	Result   string    `json:"result"` // done, failed or canceled
	InSize   int64     `json:"in_size"`
	OutSize  int64     `json:"out_size"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Err      string    `json:"error,omitempty"`
	Device   string    `json:"device,omitempty"` // empty for jobs recorded before devices had names
}

// History reads the history a Watcher keeps, without getting in its way.
type History struct{ db *sql.DB }

// OpenHistory opens the state database at path to read, as the watching
// cav1ar writes it.
func OpenHistory(path string) (*History, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	return &History{db}, nil
}

func (h *History) Close() error { return h.db.Close() }

// Recent returns up to limit finished jobs, newest first, from before the
// one with ID before, or from the newest if before is 0. result, if not
// empty, keeps only jobs that ended that way.
func (h *History) Recent(ctx context.Context, before int64, limit int, result string) ([]Entry, error) {
	if before <= 0 {
		before = 1<<63 - 1
	}
	// A database the watcher hasn't opened since devices had names has no
	// column for them.
	device := "device"
	var n int
	if h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name = 'device'`).Scan(&n); n == 0 {
		device = "''"
	}
	rows, err := h.db.QueryContext(ctx, `SELECT id, input, output, action, result, in_size, out_size,
		started, finished, error, `+device+`
		FROM jobs WHERE id < ? AND (? = '' OR result = ?) ORDER BY id DESC LIMIT ?`, before, result, result, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var action string
		var started, finished int64
		if err := rows.Scan(&e.ID, &e.Input, &e.Output, &action, &e.Result, &e.InSize, &e.OutSize,
			&started, &finished, &e.Err, &e.Device); err != nil {
			return nil, err
		}
		e.Action = Action(action)
		if started > 0 {
			e.Started = time.Unix(started, 0)
		}
		if finished > 0 {
			e.Finished = time.Unix(finished, 0)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
