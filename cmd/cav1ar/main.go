// Command cav1ar encodes video to AV1, from a terminal UI or as a daemon.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	tea "charm.land/bubbletea/v2"

	"github.com/haydenkz/cav1ar/internal/config"
	"github.com/haydenkz/cav1ar/internal/encode"
	"github.com/haydenkz/cav1ar/internal/remote"
	"github.com/haydenkz/cav1ar/internal/tui"
	"github.com/haydenkz/cav1ar/internal/worker"
)

// version is set by release builds.
var version = "dev"

func main() {
	// Settings that can't be read stop the UI and the daemon, but not
	// -version, -worker and -devices, which don't need them.
	saved, loadErr := config.Load()

	// Flags default to the saved settings and override them for this run only.
	settings := saved
	daemon := flag.Bool("daemon", false, "run without the UI, logging to stderr; follows the saved settings as they change")
	dryRun := flag.Bool("dry-run", false, "say what would be done to the files given, or in the watched folders, and exit")
	retryFailed := flag.Bool("retry-failed", false, "in watched folders, retry videos whose jobs failed before")
	local := flag.Bool("local", false, "open the UI with a queue of its own, even when a daemon is running")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Func("encoders", "how many files each device encodes at once, e.g. nvidia=2,cpu=0; "+
		"name a device by kind (cpu, nvidia, vaapi) or by its ID from -devices, and 0 turns it off", func(v string) error {
		settings.Encoders = maps.Clone(settings.Encoders)
		if settings.Encoders == nil {
			settings.Encoders = map[string]int{}
		}
		for item := range strings.SplitSeq(v, ",") {
			id, n, ok := strings.Cut(strings.TrimSpace(item), "=")
			count, err := strconv.Atoi(n)
			if !ok || id == "" || err != nil {
				return fmt.Errorf("%q isn't device=number", item)
			}
			settings.Encoders[id] = count
		}
		return nil
	})
	listDevices := flag.Bool("devices", false, "list what can encode AV1 here, and exit")
	flag.StringVar(&settings.WebListen, "web", saved.WebListen, "with -daemon: serve the web UI on this address; empty for none")
	workFor := flag.String("worker", "", "lend this machine's GPUs to the cav1ar daemon at this address, such as 192.168.1.10:7310, "+
		"instead of encoding for itself; it connects with the daemon's worker token")
	host, _ := os.Hostname()
	workerName := flag.String("worker-name", host, "with -worker: the name the daemon knows this worker by")
	allow := flag.String("allow", "", "with -worker: the folders the daemon's passes may use, comma-separated, as this worker sees them; empty allows any")
	var paths []worker.PathMap
	flag.Func("map", "with -worker: where a folder the daemon sees is on this worker, as daemon-path=worker-path, "+
		"for when they aren't at the same paths (repeatable)", func(v string) error {
		m, err := worker.ParsePathMap(v)
		paths = append(paths, m)
		return err
	})
	ffmpeg := flag.String("ffmpeg", encode.FFmpeg, "with -worker: the ffmpeg to encode with")
	flag.IntVar(&settings.CRF, "crf", saved.CRF, "quality, 1–63; lower is better quality and bigger files, about the same on every encoder")
	flag.IntVar(&settings.Preset, "preset", saved.Preset, "CPU speed, 0 (slowest, smallest files) to 13 (fastest)")
	flag.IntVar(&settings.NVENCPreset, "nvenc-preset", saved.NVENCPreset, "NVIDIA speed, 1 (fastest) to 7 (slowest, smallest files)")
	flag.IntVar(&settings.VAAPIPreset, "vaapi-speed", saved.VAAPIPreset, "AMD/Intel speed, 1 (slowest) to 7 (fastest) as Intel's driver counts it; 0 leaves it to the driver")
	flag.IntVar(&settings.MaxHeight, "max-height", saved.MaxHeight, "scale bigger video down to this height, e.g. 1080; 0 keeps the size")
	flag.Func("audio-codec", "audio codec: opus or aac (default \""+string(saved.AudioCodec)+"\")",
		func(v string) error { settings.AudioCodec = encode.AudioCodec(v); return nil })
	flag.IntVar(&settings.AudioKbps, "audio", saved.AudioKbps, "audio bitrate in kbps; -1 picks one per track, 0 copies the original audio")
	flag.BoolVar(&settings.Downmix, "stereo", saved.Downmix, "downmix surround audio to stereo (needs encoded audio)")
	flag.StringVar(&settings.Language, "lang", saved.Language, "language for the track filters, e.g. eng")
	flag.Func("audio-tracks", "audio tracks to keep: all, language, or the best one in it (default \""+string(saved.AudioTracks)+"\")",
		func(v string) error { settings.AudioTracks = encode.TrackFilter(v); return nil })
	flag.Func("subs", "subtitles to keep: all, language or none (default \""+string(saved.Subtitles)+"\")",
		func(v string) error { settings.Subtitles = encode.TrackFilter(v); return nil })
	flag.BoolVar(&settings.TidySubs, "tidy-subs", saved.TidySubs,
		"when two subtitle tracks in a language look alike, drop duplicates and ones in the wrong language, and flag forced and SDH ones")
	flag.BoolVar(&settings.StripMetadata, "strip", saved.StripMetadata, "strip the source's titles and tags")
	flag.StringVar(&settings.Title, "title", saved.Title, "title shown in players; placeholders as for -name")
	flag.StringVar(&settings.ReleaseGroup, "group", saved.ReleaseGroup, "release group, for {group} and the ENCODED_BY tag")
	flag.StringVar(&settings.NameTemplate, "name", saved.NameTemplate,
		"output file name; placeholders: "+strings.Join(encode.NamePlaceholders, " "))
	flag.StringVar(&settings.OutDir, "out", saved.OutDir, "folder for encodes; empty saves next to each source")
	flag.BoolVar(&settings.DeleteOriginal, "delete", saved.DeleteOriginal, "permanently delete each original once its result checks out")
	flag.Func("watch", "keep a folder's videos encoded, including in subfolders (repeatable); "+
		"prefix movies: or shows: to name them after their folders, and a number such as +1 to run them first", func(v string) error {
		f := encode.ParseWatchFolder(v)
		if f.Path == "" {
			return errors.New("give the folder to watch")
		}
		abs, err := filepath.Abs(f.Path)
		f.Path = abs
		settings.Watch = append(settings.Watch, f)
		return err
	})
	flag.BoolVar(&settings.WatchExisting, "watch-existing", saved.WatchExisting,
		"also handle videos already in a folder when it is first watched")
	flag.StringVar(&settings.Preroll, "preroll", saved.Preroll, "a clip to put before every result; empty for none")
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "usage: cav1ar [flags] [file or folder...]\n\n")
		fmt.Fprintf(out, "Opens the encoder UI, queueing the files given and every video in the folders.\n")
		fmt.Fprintf(out, "When a cav1ar daemon is running, the UI shows and controls its queue instead.\n")
		fmt.Fprintf(out, "Settings saved in the UI are the defaults for these flags.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println("cav1ar", version)
		return
	}
	given := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if err := misplacedFlags(given); err != nil {
		fail(2, "%v", err)
	}
	if *workFor != "" {
		encode.FFmpeg = *ffmpeg
		if err := runWorker(*workFor, *workerName, *allow, paths); err != nil {
			fail(1, "%v", err)
		}
		return
	}
	if *listDevices {
		if loadErr != nil {
			fmt.Fprintf(os.Stderr, "cav1ar: reading settings: %v\nShowing how many files each device encodes at once by default.\n", loadErr)
		}
		devices := encode.Devices()
		if len(devices) == 0 {
			fail(1, "%v", encode.ErrNoEncoder)
		}
		for _, d := range devices {
			at := "off"
			if n := settings.Slots(d, devices); n > 0 {
				at = fmt.Sprintf("%d at once", n)
			}
			fmt.Printf("%-24s %-28s %s\n", d.ID, d.Name, at)
		}
		return
	}

	if loadErr != nil {
		fail(1, "reading settings: %v\nCorrect that in the file to continue.", loadErr)
	}
	if err := settings.Validate(); err != nil {
		fail(2, "%v", err)
	}
	if fi, err := os.Stat(settings.OutDir); settings.OutDir != "" && (err != nil || !fi.IsDir()) {
		fail(2, "output folder %s doesn't exist", settings.OutDir)
	}
	go encode.Devices() // find them now, so it's done by the time it matters

	statePath, err := config.StatePath()
	if err != nil {
		fail(1, "%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if *daemon || *dryRun {
		ctx, cancel = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	}
	if *dryRun {
		err := runDryRun(ctx, settings, flag.Args())
		cancel()
		if err != nil {
			fail(1, "%v", err)
		}
		return
	}
	if !*daemon && !*local {
		c, err := remote.Dial()
		if err == nil {
			cancel()
			if err := runAttached(c, flag.Args()); err != nil {
				fail(1, "%v", err)
			}
			return
		}
		if !errors.Is(err, remote.ErrNoDaemon) { // one is running, but can't be shown
			fail(1, "%v", err)
		}
	}
	q := encode.NewQueue(ctx, settings)
	w := encode.Watch(ctx, q, statePath, *retryFailed)
	for _, arg := range flag.Args() {
		if err := add(q, arg); err != nil {
			fail(1, "%v", err)
		}
	}

	if *daemon {
		err = runDaemon(ctx, q, w, len(flag.Args()) > 0)
	} else {
		_, err = tea.NewProgram(tui.New(tui.Local(q, w))).Run()
	}
	cancel()
	q.Close()
	if !*daemon {
		printSummary(q.Jobs())
	}
	if err != nil {
		fail(1, "%v", err)
	}
}

// runAttached opens the UI on a daemon's queue, adding the files given to it.
// The daemon's settings apply, so flags that change them are refused.
func runAttached(c *remote.Client, paths []string) error {
	defer c.Close()
	var set []string
	flag.Visit(func(f *flag.Flag) { set = append(set, "-"+f.Name) })
	if len(set) > 0 {
		return fmt.Errorf("a cav1ar daemon is running, and the UI shows its queue with its settings, so %s "+
			"can't apply; leave it out, or add -local to encode with a queue of the UI's own", strings.Join(set, " "))
	}
	for _, path := range paths {
		if err := add(c, path); err != nil {
			return err
		}
	}
	_, err := tea.NewProgram(tui.New(c)).Run()
	return err
}

// workerFlags are the flags a worker takes. The rest say how to encode,
// which a worker leaves to its daemon.
var workerFlags = []string{"worker", "worker-name", "allow", "map", "ffmpeg"}

// misplacedFlags says why the flags given, by name, can't all apply
// together, rather than letting some of them be ignored.
func misplacedFlags(given map[string]bool) error {
	if given["worker"] {
		var other []string
		for name := range given {
			if !slices.Contains(workerFlags, name) {
				other = append(other, "-"+name)
			}
		}
		if len(other) > 0 {
			slices.Sort(other)
			return fmt.Errorf("a worker encodes as its daemon's settings say, so %s can't apply with -worker", strings.Join(other, " "))
		}
		return nil
	}
	for _, name := range workerFlags[1:] {
		if given[name] {
			return fmt.Errorf("-%s only applies with -worker", name)
		}
	}
	switch {
	case given["local"] && given["daemon"]:
		return errors.New("-local opens the UI with a queue of its own, and -daemon runs without the UI; give one or the other")
	case given["web"] && !given["daemon"]:
		return errors.New("-web only applies with -daemon, which serves the web UI")
	}
	return nil
}

// add queues a file, or every video in a folder and its subfolders, by its
// full path: a daemon would take a relative one to be in its own folder.
func add(q interface{ Add(string) }, path string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		q.Add(path) // a missing file fails visibly in the queue
		return nil
	}
	videos, err := encode.FindVideos(path)
	for _, v := range videos {
		q.Add(v)
	}
	return err
}

// printSummary leaves a record in the scrollback once the UI closes.
func printSummary(jobs []encode.Job) {
	for _, j := range jobs {
		switch j.State {
		case encode.Done:
			note := ""
			if j.Note != "" {
				note = " (" + j.Note + ")"
			}
			if j.Action == encode.ActionNone {
				fmt.Printf("✓ %s: nothing to do\n", j.Input)
			} else {
				fmt.Printf("✓ %s → %s%s\n", j.Input, j.Output, note)
			}
		case encode.Failed:
			fmt.Printf("✗ %s: %v\n", j.Input, j.Err)
		default:
			fmt.Printf("– %s: %s\n", j.Input, j.State)
		}
	}
}

func fail(code int, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "cav1ar: "+format+"\n", args...)
	os.Exit(code)
}
