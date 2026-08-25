package fleet

import (
	"strconv"
	"strings"
	"time"

	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/wt"
)

// Board is one reading of the fleet: the ledger's open loops merged with what
// herdr and git say is actually there. Escalations first, because they are
// the rows a person opened this to see.
type Board struct {
	Now time.Time

	// Escalations are the open tasks with an unacknowledged escalation,
	// safety tier first, newest first within a tier.
	Escalations []*TaskRow
	// Repos is the live fleet, one section per repository, each row annotated
	// with the ledger task matched to it when one was.
	Repos []RepoSection
	// Orphans are open ledger tasks no live worktree matched: dispatched work
	// whose checkout is gone, or was never here. The divergence between what
	// should be in flight and what is, which is the board's reason to exist.
	Orphans []*TaskRow
	// Peers are open tasks dispatched to peer sessions rather than worktree
	// workers — work with no checkout to land on by design, kept apart from
	// the orphans because a peer with no worktree is the ordinary case, not
	// the divergence.
	Peers []*TaskRow
	// Recent is the RECENTLY LANDED section: the last terminal tasks, newest
	// first. It is what the idle fleet shows — the board's default state is
	// nothing in flight, and that state must read as "here is what just
	// happened", never as a blank screen.
	Recent []*TaskRow

	// InFlight counts the open, unescalated tasks, for the summary header.
	InFlight int

	// Machine names the host in the top bar — a cockpit watching several
	// machines' panes needs each board to say whose fleet it is. Empty when
	// the caller has nothing to say.
	Machine string

	// LedgerFound is false when there is no ledger file yet — an ordinary
	// state, said in the header rather than implied by empty sections.
	LedgerFound bool
	LedgerPath  string
	// LedgerTS is the newest entry's timestamp — the freshness the summary
	// header reports — zero when the ledger is missing or empty.
	LedgerTS   time.Time
	Stale      int
	Malformed  int
	UnknownVer bool
}

// RepoSection is one repository's live rows.
type RepoSection struct {
	Root string
	// Err is why the repository could not be read, empty when it could.
	Err  string
	Rows []*LiveRow
}

// LiveRow is one live worktree row with the ledger task matched to it.
type LiveRow struct {
	wt.Row
	Root string
	Task *Task
}

// TaskRow is one ledger task rendered on its own — an escalation or an
// orphan — carrying the live row it matched when one did, so navigation
// still has a pane and a branch to go to.
type TaskRow struct {
	Task *Task
	Live *LiveRow
}

// Build merges the live listing with the ledger's open tasks.
//
// lookupPR fills pull request state for the rows where the fleet has claimed
// one — a worker's pane report or a ledger report naming a PR number. Only
// those rows, because each lookup is a `gh` call in series and the board
// refreshes; nil turns the column off.
func Build(repos []wt.Repo, ledger Ledger, path string, found bool, now time.Time, lookupPR func(root, branch string) (string, error)) Board {
	b := Board{Now: now, LedgerFound: found, LedgerPath: path,
		Malformed: ledger.Malformed, UnknownVer: ledger.UnknownVersion}
	if n := len(ledger.Entries); n > 0 {
		// The last line in file order rather than the max timestamp: appends
		// are the authority on what happened after what, and the timestamps
		// are whatever clock the writer had.
		b.LedgerTS = ledger.Entries[n-1].TS
	}

	folded := Fold(ledger.Entries)
	open, stale := Open(folded, now)
	b.Stale = stale

	var live []*LiveRow
	for _, repo := range repos {
		section := RepoSection{Root: repo.Root, Err: repo.Err}
		for i := range repo.Rows {
			row := &LiveRow{Row: repo.Rows[i], Root: repo.Root}
			section.Rows = append(section.Rows, row)
			live = append(live, row)
		}
		b.Repos = append(b.Repos, section)
	}

	for _, t := range open {
		row := match(t, live)
		switch {
		case t.Escalated():
			// The escalation gets its own row on top; it fills the live row's
			// task column only when nothing else is there, so a second task
			// on the same worktree keeps its place rather than being covered
			// by the one already shouting from the top of the board.
			b.Escalations = append(b.Escalations, &TaskRow{Task: t, Live: row})
			if row != nil && row.Task == nil {
				row.Task = t
			}
		case row != nil:
			b.InFlight++
			row.Task = t
		case t.Dispatch != nil && t.Dispatch.Executor == "peer":
			b.InFlight++
			b.Peers = append(b.Peers, &TaskRow{Task: t})
		default:
			b.InFlight++
			b.Orphans = append(b.Orphans, &TaskRow{Task: t})
		}
	}
	sortEscalations(b.Escalations)

	// The landed tasks still match live rows when their worktrees are up, so
	// jump-to-worker keeps working on a row whose work just finished.
	for _, t := range Landed(folded, now) {
		b.Recent = append(b.Recent, &TaskRow{Task: t, Live: match(t, live)})
	}

	if lookupPR != nil {
		withPRStates(live, lookupPR)
	}
	return b
}

// match finds the live row a task dispatched onto: same repository root, and
// a branch or directory carrying the issue number the way `start` names them
// — "<issue>-<slug>", or the bare number when the title gave no slug.
func match(t *Task, live []*LiveRow) *LiveRow {
	if t.Dispatch == nil || t.Dispatch.Repo == "" {
		return nil
	}
	root := gitx.Resolve(t.Dispatch.Repo)
	for _, row := range live {
		if gitx.Resolve(row.Root) != root {
			continue
		}
		if t.Dispatch.Issue > 0 && carriesIssue(row, t.Dispatch.Issue) {
			return row
		}
	}
	return nil
}

// carriesIssue reports whether a row's branch or directory is named for the
// issue. The prefix must end at the number — branch 156-x is issue 156, not
// issue 15 — which is what the trailing dash check is for.
func carriesIssue(row *LiveRow, issue int) bool {
	n := strconv.Itoa(issue)
	for _, name := range []string{row.Branch, row.Dir} {
		if name == n || strings.HasPrefix(name, n+"-") {
			return true
		}
	}
	return false
}

// sortEscalations puts the safety tier first and the newest first within a
// tier. Insertion order is file order, so the sort must be stable to keep
// same-moment escalations in the order they were written.
func sortEscalations(rows []*TaskRow) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && moreUrgent(rows[j], rows[j-1]); j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}

func moreUrgent(a, b *TaskRow) bool {
	sa, sb := a.Task.Escalate.Severity == "safety", b.Task.Escalate.Severity == "safety"
	if sa != sb {
		return sa
	}
	return a.Task.Escalate.TS.After(b.Task.Escalate.TS)
}

// withPRStates asks gh about the rows where the fleet claims a pull request
// exists: a pane report or a ledger report naming one. The answer lands in
// the row's own PR field, the same slot `ls --pr` fills, so the JSON carries
// asked-and-failed apart from not-asked the same way.
func withPRStates(live []*LiveRow, lookup func(root, branch string) (string, error)) {
	for _, row := range live {
		if row.Branch == "" || !claimsPR(row) {
			continue
		}
		state, err := lookup(row.Root, row.Branch)
		row.PR = &wt.PR{State: state, Err: err}
	}
}

func claimsPR(row *LiveRow) bool {
	if row.Report != nil && row.Report.Found && row.Report.PR > 0 {
		return true
	}
	return row.Task != nil && row.Task.Report != nil && row.Task.Report.PR > 0
}
