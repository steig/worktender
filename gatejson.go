package main

import (
	"time"

	"github.com/steig/worktender/internal/jsonout"
)

// The outcomes a gate can reach. A consumer branches on this rather than on the
// exit code when it is already parsing the document — the two agree, and
// `outcome` is the one that survives being piped somewhere that dropped `$?`.
//
// They are finer than the codes on purpose: `timeout` and `worker_gone` are both
// exit 4, and a coordinator deciding whether to redispatch may reasonably want
// to know a pane died rather than that nobody answered in time.
const (
	gateOutcomeReleased   = "released"
	gateOutcomeBlocked    = "blocked"
	gateOutcomeTimeout    = "timeout"
	gateOutcomeWorkerGone = "worker_gone"
	// gateOutcomeError is the wait ending for a reason that is not about any
	// worker: herdr's socket dropping, a target that would not resolve. Its
	// exit code carries the distinction; `outcome` only says the gate never
	// reached a verdict.
	gateOutcomeError = "error"
)

// How a gate came to its outcome. The gate checks a report rather than relaying
// it, but what it checks is the report's shape and freshness, never its truth:
// a `done` that released the gate is still the worker's word. A coordinator
// scanning the output has to be able to tell that from something the gate saw
// for itself, and from something it only guessed at.
//
// Each outcome carries the one that describes how it was actually reached, set
// where it is decided rather than looked up from the outcome afterwards,
// because `worker_gone` alone is reached two different ways.
const (
	// gateEvidenceVerified is the gate's own observation: its clock ran out with
	// no fresh report on either channel, or herdr announced the pane or its
	// workspace gone. Neither depends on anything the worker said.
	gateEvidenceVerified = "verified"
	// gateEvidenceReported is the worker's claim, relayed. `released` and
	// `blocked` are both this: the gate proved a well-formed report appeared
	// after it opened, and nothing about whether the work it describes is done.
	gateEvidenceReported = "reported"
	// gateEvidenceInferred is a reading of pane state. herdr's agent status
	// going `unknown` is what an agent that has gone away looks like, which is
	// not the same as herdr saying it went.
	gateEvidenceInferred = "inferred"
)

// gateJSON is `gate --json`, written whether the gate released or failed.
//
// Written on failure too, for the reason `sync --json` is: a consumer that
// learns only the exit code learns nothing about *which* of several workers it
// was about, and with `--any` that is the whole answer.
type gateJSON struct {
	// Outcome is one of the constants above.
	Outcome string `json:"outcome"`
	// ExitCode is the code the process will leave with. Carried in-band because
	// a document read off a pipe is routinely separated from its `$?`, and
	// because the pair disagreeing is then a visible bug rather than a silent
	// one.
	ExitCode int `json:"exit_code"`
	// Target is the worker the outcome is about: which one released, which one
	// reported blocked, which one's pane died. Null on a timeout, which is the
	// one outcome that belongs to no single worker.
	Target *gateTargetJSON `json:"target"`
	// Evidence is how the outcome was established, one of the constants above.
	// Null on `error`, the outcome that is not a verdict: there is nothing for
	// it to be evidence of.
	Evidence *string `json:"evidence"`
	// Report is the envelope that ended the wait, null when none did.
	Report *reportJSON `json:"report"`
	// Waited is how long the gate ran, in whole seconds. Seconds rather than a
	// duration string because the consumer is arithmetic, not prose.
	WaitedSeconds int `json:"waited_seconds"`
	// TimeoutSeconds is what it was allowed, so a consumer can tell a gate that
	// nearly made it from one that never had a chance.
	TimeoutSeconds int `json:"timeout_seconds"`
	// Until is the statuses that would have released it, and RequirePR whether a
	// pull request number was also demanded. Both echo the request: a document
	// filed away is unreadable without knowing what was asked.
	Until     []string `json:"until"`
	RequirePR bool     `json:"require_pr"`
	// Waiting is every worker the gate covered, in the order named, each with
	// whatever its channels already held when the gate opened. That baseline is
	// what the gate deliberately ignored as a previous task's answer, and a
	// coordinator that gated too late needs to see it to know that is what
	// happened.
	Waiting []gateTargetJSON `json:"waiting"`
	// Error is the message that went to stderr, null when the gate released.
	Error *string `json:"error"`
}

// gateTargetJSON is one worker the gate knew about.
type gateTargetJSON struct {
	Name        string  `json:"name"`
	PaneID      string  `json:"pane_id"`
	WorkspaceID *string `json:"workspace_id"`
	// Baseline is what this worker's channels held when the gate opened, null
	// when they held nothing. Never mistake it for an answer to this wait.
	Baseline *reportJSON `json:"baseline"`
}

// reportJSON is the three-slot envelope.
//
// The note is carried because a human reading the document wants it. It stays
// untrusted data: nothing here is a field a gate can be asked to match on, for
// the reason `--note-contains` does not exist.
type reportJSON struct {
	Status string `json:"status"`
	// PR is null rather than 0 when the worker gave none. Zero is a pull request
	// number that cannot exist, but null is the answer that does not have to be
	// explained.
	PR   *int   `json:"pr"`
	Note string `json:"note"`
}

func newReportJSON(r report) *reportJSON {
	out := &reportJSON{Status: r.status, Note: r.note}
	if r.pr > 0 {
		pr := r.pr
		out.PR = &pr
	}
	return out
}

func newGateTargetJSON(t gateTarget) gateTargetJSON {
	out := gateTargetJSON{Name: t.name, PaneID: t.pane, WorkspaceID: jsonout.String(t.workspace)}
	if t.hasBaseline {
		out.Baseline = newReportJSON(t.baseline)
	}
	return out
}

// gateDocument assembles the document. Callers pass the outcome and the error
// it corresponds to; the exit code is read off that error so the two cannot
// drift apart.
func gateDocument(opts gateOptions, targets []gateTarget, outcome, evidence string, about int, r *report, waited time.Duration, err error) gateJSON {
	doc := gateJSON{
		Outcome:        outcome,
		Evidence:       jsonout.String(evidence),
		ExitCode:       exitCode(err),
		Report:         nil,
		WaitedSeconds:  int(waited.Round(time.Second).Seconds()),
		TimeoutSeconds: int(opts.timeout.Seconds()),
		Until:          opts.until,
		RequirePR:      opts.requirePR,
		Waiting:        make([]gateTargetJSON, 0, len(targets)),
	}
	for _, t := range targets {
		doc.Waiting = append(doc.Waiting, newGateTargetJSON(t))
	}
	if about >= 0 && about < len(targets) {
		t := newGateTargetJSON(targets[about])
		doc.Target = &t
	}
	if r != nil {
		doc.Report = newReportJSON(*r)
	}
	if err != nil {
		msg := err.Error()
		doc.Error = &msg
	}
	return doc
}
