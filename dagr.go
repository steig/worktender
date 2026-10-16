package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/steig/worktender/internal/gitx"
)

// `gate --dagr` keeps a herdr-dagr run file current while the gate waits, so
// the dagr pane is a live view of a dispatch without anyone writing JSON by
// hand. The schema is dagr's run-state contract v3 (CONTRACT.md in
// aemrebarut/herdr-dagr), which dagr enforces with `dagr check`.
//
// It is a flag on gate rather than a command of its own because the gate is
// the only part of worktender that observes anything: it sees each report
// arrive, sees a pane die, sees its own clock run out, and already says which
// of those it verified and which it only relayed. A one-shot snapshot command
// could read the reports still sitting on panes, but could never see a worker
// go away, so every node it drew would be the worker's word with nothing the
// gate checked beside it. The gate is also where a coordinator already sits for
// the length of a dispatch, which is what makes the view live for free.
//
// Nothing is written unless the flag is given, and nothing here can change the
// gate's outcome or exit code: a viewer that failed to update is a warning on
// the notes stream, not a different answer to the wait.

// dagrRunID marks a run file as worktender's. dagr's discovery reads one fixed
// path, so another producer — a coordinator following dagr's own producer
// skill — may already own it, and a file not carrying this id is never
// overwritten.
const dagrRunID = "worktender"

// dagrVersion is the contract version written. dagr reads 1 through 3 and asks
// producers to write 3.
const dagrVersion = 3

// dagrRunPath is where dagr looks when nobody tells it otherwise:
// `.dagr/run.json` under the workspace's directory. The main checkout rather
// than the caller's own worktree, because that is where a coordinator's
// workspace sits and the one place every worker's gate agrees on. Outside a
// repository the current directory is used, since a gate needs no repository.
func dagrRunPath() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if root, err := gitx.RepoRoot(dir); err == nil {
		dir = root
	}
	return filepath.Join(dir, ".dagr", "run.json"), nil
}

// dagrDoc is the subset of the contract worktender has facts for. Every field
// left out is one worktender does not observe — no dependencies between
// workers, no attempt start times, no liveness — and the contract renders an
// absent fact as absent rather than guessing it.
type dagrDoc struct {
	Dagr        int        `json:"dagr"`
	Run         dagrRun    `json:"run"`
	GeneratedAt string     `json:"generated_at"`
	Tasks       []dagrTask `json:"tasks"`
}

type dagrRun struct {
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
}

// dagrTask is one worker. Its id is the name the gate was given, which is
// stable across gates where the pane id is not.
type dagrTask struct {
	ID       string        `json:"id"`
	Title    string        `json:"title"`
	Kind     string        `json:"kind"`
	Owner    string        `json:"owner,omitempty"`
	State    string        `json:"state"`
	Deps     []string      `json:"deps"`
	Unblock  string        `json:"unblock,omitempty"`
	Note     string        `json:"note,omitempty"`
	Attempts []dagrAttempt `json:"attempts"`
}

type dagrAttempt struct {
	ID       string        `json:"id"`
	N        int           `json:"n"`
	Cause    dagrCause     `json:"cause"`
	Actor    string        `json:"actor,omitempty"`
	Locator  *dagrLocator  `json:"locator,omitempty"`
	State    string        `json:"state"`
	EndedAt  string        `json:"ended_at,omitempty"`
	Outcome  *dagrOutcome  `json:"outcome,omitempty"`
	Progress *dagrProgress `json:"progress,omitempty"`
}

type dagrCause struct {
	Type   string `json:"type"`
	Ref    string `json:"ref,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type dagrLocator struct {
	Pane  string `json:"pane,omitempty"`
	Agent string `json:"agent,omitempty"`
}

type dagrOutcome struct {
	Result   string `json:"result"`
	Evidence string `json:"evidence"`
	Receipt  string `json:"receipt,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type dagrProgress struct {
	Note string `json:"note,omitempty"`
}

// dagrEvidence maps the gate's evidence onto dagr's tiers. Two of the three
// are the same word; the gate's `inferred` is dagr's `heuristic` (≈), the tier
// the contract defines as a reading of screen or pane state.
func dagrEvidence(gate string) string {
	if gate == gateEvidenceInferred {
		return "heuristic"
	}
	return gate
}

// dagrExport is one gate's hold on the run file. It is nil when the flag was
// not given, and every method is a no-op on nil so the gate's own code does not
// branch on it.
type dagrExport struct {
	path string
	doc  dagrDoc
	warn io.Writer
	now  func() time.Time
}

// openDagrExport loads the run file, or starts one, and marks every target as
// being worked on.
//
// An existing file is carried forward rather than replaced, because a fleet is
// gated in rounds — `--any a,b,c` releases b, then `--any a,c` — and a view
// that forgot b every time it released would show less of the dispatch the
// further it got. Workers this gate is not watching are left exactly as the
// last gate to watch them wrote them.
//
// A file that is not worktender's is refused before the wait opens, as a usage
// error: the caller has to move it or drop the flag, and finding out at the
// deadline would be finding out fifteen minutes late.
func openDagrExport(path string, targets []gateTarget, warn io.Writer, now func() time.Time) (*dagrExport, error) {
	e := &dagrExport{path: path, warn: warn, now: now}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		e.doc = dagrDoc{
			Dagr: dagrVersion,
			Run:  dagrRun{ID: dagrRunID, Title: "worktender dispatch", StartedAt: stamp(now())},
		}
	case err != nil:
		return nil, fmt.Errorf("--dagr: %w", err)
	default:
		if err := json.Unmarshal(raw, &e.doc); err != nil || e.doc.Run.ID != dagrRunID {
			return nil, usagef("--dagr: %s is not a run file worktender wrote (want run.id %q), so it is not overwritten; move it or drop --dagr", path, dagrRunID)
		}
		e.doc.Dagr = dagrVersion
	}

	for _, t := range targets {
		task := e.task(t)
		// A gate treats whatever a worker already reported as a previous task's
		// answer, so a worker whose last attempt settled is starting another one
		// by the gate's own definition. Opening a new attempt for it, rather than
		// moving the old one back to working, is how the contract says a retry is
		// drawn: history does not repaint.
		e.attempt(task, t, "gated again after its last attempt settled")
		task.State = "working"
		task.Unblock = ""
		task.Note = ""
	}
	e.write()
	return e, nil
}

// task returns the task for a target, appending one the first time a worker is
// gated on.
func (e *dagrExport) task(t gateTarget) *dagrTask {
	for i := range e.doc.Tasks {
		if e.doc.Tasks[i].ID == t.name {
			return &e.doc.Tasks[i]
		}
	}
	e.doc.Tasks = append(e.doc.Tasks, dagrTask{
		ID: t.name, Title: t.name, Kind: "impl", Owner: t.name, Deps: []string{},
	})
	return &e.doc.Tasks[len(e.doc.Tasks)-1]
}

// attempt returns the task's live attempt, opening one when there is none: on a
// task's first sight, or when its last attempt already settled. The locator is
// refreshed either way, because a pane id is only good while the pane lives.
func (e *dagrExport) attempt(task *dagrTask, t gateTarget, why string) *dagrAttempt {
	locator := &dagrLocator{Pane: t.pane, Agent: t.name}
	if n := len(task.Attempts); n > 0 && task.Attempts[n-1].State == "working" {
		a := &task.Attempts[n-1]
		a.Locator = locator
		return a
	}
	n := len(task.Attempts) + 1
	cause := dagrCause{Type: "initial"}
	if n > 1 {
		cause = dagrCause{Type: "followup", Ref: task.Attempts[n-2].ID, Reason: why}
	}
	task.Attempts = append(task.Attempts, dagrAttempt{
		ID: fmt.Sprintf("%s·a%d", task.ID, n), N: n, Cause: cause,
		Actor: t.name, Locator: locator, State: "working",
	})
	return &task.Attempts[n-1]
}

// reported records a fresh report — every one, not only the one that released
// the gate, because a `planned` from a worker the gate is still waiting on is
// still news about that worker. All of it is the worker's word, so a `done`
// settles as `reported` (◇) and never as `verified`.
//
// The note goes in as the worker wrote it. It is untrusted text, but dagr
// escapes control characters at its display boundary, and report has already
// refused any note that was not a single line of plain text.
func (e *dagrExport) reported(t gateTarget, r report) {
	if e == nil {
		return
	}
	task := e.task(t)
	a := e.attempt(task, t, "reported "+r.status+" after its last attempt settled")
	task.Unblock = ""
	task.Note = ""
	switch r.status {
	case "done":
		a.State = "done"
		a.EndedAt = stamp(e.now())
		a.Progress = nil
		a.Outcome = &dagrOutcome{Result: "done", Evidence: dagrEvidence(gateEvidenceReported), Reason: r.note}
		if r.pr > 0 {
			a.Outcome.Receipt = fmt.Sprintf("pr #%d", r.pr)
		}
		task.State = "done"
	case "blocked":
		// The attempt is still alive; it is the task that cannot move. The party
		// that can move it is whoever is sitting in the gate.
		a.Progress = &dagrProgress{Note: "blocked: " + r.note}
		task.State = "blocked"
		task.Unblock = "coordinator"
	default:
		a.Progress = &dagrProgress{Note: r.status + ": " + r.note}
		task.State = "working"
	}
}

// gone records a worker the gate heard go away. The attempt is `lost`, the
// contract's state for a runtime that vanished, at the tier the gate reached it
// by. The task follows the contract's two readings of a lost attempt: `failed`
// when herdr announced the pane or workspace gone, `blocked` when herdr only
// lost track of the agent — which is what a death looks like, and also what
// something less final looks like.
//
// A worker that had already reported `done` is left alone: finishing and then
// exiting is not a loss.
func (e *dagrExport) gone(t gateTarget, why, evidence string) {
	if e == nil {
		return
	}
	task := e.task(t)
	n := len(task.Attempts)
	if n == 0 || task.Attempts[n-1].State != "working" {
		return
	}
	a := &task.Attempts[n-1]
	a.State = "lost"
	a.EndedAt = stamp(e.now())
	a.Locator = nil
	a.Progress = nil
	a.Outcome = &dagrOutcome{Result: "lost", Evidence: dagrEvidence(evidence), Reason: why}
	task.Unblock = ""
	if evidence == gateEvidenceVerified {
		task.State = "failed"
		return
	}
	task.State = "blocked"
	task.Unblock = "coordinator"
}

// timedOut notes the expiry on every worker still open. Their states do not
// change: a gate running out of time is a fact about the gate, and the workers
// may well still be working. The contract has no evidence tier for a task-level
// note, so the tier the gate reached it by is said in words.
func (e *dagrExport) timedOut(targets []gateTarget, waited time.Duration) {
	if e == nil {
		return
	}
	for _, t := range targets {
		task := e.task(t)
		if task.State == "working" || task.State == "blocked" {
			task.Note = fmt.Sprintf("gate timed out after %s with no new report (evidence: %s)", waited.Round(time.Second), gateEvidenceVerified)
		}
	}
}

// write replaces the run file in one rename, so dagr's mtime poll never reads
// half a document. A failure is a warning: the view is an aside to the wait.
func (e *dagrExport) write() {
	if e == nil {
		return
	}
	e.doc.GeneratedAt = stamp(e.now())
	if err := writeDagrFile(e.path, e.doc); err != nil {
		fmt.Fprintf(e.warn, "worktender: --dagr: %v; the dagr view is stale, the gate is not\n", err)
	}
}

// writeDagrFile writes the document through a temporary file in the same
// directory and renames it into place.
//
// The directory is created on first use with a `.gitignore` of `*` inside it,
// because dagr's path is inside the user's checkout and a run file is
// ephemeral, machine-local state that should never show up as an untracked
// file. A `.dagr` directory that already existed is the user's, and is left
// without one.
func writeDagrFile(path string, doc dagrDoc) error {
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*\n"), 0o644); err != nil {
			return err
		}
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".run.json.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(body, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// stamp is the contract's timestamp: ISO-8601, in UTC so two writers on
// different zones never disagree about order.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }
