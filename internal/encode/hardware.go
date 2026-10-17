package encode

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Encoder is a kind of AV1 encoder.
type Encoder string

const (
	EncoderCPU    Encoder = "cpu"    // SVT-AV1: the best quality for the size, but far slower than a GPU
	EncoderNVIDIA Encoder = "nvidia" // NVENC, on GeForce RTX 40-series cards and newer
	EncoderVAAPI  Encoder = "vaapi"  // AMD and Intel GPUs, through VAAPI
)

// Encoders lists every kind of encoder, in the order devices are filled.
var Encoders = []Encoder{EncoderNVIDIA, EncoderVAAPI, EncoderCPU}

func (e Encoder) String() string {
	switch e {
	case EncoderNVIDIA:
		return "NVIDIA GPU"
	case EncoderVAAPI:
		return "AMD/Intel GPU"
	}
	return "CPU"
}

// Device is something that encodes AV1 video: a GPU, or the CPU.
type Device struct {
	ID    string  `json:"id"`              // stays the same between runs, e.g. "nvidia:0000:01:00.0"; settings refer to it
	Kind  Encoder `json:"kind"`            // which encoder it runs
	Name  string  `json:"name"`            // for people, e.g. "GeForce RTX 4070"
	Path  string  `json:"path,omitempty"`  // the device node VAAPI opens
	Index int     `json:"index,omitempty"` // the NVIDIA GPU's number, in PCI bus order
	// Worker is the name of the worker the device is on, or empty for one
	// here; see Runner.
	Worker string `json:"worker,omitempty"`
}

// GPU reports whether d is a GPU rather than the CPU.
func (d Device) GPU() bool { return d.Kind != EncoderCPU }

// label is d's name as it reads after "on", as in "encoding on the CPU".
func (d Device) label() string {
	if d.ID == string(EncoderCPU) {
		return "the CPU"
	}
	return d.Name
}

func init() {
	// Number NVIDIA GPUs the way nvidia-smi does, so the number found for a
	// GPU picks the same one in every ffmpeg cav1ar starts.
	if os.Getenv("CUDA_DEVICE_ORDER") == "" {
		os.Setenv("CUDA_DEVICE_ORDER", "PCI_BUS_ID")
	}
}

// Devices lists what can encode AV1 here: NVIDIA GPUs, then other GPUs, then
// the CPU. The first call tests each with a tiny encode, which takes a
// moment.
func Devices() []Device { return detect() }

var detect = sync.OnceValue(func() []Device { return findDevices(trial) })

// findDevices lists the devices that pass try, a trial encode.
func findDevices(try func(opts ...string) error) []Device {
	var candidates []Device
	var opts [][]string
	for _, d := range nvidiaGPUs() {
		candidates = append(candidates, d)
		opts = append(opts, []string{"-c:v", "av1_nvenc", "-gpu", strconv.Itoa(d.Index)})
	}
	for _, d := range vaapiGPUs() {
		candidates = append(candidates, d)
		opts = append(opts, []string{"-vaapi_device", d.Path, "-vf", "format=p010,hwupload", "-c:v", "av1_vaapi"})
	}
	candidates = append(candidates, Device{ID: string(EncoderCPU), Kind: EncoderCPU, Name: "CPU"})
	opts = append(opts, []string{"-c:v", "libsvtav1"})

	works := make([]bool, len(candidates))
	var wg sync.WaitGroup
	for i := range candidates {
		wg.Go(func() { works[i] = try(opts[i]...) == nil })
	}
	wg.Wait()
	var found []Device
	for i, d := range candidates {
		if works[i] {
			found = append(found, d)
		}
	}
	return found
}

// nvidiaGPUs lists the NVIDIA GPUs, as nvidia-smi sees them. Without
// nvidia-smi, there may still be one that ffmpeg can use.
func nvidiaGPUs() []Device {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=index,pci.bus_id,name", "--format=csv,noheader").Output()
	if err != nil {
		return []Device{{ID: "nvidia:0", Kind: EncoderNVIDIA, Name: "NVIDIA GPU"}}
	}
	var gpus []Device
	for line := range strings.Lines(string(out)) {
		f := strings.SplitN(line, ",", 3)
		if len(f) < 3 {
			continue
		}
		index, err := strconv.Atoi(strings.TrimSpace(f[0]))
		if err != nil {
			continue
		}
		// nvidia-smi gives an 8-digit PCI domain, where the rest of Linux uses 4.
		bus := strings.ToLower(strings.TrimSpace(f[1]))
		if len(bus) > 12 {
			bus = bus[len(bus)-12:]
		}
		name := strings.TrimPrefix(strings.TrimSpace(f[2]), "NVIDIA ")
		gpus = append(gpus, Device{ID: "nvidia:" + bus, Kind: EncoderNVIDIA, Name: name, Index: index})
	}
	return dedupeNames(gpus)
}

// vaapiGPUs lists the GPUs other than NVIDIA's that VAAPI may reach, opened
// by their PCI address where the system offers that, since render node
// numbers can change between boots.
func vaapiGPUs() []Device {
	nodes, _ := filepath.Glob("/dev/dri/renderD*")
	var gpus []Device
	for _, node := range nodes {
		sys := filepath.Join("/sys/class/drm", filepath.Base(node), "device")
		vendor, device := readHex(filepath.Join(sys, "vendor")), readHex(filepath.Join(sys, "device"))
		if vendor == 0x10de { // NVIDIA's own encoder is used for its GPUs
			continue
		}
		where := filepath.Base(node)
		d := Device{ID: "vaapi:" + where, Kind: EncoderVAAPI, Path: node}
		if target, err := filepath.EvalSymlinks(sys); err == nil {
			where = filepath.Base(target) // its PCI address
			d.ID = "vaapi:" + where
			if byPath := "/dev/dri/by-path/pci-" + where + "-render"; exists(byPath) {
				d.Path = byPath
			}
		}
		d.Name = gpuName(vendor, device, where)
		gpus = append(gpus, d)
	}
	return dedupeNames(gpus)
}

// gpuName names a GPU by its PCI IDs, e.g. "Intel Arc A380", falling back on
// its maker and where it is.
func gpuName(vendor, device uint64, where string) string {
	maker := map[uint64]string{0x8086: "Intel", 0x1002: "AMD"}[vendor]
	name := pciName(vendor, device)
	// The marketing name is usually in brackets after the chip's: "DG2 [Arc A380]".
	if i, j := strings.Index(name, "["), strings.LastIndex(name, "]"); i >= 0 && j > i {
		name = name[i+1 : j]
	}
	switch {
	case name == "" && maker == "":
		return "GPU at " + where
	case name == "":
		return maker + " GPU at " + where
	case maker != "" && !strings.HasPrefix(name, maker):
		return maker + " " + name
	}
	return name
}

// pciName looks a device up in the PCI ID database, or returns "" if it isn't
// there or there is no database.
func pciName(vendor, device uint64) string {
	for _, path := range []string{"/usr/share/hwdata/pci.ids", "/usr/share/misc/pci.ids"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		defer f.Close()
		// Vendors start a line, and their devices follow, indented by a tab.
		vendorLine, deviceLine := fmt.Sprintf("%04x ", vendor), fmt.Sprintf("\t%04x ", device)
		inVendor := false
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "" || line[0] == '#':
			case line[0] != '\t':
				if inVendor {
					return ""
				}
				inVendor = strings.HasPrefix(line, vendorLine)
			case inVendor && strings.HasPrefix(line, deviceLine):
				return strings.TrimSpace(line[len(deviceLine):])
			}
		}
		return ""
	}
	return ""
}

func readHex(path string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(string(b)), "0x"), 16, 64)
	return n
}

// dedupeNames tells apart devices that would otherwise share a name, such as
// two of the same card, by adding where each is.
func dedupeNames(ds []Device) []Device {
	count := map[string]int{}
	for _, d := range ds {
		count[d.Name]++
	}
	for i, d := range ds {
		if count[d.Name] > 1 {
			_, where, _ := strings.Cut(d.ID, ":")
			ds[i].Name += " (" + where + ")"
		}
	}
	return ds
}

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
	return exec.CommandContext(ctx, FFmpeg, args...).Run()
}

// CheckEncoder says whether a device of kind e was found, and why not if
// none was. The first call tests every device, which takes a moment.
func CheckEncoder(e Encoder) error {
	for _, d := range Devices() {
		if d.Kind == e {
			return nil
		}
	}
	switch e {
	case EncoderCPU:
		return errors.New("this ffmpeg was built without SVT-AV1 (libsvtav1)")
	case EncoderNVIDIA:
		return errors.New("no NVIDIA GPU that can encode AV1 was found (needs an RTX 40-series or newer, and the NVIDIA driver)")
	case EncoderVAAPI:
		return errors.New("no AMD or Intel GPU that can encode AV1 was found through VAAPI")
	}
	return errors.New("unknown encoder " + strconv.Quote(string(e)))
}

// ErrNoEncoder is returned when nothing here can encode AV1.
var ErrNoEncoder = errors.New("nothing here can encode AV1: ffmpeg needs SVT-AV1 (libsvtav1), " +
	"or a GPU that can encode AV1 with its driver installed")

// The quality setting is on NVENC's scale, and the other encoders are set to
// match it: scored with VMAF on a 1080p test clip, at the recommended speed
// presets, they land within half a point of NVENC at both 30 and 34.

// svtCRF is SVT-AV1's CRF for quality q.
func svtCRF(q int) int { return min(max(q-3, 1), 63) }

// vaapiQP is the AV1 quantizer, 0–255, that VAAPI's constant quality mode
// uses for quality q.
func vaapiQP(q int) int { return min(max(int(math.Round(4.5*float64(q)))-67, 1), 255) }

// videoArgs are the ffmpeg options that encode v on d at width x height.
// Quality and speed are mapped onto each encoder's own scale. hwDecode
// decodes on the GPU as well, which a GPU may not manage for every source.
func videoArgs(s Settings, d Device, v streamInfo, width, height int, hwDecode bool) (beforeInput, afterInput []string) {
	scale := width != v.Width || height != v.Height
	var filters []string
	switch d.Kind {
	case EncoderNVIDIA:
		gpu := strconv.Itoa(d.Index)
		if hwDecode {
			beforeInput = []string{"-hwaccel", "cuda", "-hwaccel_output_format", "cuda", "-hwaccel_device", gpu}
			if v.Interlaced() {
				filters = append(filters, "yadif_cuda=deint=interlaced")
			}
			filters = append(filters, fmt.Sprintf("scale_cuda=%d:%d:format=p010le", width, height))
		} else {
			filters = append(cpuFilters(v, width, height, scale), "format=p010le")
		}
		// The rate cap only bites on pathological scenes, such as film grain
		// or confetti.
		maxRate := nvencMaxRate(resolution(width, height))
		afterInput = []string{"-c:v", "av1_nvenc", "-gpu", gpu, "-preset", "p" + strconv.Itoa(s.NVENCPreset), "-tune", "hq",
			"-rc", "vbr", "-cq", strconv.Itoa(s.CRF), "-b:v", "0",
			"-maxrate", strconv.Itoa(maxRate) + "M", "-bufsize", strconv.Itoa(2*maxRate) + "M",
			"-rc-lookahead", "32", "-spatial-aq", "1", "-temporal-aq", "1", "-g", "240"}
	case EncoderVAAPI:
		// 10-bit throughout, as on NVIDIA, which keeps gradients from banding.
		if hwDecode {
			beforeInput = []string{"-init_hw_device", "vaapi=gpu:" + d.Path, "-filter_hw_device", "gpu",
				"-hwaccel", "vaapi", "-hwaccel_device", "gpu", "-hwaccel_output_format", "vaapi"}
			if v.Interlaced() {
				filters = append(filters, "deinterlace_vaapi")
			}
			filters = append(filters, fmt.Sprintf("scale_vaapi=w=%d:h=%d:format=p010", width, height))
		} else {
			beforeInput = []string{"-vaapi_device", d.Path}
			filters = append(cpuFilters(v, width, height, scale), "format=p010", "hwupload")
		}
		// global_quality is an option every encoder has, so it must name the
		// video's, or the audio encoder takes it too and Opus refuses it.
		afterInput = []string{"-c:v", "av1_vaapi", "-rc_mode", "CQP", "-global_quality:v", strconv.Itoa(vaapiQP(s.CRF)), "-g", "240"}
	default:
		filters = cpuFilters(v, width, height, scale)
		afterInput = []string{"-c:v", "libsvtav1", "-preset", strconv.Itoa(s.Preset), "-crf", strconv.Itoa(svtCRF(s.CRF))}
	}
	if len(filters) > 0 {
		afterInput = append([]string{"-vf", strings.Join(filters, ",")}, afterInput...)
	}
	// Carry the colour description over, so HDR and old SD video still
	// play with the right colours.
	for _, c := range []struct{ opt, val string }{
		{"-color_primaries", v.ColorPrimaries}, {"-color_trc", v.ColorTrc}, {"-colorspace", v.ColorSpace},
	} {
		if c.val != "" && c.val != "unknown" && c.val != "reserved" {
			afterInput = append(afterInput, c.opt, c.val)
		}
	}
	return beforeInput, afterInput
}

// cpuFilters deinterlace and scale on the CPU.
func cpuFilters(v streamInfo, width, height int, scale bool) []string {
	var filters []string
	if v.Interlaced() {
		filters = append(filters, "bwdif=deint=interlaced")
	}
	if scale {
		filters = append(filters, fmt.Sprintf("scale=%d:%d:flags=lanczos", width, height))
	}
	return filters
}

// nvencMaxRate is the bitrate cap for NVIDIA encodes, in Mbit/s.
func nvencMaxRate(res string) int {
	switch res {
	case "2160p":
		return 30
	case "1440p":
		return 20
	case "1080p":
		return 12
	case "720p":
		return 8
	}
	return 4
}
