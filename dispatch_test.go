package main

import (
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/herdrtest"
)

// The reconciler must never route models or autonomy. `sync` runs from a
// keybinding and from event hooks where no role exists to route on, so an
// unattended pass is given no opinion about cost or permission.
func TestReconcilerNeverSetsAgentArgs(t *testing.T) {
	if args := agentArgsFor("", "", io.Discard); len(args) != 0 {
		t.Errorf("unconfigured dispatch produced %q; staffing must stay bare unless asked", args)
	}
}

func TestModelAndPermissionModeBecomeAgentArgs(t *testing.T) {
	args := agentArgsFor("sonnet", "bypassPermissions", io.Discard)

	got := strings.Join(args, " ")
	if want := "--model sonnet --permission-mode bypassPermissions"; got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

// A worker that stalls on a permission prompt cannot be cleared by the
// coordinator that dispatched it, so an autonomy-granting mode is passed
// through rather than refused. What it must not do is pass silently: the caller
// is told what was granted, and what boundary is missing.
func TestUnsandboxedPermissionModeWarnsAndProceeds(t *testing.T) {
	for mode := range unsandboxedModes {
		t.Run(mode, func(t *testing.T) {
			var warn strings.Builder

			args := agentArgsFor("", mode, &warn)

			if got := strings.Join(args, " "); got != "--permission-mode "+mode {
				t.Errorf("args = %q; the mode must reach the agent", got)
			}
			if !strings.Contains(warn.String(), mode) {
				t.Errorf("warning must name the mode granted, got %q", warn.String())
			}
			if !strings.Contains(warn.String(), "sandbox") {
				t.Errorf("warning must say what is missing, got %q", warn.String())
			}
		})
	}
}

// A mode that leaves prompting intact grants nothing that was not already the
// default, so it warns about nothing.
func TestOrdinaryPermissionModeDoesNotWarn(t *testing.T) {
	var warn strings.Builder

	args := agentArgsFor("", "manual", &warn)

	if strings.Join(args, " ") != "--permission-mode manual" {
		t.Errorf("args = %q", args)
	}
	if warn.String() != "" {
		t.Errorf("a prompting mode must pass through quietly, warned %q", warn.String())
	}
}

func TestDispatchRequiresPaneAndName(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"--pane", "w2:p1"},
		{"--name", "worker"},
	} {
		if err := dispatchCommand(args, io.Discard); err == nil {
			t.Errorf("dispatch(%q) returned nil; both --pane and --name are required", args)
		}
	}
}

// dispatch staffs workers too, so it gives them the same token start does.
func TestDispatchGivesTheWorkerItsOwnToken(t *testing.T) {
	t.Setenv(workerTokenEnv, "github_pat_worker_only")
	repo := herdrtest.NewRepo(t)
	server := fakeStart(t, repo, "w9", "w9:p1", "working")
	server.HandleResult("pane.get", map[string]any{"type": "pane_info", "pane": map[string]any{
		"pane_id": "w9:p1", "workspace_id": "w9", "tab_id": "t1",
		"terminal_id": "term_1", "agent_status": "idle", "focused": false, "revision": 1,
	}})

	if err := dispatchCommand([]string{"--pane", "w9:p1", "--name", "w1"}, io.Discard); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if args := agentStartArgs(t, server); !slices.Contains(args, "--settings") {
		t.Errorf("agent args = %v; want --settings carrying the worker's token", args)
	}
}
