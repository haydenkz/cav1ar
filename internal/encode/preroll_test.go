package encode

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestSeamFitsCatchesAnOverlap(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("needs ffmpeg")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("ffmpeg", append([]string{"-v", "error", "-nostdin", "-y"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg: %v: %s", err, out)
		}
	}
	clip := func(name string, secs int) string {
		path := filepath.Join(dir, name)
		run("-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=160x90:rate=24:duration=%d", secs), "-c:v", "mpeg4", path)
		return path
	}
	pre, main := clip("pre.mkv", 2), clip("main.mkv", 3)
	ctx := context.Background()
	if err := seamFits(ctx, pre, main, 2*time.Second); err != nil {
		t.Errorf("a preroll that ends where the video starts failed: %v", err)
	}
	// Putting the video half a second early would overlap the two.
	if err := seamFits(ctx, pre, main, 1500*time.Millisecond); err == nil {
		t.Error("an overlapping join passed")
	}
}
