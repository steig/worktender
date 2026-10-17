package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/jsonout"
)

// verify asks whether an issue's approved checks prove anything about a change.
//
// A check that already passes before the change proves nothing about it: `go
// test -run TestX` exits 0 with "no tests to run" until the worker writes
// TestX, and then it passes because the worker wrote it. So each check runs
// twice, on the merge-base and on the head, and is classified by the pair. A
// change is verified when nothing fails on head and at least one check fails
// on base and passes on head — the evidence that the work, and not the
// repository as it already was, is what the checks are measuring.
//
// The checks are the ones factoryApproval returns, the same parse and the same
// approval start insists on, because they are run as shell commands: what runs
// is what a person approved byte for byte. Each runs in an export of the
// commit's tracked files, never in a checkout, for the reason draft's drafter
// does (exportTracked), and under a timeout. Neither is a sandbox: the
// approved command is trusted, but the head it runs, a test file most of all,
// is the change under verification, and it runs as the user.

const verifyUsage = "usage: worktender verify <issue> [--pr <n>] [--repo <path>] [--timeout 10m] [--json]"

// The classes a check falls into, by how it did on base and on head.
const (
	verifyDiscriminating = "discriminating"
	verifyGuard          = "guard"
	verifyFailing        = "failing"
	// verifyUnjudged is a check whose outcome verify cannot read, or one that
	// timed out or was killed on either side. A timeout on base is not a
	// failure that the change fixed, so it cannot count as discriminating.
	verifyUnjudged = "unjudged"
)

// verifyRun is one check run on one commit.
type verifyRun struct {
	Commit string `json:"commit"`
	// ExitCode is null when the check timed out or was killed by a signal.
	ExitCode *int `json:"exit_code"`
	TimedOut bool `json:"timed_out"`
	Passed   bool `json:"passed"`
}

type verifyCheck struct {
	Command string    `json:"command"`
	Expect  string    `json:"expect"`
	Class   string    `json:"class"`
	Base    verifyRun `json:"base"`
	Head    verifyRun `json:"head"`
}

type verifyJSON struct {
	Repository string        `json:"repository"`
	Issue      int           `json:"issue"`
	PR         *int          `json:"pr"`
	Base       *string       `json:"base"`
	Head       *string       `json:"head"`
	Checks     []verifyCheck `json:"checks"`
	Error      *string       `json:"error"`
	ExitCode   int           `json:"exit_code"`
}

func verifyCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", ".", "checkout whose HEAD is verified, without --pr")
	prFlag := fs.Int("pr", 0, "verify this pull request's head instead of the checkout's HEAD")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long one check may run on one commit")
	asJSON := jsonFlag(fs)

	positional, err := parseAround(fs, args)
	if err != nil {
		return usagef("%v; %s", err, verifyUsage)
	}
	if len(positional) != 1 {
		return usagef("want exactly one issue number; %s", verifyUsage)
	}
	number, err := strconv.Atoi(strings.TrimPrefix(positional[0], "#"))
	if err != nil || number <= 0 {
		return usagef("%q is not an issue number; want a positive integer like 42", positional[0])
	}
	if *prFlag < 0 {
		return usagef("--pr %d is not a pull request number", *prFlag)
	}
	if *timeout <= 0 {
		return usagef("--timeout must be positive")
	}

	root, err := gitx.RepoRoot(*repo)
	if err != nil {
		return usagef("--repo: %v", err)
	}

	notes := out
	doc := &verifyJSON{Repository: root, Issue: number, Checks: []verifyCheck{}}
	finish := func(err error) error {
		if *asJSON {
			doc.ExitCode = exitCode(err)
			if err != nil {
				msg := err.Error()
				doc.Error = &msg
			}
			if wErr := jsonout.Write(out, doc); wErr != nil {
				return wErr
			}
		}
		return err
	}
	if *asJSON {
		notes = os.Stderr
	}

	approved, err := factoryApproval(root, number)
	if err != nil {
		return finish(err)
	}

	head, baseTip := gitx.Commit(*repo, "HEAD"), gitx.BaseRef(root)
	if *prFlag > 0 {
		doc.PR = prFlag
		pr, err := reviewPRFor(root, strconv.Itoa(*prFlag))
		if err != nil {
			return finish(err)
		}
		if !isCommitOid(pr.HeadRefOid) || !isCommitOid(pr.BaseRefOid) {
			return finish(fmt.Errorf("gh gave PR #%d no head or base commit", pr.Number))
		}
		// Any PR's head can pass some issue's checks, so the PR has to be
		// this issue's work: on the branch start named for it, or closing it.
		if issueFromBranch(pr.HeadRefName) != number && !slices.ContainsFunc(pr.ClosingIssuesReferences,
			func(i struct{ Number int }) bool { return i.Number == number }) {
			return finish(codef(exitNeedsHuman, "PR #%d is not #%d's: its branch %s is not named for it and it does not close it",
				pr.Number, number, pr.HeadRefName))
		}
		head, baseTip = gitx.Commit(root, pr.HeadRefOid), pr.BaseRefOid
		if head == "" {
			return finish(fmt.Errorf("PR #%d's head %s is not in this repository; fetch it with `git fetch origin pull/%d/head`",
				pr.Number, shortOid(pr.HeadRefOid), pr.Number))
		}
	}
	if head == "" {
		return finish(fmt.Errorf("%s has no HEAD commit to verify", *repo))
	}
	mergeBase, err := exec.Command("git", "-C", root, "merge-base", baseTip, head).Output()
	if err != nil {
		return finish(fmt.Errorf("find the merge-base of %s and %s: %w", baseTip, shortOid(head), err))
	}
	base := strings.TrimSpace(string(mergeBase))
	doc.Base, doc.Head = jsonout.String(base), jsonout.String(head)
	fmt.Fprintf(notes, "base: %s (merge-base with %s)\nhead: %s\n", base, baseTip, head)

	baseRuns, err := runChecks(root, base, approved.Checks, *timeout)
	if err != nil {
		return finish(err)
	}
	headRuns, err := runChecks(root, head, approved.Checks, *timeout)
	if err != nil {
		return finish(err)
	}

	counts := map[string]int{}
	for i, c := range approved.Checks {
		v := verifyCheck{Command: c.Command, Expect: c.Expect, Base: baseRuns[i], Head: headRuns[i]}
		v.Class = classifyCheck(c.Expect, v.Base, v.Head)
		counts[v.Class]++
		doc.Checks = append(doc.Checks, v)
		var said [2]string
		for j, r := range []verifyRun{v.Base, v.Head} {
			switch {
			case r.TimedOut:
				said[j] = "timed out"
			case r.ExitCode == nil:
				said[j] = "killed by a signal"
			case r.Passed:
				said[j] = fmt.Sprintf("passed (exit %d)", *r.ExitCode)
			default:
				said[j] = fmt.Sprintf("failed (exit %d)", *r.ExitCode)
			}
		}
		fmt.Fprintf(notes, "%d. %-14s base %s %s, head %s %s: `%s` %s\n", i+1, v.Class,
			shortOid(base), said[0], shortOid(head), said[1], c.Command, c.Expect)
	}

	switch {
	case counts[verifyFailing]+counts[verifyUnjudged] > 0:
		return finish(codef(exitNeedsHuman, "#%d is not verified: %d check(s) failing on head, %d unjudged",
			number, counts[verifyFailing], counts[verifyUnjudged]))
	case counts[verifyDiscriminating] == 0:
		return finish(codef(exitNeedsHuman, "#%d is not verified: every check already passes on base %s, so none shows the change did anything",
			number, shortOid(base)))
	}
	if err := verifyTestsNeedTheChange(root, base, head, *timeout, notes); err != nil {
		return finish(err)
	}
	fmt.Fprintf(notes, "verified: %d discriminating, %d guard\n", counts[verifyDiscriminating], counts[verifyGuard])
	return finish(nil)
}

// runChecks runs every check on commit, each in a fresh export so that what
// one check writes is not there for the next, and judges each against its
// expected outcome.
func runChecks(root, commit string, checks []acceptanceCheck, timeout time.Duration) ([]verifyRun, error) {
	runs := make([]verifyRun, len(checks))
	for i, c := range checks {
		run, err := runOneCheck(root, commit, c, timeout)
		if err != nil {
			return nil, fmt.Errorf("run check %d on %s: %w", i+1, shortOid(commit), err)
		}
		runs[i] = run
	}
	return runs, nil
}

// runOneCheck is one check in its own export, which is gone when it returns.
func runOneCheck(root, commit string, c acceptanceCheck, timeout time.Duration) (verifyRun, error) {
	export, cleanup, err := exportTracked(root, commit)
	if err != nil {
		return verifyRun{}, err
	}
	defer cleanup()

	run, output, err := runIn(export, timeout, "sh", "-c", c.Command)
	if err != nil {
		return verifyRun{}, err
	}
	run.Commit = commit
	if run.ExitCode != nil {
		run.Passed, _ = outcomeMet(c.Expect, *run.ExitCode, output)
	}
	return run, nil
}

// runIn runs argv in dir under timeout, with its exit code, or none when it
// timed out or was killed, and everything it printed.
func runIn(dir string, timeout time.Duration, argv ...string) (verifyRun, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	// The timeout kills what the check started, not only sh (killTree), and
	// stops waiting on a child that escaped it and holds the output open.
	killTree(cmd)
	cmd.WaitDelay = 5 * time.Second
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()

	var run verifyRun
	var exit *exec.ExitError
	switch {
	// ctx's error, not Run's: a check killed by the cancel exits by signal,
	// and Run reports that exit rather than the deadline behind it.
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		run.TimedOut = true
	case err == nil:
		run.ExitCode = new(int)
	case errors.As(err, &exit):
		// -1 is a death by signal, which is no exit code the outcome speaks of.
		if code := exit.ExitCode(); code >= 0 {
			run.ExitCode = &code
		}
	default:
		return verifyRun{}, "", withCode(exitEnvironment, err)
	}
	return run, output.String(), nil
}

// fixedOutcomes are the outcomes verify can judge, and a drafted check may
// expect, besides "prints <token>": each with whether a run met it.
var fixedOutcomes = map[string]func(code int, output string) bool{
	"exits 0":        func(code int, _ string) bool { return code == 0 },
	"exits 1":        func(code int, _ string) bool { return code == 1 },
	"exits non-zero": func(code int, _ string) bool { return code != 0 },
	"prints nothing": func(_ int, output string) bool { return strings.TrimSpace(output) == "" },
}

// outcomeMet says whether a run met expect. known is false for an outcome
// verify cannot read, which no run meets. "prints <token>" is read only for
// the tokens draft would propose, so that "prints nothing and exits 0" is
// not taken for a search for "nothing and exits 0".
func outcomeMet(expect string, code int, output string) (met, known bool) {
	if judge, ok := fixedOutcomes[expect]; ok {
		return judge(code, output), true
	}
	if token, ok := strings.CutPrefix(expect, "prints "); ok && draftPrintsToken.MatchString(token) {
		return strings.Contains(output, token), true
	}
	return false, false
}

func classifyCheck(expect string, base, head verifyRun) string {
	if _, known := outcomeMet(expect, 0, ""); !known || base.ExitCode == nil || head.ExitCode == nil {
		return verifyUnjudged
	}
	switch {
	case !head.Passed:
		return verifyFailing
	case base.Passed:
		return verifyGuard
	}
	return verifyDiscriminating
}

// goTestFunc is a test go test runs: TestMain takes a *testing.M and is none.
var goTestFunc = regexp.MustCompile(`(?m)^func (Test\w*)\(\w+ \*testing\.T\)`)

// verifyTestsNeedTheChange refuses tests that pass without the change they
// came with. Approved checks are graded on base and head, but the worker's
// own tests are graded only by passing, and a test that passes with the code
// change reverted tests nothing. So head's export has every changed Go
// source file, go.mod and go.sum put back as they were on base, the changed
// test files kept, and the changed test files' tests run there: at least one
// has to fail. A README edit beside a test-only change is not code the tests
// could need, so it is left alone.
//
// Only a test that ran and failed counts. A build failure on the revert says
// the test calls something new, not that it checks what the new thing does,
// and a run where every test skipped or none matched shows nothing either
// way: both are unjudged.
func verifyTestsNeedTheChange(root, base, head string, timeout time.Duration, notes io.Writer) error {
	diff, err := exec.Command("git", "-C", root, "diff", "--name-status", "--no-renames", "-z", base, head).Output()
	if err != nil {
		return withCode(exitEnvironment, fmt.Errorf("list what %s changed: %w", shortOid(head), err))
	}
	var restore, remove, tests, testFiles []string
	packages := map[string]bool{}
	fields := strings.Split(strings.TrimSuffix(string(diff), "\x00"), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		status, file := fields[i], fields[i+1]
		switch {
		case strings.HasSuffix(file, "_test.go"):
			if status == "D" {
				continue
			}
			testFiles = append(testFiles, file)
			src, err := exec.Command("git", "-C", root, "show", head+":"+file).Output()
			if err != nil {
				return withCode(exitEnvironment, fmt.Errorf("read %s on %s: %w", file, shortOid(head), err))
			}
			for _, m := range goTestFunc.FindAllSubmatch(src, -1) {
				tests = append(tests, string(m[1]))
				packages["./"+path.Dir(file)] = true
			}
		case !strings.HasSuffix(file, ".go") && path.Base(file) != "go.mod" && path.Base(file) != "go.sum":
		case status == "A":
			remove = append(remove, file)
		default:
			restore = append(restore, file)
		}
	}
	switch {
	case len(restore)+len(remove) == 0:
		fmt.Fprintf(notes, "revert: skipped, the change touches no Go source, go.mod or go.sum\n")
		return nil
	case len(testFiles) == 0:
		fmt.Fprintf(notes, "revert: skipped, the change adds or changes no Go test\n")
		return nil
	// A test spelled so the pattern misses it, `*tt.T` from an aliased
	// import, must not pass for a change with no tests.
	case len(tests) == 0:
		return codef(exitNeedsHuman, "the changed test files (%s) hold no test verify can find to run with the change reverted",
			strings.Join(testFiles, ", "))
	}

	export, cleanup, err := exportTracked(root, head)
	if err != nil {
		return err
	}
	defer cleanup()
	for _, file := range append(remove, restore...) {
		if err := os.Remove(filepath.Join(export, filepath.FromSlash(file))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return withCode(exitEnvironment, err)
		}
	}
	if len(restore) > 0 {
		archive := exec.Command("git", append([]string{"-C", root, "archive", "--format=tar", base, "--"}, restore...)...)
		tarball, err := archive.Output()
		if err == nil {
			err = extractTar(bytes.NewReader(tarball), export)
		}
		if err != nil {
			return withCode(exitEnvironment, fmt.Errorf("put back what %s changed as it was on %s: %w", shortOid(head), shortOid(base), err))
		}
	}

	slices.Sort(tests)
	tests = slices.Compact(tests)
	argv := []string{"go", "test", "-count=1", "-v", "-run", "^(" + strings.Join(tests, "|") + ")$"}
	argv = append(argv, slices.Sorted(maps.Keys(packages))...)
	run, output, err := runIn(export, timeout, argv...)
	switch {
	case err != nil:
		return err
	case run.ExitCode == nil:
		return codef(exitNeedsHuman, "the changed tests did not finish with the change reverted, so whether they need it is unjudged")
	case strings.Contains(output, "[build failed]") || strings.Contains(output, "[setup failed]"):
		return codef(exitNeedsHuman, "the changed tests do not build with the change reverted, so whether they need it is unjudged")
	case *run.ExitCode == 0 && strings.Contains(output, "--- PASS:"):
		return codef(exitNeedsHuman, "hollow: tests pass with the change reverted (%s)", strings.Join(tests, ", "))
	case *run.ExitCode == 0 || !strings.Contains(output, "--- FAIL:"):
		return codef(exitNeedsHuman, "no changed test ran to a pass or a failure with the change reverted, so whether they need it is unjudged")
	}
	fmt.Fprintf(notes, "revert: the changed tests fail with %d Go file(s) put back as on base\n", len(restore)+len(remove))
	return nil
}
