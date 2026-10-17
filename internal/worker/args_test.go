package worker

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The passes a daemon sends a worker, which -allow mustn't get in the way
// of: an encoding pass for each kind of GPU, and a preroll's.
func TestThePassesADaemonSendsAreAllowed(t *testing.T) {
	media, shared := t.TempDir(), t.TempDir()
	l := &link{allow: resolveAll([]string{media, shared})}
	src := filepath.Join(media, "Some Film (2001)", "Some Film (2001).mkv")
	av, side := filepath.Join(shared, ".a.cav1ar-tmp.mkv"), filepath.Join(shared, ".b.cav1ar-tmp.mkv")
	clip, result := filepath.Join(shared, ".c.cav1ar-tmp.mkv"), filepath.Join(shared, ".d.cav1ar-tmp.mkv")
	start := []string{"-hide_banner", "-nostdin", "-nostats", "-y", "-progress", "pipe:1", "-analyzeduration", "100M", "-probesize", "100M"}
	for name, args := range map[string][]string{
		"VAAPI": append(slices.Clone(start),
			"-init_hw_device", "vaapi=gpu:/dev/dri/renderD128", "-filter_hw_device", "gpu", "-hwaccel", "vaapi",
			"-hwaccel_device", "gpu", "-hwaccel_output_format", "vaapi", "-ss", "5.005000", "-i", src,
			"-map", "0:0", "-map", "0:1", "-vf", "deinterlace_vaapi,scale_vaapi=w=1920:h=1080:format=p010",
			"-c:v", "av1_vaapi", "-rc_mode", "VBR", "-b:v", "5000k", "-maxrate:v", "12000k", "-bufsize:v", "24000k",
			"-qmin:v", "54", "-g", "240", "-color_primaries", "bt709", "-c:a:0", "libopus", "-b:a:0", "128k",
			"-filter:a:0", "aformat=channel_layouts=7.1|5.1|stereo|mono", "-max_muxing_queue_size", "4096", "-f", "matroska", av,
			"-map", "0:2", "-c:s:0", "copy", "-c:t", "copy", "-map_chapters", "-1", "-max_muxing_queue_size", "4096", "-f", "matroska", side),
		"NVENC": append(slices.Clone(start),
			"-hwaccel", "cuda", "-hwaccel_output_format", "cuda", "-hwaccel_device", "0", "-i", src, "-map", "0:0",
			"-vf", "yadif_cuda=deint=interlaced,scale_cuda=1280:720:format=p010le", "-c:v", "av1_nvenc", "-gpu", "0",
			"-preset", "p5", "-tune", "hq", "-rc", "vbr", "-cq", "30", "-b:v", "0", "-maxrate", "8M", "-bufsize", "16M",
			"-rc-lookahead", "32", "-spatial-aq", "1", "-temporal-aq", "1", "-g", "240", "-max_muxing_queue_size", "4096",
			"-f", "matroska", av),
		"a CPU decode": append(slices.Clone(start),
			"-vaapi_device", "/dev/dri/renderD128", "-i", src, "-map", "0:0",
			"-vf", "bwdif=deint=interlaced,scale=1920:800:flags=lanczos,format=p010,hwupload", "-c:v", "av1_vaapi",
			"-f", "matroska", av),
		"a preroll": {"-hide_banner", "-nostdin", "-nostats", "-v", "error", "-y", "-vaapi_device", "/dev/dri/renderD128",
			"-i", clip, "-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo", "-map", "0:v:0",
			"-vf", "fps=24000/1001,scale=1920:1080:force_original_aspect_ratio=increase:flags=lanczos,crop=1920:1080," +
				"format=p010,setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709:chroma_location=left,hwupload",
			"-frames:v", "120", "-c:v", "av1_vaapi", "-level", "5.1", "-tier", "0", "-map", "1:a:0",
			"-c:a:0", "libopus", "-b:a:0", "128k", "-filter:a:0", "aresample=48000,aformat=channel_layouts=stereo,apad,atrim=end=5.005000",
			"-f", "matroska", result},
	} {
		got, err := l.passArgs(args)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		// Every input that is a file, and only those, is kept to local files.
		for i, a := range got {
			if a != "-i" {
				continue
			}
			file := got[i+1] != "anullsrc=r=48000:cl=stereo"
			limited := i >= 2 && got[i-2] == "-protocol_whitelist" && got[i-1] == "file"
			if file != limited {
				t.Errorf("%s: input %s limited to local files: %v", name, got[i+1], limited)
			}
		}
	}
}

func TestPassesCantReachOutsideTheFoldersAllowed(t *testing.T) {
	media, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(media, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(media, "in.mkv"), filepath.Join(media, "inside")); err != nil {
		t.Fatal(err)
	}
	l := &link{allow: resolveAll([]string{media})}
	in, out := filepath.Join(media, "in.mkv"), filepath.Join(media, "out.mkv")
	for why, args := range map[string][]string{
		"a URL":                        {"-i", "http://example.com/a.mkv", out},
		"a file URL":                   {"-i", "file:" + in, out},
		"a relative input":             {"-i", "in.mkv", out},
		"a relative output":            {"-i", in, "out.mkv"},
		"an output elsewhere":          {"-i", in, filepath.Join(outside, "out.mkv")},
		"an input through a symlink":   {"-i", filepath.Join(media, "link", "in.mkv"), out},
		"an output through a symlink":  {"-i", in, filepath.Join(media, "link", "new.mkv")},
		"climbing out":                 {"-i", media + "/../in.mkv", out},
		"a list of files":              {"-f", "concat", "-safe", "0", "-i", in, out},
		"a filter that reads files":    {"-i", in, "-vf", "scale=10:10,movie=/etc/hostname", out},
		"a graph that reads files":     {"-i", in, "-filter_complex", "[0:v]null[v];amovie@x=/etc/hostname[a]", out},
		"an input graph that reads":    {"-f", "lavfi", "-i", "[out0]movie=/etc/hostname", out},
		"a stream's filter that loads": {"-i", in, "-filter:a:0", " ladspa=f=x", out},
		"a log file":                   {"-i", in, "-passlogfile", "stats", out},
		"an option read from a file":   {"-i", in, "-/vf", filepath.Join(outside, "graph.txt"), out},
		"a progress file":              {"-progress", filepath.Join(outside, "progress"), "-i", in, out},
		"another path elsewhere":       {"-hwaccel_device", filepath.Join(outside, "card"), "-i", in, out},
		"standard output":              {"-i", in, "-f", "matroska", "-"},
	} {
		if got, err := l.passArgs(args); err == nil {
			t.Errorf("%s was let through: %q", why, got)
		}
	}
	if _, err := l.passArgs([]string{"-i", filepath.Join(media, "inside"), out}); err != nil {
		t.Errorf("a symlink that stays in the folder was refused: %v", err)
	}
	// With nothing to allow, anything goes, as it was sent.
	args := []string{"-i", "http://example.com/a.mkv", "out.mkv"}
	if got, err := (&link{}).passArgs(args); err != nil || !slices.Equal(got, args) {
		t.Errorf("with no -allow, got %q, %v", got, err)
	}
}

// Only the arguments that are files are mapped onto the worker's paths, not
// the likes of tags that happen to look like them.
func TestOnlyFilesAreMapped(t *testing.T) {
	l := &link{paths: []PathMap{{"/daemon", "/here"}}}
	got, err := l.passArgs([]string{"-i", "/daemon/in.mkv", "-metadata", "/daemon/not a file", "-progress", "pipe:1",
		"-passlogfile", "/daemon/log", "-f", "matroska", "/daemon/out.mkv"})
	want := []string{"-i", "/here/in.mkv", "-metadata", "/daemon/not a file", "-progress", "pipe:1",
		"-passlogfile", "/here/log", "-f", "matroska", "/here/out.mkv"}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("got %q, %v, want %q", got, err, want)
	}
}
