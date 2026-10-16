package execute_test

import (
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steig/worktender/internal/execute"
	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/herdrtest"
	"github.com/steig/worktender/internal/reconcile"
)

// fixture wires an executor to a fake herdr with no agents, no panes and no
// workspaces.
func fixture(t *testing.T, repo *herdrtest.Repo) (*execute.Executor, *herdrtest.Server) {
	t.Helper()

	server := herdrtest.NewServer(t)
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
	server.HandleResult("pane.list", map[string]any{"type": "pane_list", "panes": []map[string]any{}})
	server.HandleResult("worktree.open", map[string]any{"type": "workspace_created"})
	server.HandleResult("worktree.remove", map[string]any{"type": "worktree_removed"})
	server.HandleResult("agent.start", map[string]any{"type": "agent_started"})

	return &execute.Executor{
		Client: herdrapi.NewWithSocket(server.SocketPath),
		Root:   repo.Root,
	}, server
}

// callTo finds the request a test is actually about.
//
// Indexing into Calls() couples a test to how many guard reads happen to
// precede the call, so adding a re-check breaks assertions about behaviour that
// did not change.
func callTo(t *testing.T, server *herdrtest.Server, method string) herdrtest.Call {
	t.Helper()

	for _, call := range server.Calls() {
		if call.Method == method {
			return call
		}
	}
	t.Fatalf("no %s call was made", method)
	return herdrtest.Call{}
}

func only(t *testing.T, results []execute.Result) execute.Result {
	t.Helper()
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d: %+v", len(results), results)
	}
	return results[0]
}

// mergedWorktree builds a checkout that genuinely is finished.
func mergedWorktree(t *testing.T, repo *herdrtest.Repo, name string) string {
	t.Helper()

	checkout := repo.AddWorktree(name, name)
	repo.CommitIn(checkout, name+".txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge "+name, name)
	return checkout
}

// THE staleness case: the plan said prune, then the worktree picked up
// uncommitted work before the executor got to it.
func TestPruneRefusesWorktreeThatWentDirtyAfterThePlan(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	exec, _ := fixture(t, repo)
	exec.ApplyPrune = true

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout,
		Branch: "done", Reason: "merged into main"}

	// The gap between reconcile and execute: real, uncommitted work appears.
	herdrtest.WriteFile(t, filepath.Join(checkout, "rescue-me.txt"), "unsaved\n")

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusSkipped {
		t.Fatalf("status = %q, want skipped: %s", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "uncommitted changes") {
		t.Errorf("detail should explain the refusal, got %q", result.Detail)
	}
	// The point of the whole exercise: the checkout is still on disk.
	if !repo.Exists(checkout) {
		t.Fatal("the worktree was removed despite having uncommitted work")
	}
}

// The same gap, but an agent starts in it.
func TestPruneRefusesWorktreeThatGainedAnAgentAfterThePlan(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	exec, server := fixture(t, repo)
	exec.ApplyPrune = true

	// herdr now reports an agent in this workspace's pane.
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{{
		"pane_id": "w2:p1", "workspace_id": "w2", "tab_id": "w2:t1", "terminal_id": "t",
		"agent_status": "working", "focused": false, "revision": 1,
	}}})
	server.HandleResult("pane.list", map[string]any{"type": "pane_list",
		"panes": []map[string]any{{"pane_id": "w2:p1"}}})

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout, Branch: "done",
		WorkspaceID: "w2", Reason: "merged into main"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusSkipped {
		t.Fatalf("status = %q, want skipped: %s", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "agent started here") {
		t.Errorf("detail should explain the refusal, got %q", result.Detail)
	}
	if !repo.Exists(checkout) {
		t.Fatal("the worktree was removed while an agent was working in it")
	}
}

// occupy makes the fake herdr report one agent, in the given state, in the one
// pane of workspace w2.
func occupy(server *herdrtest.Server, status string) {
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{{
		"pane_id": "w2:p1", "workspace_id": "w2", "tab_id": "w2:t1", "terminal_id": "t",
		"agent_status": status, "focused": false, "revision": 1,
	}}})
	server.HandleResult("pane.list", map[string]any{"type": "pane_list",
		"panes": []map[string]any{{"pane_id": "w2:p1"}}})
}

// The end state of a successful dispatch: the worker stopped, and still holds
// the pane, because herdr frees an agent only when the pane goes away. Removing
// the worktree closes the workspace, which is what lets go of it — so this only
// happens on an action that asked for it.
func TestPruneReleasesAFinishedAgentWhenTheActionSaysSo(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	exec, server := fixture(t, repo)
	exec.ApplyPrune = true
	occupy(server, "idle")

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout, Branch: "done",
		WorkspaceID: "w2", ReleasesAgents: true, Reason: "PR merged"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusDone {
		t.Fatalf("status = %q, want done: %s", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "releasing the finished agent") {
		t.Errorf("a removal that ends an agent should say so, got %q", result.Detail)
	}
	// Through herdr, or the workspace outlives the checkout and the agent with it.
	if got := callTo(t, server, "worktree.remove").Params["workspace_id"]; got != "w2" {
		t.Errorf("workspace_id = %v, want w2", got)
	}
}

// The same agent, on an action that did not ask. Guard b is unchanged for
// everything that did not opt in.
func TestPruneRefusesAFinishedAgentTheActionDidNotAskToRelease(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	exec, server := fixture(t, repo)
	exec.ApplyPrune = true
	occupy(server, "idle")

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout, Branch: "done",
		WorkspaceID: "w2", Reason: "PR merged"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusSkipped {
		t.Fatalf("status = %q, want skipped: %s", result.Status, result.Detail)
	}
	if !repo.Exists(checkout) {
		t.Fatal("an agent's ground was removed without being asked to release it")
	}
}

// The staleness case the release path adds: the plan was made against an agent
// that had stopped, and it has picked work up since. --release-agents does not
// reach an agent that is doing something, at plan time or at execution time.
func TestPruneRefusesAnAgentThatWentBackToWorkAfterThePlan(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	exec, server := fixture(t, repo)
	exec.ApplyPrune = true
	occupy(server, "working")

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout, Branch: "done",
		WorkspaceID: "w2", ReleasesAgents: true, Reason: "PR merged"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusSkipped {
		t.Fatalf("status = %q, want skipped: %s", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "started working since the plan was made") {
		t.Errorf("detail should explain the refusal, got %q", result.Detail)
	}
	for _, call := range server.Calls() {
		if call.Method == "worktree.remove" {
			t.Fatal("herdr was asked to close a workspace with a working agent in it")
		}
	}
}

// The dry run is what a human reads before applying, so it has to disclose the
// half of the removal the reason does not cover.
func TestDryRunSaysWhenARemovalWouldEndAnAgent(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	exec, server := fixture(t, repo)
	occupy(server, "done")

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout, Branch: "done",
		WorkspaceID: "w2", ReleasesAgents: true, Reason: "PR merged"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusPlanned {
		t.Fatalf("status = %q, want planned: %s", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "releasing the finished agent") {
		t.Errorf("detail = %q, want it to name what else the removal takes", result.Detail)
	}
}

// Pruning is a dry run unless explicitly applied.
func TestPruneIsDryRunByDefault(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	exec, _ := fixture(t, repo)

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout,
		Branch: "done", Reason: "merged into main"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusPlanned {
		t.Fatalf("status = %q, want planned: %s", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "would remove") {
		t.Errorf("a dry run should say what it would do, got %q", result.Detail)
	}
	if !repo.Exists(checkout) {
		t.Fatal("a dry run removed the worktree")
	}
}

func TestPruneAppliesWhenAsked(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	exec, _ := fixture(t, repo)
	exec.ApplyPrune = true

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout,
		Branch: "done", Reason: "merged into main"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusDone {
		t.Fatalf("status = %q, want done: %s", result.Status, result.Detail)
	}
	if repo.Exists(checkout) {
		t.Error("the worktree should be gone")
	}
	// A merged branch is safe to delete, and the report has to say so.
	if !strings.Contains(result.Detail, "deleted branch done") {
		t.Errorf("detail should report the branch deletion, got %q", result.Detail)
	}
}

// Removing the directory the caller is standing in would leave their shell on
// a dead cwd, and a plugin subprocess cannot cd on their behalf.
func TestPruneRefusesTheDirectoryTheCallerIsStandingIn(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	exec, _ := fixture(t, repo)
	exec.ApplyPrune = true
	exec.CallerDir = filepath.Join(checkout, "src")

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout,
		Branch: "done", Reason: "merged into main"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusSkipped {
		t.Fatalf("status = %q, want skipped: %s", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "cd out of it") {
		t.Errorf("detail should tell the user what to do, got %q", result.Detail)
	}
	if !repo.Exists(checkout) {
		t.Fatal("the worktree the caller is standing in was removed")
	}
}

// The asymmetric case, which is the one that happens in production: herdr
// reports resolved paths while the invocation context carries the path the user
// saw, symlinks intact. Comparing them unresolved silently disarms the guard.
func TestPruneRefusesTheCallerDirEvenWhenOnlyOneSideIsResolved(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	// repo.Root keeps its symlinks; RealRoot is what git and herdr report.
	resolved := strings.Replace(checkout, repo.Root, repo.RealRoot, 1)
	if resolved == checkout {
		t.Skip("temp dir is not behind a symlink on this machine")
	}

	exec, _ := fixture(t, repo)
	exec.ApplyPrune = true
	// The action names the checkout as herdr does; the caller sits in the same
	// directory by its unresolved name.
	exec.CallerDir = checkout

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: resolved,
		Branch: "done", Reason: "merged into main"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusSkipped {
		t.Fatalf("the guard must see through the symlink; got %q: %s", result.Status, result.Detail)
	}
	if !repo.Exists(checkout) {
		t.Fatal("the worktree the caller is standing in was removed")
	}
}

// A sibling directory sharing a name prefix is not inside the checkout.
func TestPruneAllowsCallerInASimilarlyNamedSibling(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	exec, _ := fixture(t, repo)
	exec.ApplyPrune = true
	exec.CallerDir = checkout + "-other"

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout,
		Branch: "done", Reason: "merged into main"}

	if result := only(t, exec.Run([]reconcile.Action{action})); result.Status != execute.StatusDone {
		t.Fatalf("status = %q, want done: %s", result.Status, result.Detail)
	}
}

// An unmerged branch is never force-deleted: the work exists nowhere else.
func TestPruneKeepsABranchGitWillNotDelete(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("squashed", "squashed")
	repo.CommitIn(checkout, "sq.txt", "work")
	repo.Git("merge", "--squash", "squashed")
	repo.Git("commit", "-m", "squashed work")

	exec, _ := fixture(t, repo)
	exec.ApplyPrune = true

	// The PR said merged, so the reconciler planned a prune, but git still
	// sees the branch as unmerged because the squash rewrote the commit.
	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout,
		Branch: "squashed", Reason: "PR merged"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusDone {
		t.Fatalf("status = %q, want done: %s", result.Status, result.Detail)
	}
	if repo.Exists(checkout) {
		t.Error("the checkout should be removed")
	}
	if !strings.Contains(result.Detail, "kept branch squashed") {
		t.Errorf("an unmerged branch must be kept and reported, got %q", result.Detail)
	}
	if repo.BranchMissing("squashed") {
		t.Error("the branch was deleted despite git refusing")
	}
}

// With a workspace, removal goes through herdr so the workspace is torn down
// with the checkout.
func TestPruneWithWorkspaceGoesThroughHerdr(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	exec, server := fixture(t, repo)
	exec.ApplyPrune = true

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout, Branch: "done",
		WorkspaceID: "w2", Reason: "merged into main"}

	if result := only(t, exec.Run([]reconcile.Action{action})); result.Status != execute.StatusDone {
		t.Fatalf("status = %q, want done: %s", result.Status, result.Detail)
	}

	var removed *herdrtest.Call
	for _, c := range server.Calls() {
		if c.Method == "worktree.remove" {
			call := c
			removed = &call
		}
	}
	if removed == nil {
		t.Fatal("expected herdr to be asked to remove the worktree")
	}
	if removed.Params["workspace_id"] != "w2" {
		t.Errorf("workspace_id = %v, want w2", removed.Params["workspace_id"])
	}
}

func TestAdoptOpensWorkspaceWithoutFocus(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)

	action := reconcile.Action{Kind: reconcile.KindAdopt, Path: "/repo/wt/a", Branch: "a"}

	if result := only(t, exec.Run([]reconcile.Action{action})); result.Status != execute.StatusDone {
		t.Fatalf("status = %q: %s", result.Status, result.Detail)
	}

	call := server.Calls()[0]
	if call.Method != "worktree.open" {
		t.Fatalf("method = %q, want worktree.open", call.Method)
	}
	// Adopting a batch must not drag the user through every workspace.
	if call.Params["focus"] != false {
		t.Errorf("focus = %v, want false", call.Params["focus"])
	}
	if call.Params["path"] != "/repo/wt/a" {
		t.Errorf("path = %v", call.Params["path"])
	}
}

func TestStaffResumeUsesContinue(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)

	action := reconcile.Action{Kind: reconcile.KindStaff, Path: "/repo/wt/a", Branch: "a",
		WorkspaceID: "w2", PaneID: "w2:p1", AgentName: "a", Resume: true}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusDone {
		t.Fatalf("status = %q: %s", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "resumed") {
		t.Errorf("detail should say it resumed, got %q", result.Detail)
	}

	call := callTo(t, server, "agent.start")
	args, _ := call.Params["args"].([]any)
	if len(args) != 1 || args[0] != "--continue" {
		t.Errorf("args = %v, want [--continue]", call.Params["args"])
	}
	if call.Params["pane_id"] != "w2:p1" {
		t.Errorf("pane_id = %v", call.Params["pane_id"])
	}
}

func TestStaffColdStartSendsNoArgs(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)

	action := reconcile.Action{Kind: reconcile.KindStaff, Path: "/repo/wt/a",
		PaneID: "w2:p1", AgentName: "a"}

	if result := only(t, exec.Run([]reconcile.Action{action})); result.Status != execute.StatusDone {
		t.Fatalf("status = %q: %s", result.Status, result.Detail)
	}
	if _, ok := callTo(t, server, "agent.start").Params["args"]; ok {
		t.Error("a cold start should not pass --continue")
	}
}

// A pane still running direnv rejects the agent. That has to surface as a
// reported failure, not a silent no-op.
// The staffing twin of the prune staleness cases, and the reason staffing
// re-reads its guard at all.
//
// The reconciler decides to staff a workspace that has no agent, and that
// snapshot ages. An agent starting in the gap is the common case, not a corner:
// an event hook and a human `sync` can plan from the same state moments apart.
// Starting a second agent on a pane that already has one does not fail
// harmlessly — it lands on a live conversation.
func TestStaffRefusesAPaneThatGainedAnAgentAfterThePlan(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)

	// An agent appeared in the workspace between the plan and this moment.
	server.HandleResult("agent.list", map[string]any{"type": "agent_list",
		"agents": []map[string]any{{"name": "already-here", "pane_id": "w2:p1"}}})
	server.HandleResult("pane.list", map[string]any{"type": "pane_list",
		"panes": []map[string]any{{"pane_id": "w2:p1"}}})

	action := reconcile.Action{Kind: reconcile.KindStaff, Path: "/repo/wt/a", Branch: "a",
		WorkspaceID: "w2", PaneID: "w2:p1", AgentName: "a"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusSkipped {
		t.Errorf("status = %q, want skipped: %s", result.Status, result.Detail)
	}
	for _, call := range server.Calls() {
		if call.Method == "agent.start" {
			t.Fatal("started a second agent on a pane that already had one")
		}
	}
	if !strings.Contains(result.Detail, "agent") {
		t.Errorf("the refusal should explain itself, got %q", result.Detail)
	}
}

// Staffing must still happen when the workspace really is empty; a guard that
// refuses everything is not a guard.
func TestStaffProceedsWhenTheWorkspaceIsStillIdle(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)

	server.HandleResult("pane.list", map[string]any{"type": "pane_list",
		"panes": []map[string]any{{"pane_id": "w2:p1"}}})

	action := reconcile.Action{Kind: reconcile.KindStaff, Path: "/repo/wt/a", Branch: "a",
		WorkspaceID: "w2", PaneID: "w2:p1", AgentName: "a"}

	if result := only(t, exec.Run([]reconcile.Action{action})); result.Status != execute.StatusDone {
		t.Fatalf("status = %q: %s", result.Status, result.Detail)
	}
	for _, call := range server.Calls() {
		if call.Method == "agent.start" {
			return
		}
	}
	t.Error("an idle workspace was never staffed")
}

// A workspace herdr cannot be asked about must not be staffed on the assumption
// that it is idle. The same call already blocks a prune for the same reason:
// an unverifiable guard is not a satisfied one.
func TestStaffRefusesWhenTheWorkspaceCannotBeChecked(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)

	server.Handle("agent.list", func(map[string]any) (any, error) {
		return nil, errStartFailed{}
	})

	action := reconcile.Action{Kind: reconcile.KindStaff, Path: "/repo/wt/a",
		WorkspaceID: "w2", PaneID: "w2:p1", AgentName: "a"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusSkipped {
		t.Errorf("status = %q, want skipped: %s", result.Status, result.Detail)
	}
	for _, call := range server.Calls() {
		if call.Method == "agent.start" {
			t.Fatal("staffed a workspace whose state could not be confirmed")
		}
	}
}

func TestStaffReportsAFailureToStart(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)

	server.Handle("agent.start", func(map[string]any) (any, error) {
		return nil, errStartFailed{}
	})

	action := reconcile.Action{Kind: reconcile.KindStaff, Path: "/repo/wt/a",
		PaneID: "w2:p1", AgentName: "a"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	if !strings.Contains(result.Detail, "w2:p1") {
		t.Errorf("detail should name the pane, got %q", result.Detail)
	}
}

type errStartFailed struct{}

func (errStartFailed) Error() string { return "pane is busy" }

// busyUntil makes agent.start refuse the way herdr does until the nth attempt,
// with zero meaning a pane that never settles. It returns how many attempts
// were made.
func busyUntil(server *herdrtest.Server, succeedOn int) func() int {
	var attempts atomic.Int64
	server.Handle("agent.start", func(map[string]any) (any, error) {
		if n := int(attempts.Add(1)); succeedOn <= 0 || n < succeedOn {
			return nil, &herdrtest.CodedError{
				Code: "agent_pane_busy", Message: "agent target pane w2:p1 is not an available shell"}
		}
		return map[string]any{"type": "agent_started"}, nil
	})
	return func() int { return int(attempts.Load()) }
}

func staffAction() reconcile.Action {
	return reconcile.Action{Kind: reconcile.KindStaff, Path: "/repo/wt/a", Branch: "a",
		WorkspaceID: "w2", PaneID: "w2:p1", AgentName: "a"}
}

// A worktree seconds old is still running direnv, nix or a login banner, and
// herdr will not start an agent against it. It was believed that agent.start's
// timeout_ms covered this — the constant said so — and it does not: measured
// against protocol 17, agent.start on an occupied pane answers agent_pane_busy
// in 1.6-3.0ms with timeout_ms unset, 1000, 60000 and 120000 alike. So the
// waiting happens here.
func TestStaffWaitsOutAPaneThatIsStillBusy(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)
	exec.BusyRetryFor = 5 * time.Second
	attempts := busyUntil(server, 3)

	result := only(t, exec.Run([]reconcile.Action{staffAction()}))
	if result.Status != execute.StatusDone {
		t.Fatalf("a pane that settled was never staffed; got %q: %s", result.Status, result.Detail)
	}
	if got := attempts(); got != 3 {
		t.Errorf("agent.start was attempted %d times, want 3", got)
	}
	// A staffing that took two retries and one that took none are not the same
	// event, and the report is the only place that shows.
	if !strings.Contains(result.Detail, "after waiting") {
		t.Errorf("the report should say a wait happened, got %q", result.Detail)
	}
}

// The retry is scoped to the one code that a later attempt can change. Every
// other failure describes something waiting cannot fix, and retrying it would
// turn a prompt error into a minute of silence.
func TestStaffDoesNotRetryAFailureThatIsNotABusyPane(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)
	exec.BusyRetryFor = time.Minute

	var attempts atomic.Int64
	server.Handle("agent.start", func(map[string]any) (any, error) {
		attempts.Add(1)
		return nil, &herdrtest.CodedError{Code: "agent_name_taken", Message: "that name is in use"}
	})

	done := make(chan execute.Result, 1)
	go func() { done <- only(t, exec.Run([]reconcile.Action{staffAction()})) }()

	select {
	case result := <-done:
		if result.Status != execute.StatusFailed {
			t.Fatalf("status = %q, want failed", result.Status)
		}
		if got := attempts.Load(); got != 1 {
			t.Errorf("agent.start was attempted %d times, want 1", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a failure that is not agent_pane_busy was retried")
	}
}

// A pane busy for longer than the budget still fails, and says what it waited
// for — "not an available shell" on its own reads as a bug in the caller.
func TestStaffGivesUpOnAPaneThatNeverSettles(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)
	exec.BusyRetryFor = 700 * time.Millisecond
	attempts := busyUntil(server, 0) // never succeeds

	result := only(t, exec.Run([]reconcile.Action{staffAction()}))
	if result.Status != execute.StatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	if attempts() < 2 {
		t.Errorf("gave up after %d attempts without retrying at all", attempts())
	}
	for _, want := range []string{"still busy", "direnv", "attempts"} {
		if !strings.Contains(result.Detail, want) {
			t.Errorf("the detail should mention %q, got %q", want, result.Detail)
		}
	}
}

// The budget is spent by the run, not by each action in it. Worktrees warm up
// in wall-clock time and not in turn, and a per-action wait would let a sync
// over five of them hold the repository lock for five times AgentStartTimeout —
// which is repolock.MaxHold exactly, so the holder would be evicted mid-pass.
func TestStaffingSeveralBusyWorktreesDoesNotMultiplyTheWait(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)
	exec.BusyRetryFor = time.Second
	busyUntil(server, 0) // none of them ever settles

	actions := make([]reconcile.Action, 4)
	for i := range actions {
		actions[i] = staffAction()
	}

	start := time.Now()
	results := exec.Run(actions)
	elapsed := time.Since(start)

	for _, r := range results {
		if r.Status != execute.StatusFailed {
			t.Fatalf("status = %q, want failed: %s", r.Status, r.Detail)
		}
	}
	// One budget, not four. The margin is for the last action's own attempt.
	if elapsed > 2*exec.BusyRetryFor {
		t.Errorf("staffing 4 busy worktrees took %s, want about %s", elapsed, exec.BusyRetryFor)
	}
}

// Waiting a minute for a pane is a minute in which somebody else can staff the
// workspace, so the guard the file opens with — never land on a live
// conversation — is re-read every attempt, not once before the wait.
func TestStaffRecheckstheWorkspaceGuardWhileItWaits(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)
	exec.BusyRetryFor = 10 * time.Second
	attempts := busyUntil(server, 0) // the pane stays busy throughout

	// An agent appears in the workspace after the first refusal.
	var listed atomic.Int64
	server.Handle("agent.list", func(map[string]any) (any, error) {
		if listed.Add(1) == 1 {
			return map[string]any{"type": "agent_list", "agents": []map[string]any{}}, nil
		}
		return map[string]any{"type": "agent_list", "agents": []map[string]any{
			{"terminal_id": "term_1", "agent": "claude", "agent_status": "working",
				"workspace_id": "w2", "tab_id": "t1", "pane_id": "w2:p1", "focused": false, "revision": 1},
		}}, nil
	})
	server.HandleResult("pane.list", map[string]any{"type": "pane_list", "panes": []map[string]any{
		{"pane_id": "w2:p1", "workspace_id": "w2", "tab_id": "t1", "index": 0},
	}})

	result := only(t, exec.Run([]reconcile.Action{staffAction()}))
	if result.Status != execute.StatusSkipped {
		t.Fatalf("status = %q, want skipped: the workspace was staffed while this waited", result.Status)
	}
	if attempts() != 1 {
		t.Errorf("agent.start was attempted %d times after the workspace filled up, want 1", attempts())
	}
}

// The slow-direnv case, end to end. A freshly created worktree can still be
// running direnv or nix when staffing is attempted, which is why agent.start
// asks herdr to wait. A client deadline shorter than that wait would abort the
// call first and report a hard failure on exactly the case the wait exists for.
func TestStaffSurvivesAServerSlowerThanTheOrdinaryDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("takes ~6s of real time by design")
	}

	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)

	// Longer than the 5s an ordinary call allows, well under the 60s that
	// staffing asks herdr to wait.
	server.HandleSlow("agent.start", 6*time.Second, map[string]any{"type": "agent_started"})

	action := reconcile.Action{Kind: reconcile.KindStaff, Path: "/repo/wt/a",
		PaneID: "w2:p1", AgentName: "a"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusDone {
		t.Fatalf("staffing must outlast the wait it asked herdr for; got %q: %s",
			result.Status, result.Detail)
	}
}

// The other half: an ordinary call keeps its short deadline, so a wedged herdr
// still fails rather than hanging a plugin action forever.
func TestOrdinaryCallStillTimesOut(t *testing.T) {
	if testing.Short() {
		t.Skip("takes ~5s of real time by design")
	}

	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)

	server.HandleSlow("worktree.open", 8*time.Second, map[string]any{"type": "workspace_created"})

	action := reconcile.Action{Kind: reconcile.KindAdopt, Path: "/repo/wt/a", Branch: "a"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusFailed {
		t.Fatalf("a wedged server should fail the call, got %q: %s", result.Status, result.Detail)
	}
}

// Keep actions are explanatory and must reach the report.
func TestKeepIsReportedNotExecuted(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)

	action := reconcile.Action{Kind: reconcile.KindKeep, Path: "/repo/wt/a",
		Branch: "a", Reason: "agent running"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusSkipped {
		t.Errorf("status = %q, want skipped", result.Status)
	}
	if result.Detail != "agent running" {
		t.Errorf("detail = %q, want the keep reason", result.Detail)
	}
	if len(server.Calls()) != 0 {
		t.Errorf("a keep must not talk to herdr: %+v", server.Calls())
	}
}

// The dry-run report IS the confirmation surface for `prune --apply` — there is
// no prompt to replace it. A branch name carrying a bidi override draws as a
// different branch, so it spoofs the confirmation rather than merely looking
// odd. The branch reaches the report twice, as the target column and inside the
// detail; both have to be escaped or the spoof just moves one column right.
func TestRenderEscapesABranchNameThatDrawsAsAnother(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	repo.Git("branch", "--", "evil‮hctap")
	branch := repo.Git("branch", "--list", "--format=%(refname:short)", "evil‮hctap")

	report := execute.Render([]execute.Result{{
		Status: execute.StatusPlanned,
		Action: reconcile.Action{Kind: reconcile.KindPrune, Path: "/repo/wt/evil", Branch: branch},
		Detail: "merged into main; would delete branch " + branch,
	}})

	if strings.ContainsRune(report, '‮') {
		t.Errorf("the override reached the terminal: %q", report)
	}
	if strings.Count(report, `evil\u{202E}hctap`) != 2 {
		t.Errorf("both the target and the detail must name the real branch, got %q", report)
	}
}

// Every result carries a detail — a silent no-op is the failure mode here.
func TestEveryResultExplainsItself(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")
	exec, _ := fixture(t, repo)

	results := exec.Run([]reconcile.Action{
		{Kind: reconcile.KindAdopt, Path: "/repo/wt/a", Branch: "a"},
		{Kind: reconcile.KindStaff, Path: "/repo/wt/b", PaneID: "w3:p1", AgentName: "b"},
		{Kind: reconcile.KindPrune, Path: checkout, Branch: "done", Reason: "merged into main"},
		{Kind: reconcile.KindKeep, Path: "/repo/wt/c", Reason: "still open"},
	})

	if len(results) != 4 {
		t.Fatalf("want 4 results, got %d", len(results))
	}
	for _, r := range results {
		if strings.TrimSpace(r.Detail) == "" {
			t.Errorf("%s action produced no explanation: %+v", r.Action.Kind, r)
		}
	}

	report := execute.Render(results)
	if strings.Count(strings.TrimRight(report, "\n"), "\n") != 3 {
		t.Errorf("report should have one line per action:\n%s", report)
	}
	if execute.Counts(results)[execute.StatusPlanned] != 1 {
		t.Errorf("expected one planned prune, got %v", execute.Counts(results))
	}
}

// workspaceJSON is one entry of a workspace.list reply, holding a checkout open.
func workspaceJSON(id, checkout string) map[string]any {
	return map[string]any{
		"workspace_id": id, "number": 2, "label": filepath.Base(checkout), "focused": false,
		"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "working",
		"worktree": map[string]any{"repo_key": "k", "repo_name": "repo",
			"repo_root": "/repo", "checkout_path": checkout, "is_linked_worktree": true},
	}
}

// THE #13 case. An empty workspace id on a prune action is not evidence that no
// workspace holds the checkout — it is just as likely to be a join that missed,
// which is exactly how a live agent's ground gets removed. herdr is asked
// again, and it says the checkout is held by a workspace with an agent in it.
func TestPruneRefusesWhenHerdrHoldsTheCheckoutTheActionNamesNoWorkspaceFor(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	exec, server := fixture(t, repo)
	exec.ApplyPrune = true

	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list",
		"workspaces": []map[string]any{workspaceJSON("w2", checkout)}})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{{
		"pane_id": "w2:p1", "workspace_id": "w2", "tab_id": "w2:t1", "terminal_id": "t",
		"agent_status": "working", "focused": false, "revision": 1,
	}}})
	server.HandleResult("pane.list", map[string]any{"type": "pane_list",
		"panes": []map[string]any{{"pane_id": "w2:p1"}}})

	// The plan's join missed, so it carries no workspace id at all.
	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout,
		Branch: "done", Reason: "PR merged"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusSkipped {
		t.Fatalf("status = %q, want skipped: %s", result.Status, result.Detail)
	}
	if !repo.Exists(checkout) {
		t.Fatal("a checkout hosting a live agent was removed because the plan named no workspace")
	}
}

// The same missing workspace id, spelled differently by herdr. Re-asking is
// worth nothing if the answer is compared by raw string equality.
func TestPruneRefusesWhenHerdrSpellsTheHeldCheckoutDifferently(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	resolved := strings.Replace(checkout, repo.Root, repo.RealRoot, 1)
	if resolved == checkout {
		t.Skip("temp dir is not behind a symlink on this machine")
	}

	exec, server := fixture(t, repo)
	exec.ApplyPrune = true

	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list",
		"workspaces": []map[string]any{workspaceJSON("w2", resolved)}})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{{
		"pane_id": "w2:p1", "workspace_id": "w2", "tab_id": "w2:t1", "terminal_id": "t",
		"agent_status": "working", "focused": false, "revision": 1,
	}}})
	server.HandleResult("pane.list", map[string]any{"type": "pane_list",
		"panes": []map[string]any{{"pane_id": "w2:p1"}}})

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout,
		Branch: "done", Reason: "PR merged"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusSkipped {
		t.Fatalf("status = %q, want skipped: %s", result.Status, result.Detail)
	}
	if !repo.Exists(checkout) {
		t.Fatal("a checkout hosting a live agent was removed over a symlink difference")
	}
}

// An unverifiable guard is not a satisfied one: herdr cannot say whether a
// workspace holds this checkout, so the removal does not happen.
func TestPruneRefusesWhenTheWorkspaceListCannotBeRead(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	exec, server := fixture(t, repo)
	exec.ApplyPrune = true
	// workspace.list deliberately errors.
	server.Handle("workspace.list", func(map[string]any) (any, error) {
		return nil, errStartFailed{}
	})

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout,
		Branch: "done", Reason: "PR merged"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusSkipped {
		t.Fatalf("status = %q, want skipped: %s", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "could not confirm") {
		t.Errorf("detail should say the guard could not be checked, got %q", result.Detail)
	}
	if !repo.Exists(checkout) {
		t.Fatal("a checkout was removed on an unverifiable guard")
	}
}

// A checkout herdr genuinely holds no workspace for is still prunable: the
// re-check is a verification, not a new refusal.
func TestPruneProceedsWhenHerdrHoldsNoWorkspaceForTheCheckout(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	exec, server := fixture(t, repo)
	exec.ApplyPrune = true
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list",
		"workspaces": []map[string]any{workspaceJSON("w9", "/somewhere/else")}})

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout,
		Branch: "done", Reason: "PR merged"}

	if result := only(t, exec.Run([]reconcile.Action{action})); result.Status != execute.StatusDone {
		t.Fatalf("status = %q, want done: %s", result.Status, result.Detail)
	}
	if repo.Exists(checkout) {
		t.Error("the worktree should be gone")
	}
}

// Having re-discovered the workspace, the removal goes through herdr: a git
// removal would leave herdr holding a workspace over a checkout that is no
// longer there.
func TestPruneRemovesThroughTheWorkspaceItRediscovered(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	exec, server := fixture(t, repo)
	exec.ApplyPrune = true
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list",
		"workspaces": []map[string]any{workspaceJSON("w2", checkout)}})

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout,
		Branch: "done", Reason: "PR merged"}

	if result := only(t, exec.Run([]reconcile.Action{action})); result.Status != execute.StatusDone {
		t.Fatalf("status = %q, want done: %s", result.Status, result.Detail)
	}
	if got := callTo(t, server, "worktree.remove").Params["workspace_id"]; got != "w2" {
		t.Errorf("workspace_id = %v, want the rediscovered w2", got)
	}
}

// The other half of the same rule, one call further in. workspace.list answers,
// so a workspace IS found for this checkout — and then agent.list fails, leaving
// "is anything running in it" unanswerable. An unverifiable guard is not a
// satisfied one on the destructive path either.
func TestPruneRefusesWhenTheAgentListCannotBeRead(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	exec, server := fixture(t, repo)
	exec.ApplyPrune = true
	server.Handle("agent.list", func(map[string]any) (any, error) {
		return nil, errStartFailed{}
	})

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout,
		Branch: "done", WorkspaceID: "w2", Reason: "PR merged"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusSkipped {
		t.Fatalf("status = %q, want skipped: %s", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "could not confirm") {
		t.Errorf("detail should say the guard could not be checked, got %q", result.Detail)
	}
	if !repo.Exists(checkout) {
		t.Fatal("a checkout was removed without confirming it was idle")
	}
}

// THE CLAIMED DECIDE-EXECUTE RACE, tested rather than argued. A competitive
// analysis of worktrunk said we "have exactly this race" — Collect gathers
// facts, Reconcile decides, the executor acts, and a commit landing in that
// window is deleted anyway — and proposed compare-and-swap branch deletion
// (`git update-ref -d` with an expected SHA) as the fix.
//
// The premise is wrong for the branch half, and this is what says so. We do not
// delete with `-D`. `git branch -d` evaluates merged-ness at the moment it runs,
// not at the moment the plan was made, so a commit that lands in the window is
// exactly what it refuses on.
func TestPruneKeepsABranchThatGainedACommitAfterThePlan(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "raced")

	// The window: the plan was made against a merged branch, and then work
	// landed on it. Committed, so the dirty guard does not mask the question.
	repo.CommitIn(checkout, "late.txt", "landed after the decision")

	exec, _ := fixture(t, repo)
	exec.ApplyPrune = true

	result := only(t, exec.Run([]reconcile.Action{{
		Kind: reconcile.KindPrune, Path: checkout,
		Branch: "raced", Reason: "PR merged",
	}}))

	if result.Status != execute.StatusDone {
		t.Fatalf("status = %q, want done: %s", result.Status, result.Detail)
	}
	if repo.BranchMissing("raced") {
		t.Fatal("the branch was deleted with a commit base does not have — the race is real and needs a CAS")
	}
	if !strings.Contains(result.Detail, "kept branch raced") {
		t.Errorf("keeping the branch has to be reported, got %q", result.Detail)
	}
}

// The executor with no herdr to ask. Nothing else in the suite runs an action
// through pruneBlocked with a nil client, which is the path a plain-shell
// prune-apply takes end to end: the re-check that would ask herdr which
// workspace holds the checkout has nobody to ask, and falls through to git.
func TestPruneWithoutHerdrRemovesThroughGit(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	exec := &execute.Executor{Client: nil, Root: repo.Root, ApplyPrune: true}
	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout,
		Branch: "done", Reason: "merged into main"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusDone {
		t.Fatalf("status = %q, want done: %s", result.Status, result.Detail)
	}
	if repo.Exists(checkout) {
		t.Error("the worktree should be gone")
	}
}

// The guards that protect work are git's, and a nil client does not reach them.
// This is the claim the README makes when it says the guards that matter are
// untouched, and it is the one that keeps the degraded path from being a way to
// delete uncommitted work.
func TestPruneWithoutHerdrStillRefusesADirtyCheckout(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")
	herdrtest.WriteFile(t, filepath.Join(checkout, "scratch.txt"), "unsaved")

	exec := &execute.Executor{Client: nil, Root: repo.Root, ApplyPrune: true}
	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout,
		Branch: "done", Reason: "merged into main"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusSkipped {
		t.Fatalf("status = %q, want skipped: %s", result.Status, result.Detail)
	}
	if !repo.Exists(checkout) {
		t.Fatal("uncommitted work was removed")
	}
}

// A ghost's close re-reads what can change between the plan and the act: an
// agent picking work back up, or a checkout reappearing at the path. Either
// leaves the workspace open.
func TestCloseGhostRechecksBeforeClosing(t *testing.T) {
	for _, tc := range []struct {
		name, agent string
		reappear    bool
		want        execute.Status
	}{
		{"finished agent", "idle", false, execute.StatusDone},
		{"agent went back to work", "working", false, execute.StatusSkipped},
		{"checkout reappeared", "idle", true, execute.StatusSkipped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := herdrtest.NewRepo(t)
			path := filepath.Join(t.TempDir(), "gone")
			if tc.reappear {
				herdrtest.WriteFile(t, filepath.Join(path, "x"), "x")
			}

			exec, server := fixture(t, repo)
			exec.ApplyPrune = true
			occupy(server, tc.agent)
			server.HandleResult("workspace.close", map[string]any{"type": "ok"})

			action := reconcile.Action{Kind: reconcile.KindCloseGhost, Path: path,
				WorkspaceID: "w2", ReleasesAgents: true, Reason: "PR #240 merged"}
			result := only(t, exec.Run([]reconcile.Action{action}))
			if result.Status != tc.want {
				t.Fatalf("status = %q, want %q: %s", result.Status, tc.want, result.Detail)
			}

			var closes int
			for _, call := range server.Calls() {
				if call.Method == "workspace.close" {
					closes++
				}
			}
			if want := map[bool]int{true: 1, false: 0}[tc.want == execute.StatusDone]; closes != want {
				t.Errorf("workspace.close called %d time(s), want %d", closes, want)
			}
		})
	}
}
