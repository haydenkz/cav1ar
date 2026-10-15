package main

import (
	"bytes"
	"errors"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/haydenkz/cav1ar/internal/encode"
)

// The daemon's log numbers each job's lines, marks out its stages, leaves
// out ffmpeg's output, and puts a failure down to the stage it happened in.
func TestLogEventMarksStages(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	flags := log.Flags()
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(os.Stderr); log.SetFlags(flags) })

	j := encode.Job{ID: 7, Input: "/tv/Show/Show S01E01.mkv", State: encode.Running}
	jobs := map[int]*jobLog{}
	for _, line := range []string{
		"",
		"cav1ar: ▸ checking the file",
		"cav1ar: remux, because of: audio aac 2ch",
		"cav1ar: ▸ encoding the audio, copying the video",
		"cav1ar: ffmpeg -i in.mkv out.mkv",
		"frame=  100 fps=50",
		"cav1ar: ▸ checking the result",
	} {
		logEvent(encode.Event{Job: j, Line: line}, jobs)
	}
	j.State, j.Err = encode.Failed, errors.New("the result failed its checks")
	logEvent(encode.Event{Job: j}, jobs)

	want := strings.Join([]string{
		"#7 started Show S01E01.mkv",
		"#7   ▸ checking the file",
		"#7     remux, because of: audio aac 2ch",
		"#7   ▸ encoding the audio, copying the video",
		"#7   ▸ checking the result",
		"#7 failed Show S01E01.mkv while checking the result: the result failed its checks",
	}, "\n") + "\n"
	if got := buf.String(); got != want {
		t.Errorf("logged:\n%s\nwant:\n%s", got, want)
	}
}
