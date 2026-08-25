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
	// Help draws the key overlay instead of the board. The footer stays slim
	// because this is where the full list lives.
	Help bool
	// Spin is the spinner's frame counter, advanced by the watch loop's frame
	// tick while Spinning reports there is something to animate.
	Spin int
	// spinning is whether any line carries an animated glyph, computed when
	// the lines change so the frame tick can ask cheaply.
	spinning bool
	// top is the first line the viewport shows, kept so scrolling follows the
	// cursor rather than snapping.
	top int
}

// NewModel selects the first selectable line, which by construction is the
// most urgent thing on the board: escalations render before everything else.
func NewModel(lines []Line) Model {
	m := Model{Lines: lines, Sel: -1, spinning: anySpin(lines)}
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
	m.spinning = anySpin(lines)
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

// Spinning reports whether any row's glyph animates, so the watch loop can
// skip frame ticks over a still fleet.
func (m *Model) Spinning() bool { return m.spinning }

func anySpin(lines []Line) bool {
	for _, line := range lines {
		for _, span := range line.Spans {
			if span.Spin {
				return true
			}
		}
	}
	return false
}

// footer is the slim one-line default at the bottom of every frame, drawn
// faint. The full key list lives behind `?` — a cockpit's floor line, not
// its manual.
const footer = "j/k move · enter focus · o pr · ? keys · q quit"

// helpBoard is the `?` overlay: every key the board answers, in one place.
func helpBoard() []Line {
	key := func(keys, what string) Line {
		return Line{Spans: []Span{
			{Text: "  "},
			{Text: pad(keys, 16), Style: styleKey},
			{Text: what},
		}}
	}
	return []Line{
		band("KEYS"),
		{},
		key("j / k, ↓ / ↑", "move the cursor"),
		key("g / G", "first / last row"),
		key("enter, f", "focus the worker's pane"),
		key("o", "open the row's pull request"),
		key("r", "refresh now"),
		key("?", "toggle this help"),
		key("q, ctrl-c", "quit"),
		{},
		textLine(styleDim, "  read-only plus navigation: nothing on the board changes state"),
	}
}

// Frame renders the viewport: width×height cells of the board with each
// span's style painted, the cursor row highlighted across the full width,
// and a faint status/footer line at the bottom. It returns the text only —
// screen positioning and cursor hiding belong to the terminal owner, not
// here. With Help set it draws the key overlay instead of the board.
//
// Lines are joined with \r\n because the watch terminal is in raw mode,
// where a bare \n moves down without returning; each line ends with an
// erase-to-end so a redraw needs no full-screen clear, which is what keeps
// the spinner's frame ticks from flickering.
func (m *Model) Frame(width, height int, status string) string {
	if height < 2 {
		height = 2
	}
	body := height - 1

	lines := m.Lines
	sel := m.Sel
	if m.Help {
		lines, sel = helpBoard(), -1
		m.top = 0
	} else {
		m.scrollTo(body)
	}

	var sb strings.Builder
	for i := m.top; i < m.top+body; i++ {
		if i > m.top {
			sb.WriteString("\x1b[K\r\n")
		}
		if i >= len(lines) {
			continue
		}
		sb.WriteString(renderLine(lines[i], width, i == sel, m.Spin))
	}
	sb.WriteString("\x1b[K\r\n")
	if m.Help {
		status = "any key closes help"
	} else if status == "" {
		status = footer
	}
	sb.WriteString(renderLine(textLine(styleDim, status), width, false, 0))
	sb.WriteString("\x1b[K")
	return sb.String()
}

// renderLine paints one line: spans clipped to the width, the working glyph
// swapped for its spinner frame, and — on the selected row — the accent
// background carried across the full width, the highlight being the cursor
// rather than any caret.
func renderLine(l Line, width int, selected bool, spin int) string {
	spans := clipSpans(l.Spans, width)
	var sb strings.Builder
	used := 0
	for _, s := range spans {
		text := s.Text
		if s.Spin {
			text = strings.Replace(text, glyphWorking, spinnerFrames[spin%len(spinnerFrames)], 1)
		}
		used += len([]rune(text))
		style := s.Style
		if selected {
			// Faint on the accent background is unreadable, and the default
			// foreground may be the background's own color: the highlight
			// promotes both to the on-accent text color.
			style.BG = ColorAccent
			style.Faint = false
			if style.FG == ColorNone {
				style.FG = ColorOnAccent
			}
		}
		sb.WriteString(style.paint(text))
	}
	if selected && width > used {
		sb.WriteString(Style{BG: ColorAccent}.paint(strings.Repeat(" ", width-used)))
	}
	return sb.String()
}

// clipSpans truncates a line of spans to the terminal's width, whole runes
// only, ellipsis last — the same contract a single cell's truncation keeps.
func clipSpans(spans []Span, width int) []Span {
	if width <= 0 {
		return spans
	}
	total := 0
	for _, s := range spans {
		total += len([]rune(s.Text))
	}
	if total <= width {
		return spans
	}

	out := make([]Span, 0, len(spans))
	budget := width - 1
	for _, s := range spans {
		r := []rune(s.Text)
		if len(r) < budget {
			out = append(out, s)
			budget -= len(r)
			continue
		}
		out = append(out, Span{Text: string(r[:budget]) + "…", Style: s.Style, Spin: s.Spin})
		break
	}
	return out
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
