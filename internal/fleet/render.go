package fleet

import (
	"fmt"
	"path/filepath"
	"sort"
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

// The board draws itself as bordered panels, lazygit-style: rounded corners,
// the title embedded in the top border, one cell of interior padding.
const (
	boxTL, boxTR = "╭", "╮"
	boxBL, boxBR = "╰", "╯"
	boxH, boxV   = "─", "│"
)

// Line is one rendered board line: styled spans. Selectability lives in the
// View's hotspots, not on the line — with panels side by side, one visual
// line can carry rows from two panels.
type Line struct {
	Spans []Span
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

// Hotspot is one selectable row: the line it lives on, the span range the
// selection highlight paints — the panel's interior, not the whole screen —
// and the navigation target.
type Hotspot struct {
	Line int
	// SpanFrom and SpanTo bound the highlight, [from, to) over the line's
	// spans.
	SpanFrom, SpanTo int
	Target           *Target
	// rank orders navigation by urgency — escalations first — independent of
	// where the panel sits on screen.
	rank int
}

// View is one rendered board: the lines to draw and the rows a cursor can
// visit, in navigation order.
type View struct {
	Lines []Line
	Hots  []Hotspot
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

// layout is how many columns a panel has room for, judged by its interior
// width. Panels drop their right-hand columns rather than wrapping: a narrow
// panel still shows what each row is and how old, and the detail waits for
// width.
type layout int

const (
	wide   layout = iota // everything
	medium               // drop the report and PR-state detail
	narrow               // state, name, age — the glanceable minimum
)

func layoutFor(width int) layout {
	switch {
	case width >= 70:
		return wide
	case width >= 45:
		return medium
	default:
		return narrow
	}
}

// Widths of the composition: below twoColMin the panels stack in one
// column; at or above it, IN FLIGHT takes the left three fifths and the
// task panels stack on the right.
const (
	fallbackWidth = 100
	twoColMin     = 110
	minInner      = 20
)

// Section ranks: the order navigation walks the rows, most urgent first,
// wherever the panels sit on screen.
const (
	rankEscalations = iota
	rankInFlight
	rankLanded
	rankPeers
	rankWorktrees
)

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

// panelRow is one line of a panel's interior, ready to be boxed.
type panelRow struct {
	spans  []Span
	target *Target
}

// block is a rendered region: lines plus the hotspots on them, line indices
// local to the block so blocks compose.
type block struct {
	lines []Line
	hots  []Hotspot
}

// Render draws the whole board: the top bar, the ledger's warnings, and the
// sections as bordered panels — two columns when the pane is wide enough,
// one stacked column when it is not. One function for the one-shot print and
// the watch frame, so the two cannot disagree about what the fleet looks
// like. width <= 0 means no pane is asking, and the board draws at a
// scrollback-friendly default.
//
// The idle fleet is the default state and renders as a full screen, not a
// blank one: the top bar and RECENTLY LANDED are always there, and the empty
// sections say they are empty instead of not appearing.
func Render(b Board, width int) View {
	if width <= 0 {
		width = fallbackWidth
	}

	head := block{lines: []Line{topBar(b, width)}}
	head.lines = append(head.lines, notes(b)...)
	head.lines = append(head.lines, Line{})

	var body block
	if width >= twoColMin {
		leftW := width * 3 / 5
		left := stack(inFlightPanel(b, leftW-4), worktreesPanel(b, leftW-4))
		right := stack(
			escalationsPanel(b, width-leftW-5),
			landedPanel(b, width-leftW-5),
			peersPanel(b, width-leftW-5),
		)
		body = beside(left, leftW, right)
	} else {
		inner := width - 4
		if inner < minInner {
			inner = minInner
		}
		body = stack(
			escalationsPanel(b, inner),
			inFlightPanel(b, inner),
			landedPanel(b, inner),
			peersPanel(b, inner),
			worktreesPanel(b, inner),
		)
	}

	whole := stack(head, body)
	view := View{Lines: whole.lines, Hots: whole.hots}
	// Navigation order is urgency order — the cursor starts on an
	// escalation when there is one — independent of panel placement.
	sort.SliceStable(view.Hots, func(i, j int) bool { return view.Hots[i].rank < view.Hots[j].rank })
	return view
}

// stack concatenates blocks vertically, re-basing hotspot line indices.
func stack(blocks ...block) block {
	var out block
	for _, b := range blocks {
		base := len(out.lines)
		out.lines = append(out.lines, b.lines...)
		for _, h := range b.hots {
			h.Line += base
			out.hots = append(out.hots, h)
		}
	}
	return out
}

// beside lays two blocks side by side with a one-cell gap, left column fixed
// at leftW. Hotspots keep their rows: a right-panel row's span range shifts
// by however many spans the left half of its line has.
func beside(left block, leftW int, right block) block {
	var out block
	n := len(left.lines)
	if len(right.lines) > n {
		n = len(right.lines)
	}
	offsets := make([]int, n)
	for i := 0; i < n; i++ {
		var spans []Span
		if i < len(left.lines) {
			spans = append(spans, left.lines[i].Spans...)
		}
		if w := spanWidth(spans); w < leftW {
			spans = append(spans, Span{Text: strings.Repeat(" ", leftW-w)})
		}
		spans = append(spans, Span{Text: " "})
		offsets[i] = len(spans)
		if i < len(right.lines) {
			spans = append(spans, right.lines[i].Spans...)
		}
		out.lines = append(out.lines, Line{Spans: spans})
	}
	out.hots = append(out.hots, left.hots...)
	for _, h := range right.hots {
		h.SpanFrom += offsets[h.Line]
		h.SpanTo += offsets[h.Line]
		out.hots = append(out.hots, h)
	}
	return out
}

// panel boxes rows into a bordered card: rounded corners, the title in the
// top border, one cell of interior padding. Rows are clipped to the interior
// and padded to it, so a selected row's highlight fills the panel exactly.
// A panel with no rows renders its empty line instead — or nothing at all
// when it has no empty line to speak.
func panel(title string, rank, innerW int, rows []panelRow, empty string) block {
	if len(rows) == 0 {
		if empty == "" {
			return block{}
		}
		rows = []panelRow{{spans: []Span{{Text: empty, Style: styleDim}}}}
	}

	// Top border: ╭─ TITLE ────╮ is 5 runes of frame around the title, so
	// the dashes fill what is left of innerW+4.
	t := truncate(title, innerW-1)
	fill := innerW - 1 - len([]rune(t))
	if fill < 0 {
		fill = 0
	}
	var out block
	out.lines = append(out.lines, Line{Spans: []Span{
		{Text: boxTL + boxH + " ", Style: styleBorder},
		{Text: t, Style: styleKey},
		{Text: " " + strings.Repeat(boxH, fill) + boxTR, Style: styleBorder},
	}})

	for _, row := range rows {
		spans := clipSpans(row.spans, innerW)
		lineSpans := make([]Span, 0, len(spans)+3)
		lineSpans = append(lineSpans, Span{Text: boxV + " ", Style: styleBorder})
		lineSpans = append(lineSpans, spans...)
		if w := spanWidth(spans); w < innerW {
			lineSpans = append(lineSpans, Span{Text: strings.Repeat(" ", innerW-w)})
		}
		from, to := 1, len(lineSpans)
		lineSpans = append(lineSpans, Span{Text: " " + boxV, Style: styleBorder})
		if row.target != nil {
			out.hots = append(out.hots, Hotspot{
				Line: len(out.lines), SpanFrom: from, SpanTo: to,
				Target: row.target, rank: rank,
			})
		}
		out.lines = append(out.lines, Line{Spans: lineSpans})
	}

	out.lines = append(out.lines, textLine(styleBorder, boxBL+strings.Repeat(boxH, innerW+2)+boxBR))
	return out
}

// topBar is the header bar a person reads before anything else, and the one
// line the board always has: the board's name, the machine it is watching,
// what the fleet is doing, and how fresh the ledger under it is. It is a
// full-width accent bar; the counts stay plain text so a pipe reads them
// too.
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
	if n := spanWidth(line.Spans); n < width {
		line.Spans = append(line.Spans, Span{Text: strings.Repeat(" ", width-n), Style: styleBar})
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

func escalationsPanel(b Board, innerW int) block {
	l := layoutFor(innerW)
	return panel("ESCALATIONS", rankEscalations, innerW, tableRows(rowSpecsOf(b.Escalations, func(r *TaskRow) rowSpec {
		return escalationRow(b.Now, r, l)
	})), "")
}

// inFlight reports whether a live row is actually in flight: an agent in the
// middle of something, or an open ledger task on the worktree. An idle main
// or a branch checkout nobody dispatched is capacity, not flight — it lives
// in the WORKTREES panel instead.
func inFlight(r *LiveRow) bool {
	if r.Task != nil {
		return true
	}
	return r.AgentStatus == "working" || r.AgentStatus == "blocked"
}

// inFlightPanel is the work: the live rows that are actually flying, then
// the dispatched tasks with nothing on the ground flying them. Rows carry
// repo/branch names — the panel replaces the per-repository grouping, and
// the full root only earns its width when a repository cannot be read.
func inFlightPanel(b Board, innerW int) block {
	l := layoutFor(innerW)
	var specs []rowSpec
	var broken []panelRow
	for _, repo := range b.Repos {
		if repo.Err != "" {
			broken = append(broken, panelRow{spans: []Span{
				{Text: glyphFailed + " ", Style: styleBad},
				{Text: safetext.Escape(repo.Root) + ": " + safetext.Escape(repo.Err)},
			}})
			continue
		}
		for _, row := range repo.Rows {
			if inFlight(row) {
				specs = append(specs, liveRow(b.Now, row, l))
			}
		}
	}

	rows := append(broken, tableRows(specs)...)
	if len(b.Orphans) > 0 {
		rows = append(rows, panelRow{spans: []Span{
			{Text: glyphEscalated + " ", Style: styleWarn},
			{Text: "dispatched, no live worktree", Style: styleDim},
		}})
		rows = append(rows, tableRows(rowSpecsOf(b.Orphans, func(r *TaskRow) rowSpec {
			return orphanRow(b.Now, r, l)
		}))...)
	}

	empty := "nothing in flight"
	if len(b.Repos) == 0 {
		empty = "nothing in flight — herdr has no worktree workspaces open"
	}
	return panel("IN FLIGHT", rankInFlight, innerW, rows, empty)
}

// worktreesPanel is the capacity behind the flight: mains, idle checkouts,
// ghosts — dim, compact, still navigable so a pane is one enter away.
func worktreesPanel(b Board, innerW int) block {
	var specs []rowSpec
	for _, repo := range b.Repos {
		if repo.Err != "" {
			continue
		}
		for _, row := range repo.Rows {
			if !inFlight(row) {
				specs = append(specs, worktreeRow(row))
			}
		}
	}
	return panel("WORKTREES", rankWorktrees, innerW, tableRows(specs), "")
}

func landedPanel(b Board, innerW int) block {
	l := layoutFor(innerW)
	return panel("RECENTLY LANDED", rankLanded, innerW, tableRows(rowSpecsOf(b.Recent, func(r *TaskRow) rowSpec {
		return landedRow(b.Now, r, l)
	})), "nothing landed in the last 7 days")
}

func peersPanel(b Board, innerW int) block {
	l := layoutFor(innerW)
	return panel("PEERS", rankPeers, innerW, tableRows(rowSpecsOf(b.Peers, func(r *TaskRow) rowSpec {
		return peerRow(b.Now, r, l)
	})), "")
}

func rowSpecsOf[T any](rows []T, spec func(T) rowSpec) []rowSpec {
	out := make([]rowSpec, 0, len(rows))
	for _, row := range rows {
		out = append(out, spec(row))
	}
	return out
}

// colCap bounds any one column so a long branch name or reason cannot push
// the columns beside it off the panel; the cell ends in an ellipsis instead.
const colCap = 40

// tableRows aligns one section's rows into columns: each column as wide as
// its widest cell up to the cap, two spaces between columns, every cell
// keeping its own style. Alignment counts visible runes — the styles ride
// beside the text, not in it.
func tableRows(rows []rowSpec) []panelRow {
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

	out := make([]panelRow, 0, len(rows))
	for _, r := range rows {
		spans := make([]Span, 0, len(r.cells))
		for i, c := range r.cells {
			text := truncate(c.text, colCap)
			if i < len(r.cells)-1 {
				text = pad(text, widths[i]) + "  "
			}
			spans = append(spans, Span{Text: text, Style: c.style, Spin: c.spin})
		}
		out = append(out, panelRow{spans: spans, target: r.target})
	}
	return out
}

func escalationRow(now time.Time, r *TaskRow, l layout) rowSpec {
	glyph := cell{text: glyphEscalated, style: styleBad}
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
	case r.Ghost:
		return glyphGhost, styleWarn, false
	case r.Main && r.AgentStatus != "working" && r.AgentStatus != "blocked":
		return glyphMain, styleDim, false
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

// liveName is repo/branch: the panel has no per-repository grouping, so the
// row itself says where it is, basename only.
func liveName(r *LiveRow) string {
	name := r.Branch
	if name == "" {
		name = r.Dir
	}
	return filepath.Base(r.Root) + "/" + name
}

func liveRow(now time.Time, r *LiveRow, l layout) rowSpec {
	glyph, style, spin := liveState(r)
	first := cell{text: glyph, style: style, spin: spin}
	var cells []cell
	switch l {
	case narrow:
		cells = []cell{
			first,
			{text: dash(liveName(r))},
			{text: ago(now, lastMoved(r)), style: styleDim},
		}
	case medium:
		cells = []cell{
			first,
			{text: dash(liveName(r))},
			{text: dash(r.AgentStatus), style: styleDim},
			{text: dash(taskText(r.Task))},
			{text: ago(now, lastMoved(r)), style: styleDim},
		}
	default:
		cells = []cell{
			first,
			{text: dash(liveName(r))},
			{text: dash(r.AgentStatus), style: styleDim},
			{text: dash(reportText(r.Report))},
			{text: dash(taskText(r.Task))},
			{text: dash(prText(r.PR))},
			{text: ago(now, lastMoved(r)), style: styleDim},
		}
	}
	return rowSpec{cells: cells, target: liveTarget(r)}
}

// worktreeRow is a WORKTREES panel row: compact and entirely dim — capacity
// the eye should be able to skip.
func worktreeRow(r *LiveRow) rowSpec {
	glyph := glyphIdle
	switch {
	case r.Ghost:
		glyph = glyphGhost
	case r.Main:
		glyph = glyphMain
	}
	return rowSpec{
		cells: []cell{
			{text: glyph, style: styleDim},
			{text: dash(liveName(r)), style: styleDim},
			{text: dash(r.AgentStatus), style: styleDim},
		},
		target: liveTarget(r),
	}
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
	first := cell{text: glyph, style: style, spin: spin}
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
// LANDED section leads with, and the phrase beside it. The verdict is the
// task's last verify entry — a fail followed by a later pass landed as a
// pass, which is what the fold's last-wins keeps.
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
	first := cell{text: glyph, style: style}
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
	first := cell{text: glyph, style: style, spin: spin}
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
			{text: dash(r.Task.Target)},
			{text: ago(now, r.Task.Last.TS), style: styleDim},
		}
	default:
		cells = []cell{
			first,
			{text: dash(r.Task.ID)},
			{text: dash(r.Task.Target)},
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
// full root earns its width where failure needs it, not on every task row.
// Work that never named a repository — a task steered to a peer session —
// shows the session it lives with instead, which is the answer to the same
// "where is this" question.
func taskPlace(t *Task) string {
	if t.Dispatch == nil || t.Dispatch.Repo == "" {
		return t.Target
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
