package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/herdrtest"
)

// escapesFixture lays down five merged dispatches in one repository and a gh
// that answers the per-PR views and the two searches from files:
//
//   - #50, PR 10, merged 10-01: PR 20 says "This reverts commit <its merge
//     commit, abbreviated>" and merged 10-03 — an escape.
//   - #51, PR 11, merged 09-01: PR 21 is titled `Revert "<its title>"` but
//     merged 10-05, 34 days on — outside the window, so not an escape.
//   - #52, PR 12, merged 10-02: issue 30, opened 10-04 and closed as completed
//     by PR 31, says "Fixes regression from #12" — an escape.
//   - #53, PR 13, merged 10-02: issue 32 mentions "regression test for #13",
//     and issue 33 says "caused by #13" but was closed as not planned —
//     neither is an escape.
//   - #54, PR 14, merged 10-02: PR 22's body is GitHub's revert-button
//     "Reverts o/r#14" — an escape.
//
// It returns the file gh appends each argv to.
func escapesFixture(t *testing.T) string {
	t.Helper()
	path := withLedger(t)
	var lines []string
	for _, d := range []struct{ issue, pr string }{{"50", "10"}, {"51", "11"}, {"52", "12"}, {"53", "13"}, {"54", "14"}} {
		lines = append(lines,
			`{"v":1,"event":"start","at":"2026-08-01T10:00:00Z","repository":"/r","branch":"`+d.issue+`-x","issue":`+d.issue+`,"origin":"git@github.com:o/r.git","agent_kind":"claude","agent_name":"wt-`+d.issue+`"}`,
			`{"v":1,"event":"report","at":"2026-08-01T11:00:00Z","repository":"/r","branch":"`+d.issue+`-x","status":"done","pr":`+d.pr+`}`)
	}
	herdrtest.WriteFile(t, path, strings.Join(lines, "\n")+"\n")

	views := map[string]string{
		"10": `{"state":"MERGED","additions":10,"deletions":0,"title":"feat: a thing","mergedAt":"2026-10-01T12:00:00Z","mergeCommit":{"oid":"abc1234def5678abc1234def5678abc1234def56"}}`,
		"11": `{"state":"MERGED","additions":10,"deletions":0,"title":"fix: other","mergedAt":"2026-09-01T12:00:00Z","mergeCommit":{"oid":"1111111111111111111111111111111111111111"}}`,
		"12": `{"state":"MERGED","additions":10,"deletions":0,"title":"feat: twelve","mergedAt":"2026-10-02T12:00:00Z","mergeCommit":{"oid":"2222222222222222222222222222222222222222"}}`,
		"13": `{"state":"MERGED","additions":10,"deletions":0,"title":"feat: thirteen","mergedAt":"2026-10-02T12:00:00Z","mergeCommit":{"oid":"3333333333333333333333333333333333333333"}}`,
		"14": `{"state":"MERGED","additions":10,"deletions":0,"title":"feat: fourteen","mergedAt":"2026-10-02T12:00:00Z","mergeCommit":{"oid":"4444444444444444444444444444444444444444"}}`,
	}
	reverts := `[
  {"number":20,"title":"Revert a thing","body":"This reverts commit abc1234.\n\nIt broke the build.","mergedAt":"2026-10-03T09:00:00Z"},
  {"number":21,"title":"Revert \"fix: other\"","body":"","mergedAt":"2026-10-05T09:00:00Z"},
  {"number":22,"title":"Undo fourteen","body":"Reverts o/r#14","mergedAt":"2026-10-03T09:00:00Z"}
]`
	issues := `[
  {"number":30,"title":"login broken","body":"Fixes regression from #12, seen on main.","createdAt":"2026-10-04T08:00:00Z","stateReason":"COMPLETED","closedByPullRequestsReferences":[{"number":31}]},
  {"number":32,"title":"add a regression test for #13","body":"","createdAt":"2026-10-04T08:00:00Z","stateReason":"COMPLETED","closedByPullRequestsReferences":[{"number":34}]},
  {"number":33,"title":"flaky","body":"caused by #13","createdAt":"2026-10-04T08:00:00Z","stateReason":"NOT_PLANNED","closedByPullRequestsReferences":[]}
]`

	calls := filepath.Join(t.TempDir(), "calls")
	script := `printf '%s\n' "$*" >> '` + calls + `'
case "$1 $2" in
"pr list") ` + herdrtest.CatScript(t, reverts) + ` ;;
"issue list") ` + herdrtest.CatScript(t, issues) + ` ;;
"pr view")
  case "$3" in
`
	for pr, view := range views {
		script += "  " + pr + ") " + herdrtest.CatScript(t, view) + " ;;\n"
	}
	script += `  *) echo "no pull requests found" >&2; exit 1 ;;
  esac ;;
*) exit 1 ;;
esac`
	herdrtest.FakeGh(t, script)
	return calls
}

func escapesDoc(t *testing.T, args ...string) statsJSON {
	t.Helper()
	var out strings.Builder
	if err := statsCommand(append([]string{"--json"}, args...), &out); err != nil {
		t.Fatalf("stats --json: %v", err)
	}
	var doc statsJSON
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("not a document: %v\n%s", err, out.String())
	}
	return doc
}

func TestStatsFindsEscapesWithOneSearchPerPass(t *testing.T) {
	calls := escapesFixture(t)
	doc := escapesDoc(t)

	want := map[int]*escapeFinding{
		50: {Kind: escapeRevert, By: 20, At: "2026-10-03T09:00:00Z"},
		51: nil,
		52: {Kind: escapeRegression, By: 30, At: "2026-10-04T08:00:00Z"},
		53: nil,
		54: {Kind: escapeRevert, By: 22, At: "2026-10-03T09:00:00Z"},
	}
	for _, d := range doc.Dispatches {
		w := want[d.Issue]
		if d.Escaped == nil || *d.Escaped != (w != nil) {
			t.Errorf("#%d escaped = %v, want %v", d.Issue, d.Escaped, w != nil)
			continue
		}
		if w == nil && d.Escape != nil || w != nil && (d.Escape == nil || *d.Escape != *w) {
			t.Errorf("#%d escape = %+v, want %+v", d.Issue, d.Escape, w)
		}
		if d.MergedAt == nil {
			t.Errorf("#%d merged_at is null", d.Issue)
		}
	}

	s := doc.Summary
	if s.EscapesChecked != 5 || s.Escapes == nil || *s.Escapes != 3 || s.EscapeRate == nil || *s.EscapeRate != 0.6 {
		t.Errorf("summary escapes = %d of %d at %v, want 3 of 5 at 0.6", deref(s.Escapes), s.EscapesChecked, s.EscapeRate)
	}
	if doc.EscapeWindow != 14*24*3600 {
		t.Errorf("escape window = %d, want 14 days", doc.EscapeWindow)
	}
	if doc.Alerts != nil {
		t.Errorf("alerts = %+v, want null when --check was not asked", doc.Alerts)
	}

	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	var prList, issueList int
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "pr list "):
			prList++
			if !strings.Contains(line, "merged:>=2026-09-01") || !strings.Contains(line, "--repo git@github.com:o/r.git") {
				t.Errorf("revert search %q is not bounded by the earliest merge in the repository", line)
			}
		case strings.HasPrefix(line, "issue list "):
			issueList++
		}
	}
	if prList != 1 || issueList != 1 {
		t.Errorf("gh searched %d times for reverts and %d for regressions over five pull requests, want once each", prList, issueList)
	}
}

// A wider window brings the late revert of #51 in.
func TestStatsEscapeWindowIsAFlag(t *testing.T) {
	escapesFixture(t)
	doc := escapesDoc(t, "--escape-window", "840h")
	for _, d := range doc.Dispatches {
		if d.Issue == 51 && (d.Escape == nil || d.Escape.By != 21) {
			t.Errorf("#51 escape = %+v, want the title revert #21 inside a 35-day window", d.Escape)
		}
	}
}

// A search gh cannot answer leaves the escapes unknown, not zero.
func TestStatsEscapesUnknownWhenSearchFails(t *testing.T) {
	escapesFixture(t)
	herdrtest.FakeGh(t, `case "$1 $2" in
"pr view") `+herdrtest.CatScript(t, `{"state":"MERGED","additions":1,"deletions":0,"title":"t","mergedAt":"2026-10-01T12:00:00Z"}`)+` ;;
*) echo "rate limited" >&2; exit 1 ;;
esac`)
	doc := escapesDoc(t)
	for _, d := range doc.Dispatches {
		if d.Escaped != nil {
			t.Errorf("#%d escaped = %v, want null when the search failed", d.Issue, *d.Escaped)
		}
	}
	if doc.Summary.EscapeRate != nil || doc.Summary.EscapesChecked != 0 {
		t.Errorf("summary = %+v, want no escape rate", doc.Summary)
	}
}

func TestOriginSlug(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:o/r.git":       "o/r",
		"https://github.com/o/r":       "o/r",
		"ssh://git@github.com/o/r.git": "o/r",
		"https://github.com/o/r.git/":  "o/r",
		"not a url":                    "",
		"":                             "",
	} {
		if got := originSlug(in); got != want {
			t.Errorf("originSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func deref(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}

func TestStatsPrintsTheEscapeRate(t *testing.T) {
	escapesFixture(t)
	var out strings.Builder
	if err := statsCommand(nil, &out); err != nil {
		t.Fatalf("stats: %v", err)
	}
	for _, want := range []string{"ESCAPED", "revert #20", "regression #30", "escape rate 3/5 (23–88%) of merged PRs reverted or blamed for a regression within 14d"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output is missing %q:\n%s", want, out.String())
		}
	}
}
