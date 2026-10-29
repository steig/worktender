package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/execute"
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
	t.Setenv(execute.WorkerTokenEnv, "github_pat_worker_only")
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

// A token dispatch cannot store fails the dispatch, and --json still answers
// with a document saying so rather than nothing.
func TestDispatchJSONReportsAWorkerTokenItCannotStore(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server := fakeStart(t, repo, "w9", "w9:p1", "working")
	server.HandleResult("pane.get", map[string]any{"type": "pane_info", "pane": map[string]any{
		"pane_id": "w9:p1", "workspace_id": "w9", "tab_id": "t1",
		"terminal_id": "term_1", "agent_status": "idle", "focused": false, "revision": 1,
	}})
	t.Setenv(execute.WorkerTokenEnv, "github_pat_worker_only")
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_PLUGIN_STATE_DIR", notADir)

	var out strings.Builder
	if err := dispatchCommand([]string{"--pane", "w9:p1", "--name", "w1", "--json"}, &out); err == nil {
		t.Fatal("dispatch staffed an agent with nowhere to keep its token")
	}
	var doc dispatchJSON
	decodeOne(t, out.String(), &doc)
	if doc.Error == nil || len(doc.Staffing) != 1 || !strings.Contains(doc.Staffing[0].Detail, "worker token") {
		t.Errorf("doc = %+v; want the failed staffing and its reason", doc)
	}
	if strings.Contains(out.String(), "github_pat_worker_only") {
		t.Errorf("the token was shown:\n%s", out.String())
	}
}

// The owner is read from the workspace's own checkout, not from where dispatch
// was run.
func TestDispatchPicksTheTokenByTheWorkspaceCheckout(t *testing.T) {
	t.Setenv("WORKTENDER_WORKER_GH_TOKEN", "")
	t.Setenv("WORKTENDER_WORKER_GH_TOKEN_STEIG", "github_pat_steig")
	t.Setenv("WORKTENDER_WORKER_GH_TOKEN_LAMPSCOM", "github_pat_lamps")
	repo := herdrtest.NewRepo(t)
	checkout := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "git@github.com:steig/tools.git"}} {
		if out, err := exec.Command("git", append([]string{"-C", checkout}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	server := fakeStart(t, repo, "w9", "w9:p1", "working")
	server.HandleResult("pane.get", map[string]any{"type": "pane_info", "pane": map[string]any{
		"pane_id": "w9:p1", "workspace_id": "w9", "tab_id": "t1",
		"terminal_id": "term_1", "agent_status": "idle", "focused": false, "revision": 1,
	}})
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{{
		"workspace_id": "w9", "label": "w", "number": 1, "focused": false, "pane_count": 1, "tab_count": 1,
		"agent_status": "idle", "active_tab_id": "t1",
		"worktree": map[string]any{"checkout_path": checkout, "is_linked_worktree": true,
			"repo_key": "k", "repo_name": "tools", "repo_root": repo.Root},
	}}})

	if err := dispatchCommand([]string{"--pane", "w9:p1", "--name", "w1"}, io.Discard); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	args := agentStartArgs(t, server)
	if i := slices.Index(args, "--settings"); i < 0 || !strings.HasSuffix(args[i+1], "worker-settings-steig.json") {
		t.Errorf("agent args = %v; want steig's settings file", args)
	}
}
