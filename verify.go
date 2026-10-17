package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	// timed out on either side. A timeout on base is not a failure that the
	// change fixed, so it cannot count as discriminating.
	verifyUnjudged = "unjudged"
)

// verifyRun is one check run on one commit.
type verifyRun struct {
	Commit string `json:"commit"`
	// ExitCode is null when the check timed out and was killed.
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

func (d *verifyJSON) finish(err error) *verifyJSON {
	d.ExitCode = exitCode(err)
	if err != nil {
		msg := err.Error()
		d.Error = &msg
	}
	return d
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
			if wErr := jsonout.Write(out, doc.finish(err)); wErr != nil {
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
		fmt.Fprintf(notes, "%d. %-14s base %s %s, head %s %s: `%s` %s\n", i+1, v.Class,
			shortOid(base), v.Base.describe(), shortOid(head), v.Head.describe(), c.Command, c.Expect)
	}

	switch {
	case counts[verifyFailing]+counts[verifyUnjudged] > 0:
		return finish(codef(exitNeedsHuman, "#%d is not verified: %d check(s) failing on head, %d unjudged",
			number, counts[verifyFailing], counts[verifyUnjudged]))
	case counts[verifyDiscriminating] == 0:
		return finish(codef(exitNeedsHuman, "#%d is not verified: every check already passes on base %s, so none shows the change did anything",
			number, shortOid(base)))
	}
	fmt.Fprintf(notes, "verified: %d discriminating, %d guard\n", counts[verifyDiscriminating], counts[verifyGuard])
	return finish(nil)
}

// runChecks runs every check in one export of commit, in order, and judges
// each against its expected outcome.
func runChecks(root, commit string, checks []acceptanceCheck, timeout time.Duration) ([]verifyRun, error) {
	export, cleanup, err := exportTracked(root, commit)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	runs := make([]verifyRun, len(checks))
	for i, c := range checks {
		run := verifyRun{Commit: commit}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		cmd := exec.CommandContext(ctx, "sh", "-c", c.Command)
		cmd.Dir = export
		// A child the check started can hold its output open after sh is
		// killed; stop waiting on that rather than on the child.
		cmd.WaitDelay = 5 * time.Second
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		err := cmd.Run()
		timedOut := ctx.Err() != nil
		cancel()

		var exit *exec.ExitError
		switch {
		case timedOut:
			run.TimedOut = true
		case err == nil:
			run.ExitCode = new(int)
		case errors.As(err, &exit):
			code := exit.ExitCode()
			run.ExitCode = &code
		default:
			return nil, withCode(exitEnvironment, fmt.Errorf("run check %d on %s: %w", i+1, shortOid(commit), err))
		}
		if run.ExitCode != nil {
			run.Passed, _ = outcomeMet(c.Expect, *run.ExitCode, output.String())
		}
		runs[i] = run
	}
	return runs, nil
}

// fixedOutcomes are the outcomes verify can judge, and a drafted check may
// expect, besides "prints <text>": each with whether a run met it.
var fixedOutcomes = map[string]func(code int, output string) bool{
	"exits 0":        func(code int, _ string) bool { return code == 0 },
	"exits 1":        func(code int, _ string) bool { return code == 1 },
	"exits non-zero": func(code int, _ string) bool { return code != 0 },
	"prints nothing": func(_ int, output string) bool { return strings.TrimSpace(output) == "" },
}

// outcomeMet says whether a run met expect. known is false for an outcome
// verify cannot read, which no run meets.
func outcomeMet(expect string, code int, output string) (met, known bool) {
	if judge, ok := fixedOutcomes[expect]; ok {
		return judge(code, output), true
	}
	if text, ok := strings.CutPrefix(expect, "prints "); ok {
		return strings.Contains(output, text), true
	}
	return false, false
}

func classifyCheck(expect string, base, head verifyRun) string {
	if _, known := outcomeMet(expect, 0, ""); !known || base.TimedOut || head.TimedOut {
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

func (r verifyRun) describe() string {
	switch {
	case r.TimedOut:
		return "timed out"
	case r.Passed:
		return fmt.Sprintf("passed (exit %d)", *r.ExitCode)
	}
	return fmt.Sprintf("failed (exit %d)", *r.ExitCode)
}
