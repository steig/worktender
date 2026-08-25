package fleet

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/steig/worktender/internal/safetext"
	"github.com/steig/worktender/internal/wt"
)

// Glyphs are the one-rune state column, and the only place status color
// lands: the design is a neutral base with one accent, so a row's state is a
// colored glyph beside plain text, never a painted line. A glyph beside a
// color rather than a color alone, because the one-shot print carries no
// color and the reader may not see red anyway.
const (
	glyphEscalated = "!"
	glyphWorking   = "●"
	glyphVerified  = "✓"
	glyphFailed    = "✗"
	glyphBlocked   = "◌"
	glyphIdle      = "·"
	glyphMain      = "*"
	glyphGhost     = "?"
)

// Line is one rendered board line: styled spans, plus the navigation target.
// Target is nil on headings, notes and blanks; set on the rows a cursor can
// land on.
type Line struct {
	Spans  []Span
	Target *Target
}

// Plain is the line without its dressing — what the one-shot prints, and
// what a pipe receives.
func (l Line) Plain() string {
	var sb strings.Builder
	for _, s := range l.Spans {
		sb.WriteString(s.Text)
	}
	return sb.String()
}

// textLine is a whole line in one style.
func textLine(style Style, text string) Line {
	return Line{Spans: []Span{{Text: text, Style: style}}}
}

// band is a section header: the title as an accent-background chip rather
// than a bare uppercase word. Chips are word-width while the selected row's
// highlight is full-width, so the two accent uses stay distinguishable.
func band(title string) Line {
	return textLine(styleBand, " "+title+" ")
}

// Target is what navigation needs to know about a row: the pane to focus and
// the branch to ask gh about. Every field can be empty — a row is selectable
// for whichever of the two actions its fields support.
type Target struct {
	// Label names the row in status messages, escaped already.
	Label  string
	PaneID string
	Root   string
	Branch string
	// PR is the number the fleet claims, 0 when only the branch is known.
	PR int
}

// Key is what selection survives a refresh by: the same worktree keeps the
// cursor even when every row around it moved.
func (t *Target) Key() string { return t.Root + "\x00" + t.Branch + "\x00" + t.PaneID }

// layout is how many columns the pane has room for. Sections drop their
// right-hand columns rather than wrapping: a narrow pane still shows what
// each row is and how old, and the detail waits for width.
type layout int

const (
	wide   layout = iota // everything
	medium               // drop the report and PR-state detail
	narrow               // state, name, age — the glanceable minimum
)

func layoutFor(width int) layout {
	switch {
	case width <= 0 || width >= 80:
		return wide
	case width >= 50:
		return medium
	default:
		return narrow
	}
}

// cell is one table cell before alignment: its text and its dressing.
type cell struct {
	text  string
	style Style
	spin  bool
}

// rowSpec is one table row before alignment.
type rowSpec struct {
	cells  []cell
	target *Target
}

// Lines renders the whole board as styled rows with their navigation
// targets. One function for the one-shot print and the watch frame, so the
// two cannot disagree about what the fleet looks like. width picks the
// column layout and the top bar's span; zero or less means no pane is asking
// and every column renders unpadded.
//
// The idle fleet is the default state and renders as a full screen, not a
// blank one: the top bar and RECENTLY LANDED are always there, and the empty
// sections say they are empty instead of not appearing.
func Lines(b Board, width int) []Line {
	l := layoutFor(width)
	out := []Line{topBar(b, width)}
	out = append(out, notes(b)...)

	if len(b.Escalations) > 0 {
		out = append(out, Line{}, band("ESCALATIONS"))
		out = append(out, table(rowSpecs(b.Escalations, func(r *TaskRow) rowSpec {
			return escalationRow(b.Now, r, l)
		}))...)
	}

	out = append(out, Line{}, band("IN FLIGHT"))
	out = append(out, inFlight(b, l)...)

	out = append(out, Line{}, band("RECENTLY LANDED"))
	if len(b.Recent) == 0 {
		out = append(out, placeholder("nothing landed in the last 7 days"))
	} else {
		out = append(out, table(rowSpecs(b.Recent, func(r *TaskRow) rowSpec {
			return landedRow(b.Now, r, l)
		}))...)
	}

	if len(b.Peers) > 0 {
		out = append(out, Line{}, band("PEERS"))
		out = append(out, table(rowSpecs(b.Peers, func(r *TaskRow) rowSpec {
			return peerRow(b.Now, r, l)
		}))...)
	}
	return out
}

// placeholder is an empty section saying so: styled to recede, present so
// the section reads as empty rather than broken.
func placeholder(text string) Line {
	return textLine(styleDim, "  "+text)
}

// topBar is the header bar a person reads before anything else, and the one
// line the board always has: the board's name, the machine it is watching,
// what the fleet is doing, and how fresh the ledger under it is. It is a
// full-width accent bar when a width is known; the counts stay plain text so
// a pipe reads them too.
func topBar(b Board, width int) Line {
	workers := 0
	for _, repo := range b.Repos {
		for _, row := range repo.Rows {
			if !row.Main && row.PaneID != "" {
				workers++
			}
		}
	}

	var parts []string
	if b.Machine != "" {
		parts = append(parts, safetext.Escape(b.Machine))
	}
	if n := len(b.Escalations); n > 0 {
		parts = append(parts, plural(n, "escalation"))
	}
	switch {
	case b.InFlight > 0:
		parts = append(parts, strconv.Itoa(b.InFlight)+" in flight")
	case len(b.Escalations) == 0:
		parts = append(parts, "idle")
	}
	parts = append(parts, plural(workers, "worker"))
	parts = append(parts, ledgerFreshness(b))

	line := Line{Spans: []Span{
		{Text: " FLEET ", Style: styleBand},
		{Text: " " + strings.Join(parts, " · ") + " ", Style: styleBar},
	}}
	if width > 0 {
		if n := len([]rune(line.Plain())); n < width {
			line.Spans = append(line.Spans, Span{Text: strings.Repeat(" ", width-n), Style: styleBar})
		}
	}
	return line
}

// ledgerFreshness is the bar's last clause: how recently the record under
// this board moved. Trust in the board is trust in the ledger's age.
func ledgerFreshness(b Board) string {
	switch {
	case !b.LedgerFound:
		return "no ledger"
	case b.LedgerTS.IsZero():
		return "ledger empty"
	default:
		return "ledger " + ago(b.Now, b.LedgerTS)
	}
}

// notes are the warnings the contract wants said once rather than folded
// into silence: a ledger that is not there, lines that did not parse,
// versions this board cannot read, and open loops past the horizon. A yellow
// glyph, dim text — a warning the eye can find without a painted line.
func notes(b Board) []Line {
	var out []Line
	say := func(format string, a ...any) {
		out = append(out, Line{Spans: []Span{
			{Text: "  " + glyphEscalated + " ", Style: styleWarn},
			{Text: fmt.Sprintf(format, a...), Style: styleDim},
		}})
	}
	if !b.LedgerFound {
		say("ledger: none at %s; the board shows live state only", b.LedgerPath)
	}
	if b.Malformed > 0 {
		say("ledger: %s dropped as malformed", plural(b.Malformed, "line"))
	}
	if b.UnknownVer {
		say("ledger: entries with a version this board does not know were left unread; update worktender")
	}
	if b.Stale > 0 {
		say("ledger: %s still open beyond the 7-day horizon, not shown", plural(b.Stale, "task"))
	}
	return out
}

// inFlight is the live fleet: each repository's worktrees, then the
// dispatched tasks with nothing on the ground flying them. An empty section
// says so — the idle fleet is the default state, and a heading over nothing
// reads as broken.
func inFlight(b Board, l layout) []Line {
	var out []Line
	for _, repo := range b.Repos {
		out = append(out, textLine(styleDim, "  "+safetext.Escape(repo.Root)))
		if repo.Err != "" {
			out = append(out, Line{Spans: []Span{
				{Text: "    " + glyphFailed + " ", Style: styleBad},
				{Text: "cannot be read: " + safetext.Escape(repo.Err)},
			}})
			continue
		}
		if len(repo.Rows) == 0 {
			out = append(out, textLine(styleDim, "    no worktrees"))
			continue
		}
		out = append(out, table(rowSpecs(repo.Rows, func(r *LiveRow) rowSpec {
			return liveRow(b.Now, r, l)
		}))...)
	}
	if len(b.Repos) == 0 {
		out = append(out, placeholder("nothing in flight — herdr has no worktree workspaces open"))
	}

	if len(b.Orphans) > 0 {
		out = append(out, Line{Spans: []Span{
			{Text: "  " + glyphEscalated + " ", Style: styleWarn},
			{Text: "dispatched, no live worktree", Style: styleDim},
		}})
		out = append(out, table(rowSpecs(b.Orphans, func(r *TaskRow) rowSpec {
			return orphanRow(b.Now, r, l)
		}))...)
	}
	return out
}

// rowSpecs builds one section's rows.
func rowSpecs[T any](rows []T, spec func(T) rowSpec) []rowSpec {
	out := make([]rowSpec, 0, len(rows))
	for _, row := range rows {
		out = append(out, spec(row))
	}
	return out
}

// colCap bounds any one column so a long branch name or reason cannot push
// the columns beside it off the pane; the cell ends in an ellipsis instead.
const colCap = 40

// table aligns one section's rows into columns: each column as wide as its
// widest cell up to the cap, two spaces between columns, every cell keeping
// its own style. Alignment counts visible runes — the styles ride beside the
// text, not in it.
func table(rows []rowSpec) []Line {
	var widths []int
	for _, r := range rows {
		for i, c := range r.cells {
			if i >= len(widths) {
				widths = append(widths, 0)
			}
			n := len([]rune(c.text))
			if n > colCap {
				n = colCap
			}
			if n > widths[i] {
				widths[i] = n
			}
		}
	}

	out := make([]Line, 0, len(rows))
	for _, r := range rows {
		spans := make([]Span, 0, len(r.cells))
		for i, c := range r.cells {
			text := truncate(c.text, colCap)
			if i < len(r.cells)-1 {
				text = pad(text, widths[i]) + "  "
			}
			spans = append(spans, Span{Text: text, Style: c.style, Spin: c.spin})
		}
		out = append(out, Line{Spans: spans, Target: r.target})
	}
	return out
}

func escalationRow(now time.Time, r *TaskRow, l layout) rowSpec {
	glyph := cell{text: "  " + glyphEscalated, style: styleBad}
	var cells []cell
	if l == narrow {
		cells = []cell{
			glyph,
			{text: dash(r.Task.ID)},
			{text: dash(r.Task.Escalate.Reason)},
			{text: ago(now, r.Task.Escalate.TS), style: styleDim},
		}
	} else {
		cells = []cell{
			glyph,
			{text: dash(r.Task.Escalate.Severity), style: styleBad},
			{text: dash(r.Task.ID)},
			{text: dash(taskPlace(r.Task)), style: styleDim},
			{text: dash(r.Task.Escalate.Reason)},
			{text: ago(now, r.Task.Escalate.TS), style: styleDim},
		}
	}
	return rowSpec{cells: cells, target: escalationTarget(r)}
}

func escalationTarget(r *TaskRow) *Target {
	if r.Live != nil {
		t := liveTarget(r.Live)
		t.Label = safetext.Escape(r.Task.ID) + " (" + t.Label + ")"
		return t
	}
	return taskTarget(r)
}

// liveState is a live row's glyph, most alarming fact first: an escalation
// outranks a failure, a failure outranks the agent still typing, and only a
// row with nothing to say is idle. The third return marks the glyph that
// spins while the agent works.
func liveState(r *LiveRow) (string, Style, bool) {
	switch {
	case r.Main:
		return glyphMain, styleDim, false
	case r.Ghost:
		return glyphGhost, styleWarn, false
	}
	if t := r.Task; t != nil {
		switch {
		case t.Escalated():
			return glyphEscalated, styleBad, false
		case t.DeadlineState == "expired", t.Declined():
			return glyphFailed, styleBad, false
		}
	}
	if r.AgentStatus == "working" {
		return glyphWorking, styleGood, true
	}
	if rep := r.Report; rep != nil && rep.Found {
		switch rep.Status {
		case "blocked":
			return glyphFailed, styleBad, false
		case "done":
			return glyphVerified, styleGood, false
		}
	}
	switch r.AgentStatus {
	case "blocked":
		return glyphBlocked, styleWarn, false
	case "done":
		return glyphVerified, styleGood, false
	}
	return glyphIdle, styleDim, false
}

func liveRow(now time.Time, r *LiveRow, l layout) rowSpec {
	glyph, style, spin := liveState(r)
	name := r.Branch
	if name == "" {
		name = r.Dir
	}
	first := cell{text: "    " + glyph, style: style, spin: spin}
	var cells []cell
	switch l {
	case narrow:
		cells = []cell{
			first,
			{text: dash(name)},
			{text: ago(now, lastMoved(r)), style: styleDim},
		}
	case medium:
		cells = []cell{
			first,
			{text: dash(name)},
			{text: dash(r.AgentStatus), style: styleDim},
			{text: dash(taskText(r.Task))},
			{text: ago(now, lastMoved(r)), style: styleDim},
		}
	default:
		cells = []cell{
			first,
			{text: dash(name)},
			{text: dash(r.AgentStatus), style: styleDim},
			{text: dash(reportText(r.Report))},
			{text: dash(taskText(r.Task))},
			{text: dash(prText(r.PR))},
			{text: ago(now, lastMoved(r)), style: styleDim},
		}
	}
	// The main checkout is context, not a worker: the whole row recedes.
	if r.Main {
		for i := range cells {
			cells[i].style = styleDim
		}
	}
	return rowSpec{cells: cells, target: liveTarget(r)}
}

// lastMoved is the newest ledger timestamp the row has, zero when it has
// none. herdr has no clock to offer for the live half — see WithAgentSeqs —
// so the age column is the ledger's, and a row the ledger never wrote about
// shows none.
func lastMoved(r *LiveRow) time.Time {
	if r.Task == nil {
		return time.Time{}
	}
	return r.Task.Last.TS
}

func liveTarget(r *LiveRow) *Target {
	name := r.Branch
	if name == "" {
		name = r.Dir
	}
	t := &Target{
		Label:  safetext.Escape(name),
		PaneID: r.PaneID,
		Root:   r.Root,
		Branch: r.Branch,
	}
	if r.Report != nil && r.Report.Found {
		t.PR = r.Report.PR
	}
	if t.PR == 0 && r.Task != nil && r.Task.Report != nil {
		t.PR = r.Task.Report.PR
	}
	return t
}

// taskState is an on-its-own task's glyph — orphans and peers, the rows with
// no live worktree to speak for them.
func taskState(t *Task) (string, Style, bool) {
	switch {
	case t.Escalated():
		return glyphEscalated, styleBad, false
	case t.DeadlineState == "expired", t.Declined():
		return glyphFailed, styleBad, false
	case t.Report != nil && t.Report.Status == "blocked":
		return glyphBlocked, styleWarn, false
	case t.Report != nil && t.Report.Status == "done":
		return glyphVerified, styleGood, false
	default:
		return glyphWorking, Style{}, false
	}
}

func orphanRow(now time.Time, r *TaskRow, l layout) rowSpec {
	glyph, style, spin := taskState(r.Task)
	first := cell{text: "    " + glyph, style: style, spin: spin}
	var cells []cell
	switch l {
	case narrow:
		cells = []cell{
			first,
			{text: dash(r.Task.ID)},
			{text: ago(now, r.Task.Last.TS), style: styleDim},
		}
	default:
		cells = []cell{
			first,
			{text: dash(r.Task.ID)},
			{text: dash(taskPlace(r.Task)), style: styleDim},
			{text: dash(r.Task.Summary())},
			{text: ago(now, r.Task.Last.TS), style: styleDim},
		}
	}
	return rowSpec{cells: cells, target: taskTarget(r)}
}

// landedState is how a terminal task closed: the verdict glyph the RECENTLY
// LANDED section leads with, and the phrase beside it.
func landedState(t *Task) (glyph string, style Style, outcome string) {
	switch {
	case t.Verify != nil && t.Verify.Result == "pass":
		return glyphVerified, styleGood, "verify pass"
	case t.Verify != nil:
		return glyphFailed, styleBad, "verify " + t.Verify.Result
	default:
		return glyphEscalated, styleDim, "escalation acked"
	}
}

func landedRow(now time.Time, r *TaskRow, l layout) rowSpec {
	glyph, style, outcome := landedState(r.Task)
	pr := "-"
	if r.Task.Report != nil && r.Task.Report.PR > 0 {
		pr = "#" + strconv.Itoa(r.Task.Report.PR)
	}
	first := cell{text: "  " + glyph, style: style}
	var cells []cell
	switch l {
	case narrow:
		cells = []cell{
			first,
			{text: dash(taskPlace(r.Task)), style: styleDim},
			{text: pr},
			{text: ago(now, r.Task.Last.TS), style: styleDim},
		}
	case medium:
		cells = []cell{
			first,
			{text: dash(taskPlace(r.Task)), style: styleDim},
			{text: pr},
			{text: outcome},
			{text: ago(now, r.Task.Last.TS), style: styleDim},
		}
	default:
		cells = []cell{
			first,
			{text: dash(r.Task.ID)},
			{text: dash(taskPlace(r.Task)), style: styleDim},
			{text: pr},
			{text: outcome},
			{text: ago(now, r.Task.Last.TS), style: styleDim},
		}
	}
	return rowSpec{cells: cells, target: taskTarget(r)}
}

func peerRow(now time.Time, r *TaskRow, l layout) rowSpec {
	glyph, style, spin := taskState(r.Task)
	target := ""
	if r.Task.Dispatch != nil {
		target = r.Task.Dispatch.Target
	}
	first := cell{text: "  " + glyph, style: style, spin: spin}
	var cells []cell
	switch l {
	case narrow:
		cells = []cell{
			first,
			{text: dash(r.Task.ID)},
			{text: ago(now, r.Task.Last.TS), style: styleDim},
		}
	case medium:
		cells = []cell{
			first,
			{text: dash(r.Task.ID)},
			{text: dash(target)},
			{text: ago(now, r.Task.Last.TS), style: styleDim},
		}
	default:
		cells = []cell{
			first,
			{text: dash(r.Task.ID)},
			{text: dash(target)},
			{text: dash(r.Task.Summary())},
			{text: ago(now, r.Task.Last.TS), style: styleDim},
		}
	}
	return rowSpec{cells: cells, target: taskTarget(r)}
}

// taskTarget is navigation for a task rendered on its own. With a live row
// it is that row's; without one it can only ever open a pull request — there
// is no pane, that being what makes the task an orphan.
func taskTarget(r *TaskRow) *Target {
	if r.Live != nil {
		t := liveTarget(r.Live)
		t.Label = safetext.Escape(r.Task.ID) + " (" + t.Label + ")"
		if t.PR == 0 && r.Task.Report != nil {
			t.PR = r.Task.Report.PR
		}
		return t
	}
	t := &Target{Label: safetext.Escape(r.Task.ID)}
	if d := r.Task.Dispatch; d != nil {
		t.Root = d.Repo
	}
	if rep := r.Task.Report; rep != nil {
		t.PR = rep.PR
	}
	return t
}

// taskPlace is "repo#issue" as the dispatch named them, basename only: the
// full root earns its width on the repository headings, where failure needs
// it, not on every task row.
func taskPlace(t *Task) string {
	if t.Dispatch == nil {
		return ""
	}
	place := filepath.Base(t.Dispatch.Repo)
	if t.Dispatch.Issue > 0 {
		place += "#" + strconv.Itoa(t.Dispatch.Issue)
	}
	return place
}

func taskText(t *Task) string {
	if t == nil {
		return ""
	}
	return t.Summary()
}

// reportText is the worker's own last words, the same compression the ls
// report column applies and for the same reason.
func reportText(r *wt.Report) string {
	switch {
	case r == nil, r.Err != nil, !r.Found:
		return ""
	case r.PR > 0:
		return r.Status + " #" + strconv.Itoa(r.PR)
	default:
		return r.Status
	}
}

func prText(pr *wt.PR) string {
	if pr == nil || pr.Err != nil {
		return ""
	}
	return pr.State
}

// dash is one column's text: escaped, and a dash when there is nothing to
// show — the same renderer contract ls keeps, because the reader is the same
// person.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return safetext.Escape(s)
}

// ago says how long ago in words a person says — "4m ago", never a raw
// timestamp: the board redraws, so precision would be churn, and the column
// is read relatively down the rows.
func ago(now time.Time, ts time.Time) string {
	if ts.IsZero() {
		return "-"
	}
	d := now.Sub(ts)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h ago"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d ago"
	}
}

// plural is "%d %s(s)" without the parenthesis trick, because "1 escalations"
// in a header about escalations is the kind of wrong that erodes trust in the
// numbers beside it.
func plural(n int, noun string) string {
	s := strconv.Itoa(n) + " " + noun
	if n == 1 {
		return s
	}
	if strings.HasSuffix(noun, "y") {
		return strconv.Itoa(n) + " " + strings.TrimSuffix(noun, "y") + "ies"
	}
	return s + "s"
}
