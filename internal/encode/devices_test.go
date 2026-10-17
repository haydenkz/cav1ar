package encode

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var (
	testGPU = Device{ID: "nvidia:0000:01:00.0", Kind: EncoderNVIDIA, Name: "Test GPU"}
	testArc = Device{ID: "vaapi:0000:0c:00.0", Kind: EncoderVAAPI, Name: "Test Arc", Path: "/dev/dri/renderD128"}
)

func TestSlotsDefaultsAndOverrides(t *testing.T) {
	devices := []Device{testGPU, testArc, cpuDevice}
	for _, c := range []struct {
		encoders      map[string]int
		gpu, arc, cpu int
	}{
		{nil, 2, 2, 0},                                        // GPUs busy, the CPU left out
		{map[string]int{"nvidia": 1}, 1, 2, 0},                // by kind
		{map[string]int{testArc.ID: 0}, 2, 0, 0},              // by ID
		{map[string]int{"nvidia": 3, testGPU.ID: 1}, 1, 2, 0}, // an ID beats its kind
		{map[string]int{"nvidia": 0, "vaapi": 0}, 0, 0, 1},    // no GPU encoding, so the CPU does
	} {
		s := DefaultSettings()
		s.Encoders = c.encoders
		gpu, arc, cpu := s.Slots(testGPU, devices), s.Slots(testArc, devices), s.Slots(cpuDevice, devices)
		if cpu > 1 { // depends on the test machine's cores
			cpu = 1
		}
		if gpu != c.gpu || arc != c.arc || cpu != c.cpu {
			t.Errorf("%v: got %d %d %d, want %d %d %d", c.encoders, gpu, arc, cpu, c.gpu, c.arc, c.cpu)
		}
	}
}

// The quality number maps onto each encoder's scale so that, at the
// numbers calibrated, they give about the same quality, and better quality
// always means a lower number on every scale.
func TestQualityMapsOntoEachEncoder(t *testing.T) {
	if svtCRF(30) != 23 || vaapiQP(30) != 54 || vaapiQP(34) != 72 {
		t.Errorf("30 maps to CRF %d and QP %d, and 34 to QP %d; want 23, 54 and 72", svtCRF(30), vaapiQP(30), vaapiQP(34))
	}
	for q := 2; q <= 63; q++ {
		if svtCRF(q) < svtCRF(q-1) || vaapiQP(q) < vaapiQP(q-1) {
			t.Fatalf("raising the number from %d to %d lowered an encoder's", q-1, q)
		}
	}
	if svtCRF(1) < 1 || vaapiQP(1) < 1 || svtCRF(63) > 63 || vaapiQP(63) > 255 {
		t.Error("a mapped number is out of its encoder's range")
	}
}

func TestVAAPIDecodesOnItsGPU(t *testing.T) {
	v := streamInfo{Width: 1920, Height: 1080}
	before, after := videoArgs(DefaultSettings(), testArc, v, 1920, 1080, true)
	if !hasArgs(before, "-init_hw_device", "vaapi=gpu:/dev/dri/renderD128") || !hasArgs(before, "-hwaccel", "vaapi") {
		t.Errorf("decoding on the GPU should open it for the decoder too: %q", before)
	}
	if !hasArgs(after, "-rc_mode", "VBR") || !hasArgs(after, "-qmin:v", "54") || !hasArgs(after, "-vf", "scale_vaapi=w=1920:h=1080:format=p010") {
		t.Errorf("encoding options %q", after)
	}
	if slices.Contains(after, "-compression_level:v") {
		t.Errorf("the driver's own speed was overridden: %q", after)
	}
	s := DefaultSettings()
	s.VAAPIPreset = 2
	if _, after := videoArgs(s, testArc, v, 1920, 1080, true); !hasArgs(after, "-compression_level:v", "2") {
		t.Errorf("speed 2 wasn't passed on: %q", after)
	}
	before, after = videoArgs(DefaultSettings(), testArc, v, 1280, 720, false)
	if !hasArgs(before, "-vaapi_device", "/dev/dri/renderD128") || !hasArgs(after, "-vf", "scale=1280:720:flags=lanczos,format=p010,hwupload") {
		t.Errorf("decoding on the CPU should scale there and upload: %q %q", before, after)
	}
}

func TestNVIDIAEncodesOnItsGPU(t *testing.T) {
	second := testGPU
	second.Index = 1
	before, after := videoArgs(DefaultSettings(), second, streamInfo{Width: 1920, Height: 1080}, 1920, 1080, true)
	if !hasArgs(before, "-hwaccel_device", "1") || !hasArgs(after, "-gpu", "1") {
		t.Errorf("the second GPU should both decode and encode: %q %q", before, after)
	}
}

func hasArgs(args []string, opt, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == opt && args[i+1] == val {
			return true
		}
	}
	return false
}

// Jobs spread over the devices, each going to the least busy for its number
// at once, so two devices with two each take one job apiece before either
// takes a second, and the job says which it runs on.
func TestJobsSpreadAcrossDevices(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := DefaultSettings()
	s.Encoders = map[string]int{"nvidia": 2, "vaapi": 2}
	q := newQueue(ctx, s, []Device{testGPU, testArc})
	defer func() { cancel(); q.Close() }()

	q.SetPaused(true)
	for range 3 {
		q.Add("/nonexistent/a.mkv")
	}
	q.SetPaused(false)
	started := map[int]string{}
	deadline := time.After(5 * time.Second)
	for len(started) < 2 {
		evs := make(chan []Event, 1)
		go func() { evs <- q.Next() }()
		select {
		case batch := <-evs:
			for _, ev := range batch {
				if ev.Job.State == Running {
					started[ev.Job.ID] = ev.Job.Device
				}
			}
		case <-deadline:
			t.Fatalf("only %d jobs started", len(started))
		}
	}
	if started[0] != "Test GPU" || started[1] != "Test Arc" {
		t.Errorf("jobs ran on %v; want the first on the GPU and the second on the Arc", started)
	}
}

// goneRunner is a worker that leaves as its first pass starts, taking its
// devices with it as the worker server does.
type goneRunner struct{ q *Queue }

func (g goneRunner) Run(context.Context, []string, Output) error {
	g.q.SetWorkerDevices("w", nil)
	return ErrWorkerGone
}

// A job whose worker goes away goes back in the queue, rather than failing.
func TestJobsGoBackWhenTheirWorkerGoes(t *testing.T) {
	src := filepath.Join(t.TempDir(), "in.mkv")
	if err := exec.Command(FFmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc=d=1:s=320x240",
		"-c:v", "mpeg4", src).Run(); err != nil {
		t.Skip("ffmpeg can't make a test clip:", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := DefaultSettings()
	s.WorkerDir = t.TempDir()
	q := newQueue(ctx, s, []Device{{ID: "w/vaapi:x", Kind: EncoderVAAPI, Name: "GPU on w", Worker: "w"}})
	defer func() { cancel(); q.Close() }()
	AddWorker("w", goneRunner{q})
	defer RemoveWorker("w")

	q.Add(src)
	deadline := time.After(30 * time.Second)
	ran := false
	for {
		evs := make(chan []Event, 1)
		go func() { evs <- q.Next() }()
		select {
		case batch := <-evs:
			for _, ev := range batch {
				switch {
				case ev.Job.State == Running:
					ran = true
				case ev.Job.State == Queued && ran:
					if !strings.Contains(ev.Line, "worker went away") {
						t.Errorf("it went back without saying why: %q", ev.Line)
					}
					return
				case ev.Job.State.Finished():
					t.Fatalf("the job ended %s: %v", ev.Job.State, ev.Job.Err)
				}
			}
		case <-deadline:
			t.Fatal("the job never went back in the queue")
		}
	}
}

// The history reads databases from before jobs recorded their device, and
// after, newest first.
func TestHistoryReadsOldAndNewDatabases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cav1ar.db")
	db, err := openState(path)
	if err != nil {
		t.Fatal(err)
	}
	db.addJob(Job{Input: "/a.mkv", Output: "/a.av1.mkv", Action: ActionEncode, Device: "GPU", Started: time.Unix(100, 0), Finished: time.Unix(200, 0)}, "done")
	db.addJob(Job{Input: "/b.mkv", Action: ActionEncode, Err: errors.New("broke")}, "failed")
	db.Close()

	h, err := OpenHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	all, err := h.Recent(context.Background(), 0, 10, "", "")
	if err != nil || len(all) != 2 || all[0].Input != "/b.mkv" || all[1].Device != "GPU" || all[1].Finished.Unix() != 200 {
		t.Fatalf("got %+v, %v", all, err)
	}
	failed, _ := h.Recent(context.Background(), 0, 10, "failed", "")
	if len(failed) != 1 || failed[0].Err != "broke" {
		t.Errorf("failed only: %+v", failed)
	}
	older, _ := h.Recent(context.Background(), all[0].ID, 10, "", "")
	if len(older) != 1 || older[0].Input != "/a.mkv" {
		t.Errorf("before the newest: %+v", older)
	}
}

// VAAPI aims at a bitrate that follows NVENC's for the quality, with a floor
// on the quantizer that ffmpeg caps at 69.
func TestVAAPIRateFollowsTheQuality(t *testing.T) {
	for _, c := range []struct {
		q                       int
		res                     string
		target, max, buf, floor string
	}{
		{30, "1080p", "5000k", "12000k", "24000k", "54"},
		{34, "1080p", "3150k", "12000k", "24000k", "69"},
		{26, "1080p", "7937k", "15874k", "31748k", "36"},
		{30, "2160p", "12500k", "30000k", "60000k", "54"},
	} {
		got := vaapiRate(c.q, c.res)
		if !hasArgs(got, "-b:v", c.target) || !hasArgs(got, "-maxrate:v", c.max) || !hasArgs(got, "-bufsize:v", c.buf) || !hasArgs(got, "-qmin:v", c.floor) {
			t.Errorf("quality %d at %s: %q", c.q, c.res, got)
		}
	}
}
