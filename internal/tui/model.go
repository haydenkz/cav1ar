// Package tui is the interactive front end for an encode.Queue.
package tui

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/filepicker"
	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"

	"github.com/haydenkz/cav1ar/internal/encode"
)

// maxLogLines caps how much encoder output is kept per job.
const maxLogLines = 1000

type screen int

const (
	screenQueue screen = iota
	screenSettings
	screenAdd
)

// eventsMsg carries a batch of queue events into Update.
type eventsMsg []encode.Event

// queueClosedMsg says the queue won't send any more events: the UI is
// closing, or the daemon it showed has gone.
type queueClosedMsg struct{}

// Backend is the queue the UI shows and controls: one of its own, or a
// daemon's.
type Backend interface {
	Add(path string)
	Cancel(id int)
	Retry(id int)
	SetPaused(paused bool)
	Paused() bool
	SetHolding(holding bool) // hold back new jobs, letting running ones finish
	Holding() bool
	Totals() (saved int64, results int) // over every run, as recorded by the watching cav1ar
	Jobs() []encode.Job
	Next() []encode.Event
	Settings() encode.Settings
	SetSettings(s encode.Settings)
	Watching() bool // whether this queue's cav1ar is the one watching folders
	Daemon() bool   // whether the queue is a daemon's, which carries on once the UI closes
}

// Local is a queue of the UI's own, whose folders w watches.
func Local(q *encode.Queue, w *encode.Watcher) Backend { return local{q, w} }

type local struct {
	*encode.Queue
	w *encode.Watcher
}

func (l local) Watching() bool       { return l.w.Active() }
func (l local) Daemon() bool         { return false }
func (l local) Totals() (int64, int) { return l.w.Totals() }

// clearStatusMsg hides the status line, unless a newer status replaced it.
type clearStatusMsg struct{ id int }

// Model is the root Bubble Tea model.
type Model struct {
	queue    Backend
	settings encode.Settings
	paused   bool
	holding  bool // no new jobs start; running ones finish
	screen   screen
	lost     bool // the daemon whose queue this showed has gone
	placed   bool // the cursor has been put on the daemon's current job

	jobs     []encode.Job // indexed by job ID
	logs     [][]string   // indexed by job ID
	cursor   int          // selected job
	offset   int          // first job row in view
	hideDone bool         // leave finished jobs out of the list, apart from failed ones

	width, height int
	jobsH, logH   int

	keys   keyMap
	st     styles
	help   help.Model
	spin   spinner.Model
	bar    progress.Model
	log    viewport.Model
	picker filepicker.Model
	form   *huh.Form      // set while the settings screen is open
	draft  *settingsDraft // values the form edits

	follow      bool // keep the log scrolled to the bottom
	logDirty    bool // the selected job's log needs re-rendering
	confirmQuit bool

	status    string
	statusBad bool
	statusID  int
}

// New returns a model that displays and controls q.
func New(q Backend) Model {
	m := Model{
		queue:    q,
		settings: q.Settings(),
		paused:   q.Paused(),
		holding:  q.Holding(),
		keys:     defaultKeys(),
		help:     help.New(),
		spin:     spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		log:      viewport.New(),
		picker:   newPicker(),
		follow:   true,
	}
	m.log.SoftWrap = true
	m.setTheme(true)
	return m
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(tea.RequestBackgroundColor, m.spin.Tick, listen(m.queue))
}

// listen waits for the next batch of queue events.
func listen(q Backend) tea.Cmd {
	return func() tea.Msg {
		if evs := q.Next(); evs != nil {
			return eventsMsg(evs)
		}
		return queueClosedMsg{}
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resizeForm()
	case tea.BackgroundColorMsg:
		m.setTheme(msg.IsDark())
	case eventsMsg:
		m.apply(msg)
		if !m.placed && m.queue.Daemon() {
			m.placeCursor()
		}
		cmds = append(cmds, listen(m.queue))
	case queueClosedMsg:
		m.lost = m.queue.Daemon()
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		cmds = append(cmds, cmd)
		// Another UI, or the settings file, may have changed these.
		m.settings, m.paused, m.holding = m.queue.Settings(), m.queue.Paused(), m.queue.Holding()
		m.setPauseHelp()
	case clearStatusMsg:
		if msg.id == m.statusID {
			m.status = ""
		}
	case tea.KeyPressMsg:
		if key.Matches(msg, m.keys.Kill) {
			return m, tea.Quit
		}
	}

	// The open screen gets every key, plus anything else its components may
	// be waiting on, such as cursor blinks and directory listings.
	switch m.screen {
	case screenQueue:
		if msg, ok := msg.(tea.KeyPressMsg); ok {
			cmds = append(cmds, m.queueKey(msg))
		}
	case screenSettings:
		cmds = append(cmds, m.updateSettings(msg))
	case screenAdd:
		cmds = append(cmds, m.updatePicker(msg))
	}

	m.layout()
	if m.logDirty && m.cursor < len(m.logs) {
		m.log.SetContentLines(m.logs[m.cursor])
		m.logDirty = false
	}
	if m.follow {
		m.log.GotoBottom()
	}
	return m, tea.Batch(cmds...)
}

func (m *Model) queueKey(msg tea.KeyPressMsg) tea.Cmd {
	if key.Matches(msg, m.keys.Quit) {
		if m.confirmQuit || m.count(encode.Running) == 0 || m.queue.Daemon() {
			return tea.Quit
		}
		m.confirmQuit = true
		return nil
	}

	m.confirmQuit = false
	switch {
	case key.Matches(msg, m.keys.Up):
		m.moveCursor(-1)
	case key.Matches(msg, m.keys.Down):
		m.moveCursor(1)
	case key.Matches(msg, m.keys.HideDone):
		m.hideDone = !m.hideDone
		if m.hideDone {
			m.keys.HideDone.SetHelp("h", "show done")
		} else {
			m.keys.HideDone.SetHelp("h", "hide done")
		}
	case key.Matches(msg, m.keys.LogUp):
		m.log.PageUp()
		m.follow = m.log.AtBottom()
	case key.Matches(msg, m.keys.LogDown):
		m.log.PageDown()
		m.follow = m.log.AtBottom()
	case key.Matches(msg, m.keys.Cancel):
		if m.cursor < len(m.jobs) {
			m.queue.Cancel(m.jobs[m.cursor].ID)
		}
	case key.Matches(msg, m.keys.Retry):
		if m.cursor < len(m.jobs) {
			m.queue.Retry(m.jobs[m.cursor].ID)
		}
	case key.Matches(msg, m.keys.Add):
		m.screen = screenAdd
		return m.picker.Init()
	case key.Matches(msg, m.keys.Settings):
		return m.openSettings(newDraft(m.settings))
	case key.Matches(msg, m.keys.Pause):
		m.holding = !m.holding
		m.queue.SetHolding(m.holding)
		m.setPauseHelp()
		switch {
		case !m.holding:
			return m.setStatus("Carrying on", false)
		case m.count(encode.Running) > 0:
			return m.setStatus("Pausing once the running jobs finish. Press p to carry on.", false)
		}
		return m.setStatus("Paused. Press p to carry on.", false)
	case key.Matches(msg, m.keys.PauseNow):
		m.paused = !m.paused
		m.queue.SetPaused(m.paused)
		m.setPauseHelp()
		if m.paused {
			return m.setStatus("Paused. Press P to carry on where it left off.", false)
		}
		return m.setStatus("Resumed", false)
	case key.Matches(msg, m.keys.Help):
		m.help.ShowAll = !m.help.ShowAll
		if m.help.ShowAll {
			m.keys.Help.SetHelp("?", "less")
		} else {
			m.keys.Help.SetHelp("?", "more")
		}
	}
	return nil
}

// apply folds queue events into the model. The queue numbers jobs from zero
// and announces each one before any later job, so IDs double as indexes.
func (m *Model) apply(evs []encode.Event) {
	for _, ev := range evs {
		id := ev.Job.ID
		if id == len(m.jobs) {
			m.jobs = append(m.jobs, ev.Job)
			m.logs = append(m.logs, nil)
		} else {
			if m.jobs[id].State.Finished() && ev.Job.State == encode.Queued {
				m.logs[id] = nil // retried, so start a fresh log
				m.logDirty = m.logDirty || id == m.cursor
			}
			if m.jobs[id].State != encode.Failed && ev.Job.State == encode.Failed {
				m.appendLog(id, m.st.badText.Render("✗ "+ev.Job.Err.Error()))
			}
			m.jobs[id] = ev.Job
		}
		if ev.Line != "" {
			m.appendLog(id, m.logLine(ev.Line))
		}
	}
}

// placeCursor selects the job a daemon is working on, or the last one it
// finished, rather than the first of a long list.
func (m *Model) placeCursor() {
	m.placed = true
	last := -1
	for _, i := range m.order() {
		switch j := m.jobs[i]; {
		case j.State == encode.Running:
			m.selectJob(i)
			return
		case j.State.Finished():
			last = i
		}
	}
	m.selectJob(last)
}

// logLine styles a line of a job's log so that cav1ar's own lines stand out
// from ffmpeg's output, and the start of each stage most of all.
func (m *Model) logLine(line string) string {
	line = strings.ReplaceAll(line, "\t", "    ")
	if stage, ok := encode.Stage(line); ok {
		return m.st.paneTitle.Render("▸ " + stage)
	}
	if msg, ok := encode.Own(line); ok {
		if cmd, ok := strings.CutPrefix(msg, "ffmpeg "); ok {
			return m.st.dim.Render("$ ffmpeg " + cmd)
		}
		return msg
	}
	return m.st.dim.Render(line)
}

func (m *Model) appendLog(id int, line string) {
	lines := append(m.logs[id], line)
	if len(lines) > maxLogLines {
		lines = lines[len(lines)-maxLogLines:]
	}
	m.logs[id] = lines
	m.logDirty = m.logDirty || id == m.cursor
}

func (m *Model) selectJob(i int) {
	i = min(max(i, 0), max(len(m.jobs)-1, 0))
	if i == m.cursor {
		return
	}
	m.cursor = i
	m.follow = true
	m.logDirty = true
}

// setPauseHelp words the pause keys for what pressing them would do now.
func (m *Model) setPauseHelp() {
	if m.holding {
		m.keys.Pause.SetHelp("p", "carry on")
	} else {
		m.keys.Pause.SetHelp("p", "pause after these")
	}
	if m.paused {
		m.keys.PauseNow.SetHelp("P", "resume")
	} else {
		m.keys.PauseNow.SetHelp("P", "pause now")
	}
}

// rows lists the jobs the list shows, by index, in the order they run: all
// of them, or when finished ones are hidden, those still to do and those that
// failed.
func (m Model) rows() []int {
	return slices.DeleteFunc(m.order(), func(i int) bool { return !m.shows(m.jobs[i]) })
}

// shows reports whether the list shows j.
func (m Model) shows(j encode.Job) bool {
	return !m.hideDone || j.State != encode.Done && j.State != encode.Canceled
}

// order lists every job by index in the order they run, rather than the
// order they were added, since up next jobs jump the line: those that have
// started, by when they started, then those up next, then the rest.
func (m Model) order() []int {
	order := make([]int, len(m.jobs))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		ja, jb := m.jobs[a], m.jobs[b]
		if pa, pb := place(ja), place(jb); pa == 0 || pb == 0 {
			return cmp.Or(cmp.Compare(pa, pb), ja.Started.Compare(jb.Started))
		}
		return encode.RunOrder(ja, jb) // both waiting
	})
	return order
}

// place is which part of the line j is in: 0 once it has started, 1 while
// it waits up next, and 2 while it waits behind those. Waiting jobs are put
// in the order encode.RunOrder gives, which also weighs priority.
func place(j encode.Job) int {
	switch {
	case !j.Started.IsZero():
		return 0
	case j.Next:
		return 1
	}
	return 2
}

// moveCursor selects the job n rows below the selected one, or above it
// when n is negative.
func (m *Model) moveCursor(n int) {
	rows := m.rows()
	if len(rows) == 0 {
		return
	}
	at := slices.Index(rows, m.cursor)
	m.selectJob(rows[min(max(at+n, 0), len(rows)-1)])
}

// setStatus shows a message in the footer for a few seconds.
func (m *Model) setStatus(s string, bad bool) tea.Cmd {
	m.statusID++
	m.status, m.statusBad = s, bad
	id := m.statusID
	return tea.Tick(4*time.Second, func(time.Time) tea.Msg { return clearStatusMsg{id} })
}

// bodyHeight is the height left for boxes between the header and footer.
func (m Model) bodyHeight() int {
	return m.height - 2 - lipgloss.Height(m.footer()) // header and gap
}

// layout sizes every component to the current window.
func (m *Model) layout() {
	m.help.SetWidth(m.width - 2)
	avail := m.bodyHeight()

	shown := m.rows()
	m.jobsH = max(min(len(shown)+2, avail/2), 3)
	m.logH = max(avail-m.jobsH, 3)
	m.log.SetWidth(m.width - 4)
	m.log.SetHeight(m.logH - 2)
	m.bar.SetWidth(columnsFor(m.width - 4).bar)
	m.picker.SetHeight(max(avail-2, 1))

	// A hidden job can't stay selected, so select the next one shown, or the
	// last when none follows it.
	at := slices.Index(shown, m.cursor)
	if at < 0 && len(shown) > 0 {
		at = len(shown) - 1
		all := m.order()
		for _, i := range all[slices.Index(all, m.cursor)+1:] {
			if m.shows(m.jobs[i]) {
				at = slices.Index(shown, i)
				break
			}
		}
		m.selectJob(shown[at])
	}
	rows := m.jobsH - 2
	if at < m.offset {
		m.offset = at
	}
	if at >= m.offset+rows {
		m.offset = at - rows + 1
	}
	m.offset = min(max(m.offset, 0), max(len(shown)-rows, 0))
}

func (m *Model) setTheme(isDark bool) {
	m.st = newStyles(isDark)
	m.help.Styles = help.DefaultStyles(isDark)
	m.spin.Style = lipgloss.NewStyle().Foreground(m.st.accent)
	m.bar = m.st.progressBar(m.bar.Width())
	m.picker.Styles = m.st.pickerStyles()
}

func (m Model) count(s encode.State) int {
	n := 0
	for _, j := range m.jobs {
		if j.State == s {
			n++
		}
	}
	return n
}
