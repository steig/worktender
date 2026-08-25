package fleet

import (
	"strings"
	"testing"
)

func row(text, name string) Line {
	return Line{Spans: []Span{{Text: text}}, Target: &Target{Label: name, Root: "/r", Branch: name}}
}

func modelLines() []Line {
	return []Line{
		textLine(styleBar, " FLEET  header "),
		band("ESCALATIONS"),
		row("row a", "a"),
		textLine(styleDim, "/home/x/proj"),
		row("row b", "b"),
		row("row c", "c"),
		textLine(styleDim, "trailing note"),
	}
}

// stripped is a frame line without its dressing: SGR sequences and the
// per-line erase dropped, the visible text kept.
func stripped(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case r == 0x1b:
			inEsc = true
		case inEsc:
			// SGR and erase sequences end at their letter.
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func TestModelMovesOverSelectableRowsOnly(t *testing.T) {
	m := NewModel(modelLines())
	if got := m.Current(); got == nil || got.Label != "a" {
		t.Fatalf("a fresh model selects %v, want the first row — the most urgent thing on the board", got)
	}

	m.Move(1)
	if m.Current().Label != "b" {
		t.Errorf("moved to %q, want b — the heading between them is not a stop", m.Current().Label)
	}
	m.Move(5)
	if m.Current().Label != "c" {
		t.Errorf("moving past the end lands on %q, want the last row", m.Current().Label)
	}
	m.Move(-10)
	if m.Current().Label != "a" {
		t.Errorf("moving past the top lands on %q, want the first row", m.Current().Label)
	}
	m.End()
	if m.Current().Label != "c" {
		t.Errorf("End lands on %q", m.Current().Label)
	}
	m.Home()
	if m.Current().Label != "a" {
		t.Errorf("Home lands on %q", m.Current().Label)
	}
}

func TestModelWithNoSelectableRowsHasNoCurrent(t *testing.T) {
	m := NewModel([]Line{textLine(Style{}, "fleet"), placeholder("nothing in flight")})
	if m.Current() != nil {
		t.Errorf("selected %v on a board with nothing to select", m.Current())
	}
	m.Move(1) // must not panic or invent a selection
	if m.Current() != nil {
		t.Errorf("moving selected %v", m.Current())
	}
}

// A refresh is exactly the moment rows appear, vanish and move, so the cursor
// follows the row's identity, not its index.
func TestRefreshKeepsTheCursorOnItsRow(t *testing.T) {
	m := NewModel(modelLines())
	m.Move(1) // on b

	fresh := []Line{
		textLine(styleBar, " FLEET  header "),
		row("row new", "new"),
		row("row b", "b"),
	}
	m.Refresh(fresh)
	if got := m.Current(); got == nil || got.Label != "b" {
		t.Errorf("after refresh the cursor is on %v, want still b", got)
	}

	// The row vanished: fall back to the top, which is where escalations are.
	m.Refresh([]Line{
		textLine(styleBar, " FLEET  header "),
		row("row z", "z"),
	})
	if got := m.Current(); got == nil || got.Label != "z" {
		t.Errorf("after its row vanished the cursor is on %v, want the first row", got)
	}
}

// The selected row is a full-width accent-background highlight, not a caret:
// the row's spans take the accent background and the rest of the width is
// painted to match.
func TestFrameHighlightsTheCursorRowFullWidth(t *testing.T) {
	m := NewModel(modelLines())
	frame := m.Frame(20, 10, "")

	selected := Style{FG: ColorOnAccent, BG: ColorAccent}
	if !strings.Contains(frame, selected.sgr()+"row a") {
		t.Errorf("the cursor row is not painted onto the accent background:\n%q", frame)
	}
	fill := Style{BG: ColorAccent}
	if !strings.Contains(frame, fill.sgr()+strings.Repeat(" ", 20-len("row a"))) {
		t.Errorf("the highlight does not reach the full width:\n%q", frame)
	}
}

func TestFrameClipsToTheTerminalWidth(t *testing.T) {
	m := NewModel(modelLines())
	frame := m.Frame(5, 10, "")

	for _, line := range strings.Split(frame, "\r\n") {
		if n := len([]rune(stripped(line))); n > 5 {
			t.Errorf("line %q is %d cells wide, over the 5-cell terminal", stripped(line), n)
		}
	}
	lines := strings.Split(frame, "\r\n")
	if !strings.Contains(lines[len(lines)-1], "…") {
		t.Errorf("the footer did not clip: %q", lines[len(lines)-1])
	}
}

// Styles paint spans, and only spans: the dressing must end at the reset so
// a styled cell never bleeds into its neighbor.
func TestFramePaintsSpanStyles(t *testing.T) {
	m := NewModel([]Line{
		band("ESCALATIONS"),
		{Spans: []Span{{Text: "  !", Style: styleBad}, {Text: "  reason"}}, Target: &Target{Label: "x"}},
	})
	m.Sel = -1 // no highlight in the way of the assertion
	frame := m.Frame(80, 10, "")
	if !strings.Contains(frame, styleBand.sgr()+" ESCALATIONS "+sgrReset) {
		t.Errorf("the band style is not painted:\n%q", frame)
	}
	if !strings.Contains(frame, styleBad.sgr()+"  !"+sgrReset+"  reason") {
		t.Errorf("the glyph's color leaks past its span:\n%q", frame)
	}
}

// The working glyph animates in watch mode: each frame tick swaps it for the
// next spinner frame, and Spinning tells the loop whether ticking is worth it.
func TestFrameSpinsTheWorkingGlyph(t *testing.T) {
	lines := []Line{
		{Spans: []Span{{Text: "  " + glyphWorking, Style: styleGood, Spin: true}, {Text: "  42-fix"}},
			Target: &Target{Label: "42-fix"}},
	}
	m := NewModel(lines)
	if !m.Spinning() {
		t.Fatal("a board with a working row does not report Spinning")
	}

	first := m.Frame(80, 10, "")
	m.Spin++
	second := m.Frame(80, 10, "")
	if strings.Contains(first, glyphWorking) || strings.Contains(second, glyphWorking) {
		t.Error("the static working glyph rendered in watch mode; the spinner should replace it")
	}
	if !strings.Contains(first, spinnerFrames[0]) || !strings.Contains(second, spinnerFrames[1]) {
		t.Errorf("the spinner does not advance:\n%q\n%q", first, second)
	}

	still := NewModel(modelLines())
	if still.Spinning() {
		t.Error("a board with nothing working reports Spinning")
	}
}

// The footer is one faint line; the full key list lives behind `?`.
func TestFrameHelpOverlayCarriesTheFullKeyList(t *testing.T) {
	m := NewModel(modelLines())
	frame := m.Frame(80, 24, "")
	if !strings.Contains(frame, styleDim.sgr()+footer) {
		t.Errorf("the frame lost its faint footer:\n%q", frame)
	}
	if strings.Contains(frame, "focus the worker's pane") {
		t.Errorf("the full key list is on the board rather than behind ?:\n%q", frame)
	}

	m.Help = true
	overlay := m.Frame(80, 24, "")
	for _, want := range []string{
		"focus the worker's pane",
		"open the row's pull request",
		"any key closes help",
	} {
		if !strings.Contains(overlay, want) {
			t.Errorf("the help overlay is missing %q:\n%q", want, overlay)
		}
	}
	if strings.Contains(overlay, "row a") {
		t.Errorf("the board is drawn under the help overlay:\n%q", overlay)
	}
}

// The viewport follows the cursor: a board taller than the terminal scrolls
// rather than pinning the cursor off-screen.
func TestFrameScrollsToKeepTheCursorVisible(t *testing.T) {
	m := NewModel(modelLines())
	m.End() // row c, line index 5

	frame := m.Frame(80, 4, "") // 3 body lines + footer
	if !strings.Contains(frame, "row c") {
		t.Errorf("the cursor row scrolled out of a 3-line viewport:\n%q", frame)
	}
	if strings.Contains(frame, "FLEET  header") {
		t.Errorf("the top of the board is still drawn in a viewport too short for it:\n%q", frame)
	}
}
