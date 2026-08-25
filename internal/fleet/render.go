package fleet

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/steig/worktender/internal/safetext"
	"github.com/steig/worktender/internal/wt"
)

// Tones are the ANSI colors a line asks for. They live on the Line rather
// than inside its text so the tabwriter aligns visible characters only, the
// one-shot print and --json stay pipe-clean, and the watch frame — the one
// place that knows it is drawing on a terminal — is the one place that paints.
const (
	ToneAlert = "\x1b[1;31m" // the ESCALATIONS heading: bold red, the board's siren
	ToneBad   = "\x1b[31m"   // failed, escalated, declined, expired
	ToneGood  = "\x1b[32m"   // working and verified: a healthy fleet reads green
	ToneWarn  = "\x1b[33m"   // blocked, ghosts, divergence, ledger warnings
	ToneDim   = "\x1b[2m"    // idle rows, main checkouts, empty-section notes
	ToneHead  = "\x1b[1m"    // section headings
)

// Glyphs are the one-rune state column. A glyph beside a color rather than a
// color alone, because the one-shot print carries no color and the reader may
// not see red anyway.
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

// Line is one rendered board line. Target is nil on headings, notes and
// blanks; set on the rows a cursor can land on.
type Line struct {
	Text string
	// Tone is the ANSI prefix the watch frame paints this line with, empty for
	// the terminal's own foreground.
	Tone   string
	Target *Target
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
// right-hand columns rather than wrapping: a narrow pane still shows what each
// row is and how old, and the detail waits for width.
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

// rowSpec is one table row before alignment: its cells, the tone the whole
// line takes, and the navigation target the line keeps.
type rowSpec struct {
	cells  []string
	tone   string
	target *Target
}

// Lines renders the whole board as text rows with their navigation targets.
// One function for the one-shot print and the watch frame, so the two cannot
// disagree about what the fleet looks like. width picks the column layout;
// zero or less means no pane is asking and every column renders.
//
// The idle fleet is the default state and renders as a full screen, not a
// blank one: the summary header and RECENTLY LANDED are always there, and the
// empty sections say they are empty instead of not appearing.
func Lines(b Board, width int) []Line {
	l := layoutFor(width)
	out := []Line{summary(b)}
	out = append(out, notes(b)...)

	if len(b.Escalations) > 0 {
		out = append(out, Line{}, Line{Text: "ESCALATIONS", Tone: ToneAlert})
		out = append(out, table(b.Escalations, func(r *TaskRow) rowSpec {
			return rowSpec{cells: escalationCells(b.Now, r, l), tone: ToneBad, target: escalationTarget(r)}
		})...)
	}

	out = append(out, Line{}, Line{Text: "IN FLIGHT", Tone: ToneHead})
	out = append(out, inFlight(b, l)...)

	out = append(out, Line{}, Line{Text: "RECENTLY LANDED", Tone: ToneHead})
	if len(b.Recent) == 0 {
		out = append(out, Line{Text: "  nothing landed in the last 7 days", Tone: ToneDim})
	} else {
		out = append(out, table(b.Recent, func(r *TaskRow) rowSpec {
			return landedRow(b.Now, r, l)
		})...)
	}

	if len(b.Peers) > 0 {
		out = append(out, Line{}, Line{Text: "PEERS", Tone: ToneHead})
		out = append(out, table(b.Peers, func(r *TaskRow) rowSpec {
			return peerRow(b.Now, r, l)
		})...)
	}
	return out
}

// summary is the header line a person reads before anything else, and the one
// line the board always has: what the fleet is doing, how much of it there is,
// and how fresh the ledger under it is.
func summary(b Board) Line {
	workers := 0
	for _, repo := range b.Repos {
		for _, row := range repo.Rows {
			if !row.Main && row.PaneID != "" {
				workers++
			}
		}
	}

	parts := []string{"fleet"}
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

	tone := ToneHead
	if len(b.Escalations) > 0 {
		tone = ToneAlert
	}
	return Line{Text: strings.Join(parts, " · "), Tone: tone}
}

// ledgerFreshness is the header's last clause: how recently the record under
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

// notes are the warnings the contract wants said once rather than folded into
// silence: a ledger that is not there, lines that did not parse, versions this
// board cannot read, and open loops past the horizon.
func notes(b Board) []Line {
	var out []Line
	say := func(format string, a ...any) {
		out = append(out, Line{Text: fmt.Sprintf(format, a...), Tone: ToneWarn})
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

// inFlight is the live fleet: each repository's worktrees, then the dispatched
// tasks with nothing on the ground flying them. An empty section says so — the
// idle fleet is the default state, and a heading over nothing reads as broken.
func inFlight(b Board, l layout) []Line {
	var out []Line
	for _, repo := range b.Repos {
		out = append(out, Line{Text: "  " + safetext.Escape(repo.Root), Tone: ToneDim})
		if repo.Err != "" {
			out = append(out, Line{Text: "    cannot be read: " + safetext.Escape(repo.Err), Tone: ToneWarn})
			continue
		}
		if len(repo.Rows) == 0 {
			out = append(out, Line{Text: "    no worktrees", Tone: ToneDim})
			continue
		}
		out = append(out, table(repo.Rows, func(r *LiveRow) rowSpec {
			return liveRow(b.Now, r, l)
		})...)
	}
	if len(b.Repos) == 0 {
		out = append(out, Line{Text: "  nothing in flight — herdr has no worktree workspaces open", Tone: ToneDim})
	}

	if len(b.Orphans) > 0 {
		out = append(out, Line{Text: "  dispatched, no live worktree", Tone: ToneWarn})
		out = append(out, table(b.Orphans, func(r *TaskRow) rowSpec {
			return orphanRow(b.Now, r, l)
		})...)
	}
	return out
}

// table renders one section through a tabwriter so its columns align, then
// re-attaches each row's target and tone to the line it became. The tabwriter
// is per section because the sections have different columns, and aligning an
// escalation against a worktree row would be alignment of nothing with
// nothing.
func table[T any](rows []T, spec func(T) rowSpec) []Line {
	specs := make([]rowSpec, 0, len(rows))
	var sb strings.Builder
	tw := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', 0)
	for _, row := range rows {
		s := spec(row)
		specs = append(specs, s)
		fmt.Fprintln(tw, strings.Join(s.cells, "\t"))
	}
	if err := tw.Flush(); err != nil {
		// A tabwriter writing into a strings.Builder cannot fail; if it ever
		// does, an empty section is worse than an unaligned one.
		return nil
	}

	lines := strings.Split(strings.TrimRight(sb.String(), "\n"), "\n")
	out := make([]Line, 0, len(specs))
	for i, s := range specs {
		if i >= len(lines) {
			break
		}
		out = append(out, Line{Text: lines[i], Tone: s.tone, Target: s.target})
	}
	return out
}

func escalationCells(now time.Time, r *TaskRow, l layout) []string {
	if l == narrow {
		return []string{
			"  " + glyphEscalated,
			cell(r.Task.ID),
			cell(r.Task.Escalate.Reason),
			ago(now, r.Task.Escalate.TS),
		}
	}
	return []string{
		"  " + glyphEscalated,
		cell(r.Task.Escalate.Severity),
		cell(r.Task.ID),
		cell(taskPlace(r.Task)),
		cell(r.Task.Escalate.Reason),
		ago(now, r.Task.Escalate.TS),
	}
}

func escalationTarget(r *TaskRow) *Target {
	if r.Live != nil {
		t := liveTarget(r.Live)
		t.Label = safetext.Escape(r.Task.ID) + " (" + t.Label + ")"
		return t
	}
	return taskTarget(r)
}

// liveState is a live row's glyph and tone, most alarming fact first: an
// escalation outranks a failure, a failure outranks the agent still typing,
// and only a row with nothing to say is idle.
func liveState(r *LiveRow) (string, string) {
	switch {
	case r.Main:
		return glyphMain, ToneDim
	case r.Ghost:
		return glyphGhost, ToneWarn
	}
	if t := r.Task; t != nil {
		switch {
		case t.Escalated():
			return glyphEscalated, ToneBad
		case t.DeadlineState == "expired", t.Declined():
			return glyphFailed, ToneBad
		}
	}
	switch r.AgentStatus {
	case "working":
		return glyphWorking, ToneGood
	}
	if rep := r.Report; rep != nil && rep.Found {
		switch rep.Status {
		case "blocked":
			return glyphFailed, ToneBad
		case "done":
			return glyphVerified, ToneGood
		}
	}
	switch r.AgentStatus {
	case "blocked":
		return glyphBlocked, ToneWarn
	case "done":
		return glyphVerified, ToneGood
	}
	return glyphIdle, ToneDim
}

func liveRow(now time.Time, r *LiveRow, l layout) rowSpec {
	glyph, tone := liveState(r)
	name := r.Branch
	if name == "" {
		name = r.Dir
	}
	var cells []string
	switch l {
	case narrow:
		cells = []string{
			"    " + glyph,
			cell(name),
			ago(now, lastMoved(r)),
		}
	case medium:
		cells = []string{
			"    " + glyph,
			cell(name),
			cell(r.AgentStatus),
			cell(taskText(r.Task)),
			ago(now, lastMoved(r)),
		}
	default:
		cells = []string{
			"    " + glyph,
			cell(name),
			cell(r.AgentStatus),
			cell(reportText(r.Report)),
			cell(taskText(r.Task)),
			cell(prText(r.PR)),
			ago(now, lastMoved(r)),
		}
	}
	return rowSpec{cells: cells, tone: tone, target: liveTarget(r)}
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

// taskState is an on-its-own task's glyph and tone — orphans and peers, the
// rows with no live worktree to speak for them.
func taskState(t *Task) (string, string) {
	switch {
	case t.Escalated():
		return glyphEscalated, ToneBad
	case t.DeadlineState == "expired", t.Declined():
		return glyphFailed, ToneBad
	case t.Report != nil && t.Report.Status == "blocked":
		return glyphBlocked, ToneWarn
	case t.Report != nil && t.Report.Status == "done":
		return glyphVerified, ToneGood
	default:
		return glyphWorking, ""
	}
}

func orphanRow(now time.Time, r *TaskRow, l layout) rowSpec {
	glyph, tone := taskState(r.Task)
	var cells []string
	switch l {
	case narrow:
		cells = []string{
			"    " + glyph,
			cell(r.Task.ID),
			ago(now, r.Task.Last.TS),
		}
	default:
		cells = []string{
			"    " + glyph,
			cell(r.Task.ID),
			cell(taskPlace(r.Task)),
			cell(r.Task.Summary()),
			ago(now, r.Task.Last.TS),
		}
	}
	return rowSpec{cells: cells, tone: tone, target: taskTarget(r)}
}

// landedState is how a terminal task closed: the verdict glyph the RECENTLY
// LANDED section leads with, and the phrase beside it.
func landedState(t *Task) (glyph, tone, outcome string) {
	switch {
	case t.Verify != nil && t.Verify.Result == "pass":
		return glyphVerified, ToneGood, "verify pass"
	case t.Verify != nil:
		return glyphFailed, ToneBad, "verify " + t.Verify.Result
	default:
		return glyphEscalated, ToneDim, "escalation acked"
	}
}

func landedRow(now time.Time, r *TaskRow, l layout) rowSpec {
	glyph, tone, outcome := landedState(r.Task)
	pr := "-"
	if r.Task.Report != nil && r.Task.Report.PR > 0 {
		pr = "#" + strconv.Itoa(r.Task.Report.PR)
	}
	var cells []string
	switch l {
	case narrow:
		cells = []string{
			"  " + glyph,
			cell(taskPlace(r.Task)),
			pr,
			ago(now, r.Task.Last.TS),
		}
	case medium:
		cells = []string{
			"  " + glyph,
			cell(taskPlace(r.Task)),
			pr,
			outcome,
			ago(now, r.Task.Last.TS),
		}
	default:
		cells = []string{
			"  " + glyph,
			cell(r.Task.ID),
			cell(taskPlace(r.Task)),
			pr,
			outcome,
			ago(now, r.Task.Last.TS),
		}
	}
	return rowSpec{cells: cells, tone: tone, target: taskTarget(r)}
}

func peerRow(now time.Time, r *TaskRow, l layout) rowSpec {
	glyph, tone := taskState(r.Task)
	target := ""
	if r.Task.Dispatch != nil {
		target = r.Task.Dispatch.Target
	}
	var cells []string
	switch l {
	case narrow:
		cells = []string{
			"  " + glyph,
			cell(r.Task.ID),
			ago(now, r.Task.Last.TS),
		}
	case medium:
		cells = []string{
			"  " + glyph,
			cell(r.Task.ID),
			cell(target),
			ago(now, r.Task.Last.TS),
		}
	default:
		cells = []string{
			"  " + glyph,
			cell(r.Task.ID),
			cell(target),
			cell(r.Task.Summary()),
			ago(now, r.Task.Last.TS),
		}
	}
	return rowSpec{cells: cells, tone: tone, target: taskTarget(r)}
}

// taskTarget is navigation for a task rendered on its own. With a live row it
// is that row's; without one it can only ever open a pull request — there is
// no pane, that being what makes the task an orphan.
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

// cell is one column's text: escaped, and a dash when there is nothing to
// show — the same renderer contract ls keeps, because the reader is the same
// person.
func cell(s string) string {
	if s == "" {
		return "-"
	}
	return safetext.Escape(s)
}

// ago says how long ago in words a person says — "4m ago", never a raw
// timestamp: the board redraws, so precision would be churn, and the column is
// read relatively down the rows.
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
