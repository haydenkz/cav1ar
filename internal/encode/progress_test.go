package encode

import (
	"strings"
	"testing"
	"time"
)

// Audio encodes far faster than video and runs out_time to the end early,
// so a pass that writes video is measured by its frames, and the bar doesn't
// reach the end and then fall back.
func TestReadProgressFollowsTheVideoFrames(t *testing.T) {
	// 100 s at 25 fps is 2500 frames. The audio is at 99 s, the video at
	// frame 1500, so 60% along; then the audio ends and out_time drops back.
	in := "frame=1500\nfps=500.00\nout_time_us=99000000\nspeed=33x\nprogress=continue\n" +
		"frame=1550\nfps=500.00\nout_time_us=62000000\nspeed=20x\nprogress=continue\n"
	var got []progress
	readProgress(strings.NewReader(in), 100*time.Second, 25, func(p progress) { got = append(got, p) })
	if len(got) != 2 {
		t.Fatalf("want 2 reports, got %d", len(got))
	}
	if got[0].done != 0.6 || got[1].done != 0.62 {
		t.Errorf("want progress 0.60 then 0.62 from the frames, got %.2f then %.2f", got[0].done, got[1].done)
	}
	if got[0].speed != 20 {
		t.Errorf("want speed 20x (500 fps of 25 fps video), got %gx", got[0].speed)
	}
}

// A pass with no video, such as encoding the audio again, has only out_time.
func TestReadProgressWithoutVideoUsesTheTime(t *testing.T) {
	in := "frame=0\nfps=0.00\nout_time_us=30000000\nspeed=95x\nprogress=continue\n"
	var got progress
	readProgress(strings.NewReader(in), 100*time.Second, 0, func(p progress) { got = p })
	if got.done != 0.3 || got.speed != 95 {
		t.Errorf("want progress 0.30 at 95x, got %.2f at %gx", got.done, got.speed)
	}
}
