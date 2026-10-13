// Command cav1ar encodes video to AV1, from a terminal UI or as a daemon.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	tea "charm.land/bubbletea/v2"

	"github.com/haydenkz/cav1ar/internal/config"
	"github.com/haydenkz/cav1ar/internal/encode"
	"github.com/haydenkz/cav1ar/internal/tui"
)

// version is set by release builds.
var version = "dev"

func main() {
	saved, err := config.Load()
	if err != nil {
		fail(1, "reading settings: %v\nFix or delete that file to continue.", err)
	}

	// Flags default to the saved settings and override them for this run only.
	settings := saved
	daemon := flag.Bool("daemon", false, "run without the UI, logging to stdout; follows the saved settings as they change")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Func("encoder", "video encoder: cpu, nvidia or vaapi (default \""+string(saved.Encoder)+"\")",
		func(v string) error { settings.Encoder = encode.Encoder(v); return nil })
	flag.IntVar(&settings.CRF, "crf", saved.CRF, "quality, 1–63; lower is better quality and bigger files")
	flag.IntVar(&settings.Preset, "preset", saved.Preset, "speed, 0 (slowest, smallest files) to 13 (fastest)")
	flag.IntVar(&settings.Workers, "j", saved.Workers, "number of files to encode at once")
	flag.IntVar(&settings.AudioKbps, "audio", saved.AudioKbps, "Opus bitrate in kbps; -1 picks one per track, 0 copies the original audio")
	flag.BoolVar(&settings.Downmix, "stereo", saved.Downmix, "downmix audio to stereo (needs Opus)")
	flag.StringVar(&settings.Language, "lang", saved.Language, "language for the track filters, e.g. eng")
	flag.Func("audio-tracks", "audio tracks to keep: all or language (default \""+string(saved.AudioTracks)+"\")",
		func(v string) error { settings.AudioTracks = encode.TrackFilter(v); return nil })
	flag.Func("subs", "subtitles to keep: all, language or none (default \""+string(saved.Subtitles)+"\")",
		func(v string) error { settings.Subtitles = encode.TrackFilter(v); return nil })
	flag.BoolVar(&settings.StripMetadata, "strip", saved.StripMetadata, "strip the source's titles and tags")
	flag.StringVar(&settings.Title, "title", saved.Title, "title shown in players; placeholders as for -name")
	flag.StringVar(&settings.ReleaseGroup, "group", saved.ReleaseGroup, "release group, for {group} and the ENCODED_BY tag")
	flag.StringVar(&settings.NameTemplate, "name", saved.NameTemplate,
		"output file name; placeholders: "+strings.Join(encode.NamePlaceholders, " "))
	flag.StringVar(&settings.OutDir, "out", saved.OutDir, "folder for encodes; empty saves next to each source")
	flag.BoolVar(&settings.DeleteOriginal, "delete", saved.DeleteOriginal, "permanently delete each original once its encode checks out")
	flag.Func("watch", "watch a folder and its subfolders for new videos (repeatable)", func(dir string) error {
		abs, err := filepath.Abs(dir)
		settings.Watch = append(settings.Watch, abs)
		return err
	})
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "usage: cav1ar [flags] [file or folder...]\n\n")
		fmt.Fprintf(out, "Opens the encoder UI, queueing the files given and every video in the folders.\n")
		fmt.Fprintf(out, "Settings saved in the UI are the defaults for these flags.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println("cav1ar", version)
		return
	}

	if err := settings.Validate(); err != nil {
		fail(2, "%v", err)
	}
	if fi, err := os.Stat(settings.OutDir); settings.OutDir != "" && (err != nil || !fi.IsDir()) {
		fail(2, "output folder %s doesn't exist", settings.OutDir)
	}
	go encode.CheckEncoder(settings.Encoder) // test the encoders now, so it's done by the time it matters

	watchState, err := config.WatchStatePath()
	if err != nil {
		fail(1, "%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if *daemon {
		ctx, cancel = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	}
	q := encode.NewQueue(ctx, settings)
	w := encode.Watch(ctx, q, watchState)
	for _, arg := range flag.Args() {
		if err := add(q, arg); err != nil {
			fail(1, "%v", err)
		}
	}

	if *daemon {
		err = runDaemon(ctx, q, w, len(flag.Args()) > 0)
	} else {
		_, err = tea.NewProgram(tui.New(q, w)).Run()
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

// add queues a file, or every video in a folder and its subfolders.
func add(q *encode.Queue, path string) error {
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
			fmt.Printf("✓ %s → %s%s\n", j.Input, j.Output, note)
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
