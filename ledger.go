package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/steig/worktender/internal/execute"
	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/roles"
)

// The dispatch ledger is what `worktender stats` is computed from: one JSON
// line per thing worktender saw happen to a dispatch, appended at the moment it
// saw it. Without it nobody can say whether the factory is helping or only
// burning effort, because every one of these facts is otherwise gone the moment
// the pane that produced it closes.
//
// It records only what worktender itself observes, at the places it already
// observes something:
//
//   - start, when an agent has been briefed on an issue. The only place the
//     issue, the role and the agent kind are all known at once.
//   - report, when a worker claims a status. The worker's own clock, from its
//     own worktree, so the dispatch is found by repository and branch.
//   - gate, when a coordinator opens a wait. A gate run is coordination, and
//     counting them is half of what the ceremony ratio is made of.
//   - pr-feedback, when it types a notice into a worker. A round of rework on
//     a PR, by repository and branch like a report.
//
// The inbox is NOT appended to from here. It is already a durable,
// timestamped, append-only log of its own, and `stats` reads its threads
// directly — a second copy of every message would be two records of one fact,
// free to disagree.
//
// Recording is best effort and says so. A ledger that cannot be written warns
// on stderr and the command carries on: a worker whose report was refused
// because a metrics file was unwritable would have lost the report to save
// the statistic about it, which is the wrong way round.
//
// Location: <plugin state dir>/ledger.jsonl, resolved exactly as the inbox's
// directory is — HERDR_PLUGIN_STATE_DIR when herdr supplied it, else
// defaultStateDir — and for the same reason: workers append from their own
// shell, where herdr never sets the variable. Retention is none, as for the
// inbox: append-only and unbounded until real growth says otherwise.

// ledgerEvent is one line of the ledger. One struct for every kind, with the
// fields a kind does not carry left out, so a reader decodes a line before it
// knows what it is.
type ledgerEvent struct {
	// V is the line format. A reader skips a version it does not know rather
	// than misreading it.
	V     int    `json:"v"`
	Event string `json:"event"`
	At    string `json:"at"`

	// Repository and Branch are how a report finds its dispatch: a worker knows
	// the checkout it is standing in and nothing else about how it was started.
	Repository string `json:"repository,omitempty"`
	Branch     string `json:"branch,omitempty"`
	PaneID     string `json:"pane_id,omitempty"`

	// start only.
	Issue     int    `json:"issue,omitempty"`
	Origin    string `json:"origin,omitempty"`
	Role      string `json:"role,omitempty"`
	Agent     string `json:"agent,omitempty"`
	AgentKind string `json:"agent_kind,omitempty"`
	AgentName string `json:"agent_name,omitempty"`

	// report only, except that feedback carries PR too.
	Status string `json:"status,omitempty"`
	PR     int    `json:"pr,omitempty"`

	// gate only. Every worker the wait covered, as the caller named it and as
	// herdr resolved it, because a coordinator may name a worker either way.
	Targets []ledgerTarget `json:"targets,omitempty"`
	Until   []string       `json:"until,omitempty"`

	// draft only, beside Repository, Origin and Issue.
	Drafted int  `json:"drafted,omitempty"`
	Dropped int  `json:"dropped,omitempty"`
	Written bool `json:"written,omitempty"`
}

type ledgerTarget struct {
	Name   string `json:"name"`
	PaneID string `json:"pane_id"`
}

const ledgerVersion = 1

const (
	ledgerStart  = "start"
	ledgerReport = "report"
	ledgerGate   = "gate"
	// ledgerFeedback is one pr-feedback notice typed at a worker: a round of
	// rework on its PR that nobody had to ask for. It is a kind added within
	// version 1, which is safe because readers dispatch on the kind and pass
	// over one they do not know; the shape of a line did not change.
	ledgerFeedback = "feedback"
	// ledgerDraft is one `worktender draft` that got an answer from its
	// drafter: how many checks it kept and dropped, and whether they were
	// written to the issue. Like feedback, a kind added within version 1. It
	// is what lets stats tell drafted checks from hand-written ones later.
	ledgerDraft = "draft"
)

// ledgerPath is where the ledger lives, or an error when no state directory can
// be named at all.
func ledgerPath() (string, error) {
	base := stateDir()
	if base == "" {
		base = defaultStateDir()
	}
	if base == "" {
		return "", withCode(exitEnvironment, errors.New(
			"no plugin state directory: HERDR_PLUGIN_STATE_DIR is unset and no default location could be derived"))
	}
	return filepath.Join(base, "ledger.jsonl"), nil
}

// ledgerNow is the clock every event is stamped with. A var so the suite can
// lay down a dispatch whose durations it knows.
var ledgerNow = func() time.Time { return time.Now().UTC() }

// appendLedger writes one event as one line.
//
// The line is encoded whole and handed to a single write(2) on a file opened
// O_APPEND — the same discipline appendInboxMessage keeps, and for the same
// reason: a coordinator's gate and five workers' reports land in this one file
// from separate processes, and O_APPEND with one write per line is what keeps
// their lines from interleaving on a local filesystem. A torn line, should one
// ever happen, costs that line: the reader skips what it cannot decode.
func appendLedger(e ledgerEvent) error {
	path, err := ledgerPath()
	if err != nil {
		return err
	}
	e.V = ledgerVersion
	if e.At == "" {
		e.At = ledgerNow().Format(time.RFC3339Nano)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(buf.Bytes())
	return err
}

// recordLedger appends and, on failure, says so on stderr and nothing more.
// Never fatal: see the package note above.
func recordLedger(e ledgerEvent) {
	if err := appendLedger(e); err != nil {
		fmt.Fprintf(os.Stderr, "worktender: warning: the dispatch ledger did not record this %s: %v; `worktender stats` will be missing it\n", e.Event, err)
	}
}

// recordStart is the seam `start` calls once its agent has been briefed — not
// before, because a start that never reached an agent is not a dispatch and
// would count as one with no reports forever.
//
// The origin is read now rather than when stats runs, so a dispatch can still
// be looked up after its repository has moved or its checkout been removed.
func recordStart(root string, issue int, branch string, launch *roles.Launch, agentName, pane string) {
	e := ledgerEvent{
		Event: ledgerStart, Repository: gitx.Resolve(root), Branch: branch, PaneID: pane,
		Issue: issue, Origin: gitx.RemoteURL(root), AgentKind: execute.AgentKind, AgentName: agentName,
	}
	if launch != nil {
		e.Role, e.Agent = launch.Role, launch.Agent
	}
	recordLedger(e)
}

// recordReport appends the report a worker just made, keyed by the checkout it
// was made from.
//
// The working directory rather than the pane: a pane id is herdr's, and herdr
// hands the same short ids out again after a restart, where a repository and a
// branch name one piece of work for as long as it exists. The pane is kept
// beside them as a second witness, not a key.
func recordReport(r report) {
	e := ledgerEvent{Event: ledgerReport, Status: r.status, PR: r.pr, PaneID: os.Getenv(paneEnv)}
	if dir, err := os.Getwd(); err == nil {
		if root, err := gitx.RepoRoot(dir); err == nil {
			e.Repository = root
			e.Branch = gitx.Branch(dir)
		}
	}
	recordLedger(e)
}

// recordGate appends that a coordinator opened a wait. Recorded when the wait
// opens, not when it ends: a gate the coordinator's own tool call killed at its
// timeout never reaches an ending, and it was coordination all the same.
func recordGate(opts gateOptions, targets []gateTarget) {
	e := ledgerEvent{Event: ledgerGate, Until: opts.until}
	for _, t := range targets {
		e.Targets = append(e.Targets, ledgerTarget{Name: t.name, PaneID: t.pane})
	}
	recordLedger(e)
}

// readLedger returns every event in append order, and how many lines it could
// not read.
//
// A missing ledger is an empty one: nothing has been dispatched yet. A line that
// does not decode, or carries a version this binary does not know, is skipped
// and counted rather than failing the read, unlike an inbox thread: one torn
// metrics line should cost one dispatch's figures, not every dispatch's.
func readLedger(path string) ([]ledgerEvent, int, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, withCode(exitEnvironment, fmt.Errorf("read the dispatch ledger: %w", err))
	}
	defer file.Close()
	return decodeLedger(file)
}

func decodeLedger(r io.Reader) ([]ledgerEvent, int, error) {
	var events []ledgerEvent
	skipped := 0
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var e ledgerEvent
		if err := json.Unmarshal(line, &e); err != nil || e.V != ledgerVersion {
			skipped++
			continue
		}
		events = append(events, e)
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, withCode(exitEnvironment, fmt.Errorf("read the dispatch ledger: %w", err))
	}
	return events, skipped, nil
}
