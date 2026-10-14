package encode

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// stateDB is where a Watcher keeps what it knows between runs: the settings
// the videos were checked against, the watched folders scanned before, what
// each video was found to need, and every job that finished, which the
// totals come from.
type stateDB struct{ db *sql.DB }

const stateSchema = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS folders (
	path TEXT PRIMARY KEY
);
CREATE TABLE IF NOT EXISTS files (
	path   TEXT PRIMARY KEY,
	size   INTEGER NOT NULL,
	mod    INTEGER NOT NULL, -- modification time, in Unix nanoseconds
	status TEXT NOT NULL,
	note   TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS jobs (
	id       INTEGER PRIMARY KEY,
	input    TEXT NOT NULL,
	output   TEXT NOT NULL,
	action   TEXT NOT NULL,
	result   TEXT NOT NULL, -- done, failed or canceled
	in_size  INTEGER NOT NULL,
	out_size INTEGER NOT NULL,
	started  INTEGER NOT NULL, -- Unix seconds
	finished INTEGER NOT NULL,
	error    TEXT NOT NULL DEFAULT ''
);
`

// openState opens the database at path, creating it if need be. A new one
// takes in what the JSON state file beside it held, from before there was a
// database.
func openState(path string) (*stateDB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one Watcher writes it; one connection keeps that simple
	if _, err := db.Exec(stateSchema); err != nil {
		db.Close()
		return nil, err
	}
	s := &stateDB{db}
	if err := s.importJSON(filepath.Join(filepath.Dir(path), "watch.json")); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *stateDB) Close() error { return s.db.Close() }

// savedState is what a Watcher reads back.
type savedState struct {
	settings string
	scanned  time.Time // when the state was last saved, at the end of a scan
	folders  map[string]bool
	files    map[string]fileState
}

func (s *stateDB) load() (savedState, error) {
	st := savedState{folders: map[string]bool{}, files: map[string]fileState{}}
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'settings'`).Scan(&st.settings)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, err
	}
	var scanned int64
	err = s.db.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'scanned'`).Scan(&scanned)
	switch {
	case err == nil:
		st.scanned = time.Unix(scanned, 0)
	case !errors.Is(err, sql.ErrNoRows):
		return st, err
	}
	rows, err := s.db.Query(`SELECT path FROM folders`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return st, err
		}
		st.folders[p] = true
	}
	rows.Close()
	rows, err = s.db.Query(`SELECT path, size, mod, status, note FROM files`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		var f fileState
		if err := rows.Scan(&p, &f.Size, &f.Mod, &f.Status, &f.Note); err != nil {
			return st, err
		}
		st.files[p] = f
	}
	return st, rows.Err()
}

// save writes the settings fingerprint and folders, and the given files:
// each path's state, or nil for one to forget. It notes the time too, which
// says which videos arrive after it.
func (s *stateDB) save(settings string, folders map[string]bool, files map[string]*fileState) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range map[string]any{"settings": settings, "scanned": time.Now().Unix()} {
		if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM folders`); err != nil {
		return err
	}
	for p := range folders {
		if _, err := tx.Exec(`INSERT INTO folders (path) VALUES (?)`, p); err != nil {
			return err
		}
	}
	for p, f := range files {
		if f == nil {
			_, err = tx.Exec(`DELETE FROM files WHERE path = ?`, p)
		} else {
			_, err = tx.Exec(`INSERT INTO files (path, size, mod, status, note) VALUES (?, ?, ?, ?, ?)
				ON CONFLICT (path) DO UPDATE SET size = excluded.size, mod = excluded.mod,
					status = excluded.status, note = excluded.note`,
				p, f.Size, f.Mod, f.Status, f.Note)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// addJob records a finished job.
func (s *stateDB) addJob(j Job, result string) error {
	errText := ""
	if j.Err != nil {
		errText = j.Err.Error()
	}
	_, err := s.db.Exec(`INSERT INTO jobs (input, output, action, result, in_size, out_size, started, finished, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.Input, j.Output, string(j.Action), result, j.InSize, j.OutSize,
		unixOrZero(j.Started), unixOrZero(j.Finished), errText)
	return err
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// totals sums up the jobs that rewrote a file: how much smaller their
// results came out than their sources, and how many there were.
func (s *stateDB) totals() (saved int64, results int, err error) {
	err = s.db.QueryRow(`SELECT COALESCE(SUM(in_size - out_size), 0), COUNT(*) FROM jobs
		WHERE result = 'done' AND action IN ('remux', 'encode') AND in_size > 0 AND out_size > 0`).
		Scan(&saved, &results)
	return saved, results, err
}

// importJSON takes in the JSON state file at path, if the database is new
// and there is one, then renames it so it isn't taken in again.
func (s *stateDB) importJSON(path string) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM meta`).Scan(&n); err != nil || n > 0 {
		return err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	st := savedState{folders: map[string]bool{}, files: map[string]fileState{}}
	var v2 struct {
		Version  int                  `json:"version"`
		Settings string               `json:"settings"`
		Folders  []string             `json:"folders"`
		Files    map[string]fileState `json:"files"`
	}
	if json.Unmarshal(data, &v2) == nil && v2.Version == 2 {
		st.settings = v2.Settings
		st.folders = setOf(v2.Folders)
		if v2.Files != nil {
			st.files = v2.Files
		}
	} else {
		// The first version only listed the videos seen in each folder, all
		// of which were left alone.
		var seen map[string][]string
		if json.Unmarshal(data, &seen) == nil {
			for dir, paths := range seen {
				st.folders[dir] = true
				for _, p := range paths {
					if f, ok := statFile(p); ok {
						f.Status = fileLeft
						st.files[p] = f
					}
				}
			}
		}
	}
	files := make(map[string]*fileState, len(st.files))
	for p, f := range st.files {
		files[p] = &f
	}
	if err := s.save(st.settings, st.folders, files); err != nil {
		return err
	}
	return os.Rename(path, path+".imported")
}
