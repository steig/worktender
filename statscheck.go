package main

import (
	"fmt"
	"math"
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
	// within the escape window, and unlike the others it is a gate rather
	// than a smoke alarm: it fires until the 95% interval's upper bound is
	// under it, so a fleet has to show it is safe, not merely look it. At
	// 0.05 that takes 73 escape-free merges. Escapes are found by
	// revert and blame phrasing, so the true rate is no lower than measured.
	escapeRate float64
}

var defaultThresholds = statsThresholds{ceremonyRatio: 20, proofLoop: 3, escapeRate: 0.05}

// ceremonyAlertFloor is the least ceremony a dispatch must have before its
// ratio can alert. Per hundred lines, a one-line fix gated once is 100: the
// arithmetic of a small denominator, not bureaucracy. Five events is past a
// gate and its reply.
const ceremonyAlertFloor = 5

// escapeAlertFloor is the fewest merged pull requests the escape rate must be
// over before it can alert. Under it the interval is near the whole range, so
// the summary says there is not enough data instead.
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
	if s.Escapes != nil && s.EscapesChecked >= escapeAlertFloor {
		if _, hi := wilson(*s.Escapes, s.EscapesChecked); hi > th.escapeRate {
			// A fleet that is clean so far has not failed anything, only not
			// passed yet; the tail keeps that from reading as an alarm.
			gate := ""
			if k, p := *s.Escapes, float64(*s.Escapes)/float64(s.EscapesChecked); k == 0 || p <= th.escapeRate {
				seen := "no escapes seen"
				if k > 0 {
					seen = fmt.Sprintf("the measured rate is within %.4g%%", th.escapeRate*100)
				}
				gate = fmt.Sprintf("; %s, but not yet enough merges to show the rate is under %.4g%%", seen, th.escapeRate*100)
			}
			// Three places, not two: 0/72's 0.0507 must not read as 0.05
			// against a threshold of 0.05.
			alerts = append(alerts, statsAlert{
				Kind: alertEscapeRate, Value: math.Round(hi*1000) / 1000, Threshold: th.escapeRate,
				Message: fmt.Sprintf("escape rate %s of merged PRs reverted or blamed for a regression within %s: the 95%% interval reaches past %.4g%%%s",
					escapeFigure(*s.Escapes, s.EscapesChecked), days(window), th.escapeRate*100, gate),
			})
		}
	}
	return alerts
}

// wilson is the 95% Wilson score interval for k successes in n trials. It
// stays inside [0, 1] and is not zero-width at k = 0, which is the case that
// matters here: no escapes in 13 merges is 0–23%, not 0%.
func wilson(k, n int) (lo, hi float64) {
	const z = 1.96
	p, nf := float64(k)/float64(n), float64(n)
	d := 1 + z*z/nf
	centre := (p + z*z/(2*nf)) / d
	half := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf)) / d
	return math.Max(0, centre-half), math.Min(1, centre+half)
}

// escapeFigure prints k escapes of n with its interval, as "0/13 (0–23%)".
func escapeFigure(k, n int) string {
	lo, hi := wilson(k, n)
	return fmt.Sprintf("%d/%d (%.0f–%.0f%%)", k, n, lo*100, hi*100)
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
