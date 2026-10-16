package main

import (
	"bytes"
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

	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/jsonout"
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

const factoryUsage = "usage: worktender factory [--max <n>] [--repo <path>] [--dry-run] [--json]"

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
)

func factoryCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("factory", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	maxWorkers := fs.Int("max", factoryDefaultMax, "start issues until this many workers hold a slot")
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

	// Herdr is required even for a dry run: the cap is a count of live agents,
	// and only herdr can say which are alive and what they last reported. A dry
	// run with no herdr would have to guess the one number it exists to report.
	//
	// The repository is resolved as start resolves it, because start is what
	// runs. A dry run changes nothing and may fall back to the working
	// directory; a real pass creates checkouts and may not.
	var s *session
	var err error
	if *repo != "" {
		s, err = newSessionIn(*repo, herdrRequired)
	} else {
		s, err = newSession(*dryRun, herdrRequired)
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
	doc := &factoryJSON{Repository: s.root, Max: *maxWorkers, DryRun: *dryRun, Issues: []factoryIssueJSON{}}
	fail := func(err error) error {
		if wErr := emit(doc.finish(err)); wErr != nil {
			return wErr
		}
		return err
	}

	fmt.Fprintf(notes, "repository: %s\n", s.root)

	rows, err := factoryRows(s.client, s.root)
	if err != nil {
		return fail(err)
	}
	doc.Running = busyWorkers(rows)
	fmt.Fprintf(notes, "slots held: %d of %d\n", doc.Running, *maxWorkers)

	free := *maxWorkers - doc.Running
	if free <= 0 {
		fmt.Fprintln(notes, "at the cap; starting nothing")
		return emit(doc.finish(nil))
	}

	ready, err := readyIssues(s.root)
	if err != nil {
		return fail(err)
	}
	if len(ready) == 0 {
		fmt.Fprintf(notes, "no open issues labelled %s\n", labelFactoryReady)
		return emit(doc.finish(nil))
	}

	taken, err := takenBranches(s.root, rows)
	if err != nil {
		return fail(err)
	}

	var firstErr error
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

		// THE ORDER IS START, THEN THE LABEL. The other order — claim the issue
		// by labelling it, then start — fails worse. A start that fails after
		// the claim leaves an issue labelled in-progress with nobody working
		// on it: gone from the queue and looking handled to anyone reading the
		// board. Starting first, the failure left over is a label swap that did
		// not happen after a worker did, and the next pass meets that issue
		// still labelled ready — and skips it, because its branch exists.
		//
		// That is the guarantee, and it rests on the branch rather than on the
		// label: start names the branch for the issue number, and git and herdr
		// are asked for it before every start. The label is the board's view
		// for humans, kept in step on a best-effort basis.
		fmt.Fprintf(notes, "\nstarting #%d: %s\n", candidate.Number, candidate.Title)
		startDoc, err := runStart(s.root, candidate.Number, *asJSON, out)
		entry.Start = startDoc
		if err != nil {
			entry.Outcome, entry.Reason = factoryFailed, jsonout.String(err.Error())
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
func runStart(root string, number int, asJSON bool, out io.Writer) (json.RawMessage, error) {
	args := []string{strconv.Itoa(number), "--repo", root}
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
	Issues   []factoryIssueJSON `json:"issues"`
	Error    *string            `json:"error"`
	ExitCode int                `json:"exit_code"`
}

type factoryIssueJSON struct {
	Issue int    `json:"issue"`
	Title string `json:"title"`
	// Outcome is started, would_start, skipped or failed.
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
