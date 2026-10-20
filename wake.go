package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

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

// wakeEntry is the only shape a queued entry may have. The queue is a file, so
// what it holds is checked again on the way out rather than trusted.
var wakeEntry = regexp.MustCompile(`^#[0-9]+ (planned|blocked|done) pr ([0-9]+|-) sha ([0-9a-f]{7}|-)$`)

// wakePane bounds a pane id to what herdr issues (`w2:p1`), and so to what can
// name a file once the colon is swapped out.
var wakePane = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

// recordCoordinator is `start` writing down the pane it ran in and the issue
// the worker was given.
func recordCoordinator(root, branch, pane string, issue int) error {
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
	return os.WriteFile(path, fmt.Appendf(nil, "%s\n%d\n", pane, issue), 0o644)
}

// recordedCoordinator is the pane and issue recorded for the checkout at dir,
// or "" for none usable.
func recordedCoordinator(dir string) (string, int) {
	path, err := gitPathIn(dir, coordinatorPath)
	if err != nil {
		return "", 0
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 || !wakePane.MatchString(fields[0]) {
		return "", 0
	}
	issue, err := strconv.Atoi(fields[1])
	if err != nil || issue <= 0 {
		return "", 0
	}
	return fields[0], issue
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
	pane, issue := recordedCoordinator(dir)
	if pane == "" || pane == os.Getenv(paneEnv) {
		return
	}
	if err := wakeFor(pane, wakeFragment(issue, r)); err != nil {
		fmt.Fprintf(os.Stderr, "worktender: warning: did not wake coordinator %s: %v\n", pane, err)
	}
}

func wakeFor(pane, fragment string) error {
	queue := filepath.Join(pluginStateDir(), "wake", strings.ReplaceAll(pane, ":", "_")+".queue")
	if err := os.MkdirAll(filepath.Dir(queue), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(queue, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = file.WriteString(fragment + "\n")
	if cErr := file.Close(); err == nil {
		err = cErr
	}
	if err != nil {
		return err
	}

	client, err := herdrapi.New()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(wakeWait)
	for {
		ready, err := coordinatorReady(client, pane)
		if err != nil {
			// A coordinator that is gone will never drain this; its pane id
			// can be handed out again to something that should not read it.
			_ = os.Remove(queue)
			return err
		}
		if ready {
			return flushWake(client, pane, queue)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("coordinator was busy for %s; the line stays queued for its next report", wakeWait)
		}
		time.Sleep(wakePoll)
	}
}

// flushWake types everything queued as one line. The queue is claimed by
// rename, so of several reports waiting on one coordinator exactly one types.
func flushWake(client *herdrapi.Client, pane, queue string) error {
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
	var entries []string
	for _, line := range strings.Split(string(raw), "\n") {
		if wakeEntry.MatchString(line) {
			entries = append(entries, line)
		}
	}
	if len(entries) == 0 {
		return nil
	}
	text := "worktender: " + strings.Join(entries, "; ")
	if err := client.PaneSendText(pane, text); err != nil {
		return fmt.Errorf("type into %s: %w", pane, err)
	}
	return submitBrief(client, pane, text)
}

// coordinatorReady is true only for an agent at its prompt with nothing typed
// in it: idle, or done, which is how herdr shows a finished turn nobody has
// looked at yet. A coordinator mid-turn would take the line as an interjection,
// and one with a half-written message would have the line run into it.
func coordinatorReady(client *herdrapi.Client, pane string) (bool, error) {
	info, err := client.AgentGet(pane)
	if err != nil {
		return false, err
	}
	if s := info.Agent.AgentStatus; s != herdrapi.AgentStatusIdle && s != herdrapi.AgentStatusDone {
		return false, nil
	}
	read, err := client.PaneRead(pane, herdrapi.ReadSourceVisible)
	if err != nil {
		return false, err
	}
	lines := strings.Split(read.Read.Text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); strings.HasPrefix(line, "❯") {
			box := strings.TrimSpace(strings.TrimPrefix(line, "❯"))
			// Claude Code draws a placeholder in an empty box.
			return box == "" || strings.HasPrefix(box, `Try "`), nil
		}
	}
	// No box on screen says nothing about whether it is empty.
	return false, nil
}
