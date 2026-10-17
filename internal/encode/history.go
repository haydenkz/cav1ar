package encode

import (
	"context"
	"database/sql"
	"strings"
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
	Device   string    `json:"device,omitempty"`   // empty for jobs recorded before devices had names
	Duration float64   `json:"duration,omitempty"` // of the video, in seconds; 0 if unknown
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

// column is name if the jobs table has that column, or else what stands in
// for it: a database the watcher hasn't opened since a column was added
// doesn't have it yet.
func (h *History) column(ctx context.Context, name, missing string) string {
	var n int
	h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name = ?`, name).Scan(&n)
	if n == 0 {
		return missing
	}
	return name
}

// Recent returns up to limit finished jobs, newest first, from before the
// one with ID before, or from the newest if before is 0. result, if not
// empty, keeps only jobs that ended that way, and search only those whose
// source or result has it in its path, ignoring case.
func (h *History) Recent(ctx context.Context, before int64, limit int, result, search string) ([]Entry, error) {
	if before <= 0 {
		before = 1<<63 - 1
	}
	like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(search) + "%"
	rows, err := h.db.QueryContext(ctx, `SELECT id, input, output, action, result, in_size, out_size,
		started, finished, error, `+h.column(ctx, "device", "''")+`, `+h.column(ctx, "duration", "0")+`
		FROM jobs WHERE id < ? AND (? = '' OR result = ?)
			AND (? = '' OR input LIKE ? ESCAPE '\' OR output LIKE ? ESCAPE '\')
		ORDER BY id DESC LIMIT ?`, before, result, result, search, like, like, limit)
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
			&started, &finished, &e.Err, &e.Device, &e.Duration); err != nil {
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

// Tally sums up a set of finished jobs.
type Tally struct {
	Done     int `json:"done"`
	Failed   int `json:"failed"`
	Canceled int `json:"canceled"`
	// The sizes of the jobs that rewrote a file, before and after.
	InSize  int64 `json:"in_size"`
	OutSize int64 `json:"out_size"`
	Busy    int64 `json:"busy"` // seconds the jobs that got done took
	// For the encodes that know how long their video is: how long that is in
	// all, and how long encoding it took, which give the speed.
	Video     float64 `json:"video"`
	VideoBusy int64   `json:"video_busy"`
}

// Stats sums up the jobs that finished in a stretch of time.
type Stats struct {
	Totals  Tally          `json:"totals"`
	Days    []DayTally     `json:"days"`    // oldest first, only days with jobs
	Devices []DeviceTally  `json:"devices"` // busiest first
	Actions map[Action]int `json:"actions"` // what the jobs that got done did
	Errors  []ErrorTally   `json:"errors"`  // the commonest failures first
}

type DayTally struct {
	Day string `json:"day"` // YYYY-MM-DD
	Tally
}

type DeviceTally struct {
	Device string `json:"device"` // empty for jobs recorded before devices had names
	Tally
}

type ErrorTally struct {
	Err   string    `json:"error"`
	Count int       `json:"count"`
	Last  time.Time `json:"last"`
}

// Stats sums up the jobs that finished since since, or every job if since
// is zero. Days are counted in the time zone offset seconds east of UTC.
func (h *History) Stats(ctx context.Context, since time.Time, offset int) (Stats, error) {
	from := int64(0)
	if !since.IsZero() {
		from = since.Unix()
	}
	device, duration := h.column(ctx, "device", "''"), h.column(ctx, "duration", "0")
	rewrote := `result = 'done' AND action IN ('remux', 'encode') AND in_size > 0 AND out_size > 0`
	timed := `result = 'done' AND started > 0 AND finished > started`
	timedEncode := timed + ` AND action = 'encode' AND ` + duration + ` > 0`
	sums := `COUNT(*) FILTER (WHERE result = 'done'), COUNT(*) FILTER (WHERE result = 'failed'),
		COUNT(*) FILTER (WHERE result = 'canceled'),
		COALESCE(SUM(in_size) FILTER (WHERE ` + rewrote + `), 0), COALESCE(SUM(out_size) FILTER (WHERE ` + rewrote + `), 0),
		COALESCE(SUM(finished - started) FILTER (WHERE ` + timed + `), 0),
		COALESCE(SUM(` + duration + `) FILTER (WHERE ` + timedEncode + `), 0),
		COALESCE(SUM(finished - started) FILTER (WHERE ` + timedEncode + `), 0)`
	// Jobs canceled before they started have no finish time, and count only
	// in the totals for all time.
	where := ` FROM jobs WHERE (finished >= ? OR ? = 0)`
	scan := func(row interface{ Scan(...any) error }, key *string, t *Tally) error {
		dest := []any{&t.Done, &t.Failed, &t.Canceled, &t.InSize, &t.OutSize, &t.Busy, &t.Video, &t.VideoBusy}
		if key != nil {
			dest = append([]any{key}, dest...)
		}
		return row.Scan(dest...)
	}

	st := Stats{Actions: map[Action]int{}}
	if err := scan(h.db.QueryRowContext(ctx, `SELECT `+sums+where, from, from), nil, &st.Totals); err != nil {
		return st, err
	}
	rows, err := h.db.QueryContext(ctx, `SELECT date(finished + ?, 'unixepoch') AS day, `+sums+where+
		` AND finished > 0 GROUP BY day ORDER BY day`, offset, from, from)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var d DayTally
		if err := scan(rows, &d.Day, &d.Tally); err != nil {
			rows.Close()
			return st, err
		}
		st.Days = append(st.Days, d)
	}
	rows.Close()
	rows, err = h.db.QueryContext(ctx, `SELECT `+device+` AS dev, `+sums+where+
		` GROUP BY dev ORDER BY COUNT(*) FILTER (WHERE result = 'done') DESC, dev`, from, from)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var d DeviceTally
		if err := scan(rows, &d.Device, &d.Tally); err != nil {
			rows.Close()
			return st, err
		}
		st.Devices = append(st.Devices, d)
	}
	rows.Close()
	rows, err = h.db.QueryContext(ctx, `SELECT action, COUNT(*)`+where+` AND result = 'done' GROUP BY action`, from, from)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var a string
		var n int
		if err := rows.Scan(&a, &n); err != nil {
			rows.Close()
			return st, err
		}
		st.Actions[Action(a)] = n
	}
	rows.Close()
	rows, err = h.db.QueryContext(ctx, `SELECT error, COUNT(*) AS n, MAX(finished)`+where+
		` AND result = 'failed' GROUP BY error ORDER BY n DESC, MAX(finished) DESC LIMIT 5`, from, from)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var e ErrorTally
		var last int64
		if err := rows.Scan(&e.Err, &e.Count, &last); err != nil {
			return st, err
		}
		if last > 0 {
			e.Last = time.Unix(last, 0)
		}
		st.Errors = append(st.Errors, e)
	}
	return st, rows.Err()
}
