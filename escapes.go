package main

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// An escape is a merged pull request that turned out not to be finished: it
// was reverted, or a later bug was blamed on it. It is the one figure in
// `stats` that looks past the merge, and the one that says whether the
// coordination counted elsewhere was buying anything — a factory that merges
// fast and reverts often is not fast.
//
// A merged pull request has escaped when, within the escape window after it
// merged (14 days unless --escape-window says otherwise), either:
//
//   - A later merged pull request reverts it. Any one of three marks, each the
//     one a standard tool leaves, is enough: `This reverts commit <sha>` in the
//     body where <sha> (7 to 40 hex digits) is a prefix of the merge commit —
//     what `git revert` writes; a title that is exactly `Revert "<its title>"`
//     — what GitHub's revert button and `git revert` both write; or a body line
//     that starts `Reverts #<n>` or `Reverts <owner>/<repo>#<n>` for this
//     repository — the body GitHub's revert button writes. The revert counts
//     at the time it merged.
//
//   - An issue opened after it merged, closed as completed by a pull request
//     other than it, names it as the cause in its title or body with one of
//     exactly these phrasings (case-insensitive, `#<n>` meaning this pull
//     request's number in the same repository):
//
//     regression|regressed [introduced|caused] from|by|in|since #<n>
//     caused|introduced by #<n>
//
//     (the bracketed word optional), so "Fixes regression from #12",
//     "Regressed in #12", "regression introduced by #12" and "caused by #12"
//     all count.
//
//     The issue counts at the time it was opened — when the escape was found,
//     not when it was fixed.
//
// The second rule is deliberately narrow. "Fixes #12" is a pull request
// closing an issue, not an issue blaming a pull request, and "regression test
// for #12" is somebody doing their job; an escape rate that counted either
// would alarm on diligence. A blamed issue that phrases it some other way is
// missed, and that is the cheaper error: an escape figure that cries wolf gets
// ignored, one that undercounts still trends.
//
// gh is asked twice per repository per run, whatever the number of pull
// requests: one search for merged pull requests mentioning a revert, one for
// closed issues using the regression vocabulary, both bounded below by the
// earliest merge being judged. GitHub's search is only a coarse prefilter that
// keeps the answer small; the rules above, applied here, decide. A search that
// fails leaves that repository's escapes unknown (null), never zero.

const defaultEscapeWindow = 14 * 24 * time.Hour

// escapeSearchLimit bounds each search's answer. A repository that produces
// more revert-mentioning pull requests than this inside one stats window has
// a problem the rate will already be showing; reaching the limit is said on
// stderr, because the oldest candidates are the ones gh drops.
const escapeSearchLimit = 200

// escapeFinding is the first thing found that made a pull request an escape.
type escapeFinding struct {
	Kind string `json:"kind"`
	By   int    `json:"by"`
	At   string `json:"at"`
}

const (
	escapeRevert     = "revert"
	escapeRegression = "regression"
)

type ghRevertCandidate struct {
	Number   int    `json:"number"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	MergedAt string `json:"mergedAt"`
}

type ghRegressionCandidate struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	CreatedAt   string `json:"createdAt"`
	StateReason string `json:"stateReason"`
	ClosedBy    []struct {
		Number int `json:"number"`
	} `json:"closedByPullRequestsReferences"`
}

var (
	revertCommitRe = regexp.MustCompile(`(?i)\bthis reverts commit ([0-9a-f]{7,40})\b`)
	revertsRefRe   = regexp.MustCompile(`(?im)^[ \t]*reverts[ \t]+([\w.-]+/[\w.-]+)?#(\d+)\b`)
	regressionRe   = regexp.MustCompile(`(?i)\bregress(?:ion|ed)[ \t]+(?:(?:introduced|caused)[ \t]+)?(?:from|by|in|since)[ \t]+#(\d+)\b`)
	causedByRe     = regexp.MustCompile(`(?i)\b(?:caused|introduced)[ \t]+by[ \t]+#(\d+)\b`)
)

// findEscapes decides, for every merged dispatch whose merge time gh gave, whether
// it escaped. Dispatches are grouped by the repository gh is asked about, so the
// searches run once per repository rather than once per pull request.
func findEscapes(dispatches []*statsDispatch, window time.Duration) {
	type group struct {
		start    ledgerEvent
		members  []*statsDispatch
		earliest time.Time
	}
	groups := map[string]*group{}
	var order []string
	for _, d := range dispatches {
		at, ok := d.mergedAt()
		if !ok {
			continue
		}
		key := d.start.Origin
		if key == "" {
			key = "dir:" + d.start.Repository
		}
		g := groups[key]
		if g == nil {
			g = &group{start: d.start, earliest: at}
			groups[key] = g
			order = append(order, key)
		}
		if at.Before(g.earliest) {
			g.earliest = at
		}
		g.members = append(g.members, d)
	}

	for _, key := range order {
		g := groups[key]
		since := g.earliest.UTC().Format("2006-01-02")
		limit := strconv.Itoa(escapeSearchLimit)

		var reverts []ghRevertCandidate
		err := ghJSON(g.start, []string{"pr", "list", "--state", "merged",
			"--search", "revert merged:>=" + since,
			"--json", "number,title,body,mergedAt", "--limit", limit}, &reverts)
		var issues []ghRegressionCandidate
		if err == nil {
			err = ghJSON(g.start, []string{"issue", "list", "--state", "closed",
				"--search", `regression OR regressed OR "caused by" OR "introduced by" created:>=` + since,
				"--json", "number,title,body,createdAt,stateReason,closedByPullRequestsReferences", "--limit", limit}, &issues)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "worktender: escapes not checked for %s: %v\n", groupName(g.start), err)
			continue
		}
		if len(reverts) == escapeSearchLimit || len(issues) == escapeSearchLimit {
			fmt.Fprintf(os.Stderr, "worktender: an escape search for %s hit its limit of %d; the oldest candidates may be missing\n", groupName(g.start), escapeSearchLimit)
		}

		slug := originSlug(g.start.Origin)
		for _, d := range g.members {
			d.escapeKnown = true
			d.escape = matchEscape(d, reverts, issues, window, slug)
		}
	}
}

func groupName(start ledgerEvent) string {
	if start.Origin != "" {
		return start.Origin
	}
	return start.Repository
}

// matchEscape is the earliest finding inside the window, or nil.
func matchEscape(d *statsDispatch, reverts []ghRevertCandidate, issues []ghRegressionCandidate, window time.Duration, slug string) *escapeFinding {
	merged, _ := d.mergedAt()
	inWindow := func(stamp string) (time.Time, bool) {
		at, err := time.Parse(time.RFC3339, stamp)
		if err != nil || !at.After(merged) || at.After(merged.Add(window)) {
			return time.Time{}, false
		}
		return at, true
	}

	var best *escapeFinding
	var bestAt time.Time
	consider := func(kind string, by int, at time.Time, stamp string) {
		if best == nil || at.Before(bestAt) {
			best, bestAt = &escapeFinding{Kind: kind, By: by, At: stamp}, at
		}
	}
	for _, c := range reverts {
		if c.Number == d.pr || !revertsPR(c, d.pr, d.facts, slug) {
			continue
		}
		if at, ok := inWindow(c.MergedAt); ok {
			consider(escapeRevert, c.Number, at, c.MergedAt)
		}
	}
	for _, c := range issues {
		if !blames(c, d.pr) {
			continue
		}
		if at, ok := inWindow(c.CreatedAt); ok {
			consider(escapeRegression, c.Number, at, c.CreatedAt)
		}
	}
	return best
}

// revertsPR is whether candidate c reverts pull request pr, by any of the three
// marks in the note at the top of this file.
func revertsPR(c ghRevertCandidate, pr int, facts *prFacts, slug string) bool {
	if facts.Title != "" && c.Title == `Revert "`+facts.Title+`"` {
		return true
	}
	if oid := strings.ToLower(facts.mergeOID()); oid != "" {
		for _, m := range revertCommitRe.FindAllStringSubmatch(c.Body, -1) {
			if strings.HasPrefix(oid, strings.ToLower(m[1])) {
				return true
			}
		}
	}
	for _, m := range revertsRefRe.FindAllStringSubmatch(c.Body, -1) {
		if m[2] != strconv.Itoa(pr) {
			continue
		}
		// A qualified reference counts only when it names this repository;
		// one this binary cannot place is not taken on trust.
		if m[1] == "" || (slug != "" && strings.EqualFold(m[1], slug)) {
			return true
		}
	}
	return false
}

// blames is whether issue c names pull request pr as its cause, closed as
// completed by some other pull request.
func blames(c ghRegressionCandidate, pr int) bool {
	if c.StateReason != "COMPLETED" {
		return false
	}
	closedByOther := false
	for _, ref := range c.ClosedBy {
		if ref.Number != pr {
			closedByOther = true
		}
	}
	if !closedByOther {
		return false
	}
	text := c.Title + "\n" + c.Body
	want := strconv.Itoa(pr)
	for _, re := range []*regexp.Regexp{regressionRe, causedByRe} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			if m[1] == want {
				return true
			}
		}
	}
	return false
}

// originSlug is the owner/repo a remote URL names, or empty when it is not a
// shape this recognises: git@host:owner/repo(.git), or a URL ending in
// /owner/repo(.git).
func originSlug(origin string) string {
	s := strings.TrimSuffix(strings.TrimSuffix(origin, "/"), ".git")
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	} else if i := strings.Index(s, ":"); i >= 0 {
		s = "host/" + s[i+1:]
	}
	parts := strings.Split(s, "/")
	if len(parts) < 3 || parts[len(parts)-2] == "" || parts[len(parts)-1] == "" {
		return ""
	}
	return parts[len(parts)-2] + "/" + parts[len(parts)-1]
}
