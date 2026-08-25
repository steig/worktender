package fleet

import (
	"strings"
	"testing"
)

func modelLines() []Line {
	return []Line{
		{Text: "fleet · header"},
		{Text: "escalations"},
		{Text: "row a", Target: &Target{Label: "a", Root: "/r", Branch: "a"}},
		{Text: "/home/x/proj"},
		{Text: "row b", Target: &Target{Label: "b", Root: "/r", Branch: "b"}},
		{Text: "row c", Target: &Target{Label: "c", Root: "/r", Branch: "c"}},
		{Text: "trailing note"},
	}
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
	m := NewModel([]Line{{Text: "fleet"}, {Text: "no repositories"}})
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
		{Text: "fleet · header"},
		{Text: "row new", Target: &Target{Label: "new", Root: "/r", Branch: "new"}},
		{Text: "row b", Target: &Target{Label: "b", Root: "/r", Branch: "b"}},
	}
	m.Refresh(fresh)
	if got := m.Current(); got == nil || got.Label != "b" {
		t.Errorf("after refresh the cursor is on %v, want still b", got)
	}

	// The row vanished: fall back to the top, which is where escalations are.
	m.Refresh([]Line{
		{Text: "fleet · header"},
		{Text: "row z", Target: &Target{Label: "z", Root: "/r", Branch: "z"}},
	})
	if got := m.Current(); got == nil || got.Label != "z" {
		t.Errorf("after its row vanished the cursor is on %v, want the first row", got)
	}
}

func TestFrameInvertsTheCursorRowAndClips(t *testing.T) {
	m := NewModel(modelLines())
	frame := m.Frame(5, 10, "")

	lines := strings.Split(frame, "\r\n")
	var selected string
	for _, line := range lines {
		if strings.Contains(line, invertOn) {
			selected = line
		}
	}
	if !strings.Contains(selected, "row a") {
		t.Errorf("the inverted line is %q, want the cursor row", selected)
	}
	for _, line := range lines {
		plain := strings.ReplaceAll(strings.ReplaceAll(line, invertOn, ""), sgrReset, "")
		if n := len([]rune(plain)); n > 5 {
			t.Errorf("line %q is %d cells wide, over the 5-cell terminal", plain, n)
		}
	}
	if !strings.Contains(lines[len(lines)-1], "…") {
		t.Errorf("the key line did not clip: %q", lines[len(lines)-1])
	}
}

// Tones ride under the cursor's reverse video and both end at the reset, so
// a colored line never bleeds its color into the one below.
func TestFramePaintsLineTones(t *testing.T) {
	m := NewModel([]Line{
		{Text: "ESCALATIONS", Tone: ToneAlert},
		{Text: "row a", Tone: ToneBad, Target: &Target{Label: "a"}},
	})
	frame := m.Frame(80, 10, "")
	if !strings.Contains(frame, ToneAlert+"ESCALATIONS"+sgrReset) {
		t.Errorf("the heading's tone is not painted:\n%q", frame)
	}
	if !strings.Contains(frame, ToneBad+invertOn+"row a"+sgrReset) {
		t.Errorf("the selected row lost its tone or inversion:\n%q", frame)
	}
}

// The footer is one slim line; the full key list lives behind `?`.
func TestFrameHelpOverlayCarriesTheFullKeyList(t *testing.T) {
	m := NewModel(modelLines())
	frame := m.Frame(80, 24, "")
	if !strings.Contains(frame, footer) {
		t.Errorf("the frame lost its footer:\n%q", frame)
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

	frame := m.Frame(80, 4, "") // 3 body lines + key line
	if !strings.Contains(frame, "row c") {
		t.Errorf("the cursor row scrolled out of a 3-line viewport:\n%q", frame)
	}
	if strings.Contains(frame, "fleet · header") {
		t.Errorf("the top of the board is still drawn in a viewport too short for it:\n%q", frame)
	}
}
