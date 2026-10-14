package encode

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStateKeepsWhatWasSaved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cav1ar.db")
	db, err := openState(path)
	if err != nil {
		t.Fatal(err)
	}
	a := fileState{Size: 1, Mod: 2, Status: fileOK}
	b := fileState{Size: 3, Mod: 4, Status: fileTodo}
	err = db.save("fp", map[string]bool{"/tv": true}, map[string]*fileState{"/tv/a.mkv": &a, "/tv/b.mkv": &b})
	if err != nil {
		t.Fatal(err)
	}
	// Forget one, change the other.
	b.Status = fileFailed
	b.Note = "it broke"
	if err := db.save("fp2", map[string]bool{"/tv": true}, map[string]*fileState{"/tv/a.mkv": nil, "/tv/b.mkv": &b}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db, err = openState(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st, err := db.load()
	if err != nil {
		t.Fatal(err)
	}
	if st.settings != "fp2" || !st.folders["/tv"] || len(st.folders) != 1 {
		t.Errorf("settings %q, folders %v", st.settings, st.folders)
	}
	if _, ok := st.files["/tv/a.mkv"]; ok {
		t.Error("a forgotten file was kept")
	}
	if got := st.files["/tv/b.mkv"]; got != b {
		t.Errorf("got %+v, want %+v", got, b)
	}
}

func TestStateTakesInTheJSONStateFile(t *testing.T) {
	dir := t.TempDir()
	json := `{"version":2,"settings":"fp","folders":["/movies"],
		"files":{"/movies/a.mkv":{"size":5,"mod":6,"status":"ok"}}}`
	if err := os.WriteFile(filepath.Join(dir, "watch.json"), []byte(json), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := openState(filepath.Join(dir, "cav1ar.db"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := db.load()
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if st.settings != "fp" || !st.folders["/movies"] || st.files["/movies/a.mkv"] != (fileState{Size: 5, Mod: 6, Status: fileOK}) {
		t.Errorf("took in %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, "watch.json")); !errors.Is(err, os.ErrNotExist) {
		t.Error("watch.json is still there, so it would be taken in again")
	}
	if _, err := os.Stat(filepath.Join(dir, "watch.json.imported")); err != nil {
		t.Error("watch.json wasn't kept as watch.json.imported")
	}
}

func TestStateTotalsCountOnlyJobsThatRewroteAFile(t *testing.T) {
	db, err := openState(filepath.Join(t.TempDir(), "cav1ar.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	for _, j := range []struct {
		job    Job
		result string
	}{
		{Job{Input: "a", Action: ActionRemux, InSize: 100, OutSize: 90, Started: now, Finished: now}, "done"},
		{Job{Input: "b", Action: ActionEncode, InSize: 200, OutSize: 100, Started: now, Finished: now}, "done"},
		{Job{Input: "c", Action: ActionRename, InSize: 50, OutSize: 50}, "done"},
		{Job{Input: "d", Action: ActionRemux, InSize: 70, OutSize: 10, Err: errors.New("too short")}, "failed"},
	} {
		if err := db.addJob(j.job, j.result); err != nil {
			t.Fatal(err)
		}
	}
	saved, results, err := db.totals()
	if err != nil {
		t.Fatal(err)
	}
	if saved != 110 || results != 2 {
		t.Errorf("saved %d over %d results, want 110 over 2", saved, results)
	}
}
