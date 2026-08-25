package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/steig/worktender/internal/execute"
	"github.com/steig/worktender/internal/fleet"
	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/reconcile"
)

// fleetControllerEnv arms the controller staffing one-shot. Unset means it
// does nothing at all.
//
// A gate of its own rather than a widening of eventsEnv, because the two arm
// different blast radii: WORKTENDER_EVENTS starts workers inside worktrees the
// user already made, while this starts the session that goes on to dispatch
// workers across every repository on the machine. Someone should be able to
// opt in to either without getting the other.
//
// The values it reads are exactly eventsEnv's — the same parser, not a copy of
// its rules — because two gates over the same kind of autonomous trigger that
// read the same spelling differently is how a mistyped opt-out arms something.
// Fail-closed the same way too: an unrecognised value is off, and
// unrecognisedControllerNotice makes it loud. Agents never arm this; it is a
// human exporting it into herdr's own environment, once, deliberately.
const fleetControllerEnv = "FLEET_CONTROLLER"

// controllerAgentName is the herdr agent name the controller session runs
// under, and the whole lease: while a live agent by this name exists, the
// one-shot staffs nothing.
//
// The name cannot collide with a staffed worker — reconcile.AgentName always
// ends in a dash and six hex digits — and herdr's agent namespace is global
// and refuses duplicates with agent_name_taken, so two racing one-shots
// resolve to one controller rather than two.
const controllerAgentName = "fleet"

// controllerWorkspaceLabel names the dedicated workspace the controller lives
// in. The label is how the one-shot finds the workspace again; it is never the
// lease — see fleetStaffCommand.
const controllerWorkspaceLabel = "fleet"

// fleetControllerEnabled reports whether the controller one-shot is armed.
func fleetControllerEnabled() bool {
	on, _ := parseEventsValue(os.Getenv(fleetControllerEnv))
	return on
}

// unrecognisedControllerNotice is the line owed to a session whose gate holds
// a value no rule covers, or "" when nothing is owed — eventsEnv's notice, for
// this gate.
func unrecognisedControllerNotice() string {
	raw := os.Getenv(fleetControllerEnv)
	if _, recognised := parseEventsValue(raw); recognised {
		return ""
	}
	return fmt.Sprintf("%s=%q is not a value this gate recognises, so the controller stays down; export %s=1 to arm it\n",
		fleetControllerEnv, raw, fleetControllerEnv)
}

// fleetStaffUsage is what an invocation of the controller one-shot may look
// like, which is the command and nothing else.
const fleetStaffUsage = "usage: worktender fleet staff (herdr invokes this at startup; it takes no arguments)"

// fleetStaffCommand is the controller staffing one-shot: herdr's [[startup]]
// entry for it runs once, after the server is ready, and when armed it makes
// sure one controller session exists. Nothing here loops or stays resident.
//
// The lease is agent presence, never pane or workspace existence. herdr
// restores panes across a restart but not the processes that were in them, so
// "the fleet pane is open" is true precisely when the controller is gone and
// must be re-staffed — a pane-existence lease wedges in exactly the state
// that matters. A live agent named `fleet` is the one fact that tracks the
// process, and it is the same lease worktender's own staffing runs on: staff
// what has no agent, and only that.
//
// The workspace and its transcript both survive the restart, so re-staffing
// resumes with --continue rather than starting the controller's memory over.
func fleetStaffCommand(args []string, out io.Writer) error {
	// Refused ahead of the opt-in, for the reason startup refuses it there:
	// answering a bad invocation with the not-armed notice reports the
	// argument as accepted, and rejecting argv reaches nothing the opt-in
	// guards.
	if len(args) > 0 {
		return usagef("unexpected argument %q; %s", args[0], fleetStaffUsage)
	}

	// Checked before anything else, so an un-armed install runs a process
	// that exits immediately and says why. Checked on every invocation
	// rather than only the herdr-startup one — the two are the same argv —
	// which also means no agent can reach staffing by running this command:
	// the gate lives in herdr's environment, where agents do not write.
	if !fleetControllerEnabled() {
		fmt.Fprintf(out, "the fleet controller is not armed; export %s=1 in herdr's environment to staff it at startup\n", fleetControllerEnv)
		fmt.Fprint(out, unrecognisedControllerNotice())
		return nil
	}

	client, err := herdrapi.New()
	if err != nil {
		return err
	}

	// The lease check. Presence is the whole test — a controller that is
	// working, blocked or idle is equally a controller whose conversation
	// staffing on top of would destroy. Wherever it lives, too: a controller
	// someone started by hand outside the dedicated workspace still holds
	// the role, and the name is global so herdr would refuse a second anyway.
	agents, err := client.AgentList()
	if err != nil {
		return err
	}
	for _, a := range agents.Agents {
		if a.Name != nil && *a.Name == controllerAgentName {
			fmt.Fprintf(out, "a live %s agent already holds the controller lease (pane %s); nothing to staff\n",
				controllerAgentName, a.PaneID)
			return nil
		}
	}

	home, err := controllerHome()
	if err != nil {
		return err
	}

	workspaceID, pane, err := controllerPane(client, out, home)
	if err != nil {
		return err
	}

	// Resume onto the controller's existing transcript when there is one,
	// otherwise a cold start. The controller's cwd exists for this check: a
	// directory nothing else runs Claude in, so --continue can only ever
	// reopen a controller conversation.
	resume := hasControllerTranscript(home)
	reason := "no live controller, no prior session"
	if resume {
		reason = "no live controller, prior session to resume"
	}

	// The same KindStaff action sync and dispatch build, so execute.staff's
	// re-check covers this path by construction: an agent that appeared in
	// the workspace since the lease check makes this a skip, not a second
	// controller.
	executor := &execute.Executor{Client: client, Root: home}
	results := executor.Run([]reconcile.Action{{
		Kind:        reconcile.KindStaff,
		Path:        home,
		WorkspaceID: workspaceID,
		PaneID:      pane,
		AgentName:   controllerAgentName,
		Resume:      resume,
		Reason:      reason,
	}})
	fmt.Fprint(out, execute.Render(results))
	if execute.Counts(results)[execute.StatusFailed] > 0 {
		return codef(exitNeedsHuman, "the fleet controller was not staffed; the %q workspace is there to inspect", controllerWorkspaceLabel)
	}
	if execute.Counts(results)[execute.StatusDone] == 0 {
		// Skipped: the re-check found an occupant. Whoever started it briefs
		// it; typing into their pane is not this one-shot's place.
		return nil
	}

	if err := deliverBrief(client, pane, controllerBrief(resume)); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nbriefed %s; the controller takes the fleet from here\n", controllerAgentName)
	return nil
}

// controllerHome is the directory the controller session runs in:
// the machine-local fleet state directory the ledger lives in, per
// docs/fleet-ledger-contract.md. It is in no repository — the controller
// works across all of them — and it is stable across restarts, which is what
// lets --continue find the controller's own conversation and nothing else's.
func controllerHome() (string, error) {
	ledger := fleet.LedgerPath()
	if ledger == "" {
		return "", codef(exitEnvironment, "no home directory, so no fleet state directory to run the controller in")
	}
	return filepath.Dir(ledger), nil
}

// controllerPane finds the dedicated workspace's pane, creating the workspace
// when this machine has never had one. The label is a name to find it by and
// nothing more: whether to staff was decided before this, on agent presence.
//
// Worktree workspaces are excluded from the search even when their label
// matches, because a workspace herdr labelled after a branch called `fleet`
// is somebody's checkout, not the controller's ground.
func controllerPane(client *herdrapi.Client, out io.Writer, home string) (workspaceID, paneID string, err error) {
	workspaces, err := client.WorkspaceList()
	if err != nil {
		return "", "", err
	}
	for _, ws := range workspaces.Workspaces {
		if ws.Label != controllerWorkspaceLabel || ws.Worktree != nil {
			continue
		}
		panes, err := client.PaneList(ws.WorkspaceID)
		if err != nil {
			return "", "", err
		}
		if len(panes.Panes) == 0 {
			// A workspace with no pane to staff is nothing this one-shot can
			// use, and closing it is not its call — say so and stop.
			return "", "", codef(exitNeedsHuman, "the %q workspace (%s) has no panes to staff; close it and re-run, or staff it by hand",
				controllerWorkspaceLabel, ws.WorkspaceID)
		}
		fmt.Fprintf(out, "reusing the %q workspace (%s)\n", controllerWorkspaceLabel, ws.WorkspaceID)
		return ws.WorkspaceID, panes.Panes[0].PaneID, nil
	}

	// First run on this machine: make the workspace. The state directory has
	// to exist for herdr to put a shell in it, and making it is safe — it is
	// the ledger's directory, and the contract already has the controller
	// creating it on first write.
	if err := os.MkdirAll(home, 0o755); err != nil {
		return "", "", fmt.Errorf("create the fleet state directory %s: %w", home, err)
	}
	created, err := client.WorkspaceCreate(home, controllerWorkspaceLabel, false)
	if err != nil {
		return "", "", fmt.Errorf("create the %q workspace: %w", controllerWorkspaceLabel, err)
	}
	fmt.Fprintf(out, "created the %q workspace (%s) in %s\n", controllerWorkspaceLabel, created.Workspace.WorkspaceID, home)
	return created.Workspace.WorkspaceID, created.RootPane.PaneID, nil
}

// hasControllerTranscript reports whether Claude Code has a stored
// conversation for the controller's directory — the same test staffing runs
// per worktree, against the same transcript store.
func hasControllerTranscript(home string) bool {
	projects := reconcile.DefaultProjectsDir()
	if projects == "" {
		return false
	}
	matches, err := filepath.Glob(filepath.Join(projects, reconcile.TranscriptSlug(home), "*.jsonl"))
	return err == nil && len(matches) > 0
}

// controllerBrief is the one line typed into the controller's pane once it is
// staffed. Like a worker's brief it names the role and where the instructions
// live rather than inlining them: the fleet-coordinator skill is the job
// description, and the session loads it itself.
func controllerBrief(resume bool) string {
	var b strings.Builder
	b.WriteString("You are this machine's fleet controller session. ")
	b.WriteString("Load the fleet-coordinator skill and take the fleet — the skill is the whole job description; follow it. ")
	if resume {
		b.WriteString("This is your prior controller conversation resumed after a herdr restart: ")
		b.WriteString("re-read the ledger before trusting anything you remember being in flight. ")
	}
	fmt.Fprintf(&b, "The worktender binary is at %s.", selfPath())
	return b.String()
}
