package main

import (
	"github.com/steig/worktender/internal/execute"
	"github.com/steig/worktender/internal/jsonout"
	"github.com/steig/worktender/internal/roles"
)

// startJSON is `start --json`: everything the text output prints for a human to
// copy, as fields.
//
// `start` already prints the agent name, the pane and the gate command because
// a person needs them; an agent should not have to scrape its own tool's prose
// to get them back.
//
// Written on the failure paths too, once there is anything to say. The failure
// that matters is a worktree created whose agent would not start — the caller
// then owns a checkout it did not have before, and it cannot clean that up
// without the branch and the path.
type startJSON struct {
	Repository string `json:"repository"`
	Issue      int    `json:"issue"`
	// Branch is null until the issue has been read, because the title is what
	// names it.
	Branch *string `json:"branch"`
	// Workspace and pane are null until the worktree exists.
	WorkspaceID *string `json:"workspace_id"`
	PaneID      *string `json:"pane_id"`
	// AgentName is null until an agent was actually started. It is not the
	// branch name: herdr's agent namespace spans every repository at once, so
	// the name carries a digest of the repository.
	AgentName *string `json:"agent_name"`
	// Base is the ref forked from, ForkPoint the commit that ref named at the
	// moment of the fork. The pair is here for the reason the text line is: a
	// ref moves, and a squash merge leaves a stacked branch sitting on commits
	// the base's history never contained.
	Base      *string `json:"base"`
	ForkPoint *string `json:"fork_point"`
	// Stacked is true when the fork point is not the base's own tip, so this
	// branch sits on commits the base does not have. That is the case worth
	// noticing before the base is squash-merged, which would land none of them.
	//
	// The same test the printed "stacked:" warning makes, deliberately: two
	// answers to one question is how a document and the prose beside it come to
	// disagree.
	Stacked bool `json:"stacked"`
	// Launch is the role the agent was started as and why, null when no role
	// applied — no configuration, or nothing in it that matched.
	Launch *roles.LaunchJSON `json:"launch"`
	// Model is what the agent was started with --model as, and ModelReason
	// why: "flag", "label model:<name>" or "default". Both null when claude
	// was left to pick its own.
	Model       *string `json:"model"`
	ModelReason *string `json:"model_reason"`
	// Briefed records that the agent took the brief up, which is a separate
	// fact from the agent having started: herdr answering ok means keystrokes
	// were delivered, not that anything received them.
	Briefed bool `json:"briefed"`
	// Staffing is the staff action's own result, in the shape `sync --json` and
	// `prune --json` use.
	Staffing []execute.ResultJSON `json:"staffing"`
	// GateCommand is the wait this start earned, ready to run. It targets the
	// pane, which outlives the agent name: herdr can stop resolving the name
	// while the worker is still running.
	GateCommand *string `json:"gate_command"`
	Error       *string `json:"error"`
	ExitCode    int     `json:"exit_code"`
}

// finish stamps the outcome onto a document about to be written. One place, so
// the code in the document and the code the process leaves with cannot drift.
func (d *startJSON) finish(err error) *startJSON {
	d.ExitCode = exitCode(err)
	if err != nil {
		msg := err.Error()
		d.Error = &msg
	}
	return d
}

// dispatchJSON is `dispatch --json`.
//
// Small on purpose: dispatch staffs one named pane and has nothing else to say.
// The pane is what the gate command takes; the agent name is kept beside it.
type dispatchJSON struct {
	PaneID      string               `json:"pane_id"`
	WorkspaceID *string              `json:"workspace_id"`
	AgentName   string               `json:"agent_name"`
	Launch      *roles.LaunchJSON    `json:"launch"`
	Staffing    []execute.ResultJSON `json:"staffing"`
	GateCommand *string              `json:"gate_command"`
	Error       *string              `json:"error"`
	ExitCode    int                  `json:"exit_code"`
}

func (d *dispatchJSON) finish(err error) *dispatchJSON {
	d.ExitCode = exitCode(err)
	if err != nil {
		msg := err.Error()
		d.Error = &msg
	}
	return d
}

// reportEnvelopeJSON is `report --json`: the envelope as *accepted*, which is
// not always the envelope as intended.
//
// A worker that reports and reads this back learns what was actually recorded.
// That matters more here than anywhere else in this output, because the note is
// refused rather than truncated when it is too long, and because the envelope
// reaches its coordinator through channels that have been observed to drop and
// mangle what they carry.
type reportEnvelopeJSON struct {
	Status string `json:"status"`
	PR     *int   `json:"pr"`
	Note   string `json:"note"`
	// NoteLength is the note's length in characters against the cap, so a worker
	// building one programmatically can see how close it came without counting
	// runes the way this command counts them.
	NoteLength int `json:"note_length"`
	NoteLimit  int `json:"note_limit"`
	// PaneID is the pane the envelope was attached to, null when it was not
	// attached to one — which is what a `report` run outside herdr looks like.
	PaneID *string `json:"pane_id"`
	// Delivered is whether the metadata channel took it. False with no error
	// means the envelope printed and nothing else, which a gate can still read
	// off the terminal but only while the line is still in the buffer.
	Delivered bool    `json:"delivered"`
	Error     *string `json:"error"`
	ExitCode  int     `json:"exit_code"`
}

func newReportEnvelopeJSON(r report, pane string, delivered bool, limit int) *reportEnvelopeJSON {
	out := &reportEnvelopeJSON{
		Status:     r.status,
		Note:       r.note,
		NoteLength: len([]rune(r.note)),
		NoteLimit:  limit,
		PaneID:     jsonout.String(pane),
		Delivered:  delivered,
	}
	if r.pr > 0 {
		pr := r.pr
		out.PR = &pr
	}
	return out
}

func (d *reportEnvelopeJSON) finish(err error) *reportEnvelopeJSON {
	d.ExitCode = exitCode(err)
	if err != nil {
		msg := err.Error()
		d.Error = &msg
	}
	return d
}
