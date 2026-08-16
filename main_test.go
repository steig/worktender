package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrtest"
)

// An unrecognised subcommand must fail. herdr records a plugin action that
// exits 0 as "succeeded", so returning nil here would report a command that did
// nothing as a success.
func TestUnknownCommandFails(t *testing.T) {
	for _, args := range [][]string{
		{"prune-typo"},
		{"--help"},
		{""},
		nil,
	} {
		if err := run(args, io.Discard); err == nil {
			t.Errorf("run(%q) returned nil, want an error", args)
		}
	}
}

func TestUsageNamesEveryCommand(t *testing.T) {
	for _, command := range commands {
		if !strings.Contains(usage, command) {
			t.Errorf("usage does not mention %q: %s", command, usage)
		}
	}
}

// The half the list above cannot prove on its own: that every name usage
// advertises actually dispatches. These run for real and are expected to fail —
// there is no herdr and no flags — so the assertion is only that they failed for
// some reason OTHER than not existing.
//
// `list` is checked separately because it is an alias, reachable from the switch
// but deliberately absent from usage.
func TestEveryAdvertisedCommandDispatches(t *testing.T) {
	for _, command := range append(append([]string{}, commands...), "list") {
		t.Run(command, func(t *testing.T) {
			err := run([]string{command}, io.Discard)
			if err != nil && strings.Contains(err.Error(), "unknown command") {
				t.Errorf("usage advertises %q but run does not dispatch it: %v", command, err)
			}
		})
	}
}

// herdr runs plugin commands with cwd set to the plugin root, which is itself a
// git repository. A destructive command that falls back to the process cwd
// would therefore target this plugin's own checkout, so it must refuse instead.
func TestDestructiveCommandsRefuseWithoutContext(t *testing.T) {
	server := herdrtest.NewServer(t)

	for _, tc := range []struct {
		name string
		run  func(io.Writer) error
	}{
		{"sync starts agents", func(w io.Writer) error { return syncCommand(nil, w) }},
		{"prune-apply removes worktrees", func(w io.Writer) error { return pruneCommand(nil, w, true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HERDR_SOCKET_PATH", server.SocketPath)
			t.Setenv("HERDR_PLUGIN_CONTEXT_JSON", "")

			err := tc.run(io.Discard)
			if err == nil {
				t.Fatal("expected a refusal when herdr supplied no context")
			}
			if !strings.Contains(err.Error(), "refusing to guess") {
				t.Errorf("error should explain the refusal, got %v", err)
			}
		})
	}
}

// A malformed context is a bug signal, and fatal even for a read-only command:
// treating it as absent would silently retarget the command.
func TestEveryCommandRejectsAMalformedContext(t *testing.T) {
	server := herdrtest.NewServer(t)

	for _, tc := range []struct {
		name string
		run  func(io.Writer) error
	}{
		{"ls", func(w io.Writer) error { return lsCommand(nil, w) }},
		{"sync", func(w io.Writer) error { return syncCommand(nil, w) }},
		{"prune", func(w io.Writer) error { return pruneCommand(nil, w, false) }},
		{"prune-apply", func(w io.Writer) error { return pruneCommand(nil, w, true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HERDR_SOCKET_PATH", server.SocketPath)
			t.Setenv("HERDR_PLUGIN_CONTEXT_JSON", `{"workspace_cwd":`)

			if err := tc.run(io.Discard); err == nil {
				t.Fatal("a malformed context must not be treated as absent")
			}
		})
	}
}

// fakeSession points the commands at a fake herdr and a real repository, the
// way herdr would when it invokes the plugin.
func fakeSession(t *testing.T, repo *herdrtest.Repo) *herdrtest.Server {
	t.Helper()

	server := herdrtest.NewServer(t)
	t.Setenv("HERDR_SOCKET_PATH", server.SocketPath)
	t.Setenv("HERDR_PLUGIN_CONTEXT_JSON", `{"workspace_cwd":"`+repo.Root+`"}`)
	// Point plugin state somewhere disposable. Inherited, it would be the real
	// ~/.local/state/herdr/plugins/steig.worktender, and the suite would leave repository
	// locks in a developer's live plugin state. It happens to be unset in a
	// normal shell, which is luck rather than isolation.
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())
	return server
}

// worktreeListReply builds a worktree.list reply for one linked checkout.
func worktreeListReply(repo *herdrtest.Repo, checkout, branch, workspaceID string) map[string]any {
	entry := map[string]any{
		"path": checkout, "label": filepath.Base(checkout), "branch": branch,
		"is_bare": false, "is_detached": false, "is_prunable": false,
		"is_linked_worktree": true,
	}
	if workspaceID != "" {
		entry["open_workspace_id"] = workspaceID
	}
	return map[string]any{
		"type": "worktree_list",
		"source": map[string]any{"repo_key": "k", "repo_name": "repo",
			"repo_root": repo.Root, "source_checkout_path": repo.Root},
		"worktrees": []map[string]any{entry},
	}
}

func workspaceListReply(repo *herdrtest.Repo, checkout, workspaceID string) map[string]any {
	return map[string]any{"type": "workspace_list", "workspaces": []map[string]any{{
		"workspace_id": workspaceID, "number": 2, "label": "wt", "focused": false,
		"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "idle",
		"worktree": map[string]any{"repo_key": "k", "repo_name": "repo",
			"repo_root": repo.Root, "checkout_path": checkout, "is_linked_worktree": true},
	}}}
}

// The exit-code class: a command whose actions all failed must not report
// success just because it printed the failures.
func TestSyncFailsWhenAnActionFails(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("wip", "wip")

	server := fakeSession(t, repo)
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "wip", "w2"))
	server.HandleResult("workspace.list", workspaceListReply(repo, checkout, "w2"))
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})
	server.HandleResult("pane.list", map[string]any{"type": "pane_list",
		"panes": []map[string]any{{"pane_id": "w2:p1"}}})
	// The pane is still running direnv, so the agent cannot start.
	server.Handle("agent.start", func(map[string]any) (any, error) {
		return nil, errBusyPane{}
	})

	var out strings.Builder
	err := syncCommand(nil, &out)
	if err == nil {
		t.Fatalf("sync should fail when staffing failed; output was:\n%s", out.String())
	}
	// The report still has to be printed, not swallowed by the error.
	if !strings.Contains(out.String(), "failed") {
		t.Errorf("the failure should be reported, got:\n%s", out.String())
	}
}

type errBusyPane struct{}

func (errBusyPane) Error() string { return "pane is busy" }

func TestSyncSucceedsWhenNothingFails(t *testing.T) {
	repo := herdrtest.NewRepo(t)

	server := fakeSession(t, repo)
	server.HandleResult("worktree.list", map[string]any{
		"type": "worktree_list",
		"source": map[string]any{"repo_key": "k", "repo_name": "repo",
			"repo_root": repo.Root, "source_checkout_path": repo.Root},
		"worktrees": []map[string]any{},
	})
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})

	var out strings.Builder
	if err := syncCommand(nil, &out); err != nil {
		t.Fatalf("sync with nothing to do should succeed: %v", err)
	}
}

// `prune` is the safe half: it must list candidates and remove nothing.
func TestPruneListsWithoutRemoving(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("done", "done")
	repo.CommitIn(checkout, "done.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")

	// Topology alone never prunes; an authoritative PR verdict does.
	herdrtest.FakeGhPRState(t, "MERGED")

	server := fakeSession(t, repo)
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "done", ""))
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})

	var out strings.Builder
	if err := pruneCommand(nil, &out, false); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if !strings.Contains(out.String(), "would remove") {
		t.Errorf("prune should list candidates, got:\n%s", out.String())
	}
	if !repo.Exists(checkout) {
		t.Fatal("prune removed a worktree; it must only list")
	}
	for _, call := range server.Calls() {
		if call.Method == "worktree.remove" {
			t.Error("prune asked herdr to remove a worktree")
		}
	}
}

// Asking to prune must not open workspaces or start agents on the way past.
func TestPruneDoesNotAdoptOrStaff(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("fresh", "fresh")

	server := fakeSession(t, repo)
	// The worktree has no workspace, so a full plan would adopt it.
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "fresh", ""))
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})

	var out strings.Builder
	if err := pruneCommand(nil, &out, false); err != nil {
		t.Fatalf("prune: %v", err)
	}

	for _, call := range server.Calls() {
		switch call.Method {
		case "worktree.open", "agent.start":
			t.Errorf("prune performed %s as a side effect", call.Method)
		}
	}
}

// sync resolves its repository from herdr's invocation context, not from the
// process working directory, so it can act somewhere other than where the caller
// believes they are standing. `prune` prints the root it resolved for exactly
// that reason; sync had the same divergence and none of the disclosure.
func TestSyncNamesTheRepositoryItResolved(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("feature", "feature")

	server := fakeSession(t, repo)
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "feature", ""))
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})
	server.HandleResult("worktree.open", map[string]any{"type": "workspace_created"})

	var out strings.Builder
	if err := syncCommand(nil, &out); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !strings.Contains(out.String(), "repository: "+repo.RealRoot) {
		t.Errorf("sync must name the repository it resolved, got:\n%s", out.String())
	}
}

// sync executes adoptions and staffing only — prunes are filtered out — so a
// pull request state can authorise nothing it does. Asking anyway is a `gh`
// invocation per worktree per pass, in series, while the repository lock is
// held. The event and startup paths already drop the lookup for this reason.
func TestSyncAsksGhNothing(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("feature", "feature")

	called := filepath.Join(t.TempDir(), "gh-was-called")
	herdrtest.FakeGh(t, "echo called >> "+called)

	server := fakeSession(t, repo)
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "feature", ""))
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})
	server.HandleResult("worktree.open", map[string]any{"type": "workspace_created"})

	var out strings.Builder
	if err := syncCommand(nil, &out); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, err := os.Stat(called); err == nil {
		t.Error("sync invoked gh; its answer could only ever authorise a prune, which sync does not perform")
	}
}

// And the reverse: sync must not remove anything.
func TestSyncDoesNotPrune(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("done", "done")
	repo.CommitIn(checkout, "done.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")

	server := fakeSession(t, repo)
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "done", ""))
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})
	server.HandleResult("worktree.open", map[string]any{"type": "workspace_created"})

	var out strings.Builder
	if err := syncCommand(nil, &out); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !repo.Exists(checkout) {
		t.Fatal("sync removed a worktree")
	}
}

// herdr's own worktree creation puts checkouts under ~/.herdr/worktrees/<repo>/,
// outside the repository entirely, so the .claude/worktrees convention the rest
// of these tests use is only one of the shapes that reaches the reconciler. The
// claim under test is that path layout is irrelevant: workspaces are matched by
// repo_root equality, never by containment.
func TestSyncAdoptsAWorktreeOutsideTheRepoRoot(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	outside := filepath.Join(t.TempDir(), "worktree-brave-valley-66f8")
	checkout := repo.AddWorktreeAt(outside, "worktree/brave-valley-66f8")

	if strings.HasPrefix(checkout, repo.Root) {
		t.Fatalf("checkout %s is inside the repository root %s; this test proves nothing", checkout, repo.Root)
	}

	server := fakeSession(t, repo)
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "worktree/brave-valley-66f8", ""))
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})
	server.HandleResult("worktree.open", map[string]any{"type": "workspace_created"})

	var out strings.Builder
	if err := syncCommand(nil, &out); err != nil {
		t.Fatalf("sync: %v", err)
	}

	for _, call := range server.Calls() {
		if call.Method != "worktree.open" {
			continue
		}
		if path, _ := call.Params["path"].(string); path != checkout {
			t.Errorf("adopted %q, want the out-of-root checkout %q", path, checkout)
		}
		return
	}
	t.Errorf("an out-of-root worktree was never adopted; output:\n%s", out.String())
}

// Adoption creates exactly the condition staffing exists to fix, and sync is
// documented to do both. It plans from one snapshot, though, taken before the
// workspace it opens exists — and the pass loop only runs again when somebody
// else marks the repository dirty. Left alone, the worktree sits there as a
// bare shell until sync is run a second time.
func TestSyncStaffsAWorktreeItJustAdopted(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("wip", "wip")

	server := fakeSession(t, repo)

	var mu sync.Mutex
	adopted := false
	opened := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return adopted
	}

	server.Handle("worktree.list", func(map[string]any) (any, error) {
		if opened() {
			return worktreeListReply(repo, checkout, "wip", "w2"), nil
		}
		return worktreeListReply(repo, checkout, "wip", ""), nil
	})
	server.Handle("workspace.list", func(map[string]any) (any, error) {
		if opened() {
			return workspaceListReply(repo, checkout, "w2"), nil
		}
		return map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}}, nil
	})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})
	server.HandleResult("pane.list", map[string]any{"type": "pane_list",
		"panes": []map[string]any{{"pane_id": "w2:p1"}}})
	server.Handle("worktree.open", func(map[string]any) (any, error) {
		mu.Lock()
		adopted = true
		mu.Unlock()
		return map[string]any{"type": "workspace_created"}, nil
	})
	server.HandleResult("agent.start", map[string]any{"type": "ok"})

	var out strings.Builder
	if err := syncCommand(nil, &out); err != nil {
		t.Fatalf("sync: %v", err)
	}

	for _, call := range server.Calls() {
		if call.Method == "agent.start" {
			return
		}
	}
	t.Errorf("sync adopted a worktree and left it unstaffed; output:\n%s", out.String())
}

func TestPruneApplyRemoves(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("done", "done")
	repo.CommitIn(checkout, "done.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")

	// Topology alone never prunes; an authoritative PR verdict does.
	herdrtest.FakeGhPRState(t, "MERGED")

	server := fakeSession(t, repo)
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "done", ""))
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})

	var out strings.Builder
	if err := pruneCommand(nil, &out, true); err != nil {
		t.Fatalf("prune-apply: %v", err)
	}
	if repo.Exists(checkout) {
		t.Error("prune-apply should have removed the worktree")
	}
}

// The measured case from #89, end to end: a merged pull request, a clean
// checkout, and the worker that did the work still sitting in the pane it was
// started in. Kept by default, and removable by asking — the whole difference
// being that "has an agent" and "is busy" come apart after a dispatch.
func TestPruneApplyReleasesAFinishedAgentOnlyWhenAsked(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantRemove bool
	}{
		{"without the flag", []string{"prune-apply"}, false},
		{"with the flag", []string{"prune-apply", "--release-agents"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := herdrtest.NewRepo(t)
			checkout := repo.AddWorktree("done", "done")
			repo.CommitIn(checkout, "done.txt", "work")
			repo.Git("merge", "--no-ff", "-m", "merge done", "done")
			herdrtest.FakeGhPRState(t, "MERGED")

			server := fakeSession(t, repo)
			server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "done", "w2"))
			server.HandleResult("workspace.list", workspaceListReply(repo, checkout, "w2"))
			server.HandleResult("pane.list", map[string]any{"type": "pane_list",
				"panes": []map[string]any{{"pane_id": "w2:p1"}}})
			server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{{
				"pane_id": "w2:p1", "workspace_id": "w2", "tab_id": "w2:t1", "terminal_id": "t",
				"agent_status": "idle", "focused": false, "revision": 1,
			}}})
			server.HandleResult("worktree.remove", map[string]any{"type": "worktree_removed"})

			var out bytes.Buffer
			if err := run(tc.args, &out); err != nil {
				t.Fatalf("%v: %v", tc.args, err)
			}

			removed := false
			for _, call := range server.Calls() {
				if call.Method == "worktree.remove" {
					removed = true
				}
			}
			if removed != tc.wantRemove {
				t.Fatalf("worktree.remove called = %v, want %v; output:\n%s", removed, tc.wantRemove, out.String())
			}
			if !tc.wantRemove && !strings.Contains(out.String(), "--release-agents") {
				t.Errorf("a keep nothing can act on is the bug; output:\n%s", out.String())
			}
		})
	}
}

// The dry run exists to be read before the apply, so both must say which
// repository they resolved. They do not resolve it the same way — listing may
// fall back to the working directory and applying may not — and that asymmetry
// once let a `prune` listing six worktrees be followed by a `prune-apply`
// reporting "nothing to do" about a different root, with nothing in either
// output to show they had disagreed.
func TestBothPruneHalvesNameTheRepositoryTheyResolved(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply bool
	}{
		{"prune", false},
		{"prune-apply", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := herdrtest.NewRepo(t)
			checkout := repo.AddWorktree("done", "done")
			repo.CommitIn(checkout, "done.txt", "work")
			repo.Git("merge", "--no-ff", "-m", "merge done", "done")
			herdrtest.FakeGhPRState(t, "MERGED")

			server := fakeSession(t, repo)
			server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "done", ""))
			server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
			server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})

			var out strings.Builder
			if err := pruneCommand(nil, &out, tc.apply); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}

			if !strings.Contains(out.String(), "repository: ") {
				t.Fatalf("%s must name the repository it acted on, got:\n%s", tc.name, out.String())
			}
			if !strings.Contains(out.String(), repo.RealRoot) {
				t.Errorf("%s named a root other than %s:\n%s", tc.name, repo.RealRoot, out.String())
			}
		})
	}
}

// --- --repo ------------------------------------------------------------------------------
// An action carries no arguments, so herdr's context is the only thing prune can resolve
// from — and that context names herdr's current workspace, not the repository the operator
// is standing in. Observed live: a dry run inside a repository with four staffed worktrees
// planned against a different project entirely.

func TestPruneActsOnTheNamedRepositoryRatherThanHerdrsContext(t *testing.T) {
	current := herdrtest.NewRepo(t) // what herdr thinks is current
	named := herdrtest.NewRepo(t)   // what the operator asked for

	server := fakeSession(t, current)
	server.HandleResult("worktree.list", map[string]any{
		"type": "worktree_list",
		"source": map[string]any{"repo_key": "k", "repo_name": "named",
			"repo_root": named.Root, "source_checkout_path": named.Root},
		"worktrees": []map[string]any{},
	})
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})
	server.HandleResult("pane.list", map[string]any{"type": "pane_list", "panes": []map[string]any{}})

	var out bytes.Buffer
	if err := run([]string{"prune", "--repo", named.Root}, &out); err != nil {
		t.Fatalf("prune --repo: %v", err)
	}

	want := "repository: " + gitx.Resolve(named.Root)
	if !strings.Contains(out.String(), want) {
		t.Fatalf("expected %q in output, got:\n%s", want, out.String())
	}
	// The header is the whole safety property: it must not name the one it did not act on.
	if strings.Contains(out.String(), gitx.Resolve(current.Root)) {
		t.Fatalf("named repository was ignored in favour of herdr's context:\n%s", out.String())
	}
}

func TestPruneRefusesARepoThatIsNotOne(t *testing.T) {
	// Never a fallback. The point of naming a repository is to stop the resolution from
	// wandering, so a bad path has to stop rather than quietly land somewhere plausible.
	current := herdrtest.NewRepo(t)
	fakeSession(t, current)

	var out bytes.Buffer
	err := run([]string{"prune", "--repo", t.TempDir()}, &out)
	if err == nil {
		t.Fatalf("expected an error for a non-repository, got output:\n%s", out.String())
	}
	if strings.Contains(out.String(), gitx.Resolve(current.Root)) {
		t.Fatalf("fell back to herdr's context instead of failing:\n%s", out.String())
	}
}

func TestPruneRejectsAStrayArgument(t *testing.T) {
	// `prune /some/path` is the shape someone reaches for before finding the flag. Taking
	// it silently would act on the context while looking like it acted on the path.
	fakeSession(t, herdrtest.NewRepo(t))

	var out bytes.Buffer
	if err := run([]string{"prune-apply", "/tmp/somewhere"}, &out); err == nil {
		t.Fatal("expected a stray positional argument to be refused")
	} else if !strings.Contains(err.Error(), "prune-apply") {
		t.Fatalf("the error should name the half it came from, got: %v", err)
	}
}

// #77's whole point: someone with agents in several repositories sees them
// without visiting each one — and from outside any repository at all, which is
// the case a per-repository listing cannot serve. The scope is herdr's open
// worktree workspaces, the set startup already computes.
func TestLsAllRepositoriesAnswersFromOutsideARepository(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("77-cross-repo", "77-cross-repo")

	server := herdrtest.NewServer(t)
	t.Setenv("HERDR_SOCKET_PATH", server.SocketPath)
	// Neither a launch context nor a repository to stand in.
	t.Setenv("HERDR_PLUGIN_CONTEXT_JSON", "")
	t.Chdir(t.TempDir())

	server.HandleResult("workspace.list", workspaceListReply(repo, checkout, "w2"))
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "77-cross-repo", "w2"))
	server.HandleResult("pane.list", map[string]any{"type": "pane_list",
		"panes": []map[string]any{{"pane_id": "w2:p1", "workspace_id": "w2", "tab_id": "t1", "index": 0}}})

	var out bytes.Buffer
	if err := run([]string{"ls", "--all-repos"}, &out); err != nil {
		t.Fatalf("ls --all-repos: %v", err)
	}

	for _, want := range []string{repo.RealRoot, "77-cross-repo", "w2:p1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

// The pull request lookup is scoped to one repository and runs in series, so
// across several it would be slow and asking the wrong repository. Refused
// rather than quietly answered from whichever repository happened to be first.
func TestLsRefusesPullRequestStateAcrossRepositories(t *testing.T) {
	t.Setenv("HERDR_SOCKET_PATH", "")

	err := run([]string{"ls", "--all-repos", "--pr"}, new(bytes.Buffer))
	if err == nil {
		t.Fatal("--pr with --all-repos should be refused")
	}
	if !strings.Contains(err.Error(), "--pr cannot be combined with --all-repos") {
		t.Errorf("the error must name the combination, got: %v", err)
	}
}

// End to end over the wire, which is where this was found: herdr held three
// workspaces pointing into a directory that no longer existed, `worktender ls`
// printed one row, and `prune` reported nothing. "Nothing to do" is the same
// output a clean repository gets, so the tool that exists to say which of the
// things on disk are real was the one thing that could not see them.
//
// The shape the fake can express exactly: a workspace.list entry whose
// checkout_path no worktree.list entry mentions.
func TestPruneNamesAWorkspaceWhoseCheckoutHasVanished(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	gone := filepath.Join(repo.Root, "..", "vanished")

	server := fakeSession(t, repo)
	server.HandleResult("worktree.list", map[string]any{
		"type": "worktree_list",
		"source": map[string]any{"repo_key": "k", "repo_name": "repo",
			"repo_root": repo.Root, "source_checkout_path": repo.Root},
		"worktrees": []map[string]any{},
	})
	server.HandleResult("workspace.list", workspaceListReply(repo, gone, "w35"))
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})
	server.HandleResult("pane.list", map[string]any{"type": "pane_list",
		"panes": []map[string]any{{"pane_id": "w35:p1"}}})

	var out strings.Builder
	if err := pruneCommand(nil, &out, false); err != nil {
		t.Fatalf("prune: %v", err)
	}

	got := out.String()
	if strings.Contains(got, "nothing to do") {
		t.Errorf("a ghost workspace read as a clean repository:\n%s", got)
	}
	if !strings.Contains(got, "ghost") {
		t.Errorf("prune should name the verdict, got:\n%s", got)
	}
	if !strings.Contains(got, "vanished") {
		t.Errorf("prune should name the checkout herdr is holding open, got:\n%s", got)
	}
	// Diagnosis only: naming it must not start anything or close anything.
	for _, call := range server.Calls() {
		switch call.Method {
		case "worktree.remove", "workspace.close", "agent.start":
			t.Errorf("a ghost is diagnosis only; prune called %s", call.Method)
		}
	}
}

// noHerdr puts the machine in the state the degraded path is for: no herdr
// running.
//
// Clearing HERDR_SOCKET_PATH is not enough to establish that and never was —
// it is unset in every ordinary terminal, herdr running or not. HerdrDown also
// points the default socket at an empty directory, so the probe dials something
// and finds nothing. Its opposite number is herdrtest.HerdrUnnamed.
func noHerdr(t *testing.T) {
	t.Helper()
	herdrtest.HerdrDown(t)
}

// The listing works with herdr absent: the checkouts come from git and every
// herdr column is empty, because those facts do not exist rather than could
// not be read.
func TestLsWorksWithoutHerdr(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	repo.AddWorktree("wip", "wip")
	noHerdr(t)
	t.Chdir(repo.Root)

	var out strings.Builder
	if err := lsCommand(nil, &out); err != nil {
		t.Fatalf("ls without herdr: %v", err)
	}

	// Asserted on the parsed row rather than on the text containing "wip":
	// every branch name here is also a directory basename, so a substring match
	// cannot say which column it found — and the columns are the claim.
	got := out.String()
	var linked string
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.Contains(line, "wip") {
			linked = line
		}
	}
	if linked == "" {
		t.Fatalf("the linked worktree should be listed:\n%s", got)
	}
	if !strings.HasPrefix(strings.TrimSpace(got), "*") {
		t.Errorf("the main checkout should be listed and marked:\n%s", got)
	}

	// Marker, branch, workspace, pane, agent status, counter, dir. The four in
	// the middle are herdr's, and with no herdr they are empty rather than
	// unread — that is the whole claim the README's example block makes.
	fields := strings.Fields(linked)
	if len(fields) != 6 {
		t.Fatalf("want branch, four herdr columns and a dir, got %d in %q", len(fields), linked)
	}
	if fields[0] != "wip" {
		t.Errorf("branch column = %q, want wip", fields[0])
	}
	for i, column := range []string{"workspace", "pane", "agent status", "counter"} {
		if fields[1+i] != "-" {
			t.Errorf("%s column = %q, want empty with no herdr", column, fields[1+i])
		}
	}
}

// The listing is the one command that degrades; its two agent-shaped flags do
// not. Answering them with no herdr would be a claim about a fleet this process
// cannot see — "no blocked agents" is not the same answer as "no way to tell".
func TestLsRefusesTheAgentFlagsWithoutHerdr(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	repo.AddWorktree("wip", "wip")

	for _, flag := range []string{"--blocked", "--reports"} {
		t.Run(flag, func(t *testing.T) {
			noHerdr(t)
			t.Chdir(repo.Root)

			err := lsCommand([]string{flag}, &strings.Builder{})
			if err == nil {
				t.Fatalf("ls %s answered with no herdr to answer from", flag)
			}
			if got := exitCode(err); got != exitEnvironment {
				t.Errorf("exit %d (%v), want exitEnvironment (%d)", got, err, exitEnvironment)
			}
		})
	}
}

// prune-apply is named in the title, the CHANGELOG and the README as running
// without herdr, and this is the invocation a plain-shell user types: no
// --repo, no context to resolve from. It removes, so nothing else in the suite
// reaches the nil-client path through pruneBlocked into removeCheckout.
func TestPruneApplyRemovesAWorktreeWithoutHerdr(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("done", "done")
	repo.CommitIn(checkout, "done.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")
	herdrtest.FakeGhPRState(t, "MERGED")
	noHerdr(t)
	t.Chdir(repo.Root)

	var out strings.Builder
	if err := pruneCommand(nil, &out, true); err != nil {
		t.Fatalf("prune-apply without herdr: %v", err)
	}

	if repo.Exists(checkout) {
		t.Errorf("the merged worktree should have been removed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "removed") {
		t.Errorf("the result should say it removed something:\n%s", out.String())
	}
}

// The destructive command must refuse when herdr's state is merely unknown, not
// only when herdr is proven absent.
//
// What makes it refuse is an ordering inside dialHerdrIfPresent: the
// ErrHerdrUnknown case sits ahead of the `need == herdrRequired` case and
// returns unconditionally, so prune-apply — which is herdrOptional — takes it
// too. Nothing else holds that ordering. Folding the unknown check into the
// herdrRequired branch would compile, pass every other test, and quietly put
// back a prune-apply that degrades on a dial it could not finish.
//
// A socket with no listener is the cheapest way to produce the state: not
// ENOENT, so not proof, and on darwin indistinguishable from a herdr too busy
// to accept.
func TestPruneApplyRefusesWhenHerdrCannotBeEstablished(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("done", "done")
	repo.CommitIn(checkout, "done.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")
	herdrtest.FakeGhPRState(t, "MERGED")

	noHerdr(t)
	herdrtest.StaleHerdrSocket(t)
	t.Chdir(repo.Root)

	var out strings.Builder
	err := pruneCommand(nil, &out, true)
	if err == nil {
		t.Fatalf("prune-apply degraded on a dial that established nothing:\n%s", out.String())
	}
	if got := exitCode(err); got != exitEnvironment {
		t.Errorf("exit %d (%v), want exitEnvironment (%d)", got, err, exitEnvironment)
	}
	if !repo.Exists(checkout) {
		t.Fatal("removed a checkout without establishing that herdr is not running")
	}
}

// The state that separates a probe from an environment variable, and the reason
// the probe had to exist.
//
// A plain terminal has no HERDR_SOCKET_PATH whether or not herdr is running.
// Read as absence, every workspace-shaped fact goes empty: the reconciler
// cannot see the agent, plans a prune, and the execution-time re-check asks a
// client that is nil and hears "nothing holds it" — so a checkout with a live
// agent standing in it is removed with --force. Before the degraded path
// existed this invocation refused outright, which is why this is the test that
// has to hold.
func TestPruneApplySparesACheckoutHerdrHoldsWhenTheVariableIsUnset(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("done", "done")
	repo.CommitIn(checkout, "done.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")
	herdrtest.FakeGhPRState(t, "MERGED")

	// herdr up, holding this checkout, with a working agent in its pane —
	// and it did not start this process, so it named no socket.
	server := herdrtest.HerdrUnnamed(t)
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "done", "ws1"))
	server.HandleResult("workspace.list", map[string]any{
		"type": "workspace_list",
		"workspaces": []map[string]any{{
			"workspace_id": "ws1",
			"worktree": map[string]any{
				"repo_root": repo.RealRoot, "checkout_path": checkout, "is_linked_worktree": true,
			},
		}},
	})
	server.HandleResult("pane.list", map[string]any{
		"type": "pane_list", "panes": []map[string]any{{"pane_id": "p1"}},
	})
	server.HandleResult("agent.list", map[string]any{
		"type":   "agent_list",
		"agents": []map[string]any{{"pane_id": "p1", "agent_status": "working", "agent_name": "worker"}},
	})
	t.Chdir(repo.Root)

	var out strings.Builder
	if err := pruneCommand([]string{"--repo", repo.Root}, &out, true); err != nil {
		t.Fatalf("prune-apply: %v", err)
	}

	if !repo.Exists(checkout) {
		t.Fatalf("removed a checkout a working agent is standing in:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "agent running") {
		t.Errorf("the guard should say what spared it:\n%s", out.String())
	}
}

// Every prune guard is git or gh, so the verdicts do not need herdr and never
// did. Only the enumeration ever came from it.
func TestPruneWorksWithoutHerdr(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("done", "done")
	repo.CommitIn(checkout, "done.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")
	herdrtest.FakeGhPRState(t, "MERGED")
	noHerdr(t)
	t.Chdir(repo.Root)

	var out strings.Builder
	if err := pruneCommand(nil, &out, false); err != nil {
		t.Fatalf("prune without herdr: %v", err)
	}

	if !strings.Contains(out.String(), "would remove") {
		t.Errorf("prune should still reach a verdict without herdr:\n%s", out.String())
	}
	if !repo.Exists(checkout) {
		t.Fatal("prune removed a worktree; it must only list")
	}
}

// The commands whose whole job is herdr say so, on the environment exit code
// rather than the usage one: the command was spelled correctly and the machine
// could not answer it.
func TestHerdrOnlyCommandsExitEnvironmentWithoutHerdr(t *testing.T) {
	repo := herdrtest.NewRepo(t)

	for _, args := range [][]string{
		{"sync"},
		{"start", "42"},
		{"dispatch", "--pane", "w1:p1", "--name", "worker"},
		// Named in the CHANGELOG and the README alongside the other three.
		// It used to reach 2 through exitCode's catch-all for unclassified
		// errors, which is the documented outcome arrived at by accident.
		{"gate", "--target", "worker"},
	} {
		t.Run(args[0], func(t *testing.T) {
			noHerdr(t)
			t.Chdir(repo.Root)

			err := run(args, &strings.Builder{})
			if err == nil {
				t.Fatalf("run(%q) succeeded with no herdr", args)
			}
			if got := exitCode(err); got != exitEnvironment {
				t.Errorf("run(%q) = exit %d (%v), want exitEnvironment (%d)", args, got, err, exitEnvironment)
			}
			if !strings.Contains(err.Error(), "herdr") {
				t.Errorf("the error should name what is missing: %v", err)
			}
		})
	}
}
