package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/steig/worktender/internal/execute"
	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrapi"
)

// A report is also announced to the coordinator that dispatched the worker, so
// nobody has to ask where it is. The line typed is built from the closed slots
// alone — status, pr, sha, the issue number — and never the note, which is
// untrusted text and stays behind the framed envelope a gate reads.

// coordinatorPath is where `start` records the pane it was run from, relative
// to the worktree's git dir beside startedRolePath and for the same reasons. A
// worktree with no record has no coordinator, and its reports wake nobody.
const coordinatorPath = "worktender/coordinator"

// wakeWait bounds how long a report holds its worker waiting for the
// coordinator to be free to type into. Whatever is not sent by then stays
// queued and rides along with the next report's line.
var (
	wakeWait = 30 * time.Second
	wakePoll = time.Second
)

// wakeEntry is the only shape a queued entry may have: a Unix time, then the
// slots. The queue is a file in plugin state a worker can write, so what it
// holds is checked again on the way out rather than trusted.
var wakeEntry = regexp.MustCompile(`^([0-9]+) #([0-9]+) (planned|blocked|done) pr ([0-9]+|-) sha ([0-9a-f]{7}|-)$`)

// wakeStale is how old a queued entry may be and still be typed; a line typed
// hours late reads as news.
const wakeStale = 5 * time.Minute

var wakeNow = time.Now

// errNotCoordinator marks a live pane that is not who start recorded.
var errNotCoordinator = errors.New("not the recorded coordinator")

// wakePane bounds a pane id to what herdr issues (`w2:p1`), and so to what can
// name a file once the colon is swapped out.
var wakePane = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

// coordinator is who `start` recorded as the one to wake. The pane id alone is
// not identity: a worker can edit the record, so the live pane must still be
// the same terminal running the same agent.
type coordinator struct {
	pane, terminal, agent string
	issue                 int
}

// recordCoordinator is `start` writing down the pane it ran in, who is in it,
// and the issue the worker was given.
func recordCoordinator(client *herdrapi.Client, root, branch, pane string, issue int) error {
	info, err := client.AgentGet(pane)
	if err != nil {
		return err
	}
	agent := ""
	if info.Agent.Agent != nil {
		agent = *info.Agent.Agent
	}
	checkout, err := worktreeFor(root, branch)
	if err != nil {
		return err
	}
	path, err := gitPathIn(checkout, coordinatorPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, fmt.Appendf(nil, "%s\n%s\n%s\n%d\n", pane, info.Agent.TerminalID, agent, issue), 0o644)
}

// recordedCoordinator is the record for the checkout at dir, or the zero value
// for none usable. A record whose issue is not the one the branch was started
// for is no record.
func recordedCoordinator(dir string) coordinator {
	path, err := gitPathIn(dir, coordinatorPath)
	if err != nil {
		return coordinator{}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return coordinator{}
	}
	f := strings.Fields(string(raw))
	if len(f) != 4 || !wakePane.MatchString(f[0]) || f[3] != strconv.Itoa(branchIssue(dir)) {
		return coordinator{}
	}
	issue, _ := strconv.Atoi(f[3])
	return coordinator{pane: f[0], terminal: f[1], agent: f[2], issue: issue}
}

// branchIssue is the issue number `start` put at the front of the branch name,
// or 0.
func branchIssue(dir string) int {
	name := gitx.Branch(dir)
	name = name[strings.LastIndex(name, "/")+1:]
	n, _, _ := strings.Cut(name, "-")
	issue, _ := strconv.Atoi(n)
	return issue
}

// wakeFragment is one report as the coordinator reads it.
func wakeFragment(issue int, r report) string {
	sha := "-"
	if len(r.sha) >= 7 {
		sha = r.sha[:7]
	}
	return fmt.Sprintf("#%d %s pr %s sha %s", issue, r.status, prSlot(r), sha)
}

// wakeCoordinator announces r to the coordinator that dispatched this worker.
// Best effort: whatever goes wrong is a warning, never a failed report.
func wakeCoordinator(r report) {
	if os.Getenv(paneEnv) == "" || os.Getenv(socketEnv) == "" {
		return
	}
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	c := recordedCoordinator(dir)
	if c.pane == "" || c.pane == os.Getenv(paneEnv) {
		return
	}
	if err := wakeFor(c, wakeFragment(c.issue, r)); err != nil {
		fmt.Fprintf(os.Stderr, "worktender: warning: did not wake coordinator %s: %v\n", c.pane, err)
	}
}

// wakeFor queues the fragment and types the queue when the coordinator is free.
// The queue is per worker, since an entry for any other issue is dropped.
func wakeFor(c coordinator, fragment string) error {
	queue := filepath.Join(pluginStateDir(), "wake", fmt.Sprintf("%s-%d.queue", strings.ReplaceAll(c.pane, ":", "_"), c.issue))
	if err := os.MkdirAll(filepath.Dir(queue), 0o755); err != nil {
		return err
	}
	if err := appendQueue(queue, []string{fmt.Sprintf("%d %s", wakeNow().Unix(), fragment)}); err != nil {
		return err
	}

	client, err := herdrapi.New()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(wakeWait)
	for {
		ready, err := coordinatorReady(client, c)
		if errors.Is(err, errNotCoordinator) {
			// Its pane id has been handed out again to something that should
			// not read this. A transport error says nothing of the kind, so the
			// queue survives it.
			_ = os.Remove(queue)
		}
		if err != nil {
			return err
		}
		if ready {
			return flushWake(client, c, queue)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("coordinator was busy for %s; the line stays queued for its next report", wakeWait)
		}
		time.Sleep(wakePoll)
	}
}

func appendQueue(queue string, lines []string) error {
	file, err := os.OpenFile(queue, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = file.WriteString(strings.Join(lines, "\n") + "\n")
	if cErr := file.Close(); err == nil {
		err = cErr
	}
	return err
}

// flushWake types everything queued as one line. The queue is claimed by
// rename, so of several reports waiting on one coordinator exactly one types.
//
// Enter is pressed once and never retried, unlike the brief's: that loop can
// keep pressing for seconds, and a human who has started typing by then would
// have their message submitted. If the composer no longer holds exactly the
// wake text, nothing is pressed. Any failure puts the entries back.
func flushWake(client *herdrapi.Client, c coordinator, queue string) error {
	claimed := fmt.Sprintf("%s.%d", queue, os.Getpid())
	if err := os.Rename(queue, claimed); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer os.Remove(claimed)
	raw, err := os.ReadFile(claimed)
	if err != nil {
		return err
	}
	var kept, said []string
	for _, line := range strings.Split(string(raw), "\n") {
		m := wakeEntry.FindStringSubmatch(line)
		if m == nil || m[2] != strconv.Itoa(c.issue) {
			continue
		}
		if at, _ := strconv.ParseInt(m[1], 10, 64); wakeNow().Sub(time.Unix(at, 0)) > wakeStale {
			continue
		}
		kept = append(kept, line)
		said = append(said, strings.TrimPrefix(line, m[1]+" "))
	}
	if len(kept) == 0 {
		return nil
	}
	requeue := func(err error) error {
		if qErr := appendQueue(queue, kept); qErr != nil {
			return fmt.Errorf("%w (and could not requeue: %v)", err, qErr)
		}
		return err
	}
	text := "worktender: " + strings.Join(said, "; ")
	if err := client.PaneSendText(c.pane, text); err != nil {
		return requeue(fmt.Errorf("type into %s: %w", c.pane, err))
	}
	box, found, err := composerBox(client, c.pane)
	if err != nil {
		return requeue(err)
	}
	// Exactly the wake line: text typed after it, or no box to read, is not
	// ours to submit.
	if !found || box != text {
		return requeue(fmt.Errorf("the composer in %s does not hold exactly the wake line; not submitting", c.pane))
	}
	if err := client.PaneSendKeys(c.pane, []string{briefSubmitKey}); err != nil {
		return requeue(fmt.Errorf("submit in %s: %w", c.pane, err))
	}
	return nil
}

// coordinatorReady is true only for the recorded agent at its prompt with
// nothing typed in it: idle, or done, which is how herdr shows a finished turn
// nobody has looked at yet. A coordinator mid-turn would take the line as an
// interjection, and one with a half-written message would have the line run
// into it. A pane that is not the recorded terminal running the recorded claude
// is an error, since `❯` is also a plain shell's prompt.
func coordinatorReady(client *herdrapi.Client, c coordinator) (bool, error) {
	info, err := client.AgentGet(c.pane)
	if err != nil {
		return false, err
	}
	agent := ""
	if info.Agent.Agent != nil {
		agent = *info.Agent.Agent
	}
	if info.Agent.TerminalID != c.terminal || agent != c.agent || agent != execute.AgentKind {
		return false, fmt.Errorf("%w: %s is not the %s that dispatched this worker", errNotCoordinator, c.pane, execute.AgentKind)
	}
	if s := info.Agent.AgentStatus; s != herdrapi.AgentStatusIdle && s != herdrapi.AgentStatusDone {
		return false, nil
	}
	box, found, err := composerBox(client, c.pane)
	// Claude Code draws a placeholder in an empty box.
	return found && (box == "" || strings.HasPrefix(box, `Try "`)), err
}

// composerBox is what the input box on screen in pane holds: the last `❯`
// line, earlier ones being the transcript. found is false when there is none.
func composerBox(client *herdrapi.Client, pane string) (string, bool, error) {
	read, err := client.PaneRead(pane, herdrapi.ReadSourceVisible)
	if err != nil {
		return "", false, err
	}
	lines := strings.Split(read.Read.Text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); strings.HasPrefix(line, "❯") {
			return strings.TrimSpace(strings.TrimPrefix(line, "❯")), true, nil
		}
	}
	return "", false, nil
}
