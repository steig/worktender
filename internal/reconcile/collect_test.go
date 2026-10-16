package reconcile_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/herdrtest"
	"github.com/steig/worktender/internal/reconcile"
)

// worktreeJSON builds one entry of a worktree.list reply.
func worktreeJSON(path, branch string, linked bool, workspaceID string) map[string]any {
	entry := map[string]any{
		"path": path, "label": filepath.Base(path), "branch": branch,
		"is_bare": false, "is_detached": false, "is_prunable": false,
		"is_linked_worktree": linked,
	}
	if workspaceID != "" {
		entry["open_workspace_id"] = workspaceID
	}
	return entry
}

// collectFixture wires a real repo to a fake herdr and returns the collector.
func collectFixture(t *testing.T, repo *herdrtest.Repo, worktrees []map[string]any,
	workspaces []map[string]any, agents []map[string]any, panes map[string][]string,
) *reconcile.Collector {
	t.Helper()

	server := herdrtest.NewServer(t)
	server.HandleResult("worktree.list", map[string]any{
		"type": "worktree_list",
		"source": map[string]any{"repo_key": "k", "repo_name": "repo",
			"repo_root": repo.Root, "source_checkout_path": repo.Root},
		"worktrees": worktrees,
	})
	server.HandleResult("workspace.list", map[string]any{
		"type": "workspace_list", "workspaces": workspaces,
	})
	server.HandleResult("agent.list", map[string]any{
		"type": "agent_list", "agents": agents,
	})
	server.Handle("pane.list", func(params map[string]any) (any, error) {
		id, _ := params["workspace_id"].(string)
		out := []map[string]any{}
		for _, pane := range panes[id] {
			out = append(out, map[string]any{"pane_id": pane})
		}
		return map[string]any{"type": "pane_list", "panes": out}, nil
	})

	collector := reconcile.NewCollector(herdrapi.NewWithSocket(server.SocketPath), repo.Root)
	collector.ProjectsDir = t.TempDir()
	// Default to "no PR"; individual tests install a fake gh instead.
	collector.LookupPR = func(string) reconcile.PRState { return reconcile.PRNone }
	return collector
}

// The full loop: real worktrees on disk, herdr's view of them, and the git
// facts that decide their fate.
func TestCollectAndReconcileAgainstRealGit(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	fresh := repo.AddWorktree("fresh", "fresh")
	done := repo.AddWorktree("done", "done")
	repo.CommitIn(done, "shipped.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")

	collector := collectFixture(t, repo,
		[]map[string]any{
			worktreeJSON(repo.Root, "main", false, "w1"),
			worktreeJSON(fresh, "fresh", true, ""),
			worktreeJSON(done, "done", true, ""),
		}, nil, nil, nil)

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	actions := reconcile.Reconcile(state)

	// With no PR to appeal to, neither is removed: git alone cannot tell a
	// merged branch from a branch forked off merged work, and cannot tell an
	// unstarted branch from a fast-forwarded one.
	for _, checkout := range []string{done, fresh} {
		if find(actions, reconcile.KindPrune, checkout) != nil {
			t.Errorf("nothing should be pruned on topology alone, got %+v", actions)
		}
		keep := find(actions, reconcile.KindKeep, checkout)
		if keep == nil {
			t.Fatalf("every candidate must be explained, none for %s", checkout)
		}
		if !strings.Contains(keep.Reason, "cannot tell") {
			t.Errorf("%s: reason should admit the ambiguity, got %q", filepath.Base(checkout), keep.Reason)
		}
	}
	// Both lack a workspace, so both get adopted.
	if find(actions, reconcile.KindAdopt, fresh) == nil {
		t.Error("the fresh worktree should be adopted")
	}
}

// The same repository with a merged PR: the authoritative signal removes what
// topology alone could not.
func TestCollectPrunesOnlyWithAPRVerdict(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	done := repo.AddWorktree("done", "done")
	repo.CommitIn(done, "shipped.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")

	herdrtest.FakeGhPRState(t, "MERGED")

	collector := collectFixture(t, repo,
		[]map[string]any{worktreeJSON(done, "done", true, "")}, nil, nil, nil)
	collector.LookupPR = func(branch string) reconcile.PRState {
		return reconcile.GhPRState(repo.Root, branch)
	}

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if find(reconcile.Reconcile(state), reconcile.KindPrune, done) == nil {
		t.Error("a merged PR should prune the worktree")
	}
}

// A branch forked off already-merged work points at the same commit as the
// branch that landed, so topology calls it merged. It has done nothing, and
// removing it would bin a worktree someone just set up.
func TestCollectDoesNotPruneABranchForkedOffMergedWork(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	done := repo.AddWorktree("done", "done")
	repo.CommitIn(done, "done.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")

	// Brand-new branch off the merged tip: no work of its own.
	later := repo.AddWorktreeFrom("later", "later", "done")

	collector := collectFixture(t, repo,
		[]map[string]any{worktreeJSON(later, "later", true, "")}, nil, nil, nil)

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if find(reconcile.Reconcile(state), reconcile.KindPrune, later) != nil {
		t.Fatal("a branch forked off merged work has done nothing and must not be pruned")
	}
}

// A fast-forward merged branch and an unstarted one reach the reconciler with
// identical facts, so neither may be removed and neither may be described as
// definitely unstarted.
func TestCollectKeepsFastForwardMergedWorktree(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	ff := repo.AddWorktree("ff", "ff")
	repo.CommitIn(ff, "ff.txt", "work")
	repo.Git("merge", "--ff-only", "ff")

	collector := collectFixture(t, repo,
		[]map[string]any{worktreeJSON(ff, "ff", true, "")}, nil, nil, nil)

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	actions := reconcile.Reconcile(state)
	if find(actions, reconcile.KindPrune, ff) != nil {
		t.Fatal("a fast-forward merge is indistinguishable from unstarted work; keep it")
	}
	keep := find(actions, reconcile.KindKeep, ff)
	if keep == nil {
		t.Fatal("expected a keep")
	}
	if !strings.Contains(keep.Reason, "cannot tell") {
		t.Errorf("the reason must not claim the branch is unstarted, got %q", keep.Reason)
	}
}

// Guard a against real git: a real uncommitted file must stop a prune that
// every other signal says should happen.
func TestCollectSeesUncommittedChanges(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("done", "done")
	repo.CommitIn(checkout, "shipped.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")
	herdrtest.WriteFile(t, filepath.Join(checkout, "scratch.txt"), "unsaved\n")

	collector := collectFixture(t, repo,
		[]map[string]any{worktreeJSON(checkout, "done", true, "")}, nil, nil, nil)

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	actions := reconcile.Reconcile(state)
	if find(actions, reconcile.KindPrune, checkout) != nil {
		t.Fatal("uncommitted changes must stop a prune even for a merged branch")
	}
	if keep := find(actions, reconcile.KindKeep, checkout); keep == nil || keep.Reason != "uncommitted changes" {
		t.Errorf("expected keep for uncommitted changes, got %+v", keep)
	}
}

// Guard b against real state: herdr reports an agent in the workspace's pane.
func TestCollectSeesLiveAgent(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("done", "done")
	repo.CommitIn(checkout, "shipped.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")

	collector := collectFixture(t, repo,
		[]map[string]any{worktreeJSON(checkout, "done", true, "w2")},
		[]map[string]any{{
			"workspace_id": "w2", "number": 2, "label": "done", "focused": false,
			"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "working",
			"worktree": map[string]any{"repo_key": "k", "repo_name": "repo",
				"repo_root": repo.Root, "checkout_path": checkout, "is_linked_worktree": true},
		}},
		[]map[string]any{{
			"pane_id": "w2:p1", "workspace_id": "w2", "tab_id": "w2:t1",
			"terminal_id": "term1", "agent_status": "working", "focused": false, "revision": 1,
		}},
		map[string][]string{"w2": {"w2:p1"}},
	)

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	actions := reconcile.Reconcile(state)
	if find(actions, reconcile.KindPrune, checkout) != nil {
		t.Fatal("a worktree hosting a live agent must never be pruned")
	}
	if keep := find(actions, reconcile.KindKeep, checkout); keep == nil || keep.Reason != "agent running" {
		t.Errorf("expected keep for a running agent, got %+v", keep)
	}
	if find(actions, reconcile.KindStaff, checkout) != nil {
		t.Error("a staffed workspace should not be staffed again")
	}
}

// A squash-merged branch is not an ancestor of base, so only gh knows it is
// done. This is the case the git fallback cannot catch.
func TestCollectUsesGhForSquashMergedBranch(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("squashed", "squashed")
	repo.CommitIn(checkout, "sq.txt", "work")
	repo.Git("merge", "--squash", "squashed")
	repo.Git("commit", "-m", "squashed work")

	herdrtest.FakeGhPRState(t, "MERGED")

	collector := collectFixture(t, repo,
		[]map[string]any{worktreeJSON(checkout, "squashed", true, "")}, nil, nil, nil)
	collector.LookupPR = func(branch string) reconcile.PRState {
		return reconcile.GhPRState(repo.Root, branch)
	}

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	action := find(reconcile.Reconcile(state), reconcile.KindPrune, checkout)
	if action == nil {
		t.Fatal("a squash-merged branch should be pruned on the PR verdict")
	}
	if action.Reason != "PR merged" {
		t.Errorf("Reason = %q, want \"PR merged\"", action.Reason)
	}
}

// gh missing or failing must not invent a verdict.
func TestGhPRStateToleratesFailure(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	herdrtest.FakeGhUnavailable(t)

	if got := reconcile.GhPRState(repo.Root, "anything"); got != reconcile.PRNone {
		t.Errorf("a failing gh should yield PRNone, got %q", got)
	}
}

// The distinction GhPRState folds away and GhPRLookup keeps: a consumer told
// only "PRNone" cannot tell a branch that was never opened as a pull request
// from a gh that is not logged in — and it is the second that makes prune keep
// everything. The two arrive as an empty array on a success and a non-zero
// exit, which is a contract and not a sentence, so this fake can speak it
// without pinning anyone's prose. TestGhPRLookupAgainstRealGh checks the real
// gh still keeps that contract.
func TestGhPRLookupTellsNoPullRequestFromNoAnswer(t *testing.T) {
	repo := herdrtest.NewRepo(t)

	t.Run("no pull request", func(t *testing.T) {
		herdrtest.FakeGhNoPR(t)

		state, err := reconcile.GhPRLookup(repo.Root, "wip")
		if err != nil {
			t.Errorf("a branch with no pull request is an answer, not a failure: %v", err)
		}
		if state != reconcile.PRNone {
			t.Errorf("state = %q, want PRNone", state)
		}
	})

	t.Run("gh could not be asked", func(t *testing.T) {
		herdrtest.FakeGh(t, `echo 'gh: To get started with GitHub CLI, please run: gh auth login' >&2; exit 4`)

		state, err := reconcile.GhPRLookup(repo.Root, "wip")
		if err == nil {
			t.Fatal("an unauthenticated gh must not read as no pull request")
		}
		// The message has to name what to do; the exit status alone does not.
		if !strings.Contains(err.Error(), "gh auth login") {
			t.Errorf("the error must quote what gh said, got %v", err)
		}
		// Still PRNone, because the reconciler's safe direction is to keep.
		if state != reconcile.PRNone {
			t.Errorf("state = %q, want PRNone", state)
		}
	})

	// And the fold itself: the reconciler asks through GhPRState and must see
	// both cases as the verdict that keeps the worktree.
	t.Run("GhPRState folds both", func(t *testing.T) {
		herdrtest.FakeGh(t, `echo 'gh auth login' >&2; exit 4`)

		if got := reconcile.GhPRState(repo.Root, "wip"); got != reconcile.PRNone {
			t.Errorf("GhPRState = %q, want PRNone", got)
		}
	})
}

// A branch can carry several pull requests — close one, push again, open
// another — and the open one is the verdict, because reading the closed one
// prunes a worktree that is still being worked in.
func TestGhPRLookupPrefersTheOpenPullRequest(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	herdrtest.FakeGhPRStates(t, "CLOSED", "OPEN")

	state, err := reconcile.GhPRLookup(repo.Root, "wip")
	if err != nil {
		t.Fatalf("GhPRLookup: %v", err)
	}
	if state != reconcile.PROpen {
		t.Errorf("state = %q, want PROpen", state)
	}
}

// The reused branch: merged once, pushed again, and the second pull request
// closed unmerged. The branch now holds the second attempt's commits, so the
// answer is CLOSED — and it has to be CLOSED in both array orders, because
// gh's list order is undocumented and reading MERGED prunes the checkout with
// the reason "PR merged", which is both wrong and reassuring.
func TestGhPRLookupPicksTheLatestPullRequest(t *testing.T) {
	repo := herdrtest.NewRepo(t)

	// When each state's pull request was opened: the first attempt in January,
	// the reuse — closed unmerged — in March. OPEN dates with the first, so a
	// subtest can pit it against a newer CLOSED.
	createdAt := map[string]string{
		"MERGED": "2026-01-05T10:00:00Z",
		"OPEN":   "2026-01-05T10:00:00Z",
		"CLOSED": "2026-03-09T10:00:00Z",
	}

	// fakeDated is FakeGhPRStates with the timestamps left in.
	fakeDated := func(t *testing.T, states ...string) {
		t.Helper()

		rows := make([]string, len(states))
		for i, state := range states {
			rows[i] = fmt.Sprintf(`{"state":%q,"createdAt":%q}`, state, createdAt[state])
		}
		herdrtest.FakeGh(t, herdrtest.CatScript(t, "["+strings.Join(rows, ",")+"]"))
	}

	wantClosed := func(t *testing.T) {
		t.Helper()

		state, err := reconcile.GhPRLookup(repo.Root, "fix/reused")
		if err != nil {
			t.Fatalf("GhPRLookup: %v", err)
		}
		if state != reconcile.PRClosed {
			t.Errorf("state = %q, want PRClosed: the branch's latest pull request "+
				"was closed unmerged and its commits exist nowhere else", state)
		}
	}

	for _, tc := range []struct {
		name   string
		states []string
	}{
		{"merged first", []string{"MERGED", "CLOSED"}},
		{"closed first", []string{"CLOSED", "MERGED"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("dated", func(t *testing.T) {
				fakeDated(t, tc.states...)

				wantClosed(t)
			})

			// With the field gone the tie-break carries the verdict alone, so
			// it has to carry it in both orders too: one order would pass on a
			// tie-break that had gone back to trusting gh's list order. Same
			// table as the dated case, so the two cannot drift apart.
			t.Run("no timestamps", func(t *testing.T) {
				herdrtest.FakeGhPRStates(t, tc.states...)

				wantClosed(t)
			})
		})
	}

	// The ordering rests on a field gh only sends when asked for it, and the
	// fake above answers whatever this file tells it to regardless of argv.
	t.Run("createdAt is asked for", func(t *testing.T) {
		argv := filepath.Join(t.TempDir(), "argv")
		herdrtest.FakeGh(t, `echo "$@" > `+argv+`; `+herdrtest.CatScript(t, "[]"))

		if _, err := reconcile.GhPRLookup(repo.Root, "fix/reused"); err != nil {
			t.Fatalf("GhPRLookup: %v", err)
		}
		asked, err := os.ReadFile(argv)
		if err != nil {
			t.Fatalf("read argv: %v", err)
		}
		if !strings.Contains(string(asked), "createdAt") {
			t.Errorf("gh was asked %q, which leaves the pull requests unordered",
				strings.TrimSpace(string(asked)))
		}
	})

	// An open pull request is still the answer even when an older one, because
	// it is what someone is working on now.
	t.Run("open outranks a newer closed", func(t *testing.T) {
		fakeDated(t, "OPEN", "CLOSED")

		state, err := reconcile.GhPRLookup(repo.Root, "fix/reused")
		if err != nil {
			t.Fatalf("GhPRLookup: %v", err)
		}
		if state != reconcile.PROpen {
			t.Errorf("state = %q, want PROpen", state)
		}
	})
}

// The verdict the ordering protects, end to end: the reconciler must keep this
// worktree, and say why.
func TestCollectKeepsReusedBranchClosedUnmerged(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("reused", "reused")
	repo.CommitIn(checkout, "first.txt", "landed")
	repo.Git("merge", "--no-ff", "-m", "merge first attempt", "reused")
	repo.CommitIn(checkout, "second.txt", "never merged anywhere")

	herdrtest.FakeGh(t, herdrtest.CatScript(t, `[{"state":"MERGED","createdAt":"2026-01-05T10:00:00Z"},`+
		`{"state":"CLOSED","createdAt":"2026-03-09T10:00:00Z"}]`))

	collector := collectFixture(t, repo,
		[]map[string]any{worktreeJSON(checkout, "reused", true, "")}, nil, nil, nil)
	collector.LookupPR = func(branch string) reconcile.PRState {
		return reconcile.GhPRState(repo.Root, branch)
	}

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	actions := reconcile.Reconcile(state)
	if action := find(actions, reconcile.KindPrune, checkout); action != nil {
		t.Fatalf("a branch whose latest pull request was closed unmerged must not be pruned, got %q", action.Reason)
	}
	keep := find(actions, reconcile.KindKeep, checkout)
	if keep == nil {
		t.Fatal("expected a keep for the reused branch")
	}
	if !strings.Contains(keep.Reason, "closed without merging") {
		t.Errorf("Reason = %q, want the closed-unmerged reason", keep.Reason)
	}
}

// The lookup has to ask about every state. gh's default filter is open-only,
// and under it a merged pull request comes back as the empty array that means
// "this branch was never opened as one" — which is prune's whole verdict read
// backwards.
func TestGhPRLookupAsksAboutEveryState(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	argv := filepath.Join(t.TempDir(), "argv")
	herdrtest.FakeGh(t, `echo "$@" > `+argv+`; `+herdrtest.GhPRScript(t))

	if _, err := reconcile.GhPRLookup(repo.Root, "wip"); err != nil {
		t.Fatalf("GhPRLookup: %v", err)
	}
	asked, err := os.ReadFile(argv)
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	if !strings.Contains(string(asked), "--state all") {
		t.Errorf("gh was asked %q, which omits merged and closed pull requests", strings.TrimSpace(string(asked)))
	}
}

// The fake above speaks whatever this file tells it to, so it cannot notice gh
// changing its mind. This one asks the gh that is actually installed, and fails
// when the contract the lookup rests on — an empty array and a zero exit for a
// branch with no pull request, a state for one that has them — stops holding.
// It is skipped without a gh that can reach GitHub, so CI and offline runs
// never see it.
func TestGhPRLookupAgainstRealGh(t *testing.T) {
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("no gh installed")
	}
	if err := exec.Command("gh", "auth", "status").Run(); err != nil {
		t.Skip("gh cannot reach GitHub")
	}
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	// A branch nobody has ever opened a pull request for.
	state, err := reconcile.GhPRLookup(root, "worktender/no-such-branch-8f3a1c")
	if err != nil {
		t.Errorf("gh no longer answers absence with an empty array: %v", err)
	}
	if state != reconcile.PRNone {
		t.Errorf("state = %q, want PRNone", state)
	}

	// And the other half: a branch that does have a pull request answers with a
	// state. Which branch is found at runtime rather than written down, because
	// no particular pull request is permanent — a branch can be reused and its
	// state change under a test that pinned one. The assertion is correspondingly
	// loose: some state, not a named one.
	if !strings.Contains(gitx.RemoteURL(root), "steig/worktender") {
		t.Skip("origin is not the upstream repository")
	}
	found, err := exec.Command("gh", "pr", "list", "--state", "all", "--limit", "1",
		"--json", "headRefName", "--jq", ".[].headRefName").Output()
	if err != nil {
		t.Skipf("gh could not name a pull request to ask about: %v", err)
	}
	branch := strings.TrimSpace(string(found))
	if branch == "" {
		t.Skip("the repository has no pull requests to ask about")
	}

	state, err = reconcile.GhPRLookup(root, branch)
	if err != nil {
		t.Fatalf("GhPRLookup %s: %v", branch, err)
	}
	if state == reconcile.PRNone {
		t.Errorf("state = PRNone for %s, which gh just listed a pull request for", branch)
	}
}

// An open PR keeps the worktree even when everything else looks finished.
func TestCollectKeepsWorktreeWithOpenPR(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("wip", "wip")
	repo.CommitIn(checkout, "wip.txt", "work")

	herdrtest.FakeGhPRState(t, "OPEN")

	collector := collectFixture(t, repo,
		[]map[string]any{worktreeJSON(checkout, "wip", true, "")}, nil, nil, nil)
	collector.LookupPR = func(branch string) reconcile.PRState {
		return reconcile.GhPRState(repo.Root, branch)
	}

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if find(reconcile.Reconcile(state), reconcile.KindPrune, checkout) != nil {
		t.Error("an open PR is active work")
	}
}

// The resume decision reads a real transcript directory.
func TestCollectDetectsPriorTranscript(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("resume-me", "resume-me")

	collector := collectFixture(t, repo,
		[]map[string]any{worktreeJSON(checkout, "resume-me", true, "w2")},
		[]map[string]any{{
			"workspace_id": "w2", "number": 2, "label": "resume-me", "focused": false,
			"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "idle",
			"worktree": map[string]any{"repo_key": "k", "repo_name": "repo",
				"repo_root": repo.Root, "checkout_path": checkout, "is_linked_worktree": true},
		}}, nil, map[string][]string{"w2": {"w2:p1"}})

	// Claude Code stores conversations under a slug of the absolute path.
	herdrtest.WriteFile(t, filepath.Join(collector.ProjectsDir,
		reconcile.TranscriptSlug(checkout), "session.jsonl"), "{}\n")

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	action := find(reconcile.Reconcile(state), reconcile.KindStaff, checkout)
	if action == nil {
		t.Fatal("an agentless workspace should be staffed")
	}
	if !action.Resume {
		t.Error("a worktree with a prior transcript should resume, not start cold")
	}
}

func TestCollectStartsColdWithoutTranscript(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("brand-new", "brand-new")

	collector := collectFixture(t, repo,
		[]map[string]any{worktreeJSON(checkout, "brand-new", true, "w2")},
		[]map[string]any{{
			"workspace_id": "w2", "number": 2, "label": "brand-new", "focused": false,
			"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "idle",
			"worktree": map[string]any{"repo_key": "k", "repo_name": "repo",
				"repo_root": repo.Root, "checkout_path": checkout, "is_linked_worktree": true},
		}}, nil, map[string][]string{"w2": {"w2:p1"}})

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	action := find(reconcile.Reconcile(state), reconcile.KindStaff, checkout)
	if action == nil {
		t.Fatal("an agentless workspace should be staffed")
	}
	if action.Resume {
		t.Error("no transcript means a cold start")
	}
}

// herdr reports resolved paths; the collector may have been handed the repo
// root with its symlinks intact. Comparing them unresolved drops every
// workspace, which turns sync into a silent no-op — nothing to staff, because
// nothing matched.
func TestCollectMatchesWorkspacesAcrossSymlinkedRoots(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("wip", "wip")

	if repo.RealRoot == repo.Root {
		t.Skip("temp dir is not behind a symlink on this machine")
	}
	resolvedCheckout := strings.Replace(checkout, repo.Root, repo.RealRoot, 1)

	collector := collectFixture(t, repo,
		[]map[string]any{worktreeJSON(checkout, "wip", true, "w2")},
		[]map[string]any{{
			"workspace_id": "w2", "number": 2, "label": "wip", "focused": false,
			"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "idle",
			// As herdr reports it: fully resolved.
			"worktree": map[string]any{"repo_key": "k", "repo_name": "repo",
				"repo_root": repo.RealRoot, "checkout_path": resolvedCheckout,
				"is_linked_worktree": true},
		}}, nil, map[string][]string{"w2": {"w2:p1"}})
	// As the invocation context supplies it: symlinks intact.
	collector.Root = repo.Root

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(state.Workspaces) != 1 {
		t.Fatalf("the workspace was dropped over a symlink difference: %+v", state.Workspaces)
	}
	if find(reconcile.Reconcile(state), reconcile.KindStaff, resolvedCheckout) == nil {
		t.Error("an agentless workspace should still be staffed")
	}
}

// Workspaces belonging to other repositories must not be touched.
func TestCollectIgnoresOtherRepositoriesWorkspaces(t *testing.T) {
	repo := herdrtest.NewRepo(t)

	collector := collectFixture(t, repo,
		[]map[string]any{worktreeJSON(repo.Root, "main", false, "w1")},
		[]map[string]any{{
			"workspace_id": "w9", "number": 9, "label": "elsewhere", "focused": false,
			"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "idle",
			"worktree": map[string]any{"repo_key": "other", "repo_name": "other",
				"repo_root": "/some/other/repo", "checkout_path": "/some/other/repo/wt/x",
				"is_linked_worktree": true},
		}}, nil, map[string][]string{"w9": {"w9:p1"}})

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(state.Workspaces) != 0 {
		t.Errorf("another repository's workspace leaked in: %+v", state.Workspaces)
	}
}

// A workspace herdr has just listed can be closed before its panes are read.
// Failing the whole repository over that meant one vanished workspace hid every
// other worktree's verdict — seen live as `prune` exiting 1 having printed
// nothing but its header, immediately after a `sync` that opened the workspace.
func TestCollectSkipsAWorkspaceThatDisappears(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	survivor := repo.AddWorktree("survivor", "survivor")

	workspace := func(id, checkout string) map[string]any {
		return map[string]any{
			"workspace_id": id, "number": 1, "label": id, "focused": false,
			"pane_count": 1, "tab_count": 1, "active_tab_id": id + ":t1", "agent_status": "idle",
			"worktree": map[string]any{"repo_key": "k", "repo_name": "repo",
				"repo_root": repo.Root, "checkout_path": checkout,
				"is_linked_worktree": true},
		}
	}

	server := herdrtest.NewServer(t)
	server.HandleResult("worktree.list", map[string]any{
		"type": "worktree_list",
		"source": map[string]any{"repo_key": "k", "repo_name": "repo",
			"repo_root": repo.Root, "source_checkout_path": repo.Root},
		"worktrees": []map[string]any{
			worktreeJSON(repo.Root, "main", false, "w1"),
			worktreeJSON(survivor, "survivor", true, "w2"),
		},
	})
	server.HandleResult("workspace.list", map[string]any{
		"type": "workspace_list",
		"workspaces": []map[string]any{
			workspace("w20", filepath.Join(repo.Root, "gone")),
			workspace("w2", survivor),
		},
	})
	server.HandleResult("agent.list", map[string]any{
		"type": "agent_list", "agents": []map[string]any{},
	})
	server.Handle("pane.list", func(params map[string]any) (any, error) {
		if params["workspace_id"] == "w20" {
			return nil, &herdrtest.CodedError{
				Code: "workspace_not_found", Message: "workspace w20 not found",
			}
		}
		return map[string]any{"type": "pane_list",
			"panes": []map[string]any{{"pane_id": "w2:p1"}}}, nil
	})

	var warnings strings.Builder
	collector := reconcile.NewCollector(herdrapi.NewWithSocket(server.SocketPath), repo.Root)
	collector.ProjectsDir = t.TempDir()
	collector.LookupPR = func(string) reconcile.PRState { return reconcile.PRNone }
	collector.Warn = &warnings

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("one vanished workspace must not fail the repository: %v", err)
	}
	if len(state.Workspaces) != 1 || state.Workspaces[0].ID != "w2" {
		t.Fatalf("the surviving workspace should still be collected, got %+v", state.Workspaces)
	}
	if !strings.Contains(warnings.String(), "w20") {
		t.Errorf("a skipped workspace must be named, got %q", warnings.String())
	}
}

// Every other herdr failure still means the repository's state is unknown, so
// the skip must not have widened into a blanket ignore-on-error.
func TestCollectStillFailsOnOtherPaneErrors(t *testing.T) {
	repo := herdrtest.NewRepo(t)

	server := herdrtest.NewServer(t)
	server.HandleResult("worktree.list", map[string]any{
		"type": "worktree_list",
		"source": map[string]any{"repo_key": "k", "repo_name": "repo",
			"repo_root": repo.Root, "source_checkout_path": repo.Root},
		"worktrees": []map[string]any{worktreeJSON(repo.Root, "main", false, "w1")},
	})
	server.HandleResult("workspace.list", map[string]any{
		"type": "workspace_list",
		"workspaces": []map[string]any{{
			"workspace_id": "w1", "number": 1, "label": "main", "focused": false,
			"pane_count": 1, "tab_count": 1, "active_tab_id": "w1:t1", "agent_status": "idle",
			"worktree": map[string]any{"repo_key": "k", "repo_name": "repo",
				"repo_root": repo.Root, "checkout_path": repo.Root,
				"is_linked_worktree": false},
		}},
	})
	server.HandleResult("agent.list", map[string]any{
		"type": "agent_list", "agents": []map[string]any{},
	})
	server.Handle("pane.list", func(map[string]any) (any, error) {
		return nil, &herdrtest.CodedError{Code: "internal", Message: "herdr fell over"}
	})

	collector := reconcile.NewCollector(herdrapi.NewWithSocket(server.SocketPath), repo.Root)
	collector.ProjectsDir = t.TempDir()
	collector.LookupPR = func(string) reconcile.PRState { return reconcile.PRNone }

	if _, err := collector.Collect(); err == nil {
		t.Fatal("an unrecognised pane.list failure must still fail the collection")
	}
}

// Herdr absent: the checkouts come from git and every workspace-shaped fact is
// empty. The prune guards are already entirely git and gh, so the verdicts do
// not change — only the enumeration had ever needed herdr.
func TestCollectWithoutHerdrReadsTheWorktreesFromGit(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("wip", "wip")

	c := &reconcile.Collector{Client: nil, Root: repo.Root, ProjectsDir: t.TempDir()}
	state, err := c.Collect()
	if err != nil {
		t.Fatalf("Collect with no herdr: %v", err)
	}

	if len(state.Worktrees) != 2 {
		t.Fatalf("want the main checkout and one linked, got %d: %+v", len(state.Worktrees), state.Worktrees)
	}
	// The field the design note is about, and the only thing in this State that
	// is not obvious from the code beside it: Workspaces is never assigned and
	// AgentPanes is an empty literal four lines up, so asserting those would
	// restate the constructor. HerdrAbsent is what adopt reads.
	if !state.HerdrAbsent {
		t.Error("the collector took the herdr-free path; the state has to record it")
	}

	var linked *reconcile.Worktree
	for i := range state.Worktrees {
		if gitx.Resolve(state.Worktrees[i].Path) == gitx.Resolve(checkout) {
			linked = &state.Worktrees[i]
		}
	}
	if linked == nil {
		t.Fatalf("the linked checkout is missing: %+v", state.Worktrees)
	}
	if linked.Branch != "wip" {
		t.Errorf("branch = %q, want wip", linked.Branch)
	}
	if linked.WorkspaceID != "" {
		t.Errorf("no herdr means no workspace id, got %q", linked.WorkspaceID)
	}
}

// With herdr absent there is nothing to adopt into, so a herdr-free pass must
// plan no adoptions: every checkout reads as unadopted, and ungated the whole
// repository is planned against a herdr that is not listening.
//
// Only adopt is asserted. staff and ghosts both iterate state.Workspaces, which
// is empty here whatever HerdrAbsent says, so asserting they plan nothing would
// pass with reconcile's gate deleted — it would be testing the fixture.
func TestReconcileWithoutHerdrPlansNoAdopt(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	repo.AddWorktree("wip", "wip")

	c := &reconcile.Collector{Client: nil, Root: repo.Root, ProjectsDir: t.TempDir()}
	state, err := c.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(reconcile.Only(reconcile.Reconcile(state), reconcile.KindAdopt)) != 0 {
		t.Errorf("nothing to adopt into with herdr absent: %+v", reconcile.Reconcile(state))
	}

	// The same worktrees with herdr merely not having opened them yet is the
	// state adopt exists for, and it must still fire — this is what makes
	// HerdrAbsent a recorded fact rather than an inference from an empty
	// Workspaces, which these two states share.
	state.HerdrAbsent = false
	if len(reconcile.Only(reconcile.Reconcile(state), reconcile.KindAdopt)) == 0 {
		t.Error("an unopened checkout with herdr running is exactly what adopt is for")
	}
}
