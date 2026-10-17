package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/jsonout"
	"github.com/steig/worktender/internal/roles"
	"github.com/steig/worktender/internal/wt"
)

// factory takes the human out of the conveyor belt: issues labelled
// factory:ready are started oldest first, until the repository has --max
// live workers that have not reported done.
//
// It is one pass, not a loop, for the reason pr-feedback is: something else
// decides how often to look — cron, a shell loop, a coordinator — so there is
// no daemon to start, stop and keep alive. Everything a pass needs from the
// last one is already in the world: which issues are labelled, which branches
// exist, which workers herdr still has and what each last reported. Nothing is
// kept on disk, so nothing on disk can disagree with it.
//
// Starting is `start`, called through its own entry point with the same
// arguments a person would type. The queue decides which issue and when; how an
// issue becomes a worker stays in one place.

const factoryUsage = "usage: worktender factory [--max <n>] [--model <model>] [--repo <path>] [--dry-run] [--json]"

// factoryDefaultMax is small on purpose. The cap is a bet on how many workers
// a person can review behind, and it is not worth raising until the metrics
// say the throughput holds up.
const factoryDefaultMax = 2

// factoryReadyLimit bounds how many ready issues are read in one pass. gh lists
// newest first and pages up to the limit, so a queue longer than this would
// lose its oldest issues — the ones a pass starts. A thousand is far beyond
// any queue a cap of two drains.
const factoryReadyLimit = 1000

// The outcomes a pass reaches for each issue it looks at. Issues further down
// the queue than the free slots reach are not looked at and not listed.
const (
	factoryStarted    = "started"
	factoryWouldStart = "would_start"
	factorySkipped    = "skipped"
	factoryFailed     = "failed"
	// factoryClaimedElsewhere is an issue whose claim ref another coordinator
	// already holds on origin.
	factoryClaimedElsewhere = "claimed-elsewhere"
)

func factoryCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("factory", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	maxWorkers := fs.Int("max", factoryDefaultMax, "start issues until this many workers hold a slot")
	model := fs.String("model", "", "model to pass to every agent the pass starts, over any model: label")
	repo := fs.String("repo", "", "repository to act on, instead of the one herdr is currently in")
	dryRun := fs.Bool("dry-run", false, "say which issues would be started, and start none")
	asJSON := jsonFlag(fs)

	if err := fs.Parse(args); err != nil {
		return usagef("%v; %s", err, factoryUsage)
	}
	if fs.NArg() > 0 {
		return usagef("unexpected argument %q; %s", fs.Arg(0), factoryUsage)
	}
	if *maxWorkers < 1 {
		return usagef("--max must be at least 1, got %d; %s", *maxWorkers, factoryUsage)
	}
	// Refused here, before anything is claimed, rather than by every start in
	// the pass in turn.
	if *model != "" && !roles.IsModelName(*model) {
		return usagef("--model %q is not a model name; %s", *model, factoryUsage)
	}

	// Herdr is required even for a dry run: the cap is a count of live agents,
	// and only herdr can say which are alive and what they last reported. A dry
	// run with no herdr would have to guess the one number it exists to report.
	//
	// The repository is resolved as start resolves it, because start is what
	// runs — and a dry run resolves it the same way a real pass does. A dry run
	// changes nothing, so it could safely fall back to the working directory,
	// but then it would succeed exactly where the real pass refuses, planning
	// against a repository the real pass would never act on. A rehearsal is
	// only worth anything if it fails where the performance would.
	var s *session
	var err error
	if *repo != "" {
		s, err = newSessionIn(*repo, herdrRequired)
	} else {
		s, err = newSession(false, herdrRequired)
		// From cron there is never a context. Say the way past it, as start
		// does — this is the failure everyone scheduling it meets first.
		if errors.Is(err, herdrapi.ErrNoContext) {
			err = withCode(exitUsage, fmt.Errorf("%w; name it with --repo <path>", err))
		}
	}
	if err != nil {
		return err
	}

	notes := out
	emit := func(*factoryJSON) error { return nil }
	if *asJSON {
		notes = os.Stderr
		emit = func(doc *factoryJSON) error { return jsonout.Write(out, doc) }
	}
	doc := &factoryJSON{Repository: s.root, Max: *maxWorkers, DryRun: *dryRun, Issues: []factoryIssueJSON{}, Cleared: []factoryClearedJSON{}}
	fail := func(err error) error {
		if wErr := emit(doc.finish(err)); wErr != nil {
			return wErr
		}
		return err
	}

	fmt.Fprintf(notes, "repository: %s\n", s.root)

	// Cleared before the cap is looked at, so a pass at the cap — the pass
	// that runs most often while workers are out — still tidies the board.
	// A failure here is reported and the pass goes on: a stale label is
	// cosmetic, and the queue should not stall behind it.
	var firstErr error
	if err := clearClosedInProgress(s.root, *dryRun, notes, doc); err != nil {
		fmt.Fprintf(notes, "warning: %v\n", err)
		firstErr = err
	}

	// Checked before the cap, so every pass while main is red says so, and a
	// dry run refuses where a real pass would. A gh that cannot answer is a
	// warning: the gate is a safety net, not a reason to stall the queue.
	if url, err := redMain(s.root); err != nil {
		fmt.Fprintf(notes, "warning: could not read the default branch's latest ci run: %v\n", err)
	} else if url != "" {
		return fail(withCode(exitNeedsHuman, fmt.Errorf("the latest ci run on the default branch failed, so a worker would start from a red base; starting nothing until it is green: %s", url)))
	}

	rows, err := factoryRows(s.client, s.root)
	if err != nil {
		return fail(err)
	}
	doc.Running = busyWorkers(rows)
	fmt.Fprintf(notes, "slots held: %d of %d\n", doc.Running, *maxWorkers)

	free := *maxWorkers - doc.Running
	if free <= 0 {
		fmt.Fprintln(notes, "at the cap; starting nothing")
		return fail(firstErr)
	}

	ready, err := readyIssues(s.root)
	if err != nil {
		return fail(err)
	}
	if len(ready) == 0 {
		fmt.Fprintf(notes, "no open issues labelled %s\n", labelFactoryReady)
		return fail(firstErr)
	}

	taken, err := takenBranches(s.root, rows)
	if err != nil {
		return fail(err)
	}

	for _, candidate := range ready {
		if free == 0 {
			break
		}
		entry := factoryIssueJSON{Issue: candidate.Number, Title: candidate.Title}

		if reason := alreadyStarted(candidate, taken); reason != "" {
			entry.Outcome, entry.Reason = factorySkipped, jsonout.String(reason)
			doc.Issues = append(doc.Issues, entry)
			fmt.Fprintf(notes, "skipped #%d: %s\n", candidate.Number, reason)
			continue
		}

		if *dryRun {
			entry.Outcome = factoryWouldStart
			doc.Issues = append(doc.Issues, entry)
			fmt.Fprintf(notes, "would start #%d: %s\n", candidate.Number, candidate.Title)
			free--
			continue
		}

		// THE ORDER IS CLAIM, START, THEN THE LABEL. The branch check above
		// only sees this machine's branches and whatever has been pushed, and a
		// worker's branch is not pushed until its pull request opens, so two
		// coordinators could both pass it for a whole worker run. The claim is
		// the lock: a ref on origin that only one push can create. It is taken
		// before the start and given back if the start fails, so a failure
		// leaves the issue on the queue rather than held by nobody.
		//
		// The label comes last, as the board's view for humans, kept in step on
		// a best-effort basis. Labelling first would leave an issue marked in
		// progress whenever a start failed; labelling last, the failure left
		// over is a swap that did not happen after a worker did, and the next
		// pass skips that issue because its branch and its claim exist.
		claimed, err := claimIssue(s.root, candidate.Number)
		if err != nil {
			entry.Outcome, entry.Reason = factoryFailed, jsonout.String(err.Error())
			doc.Issues = append(doc.Issues, entry)
			fmt.Fprintf(notes, "failed to claim #%d: %v\n", candidate.Number, err)
			if firstErr == nil {
				firstErr = fmt.Errorf("claim #%d: %w", candidate.Number, err)
			}
			break
		}
		if !claimed {
			reason := "another coordinator holds " + claimRef(candidate.Number) + " on origin"
			entry.Outcome, entry.Reason = factoryClaimedElsewhere, jsonout.String(reason)
			doc.Issues = append(doc.Issues, entry)
			fmt.Fprintf(notes, "skipped #%d: %s: %s\n", candidate.Number, factoryClaimedElsewhere, reason)
			continue
		}

		fmt.Fprintf(notes, "\nstarting #%d: %s\n", candidate.Number, candidate.Title)
		startDoc, err := runStart(s.root, candidate.Number, *model, *asJSON, out)
		entry.Start = startDoc
		if err != nil {
			entry.Outcome, entry.Reason = factoryFailed, jsonout.String(err.Error())
			// A start can fail after the worktree exists, with an agent maybe
			// live in it. Its branch is not on origin, so the claim is the only
			// thing keeping another machine off the issue, and it stays.
			if rErr := releaseFailedClaim(s.root, candidate); rErr != nil {
				fmt.Fprintf(notes, "warning: #%d did not start and still holds its claim: %v\n", candidate.Number, rErr)
			}
			// start is where an issue's approval is checked — factoryApproval:
			// a write-access account's `worktender approve` hash must match the
			// issue as it stands — so manual starts and this queue cannot
			// disagree about it, and nothing here restates any part of it. A
			// refusal there is exit 3, and its message names the
			// `worktender approve` to run. The issue is skipped with that
			// message, keeps its label and holds no slot, and the pass moves on.
			// It still exits 3 at the end, so whoever reads the cron log learns
			// somebody is wanted.
			verb := "failed to start"
			if exitCode(err) == exitNeedsHuman {
				entry.Outcome, verb = factorySkipped, "skipped"
			}
			doc.Issues = append(doc.Issues, entry)
			fmt.Fprintf(notes, "%s #%d: %v\n", verb, candidate.Number, err)
			if firstErr == nil {
				firstErr = fmt.Errorf("start #%d: %w", candidate.Number, err)
			}
			// A failure that belongs to this issue — a refused approval, a role
			// nobody defined, an agent that never took its brief — leaves the
			// next one startable, and holding the whole queue behind one bad
			// issue would make the oldest problem everybody's problem. A failure
			// of the machine would fail every start after it, each of them maybe
			// leaving a checkout behind, so the pass stops there.
			if exitCode(err) == exitEnvironment {
				break
			}
			continue
		}
		entry.Outcome = factoryStarted
		free--

		if err := swapLabel(s.root, candidate.Number); err != nil {
			entry.Reason = jsonout.String(err.Error())
			doc.Issues = append(doc.Issues, entry)
			fmt.Fprintf(notes, "warning: #%d is started but still labelled %s: %v\n", candidate.Number, labelFactoryReady, err)
			// gh just failed, and the next start reads its issue through gh.
			if firstErr == nil {
				firstErr = withCode(exitEnvironment, err)
			}
			break
		}
		entry.LabelSwapped = true
		doc.Issues = append(doc.Issues, entry)
	}

	return fail(firstErr)
}

// runStart runs `start` for one issue, exactly as a person would type it.
//
// --repo is always passed: the factory has already resolved the repository,
// and start resolving it a second time from herdr's context could land
// somewhere else. In JSON mode start's own document is captured and carried
// in the factory's, rather than becoming a second document on stdout.
func runStart(root string, number int, model string, asJSON bool, out io.Writer) (json.RawMessage, error) {
	args := []string{strconv.Itoa(number), "--repo", root}
	if model != "" {
		args = append(args, "--model", model)
	}
	if !asJSON {
		return nil, startCommand(args, out)
	}

	var buf bytes.Buffer
	err := startCommand(append(args, "--json"), &buf)
	// A start that failed before it had anything to say writes nothing, and
	// nothing is a null here rather than an invalid document.
	if raw := bytes.TrimSpace(buf.Bytes()); json.Valid(raw) {
		return json.RawMessage(raw), err
	}
	return nil, err
}

// factoryRows is the repository's worktrees joined with herdr's workspaces,
// which is the same join `ls` prints, with each pane's last report: the
// reading `ls --reports` gives.
//
// The report is read through the session's own client rather than through
// paneReport, which dials HERDR_SOCKET_PATH. That is set only when herdr
// started us, and from cron it is not — every lookup would fail, and every
// worker would hold its slot whatever it had reported.
func factoryRows(client *herdrapi.Client, root string) ([]wt.Row, error) {
	worktrees, err := client.WorktreeList(root)
	if err != nil {
		return nil, fmt.Errorf("list worktrees: %w", err)
	}
	workspaces, err := client.WorkspaceList()
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	rows := wt.Rows(worktrees, workspaces)
	wt.WithPanes(client, rows)
	wt.WithReports(rows, func(pane string) (wt.Report, error) {
		info, err := client.PaneGet(pane)
		if err != nil {
			return wt.Report{}, err
		}
		r, _, ok := decodeReport(info.Pane.Tokens)
		if !ok {
			return wt.Report{}, nil
		}
		return wt.Report{Found: true, Status: r.status, PR: r.pr, Note: r.note}, nil
	})
	return rows, nil
}

// busyWorkers counts the slots the cap is about: every live worker in a
// checkout of this repository other than its main one, unless its last report
// says done.
//
// IDLE HOLDS A SLOT. A worker between turns is still mid-task — waiting on its
// pull request's checks, on a review, on the pr-feedback notice that wakes it
// — and spends again when it resumes. Counting only the agents working at this
// instant would let every pass start more while the earlier ones waited, and
// the number alive would grow without bound: the opposite of what the cap is
// for, which is keeping spend small until the metrics justify more.
//
// A slot is freed by the worker reporting done, or by its agent going — the
// pane closed, the worktree pruned, herdr seeing no agent there. A report that
// could not be read leaves the slot held, because the side to err on is
// starting too few. An open pull request would be a second signal, but it is a
// gh call per branch on every pass; the done report is the worker's own word
// that it has finished, and costs one herdr call.
//
// A ghost row counts too: its checkout is gone from git but herdr still has an
// agent in it.
func busyWorkers(rows []wt.Row) int {
	n := 0
	for _, row := range rows {
		if row.Main {
			continue
		}
		switch herdrapi.AgentStatus(row.AgentStatus) {
		case "", herdrapi.AgentStatusUnknown:
			continue
		}
		if r := row.Report; r != nil && r.Err == nil && r.Found && r.Status == "done" {
			continue
		}
		n++
	}
	return n
}

// readyIssues is every open issue labelled factory:ready, oldest first, in
// start's own issue shape: the labels are what tell a pass an issue is already
// in progress.
//
// Oldest by number, which GitHub assigns in creation order within a
// repository, so it is the same order as created-at without parsing a time.
// Sorted here rather than asked of gh's search, which is eventually consistent
// and could miss a label set a moment ago.
func readyIssues(root string) ([]issue, error) {
	out, err := factoryGh(root, "issue", "list", "--state", "open", "--label", labelFactoryReady,
		"--json", "number,title,labels", "--limit", strconv.Itoa(factoryReadyLimit))
	if err != nil {
		return nil, fmt.Errorf("list issues labelled %s: %w", labelFactoryReady, err)
	}
	var issues []issue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, withCode(exitEnvironment, fmt.Errorf("read the issues gh listed: %w", err))
	}
	slices.SortFunc(issues, func(a, b issue) int { return a.Number - b.Number })
	return issues, nil
}

// swapLabel moves an issue from the queue to in progress. One gh call, but not
// necessarily one write on GitHub's side; a swap that half-happened leaves an
// issue carrying both labels, which a pass reads as in progress.
func swapLabel(root string, number int) error {
	_, err := factoryGh(root, "issue", "edit", strconv.Itoa(number),
		"--add-label", labelFactoryInProgress, "--remove-label", labelFactoryReady)
	if err != nil {
		return fmt.Errorf("relabel #%d %s -> %s: %w", number, labelFactoryReady, labelFactoryInProgress, err)
	}
	return nil
}

// clearClosedInProgress takes factory:in-progress off every closed issue that
// still carries it, and its claim ref off origin, and says which. A worker's
// pull request closes its issue when it merges, and nothing else takes the
// label off: without this, closed issues read as work in progress and the
// label stops being a filter for what is.
//
// Only closed issues are asked for, so an open one is never touched, and only
// that label is removed. A dry run lists what it would clear and clears
// nothing. The first failed edit stops the clearing: gh just failed, and the
// next edit would go through the same gh.
func clearClosedInProgress(root string, dryRun bool, notes io.Writer, doc *factoryJSON) error {
	out, err := factoryGh(root, "issue", "list", "--state", "closed", "--label", labelFactoryInProgress,
		"--json", "number,title", "--limit", strconv.Itoa(factoryReadyLimit))
	if err != nil {
		return fmt.Errorf("list closed issues labelled %s: %w", labelFactoryInProgress, err)
	}
	var closed []issue
	if err := json.Unmarshal(out, &closed); err != nil {
		return withCode(exitEnvironment, fmt.Errorf("read the closed issues gh listed: %w", err))
	}
	slices.SortFunc(closed, func(a, b issue) int { return a.Number - b.Number })

	for _, c := range closed {
		entry := factoryClearedJSON{Issue: c.Number, Title: c.Title}
		if dryRun {
			doc.Cleared = append(doc.Cleared, entry)
			fmt.Fprintf(notes, "would remove %s from closed #%d: %s\n", labelFactoryInProgress, c.Number, c.Title)
			continue
		}
		// The claim goes before the label: a pass only finds this issue again
		// while the label is on it, so a claim left behind a removed label
		// would never be released.
		if err := releaseClaim(root, c.Number); err != nil {
			entry.Reason = jsonout.String(err.Error())
			doc.Cleared = append(doc.Cleared, entry)
			return fmt.Errorf("release the claim of closed #%d: %w", c.Number, err)
		}
		if _, err := factoryGh(root, "issue", "edit", strconv.Itoa(c.Number), "--remove-label", labelFactoryInProgress); err != nil {
			entry.Reason = jsonout.String(err.Error())
			doc.Cleared = append(doc.Cleared, entry)
			return fmt.Errorf("remove %s from closed #%d: %w", labelFactoryInProgress, c.Number, err)
		}
		entry.Removed = true
		doc.Cleared = append(doc.Cleared, entry)
		fmt.Fprintf(notes, "removed %s from closed #%d: %s\n", labelFactoryInProgress, c.Number, c.Title)
	}
	return nil
}

// redMainConclusions are the conclusions of a run that failed.
var redMainConclusions = []string{"failure", "timed_out", "startup_failure"}

// redMainSilent are the conclusions of a run that judged nothing about the
// code, so the run before it still says how the branch stands.
var redMainSilent = []string{"cancelled", "skipped", "neutral"}

// redMain is the URL of the latest completed ci run on the branch workers fork
// from that judged the code, when that run failed; "" when it passed or none of
// the last few judged anything.
func redMain(root string) (string, error) {
	branch := strings.TrimPrefix(gitx.BaseRef(root), "origin/")
	raw, err := factoryGh(root, "run", "list", "--workflow", "ci", "--branch", branch,
		"--status", "completed", "--limit", "5", "--json", "conclusion,url")
	if err != nil {
		return "", err
	}
	var runs []struct {
		Conclusion string `json:"conclusion"`
		URL        string `json:"url"`
	}
	if err := json.Unmarshal(raw, &runs); err != nil {
		return "", fmt.Errorf("read the ci runs gh listed: %w", err)
	}
	for _, run := range runs {
		if slices.Contains(redMainSilent, run.Conclusion) {
			continue
		}
		if slices.Contains(redMainConclusions, run.Conclusion) {
			return run.URL, nil
		}
		return "", nil
	}
	return "", nil
}

// factoryGh runs gh against the repository's origin, as start's issue lookup
// does: a worktree's own directory is not always enough for gh to tell which
// repository it is in.
func factoryGh(root string, args ...string) ([]byte, error) {
	if origin := gitx.RemoteURL(root); origin != "" {
		args = append(args, "--repo", origin)
	}
	cmd := exec.Command("gh", args...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return nil, withCode(exitEnvironment, fmt.Errorf("gh %s: %w; check `gh auth status`", args[0]+" "+args[1], err))
	}
	return out, nil
}

// claimRef is where a coordinator's claim on an issue lives on origin: under
// refs/worktender/ rather than refs/heads/, so it never shows up as a branch.
func claimRef(number int) string {
	return claimRefPrefix + strconv.Itoa(number)
}

const claimRefPrefix = "refs/worktender/claims/"

// claimIssue creates the issue's claim ref on origin, and says whether this
// pass now holds it. False with no error is another coordinator holding it.
//
// The empty lease makes the push create-only, and origin checks it as it
// updates the ref, so of two pushes racing for one claim exactly one creates
// it. The exit status does not say which: a push of the same commit to a ref
// that already holds it is "up to date" and exits 0, and two coordinators
// claiming from the same base push the same commit. So the claim is read from
// the porcelain status, where only "*" is a ref this push created.
//
// Of the refusals, only the lease's own say the ref exists. One from origin —
// a ruleset, a hook, missing permission — would otherwise read as every issue
// claimed elsewhere, and a pass that starts nothing would exit 0.
func claimIssue(root string, number int) (bool, error) {
	ref := claimRef(number)
	base := gitx.BaseRef(root)
	sha := gitx.Commit(root, base)
	if sha == "" {
		return false, withCode(exitEnvironment, fmt.Errorf("resolve %s in %s to claim #%d on", base, root, number))
	}
	out, err := factoryGit(root, "push", "--porcelain", "origin", sha+":"+ref, "--force-with-lease="+ref+":")
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 2 || !strings.HasSuffix(fields[1], ":"+ref) {
			continue
		}
		summary := strings.Join(fields[2:], " ")
		switch {
		case fields[0] == "*":
			return true, nil
		case fields[0] == "=":
			return false, nil
		case fields[0] == "!" && strings.HasPrefix(summary, "[rejected]") &&
			(strings.Contains(summary, "(stale info)") || strings.Contains(summary, "(already exists)")):
			return false, nil
		case fields[0] == "!":
			return false, withCode(exitEnvironment, fmt.Errorf("origin refused the claim %s: %s", ref, summary))
		}
	}
	if err == nil {
		err = withCode(exitEnvironment, fmt.Errorf("git push reported nothing for %s:\n%s", ref, out))
	}
	return false, err
}

// releaseFailedClaim gives back the claim of an issue whose start failed,
// unless the start got far enough to leave a branch or checkout behind.
func releaseFailedClaim(root string, candidate issue) error {
	taken, err := takenBranches(root, nil)
	if err != nil {
		return err
	}
	if reason := alreadyStarted(candidate, taken); reason != "" {
		return fmt.Errorf("%s; once nothing is working on it, clear it with `git push origin :%s`", reason, claimRef(candidate.Number))
	}
	return releaseClaim(root, candidate.Number)
}

// releaseClaim deletes the issue's claim ref from origin. A claim that is not
// there is already released: an issue started before claims existed has none.
func releaseClaim(root string, number int) error {
	_, err := factoryGit(root, "push", "--porcelain", "origin", ":"+claimRef(number))
	if err != nil && strings.Contains(err.Error(), "remote ref does not exist") {
		return nil
	}
	return err
}

// factoryGitTimeout bounds each call to origin, so a hung remote fails the
// pass rather than holding it, and the cron line behind it, forever.
const factoryGitTimeout = time.Minute

// factoryGit runs git in root and returns its stdout, carrying stderr in the
// error, for the calls that reach origin. It never prompts: from cron there is
// nobody to answer.
func factoryGit(root string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), factoryGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return string(out), withCode(exitEnvironment, fmt.Errorf("git %s: %w", args[0], err))
	}
	return string(out), nil
}

// takenBranches is every name an issue's work could already be living under:
// local branches, remote-tracking branches with the remote taken off, and the
// branch and directory of every checkout herdr knows.
//
// Remote branches are included so a worker started from another machine, or a
// branch pushed and since deleted locally, still counts. A stale remote ref
// errs towards skipping, which is the side to err on: a skipped issue costs a
// look, a second worker costs a duplicate pull request.
func takenBranches(root string, rows []wt.Row) ([]string, error) {
	local, err := gitRefs(root, "refs/heads", 2)
	if err != nil {
		return nil, err
	}
	remote, err := gitRefs(root, "refs/remotes", 3)
	if err != nil {
		return nil, err
	}
	names := append(local, remote...)
	for _, row := range rows {
		names = append(names, row.Branch, row.Dir)
	}
	return names, nil
}

// gitRefs lists the refs under prefix with the first strip components taken
// off their names.
func gitRefs(root, prefix string, strip int) ([]string, error) {
	cmd := exec.Command("git", "for-each-ref", fmt.Sprintf("--format=%%(refname:lstrip=%d)", strip), prefix)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, withCode(exitEnvironment, fmt.Errorf("list %s in %s: %w", prefix, root, err))
	}
	return strings.Fields(string(out)), nil
}

// alreadyStarted says why an issue must not be started again, or "" when it
// may be.
//
// The branch test matches start's naming — the number, then a hyphen and the
// title's slug — by the number alone. A title edited since the start changes
// the slug and not the number, and matching on the whole name would then miss
// the branch and start a second worker. The hyphen is what keeps #2 from
// matching 20-something.
func alreadyStarted(candidate issue, taken []string) string {
	if slices.Contains(candidate.labelNames(), labelFactoryInProgress) {
		return "already labelled " + labelFactoryInProgress
	}
	prefix := strconv.Itoa(candidate.Number)
	for _, name := range taken {
		if name == prefix || strings.HasPrefix(name, prefix+"-") {
			return fmt.Sprintf("%s already exists, so a worker was started on it before", name)
		}
	}
	return ""
}

// factoryJSON is `factory --json`. Written on the failure paths too, as start's
// is: a pass that started one issue and then failed has changed the world, and
// the caller needs to know which part.
type factoryJSON struct {
	Repository string `json:"repository"`
	Max        int    `json:"max"`
	// Running is the slots live workers held before anything was started: the
	// number the cap was measured against.
	Running int  `json:"running"`
	DryRun  bool `json:"dry_run"`
	// Issues is every issue the pass looked at, in the order it looked.
	Issues []factoryIssueJSON `json:"issues"`
	// Cleared is every closed issue the pass found still labelled
	// factory:in-progress, oldest first.
	Cleared  []factoryClearedJSON `json:"cleared"`
	Error    *string              `json:"error"`
	ExitCode int                  `json:"exit_code"`
}

type factoryClearedJSON struct {
	Issue int    `json:"issue"`
	Title string `json:"title"`
	// Removed is whether the label came off; false on a dry run, and on a
	// failed edit, whose error is Reason.
	Removed bool    `json:"removed"`
	Reason  *string `json:"reason"`
}

type factoryIssueJSON struct {
	Issue int    `json:"issue"`
	Title string `json:"title"`
	// Outcome is started, would_start, skipped, claimed-elsewhere or failed.
	Outcome string `json:"outcome"`
	// Reason is why it was skipped or failed, and on a started issue why its
	// label could not be swapped.
	Reason       *string `json:"reason"`
	LabelSwapped bool    `json:"label_swapped"`
	// Start is `start --json`'s own document for this issue, null when start
	// did not run or had nothing to say.
	Start json.RawMessage `json:"start"`
}

func (d *factoryJSON) finish(err error) *factoryJSON {
	d.ExitCode = exitCode(err)
	if err != nil {
		msg := err.Error()
		d.Error = &msg
	}
	return d
}
