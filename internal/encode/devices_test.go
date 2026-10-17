package encode

import (
	"context"
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
	if svtCRF(30) != 27 || vaapiQP(30) != 68 || vaapiQP(34) != 86 {
		t.Errorf("30 maps to CRF %d and QP %d, and 34 to QP %d; want 27, 68 and 86", svtCRF(30), vaapiQP(30), vaapiQP(34))
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
	if !hasArgs(after, "-global_quality", "68") || !hasArgs(after, "-vf", "scale_vaapi=w=1920:h=1080:format=p010") {
		t.Errorf("encoding options %q", after)
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

// Each device runs as many jobs as it has room for, so two devices with
// one each run two jobs at once, and the job says which it runs on.
func TestJobsSpreadAcrossDevices(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := DefaultSettings()
	s.Encoders = map[string]int{"nvidia": 1, "vaapi": 1}
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
