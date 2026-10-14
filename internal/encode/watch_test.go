package encode

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRemoveStaleTempsLeavesHeldTemps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".job"+tempSuffix)
	release, err := holdTemp(path)
	if err != nil {
		t.Fatal(err)
	}
	// As if the job had been paused for longer than a temp is kept.
	old := time.Now().Add(-2 * staleTemp)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	removeStaleTemps(dir)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a held temp was removed: %v", err)
	}

	release()
	removeStaleTemps(dir)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a released stale temp was kept: %v", err)
	}
}
