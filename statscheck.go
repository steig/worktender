package main

import (
	"fmt"
	"time"

	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/safetext"
)

// `stats --check` turns three of the figures into alerts, for cron and the
// watch mod: one line per alert on stdout, and exit 3 (needs a human) when
// there is any. Quiet is no output and exit 0, so a cron job that mails its
// output mails only when something is wrong.
//
// Every default below is set to stay quiet on a healthy fleet and fire on one
// that is plainly not, rather than to catch the first sign of drift. An alert
// that fires on ordinary work is turned off within a week, and then it catches
// nothing at all. Each is a flag, so a fleet that knows its own normal can
// tighten it.

type statsThresholds struct {
	// ceremonyRatio is inbox messages plus gate runs per 100 changed lines.
	// 20 means one coordination event for every five lines changed. A
	// typical dispatch — a gate or two, a handful of inbox messages — over a
	// change of any real size reads well under 5; 20 is a dispatch where
	// talking about the change has outgrown the change.
	ceremonyRatio float64

	// proofLoop is gate runs and reports after the worker said done with no
	// commit on its branch since. A coordinator gating once more for review
	// and the worker re-reporting once is normal; 3 leaves room for that and
	// for a second look, and a fourth round with nothing new committed is
	// the recursive proof loop #204 describes — the gate already passed and
	// everyone is re-proving it.
	proofLoop int

	// escapeRate is the share of merged pull requests reverted or blamed
	// within the escape window. 0.2 — one in five — is past what published
	// change-failure-rate surveys put even middling teams at, so crossing it
	// is a factory merging work that is not done, not bad luck.
	escapeRate float64
}

var defaultThresholds = statsThresholds{ceremonyRatio: 20, proofLoop: 3, escapeRate: 0.2}

// ceremonyAlertFloor is the least ceremony a dispatch must have before its
// ratio can alert. Per hundred lines, a one-line fix gated once is 100: the
// arithmetic of a small denominator, not bureaucracy. Five events is past a
// gate and its reply.
const ceremonyAlertFloor = 5

// escapeAlertFloor is the fewest merged pull requests the escape rate must be
// over before it can alert. One revert in the first two merges is 50% and
// says nothing yet.
const escapeAlertFloor = 5

// defaultCheckSince is how far back --check looks when --since is not given.
const defaultCheckSince = 30 * 24 * time.Hour

const (
	alertCeremony   = "ceremony"
	alertProofLoop  = "proof_loop"
	alertEscapeRate = "escape_rate"
)

// statsAlert is one threshold crossed. Issue and Branch are null for the
// escape rate, which is about the fleet rather than any one dispatch.
type statsAlert struct {
	Kind      string  `json:"kind"`
	Issue     *int    `json:"issue"`
	Branch    *string `json:"branch"`
	Value     float64 `json:"value"`
	Threshold float64 `json:"threshold"`
	Message   string  `json:"message"`
}

// branchCommitTime is when a dispatch's branch last moved. A var only so the
// seam is named; the suite drives it through a real repository.
var branchCommitTime = gitx.CommitTime

// checkStats is every alert the dispatches cross, in dispatch order with the
// fleet-wide escape rate last. Never nil, so --json can tell "checked, quiet"
// from "not checked".
func checkStats(dispatches []*statsDispatch, th statsThresholds, window time.Duration) []statsAlert {
	alerts := []statsAlert{}
	for _, d := range dispatches {
		issue, branch := d.start.Issue, d.start.Branch
		who := fmt.Sprintf("#%d %s", issue, safetext.Escape(branch))

		if r, ok := d.ceremonyRatio(); ok && d.ceremony() >= ceremonyAlertFloor && r > th.ceremonyRatio {
			lines := d.facts.Additions + d.facts.Deletions
			alerts = append(alerts, statsAlert{
				Kind: alertCeremony, Issue: &issue, Branch: &branch, Value: r, Threshold: th.ceremonyRatio,
				Message: fmt.Sprintf("ceremony %s: %.2f inbox messages and gate runs per 100 changed lines (%d over %d lines), over %g",
					who, r, d.ceremony(), lines, th.ceremonyRatio),
			})
		}

		if n, ok := d.sinceProgress(); ok && n > th.proofLoop {
			alerts = append(alerts, statsAlert{
				Kind: alertProofLoop, Issue: &issue, Branch: &branch, Value: float64(n), Threshold: float64(th.proofLoop),
				Message: fmt.Sprintf("proof loop %s: %d gate runs and reports since it said done, with no new commit on the branch, over %d",
					who, n, th.proofLoop),
			})
		}
	}

	s := summarise(dispatches)
	if s.EscapeRate != nil && s.EscapesChecked >= escapeAlertFloor && *s.EscapeRate > th.escapeRate {
		alerts = append(alerts, statsAlert{
			Kind: alertEscapeRate, Value: *s.EscapeRate, Threshold: th.escapeRate,
			Message: fmt.Sprintf("escape rate: %d of %d merged PRs (%.0f%%) reverted or blamed for a regression within %s, over %.0f%%",
				*s.Escapes, s.EscapesChecked, *s.EscapeRate*100, days(window), th.escapeRate*100),
		})
	}
	return alerts
}

// sinceProgress counts the gate runs and reports a dispatch has had since it
// last made progress, where progress is its first `done` or, after that, the
// newest commit on its branch. False when it has never said done, or when its
// branch cannot be read — a branch deleted after its merge — because "no new
// commits" is then a thing nobody can know.
//
// The branch is asked of git, not gh: a commit made and not yet pushed is
// still the worker moving, and the local ref is shared by every worktree of
// the repository, so it is current wherever the worker committed.
func (d *statsDispatch) sinceProgress() (int, bool) {
	var done time.Time
	for _, r := range d.reports {
		if r.Status != "done" {
			continue
		}
		if at, err := time.Parse(time.RFC3339Nano, r.At); err == nil {
			done = at
			break
		}
	}
	if done.IsZero() || d.start.Branch == "" {
		return 0, false
	}
	committed, ok := branchCommitTime(d.start.Repository, d.start.Branch)
	if !ok {
		return 0, false
	}
	progress := done
	if committed.After(progress) {
		progress = committed
	}

	n := 0
	for _, r := range d.reports {
		if at, err := time.Parse(time.RFC3339Nano, r.At); err == nil && at.After(progress) {
			n++
		}
	}
	for _, at := range d.gateAts {
		if at.After(progress) {
			n++
		}
	}
	return n, true
}

// days prints a whole number of days as "14d", and anything else as Go does.
func days(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	return d.String()
}
