package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrtest"
	"github.com/steig/worktender/internal/reconcile"
	"github.com/steig/worktender/internal/repolock"
)

// restoredRepo is a repository as herdr leaves it after a restart: the main
// checkout is back as a workspace, and a linked worktree that appeared while
// herdr was down has none. That gap is the whole reason startup exists — no
// event fired for it, because nothing was running to fire one.
type restoredRepo struct {
	repo     *herdrtest.Repo
	checkout string
	// workspaceID is the restored workspace holding the main checkout, which is
	// what puts this repository in startup's scope.
	workspaceID string
}

func newRestoredRepo(t *testing.T, slug, branch, workspaceID string) restoredRepo {
	t.Helper()

	repo := herdrtest.NewRepo(t)
	return restoredRepo{repo: repo, checkout: repo.AddWorktree(slug, branch), workspaceID: workspaceID}
}

// mainWorkspace is the workspace.list entry for the restored main checkout. It
// is deliberately the MAIN checkout: is_linked_worktree is false, so it is never
// staffed, and its only job is to name the repository.
func (r restoredRepo) mainWorkspace() map[string]any {
	return map[string]any{
		"workspace_id": r.workspaceID, "number": 1, "label": filepath.Base(r.repo.Root),
		"focused": false, "pane_count": 1, "tab_count": 1, "active_tab_id": "t1",
		"agent_status": "unknown",
		"worktree": map[string]any{
			"repo_key": "k", "repo_name": filepath.Base(r.repo.Root),
			"repo_root": r.repo.Root, "checkout_path": r.repo.Root,
			"is_linked_worktree": false,
		},
	}
}

// matches reports whether a worktree.list cwd names this repository. herdr
// reports resolved paths and the test repo is handed out unresolved, so both
// spellings have to be accepted.
func (r restoredRepo) matches(cwd string) bool {
	return cwd == r.repo.Root || cwd == r.repo.RealRoot || cwd == gitx.Resolve(r.repo.Root)
}

// startupSession wires a fake herdr that serves every given repository at once,
// which is the shape startup actually faces: workspace.list is global, while
// worktree.list is asked once per repository.
//
// HERDR_PLUGIN_CONTEXT_JSON is deliberately left unset. At server start nobody
// is standing anywhere, so startup must not need a context — and a startup path
// that quietly depended on one would only fail on a real launch.
func startupSession(t *testing.T, repos ...restoredRepo) *herdrtest.Server {
	t.Helper()

	server := herdrtest.NewServer(t)
	t.Setenv("HERDR_SOCKET_PATH", server.SocketPath)
	t.Setenv("HERDR_PLUGIN_CONTEXT_JSON", "")
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())

	workspaces := make([]map[string]any, 0, len(repos))
	for _, r := range repos {
		workspaces = append(workspaces, r.mainWorkspace())
	}
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": workspaces})

	server.Handle("worktree.list", func(params map[string]any) (any, error) {
		cwd, _ := params["cwd"].(string)
		for _, r := range repos {
			if r.matches(cwd) {
				return worktreeListReply(r.repo, r.checkout, filepath.Base(r.checkout), ""), nil
			}
		}
		return nil, errors.New("worktree.list for an unknown repository: " + cwd)
	})

	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})
	server.HandleResult("pane.list", map[string]any{"type": "pane_list", "panes": []map[string]any{}})
	server.HandleResult("worktree.open", map[string]any{"type": "workspace_created"})
	return server
}

// adopted returns the checkout paths startup asked herdr to open.
func adopted(server *herdrtest.Server) []string {
	var paths []string
	for _, call := range server.Calls() {
		if call.Method != "worktree.open" {
			continue
		}
		if path, ok := call.Params["path"].(string); ok {
			paths = append(paths, path)
		}
	}
	return paths
}

// The safety property, and the reason this test is first — the same one the
// event hooks carry. A [[startup]] entry is armed the moment the manifest is
// saved, and this one fires on every launch across every open repository, which
// makes it the louder of the two autonomous triggers rather than the quieter.
func TestStartupIsOffByDefault(t *testing.T) {
	r := newRestoredRepo(t, "wip", "wip", "w1")
	server := startupSession(t, r)
	t.Setenv(eventsEnv, "")

	var out strings.Builder
	if err := startupCommand(nil, &out); err != nil {
		t.Fatalf("a disabled startup command must exit 0, not fail: %v", err)
	}

	for _, method := range []string{"worktree.open", "agent.start", "worktree.remove"} {
		if called(t, server, method) {
			t.Errorf("startup called %s while the opt-in was unset", method)
		}
	}
	if !strings.Contains(out.String(), "WORKTENDER_EVENTS") {
		t.Errorf("a disabled startup command must say why it did nothing, got: %q", out.String())
	}
}

// Startup shares the opt-in, so it must share the rename notice too — it is the
// trigger that fires on every launch across every repository, and the one most
// likely to be the first thing a stale opt-in fails to arm.
func TestStartupRefusesTheOldEnvNameAndSaysSo(t *testing.T) {
	for _, legacy := range legacyEventsEnvs {
		t.Run(legacy, func(t *testing.T) {
			r := newRestoredRepo(t, "wip", "wip", "w1")
			server := startupSession(t, r)
			t.Setenv(eventsEnv, "")
			t.Setenv(legacy, "1")

			var out strings.Builder
			if err := startupCommand(nil, &out); err != nil {
				t.Fatalf("the old name must decline, not fail: %v", err)
			}

			for _, method := range []string{"worktree.open", "agent.start", "worktree.remove"} {
				if called(t, server, method) {
					t.Errorf("the old env name enabled %s; it must not be an alias", method)
				}
			}
			for _, want := range []string{legacy, eventsEnv} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("declining on the old name must mention %s, got: %q", want, out.String())
				}
			}
		})
	}
}

// The gap startup exists to close: a worktree that appeared while herdr was not
// running, so no event was ever delivered for it.
func TestStartupAdoptsAWorktreeThatAppearedWhileHerdrWasDown(t *testing.T) {
	r := newRestoredRepo(t, "wip", "wip", "w1")
	server := startupSession(t, r)
	t.Setenv(eventsEnv, "1")

	var out strings.Builder
	if err := startupCommand(nil, &out); err != nil {
		t.Fatalf("startup: %v", err)
	}

	if !slices.Contains(adopted(server), r.checkout) {
		t.Errorf("startup did not adopt %s; output:\n%s", r.checkout, out.String())
	}
}

// herdr restores every workspace, not the one you were last in, so startup is
// scoped to all of them. A pass that reconciled only one repository would leave
// the rest waiting for someone to remember `sync`.
func TestStartupReconcilesEveryOpenRepository(t *testing.T) {
	first := newRestoredRepo(t, "alpha", "alpha", "w1")
	second := newRestoredRepo(t, "beta", "beta", "w2")
	server := startupSession(t, first, second)
	t.Setenv(eventsEnv, "1")

	var out strings.Builder
	if err := startupCommand(nil, &out); err != nil {
		t.Fatalf("startup: %v", err)
	}

	opened := adopted(server)
	for _, want := range []string{first.checkout, second.checkout} {
		if !slices.Contains(opened, want) {
			t.Errorf("startup did not adopt %s; adopted %v\noutput:\n%s", want, opened, out.String())
		}
	}
}

// Two workspaces on one repository must not become two reconciles of it. This is
// the same coalescing the event path does, applied to startup's own scope.
func TestStartupReconcilesEachRepositoryOnce(t *testing.T) {
	r := newRestoredRepo(t, "wip", "wip", "w1")

	// A second workspace, on the SAME repository — a linked worktree already
	// held open. herdr restores both, so workspace.list names the repo twice.
	extra := r.mainWorkspace()
	extra["workspace_id"] = "w2"
	extra["worktree"].(map[string]any)["checkout_path"] = r.checkout
	extra["worktree"].(map[string]any)["is_linked_worktree"] = true

	server := herdrtest.NewServer(t)
	t.Setenv("HERDR_SOCKET_PATH", server.SocketPath)
	t.Setenv("HERDR_PLUGIN_CONTEXT_JSON", "")
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list",
		"workspaces": []map[string]any{r.mainWorkspace(), extra}})
	server.HandleResult("worktree.list", worktreeListReply(r.repo, r.checkout, "wip", "w2"))
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})
	server.HandleResult("pane.list", map[string]any{"type": "pane_list", "panes": []map[string]any{}})
	t.Setenv(eventsEnv, "1")

	var out strings.Builder
	if err := startupCommand(nil, &out); err != nil {
		t.Fatalf("startup: %v", err)
	}

	var lists int
	for _, call := range server.Calls() {
		if call.Method == "worktree.list" {
			lists++
		}
	}
	if lists != 1 {
		t.Errorf("asked herdr for the worktree list %d times for one repository, want 1", lists)
	}
}

// The claim that makes this a replacement for `wt watch` rather than a slower
// copy of it: startup terminates. The coalescing loop re-runs while work keeps
// being marked, so a repository that is marked dirty on every pass is exactly
// the input that would spin forever without a cap — which is a poll loop with
// no sleep in it.
func TestStartupTerminatesEvenWhileWorkKeepsArriving(t *testing.T) {
	r := newRestoredRepo(t, "wip", "wip", "w1")
	server := startupSession(t, r)
	state := t.TempDir()
	t.Setenv("HERDR_PLUGIN_STATE_DIR", state)
	t.Setenv(eventsEnv, "1")

	// Mark the repository from "another handler" on every pass, so the loop is
	// never allowed to settle.
	var lists int
	server.Handle("worktree.list", func(map[string]any) (any, error) {
		lists++
		if err := repolock.MarkDirty(state, gitx.Resolve(r.repo.Root)); err != nil {
			t.Errorf("mark dirty: %v", err)
		}
		return worktreeListReply(r.repo, r.checkout, "wip", ""), nil
	})

	if err := startupCommand(nil, io.Discard); err != nil {
		t.Fatalf("startup: %v", err)
	}

	// The cap plus the one staffing pass that follows an adoption. That pass is
	// deliberately outside the loop: inside it, continuous marking like this
	// could starve it, and staffing is the whole point of having adopted.
	if want := reconcilePasses + 1; lists != want {
		t.Errorf("ran %d pass(es) under continuous marking, want exactly %d "+
			"(the %d-pass cap and the staffing pass after an adoption)", lists, want, reconcilePasses)
	}
}

// The specific thing issue #3 exists to delete. `wt watch` cost one GitHub API
// call per worktree per cycle; a startup pass that made the same call once per
// worktree would be that loop at a lower rate, not the end of it. PR state only
// authorises a prune, and startup never prunes, so it must not be asked for.
func TestStartupMakesNoGhCalls(t *testing.T) {
	r := newRestoredRepo(t, "wip", "wip", "w1")
	startupSession(t, r)

	sentinel := filepath.Join(t.TempDir(), "gh-was-called")
	herdrtest.FakeGh(t, "touch "+sentinel+"; "+herdrtest.GhPRScript("OPEN"))
	t.Setenv(eventsEnv, "1")

	if err := startupCommand(nil, io.Discard); err != nil {
		t.Fatalf("startup: %v", err)
	}

	if _, err := os.Stat(sentinel); err == nil {
		t.Error("startup shelled out to gh; it must make no network calls")
	}
}

// Startup is the moment with the least information about what anyone intends, so
// it is the last place that should be removing checkouts.
func TestStartupNeverPrunes(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("done", "done")
	repo.CommitIn(checkout, "done.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")

	// An authoritative merged verdict — the one thing that CAN justify a prune.
	herdrtest.FakeGhPRState(t, "MERGED")

	r := restoredRepo{repo: repo, checkout: checkout, workspaceID: "w1"}
	server := startupSession(t, r)
	t.Setenv(eventsEnv, "1")

	var out strings.Builder
	if err := startupCommand(nil, &out); err != nil {
		t.Fatalf("startup: %v", err)
	}

	if called(t, server, "worktree.remove") {
		t.Error("startup asked herdr to remove a worktree")
	}
	if !repo.Exists(checkout) {
		t.Fatal("startup removed a worktree")
	}
	// Not removing it is the weaker half: the executor's dry run and the absent
	// PR lookup each deliver that on their own, so passing there says nothing
	// about the filter. What the filter is responsible for is that no removal
	// verdict reaches the executor AT ALL, so the report is what pins it —
	// Render writes the action's kind as the second column of every line.
	for _, line := range strings.Split(out.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[1] {
		case string(reconcile.KindPrune), string(reconcile.KindKeep):
			t.Errorf("a %s action reached the executor; startup must filter them out:\n%s",
				fields[1], out.String())
		}
	}
}

// A workspace herdr does not report as a worktree is some directory someone
// opened, which may sit in a repository this plugin was never pointed at.
// Deriving a root from it would have startup adopting and staffing repositories
// nobody asked it to manage.
func TestStartupSkipsWorkspacesThatAreNotWorktrees(t *testing.T) {
	elsewhere := herdrtest.NewRepo(t)
	elsewhere.AddWorktree("stray", "stray")

	server := herdrtest.NewServer(t)
	t.Setenv("HERDR_SOCKET_PATH", server.SocketPath)
	t.Setenv("HERDR_PLUGIN_CONTEXT_JSON", "")
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())
	// A plain workspace: no worktree block at all, so nothing carries a repo
	// root, even though its label names a directory inside a git repository.
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list",
		"workspaces": []map[string]any{{
			"workspace_id": "w1", "number": 1, "label": "notes", "focused": false,
			"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "unknown",
		}}})
	t.Setenv(eventsEnv, "1")

	var out strings.Builder
	if err := startupCommand(nil, &out); err != nil {
		t.Fatalf("startup: %v", err)
	}

	if called(t, server, "worktree.list") {
		t.Error("startup reconciled a repository it was never pointed at")
	}
	// A silent no-op is the failure mode this codebase avoids.
	if !strings.Contains(out.String(), "nothing to reconcile") {
		t.Errorf("startup should say it found nothing, got: %q", out.String())
	}
}

// herdr emits worktree.opened as it restores workspaces, so an event hook can be
// mid-reconcile on a repository by the time startup reaches it. It is running
// this same whole-repository pass, so standing down is correct — and it must not
// cost the OTHER repositories their pass.
func TestStartupCoalescesPerRepository(t *testing.T) {
	busy := newRestoredRepo(t, "alpha", "alpha", "w1")
	free := newRestoredRepo(t, "beta", "beta", "w2")
	server := startupSession(t, busy, free)
	t.Setenv(eventsEnv, "1")

	state := t.TempDir()
	t.Setenv("HERDR_PLUGIN_STATE_DIR", state)

	// Stand in for the event hook already reconciling the first repository.
	held, err := repolock.Acquire(state, gitx.Resolve(busy.repo.Root))
	if err != nil || held == nil {
		t.Fatalf("could not simulate a holder: %v", err)
	}

	var out strings.Builder
	if err := startupCommand(nil, &out); err != nil {
		t.Fatalf("coalescing must not be an error: %v", err)
	}

	opened := adopted(server)
	if slices.Contains(opened, busy.checkout) {
		t.Error("startup reconciled a repository that was already being reconciled")
	}
	if !slices.Contains(opened, free.checkout) {
		t.Errorf("one busy repository stopped the others; adopted %v\noutput:\n%s", opened, out.String())
	}
	if !held.TakeDirty() {
		t.Error("the coalesced repository was left unmarked; the holder will finish on a stale snapshot")
	}
}

// There is no second startup, so one broken repository must not cost the others
// their only pass. The failure still has to reach the exit code: herdr records a
// plugin command that exits 0 as succeeded.
func TestStartupKeepsGoingWhenOneRepositoryFails(t *testing.T) {
	broken := newRestoredRepo(t, "alpha", "alpha", "w1")
	healthy := newRestoredRepo(t, "beta", "beta", "w2")
	server := startupSession(t, broken, healthy)
	t.Setenv(eventsEnv, "1")

	server.Handle("worktree.list", func(params map[string]any) (any, error) {
		cwd, _ := params["cwd"].(string)
		if broken.matches(cwd) {
			return nil, errors.New("repository is in a bad way")
		}
		return worktreeListReply(healthy.repo, healthy.checkout, "beta", ""), nil
	})

	var out strings.Builder
	err := startupCommand(nil, &out)
	if err == nil {
		t.Fatalf("a failed repository must reach the exit code; output:\n%s", out.String())
	}
	if !slices.Contains(adopted(server), healthy.checkout) {
		t.Errorf("one failing repository stopped the rest; output:\n%s", out.String())
	}
}
