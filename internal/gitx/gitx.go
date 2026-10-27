// Package gitx runs the git queries the worktree driver depends on.
//
// It is the only place that shells out to git, so the reconciler above it can
// stay a pure function over already-collected facts.
package gitx

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// run executes git in dir and returns trimmed stdout.
func run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s in %s: %w", strings.Join(args, " "), dir, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// RepoRoot returns the absolute path of the MAIN worktree, even when called
// from a linked one. --git-common-dir points at the real .git for every linked
// checkout, so its parent is the main checkout.
//
// This matters because herdr keys worktrees by repository: asking from inside a
// linked worktree must return the same list as asking from the main one.
func RepoRoot(dir string) (string, error) {
	common, err := run(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("not inside a git repository: %s", dir)
	}
	if common == "" {
		return "", fmt.Errorf("git reported no common dir for %s", dir)
	}
	return Resolve(filepath.Dir(common)), nil
}

// Toplevel returns the root of the checkout dir sits in — the linked worktree
// itself when called from one, unlike RepoRoot — resolved for comparison with
// the paths Worktrees reports.
func Toplevel(dir string) (string, error) {
	top, err := run(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("not inside a git checkout: %s", dir)
	}
	return Resolve(top), nil
}

// Resolve normalises a path for comparison: absolute, cleaned, and with every
// symlink expanded.
//
// git reports resolved paths, so an unresolved caller directory compares unequal
// to the same directory as git names it — the normal case on macOS, where TMPDIR
// lives under the /var symlink — which silently disarms the guard refusing to
// delete the directory you are standing in.
//
// Paths that do not exist still resolve: EvalSymlinks fails outright on a
// missing path, and missing paths are routine here, so the longest existing
// ancestor is resolved and the remainder re-appended.
func Resolve(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	path = filepath.Clean(path)

	remainder := ""
	for current := path; ; {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(resolved, remainder)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path // reached the root without resolving anything
		}
		remainder = filepath.Join(filepath.Base(current), remainder)
		current = parent
	}
}

// DefaultBase is used when a repository has no origin to ask.
const DefaultBase = "main"

// BaseRef is the branch new work forks from: whatever origin/HEAD points at,
// which is main on some repositories and develop on others. Never assume.
func BaseRef(root string) string {
	base, err := run(root, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil || base == "" {
		return DefaultBase
	}
	return base
}

// Commit resolves a ref to the full commit it names right now, or "" when there
// is nothing to resolve.
//
// A ref name is not a fixed point, which is why the name alone does not record
// where a worktree was forked from. A base branch that is squash-merged puts one
// new commit on the trunk and none of its own, so the commit a stacked branch
// was forked from afterwards exists nowhere in the trunk's history — and
// replaying that branch needs the commit, not the name.
//
// Empty rather than an error: this annotates work that has already happened, and
// a ref git cannot resolve fails the worktree create with a better message than
// this could produce.
func Commit(root, ref string) string {
	out, err := run(root, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return ""
	}
	return out
}

// Branch is the branch checked out in dir, or empty when HEAD is detached or
// dir is not a checkout at all. Empty rather than an error for the reason Commit
// is: its one caller is annotating something that already happened, and has
// nothing better to do with a failure than record the absence.
func Branch(dir string) string {
	out, err := run(dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return out
}

// CommitTime is the committer date of the commit at the tip of a local branch,
// and false when the branch cannot be read — deleted after its merge, or never
// in this repository. The committer date rather than the author date because a
// rebase or an amend is new work on the branch as far as anyone waiting on it is
// concerned, and only the committer date moves for those.
func CommitTime(root, branch string) (time.Time, bool) {
	out, err := run(root, "log", "-1", "--format=%cI", "refs/heads/"+branch, "--")
	if err != nil || out == "" {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, out)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// IsDirty reports whether a checkout has uncommitted changes, including
// untracked files. Work that exists nowhere else must never be pruned.
func IsDirty(checkout string) bool {
	out, err := run(checkout, "status", "--porcelain")
	if err != nil {
		// An unreadable checkout is treated as dirty: refusing to delete is
		// always the safe direction.
		return true
	}
	return out != ""
}

// OwnCommits counts commits on the checkout's HEAD that base does not have.
//
// A branch with none of its own is trivially an ancestor of base, so callers
// use this to tell "not started" apart from "finished". An unresolvable base
// counts as zero, which keeps the worktree.
func OwnCommits(checkout, base string) int {
	n, _ := CommitsAhead(checkout, base)
	return n
}

// CommitsAhead is OwnCommits for a caller that must not read a failure as zero:
// a scout's check passes on zero.
func CommitsAhead(checkout, base string) (int, error) {
	out, err := run(checkout, "rev-list", "--count", base+"..HEAD")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(out)
}

// UpstreamGone reports that a branch was published and its remote counterpart
// has since been deleted. It is the one fact in this file that is not a graph
// shape: it records that a person deleted a remote branch.
//
// The distinction is never-published versus published-then-deleted. A branch
// with no configured upstream was never pushed; one whose `branch.<name>.merge`
// is still configured but whose remote-tracking ref has gone was pushed and had
// that ref removed. Only the second returns true — `git branch -vv` renders it
// as "[origin/foo: gone]".
//
// It establishes nothing on its own, because a remote branch can be deleted
// without merging, so verdict pairs it with ancestry. It also requires current
// remote-tracking refs: a stale ref reads as still present, which keeps the
// worktree, so being out of date fails safe.
func UpstreamGone(root, branch string) bool {
	if branch == "" {
		return false
	}

	remote, err := run(root, "config", "--get", "branch."+branch+".remote")
	if err != nil || remote == "" {
		// Never published. Nothing was deleted, so nothing is gone.
		return false
	}
	merge, err := run(root, "config", "--get", "branch."+branch+".merge")
	if err != nil || merge == "" {
		return false
	}

	// branch.<name>.merge is a full ref on the remote (refs/heads/foo); the
	// tracking ref is that name under refs/remotes/<remote>/.
	tracking := remote + "/" + strings.TrimPrefix(merge, "refs/heads/")
	if _, err := run(root, "rev-parse", "--verify", "--quiet", "refs/remotes/"+tracking); err != nil {
		return true
	}
	return false
}

// IsMergedInto reports that base absorbed the branch through a merge.
//
// "Ancestor of base" alone is not enough: a branch never committed to is also an
// ancestor, pointing straight at a trunk commit, and both cases have zero
// commits base lacks. What separates them is that a merged branch's tip is the
// second parent of a merge commit, so it sits off base's first-parent trunk.
//
// Squash and rebase rewrite the commits, so the branch is not an ancestor at all
// and PR state is the only signal — which is why callers check the PR first.
func IsMergedInto(root, branch, base string) bool {
	if !isAncestor(root, branch, base) {
		return false
	}
	tip, err := run(root, "rev-parse", branch)
	if err != nil {
		return false
	}

	onTrunk, ok := onFirstParentTrunk(root, base, tip)
	if !ok {
		// The walk failed, so nothing was established. Reporting "merged"
		// here would be a confident answer built on a broken read.
		return false
	}
	return !onTrunk
}

func isAncestor(root, branch, base string) bool {
	cmd := exec.Command("git", "merge-base", "--is-ancestor", branch, base)
	cmd.Dir = root
	return cmd.Run() == nil
}

// onFirstParentTrunk reports whether sha is on base's first-parent history.
// The walk streams and stops at the first match, so a deep history costs no
// more than reaching the commit.
//
// ok is false when the walk could not be completed. A truncated read looks
// exactly like "not found", and callers must not mistake one for the other.
func onFirstParentTrunk(root, base, sha string) (found, ok bool) {
	cmd := exec.Command("git", "rev-list", "--first-parent", base)
	cmd.Dir = root

	out, err := cmd.StdoutPipe()
	if err != nil {
		return false, false
	}
	if err := cmd.Start(); err != nil {
		return false, false
	}
	defer func() {
		_ = out.Close()
		_ = cmd.Wait()
	}()

	scanner := bufio.NewScanner(out)
	for scanner.Scan() {
		if scanner.Text() == sha {
			return true, true
		}
	}
	if err := scanner.Err(); err != nil {
		return false, false
	}
	return false, true
}

// RemoteURL is origin's URL, or empty when there is no origin. `gh` needs it to
// target the right repository from inside a worktree.
func RemoteURL(root string) string {
	out, err := run(root, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return out
}

// Worktree is one checkout as `git worktree list --porcelain` describes it.
//
// It is deliberately the subset herdr's own worktree.list carries, so the two
// sources can fill the same reconcile input rather than one of them being a
// second shape the decision layer has to know about.
type Worktree struct {
	Path   string
	Branch string
	// IsLinked is false for the repository's main checkout, which porcelain
	// always lists first.
	IsLinked bool
	IsBare   bool
}

// Worktrees enumerates the repository's checkouts from git rather than herdr.
//
// herdr's worktree.list is the source whenever herdr is there, because it also
// answers which workspace holds each checkout open — a question git cannot be
// asked. This exists for the case where herdr is not running at all: every
// prune guard is git or gh, so the verdicts do not need herdr, and only the
// enumeration did.
//
// The porcelain format is stanzas separated by blank lines, one `worktree`
// line each, and it is the stable interface — the human-readable form is not.
// A detached checkout has no `branch` line, so its branch stays empty, which is
// the same absence herdr reports as a null.
func Worktrees(root string) ([]Worktree, error) {
	out, err := run(root, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}

	var (
		list    []Worktree
		current *Worktree
	)
	flush := func() {
		if current != nil {
			// Porcelain lists the main checkout first and linked ones after,
			// which is the only signal in the format for which is which.
			current.IsLinked = len(list) > 0
			list = append(list, *current)
			current = nil
		}
	}

	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			current = &Worktree{Path: Resolve(strings.TrimPrefix(line, "worktree "))}
		case current == nil:
			// A stanza key before any `worktree` line is not a shape git
			// produces; ignoring it keeps a future key from being read as a
			// checkout.
		case line == "bare":
			current.IsBare = true
		case strings.HasPrefix(line, "branch "):
			current.Branch = strings.TrimPrefix(
				strings.TrimPrefix(line, "branch "), "refs/heads/")
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read git worktree list in %s: %w", root, err)
	}
	return list, nil
}

// Exclude makes sure entry is a line of the repository's info/exclude, adding
// it if missing. The file lives in the common git dir, so one entry covers the
// main checkout and every linked worktree, and no tracked file changes.
func Exclude(root, entry string) error {
	common, err := run(root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	path := filepath.Join(common, "info", "exclude")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == entry {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		entry = "\n" + entry
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(entry + "\n"); err != nil {
		return err
	}
	return f.Close()
}
