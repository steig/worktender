package fleet

import (
	"strings"
)

// Model is the watch view's state between redraws: the rendered view and
// where the cursor is. It is pure — no terminal, no clock — so the key
// handling the TUI depends on is testable without one.
type Model struct {
	View View
	// Sel indexes View.Hots; -1 when the board has no selectable row.
	Sel int
	// Help draws the key overlay instead of the board. The footer stays slim
	// because this is where the full list lives.
	Help bool
	// Spin is the spinner's frame counter, advanced by the watch loop's frame
	// tick while Spinning reports there is something to animate.
	Spin int
	// spinning is whether any line carries an animated glyph, computed when
	// the view changes so the frame tick can ask cheaply.
	spinning bool
	// top is the first line the viewport shows, kept so scrolling follows the
	// cursor rather than snapping.
	top int
}

// NewModel selects the first selectable row. Hotspots are ordered by
// urgency — escalations first — so the fresh cursor lands on the most urgent
// thing on the board.
func NewModel(v View) Model {
	m := Model{View: v, Sel: -1, spinning: anySpin(v.Lines)}
	if len(v.Hots) > 0 {
		m.Sel = 0
	}
	return m
}

// Current is the selected row's target, nil when there is none.
func (m *Model) Current() *Target {
	if m.Sel < 0 || m.Sel >= len(m.View.Hots) {
		return nil
	}
	return m.View.Hots[m.Sel].Target
}

// Move advances the cursor by delta rows, clamping at the ends. Every
// hotspot is selectable, so movement is arithmetic rather than a search.
func (m *Model) Move(delta int) {
	if len(m.View.Hots) == 0 {
		return
	}
	if m.Sel < 0 {
		m.Sel = 0
		return
	}
	m.Sel += delta
	if m.Sel < 0 {
		m.Sel = 0
	}
	if m.Sel >= len(m.View.Hots) {
		m.Sel = len(m.View.Hots) - 1
	}
}

// Home and End jump to the first and last selectable row.
func (m *Model) Home() {
	if len(m.View.Hots) > 0 {
		m.Sel = 0
	}
}

func (m *Model) End() {
	if n := len(m.View.Hots); n > 0 {
		m.Sel = n - 1
	}
}

// Refresh replaces the view with a fresh render, keeping the cursor on the
// row it was on when that row still exists. Matched by target key rather
// than by index, because a refresh is exactly the moment rows appear, vanish
// and move — an index-stable cursor would silently land on a different
// worktree.
func (m *Model) Refresh(v View) {
	prev := m.Current()
	m.View = v
	m.Sel = -1
	m.spinning = anySpin(v.Lines)
	if prev != nil {
		for i, h := range v.Hots {
			if h.Target.Key() == prev.Key() {
				m.Sel = i
				break
			}
		}
	}
	if m.Sel < 0 && len(v.Hots) > 0 {
		m.Sel = 0
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
		textLine(styleBand, " KEYS "),
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
// span's style painted, the cursor row highlighted across its panel's
// interior, and a faint status/footer line at the bottom. It returns the
// text only — screen positioning and cursor hiding belong to the terminal
// owner, not here. With Help set it draws the key overlay instead of the
// board.
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

	lines := m.View.Lines
	var hi *Hotspot
	if m.Help {
		lines = helpBoard()
		m.top = 0
	} else {
		if m.Sel >= 0 && m.Sel < len(m.View.Hots) {
			hi = &m.View.Hots[m.Sel]
		}
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
		var rowHi *Hotspot
		if hi != nil && hi.Line == i {
			rowHi = hi
		}
		sb.WriteString(renderLine(lines[i], width, rowHi, m.Spin))
	}
	sb.WriteString("\x1b[K\r\n")
	if m.Help {
		status = "any key closes help"
	} else if status == "" {
		status = footer
	}
	sb.WriteString(renderLine(textLine(styleDim, status), width, nil, 0))
	sb.WriteString("\x1b[K")
	return sb.String()
}

// renderLine paints one line: spans clipped to the width, the working glyph
// swapped for its spinner frame, and — when the selected hotspot is on this
// line — the accent background across the hotspot's span range, which is
// the row's panel interior.
func renderLine(l Line, width int, hi *Hotspot, spin int) string {
	spans := clipSpans(l.Spans, width)
	var sb strings.Builder
	for i, s := range spans {
		text := s.Text
		if s.Spin {
			text = strings.Replace(text, glyphWorking, spinnerFrames[spin%len(spinnerFrames)], 1)
		}
		style := s.Style
		if hi != nil && i >= hi.SpanFrom && i < hi.SpanTo {
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
	return sb.String()
}

// scrollTo keeps the cursor's line inside a viewport of the given height.
// With no cursor the board shows its top, which is where the bar and the
// most urgent panel are.
func (m *Model) scrollTo(body int) {
	if m.top > len(m.View.Lines)-1 {
		m.top = max(0, len(m.View.Lines)-body)
	}
	if m.Sel < 0 || m.Sel >= len(m.View.Hots) {
		return
	}
	line := m.View.Hots[m.Sel].Line
	if line < m.top {
		m.top = line
	}
	if line >= m.top+body {
		m.top = line - body + 1
	}
}
