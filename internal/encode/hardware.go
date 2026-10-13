package encode

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Encoder picks what encodes the video.
type Encoder string

const (
	EncoderCPU    Encoder = "cpu"    // SVT-AV1: the best quality for the size
	EncoderNVIDIA Encoder = "nvidia" // NVENC, on GeForce RTX 40-series cards and newer
	EncoderVAAPI  Encoder = "vaapi"  // AMD and Intel GPUs, through VAAPI
)

// Encoders lists every encoder, best quality first.
var Encoders = []Encoder{EncoderCPU, EncoderNVIDIA, EncoderVAAPI}

func (e Encoder) String() string {
	switch e {
	case EncoderNVIDIA:
		return "NVIDIA GPU"
	case EncoderVAAPI:
		return "AMD/Intel GPU"
	}
	return "CPU"
}

// hardware is what DetectEncoders found: whether each encoder works, and the
// GPU device VAAPI should use.
type hardware struct {
	errs        map[Encoder]error
	vaapiDevice string
}

// detect runs once, the first time anything asks.
var detect = sync.OnceValue(func() hardware {
	hw := hardware{errs: map[Encoder]error{}}
	var wg sync.WaitGroup
	var mu sync.Mutex
	set := func(e Encoder, err error) { mu.Lock(); hw.errs[e] = err; mu.Unlock() }

	wg.Go(func() {
		if trial("-c:v", "libsvtav1") != nil {
			set(EncoderCPU, errors.New("this ffmpeg was built without SVT-AV1 (libsvtav1)"))
		} else {
			set(EncoderCPU, nil)
		}
	})
	wg.Go(func() {
		if trial("-c:v", "av1_nvenc") != nil {
			set(EncoderNVIDIA, errors.New("no NVIDIA GPU that can encode AV1 was found (needs an RTX 40-series or newer, and the NVIDIA driver)"))
		} else {
			set(EncoderNVIDIA, nil)
		}
	})
	wg.Go(func() {
		devices, _ := filepath.Glob("/dev/dri/renderD*")
		for _, dev := range devices {
			if trial("-vaapi_device", dev, "-vf", "format=nv12,hwupload", "-c:v", "av1_vaapi") == nil {
				mu.Lock()
				hw.vaapiDevice = dev
				mu.Unlock()
				set(EncoderVAAPI, nil)
				return
			}
		}
		set(EncoderVAAPI, errors.New("no AMD or Intel GPU that can encode AV1 was found through VAAPI"))
	})
	wg.Wait()
	return hw
})

// trial encodes one tiny frame with the given options, to see if they work.
func trial(opts ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=black:s=256x256:d=0.1"}
	// Device options go before the input; everything else after it.
	if len(opts) > 1 && opts[0] == "-vaapi_device" {
		args = append([]string{opts[0], opts[1]}, args...)
		opts = opts[2:]
	}
	args = append(args, opts...)
	args = append(args, "-frames:v", "1", "-f", "null", "-")
	return exec.CommandContext(ctx, "ffmpeg", args...).Run()
}

// CheckEncoder says whether e works on this machine, and why not if it
// doesn't. The first call tests every encoder, which takes a moment.
func CheckEncoder(e Encoder) error {
	if err, ok := detect().errs[e]; ok {
		return err
	}
	return errors.New("unknown encoder " + strconv.Quote(string(e)))
}

// videoArgs are the ffmpeg options that encode the video with s.Encoder.
// Quality and speed are mapped onto each encoder's own scale.
func videoArgs(s Settings) (beforeInput, afterInput []string) {
	switch s.Encoder {
	case EncoderNVIDIA:
		// NVENC's quality scale matches CRF; its presets run p1 (fastest) to p7.
		preset := 7 - (s.Preset*6+6)/13
		return nil, []string{"-c:v", "av1_nvenc", "-preset", "p" + strconv.Itoa(preset), "-tune", "hq",
			"-rc", "vbr", "-cq", strconv.Itoa(s.CRF), "-b:v", "0"}
	case EncoderVAAPI:
		// Constant quality on AV1's 0–255 quantizer scale, about 4× CRF.
		// Uploading as nv12 or p010 keeps 10-bit sources 10-bit.
		return []string{"-vaapi_device", detect().vaapiDevice},
			[]string{"-vf", "format=nv12|p010,hwupload", "-c:v", "av1_vaapi",
				"-rc_mode", "CQP", "-global_quality", strconv.Itoa(s.CRF * 4)}
	}
	return nil, []string{"-c:v", "libsvtav1", "-preset", strconv.Itoa(s.Preset), "-crf", strconv.Itoa(s.CRF)}
}
