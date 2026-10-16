package reconcile_test

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/reconcile"
)

// linked builds a linked worktree that would otherwise be prunable, so each
// test can flip exactly the one field it is about.
func linked(path, branch string) reconcile.Worktree {
	return reconcile.Worktree{
		Path: path, Branch: branch, IsLinked: true,
		OwnCommits: 3, PR: reconcile.PRMerged,
	}
}

func find(actions []reconcile.Action, kind reconcile.Kind, path string) *reconcile.Action {
	for i := range actions {
		if actions[i].Kind == kind && actions[i].Path == path {
			return &actions[i]
		}
	}
	return nil
}

func TestAdoptsWorktreeWithNoWorkspace(t *testing.T) {
	state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{
		{Path: "/repo/wt/a", Branch: "a", IsLinked: true},
		{Path: "/repo/wt/b", Branch: "b", IsLinked: true, WorkspaceID: "w2"},
		{Path: "/repo/bare", IsBare: true},
	}}

	actions := reconcile.Reconcile(state)

	if find(actions, reconcile.KindAdopt, "/repo/wt/a") == nil {
		t.Error("worktree with no workspace should be adopted")
	}
	if find(actions, reconcile.KindAdopt, "/repo/wt/b") != nil {
		t.Error("worktree that already has a workspace should not be adopted")
	}
	if find(actions, reconcile.KindAdopt, "/repo/bare") != nil {
		t.Error("a bare worktree has no working tree to open")
	}
}

func TestStaffsAgentlessWorkspace(t *testing.T) {
	state := reconcile.State{
		Root:      "/repo",
		Base:      "main",
		Worktrees: []reconcile.Worktree{{Path: "/repo/wt/a", Branch: "a", IsLinked: true, WorkspaceID: "w2"}},
		Workspaces: []reconcile.Workspace{
			{ID: "w2", CheckoutPath: "/repo/wt/a", IsLinked: true, PaneIDs: []string{"w2:p1", "w2:p2"}},
		},
		AgentPanes: map[string]reconcile.AgentState{},
	}

	action := find(reconcile.Reconcile(state), reconcile.KindStaff, "/repo/wt/a")
	if action == nil {
		t.Fatal("agentless workspace should be staffed")
	}
	if action.PaneID != "w2:p1" {
		t.Errorf("should staff the first pane, got %q", action.PaneID)
	}
	if action.WorkspaceID != "w2" {
		t.Errorf("WorkspaceID = %q, want w2", action.WorkspaceID)
	}
	if want := reconcile.AgentName("/repo", "a"); action.AgentName != want {
		t.Errorf("AgentName = %q, want %q", action.AgentName, want)
	}
}

// The staffing name must carry the repository, not just the checkout: herdr's
// agent namespace is global, so two repositories with a worktree called `api`
// would otherwise ask for one name and the second would be refused.
func TestStaffingNameSeparatesRepositories(t *testing.T) {
	nameFor := func(root string) string {
		state := reconcile.State{
			Root:      root,
			Base:      "main",
			Worktrees: []reconcile.Worktree{{Path: root + "/wt/api", Branch: "api", IsLinked: true, WorkspaceID: "w2"}},
			Workspaces: []reconcile.Workspace{
				{ID: "w2", CheckoutPath: root + "/wt/api", IsLinked: true, PaneIDs: []string{"w2:p1"}},
			},
			AgentPanes: map[string]reconcile.AgentState{},
		}
		action := find(reconcile.Reconcile(state), reconcile.KindStaff, root+"/wt/api")
		if action == nil {
			t.Fatalf("workspace in %s should be staffed", root)
		}
		return action.AgentName
	}

	first, second := nameFor("/one"), nameFor("/two")
	if first == second {
		t.Errorf("both repositories staff an agent called %q; herdr refuses the second with agent_name_taken", first)
	}
	if !strings.HasPrefix(first, "api-") || !strings.HasPrefix(second, "api-") {
		t.Errorf("names %q and %q should still read as the checkout they staff", first, second)
	}
}

func TestDoesNotStaffWorkspaceThatAlreadyHasAnAgent(t *testing.T) {
	state := reconcile.State{
		Base:      "main",
		Worktrees: []reconcile.Worktree{{Path: "/repo/wt/a", Branch: "a", IsLinked: true, WorkspaceID: "w2"}},
		Workspaces: []reconcile.Workspace{
			// The agent is in the second pane, so a first-pane-only check
			// would wrongly restaff this workspace.
			{ID: "w2", CheckoutPath: "/repo/wt/a", IsLinked: true, PaneIDs: []string{"w2:p1", "w2:p2"}},
		},
		AgentPanes: map[string]reconcile.AgentState{"w2:p2": reconcile.AgentWorking},
	}

	if find(reconcile.Reconcile(state), reconcile.KindStaff, "/repo/wt/a") != nil {
		t.Error("a workspace with an agent in any pane is already staffed")
	}
}

// The main checkout is the user's own workspace, not something to staff.
func TestDoesNotStaffMainCheckoutWorkspace(t *testing.T) {
	state := reconcile.State{
		Base: "main",
		Workspaces: []reconcile.Workspace{
			{ID: "w1", CheckoutPath: "/repo", IsLinked: false, PaneIDs: []string{"w1:p1"}},
		},
		AgentPanes: map[string]reconcile.AgentState{},
	}

	if find(reconcile.Reconcile(state), reconcile.KindStaff, "/repo") != nil {
		t.Error("the main checkout should not be auto-staffed")
	}
}

func TestStaffResumesWhenATranscriptExists(t *testing.T) {
	for _, tc := range []struct {
		name       string
		transcript bool
		wantResume bool
	}{
		{"prior conversation", true, true},
		{"no prior conversation", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := reconcile.State{
				Base: "main",
				Worktrees: []reconcile.Worktree{
					{Path: "/repo/wt/a", Branch: "a", IsLinked: true, WorkspaceID: "w2", HasTranscript: tc.transcript},
				},
				Workspaces: []reconcile.Workspace{
					{ID: "w2", CheckoutPath: "/repo/wt/a", IsLinked: true, PaneIDs: []string{"w2:p1"}},
				},
				AgentPanes: map[string]reconcile.AgentState{},
			}

			action := find(reconcile.Reconcile(state), reconcile.KindStaff, "/repo/wt/a")
			if action == nil {
				t.Fatal("expected a staff action")
			}
			if action.Resume != tc.wantResume {
				t.Errorf("Resume = %v, want %v", action.Resume, tc.wantResume)
			}
		})
	}
}

// A merged PR is the only signal that removes anything.
func TestPrunesFinishedWork(t *testing.T) {
	w := linked("/repo/wt/a", "a")
	w.PR = reconcile.PRMerged
	state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{w}}

	action := find(reconcile.Reconcile(state), reconcile.KindPrune, "/repo/wt/a")
	if action == nil {
		t.Fatalf("expected a prune action, got %+v", reconcile.Reconcile(state))
	}
	if action.Reason != "PR merged" {
		t.Errorf("Reason = %q, want \"PR merged\"", action.Reason)
	}
}

// Guard a. This work exists nowhere else.
func TestNeverPrunesDirtyWorktree(t *testing.T) {
	w := linked("/repo/wt/a", "a")
	w.Dirty = true
	state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{w}}

	actions := reconcile.Reconcile(state)
	if find(actions, reconcile.KindPrune, "/repo/wt/a") != nil {
		t.Fatal("a dirty worktree must never be pruned, merged PR or not")
	}
	if keep := find(actions, reconcile.KindKeep, "/repo/wt/a"); keep == nil || keep.Reason != "uncommitted changes" {
		t.Errorf("expected an explanatory keep, got %+v", keep)
	}
}

// Guard b. In-flight work, whatever the branch looks like.
func TestNeverPrunesWorktreeHostingALiveAgent(t *testing.T) {
	w := linked("/repo/wt/a", "a")
	w.WorkspaceID = "w2"
	state := reconcile.State{
		Base:      "main",
		Worktrees: []reconcile.Worktree{w},
		Workspaces: []reconcile.Workspace{
			{ID: "w2", CheckoutPath: "/repo/wt/a", IsLinked: true, PaneIDs: []string{"w2:p1"}},
		},
		AgentPanes: map[string]reconcile.AgentState{"w2:p1": reconcile.AgentWorking},
	}

	actions := reconcile.Reconcile(state)
	if find(actions, reconcile.KindPrune, "/repo/wt/a") != nil {
		t.Fatal("a worktree whose pane hosts a live agent must never be pruned")
	}
	if keep := find(actions, reconcile.KindKeep, "/repo/wt/a"); keep == nil || keep.Reason != "agent running" {
		t.Errorf("expected an explanatory keep, got %+v", keep)
	}
}

// occupied is the state the whole release question is about: a worktree whose
// work has landed, held open by a workspace whose one pane still hosts an agent
// in the given state.
func occupied(path, branch string, status reconcile.AgentState) reconcile.State {
	w := linked(path, branch)
	w.WorkspaceID = "w2"
	return reconcile.State{
		Base:      "main",
		Worktrees: []reconcile.Worktree{w},
		Workspaces: []reconcile.Workspace{
			{ID: "w2", CheckoutPath: path, IsLinked: true, PaneIDs: []string{"w2:p1"}},
		},
		AgentPanes: map[string]reconcile.AgentState{"w2:p1": status},
	}
}

// The issue this is all for. A dispatched worker that finished still occupies
// its pane — herdr frees an agent only when the pane goes away — so read as
// presence, guard b makes the ordinary end state of a successful dispatch a
// worktree nothing can ever remove. It is still kept by default, but the line
// has to say what would remove it rather than read as live work.
func TestKeepingAFinishedAgentsWorktreeSaysWhatWouldRemoveIt(t *testing.T) {
	for _, status := range []reconcile.AgentState{reconcile.AgentIdle, reconcile.AgentDone} {
		t.Run(string(status), func(t *testing.T) {
			actions := reconcile.Reconcile(occupied("/repo/wt/a", "a", status))

			if find(actions, reconcile.KindPrune, "/repo/wt/a") != nil {
				t.Fatal("without --release-agents this is still a keep")
			}
			keep := find(actions, reconcile.KindKeep, "/repo/wt/a")
			if keep == nil {
				t.Fatal("expected an explanatory keep")
			}
			if keep.Reason == "agent running" {
				t.Error("a finished agent must not be reported as work in flight")
			}
			if !strings.Contains(keep.Reason, "PR merged") {
				t.Errorf("the reason should still say the work landed, got %q", keep.Reason)
			}
			if !strings.Contains(keep.Reason, "--release-agents") {
				t.Errorf("a keep nothing can act on is the bug; got %q", keep.Reason)
			}
		})
	}
}

func TestPrunesAFinishedAgentsWorktreeWhenAskedTo(t *testing.T) {
	state := occupied("/repo/wt/a", "a", reconcile.AgentIdle)
	state.ReleaseAgents = true

	action := find(reconcile.Reconcile(state), reconcile.KindPrune, "/repo/wt/a")
	if action == nil {
		t.Fatal("--release-agents is what gives the keep somewhere to go")
	}
	if !action.ReleasesAgents {
		t.Error("the executor has to know this removal is also taking a pane away")
	}
	if action.Reason != "PR merged" {
		t.Errorf("Reason = %q; the authority for the removal is unchanged", action.Reason)
	}
}

// --release-agents reaches an agent that has stopped, and nothing else. An
// unrecognised status is busy for the reason an unreadable guard is unsatisfied:
// this build cannot tell what it means.
func TestNeverReleasesAnAgentThatIsNotFinished(t *testing.T) {
	for _, status := range []reconcile.AgentState{
		reconcile.AgentWorking, reconcile.AgentBlocked, reconcile.AgentUnknown, "compacting", "",
	} {
		t.Run(string(status), func(t *testing.T) {
			state := occupied("/repo/wt/a", "a", status)
			state.ReleaseAgents = true

			actions := reconcile.Reconcile(state)
			if find(actions, reconcile.KindPrune, "/repo/wt/a") != nil {
				t.Fatal("asking to release finished agents must not reach one that is not")
			}
			if keep := find(actions, reconcile.KindKeep, "/repo/wt/a"); keep == nil || keep.Reason != "agent running" {
				t.Errorf("expected the ordinary agent keep, got %+v", keep)
			}
		})
	}
}

// A workspace holding a finished agent beside a working one has live work in
// it. Reporting the finished one would be answering with the pane that suits
// the verdict rather than the one that is true.
func TestABusyPaneOutweighsAFinishedOneInTheSameWorkspace(t *testing.T) {
	state := occupied("/repo/wt/a", "a", reconcile.AgentIdle)
	state.Workspaces[0].PaneIDs = []string{"w2:p1", "w2:p2"}
	state.AgentPanes["w2:p2"] = reconcile.AgentWorking
	state.ReleaseAgents = true

	actions := reconcile.Reconcile(state)
	if find(actions, reconcile.KindPrune, "/repo/wt/a") != nil {
		t.Fatal("a workspace with a working agent in any pane is not free to remove")
	}
	if keep := find(actions, reconcile.KindKeep, "/repo/wt/a"); keep == nil || keep.Reason != "agent running" {
		t.Errorf("expected the ordinary agent keep, got %+v", keep)
	}
}

// Releasing agents removes a reason to keep; it is not a second authority to
// remove. Everything the verdict decides still decides it.
func TestReleasingAgentsDoesNotWidenTheVerdict(t *testing.T) {
	state := occupied("/repo/wt/a", "a", reconcile.AgentIdle)
	state.Worktrees[0].PR = reconcile.PROpen
	state.ReleaseAgents = true

	actions := reconcile.Reconcile(state)
	if find(actions, reconcile.KindPrune, "/repo/wt/a") != nil {
		t.Fatal("an open pull request is unfinished work, agent or no agent")
	}
	keep := find(actions, reconcile.KindKeep, "/repo/wt/a")
	if keep == nil || keep.Reason != "still open" {
		t.Errorf("expected the verdict's own reason, got %+v", keep)
	}
}

// The other half of telling attached from busy: staffing still asks only
// whether a pane is occupied. A finished agent is sitting at a prompt, and a
// second agent started on top of it lands on that conversation.
func TestAFinishedAgentStillCountsAsStaffed(t *testing.T) {
	state := occupied("/repo/wt/a", "a", reconcile.AgentDone)

	if find(reconcile.Reconcile(state), reconcile.KindStaff, "/repo/wt/a") != nil {
		t.Error("a workspace whose agent has finished is occupied, not empty")
	}
}

// Guard c. A fresh branch is trivially an ancestor of base; unstarted is not
// finished.
func TestNeverPrunesBranchWithNoCommitsOfItsOwn(t *testing.T) {
	state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{
		{Path: "/repo/wt/fresh", Branch: "fresh", IsLinked: true, OwnCommits: 0},
	}}

	actions := reconcile.Reconcile(state)
	if find(actions, reconcile.KindPrune, "/repo/wt/fresh") != nil {
		t.Fatal("a branch with no commits of its own has not started, let alone finished")
	}
	if keep := find(actions, reconcile.KindKeep, "/repo/wt/fresh"); keep == nil {
		t.Error("expected an explanatory keep")
	}
}

// A closed-but-unmerged PR is abandoned work, not finished work: the branch
// still holds commits that exist nowhere else. It must be surfaced for a human
// to decide, never auto-pruned.
func TestNeverPrunesBranchWithAClosedPR(t *testing.T) {
	w := linked("/repo/wt/a", "a")
	w.PR = reconcile.PRClosed
	state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{w}}

	actions := reconcile.Reconcile(state)
	if find(actions, reconcile.KindPrune, "/repo/wt/a") != nil {
		t.Fatal("a closed PR is abandoned work, not landed work")
	}

	keep := find(actions, reconcile.KindKeep, "/repo/wt/a")
	if keep == nil {
		t.Fatal("a closed PR must still be surfaced")
	}
	if !strings.Contains(keep.Reason, "closed") {
		t.Errorf("the reason must say the PR was closed so a human can decide, got %q", keep.Reason)
	}
}

// Topology cannot tell a merged branch from a branch forked off merged work:
// both point at the same commit. Where the PR is silent, keep.
func TestNeverPrunesOnTopologyAlone(t *testing.T) {
	state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{
		{Path: "/repo/wt/looks-merged", Branch: "looks-merged", IsLinked: true,
			OwnCommits: 0, MergedIntoBase: true, PR: reconcile.PRNone},
	}}

	actions := reconcile.Reconcile(state)
	if find(actions, reconcile.KindPrune, "/repo/wt/looks-merged") != nil {
		t.Fatal("git topology alone must never be enough to remove a worktree")
	}

	keep := find(actions, reconcile.KindKeep, "/repo/wt/looks-merged")
	if keep == nil {
		t.Fatal("expected a keep")
	}
	// The ambiguity has to be visible, not dressed up as a confident verdict.
	if !strings.Contains(keep.Reason, "cannot tell") {
		t.Errorf("the reason should admit the ambiguity, got %q", keep.Reason)
	}
}

// A merged PR is authoritative and prunes whatever shape the graph is in.
func TestPrunesOnAMergedPRRegardlessOfTopology(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    reconcile.Worktree
	}{
		{"merge commit", reconcile.Worktree{OwnCommits: 0, MergedIntoBase: true}},
		{"fast-forward", reconcile.Worktree{OwnCommits: 0, MergedIntoBase: false}},
		{"squash, commits still local", reconcile.Worktree{OwnCommits: 3, MergedIntoBase: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := tc.w
			w.Path, w.Branch, w.IsLinked, w.PR = "/repo/wt/a", "a", true, reconcile.PRMerged
			state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{w}}

			if find(reconcile.Reconcile(state), reconcile.KindPrune, "/repo/wt/a") == nil {
				t.Fatal("a merged PR is authoritative")
			}
		})
	}
}

// A fast-forward merge leaves no trace distinguishing it from a branch that
// never started, so the reason must not claim the branch is unstarted.
func TestZeroCommitReasonDoesNotClaimUnstarted(t *testing.T) {
	state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{
		{Path: "/repo/wt/maybe", Branch: "maybe", IsLinked: true, OwnCommits: 0},
	}}

	keep := find(reconcile.Reconcile(state), reconcile.KindKeep, "/repo/wt/maybe")
	if keep == nil {
		t.Fatal("expected a keep")
	}
	if !strings.Contains(keep.Reason, "cannot tell") {
		t.Errorf("a fast-forwarded branch looks identical to an unstarted one; "+
			"the reason must not assert one of them, got %q", keep.Reason)
	}
}

// An open PR is active work even when git thinks base contains the commits.
func TestNeverPrunesBranchWithAnOpenPR(t *testing.T) {
	w := linked("/repo/wt/a", "a")
	w.PR = reconcile.PROpen
	w.MergedIntoBase = true
	state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{w}}

	actions := reconcile.Reconcile(state)
	if find(actions, reconcile.KindPrune, "/repo/wt/a") != nil {
		t.Fatal("an open PR is still active work")
	}
	if keep := find(actions, reconcile.KindKeep, "/repo/wt/a"); keep == nil || keep.Reason != "still open" {
		t.Errorf("expected keep \"still open\", got %+v", keep)
	}
}

// The main checkout is the repository itself.
func TestNeverPrunesMainCheckout(t *testing.T) {
	state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{
		{Path: "/repo", Branch: "main", IsLinked: false, OwnCommits: 5, PR: reconcile.PRMerged},
	}}

	for _, a := range reconcile.Reconcile(state) {
		if a.Kind == reconcile.KindPrune {
			t.Fatalf("the main checkout must never be pruned: %+v", a)
		}
	}
}

// Adoptions have to land before staffing, and prunes last.
func TestActionsAreOrderedForExecution(t *testing.T) {
	state := reconcile.State{
		Base: "main",
		Worktrees: []reconcile.Worktree{
			{Path: "/repo/wt/new", Branch: "new", IsLinked: true},
			// Has a workspace id herdr has since closed, so it is neither
			// adopted nor staffed — only pruned.
			{Path: "/repo/wt/old", Branch: "old", IsLinked: true, WorkspaceID: "w4",
				OwnCommits: 2, PR: reconcile.PRMerged},
			{Path: "/repo/wt/bare", Branch: "bare", IsLinked: true, WorkspaceID: "w3"},
		},
		Workspaces: []reconcile.Workspace{
			{ID: "w3", CheckoutPath: "/repo/wt/bare", IsLinked: true, PaneIDs: []string{"w3:p1"}},
		},
		AgentPanes: map[string]reconcile.AgentState{},
	}

	var order []reconcile.Kind
	for _, a := range reconcile.Reconcile(state) {
		if a.Kind != reconcile.KindKeep {
			order = append(order, a.Kind)
		}
	}

	want := []reconcile.Kind{reconcile.KindAdopt, reconcile.KindStaff, reconcile.KindPrune}
	if len(order) != len(want) {
		t.Fatalf("got %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("got %v, want %v", order, want)
		}
	}
}

// A worktree adopted in this pass has no workspace yet, so it cannot also be
// staffed until the next one. Reconcile converges rather than guessing ids.
func TestAdoptedWorktreeIsNotAlsoStaffedInTheSamePass(t *testing.T) {
	state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{
		{Path: "/repo/wt/new", Branch: "new", IsLinked: true},
	}}

	actions := reconcile.Reconcile(state)
	if find(actions, reconcile.KindAdopt, "/repo/wt/new") == nil {
		t.Fatal("expected an adopt")
	}
	if find(actions, reconcile.KindStaff, "/repo/wt/new") != nil {
		t.Error("nothing can be staffed before its workspace exists")
	}
}

func TestReconcileIsPure(t *testing.T) {
	state := reconcile.State{
		Base: "main",
		Worktrees: []reconcile.Worktree{
			{Path: "/repo/wt/a", Branch: "a", IsLinked: true, OwnCommits: 1, PR: reconcile.PRMerged},
		},
		AgentPanes: map[string]reconcile.AgentState{},
	}

	first := reconcile.Reconcile(state)
	second := reconcile.Reconcile(state)

	if len(first) != len(second) {
		t.Fatalf("same input gave different output: %d vs %d actions", len(first), len(second))
	}
	for i := range first {
		// DeepEqual rather than !=: Action carries AgentArgs now, so it is no
		// longer a comparable struct.
		if !reflect.DeepEqual(first[i], second[i]) {
			t.Errorf("action %d differs:\n%+v\n%+v", i, first[i], second[i])
		}
	}
	if len(state.Worktrees) != 1 || state.Worktrees[0].Path != "/repo/wt/a" {
		t.Error("Reconcile mutated its input")
	}
}

func TestOnlyKeepsRequestedKindsInOrder(t *testing.T) {
	actions := []reconcile.Action{
		{Kind: reconcile.KindAdopt, Path: "a"},
		{Kind: reconcile.KindPrune, Path: "b"},
		{Kind: reconcile.KindStaff, Path: "c"},
		{Kind: reconcile.KindKeep, Path: "d"},
		{Kind: reconcile.KindPrune, Path: "e"},
	}

	got := reconcile.Only(actions, reconcile.KindPrune, reconcile.KindKeep)
	if len(got) != 3 {
		t.Fatalf("want 3 actions, got %d: %+v", len(got), got)
	}
	for i, wantPath := range []string{"b", "d", "e"} {
		if got[i].Path != wantPath {
			t.Errorf("action %d = %q, want %q (order must be preserved)", i, got[i].Path, wantPath)
		}
	}

	if got := reconcile.Only(actions); len(got) != 0 {
		t.Errorf("Only with no kinds should keep nothing, got %+v", got)
	}
}

func TestSlug(t *testing.T) {
	for _, tc := range []struct{ in, slug string }{
		{"fix-auth", "fix-auth"},
		{"Feat/249-Segments UI", "feat-249-segments-ui"},
		{"249-segments", "249-segments"},
		{"--weird--", "weird"},
	} {
		if got := reconcile.Slug(tc.in); got != tc.slug {
			t.Errorf("Slug(%q) = %q, want %q", tc.in, got, tc.slug)
		}
	}
}

// herdr's pattern for an agent name, which is the whole of what it will accept.
var herdrAgentName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func TestAgentNameMatchesWhatHerdrAccepts(t *testing.T) {
	for _, label := range []string{
		"fix-auth",
		"Feat/249-Segments UI",
		"249-segments",
		"--weird--",
		"",
		"a-really-very-extremely-long-branch-name-that-keeps-going",
		"1234567890123456789012345678901234567890",
	} {
		got := reconcile.AgentName("/repo", label)
		if !herdrAgentName.MatchString(got) {
			t.Errorf("AgentName(%q) = %q, which herdr will not accept", label, got)
		}
	}
}

// The readable half is still the checkout, so a name in `herdr agent list` says
// which branch it belongs to without being looked up.
func TestAgentNameLeadsWithTheLabel(t *testing.T) {
	for _, tc := range []struct{ label, head string }{
		{"fix-auth", "fix-auth-"},
		{"Feat/249-Segments UI", "feat-249-segments-ui-"},
		// An issue branch starts with its number, and herdr demands a letter.
		{"249-segments", "wt-249-segments-"},
		{"", "wt-"},
	} {
		if got := reconcile.AgentName("/repo", tc.label); !strings.HasPrefix(got, tc.head) {
			t.Errorf("AgentName(%q) = %q, want it to start with %q", tc.label, got, tc.head)
		}
	}
}

// The collision the name exists to avoid: herdr's agent namespace is global and
// it refuses a duplicate with agent_name_taken, so a name derived from a
// checkout basename or an issue number alone loses the second repository.
func TestAgentNameSeparatesRepositories(t *testing.T) {
	for _, label := range []string{"api", "12-fix-the-thing"} {
		one := reconcile.AgentName("/code/one", label)
		two := reconcile.AgentName("/code/two", label)
		if one == two {
			t.Errorf("two repositories both want the agent name %q for %q", one, label)
		}
	}
}

// The other half of the same defect: the 32-character limit truncates, so two
// long branches of one repository must not converge on one name.
func TestAgentNameSeparatesBranchesThatShareALongPrefix(t *testing.T) {
	one := reconcile.AgentName("/repo", "refactor-the-authentication-middleware")
	two := reconcile.AgentName("/repo", "refactor-the-authentication-later")
	if one == two {
		t.Errorf("two branches both want the agent name %q", one)
	}
}

// Re-deriving is the only way `sync` can name an existing worktree's agent, so
// the same worktree must always produce the same name.
func TestAgentNameIsStable(t *testing.T) {
	if one, two := reconcile.AgentName("/repo", "fix-auth"), reconcile.AgentName("/repo", "fix-auth"); one != two {
		t.Errorf("AgentName is not stable: %q then %q", one, two)
	}
}

// Claude Code's directory name: every "/" and "." becomes "-".
func TestTranscriptSlug(t *testing.T) {
	got := reconcile.TranscriptSlug("/Users/t/code/repo/.claude/worktrees/fix-auth")
	want := "-Users-t-code-repo--claude-worktrees-fix-auth"
	if got != want {
		t.Errorf("TranscriptSlug = %q, want %q", got, want)
	}
}

// symlinkedPair returns two spellings of one directory: the real path and a
// path reaching it through a symlink. It is how herdr can name one checkout two
// ways, which is the join this guard has to survive.
func symlinkedPair(t *testing.T) (real, linked string) {
	t.Helper()

	real = t.TempDir()
	linked = filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, linked); err != nil {
		t.Skipf("this filesystem will not make a symlink: %v", err)
	}
	return real, linked
}

// Guard b again, with the two sides spelled differently. herdr reports the
// checkout resolved in one call and unresolved in another, so a join by raw
// string equality misses and the agent guard never fires — the failure this
// whole project exists to prevent.
func TestNeverPrunesWorktreeHostingALiveAgentUnderADifferentSpelling(t *testing.T) {
	real, link := symlinkedPair(t)

	w := linked(link, "a")
	state := reconcile.State{
		Base:      "main",
		Worktrees: []reconcile.Worktree{w},
		Workspaces: []reconcile.Workspace{
			{ID: "w2", CheckoutPath: real, IsLinked: true, PaneIDs: []string{"w2:p1"}},
		},
		AgentPanes: map[string]reconcile.AgentState{"w2:p1": reconcile.AgentWorking},
	}

	actions := reconcile.Reconcile(state)
	if find(actions, reconcile.KindPrune, link) != nil {
		t.Fatal("a worktree whose pane hosts a live agent must never be pruned, however its path is spelled")
	}
	if keep := find(actions, reconcile.KindKeep, link); keep == nil || keep.Reason != "agent running" {
		t.Errorf("expected an explanatory keep, got %+v", keep)
	}
}

// The same join, from the staffing side: a workspace whose checkout is spelled
// differently must still find its worktree, or a resumable session is started
// cold and the branch column goes blank.
func TestStaffFindsItsWorktreeUnderADifferentSpelling(t *testing.T) {
	real, link := symlinkedPair(t)

	state := reconcile.State{
		Base: "main",
		Worktrees: []reconcile.Worktree{
			{Path: link, Branch: "a", IsLinked: true, WorkspaceID: "w2", HasTranscript: true},
		},
		Workspaces: []reconcile.Workspace{
			{ID: "w2", CheckoutPath: real, IsLinked: true, PaneIDs: []string{"w2:p1"}},
		},
	}

	actions := reconcile.Reconcile(state)
	staff := find(actions, reconcile.KindStaff, real)
	if staff == nil {
		t.Fatal("the agentless workspace should be staffed")
	}
	if !staff.Resume {
		t.Error("a prior transcript exists, so staffing must resume rather than start cold")
	}
	if staff.Branch != "a" {
		t.Errorf("branch = %q, want the joined worktree's branch %q", staff.Branch, "a")
	}
}

// The pair that widens pruning past a merged PR. Neither half is sufficient and
// the two tests below pin that; this one pins that together they suffice.
func TestPrunesWhenMergedAndUpstreamDeleted(t *testing.T) {
	w := linked("/repo/wt/a", "a")
	w.MergedIntoBase, w.UpstreamGone, w.PR = true, true, reconcile.PRNone
	state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{w}}

	action := find(reconcile.Reconcile(state), reconcile.KindPrune, "/repo/wt/a")
	if action == nil {
		t.Fatalf("a deleted upstream resolves the merged-looking ambiguity, got %+v", reconcile.Reconcile(state))
	}
	if !strings.Contains(action.Reason, "upstream") {
		t.Errorf("the reason must name the signal that authorised this, got %q", action.Reason)
	}
}

// A deleted remote branch is equally what abandoning work looks like. Without
// base holding the commits, acting on it would delete work that exists nowhere
// else — the exact failure this project refuses to risk.
func TestNeverPrunesOnAGoneUpstreamAlone(t *testing.T) {
	w := linked("/repo/wt/a", "a")
	w.UpstreamGone, w.OwnCommits, w.MergedIntoBase, w.PR = true, 3, false, reconcile.PRNone
	state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{w}}

	actions := reconcile.Reconcile(state)
	if find(actions, reconcile.KindPrune, "/repo/wt/a") != nil {
		t.Fatal("a deleted upstream alone is abandonment as often as completion")
	}

	keep := find(actions, reconcile.KindKeep, "/repo/wt/a")
	if keep == nil {
		t.Fatal("expected a keep")
	}
	if !strings.Contains(keep.Reason, "upstream") {
		t.Errorf("the reason must say the upstream was deleted so a human can act, got %q", keep.Reason)
	}
}

// Pull request state stays authoritative wherever it exists. A gone upstream
// must not talk the reconciler past an OPEN pull request.
func TestGoneUpstreamNeverOverridesPRState(t *testing.T) {
	for _, pr := range []reconcile.PRState{reconcile.PROpen, reconcile.PRClosed} {
		t.Run(string(pr), func(t *testing.T) {
			w := linked("/repo/wt/a", "a")
			w.MergedIntoBase, w.UpstreamGone, w.PR = true, true, pr
			state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{w}}

			if find(reconcile.Reconcile(state), reconcile.KindPrune, "/repo/wt/a") != nil {
				t.Fatalf("PR %s must outrank the upstream signal", pr)
			}
		})
	}
}

// A workspace can outlive its checkout: someone removes the directory, or a
// `git worktree remove` runs while herdr goes on holding the workspace open.
//
// Before this, nothing visited that state. adopt and prune both walk
// state.Worktrees, and staff walks workspaces but only ever reads through to a
// worktree — so the workspace was in no pass's iteration and `prune` reported
// nothing, which reads as "you are clean" on exactly the state this plugin
// exists to name. It is the mirror of the workspace that vanished underneath
// the collector; this one stayed while its checkout went.
func TestGhostWorkspaceWhoseCheckoutHasVanishedIsReported(t *testing.T) {
	state := reconcile.State{
		Root:      "/repo",
		Base:      "main",
		Worktrees: []reconcile.Worktree{{Path: "/repo/wt/live", Branch: "live", IsLinked: true, WorkspaceID: "w2"}},
		Workspaces: []reconcile.Workspace{
			{ID: "w2", CheckoutPath: "/repo/wt/live", IsLinked: true, PaneIDs: []string{"w2:p1"}},
			{ID: "w35", CheckoutPath: "/repo/wt/gone", IsLinked: true, PaneIDs: []string{"w35:p1"}},
		},
		AgentPanes: map[string]reconcile.AgentState{"w2:p1": reconcile.AgentWorking},
	}

	actions := reconcile.Reconcile(state)

	ghost := find(actions, reconcile.KindGhost, "/repo/wt/gone")
	if ghost == nil {
		t.Fatal("a workspace whose checkout is gone should be reported")
	}
	if ghost.WorkspaceID != "w35" {
		t.Errorf("the report must name the workspace to close, got %q", ghost.WorkspaceID)
	}
	if ghost.Reason == "" {
		t.Error("a ghost must say what it is; the reason is the whole point of the verdict")
	}
	if find(actions, reconcile.KindGhost, "/repo/wt/live") != nil {
		t.Error("a workspace whose checkout is right there is not a ghost")
	}
}

// Diagnosis only. There is no checkout left to test for uncommitted work, so
// the strongest guard this plugin has is not unsatisfied but unavailable — and
// a ghost can still host a live agent whose conversation closing the workspace
// would destroy. A removal, if one is ever added, wants its own command.
func TestGhostIsNeverPruned(t *testing.T) {
	state := reconcile.State{
		Root: "/repo",
		Base: "main",
		Workspaces: []reconcile.Workspace{
			{ID: "w35", CheckoutPath: "/repo/wt/gone", IsLinked: true, PaneIDs: []string{"w35:p1"}},
		},
		AgentPanes:    map[string]reconcile.AgentState{},
		ReleaseAgents: true,
	}

	for _, a := range reconcile.Reconcile(state) {
		if a.Kind == reconcile.KindPrune {
			t.Errorf("a ghost must never be planned for removal, got %+v", a)
		}
	}
}

// The join that missed did not merely hide the workspace: staff read through it
// to a zero Worktree, so an empty-paned ghost was planned an agent on an empty
// branch in a directory that is not there.
func TestGhostIsNeverStaffed(t *testing.T) {
	state := reconcile.State{
		Root: "/repo",
		Base: "main",
		Workspaces: []reconcile.Workspace{
			{ID: "w35", CheckoutPath: "/repo/wt/gone", IsLinked: true, PaneIDs: []string{"w35:p1"}},
		},
		AgentPanes: map[string]reconcile.AgentState{},
	}

	if a := find(reconcile.Reconcile(state), reconcile.KindStaff, "/repo/wt/gone"); a != nil {
		t.Errorf("there is no directory to start an agent in, got %+v", a)
	}
}

// finishedGhost is the factory's ordinary finish: the worker reported done
// with its PR, the PR merged, the checkout was deleted, and the agent sits idle
// in a workspace herdr still holds open.
func finishedGhost() reconcile.State {
	return reconcile.State{
		Root: "/repo",
		Base: "main",
		Workspaces: []reconcile.Workspace{
			{ID: "w35", CheckoutPath: "/repo/wt/gone", IsLinked: true, PaneIDs: []string{"w35:p1", "w35:p2"}},
		},
		AgentPanes:    map[string]reconcile.AgentState{"w35:p1": reconcile.AgentIdle},
		ReleaseAgents: true,
		PaneReports: map[string]reconcile.PaneReport{
			"w35:p1": {Found: true, Status: "done", PR: 240, PRState: reconcile.PRMerged, PRHead: "gone"},
			// A shell split beside the worker, with nothing to say.
			"w35:p2": {},
		},
	}
}

func TestGhostWhoseWorkerIsDoneAndWhosePRMergedIsClosed(t *testing.T) {
	actions := reconcile.Reconcile(finishedGhost())

	closing := find(actions, reconcile.KindCloseGhost, "/repo/wt/gone")
	if closing == nil {
		t.Fatalf("a finished ghost should be planned for closing, got %+v", actions)
	}
	if closing.WorkspaceID != "w35" || closing.PaneID != "w35:p1" || closing.PR != 240 {
		t.Errorf("the close must name the workspace, the worker's pane and its PR, got %+v", closing)
	}
	if !strings.Contains(closing.Reason, "#240") {
		t.Errorf("the reason should name the merged PR, got %q", closing.Reason)
	}
	if !closing.ReleasesAgents {
		t.Error("closing a workspace with an agent in it ends that agent, and the action must say so")
	}
	if find(actions, reconcile.KindGhost, "/repo/wt/gone") != nil {
		t.Error("a ghost planned for closing must not also be reported as one left for a person")
	}
}

// Every condition that stops the close leaves a named ghost, as before.
func TestGhostStaysAGhostUnlessEveryConditionHolds(t *testing.T) {
	cases := map[string]func(*reconcile.State){
		"agent working": func(s *reconcile.State) {
			s.AgentPanes["w35:p1"] = reconcile.AgentWorking
		},
		"agent blocked on a person": func(s *reconcile.State) {
			s.AgentPanes["w35:p1"] = reconcile.AgentBlocked
		},
		"a working agent in another pane": func(s *reconcile.State) {
			s.AgentPanes["w35:p2"] = reconcile.AgentWorking
		},
		"no report": func(s *reconcile.State) {
			s.PaneReports = map[string]reconcile.PaneReport{"w35:p1": {}, "w35:p2": {}}
		},
		"panes never read": func(s *reconcile.State) {
			s.PaneReports = nil
		},
		"one pane never read": func(s *reconcile.State) {
			delete(s.PaneReports, "w35:p2")
		},
		"finished agent without --release-agents": func(s *reconcile.State) {
			s.ReleaseAgents = false
		},
		"a second agent in another pane": func(s *reconcile.State) {
			s.AgentPanes["w35:p2"] = reconcile.AgentIdle
		},
		"a second worker reported done too": func(s *reconcile.State) {
			s.PaneReports["w35:p2"] = s.PaneReports["w35:p1"]
		},
		"PR from another branch": func(s *reconcile.State) {
			s.PaneReports["w35:p1"] = reconcile.PaneReport{Found: true, Status: "done", PR: 240, PRState: reconcile.PRMerged, PRHead: "something-else"}
		},
		"gh named no head branch": func(s *reconcile.State) {
			s.PaneReports["w35:p1"] = reconcile.PaneReport{Found: true, Status: "done", PR: 240, PRState: reconcile.PRMerged}
		},
		"last report planned": func(s *reconcile.State) {
			s.PaneReports["w35:p1"] = reconcile.PaneReport{Found: true, Status: "planned", PR: 240, PRState: reconcile.PRMerged}
		},
		"last report blocked": func(s *reconcile.State) {
			s.PaneReports["w35:p1"] = reconcile.PaneReport{Found: true, Status: "blocked", PR: 240, PRState: reconcile.PRMerged}
		},
		"done without a PR": func(s *reconcile.State) {
			s.PaneReports["w35:p1"] = reconcile.PaneReport{Found: true, Status: "done"}
		},
		"PR still open": func(s *reconcile.State) {
			s.PaneReports["w35:p1"] = reconcile.PaneReport{Found: true, Status: "done", PR: 240, PRState: reconcile.PROpen}
		},
		"PR closed unmerged": func(s *reconcile.State) {
			s.PaneReports["w35:p1"] = reconcile.PaneReport{Found: true, Status: "done", PR: 240, PRState: reconcile.PRClosed}
		},
		"gh could not answer": func(s *reconcile.State) {
			s.PaneReports["w35:p1"] = reconcile.PaneReport{Found: true, Status: "done", PR: 240}
		},
		"another pane unreadable": func(s *reconcile.State) {
			s.PaneReports["w35:p2"] = reconcile.PaneReport{Unreadable: true}
		},
		"another pane reports unfinished work": func(s *reconcile.State) {
			s.PaneReports["w35:p2"] = reconcile.PaneReport{Found: true, Status: "planned"}
		},
	}
	for name, spoil := range cases {
		t.Run(name, func(t *testing.T) {
			state := finishedGhost()
			spoil(&state)
			actions := reconcile.Reconcile(state)

			if a := find(actions, reconcile.KindCloseGhost, "/repo/wt/gone"); a != nil {
				t.Errorf("the close must not be planned, got %+v", a)
			}
			if find(actions, reconcile.KindGhost, "/repo/wt/gone") == nil {
				t.Error("it should still be named as a ghost")
			}
		})
	}
}

// With no agent left in it there is no conversation to end, so the close does
// not need --release-agents.
func TestFinishedGhostWithNoAgentClosesWithoutReleaseAgents(t *testing.T) {
	state := finishedGhost()
	state.AgentPanes = map[string]reconcile.AgentState{}
	state.ReleaseAgents = false

	closing := find(reconcile.Reconcile(state), reconcile.KindCloseGhost, "/repo/wt/gone")
	if closing == nil {
		t.Fatal("a finished ghost with no agent in it should be planned for closing")
	}
	if closing.ReleasesAgents {
		t.Error("there is no agent for this close to release")
	}
}

// Held for want of the flag, the ghost says which flag.
func TestFinishedGhostHeldForReleaseAgentsSaysSo(t *testing.T) {
	state := finishedGhost()
	state.ReleaseAgents = false

	ghost := find(reconcile.Reconcile(state), reconcile.KindGhost, "/repo/wt/gone")
	if ghost == nil {
		t.Fatal("it should be named as a ghost")
	}
	if !strings.Contains(ghost.Reason, "--release-agents") {
		t.Errorf("the reason should name the flag that closes it, got %q", ghost.Reason)
	}
}
