package fleet

import (
	"strconv"
	"strings"
)

// The board's styling layer. worktender is dependency-free by charter, so
// this is the lipgloss-shaped corner the board actually needs and nothing
// more: palette styles, padded and ellipsis-truncated cells, and spans that
// keep text and dressing apart until a frame paints them. Layout is never
// done with raw escape strings — everything visible goes through a Style.
//
// Every color is one of the terminal's own 16 palette slots, never an RGB
// value: a light theme and a dark theme each define that palette to be
// legible against their own background, which is what makes the board
// adaptive without querying the terminal for its colors.

// Color is an ANSI palette foreground code; the zero value is the terminal's
// default foreground.
type Color int

const (
	ColorNone Color = 0
	ColorBad  Color = 31 // red: failed, escalated
	ColorGood Color = 32 // green: working, verified
	ColorWarn Color = 33 // yellow: blocked, ghosts, warnings
	// ColorAccent is the board's one accent — section bands, the top bar, the
	// selected row, key names. One accent, so nothing competes with the
	// status glyphs for attention.
	ColorAccent Color = 34
	// ColorOnAccent is the text color on accent-background regions: bright
	// white reads on the palette's blue in both light and dark themes.
	ColorOnAccent Color = 97
)

// Style is one span's dressing. The zero Style paints nothing.
type Style struct {
	FG, BG Color
	Bold   bool
	Faint  bool
}

// The board's design system: a neutral base, one accent, status color only
// on glyphs, faint for the secondary text the eye should skip.
var (
	styleBand   = Style{FG: ColorOnAccent, BG: ColorAccent, Bold: true}
	styleBar    = Style{FG: ColorOnAccent, BG: ColorAccent}
	styleDim    = Style{Faint: true}
	styleGood   = Style{FG: ColorGood}
	styleWarn   = Style{FG: ColorWarn}
	styleBad    = Style{FG: ColorBad}
	styleKey    = Style{FG: ColorAccent, Bold: true}
	styleBorder = Style{Faint: true}
)

// sgr is the escape sequence that turns the style on, empty for the zero
// style so unstyled text stays byte-identical to its input.
func (s Style) sgr() string {
	if s == (Style{}) {
		return ""
	}
	codes := make([]string, 0, 4)
	if s.Bold {
		codes = append(codes, "1")
	}
	if s.Faint {
		codes = append(codes, "2")
	}
	if s.FG != ColorNone {
		codes = append(codes, strconv.Itoa(int(s.FG)))
	}
	if s.BG != ColorNone {
		codes = append(codes, strconv.Itoa(int(s.BG)+10))
	}
	return "\x1b[" + strings.Join(codes, ";") + "m"
}

const sgrReset = "\x1b[0m"

// paint wraps text in the style and a reset.
func (s Style) paint(text string) string {
	seq := s.sgr()
	if seq == "" {
		return text
	}
	return seq + text + sgrReset
}

// Span is one styled fragment of a line. Text is plain — the style rides
// beside it, so alignment counts visible runes and the one-shot print can
// drop the dressing entirely.
type Span struct {
	Text  string
	Style Style
	// Spin marks the span whose working glyph animates in watch mode; the
	// one-shot print keeps the static glyph.
	Spin bool
}

// spinnerFrames animate the working glyph, one step per frame tick.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// truncate cuts one cell to w cells, ellipsis last, whole runes only.
func truncate(s string, w int) string {
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w <= 1 {
		return "…"
	}
	return string(r[:w-1]) + "…"
}

// pad right-pads to w cells; text already wider is returned as it is.
func pad(s string, w int) string {
	if n := len([]rune(s)); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

// spanWidth is the visible width of a run of spans — text runes only, since
// styles live beside the text rather than in it.
func spanWidth(spans []Span) int {
	n := 0
	for _, s := range spans {
		n += len([]rune(s.Text))
	}
	return n
}

// clipSpans truncates a run of spans to a width, whole runes only, ellipsis
// last — the same contract a single cell's truncation keeps.
func clipSpans(spans []Span, width int) []Span {
	if width <= 0 || spanWidth(spans) <= width {
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
