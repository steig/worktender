package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/steig/worktender/internal/execute"
	"github.com/steig/worktender/internal/jsonout"
	"github.com/steig/worktender/internal/reconcile"
	"github.com/steig/worktender/internal/roles"
)

// dispatch starts an agent in a named pane with configuration `sync` never
// supplies: which model to spend, how much autonomy to grant, and which role
// the agent starts as.
//
// It is separate from `sync` because sync runs from a keybinding and from event
// hooks, where nothing knows what the work is — an unattended reconciler with an
// opinion about cost or autonomy is how a hook starts spending the expensive
// model. It is not a herdr action for the reason `report` and `gate` are not: an
// action is a fixed command array with no argument surface.
//
// The pane re-check is not duplicated here on purpose. This builds a KindStaff
// action and hands it to the same executor `sync` uses, so execute.staff()
// covers both by construction rather than by a rule someone has to remember.
func dispatchCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("dispatch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	pane := fs.String("pane", "", "pane to start the agent in")
	name := fs.String("name", "", "herdr agent name")
	model := fs.String("model", "", "model to pass to the agent")
	permissionMode := fs.String("permission-mode", "", "agent permission mode")
	role := fs.String("role", "", "role to start the agent as, from the user's config")
	resume := fs.Bool("resume", false, "continue the pane's existing transcript")
	asJSON := jsonFlag(fs)

	if err := fs.Parse(args); err != nil {
		return usagef("%v; %s", err, dispatchUsage)
	}
	if fs.NArg() > 0 {
		return usagef("unexpected argument %q; %s", fs.Arg(0), dispatchUsage)
	}
	if *pane == "" {
		return usagef("--pane is required; %s", dispatchUsage)
	}
	if *name == "" {
		return usagef("--name is required; %s", dispatchUsage)
	}

	config, err := roles.Load()
	if err != nil {
		return withCode(exitUsage, err)
	}

	// Dispatch reads herdr and starts an agent; it changes no worktree and
	// removes nothing, so it needs no repository and takes no lock.
	s, err := newSession(true, herdrRequired)
	if err != nil {
		return err
	}

	// There is no issue here, so no labels: the flag or the default decides.
	launch, err := config.Resolve(*role, nil, s.root)
	if err == nil {
		err = launch.CheckAgent(s.root)
	}
	if err != nil {
		return withCode(exitUsage, err)
	}
	agentArgs := append(launch.Args(), agentArgsFor(*model, *permissionMode, os.Stderr)...)

	workspace, err := workspaceForPane(s, *pane)
	if err != nil {
		return err
	}

	executor := &execute.Executor{Client: s.client, Root: s.root, CallerDir: s.dir}
	results := executor.Run([]reconcile.Action{{
		Kind:        reconcile.KindStaff,
		WorkspaceID: workspace,
		PaneID:      *pane,
		AgentName:   *name,
		Resume:      *resume,
		AgentArgs:   agentArgs,
	}})

	notes := out
	if *asJSON {
		notes = os.Stderr
	}
	if line := launch.Line(); line != "" {
		fmt.Fprintln(notes, line)
	}
	fmt.Fprint(notes, execute.Render(results))

	doc := &dispatchJSON{
		PaneID:      *pane,
		WorkspaceID: jsonout.String(workspace),
		AgentName:   *name,
		Launch:      launch.JSON(),
		Staffing:    execute.JSON(results),
	}
	var failed error
	if execute.Counts(results)[execute.StatusFailed] > 0 {
		failed = fmt.Errorf("dispatch to %s failed", *pane)
	} else {
		// Only a dispatch that actually staffed earns a gate command; naming one
		// for an agent that never started would be an instruction to wait for
		// nothing.
		doc.GateCommand = jsonout.String(fmt.Sprintf("%s gate --target %s --until done", selfPath(), *name))
	}
	if *asJSON {
		if err := jsonout.Write(out, doc.finish(failed)); err != nil {
			return err
		}
	}
	return failed
}

// agentArgsFor turns dispatch's flags into agent arguments.
//
// A sandbox profile is deliberately missing and is not reachable from here:
// `claude` has no sandbox flag, sandboxing lives in settings.json, and this
// plugin does not write your agent's configuration. So --permission-mode can
// grant autonomy without the boundary that should accompany it. Two things keep
// that honest instead of a refusal — it is never defaulted, and a mode that
// disables prompting says so on stderr.
func agentArgsFor(model, permissionMode string, warn io.Writer) []string {
	var args []string
	if model != "" {
		args = append(args, "--model", model)
	}
	if permissionMode != "" {
		if unsandboxedModes[permissionMode] {
			fmt.Fprintf(warn, unsandboxedWarning, permissionMode)
		}
		args = append(args, "--permission-mode", permissionMode)
	}
	return args
}

// unsandboxedModes are the permission modes that stop an agent asking before it
// acts. A dispatched worker has no human at its pane, so it stalls on the first
// prompt and a coordinator structurally cannot clear it.
//
// An allowlist cannot substitute for a boundary: `$(...)` and `find -exec` run
// anything, so a name-based rule is decoration wherever the action has an API
// behind it. The boundary has to be capability-based, and this command cannot
// install one — the most it can do is decline to remove prompting silently.
var unsandboxedModes = map[string]bool{
	"bypassPermissions": true,
	"acceptEdits":       true,
}

// unsandboxedWarning goes to stderr rather than stdout: an action's stdout is
// read back out of the plugin log and parsed.
const unsandboxedWarning = "worktender: --permission-mode %s stops the agent asking before it acts, " +
	"and worktender cannot sandbox it — `claude` takes no sandbox flag, and this plugin does not " +
	"write your agent's configuration. Give the worker a boundary that does not depend on command " +
	"spelling: a sandbox profile, or a separate uid.\n"

const dispatchUsage = "usage: worktender dispatch --pane <id> --name <agent> " +
	"[--role <role>] [--model <model>] [--permission-mode <mode>] [--resume]"

// workspaceForPane asks herdr which workspace holds a pane, so the executor's
// re-check has something to check. A pane herdr does not know is fatal rather
// than dispatched blind.
func workspaceForPane(s *session, pane string) (string, error) {
	info, err := s.client.PaneGet(pane)
	if err != nil {
		return "", fmt.Errorf("ask herdr about pane %s: %w", pane, err)
	}
	if info.Pane.WorkspaceID == "" {
		// An unverifiable guard is not a satisfied one.
		return "", usagef("herdr reports no workspace for pane %s; refusing to start an agent there", pane)
	}
	return info.Pane.WorkspaceID, nil
}
