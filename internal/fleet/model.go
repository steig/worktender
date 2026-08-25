package fleet

import (
	"strings"
)

// Model is the watch view's state between redraws: the rendered lines and
// where the cursor is. It is pure — no terminal, no clock — so the key
// handling the TUI depends on is testable without one.
type Model struct {
	Lines []Line
	// Sel indexes Lines; -1 when the board has no selectable row.
	Sel int
	// top is the first line the viewport shows, kept so scrolling follows the
	// cursor rather than snapping.
	top int
}

// NewModel selects the first selectable line, which by construction is the
// most urgent thing on the board: escalations render before everything else.
func NewModel(lines []Line) Model {
	m := Model{Lines: lines, Sel: -1}
	m.Move(1)
	return m
}

// Current is the selected row's target, nil when there is none.
func (m *Model) Current() *Target {
	if m.Sel < 0 || m.Sel >= len(m.Lines) {
		return nil
	}
	return m.Lines[m.Sel].Target
}

// Move advances the cursor by delta selectable rows, clamping at the ends.
func (m *Model) Move(delta int) {
	if delta == 0 {
		return
	}
	step := 1
	if delta < 0 {
		step, delta = -1, -delta
	}
	for ; delta > 0; delta-- {
		next := m.nextFrom(m.Sel, step)
		if next < 0 {
			return
		}
		m.Sel = next
	}
}

// Home and End jump to the first and last selectable row.
func (m *Model) Home() {
	if next := m.nextFrom(-1, 1); next >= 0 {
		m.Sel = next
	}
}

func (m *Model) End() {
	if next := m.nextFrom(len(m.Lines), -1); next >= 0 {
		m.Sel = next
	}
}

// nextFrom is the nearest selectable line beyond from in the given direction,
// -1 when there is none.
func (m *Model) nextFrom(from, step int) int {
	for i := from + step; i >= 0 && i < len(m.Lines); i += step {
		if m.Lines[i].Target != nil {
			return i
		}
	}
	return -1
}

// Refresh replaces the lines with a fresh render, keeping the cursor on the
// row it was on when that row still exists. Matched by target key rather than
// by index, because a refresh is exactly the moment rows appear, vanish and
// move — an index-stable cursor would silently land on a different worktree.
func (m *Model) Refresh(lines []Line) {
	prev := m.Current()
	m.Lines = lines
	m.Sel = -1
	if prev != nil {
		for i, line := range lines {
			if line.Target != nil && line.Target.Key() == prev.Key() {
				m.Sel = i
				break
			}
		}
	}
	if m.Sel < 0 {
		m.Move(1)
	}
}

// selected is the ANSI dressing on the cursor row. Reverse video rather than
// color, because the board runs inside whatever theme the user's terminal
// already has and reverse is the one emphasis every theme renders.
const (
	invertOn  = "\x1b[7m"
	invertOff = "\x1b[27m"
)

// Frame renders the viewport: width×height cells of the board with the cursor
// row inverted and a status/key line at the bottom. It returns the text only —
// screen clearing and cursor hiding belong to the terminal owner, not here.
//
// Lines are joined with \r\n because the watch terminal is in raw mode, where
// a bare \n moves down without returning.
func (m *Model) Frame(width, height int, status string) string {
	if height < 2 {
		height = 2
	}
	body := height - 1

	m.scrollTo(body)
	var sb strings.Builder
	for i := m.top; i < m.top+body; i++ {
		if i > m.top {
			sb.WriteString("\r\n")
		}
		if i >= len(m.Lines) {
			continue
		}
		text := clip(m.Lines[i].Text, width)
		if i == m.Sel {
			text = invertOn + text + invertOff
		}
		sb.WriteString(text)
	}
	sb.WriteString("\r\n")
	if status == "" {
		status = "j/k move · enter focus worker · o open PR · r refresh · q quit"
	}
	sb.WriteString(clip(status, width))
	return sb.String()
}

// scrollTo keeps the cursor inside a viewport of the given height. With no
// cursor the board shows its top, which is where the escalations are.
func (m *Model) scrollTo(body int) {
	if m.top > len(m.Lines)-1 {
		m.top = max(0, len(m.Lines)-body)
	}
	if m.Sel < 0 {
		return
	}
	if m.Sel < m.top {
		m.top = m.Sel
	}
	if m.Sel >= m.top+body {
		m.top = m.Sel - body + 1
	}
}

// clip truncates one line to the terminal's width, by rune so a multi-byte
// character is dropped whole rather than split into replacement glyphs.
func clip(s string, width int) string {
	if width <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return string(runes[:width-1]) + "…"
}
