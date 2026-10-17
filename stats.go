package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/steig/worktender/internal/jsonout"
	"github.com/steig/worktender/internal/safetext"
)

// stats turns the dispatch ledger into the handful of figures that say whether
// the factory is worth what it costs: how long a merged pull request took from
// start to done, how many tries it needed, how often it needed more than one,
// and how much coordination it took against how much it changed.
//
// Everything here is derived at read time. The ledger holds what happened and
// nothing computed from it, so a figure defined badly today can be redefined
// tomorrow over the same history.
//
// The one thing it asks anyone else is gh: whether each pull request merged,
// and how big its diff was. That is not worktender's to observe — a merge
// happens on GitHub, long after every pane involved has closed.

const statsUsage = "usage: worktender stats [--json] [--since DURATION] [--escape-window DURATION] [--check [--max-ceremony-ratio N] [--max-proof-loop N] [--max-escape-rate F]]"

// statsDispatch is one `start`, and everything later attributed to it.
type statsDispatch struct {
	start   ledgerEvent
	reports []ledgerEvent
	gates   int
	gateAts []time.Time
	inbox   int
	// feedback is the pr-feedback notices typed at its worker, and feedbackAts
	// when each was typed: `lessons propose` needs to know whether a round came
	// before the worker's last done, which a count cannot say.
	feedback    int
	feedbackAts []time.Time

	pr     int
	facts  *prFacts
	ghErr  error
	doneAt time.Time

	// escape is nil both for "did not escape" and "could not tell";
	// escapeKnown is what separates them.
	escape      *escapeFinding
	escapeKnown bool

	// sessions is every agent session its reports named, in the order first
	// seen: a worker restarted mid-dispatch spent tokens in both. tokens is
	// nil when none of their transcripts could be found.
	sessions []string
	tokens   []modelTokens
}

// attempts is how many times the worker claimed it was finished.
//
// A `done` report is the one completion claim worktender sees from the worker
// itself, whoever was or was not gating on it, so a second one means the first
// did not stick: review sent it back, a check failed, a coordinator re-briefed
// it. Gate runs are not counted here — a coordinator gating for `planned` and
// then for `done` has made two waits and one attempt — and they are counted as
// ceremony instead.
//
// A pr-feedback round is a retry too, one the worker was sent on rather than
// claimed, so a dispatch with N rounds made at least N+1 attempts. It is a
// floor rather than an addition: a worker that fixes what a round named
// usually reports done again, and that is the same retry counted once.
func (d *statsDispatch) attempts() int {
	n := 0
	for _, r := range d.reports {
		if r.Status == "done" {
			n++
		}
	}
	if d.feedback > 0 && d.feedback+1 > n {
		n = d.feedback + 1
	}
	return n
}

// merged is a tri-state: nil when there is no pull request, or gh could not say.
func (d *statsDispatch) merged() *bool {
	if d.facts == nil {
		return nil
	}
	m := d.facts.State == "MERGED"
	return &m
}

// mergedAt is when the pull request merged, and false when it has not or gh
// did not say. An escape is measured from it.
func (d *statsDispatch) mergedAt() (time.Time, bool) {
	if m := d.merged(); m == nil || !*m {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, d.facts.MergedAt)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// wallClock is start to the LAST done report: a dispatch that had to go round
// again was not finished the first time it said so.
func (d *statsDispatch) wallClock() (time.Duration, bool) {
	if d.doneAt.IsZero() {
		return 0, false
	}
	started, err := time.Parse(time.RFC3339Nano, d.start.At)
	if err != nil {
		return 0, false
	}
	return d.doneAt.Sub(started), true
}

// ceremony is the coordination this dispatch took: inbox messages on its
// issue's thread, and gate runs on its worker.
func (d *statsDispatch) ceremony() int { return d.inbox + d.gates }

// ceremonyRatio is ceremony per hundred changed lines. Per hundred so a typical
// figure reads as a small number rather than a long decimal; absent when there
// is no diff to divide by, because a ratio over zero lines is not small or
// large, it is undefined.
func (d *statsDispatch) ceremonyRatio() (float64, bool) {
	if d.facts == nil {
		return 0, false
	}
	lines := d.facts.Additions + d.facts.Deletions
	if lines == 0 {
		return 0, false
	}
	return round2(float64(d.ceremony()) * 100 / float64(lines)), true
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// prFacts is what gh says about one pull request.
//
// The title, merge time and merge commit ride along on the same call for
// escape detection: a revert names the commit or the title, and the window is
// measured from the merge.
type prFacts struct {
	State       string `json:"state"`
	Additions   int    `json:"additions"`
	Deletions   int    `json:"deletions"`
	Title       string `json:"title"`
	MergedAt    string `json:"mergedAt"`
	MergeCommit *struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
}

func (f *prFacts) mergeOID() string {
	if f.MergeCommit == nil {
		return ""
	}
	return f.MergeCommit.OID
}

// prLookup asks about one pull request. A var so a test can count the calls.
var prLookup = ghPRFacts

// ghPRFacts asks gh about one pull request, in the repository the dispatch
// recorded. The origin recorded at start is preferred over the checkout's
// current one, which may no longer exist.
func ghPRFacts(start ledgerEvent, pr int) (*prFacts, error) {
	var facts prFacts
	if err := ghJSON(start, []string{"pr", "view", strconv.Itoa(pr), "--json", "state,additions,deletions,title,mergedAt,mergeCommit"}, &facts); err != nil {
		return nil, err
	}
	return &facts, nil
}

// ghJSON runs one gh query about the dispatch's repository and decodes the
// answer into v. Errors name the gh subcommand, and carry what gh said on
// stderr when it said anything.
func ghJSON(start ledgerEvent, args []string, v any) error {
	what := "gh " + strings.Join(args[:min(3, len(args))], " ")
	if start.Origin != "" {
		args = append(args, "--repo", start.Origin)
	}
	cmd := exec.Command("gh", args...)
	if info, err := os.Stat(start.Repository); err == nil && info.IsDir() {
		cmd.Dir = start.Repository
	}
	raw, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return fmt.Errorf("%s: %s", what, strings.TrimSpace(string(exit.Stderr)))
		}
		return fmt.Errorf("%s: %w", what, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%s: unreadable answer: %w", what, err)
	}
	return nil
}

func statsCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := jsonFlag(fs)
	since := fs.Duration("since", 0, "only dispatches started within this long ago (default all; 720h under --check)")
	window := fs.Duration("escape-window", defaultEscapeWindow, "how long after its merge a revert or blamed issue makes a pull request an escape")
	check := fs.Bool("check", false, "print one line per alert and exit 3 when any threshold is crossed")
	th := defaultThresholds
	fs.Float64Var(&th.ceremonyRatio, "max-ceremony-ratio", th.ceremonyRatio, "alert on a dispatch with more ceremony than this per 100 changed lines")
	fs.IntVar(&th.proofLoop, "max-proof-loop", th.proofLoop, "alert on a dispatch with more gate runs and reports than this since done with no new commit")
	fs.Float64Var(&th.escapeRate, "max-escape-rate", th.escapeRate, "alert when the upper bound of the 95% interval on the fraction of merged pull requests that escaped is over this")
	if err := fs.Parse(args); err != nil {
		return usagef("%v; %s", err, statsUsage)
	}
	if fs.NArg() > 0 {
		return usagef("unexpected argument %q; %s", fs.Arg(0), statsUsage)
	}
	if *since < 0 || *window <= 0 || th.ceremonyRatio < 0 || th.proofLoop < 0 || th.escapeRate < 0 {
		return usagef("--since and the thresholds must not be negative, and --escape-window must be positive; %s", statsUsage)
	}
	// A check run from cron every day over the whole unbounded ledger would
	// re-raise a month-old dispatch forever, and the one alert nobody can clear
	// teaches everyone to ignore the exit code. So --check looks back a month
	// unless told otherwise; the report, which is read rather than acted on,
	// keeps all of history.
	sinceSet := false
	fs.Visit(func(f *flag.Flag) { sinceSet = sinceSet || f.Name == "since" })
	if *check && !sinceSet {
		*since = defaultCheckSince
	}

	path, err := ledgerPath()
	if err != nil {
		return err
	}
	events, skipped, err := readLedger(path)
	if err != nil {
		return err
	}
	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "worktender: %d line(s) of %s could not be read and were left out\n", skipped, path)
	}

	dispatches := attribute(events)
	countInbox(dispatches, filepath.Join(filepath.Dir(path), "inbox"))
	// Filtered after the inbox is counted, not before: an issue's inbox window
	// ends where its next dispatch starts, and that bound must not depend on
	// which dispatches happen to be shown.
	dispatches = startedWithin(dispatches, *since)
	lookupPRs(dispatches)
	countTokens(dispatches)
	findEscapes(dispatches, *window)

	var alerts []statsAlert
	if *check {
		alerts = checkStats(dispatches, th, *window)
	}
	if *asJSON {
		if err := jsonout.Write(out, statsDocument(path, skipped, dispatches, *window, alerts)); err != nil {
			return err
		}
	} else if *check {
		for _, a := range alerts {
			fmt.Fprintln(out, "alert: "+a.Message)
		}
	} else {
		renderStats(out, dispatches, *window)
	}
	if len(alerts) > 0 {
		return codef(exitNeedsHuman, "%d stats alert(s) over threshold", len(alerts))
	}
	return nil
}

// startedWithin keeps the dispatches started no longer than since ago; zero
// keeps every one.
func startedWithin(dispatches []*statsDispatch, since time.Duration) []*statsDispatch {
	if since == 0 {
		return dispatches
	}
	cutoff := ledgerNow().Add(-since)
	var kept []*statsDispatch
	for _, d := range dispatches {
		if at, err := time.Parse(time.RFC3339Nano, d.start.At); err == nil && !at.Before(cutoff) {
			kept = append(kept, d)
		}
	}
	return kept
}

// attribute walks the ledger in append order and hands each event to the
// dispatch it belongs to. Order is what makes "belongs to" well defined: an
// event goes to the latest start that came before it, so a branch started a
// second time begins a second dispatch rather than inheriting the first's
// reports.
//
// Events that match no start are dropped. A worker staffed by `dispatch` or
// `sync` rather than `start` has no issue, and so nothing these figures are
// per.
func attribute(events []ledgerEvent) []*statsDispatch {
	var dispatches []*statsDispatch
	byBranch := map[string]*statsDispatch{}
	branchKey := func(repo, branch string) string { return repo + "\x00" + branch }

	for _, e := range events {
		switch e.Event {
		case ledgerStart:
			d := &statsDispatch{start: e}
			dispatches = append(dispatches, d)
			byBranch[branchKey(e.Repository, e.Branch)] = d

		case ledgerReport:
			d := byBranch[branchKey(e.Repository, e.Branch)]
			if d == nil || e.Branch == "" {
				continue
			}
			d.reports = append(d.reports, e)
			if e.Session != "" && !slices.Contains(d.sessions, e.Session) {
				d.sessions = append(d.sessions, e.Session)
			}
			if e.PR > 0 {
				d.pr = e.PR
			}
			if e.Status == "done" {
				if at, err := time.Parse(time.RFC3339Nano, e.At); err == nil {
					d.doneAt = at
				}
			}

		case ledgerFeedback:
			if d := byBranch[branchKey(e.Repository, e.Branch)]; d != nil && e.Branch != "" {
				d.feedback++
				if at, err := time.Parse(time.RFC3339Nano, e.At); err == nil {
					d.feedbackAts = append(d.feedbackAts, at)
				}
			}

		case ledgerGate:
			// A gate names its workers by agent name or by pane, so it is
			// matched on either — to the latest dispatch holding that name, and
			// counted once per dispatch however many of its targets matched.
			counted := map[*statsDispatch]bool{}
			for _, t := range e.Targets {
				if d := latestFor(dispatches, t); d != nil && !counted[d] {
					counted[d] = true
					d.gates++
					if at, err := time.Parse(time.RFC3339Nano, e.At); err == nil {
						d.gateAts = append(d.gateAts, at)
					}
				}
			}
		}
	}
	return dispatches
}

func latestFor(dispatches []*statsDispatch, t ledgerTarget) *statsDispatch {
	for i := len(dispatches) - 1; i >= 0; i-- {
		s := dispatches[i].start
		if (s.AgentName != "" && (s.AgentName == t.Name || s.AgentName == t.PaneID)) ||
			(s.PaneID != "" && s.PaneID == t.PaneID) {
			return dispatches[i]
		}
	}
	return nil
}

// countInbox counts the messages on each dispatch's issue thread.
//
// The thread is the issue number, the inbox's own documented convention; a
// thread named anything else is not counted, because nothing says which issue
// it was about. A message counts toward a dispatch when it was posted at or
// after that dispatch started and before the next dispatch of the same issue
// did, so an issue started twice does not count its inbox twice.
//
// Thread ids are fleet-wide rather than per repository, so two repositories
// with an issue of the same number share a thread. That is the inbox's model,
// and it is inherited here rather than second-guessed.
func countInbox(dispatches []*statsDispatch, dir string) {
	for i, d := range dispatches {
		messages, err := readThreadMessages(dir, strconv.Itoa(d.start.Issue))
		if err != nil {
			fmt.Fprintf(os.Stderr, "worktender: %v; its messages are not counted\n", err)
			continue
		}
		from, _ := time.Parse(time.RFC3339Nano, d.start.At)
		var until time.Time
		for _, later := range dispatches[i+1:] {
			if later.start.Issue == d.start.Issue {
				until, _ = time.Parse(time.RFC3339Nano, later.start.At)
				break
			}
		}
		for _, m := range messages {
			at, err := time.Parse(time.RFC3339, m.At)
			if err != nil {
				continue
			}
			// The inbox stamps whole seconds and the ledger does not, so the
			// start is truncated to match before comparing.
			if at.Before(from.Truncate(time.Second)) || (!until.IsZero() && !at.Before(until.Truncate(time.Second))) {
				continue
			}
			d.inbox++
		}
	}
}

// lookupPRs asks gh once per distinct pull request.
func lookupPRs(dispatches []*statsDispatch) {
	type key struct {
		origin, repo string
		pr           int
	}
	type answer struct {
		facts *prFacts
		err   error
	}
	seen := map[key]answer{}
	for _, d := range dispatches {
		if d.pr == 0 {
			continue
		}
		k := key{d.start.Origin, d.start.Repository, d.pr}
		a, ok := seen[k]
		if !ok {
			a.facts, a.err = prLookup(d.start, d.pr)
			seen[k] = a
		}
		d.facts, d.ghErr = a.facts, a.err
	}
}

// statsSummary is the figures across every dispatch. Each per-merged-PR figure
// is null when nothing has merged, because an average over nothing is not
// zero.
type statsSummary struct {
	Dispatches          int      `json:"dispatches"`
	WithPR              int      `json:"with_pr"`
	Merged              int      `json:"merged"`
	MedianWallClock     *int64   `json:"median_wall_clock_seconds"`
	AttemptsPerMergedPR *float64 `json:"attempts_per_merged_pr"`
	ReworkRate          *float64 `json:"rework_rate"`
	CeremonyPerMergedPR *float64 `json:"ceremony_per_merged_pr"`

	// EscapesChecked is the merged dispatches gh could be asked about; the rate
	// is over those, so a repository gh cannot search shrinks the denominator
	// rather than passing as escape-free.
	EscapesChecked int      `json:"escapes_checked"`
	Escapes        *int     `json:"escapes"`
	EscapeRate     *float64 `json:"escape_rate"`

	// TokensCounted is the merged dispatches whose transcripts were found; the
	// totals and the median are over those alone.
	TokensCounted           int           `json:"tokens_counted"`
	TokensByModel           []modelTokens `json:"tokens_by_model"`
	MedianTokensPerMergedPR *tokenCounts  `json:"median_tokens_per_merged_pr"`
}

func summarise(dispatches []*statsDispatch) statsSummary {
	s := statsSummary{Dispatches: len(dispatches)}
	var walls []time.Duration
	attempts, reworked, ceremony, escaped := 0, 0, 0, 0
	for _, d := range dispatches {
		if d.pr > 0 {
			s.WithPR++
		}
		if m := d.merged(); m == nil || !*m {
			continue
		}
		s.Merged++
		attempts += d.attempts()
		ceremony += d.ceremony()
		if d.attempts() > 1 {
			reworked++
		}
		if w, ok := d.wallClock(); ok {
			walls = append(walls, w)
		}
		if d.escapeKnown {
			s.EscapesChecked++
			if d.escape != nil {
				escaped++
			}
		}
	}
	if s.EscapesChecked > 0 {
		r := round2(float64(escaped) / float64(s.EscapesChecked))
		s.Escapes, s.EscapeRate = &escaped, &r
	}
	if s.Merged > 0 {
		a := round2(float64(attempts) / float64(s.Merged))
		r := round2(float64(reworked) / float64(s.Merged))
		c := round2(float64(ceremony) / float64(s.Merged))
		s.AttemptsPerMergedPR, s.ReworkRate, s.CeremonyPerMergedPR = &a, &r, &c
	}
	if len(walls) > 0 {
		sort.Slice(walls, func(i, j int) bool { return walls[i] < walls[j] })
		m := walls[len(walls)/2]
		if len(walls)%2 == 0 {
			m = (walls[len(walls)/2-1] + walls[len(walls)/2]) / 2
		}
		secs := int64(m.Round(time.Second) / time.Second)
		s.MedianWallClock = &secs
	}
	s.TokensByModel, s.MedianTokensPerMergedPR, s.TokensCounted = summariseTokens(dispatches)
	return s
}

// renderStats prints one row per dispatch, then the summary.
func renderStats(out io.Writer, dispatches []*statsDispatch, window time.Duration) {
	if len(dispatches) == 0 {
		fmt.Fprintln(out, "no dispatches recorded in range; `worktender start` records one")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ISSUE\tBRANCH\tROLE\tPR\tMERGED\tWALL\tATTEMPTS\tINBOX\tGATES\tDIFF\tCEREMONY/100L\tESCAPED")
	unanswered := 0
	for _, d := range dispatches {
		if d.ghErr != nil {
			unanswered++
		}
		pr, merged, wall, diff, ratio := missing, missing, missing, missing, missing
		if d.pr > 0 {
			pr = strconv.Itoa(d.pr)
			merged = "?"
		}
		if m := d.merged(); m != nil {
			merged = "no"
			if *m {
				merged = "yes"
			}
			diff = strconv.Itoa(d.facts.Additions + d.facts.Deletions)
		}
		if w, ok := d.wallClock(); ok {
			wall = w.Round(time.Second).String()
		}
		if r, ok := d.ceremonyRatio(); ok {
			ratio = strconv.FormatFloat(r, 'f', 2, 64)
		}
		role := d.start.Role
		if role == "" {
			role = missing
		}
		escaped := missing
		if d.escapeKnown {
			escaped = "no"
			if e := d.escape; e != nil {
				escaped = fmt.Sprintf("%s #%d", e.Kind, e.By)
			}
		}
		fmt.Fprintf(tw, "#%d\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s\n",
			d.start.Issue, safetext.Escape(d.start.Branch), safetext.Escape(role), pr, merged, wall,
			d.attempts(), d.inbox, d.gates, diff, ratio, escaped)
	}
	tw.Flush()

	s := summarise(dispatches)
	fmt.Fprintf(out, "\n%d dispatches, %d with a pull request, %d merged\n", s.Dispatches, s.WithPR, s.Merged)
	if s.Merged > 0 {
		wall := missing
		if s.MedianWallClock != nil {
			wall = (time.Duration(*s.MedianWallClock) * time.Second).String()
		}
		fmt.Fprintf(out, "per merged PR: median wall clock %s, %.2f attempts, rework rate %.0f%%, %.2f ceremony\n",
			wall, *s.AttemptsPerMergedPR, *s.ReworkRate*100, *s.CeremonyPerMergedPR)
	}
	if m := s.MedianTokensPerMergedPR; m != nil {
		fmt.Fprintf(out, "tokens per merged PR, median over the %d of %d with a transcript: %d in, %d out, %d cache read, %d cache write\n",
			s.TokensCounted, s.Merged, m.Input, m.Output, m.CacheRead, m.CacheWrite)
		for _, t := range s.TokensByModel {
			fmt.Fprintf(out, "  %s: %d in, %d out, %d cache read, %d cache write\n",
				safetext.Escape(t.Model), t.Input, t.Output, t.CacheRead, t.CacheWrite)
		}
	}
	if s.Escapes != nil {
		enough := ""
		if s.EscapesChecked < escapeAlertFloor {
			enough = fmt.Sprintf("; under %d, not enough data to judge yet", escapeAlertFloor)
		}
		fmt.Fprintf(out, "escape rate %s of merged PRs reverted or blamed for a regression within %s of merging%s\n",
			escapeFigure(*s.Escapes, s.EscapesChecked), days(window), enough)
	}
	if unanswered > 0 {
		fmt.Fprintf(out, "gh could not answer for %d pull request(s), so MERGED reads ? there; `worktender stats --json` carries why\n", unanswered)
	}
}

// statsJSON is `stats --json`.
type statsJSON struct {
	Ledger       string `json:"ledger"`
	SkippedLines int    `json:"skipped_lines"`
	// EscapeWindow is the window the escape figures were judged over, so a
	// reader comparing two runs can tell whether they asked the same question.
	EscapeWindow int64               `json:"escape_window_seconds"`
	Dispatches   []statsDispatchJSON `json:"dispatches"`
	Summary      statsSummary        `json:"summary"`
	// Alerts is null unless --check asked for them, and an empty list when it
	// did and nothing crossed a threshold: "not checked" and "checked, quiet"
	// are different answers.
	Alerts []statsAlert `json:"alerts"`
}

type statsDispatchJSON struct {
	Issue         int               `json:"issue"`
	Repository    string            `json:"repository"`
	Branch        string            `json:"branch"`
	Role          *string           `json:"role"`
	Agent         *string           `json:"agent"`
	AgentKind     string            `json:"agent_kind"`
	AgentName     string            `json:"agent_name"`
	PaneID        string            `json:"pane_id"`
	StartedAt     string            `json:"started_at"`
	Reports       []statsReportJSON `json:"reports"`
	Attempts      int               `json:"attempts"`
	Rounds        int               `json:"feedback_rounds"`
	PR            *int              `json:"pr"`
	Merged        *bool             `json:"merged"`
	Additions     *int              `json:"additions"`
	Deletions     *int              `json:"deletions"`
	WallClock     *int64            `json:"wall_clock_seconds"`
	InboxMessages int               `json:"inbox_messages"`
	GateRuns      int               `json:"gate_runs"`
	CeremonyRatio *float64          `json:"ceremony_ratio"`
	MergedAt      *string           `json:"merged_at"`
	Escaped       *bool             `json:"escaped"`
	Escape        *escapeFinding    `json:"escape"`
	Tokens        []modelTokens     `json:"tokens"`
	Error         *string           `json:"error"`
}

type statsReportJSON struct {
	Status string `json:"status"`
	At     string `json:"at"`
	PR     *int   `json:"pr"`
}

func statsDocument(path string, skipped int, dispatches []*statsDispatch, window time.Duration, alerts []statsAlert) statsJSON {
	doc := statsJSON{
		Ledger: path, SkippedLines: skipped, EscapeWindow: int64(window / time.Second),
		Dispatches: []statsDispatchJSON{}, Summary: summarise(dispatches), Alerts: alerts,
	}
	for _, d := range dispatches {
		j := statsDispatchJSON{
			Issue: d.start.Issue, Repository: d.start.Repository, Branch: d.start.Branch,
			AgentKind: d.start.AgentKind, AgentName: d.start.AgentName, PaneID: d.start.PaneID,
			StartedAt: d.start.At, Reports: []statsReportJSON{}, Attempts: d.attempts(), Rounds: d.feedback,
			Merged: d.merged(), InboxMessages: d.inbox, GateRuns: d.gates, Tokens: d.tokens,
		}
		if d.start.Role != "" {
			j.Role = jsonout.String(d.start.Role)
		}
		if d.start.Agent != "" {
			j.Agent = jsonout.String(d.start.Agent)
		}
		for _, r := range d.reports {
			rj := statsReportJSON{Status: r.Status, At: r.At}
			if r.PR > 0 {
				pr := r.PR
				rj.PR = &pr
			}
			j.Reports = append(j.Reports, rj)
		}
		if d.pr > 0 {
			pr := d.pr
			j.PR = &pr
		}
		if d.facts != nil {
			add, del := d.facts.Additions, d.facts.Deletions
			j.Additions, j.Deletions = &add, &del
		}
		if w, ok := d.wallClock(); ok {
			secs := int64(w.Round(time.Second) / time.Second)
			j.WallClock = &secs
		}
		if r, ok := d.ceremonyRatio(); ok {
			j.CeremonyRatio = &r
		}
		if _, ok := d.mergedAt(); ok {
			j.MergedAt = jsonout.String(d.facts.MergedAt)
		}
		if d.escapeKnown {
			escaped := d.escape != nil
			j.Escaped, j.Escape = &escaped, d.escape
		}
		if d.ghErr != nil {
			j.Error = jsonout.String(d.ghErr.Error())
		}
		doc.Dispatches = append(doc.Dispatches, j)
	}
	return doc
}
