package gitx_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrtest"
)

func TestCommitTimeReadsTheBranchTipAndNothingElse(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	repo.Git("branch", "feature")
	at, ok := gitx.CommitTime(repo.Root, "feature")
	if !ok || time.Since(at) > time.Minute {
		t.Errorf("CommitTime = %v, %v; want the commit just made", at, ok)
	}
	if _, ok := gitx.CommitTime(repo.Root, "gone"); ok {
		t.Error("a branch that does not exist has a commit time")
	}
}

func TestRepoRootFromLinkedWorktree(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("feature", "feature")

	got, err := gitx.RepoRoot(checkout)
	if err != nil {
		t.Fatal(err)
	}
	// git reports resolved paths, and so must we: repo.Root is handed out
	// with its symlinks intact.
	if got != repo.RealRoot {
		t.Errorf("RepoRoot(%s) = %s, want %s", checkout, got, repo.RealRoot)
	}
}

func TestRepoRootOutsideGitFails(t *testing.T) {
	dir := t.TempDir()
	if _, err := gitx.RepoRoot(filepath.Clean(dir)); err == nil {
		t.Skip("temp dir is inside a git repository on this machine")
	}
}

// Without an origin there is nothing to ask, and main is the only sane guess.
func TestBaseRefFallsBackToMain(t *testing.T) {
	repo := herdrtest.NewRepo(t)

	if got := gitx.BaseRef(repo.Root); got != "main" {
		t.Errorf("BaseRef = %q, want main", got)
	}
}

// The base ref is develop on some repositories. It must be read, never assumed.
func TestBaseRefFollowsOriginHead(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	repo.SetOriginHead("develop")

	if got := gitx.BaseRef(repo.Root); got != "origin/develop" {
		t.Errorf("BaseRef = %q, want origin/develop", got)
	}
}

// A ref is not a fixed point and a commit is. Commit is what makes a fork point
// recordable, so it must resolve every spelling of one and answer nothing at all
// for a ref that names none — never a plausible-looking substitute.
func TestCommitResolvesRefsAndOnlyRefs(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	repo.SetOriginHead("main")
	head := repo.Git("rev-parse", "HEAD")

	for _, ref := range []string{"HEAD", "main", "origin/main", head, head[:7]} {
		if got := gitx.Commit(repo.Root, ref); got != head {
			t.Errorf("Commit(%q) = %q, want %q", ref, got, head)
		}
	}
	for _, ref := range []string{"no/such/ref", "", "  "} {
		if got := gitx.Commit(repo.Root, ref); got != "" {
			t.Errorf("Commit(%q) = %q, want nothing", ref, got)
		}
	}
}

// An annotated tag is an object of its own, and the commit it points at is what
// a rebase needs. `^{commit}` is doing that work and dropping it would print an
// object `git rebase --onto` cannot use.
func TestCommitPeelsAnAnnotatedTag(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	repo.Git("tag", "-a", "v1", "-m", "release")

	if got, want := gitx.Commit(repo.Root, "v1"), repo.Git("rev-parse", "HEAD"); got != want {
		t.Errorf("Commit(v1) = %q, want the commit it points at (%q)", got, want)
	}
}

func TestIsDirty(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("feature", "feature")

	if gitx.IsDirty(checkout) {
		t.Error("a fresh worktree should be clean")
	}

	// An untracked file is uncommitted work too: it exists nowhere else.
	herdrtest.WriteFile(t, filepath.Join(checkout, "scratch.txt"), "wip\n")
	if !gitx.IsDirty(checkout) {
		t.Error("untracked file should count as dirty")
	}
}

func TestOwnCommitsCountsOnlyNewWork(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("feature", "feature")

	if got := gitx.OwnCommits(checkout, "main"); got != 0 {
		t.Errorf("a branch with no commits should have 0 own commits, got %d", got)
	}

	repo.CommitIn(checkout, "a.txt", "one")
	repo.CommitIn(checkout, "b.txt", "two")
	if got := gitx.OwnCommits(checkout, "main"); got != 2 {
		t.Errorf("OwnCommits = %d, want 2", got)
	}
}

// Guard c's reason for existing, against real git: a worktree created seconds
// ago is an ancestor of base, and must NOT be reported as merged.
func TestIsMergedIntoRejectsUnstartedBranch(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	repo.AddWorktree("fresh", "fresh")

	if gitx.IsMergedInto(repo.Root, "fresh", "main") {
		t.Error("a branch with no commits of its own must not count as merged")
	}
}

// The other half of the same distinction: a branch that really was merged into
// base has to be detected, even though it also has zero commits base lacks.
func TestIsMergedIntoDetectsMergedBranch(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("done", "done")
	repo.CommitIn(checkout, "feature.txt", "shipped")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")

	if !gitx.IsMergedInto(repo.Root, "done", "main") {
		t.Error("a branch merged into base must count as merged")
	}
	// And the naive commit count cannot tell the two apart, which is the
	// whole reason IsMergedInto looks at the first-parent trunk.
	if got := gitx.OwnCommits(checkout, "main"); got != 0 {
		t.Errorf("after a merge the branch has 0 commits base lacks, got %d", got)
	}
}

func TestIsMergedIntoRejectsUnmergedBranch(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("wip", "wip")
	repo.CommitIn(checkout, "wip.txt", "in progress")

	if gitx.IsMergedInto(repo.Root, "wip", "main") {
		t.Error("an unmerged branch must not count as merged")
	}
}

// A fast-forward merge moves base's pointer to the branch tip, so the tip
// becomes a first-parent trunk commit — exactly what a branch that never
// committed also looks like. This test pins that indistinguishability: both
// states produce the same two answers, so no caller may claim to tell them
// apart.
func TestFastForwardMergeIsIndistinguishableFromUnstarted(t *testing.T) {
	repo := herdrtest.NewRepo(t)

	merged := repo.AddWorktree("ff", "ff")
	repo.CommitIn(merged, "ff.txt", "work")
	repo.Git("merge", "--ff-only", "ff")

	unstarted := repo.AddWorktree("fresh", "fresh")

	for _, tc := range []struct {
		name     string
		branch   string
		checkout string
	}{
		{"fast-forward merged", "ff", merged},
		{"never started", "fresh", unstarted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if gitx.IsMergedInto(repo.Root, tc.branch, "main") {
				t.Error("a tip on base's trunk cannot be reported as merged")
			}
			if got := gitx.OwnCommits(tc.checkout, "main"); got != 0 {
				t.Errorf("OwnCommits = %d, want 0", got)
			}
		})
	}
}

// A branch forked off already-merged work inherits a tip that sits off base's
// trunk, so the topological test calls it merged even though the branch has
// done nothing. Same commit, two branches: graph shape cannot separate them.
func TestBranchForkedOffMergedWorkLooksMerged(t *testing.T) {
	repo := herdrtest.NewRepo(t)

	done := repo.AddWorktree("done", "done")
	repo.CommitIn(done, "done.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")

	// Fork a brand-new branch from the merged branch's tip.
	fresh := repo.AddWorktreeFrom("later", "later", "done")

	if !gitx.IsMergedInto(repo.Root, "later", "main") {
		t.Skip("topology no longer reports this as merged; the guard below is moot")
	}
	// The branch has no work of its own, which is what makes acting on the
	// topological verdict alone unsafe.
	if got := gitx.OwnCommits(fresh, "main"); got != 0 {
		t.Errorf("OwnCommits = %d, want 0", got)
	}
}

// A squash merge rewrites the commit, so the branch is not an ancestor of base
// at all. git cannot see it as merged; only the PR state can.
func TestIsMergedIntoMissesSquashMerge(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("squashed", "squashed")
	repo.CommitIn(checkout, "sq.txt", "work")
	repo.Git("merge", "--squash", "squashed")
	repo.Git("commit", "-m", "squashed work")

	if gitx.IsMergedInto(repo.Root, "squashed", "main") {
		t.Error("a squash merge leaves the branch unmerged as far as git is concerned")
	}
}

// IsDirty returns true when git cannot be run at all. The fail-safe direction:
// a checkout that cannot be read must never be reported as clean, because clean
// is what authorises removing it.
func TestIsDirtyTreatsAnUnreadableCheckoutAsDirty(t *testing.T) {
	if !gitx.IsDirty(filepath.Join(t.TempDir(), "does-not-exist")) {
		t.Error("an unreadable checkout must read as dirty; clean is what lets it be deleted")
	}
}

// publish gives a branch the config a `git push -u` would leave behind, plus
// the remote-tracking ref that push would have created.
func publish(t *testing.T, repo *herdrtest.Repo, branch string) {
	t.Helper()
	repo.Git("config", "branch."+branch+".remote", "origin")
	repo.Git("config", "branch."+branch+".merge", "refs/heads/"+branch)
	repo.Git("update-ref", "refs/remotes/origin/"+branch, branch)
}

// A branch that was never pushed has nothing to have been deleted. This is the
// distinction the whole signal rests on: never-published must not read as
// published-then-deleted, or every local scratch branch looks finished.
func TestUpstreamGoneIgnoresANeverPublishedBranch(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	repo.Git("branch", "local-only")

	if gitx.UpstreamGone(repo.Root, "local-only") {
		t.Error("a branch with no upstream has had nothing deleted")
	}
}

func TestUpstreamGoneIsFalseWhileTheRemoteBranchExists(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	repo.Git("branch", "published")
	publish(t, repo, "published")

	if gitx.UpstreamGone(repo.Root, "published") {
		t.Error("the remote-tracking ref is still there; nothing is gone")
	}
}

func TestUpstreamGoneDetectsADeletedRemoteBranch(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	repo.Git("branch", "landed")
	publish(t, repo, "landed")
	// What `git fetch --prune` does once the remote branch is deleted.
	repo.Git("update-ref", "-d", "refs/remotes/origin/landed")

	if !gitx.UpstreamGone(repo.Root, "landed") {
		t.Error("a configured upstream whose tracking ref has gone is the signal")
	}
}

// An empty branch is a detached HEAD, not a branch anyone published.
func TestUpstreamGoneIgnoresAnEmptyBranch(t *testing.T) {
	repo := herdrtest.NewRepo(t)

	if gitx.UpstreamGone(repo.Root, "") {
		t.Error("no branch name means no upstream to be gone")
	}
}

// The enumeration herdr normally answers, asked of git instead. Every prune
// guard is already git or gh; this is the one fact that was only ever herdr's,
// and without it nothing else could run with herdr absent.
func TestWorktreesEnumeratesFromGit(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	linked := repo.AddWorktree("wip", "wip")

	list, err := gitx.Worktrees(repo.Root)
	if err != nil {
		t.Fatalf("Worktrees: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want the main checkout and one linked, got %d: %+v", len(list), list)
	}

	main := list[0]
	if main.IsLinked {
		t.Error("porcelain lists the main checkout first; it is not linked")
	}
	if gitx.Resolve(main.Path) != gitx.Resolve(repo.Root) {
		t.Errorf("main path = %q, want %q", main.Path, repo.Root)
	}

	wt := list[1]
	if !wt.IsLinked {
		t.Error("the second checkout is a linked worktree")
	}
	if gitx.Resolve(wt.Path) != gitx.Resolve(linked) {
		t.Errorf("linked path = %q, want %q", wt.Path, linked)
	}
	if wt.Branch != "wip" {
		t.Errorf("branch = %q, want wip (refs/heads/ stripped)", wt.Branch)
	}
}

// A detached checkout has no `branch` stanza line at all. Empty is the same
// absence herdr reports as a null branch, so the reconciler above sees one
// shape whichever source filled it.
func TestWorktreesLeavesADetachedCheckoutWithNoBranch(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("detached", "detached")
	repo.GitIn(checkout, "checkout", "--detach")

	list, err := gitx.Worktrees(repo.Root)
	if err != nil {
		t.Fatalf("Worktrees: %v", err)
	}
	for _, w := range list {
		if gitx.Resolve(w.Path) != gitx.Resolve(checkout) {
			continue
		}
		if w.Branch != "" {
			t.Errorf("a detached checkout has no branch, got %q", w.Branch)
		}
		return
	}
	t.Fatalf("the detached checkout is missing from the listing: %+v", list)
}
