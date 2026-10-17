package encode

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Stats add up the jobs that finished in the time asked about, by day, by
// device and by what went wrong.
func TestStatsSumUpTheHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cav1ar.db")
	db, err := openState(path)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	at := func(d, secs int) time.Time { return day.AddDate(0, 0, d).Add(time.Duration(secs) * time.Second) }
	for _, j := range []struct {
		job    Job
		result string
	}{
		{Job{Input: "/old.mkv", Action: ActionEncode, InSize: 50, OutSize: 10, Started: at(-40, 0), Finished: at(-40, 60)}, "done"},
		{Job{Input: "/a.mkv", Action: ActionEncode, Device: "GPU", InSize: 1000, OutSize: 400, Duration: 600 * time.Second,
			Started: at(-1, 0), Finished: at(-1, 60)}, "done"},
		{Job{Input: "/b.mkv", Action: ActionEncode, Device: "GPU", InSize: 500, OutSize: 300, Duration: 300 * time.Second,
			Started: at(0, 0), Finished: at(0, 60)}, "done"},
		{Job{Input: "/c.mkv", Action: ActionRetag, Device: "GPU", InSize: 500, OutSize: 500, Started: at(0, 0), Finished: at(0, 5)}, "done"},
		{Job{Input: "/d.mkv", Action: ActionEncode, Device: "Arc", Err: errors.New("broke"), Started: at(0, 0), Finished: at(0, 9)}, "failed"},
		{Job{Input: "/e.mkv", Action: ActionEncode, Device: "Arc", Err: errors.New("broke"), Started: at(0, 0), Finished: at(0, 10)}, "failed"},
		{Job{Input: "/f.mkv"}, "canceled"}, // before it started
	} {
		if err := db.addJob(j.job, j.result); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	h, err := OpenHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	st, err := h.Stats(context.Background(), at(-7, 0), 0)
	if err != nil {
		t.Fatal(err)
	}
	want := Tally{Done: 3, Failed: 2, InSize: 1500, OutSize: 700, Busy: 125, Video: 900, VideoBusy: 120}
	if st.Totals != want {
		t.Errorf("totals: got %+v, want %+v", st.Totals, want)
	}
	if len(st.Days) != 2 || st.Days[0].Day != "2026-03-09" || st.Days[1].Day != "2026-03-10" || st.Days[1].Done != 2 || st.Days[1].Failed != 2 {
		t.Errorf("days: %+v", st.Days)
	}
	if len(st.Devices) != 2 || st.Devices[0].Device != "GPU" || st.Devices[0].Done != 3 || st.Devices[1].Failed != 2 {
		t.Errorf("devices: %+v", st.Devices)
	}
	if st.Actions[ActionEncode] != 2 || st.Actions[ActionRetag] != 1 {
		t.Errorf("actions: %v", st.Actions)
	}
	if len(st.Errors) != 1 || st.Errors[0].Err != "broke" || st.Errors[0].Count != 2 || !st.Errors[0].Last.Equal(at(0, 10)) {
		t.Errorf("errors: %+v", st.Errors)
	}

	// Days follow the time zone asked for: 12:00 UTC is the next day 14 hours east.
	st, _ = h.Stats(context.Background(), at(-7, 0), 14*3600)
	if st.Days[0].Day != "2026-03-10" {
		t.Errorf("days 14 hours east: %+v", st.Days)
	}
	// All time takes in the old job and the one canceled before it started.
	st, _ = h.Stats(context.Background(), time.Time{}, 0)
	if st.Totals.Done != 4 || st.Totals.Canceled != 1 {
		t.Errorf("all time: %+v", st.Totals)
	}

	if got, _ := h.Recent(context.Background(), 0, 10, "", "B.MKV"); len(got) != 1 || got[0].Input != "/b.mkv" {
		t.Errorf("searching for b: %+v", got)
	}
	if got, _ := h.Recent(context.Background(), 0, 10, "", "%"); len(got) != 0 {
		t.Errorf("a %% matched literally: %+v", got)
	}
}
