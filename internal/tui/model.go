// Package tui is the interactive front end for an encode.Queue.
package tui

import (
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

// clearStatusMsg hides the status line, unless a newer status replaced it.
type clearStatusMsg struct{ id int }

// Model is the root Bubble Tea model.
type Model struct {
	queue    *encode.Queue
	watcher  *encode.Watcher
	settings encode.Settings
	paused   bool
	screen   screen

	jobs   []encode.Job // indexed by job ID
	logs   [][]string   // indexed by job ID
	cursor int          // selected job
	offset int          // first job row in view

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

// New returns a model that displays and controls q, whose folders w watches.
func New(q *encode.Queue, w *encode.Watcher) Model {
	m := Model{
		queue:    q,
		watcher:  w,
		settings: q.Settings(),
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
func listen(q *encode.Queue) tea.Cmd {
	return func() tea.Msg {
		if evs := q.Next(); evs != nil {
			return eventsMsg(evs)
		}
		return nil
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
		cmds = append(cmds, listen(m.queue))
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		cmds = append(cmds, cmd)
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
		if m.confirmQuit || m.count(encode.Running) == 0 {
			return tea.Quit
		}
		m.confirmQuit = true
		return nil
	}

	m.confirmQuit = false
	switch {
	case key.Matches(msg, m.keys.Up):
		m.selectJob(m.cursor - 1)
	case key.Matches(msg, m.keys.Down):
		m.selectJob(m.cursor + 1)
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
		m.paused = !m.paused
		m.queue.SetPaused(m.paused)
		if m.paused {
			m.keys.Pause.SetHelp("p", "resume")
			return m.setStatus("Paused. Press p to carry on where it left off.", false)
		}
		m.keys.Pause.SetHelp("p", "pause")
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
			m.appendLog(id, strings.ReplaceAll(ev.Line, "\t", "    "))
		}
	}
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

	m.jobsH = max(min(len(m.jobs)+2, avail/2), 3)
	m.logH = max(avail-m.jobsH, 3)
	m.log.SetWidth(m.width - 4)
	m.log.SetHeight(m.logH - 2)
	m.bar.SetWidth(columnsFor(m.width - 4).bar)
	m.picker.SetHeight(max(avail-2, 1))

	rows := m.jobsH - 2
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+rows {
		m.offset = m.cursor - rows + 1
	}
	m.offset = min(max(m.offset, 0), max(len(m.jobs)-rows, 0))
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
