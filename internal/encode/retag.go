package encode

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// haveMKVToolNix reports whether MKVToolNix's mkvpropedit and mkvextract are
// installed. With them, a Matroska file whose only problems are its
// metadata is fixed in place instead of rewritten.
var haveMKVToolNix = sync.OnceValue(func() bool {
	_, propedit := exec.LookPath("mkvpropedit")
	_, extract := exec.LookPath("mkvextract")
	return propedit == nil && extract == nil
})

// retag edits p.in in place to carry the title, track names, languages,
// flags, chapters and tags a remux would give it, then checks that the
// file now matches the settings. Only the file's headers change, so it
// takes a moment however big the file is.
func retag(ctx context.Context, p *plan) error {
	dir, err := os.MkdirTemp("", "cav1ar-retag-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	args, err := p.retagArgs(ctx, dir)
	if err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, "mkvpropedit", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("mkvpropedit: %s", lastLine(string(out), err))
	}

	info, err := probe(ctx, p.in)
	if err != nil {
		return err
	}
	after, err := newPlan(p.in, p.settings, info)
	if err != nil {
		return err
	}
	if reasons := after.reasons(); len(reasons) > 0 {
		return fmt.Errorf("it still doesn't match: %s", strings.Join(reasons, "; "))
	}
	return nil
}

// retagArgs are mkvpropedit's arguments for retag, mirroring what remuxArgs
// writes; a file of tags for it goes in dir. Tracks are picked by their
// position in the file, which is the stream index ffprobe gives them.
func (p *plan) retagArgs(ctx context.Context, dir string) ([]string, error) {
	s := p.settings
	strip := s.StripMetadata
	args := []string{p.in}

	// The title is set even when it looks right: a file-wide TITLE tag,
	// which retagTags removes, may be what ffprobe showed.
	switch want := p.title(); {
	case want != "":
		args = append(args, "--edit", "info", "--set", "title="+want)
	case p.info.Tags["TITLE"] != "":
		args = append(args, "--edit", "info", "--delete", "title")
	}
	edit := func(st streamInfo, props []string) {
		if len(props) > 0 {
			args = append(append(args, "--edit", "track:"+strconv.Itoa(st.Index+1)), props...)
		}
	}
	if strip && p.video.Title != "" {
		edit(p.video, []string{"--delete", "name"})
	}
	for i, a := range p.audio {
		var props []string
		if strip && a.Title != "" {
			props = append(props, "--delete", "name")
		}
		if a.language != "" && a.language != a.Language {
			props = append(props, "--set", "language="+a.language)
		}
		if i == 0 && !a.Default && (s.AudioTracks == KeepLanguage || s.AudioTracks == KeepBest) {
			props = append(props, "--set", "flag-default=1")
		}
		edit(a.streamInfo, props)
	}
	for _, sub := range p.subs {
		props := []string{
			"--set", "flag-forced=" + strconv.Itoa(b2i(sub.forced)),
			"--set", "flag-hearing-impaired=" + strconv.Itoa(b2i(sub.sdh)),
		}
		switch label := sub.label(); {
		case label != "" && (strip || sub.Title == ""):
			props = append(props, "--set", "name="+label)
		case strip && sub.Title != "":
			props = append(props, "--delete", "name")
		}
		edit(sub.streamInfo, props)
	}
	if strip && p.info.Chapters > 0 {
		args = append(args, "--chapters", "")
	}

	tags, changed, err := p.retagTags(ctx, dir)
	switch {
	case err != nil:
		return nil, err
	case !changed:
	case tags == nil:
		args = append(args, "--tags", "all:")
	default:
		path := filepath.Join(dir, "new-tags.xml")
		if err := os.WriteFile(path, tags, 0o644); err != nil {
			return nil, err
		}
		args = append(args, "--tags", "all:"+path)
	}
	return args, nil
}

// Matroska tags as mkvextract writes them, keeping each part's XML as it is.
type mkvTags struct {
	Tags []struct {
		Targets struct {
			TypeValue string `xml:"TargetTypeValue"`
			XML       string `xml:",innerxml"`
		} `xml:"Targets"`
		Simples []struct {
			Name   string `xml:"Name"`
			String string `xml:"String"`
			XML    string `xml:",innerxml"`
		} `xml:"Simple"`
		XML string `xml:",innerxml"`
	} `xml:"Tag"`
}

// retagTags works out the file's tags after a retag, as tags XML for
// mkvpropedit or nil for none, and whether they differ from the file's now.
// Tags aimed at a track, edition, chapter or attachment are kept as they
// are. Of the file-wide ones, TITLE goes, since the title lives in the
// segment's info; ENCODED_BY is as the settings say; and stripping metadata
// keeps only the ones tagsDiffer allows.
func (p *plan) retagTags(ctx context.Context, dir string) (tags []byte, changed bool, err error) {
	path := filepath.Join(dir, "tags.xml")
	if out, err := exec.CommandContext(ctx, "mkvextract", p.in, "tags", path).CombinedOutput(); err != nil {
		return nil, false, fmt.Errorf("mkvextract: %s", lastLine(string(out), err))
	}
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	old = bytes.TrimPrefix(old, []byte("\ufeff")) // mkvextract starts with a byte order mark
	var doc mkvTags
	if len(bytes.TrimSpace(old)) > 0 {
		if err := xml.Unmarshal(old, &doc); err != nil {
			return nil, false, fmt.Errorf("reading the tags: %w", err)
		}
	}

	keep := func(name string) bool {
		switch strings.ToUpper(name) {
		case "ENCODER", "CREATION_TIME", "DURATION", prerollTag:
			return true
		}
		return !p.settings.StripMetadata
	}
	want := p.encodedBy()
	placed := false // ENCODED_BY written
	var out bytes.Buffer
	n := 0
	write := func(targets, simples string) {
		fmt.Fprintf(&out, "<Tag><Targets>%s</Targets>%s</Tag>\n", targets, simples)
		n++
	}
	for _, t := range doc.Tags {
		if strings.Contains(t.Targets.XML, "UID>") { // aimed at something within the file
			out.WriteString("<Tag>" + t.XML + "</Tag>\n")
			n++
			continue
		}
		var simples strings.Builder
		for _, st := range t.Simples {
			switch name := strings.ToUpper(st.Name); {
			case name == "ENCODED_BY":
				changed = changed || placed || st.String != want
				if !placed && want != "" {
					simples.WriteString(simpleTag("ENCODED_BY", want))
				}
				placed = true
			case name != "TITLE" && keep(name):
				simples.WriteString("<Simple>" + st.XML + "</Simple>")
			default:
				changed = true
			}
		}
		if simples.Len() > 0 {
			write(t.Targets.XML, simples.String())
		}
	}
	if !placed && want != "" { // there was no ENCODED_BY to replace
		write("<TargetTypeValue>50</TargetTypeValue>", simpleTag("ENCODED_BY", want))
		changed = true
	}
	if n == 0 {
		return nil, changed, nil
	}
	return []byte("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<Tags>\n" +
		out.String() + "</Tags>\n"), changed, nil
}

func simpleTag(name, value string) string {
	var b strings.Builder
	b.WriteString("<Simple><Name>")
	xml.EscapeText(&b, []byte(name))
	b.WriteString("</Name><String>")
	xml.EscapeText(&b, []byte(value))
	b.WriteString("</String></Simple>")
	return b.String()
}
