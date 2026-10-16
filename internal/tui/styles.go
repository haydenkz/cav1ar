package tui

import (
	"image/color"
	"strings"

	"charm.land/bubbles/v2/filepicker"
	"charm.land/bubbles/v2/progress"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type styles struct {
	accent, bad, subtle, border color.Color

	logo      lipgloss.Style
	title     lipgloss.Style
	paneTitle lipgloss.Style
	pane      lipgloss.Style
	paneEdge  lipgloss.Style
	selected  lipgloss.Style
	dim       lipgloss.Style
	okText    lipgloss.Style
	warnText  lipgloss.Style
	badText   lipgloss.Style
}

func newStyles(isDark bool) styles {
	ld := lipgloss.LightDark(isDark)
	s := styles{
		accent: ld(lipgloss.Color("#5A4FCF"), lipgloss.Color("#A594FF")),
		bad:    ld(lipgloss.Color("#CF222E"), lipgloss.Color("#F85149")),
		subtle: ld(lipgloss.Color("#6E7781"), lipgloss.Color("#8B949E")),
		border: ld(lipgloss.Color("#D0D7DE"), lipgloss.Color("#3D444D")),
	}
	ok := ld(lipgloss.Color("#1A7F37"), lipgloss.Color("#3FB950"))
	warn := ld(lipgloss.Color("#9A6700"), lipgloss.Color("#D29922"))
	s.logo = lipgloss.NewStyle().Bold(true).Padding(0, 1).
		Foreground(lipgloss.Color("#FFFFFF")).Background(s.accent)
	s.title = lipgloss.NewStyle().Bold(true)
	s.paneTitle = lipgloss.NewStyle().Bold(true).Foreground(s.accent)
	s.paneEdge = lipgloss.NewStyle().Foreground(s.border)
	s.pane = lipgloss.NewStyle().Padding(0, 1).
		Border(lipgloss.RoundedBorder(), false, true, true, true).
		BorderForeground(s.border)
	s.selected = lipgloss.NewStyle().Bold(true).Foreground(s.accent)
	s.dim = lipgloss.NewStyle().Foreground(s.subtle)
	s.okText = lipgloss.NewStyle().Foreground(ok)
	s.warnText = lipgloss.NewStyle().Foreground(warn)
	s.badText = lipgloss.NewStyle().Foreground(s.bad)
	return s
}

func (s styles) progressBar(width int) progress.Model {
	bar := progress.New(
		progress.WithoutPercentage(),
		progress.WithColors(s.accent, lipgloss.Color("#43BFAE")),
		progress.WithWidth(width),
	)
	bar.EmptyColor = s.border
	return bar
}

// formTheme styles the settings form to match the rest of the app.
func (s styles) formTheme() huh.Theme {
	return huh.ThemeFunc(func(bool) *huh.Styles {
		t := huh.ThemeBase(true)
		accent := lipgloss.NewStyle().Foreground(s.accent)

		f := &t.Focused
		f.Base = f.Base.BorderForeground(s.accent)
		f.Card = f.Base
		f.Title = s.paneTitle
		f.Description = s.dim
		f.ErrorIndicator = f.ErrorIndicator.Foreground(s.bad)
		f.ErrorMessage = f.ErrorMessage.Foreground(s.bad)
		f.SelectSelector = f.SelectSelector.Foreground(s.accent)
		f.NextIndicator = f.NextIndicator.Foreground(s.accent)
		f.PrevIndicator = f.PrevIndicator.Foreground(s.accent)
		f.Option = lipgloss.NewStyle()
		f.TextInput.Cursor = accent
		f.TextInput.Prompt = accent
		f.TextInput.Placeholder = s.dim

		t.Blurred = t.Focused
		t.Blurred.Base = f.Base.BorderStyle(lipgloss.HiddenBorder())
		t.Blurred.Card = t.Blurred.Base
		t.Blurred.Title = s.title
		t.Blurred.NextIndicator = lipgloss.NewStyle()
		t.Blurred.PrevIndicator = lipgloss.NewStyle()
		t.Blurred.TextInput.Prompt = s.dim

		t.Group.Title = f.Title
		t.Group.Description = f.Description
		return t
	})
}

func (s styles) pickerStyles() filepicker.Styles {
	p := filepicker.DefaultStyles()
	p.Cursor = lipgloss.NewStyle().Foreground(s.accent)
	p.Selected = s.selected
	p.Directory = lipgloss.NewStyle().Foreground(s.accent)
	p.DisabledFile = s.dim
	p.DisabledCursor = s.dim
	p.DisabledSelected = s.dim
	p.FileSize = p.FileSize.Foreground(s.subtle)
	p.EmptyDirectory = s.dim.PaddingLeft(2).SetString("No files here.")
	return p
}

// box draws a rounded box of exactly w×h cells with title set into the top
// border.
func (s styles) box(title, body string, w, h int) string {
	b := lipgloss.RoundedBorder()
	title = ansi.Truncate(" "+title+" ", max(w-4, 0), "…")
	fill := max(w-3-lipgloss.Width(title), 0)
	top := s.paneEdge.Render(b.TopLeft+b.Top) +
		s.paneTitle.Render(title) +
		s.paneEdge.Render(strings.Repeat(b.Top, fill)+b.TopRight)
	return lipgloss.JoinVertical(lipgloss.Left, top,
		s.pane.Width(w).Height(h-1).MaxHeight(h-1).Render(body))
}

// cellLeft fits s into exactly w cells like cell, but drops the start rather
// than the end, keeping the end of a path, which names it.
func cellLeft(s string, w int) string {
	if over := lipgloss.Width(s) - w; over > 0 {
		s = "…" + ansi.TruncateLeft(s, over+1, "")
	}
	return cell(s, w)
}

// cell fits s into exactly w cells, truncating with an ellipsis or padding.
func cell(s string, w int) string {
	s = ansi.Truncate(s, w, "…")
	return s + strings.Repeat(" ", max(w-lipgloss.Width(s), 0))
}
