package worker

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// A pass is an ffmpeg command line. To map its paths onto this worker's and
// keep it to the folders allowed, the worker works out which arguments are
// files: what the pass reads, after -i; what it writes, which are the
// arguments that aren't options or their values; and the values of options
// that name a file.
//
// The token, which the daemon has to prove it has, is what keeps strangers
// from running passes here at all. -allow is a second line, which keeps the
// daemon's passes to the folders given: every file they read or write must
// be in one of them, symlinks followed, inputs can only be local files, and
// options and filters known to name other files must keep to those folders
// too or aren't allowed. ffmpeg can reach files in more ways than these
// checks know of, so -allow narrows what a daemon can do, rather than
// making it safe to work for one that isn't trusted.

// argKind is what an argument of a pass is.
type argKind int

const (
	argOther  argKind = iota
	argInput          // a file the pass reads, after -i
	argGraph          // an input that is a filter graph (-f lavfi)
	argList           // an input that names other files (-f concat)
	argOutput         // a file the pass writes
	argFile           // a file an option names
	argFilter         // a filter graph an option gives
)

// flags are ffmpeg options that take no value, as far as passes use them.
// An option missing from here is taken to have one, which reads what
// follows wrongly, and gets the pass refused rather than let through.
var flags = map[string]bool{
	"y": true, "n": true, "hide_banner": true, "stdin": true, "nostdin": true, "stats": true, "nostats": true,
	"an": true, "vn": true, "sn": true, "dn": true, "shortest": true, "copyts": true, "start_at_zero": true,
	"re": true, "accurate_seek": true, "noaccurate_seek": true, "seek_timestamp": true, "autorotate": true,
	"noautorotate": true, "autoscale": true, "noautoscale": true, "ignore_unknown": true, "copy_unknown": true,
	"fix_sub_duration": true, "xerror": true, "bitexact": true,
}

// fileOptions name a file the pass reads or writes.
var fileOptions = map[string]bool{
	"progress": true, "passlogfile": true, "vstats_file": true, "sdp_file": true, "attach": true,
	"dump_attachment": true, "filter_script": true, "filter_complex_script": true, "stats_enc_pre": true,
	"stats_enc_post": true, "stats_mux_pre": true, "fpre": true,
}

// filterOptions give a filter graph.
var filterOptions = map[string]bool{"vf": true, "af": true, "filter": true, "filter_complex": true, "lavfi": true}

// fileFilters are filters that open files they're given by name, or load
// code. Graphs that use them aren't allowed.
var fileFilters = map[string]bool{
	"movie": true, "amovie": true, "sendcmd": true, "asendcmd": true, "zmq": true, "azmq": true,
	"ass": true, "subtitles": true, "drawtext": true, "curves": true, "selectivecolor": true,
	"lut1d": true, "lut3d": true, "nnedi": true, "arnndn": true, "sofalizer": true, "libvmaf": true,
	"psnr": true, "ssim": true, "xpsnr": true, "vmafmotion": true, "vidstabdetect": true, "vidstabtransform": true,
	"signature": true, "metadata": true, "ametadata": true, "deshake": true, "firequalizer": true,
	"removelogo": true, "cover_rect": true, "find_rect": true, "ocr": true, "frei0r": true, "frei0r_src": true,
	"ladspa": true, "lv2": true, "dnn_processing": true, "dnn_detect": true, "dnn_classify": true,
	"derain": true, "sr": true, "openclsrc": true, "program_opencl": true, "libplacebo": true,
	"lensfun": true, "fsync": true, "qrencode": true, "qrencodesrc": true, "showcqt": true,
	"coreimage": true, "coreimagesrc": true,
}

// kinds says what each of a pass's arguments is.
func kinds(args []string) []argKind {
	k := make([]argKind, len(args))
	format := "" // the -f for the next file
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-" || !strings.HasPrefix(a, "-") {
			k[i] = argOutput
			format = ""
			continue
		}
		name, _, _ := strings.Cut(a[1:], ":") // without a stream specifier, as in -filter:v
		if flags[name] || i+1 == len(args) {
			continue
		}
		i++
		switch {
		case name == "i":
			switch format {
			case "lavfi":
				k[i] = argGraph
			case "concat":
				k[i] = argList
			default:
				k[i] = argInput
			}
			format = ""
		case name == "f":
			format = args[i]
		case fileOptions[name] || strings.HasPrefix(name, "/"): // -/opt reads opt's value from a file
			k[i] = argFile
		case filterOptions[name]:
			k[i] = argFilter
		}
	}
	return k
}

// passArgs turns a pass's arguments as the daemon sent them into the ones
// to run here: its files moved to where this worker sees them, then, if the
// worker allows only some folders, checked against them, with each input
// limited to local files.
func (l *link) passArgs(args []string) ([]string, error) {
	k := kinds(args)
	args = slices.Clone(args)
	for i, a := range args {
		switch k[i] {
		case argInput, argList, argOutput, argFile:
			args[i] = mapPath(l.paths, a)
		}
	}
	if len(l.allow) == 0 {
		return args, nil
	}
	var checked []string
	for i, a := range args {
		if err := l.allowed(k[i], a); err != nil {
			return nil, err
		}
		if k[i] == argInput { // no URLs, nor anything an input could name in turn
			checked = append(checked[:len(checked)-1], "-protocol_whitelist", "file", "-i")
		}
		checked = append(checked, a)
	}
	return checked, nil
}

// allowed checks an argument of kind k is something a pass may use: a file
// in a folder this worker allows, a GPU, or neither of those.
func (l *link) allowed(k argKind, a string) error {
	refuse := func(why string) error { return fmt.Errorf("this worker doesn't let passes %s; see its -allow", why) }
	switch k {
	case argList:
		return refuse(fmt.Sprintf("read lists of files, such as %s", a))
	case argGraph, argFilter:
		for _, name := range filterNames(a) {
			if fileFilters[name] {
				return refuse(fmt.Sprintf("use the %s filter, which opens files", name))
			}
		}
		return nil
	case argFile:
		if a == "pipe:1" || a == "pipe:2" { // back to the daemon
			return nil
		}
	case argOther:
		if !strings.HasPrefix(a, "/") || strings.HasPrefix(filepath.Clean(a), "/dev/dri/") {
			return nil
		}
	}
	switch {
	case !filepath.IsAbs(a):
		return refuse(fmt.Sprintf("use %s, which isn't a full path", a))
	case slices.Contains(strings.Split(a, "/"), ".."):
		return refuse(fmt.Sprintf("use %s, which climbs out of a folder with ..", a))
	}
	real := resolve(a)
	for _, dir := range l.allow {
		if rel, err := filepath.Rel(dir, real); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return nil
		}
	}
	if real != filepath.Clean(a) {
		return refuse(fmt.Sprintf("use %s, which is %s", a, real))
	}
	return refuse("use " + a)
}

// filterNames lists the filters a filter graph uses.
func filterNames(graph string) []string {
	var names []string
	for f := range strings.FieldsFuncSeq(graph, func(r rune) bool { return r == ',' || r == ';' }) {
		f = strings.TrimSpace(f)
		for strings.HasPrefix(f, "[") { // the pads it reads
			_, f, _ = strings.Cut(f, "]")
			f = strings.TrimSpace(f)
		}
		name, _, _ := strings.Cut(f, "=")
		name, _, _ = strings.Cut(name, "@") // an instance's name
		name, _, _ = strings.Cut(name, "[") // the pads it writes
		names = append(names, strings.TrimSpace(name))
	}
	return names
}

// resolve is where p really is, with the symlinks in as much of it as
// exists followed, so a link in an allowed folder can't lead out of it.
func resolve(p string) string {
	rest := ""
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// resolveAll resolves each of dirs, the folders a worker allows.
func resolveAll(dirs []string) []string {
	var all []string
	for _, d := range dirs {
		all = append(all, resolve(filepath.Clean(d)))
	}
	return all
}
