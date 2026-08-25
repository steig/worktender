package fleet

import (
	"path/filepath"
	"time"

	"github.com/steig/worktender/internal/jsonout"
	"github.com/steig/worktender/internal/wt"
)

// BoardJSON is what `fleet board --json` writes: the same board the table
// renders, built from the same merge, so the two cannot disagree about what
// the fleet looks like.
type BoardJSON struct {
	Ledger LedgerJSON `json:"ledger"`
	// Escalations are the open tasks with an unacknowledged escalation, in
	// the order the board draws them: safety first, newest first.
	Escalations []TaskRowJSON `json:"escalations"`
	// Repositories is the live fleet, the `ls --all-repos` shape with a task
	// field on each row where the ledger matched one.
	Repositories []RepoJSON `json:"repositories"`
	// UnmatchedTasks are open ledger tasks no live worktree matched: work
	// that should be in flight with nothing on the ground flying it.
	UnmatchedTasks []TaskRowJSON `json:"unmatched_tasks"`
}

// LedgerJSON says what the reading of the ledger was like, because a board
// built over dropped lines has to say so to be trusted.
type LedgerJSON struct {
	Path string `json:"path"`
	// Found is false when no ledger file exists yet, which is an ordinary
	// state, not a failure.
	Found          bool `json:"found"`
	Malformed      int  `json:"malformed"`
	UnknownVersion bool `json:"unknown_version"`
	StaleOpenTasks int  `json:"stale_open_tasks"`
}

// RepoJSON is one repository's live rows. The same shape and reasons as the
// ls listing's — see wt.RepoJSON — with the ledger annotation added.
type RepoJSON struct {
	Root      string        `json:"root"`
	Name      string        `json:"name"`
	Error     *string       `json:"error"`
	Worktrees []LiveRowJSON `json:"worktrees"`
}

// LiveRowJSON is a worktree row plus the ledger task matched to it, null when
// the ledger has no open task for this worktree.
type LiveRowJSON struct {
	wt.RowJSON
	Task *TaskJSON `json:"task"`
}

// TaskRowJSON is a task rendered on its own — an escalation or an unmatched
// task — with the live coordinates it matched, when it matched any, so a
// consumer can navigate the way the board does.
type TaskRowJSON struct {
	TaskJSON
	Root   *string `json:"root"`
	Branch *string `json:"branch"`
	PaneID *string `json:"pane_id"`
}

// TaskJSON is one open ledger task. Absence is null throughout, for the
// reason the ls row's is: the dash means several things and JSON has a word
// for none of them.
type TaskJSON struct {
	Task string  `json:"task"`
	By   *string `json:"by"`
	// Summary is the phrase the table's task column prints.
	Summary   string  `json:"summary"`
	Escalated bool    `json:"escalated"`
	Severity  *string `json:"severity"`
	Reason    *string `json:"reason"`
	Executor  *string `json:"executor"`
	Target    *string `json:"target"`
	Repo      *string `json:"repo"`
	Issue     *int    `json:"issue"`
	// Status and PR are the ledger `report`'s, not the pane report's — the
	// row carries that one itself.
	Status        *string `json:"status"`
	PR            *int    `json:"pr"`
	Deadline      *string `json:"deadline"`
	DeadlineState *string `json:"deadline_state"`
	LastType      string  `json:"last_type"`
	LastTS        string  `json:"last_ts"`
	// LastRaw is the raw line of the latest entry when its type is not one
	// this board knows — the contract has unknown types rendering raw, and
	// UNTRUSTED is the right reading of a line whose meaning is unknown.
	LastRaw *string `json:"last_raw"`
}

// JSON projects the board for a machine.
func JSON(b Board) BoardJSON {
	out := BoardJSON{
		Ledger: LedgerJSON{
			Path: b.LedgerPath, Found: b.LedgerFound,
			Malformed: b.Malformed, UnknownVersion: b.UnknownVer,
			StaleOpenTasks: b.Stale,
		},
		Escalations:    make([]TaskRowJSON, 0, len(b.Escalations)),
		Repositories:   make([]RepoJSON, 0, len(b.Repos)),
		UnmatchedTasks: make([]TaskRowJSON, 0, len(b.Orphans)),
	}
	for _, row := range b.Escalations {
		out.Escalations = append(out.Escalations, taskRowJSON(row))
	}
	for _, repo := range b.Repos {
		entry := RepoJSON{Root: repo.Root, Name: filepath.Base(repo.Root), Error: jsonout.String(repo.Err)}
		if repo.Err == "" {
			entry.Worktrees = make([]LiveRowJSON, 0, len(repo.Rows))
			for _, row := range repo.Rows {
				entry.Worktrees = append(entry.Worktrees, LiveRowJSON{
					RowJSON: wt.JSON([]wt.Row{row.Row})[0],
					Task:    taskJSON(row.Task),
				})
			}
		}
		out.Repositories = append(out.Repositories, entry)
	}
	for _, row := range b.Orphans {
		out.UnmatchedTasks = append(out.UnmatchedTasks, taskRowJSON(row))
	}
	return out
}

func taskRowJSON(row *TaskRow) TaskRowJSON {
	out := TaskRowJSON{TaskJSON: *taskJSON(row.Task)}
	if row.Live != nil {
		out.Root = jsonout.String(row.Live.Root)
		out.Branch = jsonout.String(row.Live.Branch)
		out.PaneID = jsonout.String(row.Live.PaneID)
	}
	return out
}

func taskJSON(t *Task) *TaskJSON {
	if t == nil {
		return nil
	}
	out := &TaskJSON{
		Task: t.ID, By: jsonout.String(t.By), Summary: t.Summary(),
		Escalated: t.Escalated(),
		LastType:  t.Last.Type, LastTS: t.Last.TS.Format(time.RFC3339),
	}
	if t.Escalate != nil {
		out.Severity = jsonout.String(t.Escalate.Severity)
		out.Reason = jsonout.String(t.Escalate.Reason)
	}
	if d := t.Dispatch; d != nil {
		out.Executor = jsonout.String(d.Executor)
		out.Target = jsonout.String(d.Target)
		out.Repo = jsonout.String(d.Repo)
		if d.Issue > 0 {
			issue := d.Issue
			out.Issue = &issue
		}
	}
	if r := t.Report; r != nil {
		out.Status = jsonout.String(r.Status)
		if r.PR > 0 {
			pr := r.PR
			out.PR = &pr
		}
	}
	if !t.Deadline.IsZero() {
		out.Deadline = jsonout.String(t.Deadline.Format(time.RFC3339))
	}
	out.DeadlineState = jsonout.String(t.DeadlineState)
	if !knownType(t.Last.Type) {
		out.LastRaw = jsonout.String(t.Last.Raw)
	}
	return out
}
