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

// Line is one rendered board line. Target is nil on headings, notes and
// blanks; set on the rows a cursor can land on.
type Line struct {
	Text   string
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

// Lines renders the whole board as text rows with their navigation targets.
// One function for the one-shot print and the watch frame, so the two cannot
// disagree about what the fleet looks like.
func Lines(b Board) []Line {
	var out []Line
	out = append(out, Line{Text: header(b)})
	out = append(out, notes(b)...)

	if len(b.Escalations) > 0 {
		out = append(out, Line{}, Line{Text: "escalations"})
		out = append(out, table(b.Escalations, func(r *TaskRow) []string {
			return escalationCells(b.Now, r)
		}, escalationTarget)...)
	}

	for _, repo := range b.Repos {
		out = append(out, Line{}, Line{Text: safetext.Escape(repo.Root)})
		if repo.Err != "" {
			out = append(out, Line{Text: "  cannot be read: " + safetext.Escape(repo.Err)})
			continue
		}
		if len(repo.Rows) == 0 {
			out = append(out, Line{Text: "  no worktrees"})
			continue
		}
		out = append(out, table(repo.Rows, func(r *LiveRow) []string {
			return liveCells(b.Now, r)
		}, liveTarget)...)
	}
	if len(b.Repos) == 0 {
		out = append(out, Line{}, Line{Text: "no repositories: herdr has no worktree workspaces open"})
	}

	if len(b.Orphans) > 0 {
		out = append(out, Line{}, Line{Text: "ledger tasks with no live worktree"})
		out = append(out, table(b.Orphans, func(r *TaskRow) []string {
			return orphanCells(b.Now, r)
		}, orphanTarget)...)
	}
	return out
}

// header is the one line that says how much fleet there is before the eye
// starts scanning rows.
func header(b Board) string {
	open := len(b.Orphans)
	for _, repo := range b.Repos {
		for _, row := range repo.Rows {
			if row.Task != nil {
				open++
			}
		}
	}
	open += len(b.Escalations)

	parts := []string{"fleet"}
	parts = append(parts, plural(open, "open task"))
	if n := len(b.Escalations); n > 0 {
		parts = append(parts, plural(n, "escalation"))
	}
	parts = append(parts, plural(len(b.Repos), "repository"))
	return strings.Join(parts, " · ")
}

// notes are the warnings the contract wants said once rather than folded into
// silence: a ledger that is not there, lines that did not parse, versions this
// board cannot read, and open loops past the horizon.
func notes(b Board) []Line {
	var out []Line
	say := func(format string, a ...any) {
		out = append(out, Line{Text: fmt.Sprintf(format, a...)})
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

// table renders one section through a tabwriter so its columns align, then
// re-attaches each row's target to the line it became. The tabwriter is per
// section because the sections have different columns, and aligning an
// escalation against a worktree row would be alignment of nothing with
// nothing.
func table[T any](rows []T, cells func(T) []string, target func(T) *Target) []Line {
	var sb strings.Builder
	tw := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', 0)
	for _, row := range rows {
		fmt.Fprintln(tw, strings.Join(cells(row), "\t"))
	}
	if err := tw.Flush(); err != nil {
		// A tabwriter writing into a strings.Builder cannot fail; if it ever
		// does, an empty section is worse than an unaligned one.
		return nil
	}

	lines := strings.Split(strings.TrimRight(sb.String(), "\n"), "\n")
	out := make([]Line, 0, len(rows))
	for i, row := range rows {
		if i >= len(lines) {
			break
		}
		out = append(out, Line{Text: lines[i], Target: target(row)})
	}
	return out
}

func escalationCells(now time.Time, r *TaskRow) []string {
	return []string{
		"  !",
		cell(r.Task.Escalate.Severity),
		cell(r.Task.ID),
		cell(taskPlace(r.Task)),
		cell(r.Task.Escalate.Reason),
		age(now, r.Task.Escalate.TS),
	}
}

func escalationTarget(r *TaskRow) *Target {
	if r.Live != nil {
		t := liveTarget(r.Live)
		t.Label = safetext.Escape(r.Task.ID) + " (" + t.Label + ")"
		return t
	}
	return orphanTarget(r)
}

func liveCells(now time.Time, r *LiveRow) []string {
	marker := "   "
	switch {
	case r.Main:
		marker = "  *"
	case r.Ghost:
		marker = "  ?"
	}
	name := r.Branch
	if name == "" {
		name = r.Dir
	}
	return []string{
		marker,
		cell(name),
		cell(r.AgentStatus),
		cell(reportText(r.Report)),
		cell(taskText(r.Task)),
		cell(prText(r.PR)),
		age(now, lastMoved(r)),
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

func orphanCells(now time.Time, r *TaskRow) []string {
	return []string{
		"  ",
		cell(r.Task.ID),
		cell(taskPlace(r.Task)),
		cell(r.Task.Summary()),
		age(now, r.Task.Last.TS),
	}
}

// orphanTarget can only ever open a pull request: there is no pane, that
// being what makes the task an orphan.
func orphanTarget(r *TaskRow) *Target {
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

// age says how long ago in one short word-of-a-number: the board redraws, so
// precision would be churn, and the column is read relatively down the rows.
func age(now time.Time, ts time.Time) string {
	if ts.IsZero() {
		return "-"
	}
	d := now.Sub(ts)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
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
