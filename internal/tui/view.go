package tui

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/haydenkz/cav1ar/internal/encode"
)

func (m Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "cav1ar"
	if n := len(m.jobs); n > 0 {
		v.WindowTitle = fmt.Sprintf("cav1ar · %d/%d done", m.count(encode.Done), n)
	}
	return v
}

func (m Model) render() string {
	if m.width == 0 {
		return ""
	}
	if m.width < 50 || m.height < 12 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
			m.st.dim.Render("terminal too small"))
	}

	var body string
	switch m.screen {
	case screenQueue:
		body = lipgloss.JoinVertical(lipgloss.Left,
			m.st.box(m.jobsTitle(), m.jobList(), m.width, m.jobsH),
			m.st.box(m.logTitle(), m.logBody(), m.width, m.logH),
		)
	case screenSettings:
		body = m.st.box("Settings", m.settingsTabs()+"\n\n"+m.form.View(), m.width, m.bodyHeight())
	case screenAdd:
		body = m.st.box(m.pickerTitle(), m.picker.View(), m.width, m.bodyHeight())
	}
	return lipgloss.JoinVertical(lipgloss.Left, m.header(), "", body, m.footer())
}

func (m Model) header() string {
	left := m.st.logo.Render("cav1ar") + " "
	switch {
	case m.lost:
		left += m.st.badText.Render("lost the daemon; reopen cav1ar") + "  "
	case m.queue.Daemon():
		left += m.st.okText.Render("daemon") + "  "
	}
	// Ahead of the settings, which can run past the edge.
	switch n := m.count(encode.Running); {
	case m.paused, m.holding && n == 0:
		left += m.st.warnText.Render("⏸ paused") + "  "
	case m.holding:
		left += m.st.warnText.Render(fmt.Sprintf("⏸ pausing once %d %s", n, plural(n, "job finishes", "jobs finish"))) + "  "
	}
	left += m.st.dim.Render(m.settings.Summary())
	if n := len(m.settings.Watch); n > 0 && m.queue.Watching() {
		left += "  " + m.st.okText.Render(fmt.Sprintf("watching %d %s", n, plural(n, "folder", "folders")))
	} else if n > 0 {
		left += "  " + m.st.dim.Render("folders watched by another cav1ar")
	}

	var parts []string
	add := func(n int, s lipgloss.Style, label string) {
		if n > 0 {
			parts = append(parts, s.Render(fmt.Sprintf("%s %d", label, n)))
		}
	}
	add(m.count(encode.Running), m.st.selected, "● running")
	add(m.count(encode.Queued), m.st.dim, "○ queued")
	add(m.count(encode.Done), m.st.okText, "✓ done")
	add(m.count(encode.Failed), m.st.badText, "✗ failed")
	add(m.count(encode.Canceled), m.st.dim, "– canceled")
	right := strings.Join(parts, "   ")

	gap := m.width - 2 - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 2 { // no room for the counts
		right, gap = "", 0
	}
	return ansi.Truncate(" "+left+strings.Repeat(" ", gap)+right, m.width, "…")
}

func (m Model) footer() string {
	var s string
	switch {
	case m.confirmQuit:
		n := m.count(encode.Running)
		s = m.st.warnText.Render(fmt.Sprintf(
			"%d %s still running. Press q again to cancel and quit.", n, plural(n, "encode", "encodes")))
	case m.status != "" && m.statusBad:
		s = m.st.badText.Render(m.status)
	case m.status != "":
		s = m.st.okText.Render(m.status)
	case m.screen == screenSettings:
		s = m.help.ShortHelpView(m.keys.settingsHelp())
	case m.screen == screenAdd:
		s = m.help.ShortHelpView(m.keys.pickerHelp())
	default:
		s = m.help.View(m.keys)
	}
	// help can overrun its width when there is no room for its ellipsis.
	return lipgloss.NewStyle().Padding(0, 1).MaxWidth(m.width).Render(s)
}

// settingsTabs shows the settings pages, highlighting the current one.
func (m Model) settingsTabs() string {
	current := m.settingsPage()
	tabs := make([]string, len(settingsPages))
	for i, name := range settingsPages {
		if i == current {
			tabs[i] = m.st.selected.Render(name)
		} else {
			tabs[i] = m.st.dim.Render(name)
		}
	}
	return ansi.Truncate(strings.Join(tabs, m.st.dim.Render("  ›  ")), m.width-4, "…")
}

// pickerTitle names the folder being browsed, dropping the start of the path
// rather than the end when it is too long to fit.
func (m Model) pickerTitle() string {
	const prefix = "Add files · "
	dir := shortPath(m.picker.CurrentDirectory)
	for _, f := range m.settings.Watch {
		if f.Path != m.picker.CurrentDirectory {
			continue
		}
		switch f.Kind {
		case encode.FolderMovies:
			dir += " (watched as movies)"
		case encode.FolderShows:
			dir += " (watched as shows)"
		default:
			dir += " (watched)"
		}
	}
	if over := lipgloss.Width(prefix+dir) - (m.width - 6); over > 0 {
		dir = "…" + ansi.TruncateLeft(dir, over+1, "")
	}
	return prefix + dir
}

func (m Model) jobsTitle() string {
	title := "Jobs"
	if shown := m.rows(); len(shown) > m.jobsH-2 {
		title = fmt.Sprintf("Jobs %d/%d", slices.Index(shown, m.cursor)+1, len(shown))
	}
	if n := m.count(encode.Done) + m.count(encode.Canceled); m.hideDone && n > 0 {
		title += fmt.Sprintf(" · %d finished hidden", n)
	}
	if s := m.progressSummary(); s != "" {
		title += " · " + s
	}
	return title
}

// progressSummary says how much is left to do, about how long it will take
// at the pace of the last hour, and how much smaller the results have come
// out than their sources.
func (m Model) progressSummary() string {
	now := time.Now()
	since := now.Add(-time.Hour) // the pace is taken over this hour, or since work began
	began := now
	var left, recent, saved int64
	todo := 0
	for _, j := range m.jobs {
		if !j.Started.IsZero() && j.Started.Before(began) {
			began = j.Started
		}
		worked := j.Action == encode.ActionRemux || j.Action == encode.ActionEncode
		switch j.State {
		case encode.Queued:
			todo++
			left += j.InSize
		case encode.Running:
			todo++
			left += int64(float64(j.InSize) * (1 - j.Progress))
		case encode.Done:
			if worked && j.InSize > 0 && j.OutSize > 0 {
				saved += j.InSize - j.OutSize
			}
			if worked && j.Finished.After(since) {
				recent += j.InSize
			}
		}
	}

	var parts []string
	if todo > 0 {
		s := fmt.Sprintf("%d to do, %s", todo, fmtSize(left))
		if began.After(since) {
			since = began
		}
		// Guessing from less than a few minutes' work would swing about.
		if window := now.Sub(since); window >= 5*time.Minute && recent > 0 {
			eta := time.Duration(float64(left) / float64(recent) * float64(window))
			s += ", about " + fmtRough(eta) + " left"
		}
		parts = append(parts, s)
	}
	switch {
	case saved > 0:
		parts = append(parts, "saved "+fmtSize(saved))
	case saved < 0:
		parts = append(parts, fmtSize(-saved)+" bigger")
	}
	// Over every run, which outlasts the jobs listed here.
	if all, n := m.queue.Totals(); n > 0 && all != saved {
		if all >= 0 {
			parts = append(parts, fmt.Sprintf("%s saved over %d %s in all", fmtSize(all), n, plural(n, "file", "files")))
		} else {
			parts = append(parts, fmt.Sprintf("%s bigger over %d %s in all", fmtSize(-all), n, plural(n, "file", "files")))
		}
	}
	return strings.Join(parts, " · ")
}

// columns are the widths of the parts of a job row, in cells.
type columns struct{ name, bar, info int }

const (
	gutterW  = 2 // selection marker
	iconW    = 2 // status icon
	percentW = 5
)

func columnsFor(w int) columns {
	c := columns{bar: min(max(w/4, 10), 30), info: 38}
	c.name = w - gutterW - iconW - c.bar - percentW - c.info - 3
	if c.name < 16 { // too narrow for the info column, so drop it
		c.name += c.info + 1
		c.info = 0
	}
	return c
}

func (m Model) jobList() string {
	if len(m.jobs) == 0 {
		hint := m.st.dim.Render("Nothing queued yet. Press ") + m.st.selected.Render("a") +
			m.st.dim.Render(" to add files, or ") + m.st.selected.Render("s") +
			m.st.dim.Render(" to change settings.")
		return ansi.Truncate(hint, m.width-4, "…")
	}
	shown := m.rows()
	if len(shown) == 0 {
		return m.st.dim.Render("Everything has finished. Press ") + m.st.selected.Render("h") +
			m.st.dim.Render(" to show it.")
	}
	cols := columnsFor(m.width - 4)
	start := min(m.offset, len(shown))
	end := min(start+m.jobsH-2, len(shown))
	rows := make([]string, 0, end-start)
	for _, i := range shown[start:end] {
		rows = append(rows, m.jobRow(m.jobs[i], i == m.cursor, cols))
	}
	return strings.Join(rows, "\n")
}

func (m Model) jobRow(j encode.Job, selected bool, c columns) string {
	gutter, name := "  ", cell(filepath.Base(j.Input), c.name)
	if selected {
		gutter, name = m.st.selected.Render("▌ "), m.st.selected.Render(name)
	}

	var icon, bar, pct, info string
	switch j.State {
	case encode.Queued:
		icon = m.st.dim.Render("○")
		bar = m.st.dim.Render(cell("queued", c.bar))
		if j.Next {
			icon, bar = m.st.selected.Render("○"), m.st.selected.Render(cell("up next", c.bar))
		}
		if j.InSize > 0 {
			info = m.st.dim.Render(cell(fmtSize(j.InSize), c.info))
		}
	case encode.Running:
		icon = m.spin.View()
		if j.Duration > 0 {
			bar, pct = m.bar.ViewAs(j.Progress), fmt.Sprintf("%3.0f%%", j.Progress*100)
		} else {
			bar = m.st.dim.Render(cell(working(j), c.bar))
		}
		info = cell(runningInfo(j), c.info)
		if m.paused {
			icon, info = m.st.warnText.Render("⏸"), m.st.warnText.Render(cell("paused", c.info))
		}
	case encode.Done:
		icon = m.st.okText.Render("✓")
		bar, pct = m.bar.ViewAs(1), "100%"
		info = m.st.okText.Render(cell(doneInfo(j), c.info))
		switch j.Action {
		case encode.ActionNone:
			bar, pct = m.st.dim.Render(cell("nothing to do", c.bar)), ""
			info = ""
		case encode.ActionRename:
			bar, pct = m.st.okText.Render(cell("renamed", c.bar)), ""
			info = ""
		case encode.ActionRetag:
			bar, pct = m.st.okText.Render(cell("tags fixed", c.bar)), ""
			info = ""
		}
		if j.Note != "" {
			info = m.st.warnText.Render(cell(j.Note+", see log", c.info))
		}
	case encode.Failed:
		icon = m.st.badText.Render("✗")
		bar = m.st.badText.Render(cell("failed", c.bar))
		if j.Err != nil {
			info = m.st.badText.Render(cell(j.Err.Error(), c.info))
		}
	case encode.Canceled:
		icon = m.st.dim.Render("–")
		bar = m.st.dim.Render(cell("canceled", c.bar))
	}

	row := gutter + cell(icon, iconW) + name + " " + cell(bar, c.bar) + " " + cell(pct, percentW)
	if c.info > 0 {
		row += " " + cell(info, c.info)
	}
	return row
}

// working says what a running job is doing before it reports progress: still
// looking the file over, until it has decided what the file needs.
func working(j encode.Job) string {
	if j.Step != "" {
		return j.Step + "…"
	}
	switch j.Action {
	case encode.ActionRemux:
		return "remuxing…"
	case encode.ActionEncode:
		return "encoding…"
	case encode.ActionRetag:
		return "fixing tags…"
	}
	return "checking…"
}

// actionName is a short name for what a job does to its file, or "" if it
// hasn't decided yet.
func actionName(a encode.Action) string {
	switch a {
	case encode.ActionRemux:
		return "remux"
	case encode.ActionEncode:
		return "encode"
	case encode.ActionRetag:
		return "retag"
	}
	return ""
}

func runningInfo(j encode.Job) string {
	var parts []string
	label := actionName(j.Action)
	if j.Step != "" {
		label = j.Step
	}
	if label != "" {
		parts = append(parts, label)
	}
	// A remux copies frames, as does adding a preroll, so their rate means little.
	if j.FPS > 0 && j.Action != encode.ActionRemux && j.Step == "" {
		parts = append(parts, fmt.Sprintf("%.0f fps", j.FPS))
	}
	switch {
	case j.Speed >= 10:
		parts = append(parts, fmt.Sprintf("%.0fx", j.Speed))
	case j.Speed > 0:
		parts = append(parts, fmt.Sprintf("%.2gx", j.Speed))
	}
	if eta := j.ETA(); eta > 0 {
		parts = append(parts, fmtDuration(eta)+" left")
	}
	return strings.Join(parts, " · ")
}

func doneInfo(j encode.Job) string {
	s := "in " + fmtDuration(j.Elapsed())
	switch j.Action {
	case encode.ActionRemux:
		s = "remuxed " + s
	case encode.ActionEncode:
		s = "encoded " + s
	}
	if j.InSize > 0 && j.OutSize > 0 {
		s += fmt.Sprintf(" · %s (%+.0f%%)", fmtSize(j.OutSize),
			(float64(j.OutSize)/float64(j.InSize)-1)*100)
	}
	return s
}

func (m Model) logTitle() string {
	if m.cursor >= len(m.jobs) {
		return "Log"
	}
	j := m.jobs[m.cursor]
	title := "Log · " + filepath.Base(j.Input)
	if j.Output != "" {
		title += " → " + filepath.Base(j.Output)
	}
	return title
}

func (m Model) logBody() string {
	if m.cursor >= len(m.logs) || len(m.logs[m.cursor]) == 0 {
		return m.st.dim.Render("No output yet")
	}
	return m.log.View()
}

// fmtRough rounds a long wait to what's worth knowing of it.
func fmtRough(d time.Duration) string {
	switch {
	case d >= 10*time.Hour:
		return fmt.Sprintf("%dh", int(d.Round(time.Hour).Hours()))
	case d >= time.Hour:
		d = d.Round(10 * time.Minute)
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dm", int(max(d.Round(time.Minute), time.Minute).Minutes()))
}

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h, mnt, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, mnt, s)
	}
	return fmt.Sprintf("%d:%02d", mnt, s)
}

func fmtSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
