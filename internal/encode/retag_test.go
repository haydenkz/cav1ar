package encode

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

// A file that only needs its metadata changed is edited in place, not
// rewritten: it keeps its inode, and comes out matching the settings.
func TestRetagFixesMetadataWithoutRewritingTheFile(t *testing.T) {
	if err := CheckEncoder(EncoderCPU); err != nil {
		t.Skip(err)
	}
	if !haveMKVToolNix() {
		t.Skip("needs MKVToolNix")
	}
	dir, outDir := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ctx := context.Background()

	// An encode that matches the first settings, with a chapter list and a
	// subtitle track whose title says forced but whose flag doesn't.
	first := testSettings(outDir)
	p, err := prepare(ctx, makeSource(t, dir), first, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if err := encodeFile(ctx, p.in, first, cpuDevice, noUpdates()); err != nil {
		t.Fatal(err)
	}
	encoded := p.out
	info, err := probe(ctx, encoded)
	if err != nil {
		t.Fatal(err)
	}
	track := "track:" + strconv.Itoa(streamOf(info, "subtitle").Index+1)
	if out, err := exec.Command("mkvpropedit", encoded, "--edit", track, "--set", "name=English Forced").CombinedOutput(); err != nil {
		t.Fatalf("mkvpropedit: %v: %s", err, out)
	}
	before, err := os.Stat(encoded)
	if err != nil {
		t.Fatal(err)
	}

	// Settings that want other tags, no chapters and the forced flag set.
	second := first
	second.ReleaseGroup, second.Title, second.StripMetadata = "OTHER", "{title} ({year})", true
	kept := second
	second.DeleteOriginal = true
	if p, err := prepare(ctx, encoded, kept, func(string) {}); err != nil || p.action() != ActionRemux {
		t.Fatalf("with originals kept, want a remux, got %v (%v)", p.action(), err)
	}
	p, err = prepare(ctx, encoded, second, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if p.action() != ActionRetag {
		t.Fatalf("want a retag, got %s, plan %q", p.action(), p.reasons())
	}
	if err := encodeFile(ctx, encoded, second, cpuDevice, noUpdates()); err != nil {
		t.Fatal(err)
	}

	after, err := os.Stat(p.out)
	if err != nil {
		t.Fatal(err)
	}
	if p.out == encoded {
		t.Fatal("want the new release group in the name")
	}
	if _, err := os.Stat(encoded); err == nil {
		t.Error("the file is still under its old name")
	}
	if !os.SameFile(before, after) {
		t.Error("the file was rewritten, not edited in place")
	}
	info, err = probe(ctx, p.out)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Tags["ENCODED_BY"]; got != "OTHER" {
		t.Errorf("ENCODED_BY is %q, want OTHER", got)
	}
	if got, want := info.Tags["TITLE"], p.title(); got != want {
		t.Errorf("title is %q, want %q", got, want)
	}
	if info.Chapters != 0 {
		t.Errorf("want the chapters stripped, got %d", info.Chapters)
	}
	if sub := streamOf(info, "subtitle"); !sub.Forced || sub.Title != "Forced" {
		t.Errorf("want the subtitle flagged forced and titled Forced, got %v %q", sub.Forced, sub.Title)
	}
	if streamOf(info, "audio").Duration == 0 {
		t.Error("the tracks lost their DURATION tags")
	}
	again, err := newPlan(p.out, second, info)
	if err != nil {
		t.Fatal(err)
	}
	if a := again.action(); a != ActionNone {
		t.Errorf("after the retag, want nothing left to do, got %s, plan %q", a, again.reasons())
	}
}
