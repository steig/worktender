package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/herdrtest"
)

// statsFixture lays down a ledger and an inbox describing three dispatches:
//
//   - #42 started at 10:00, planned, done with PR 7 at 10:30, sent back, done
//     again at 11:00 — two attempts, merged. Two gate runs, one naming it by
//     agent and one by pane, and three inbox messages, one of them before it
//     started.
//   - #43 started at 10:05, done with PR 8 once at 10:20 — one attempt, merged.
//   - #44 started at 10:10, done with PR 9, which gh cannot answer for.
//
// And one report from a branch nothing started, which belongs to no dispatch.
func statsFixture(t *testing.T) string {
	t.Helper()
	path := withLedger(t)
	lines := []string{
		`{"v":1,"event":"start","at":"2026-10-01T10:00:00Z","repository":"/r","branch":"42-a","pane_id":"w1:p1","issue":42,"origin":"git@github.com:o/r.git","role":"eng","agent":"builder","agent_kind":"claude","agent_name":"wt-42-a-x"}`,
		`{"v":1,"event":"start","at":"2026-10-01T10:05:00Z","repository":"/r","branch":"43-b","pane_id":"w2:p1","issue":43,"origin":"git@github.com:o/r.git","agent_kind":"claude","agent_name":"wt-43-b-x"}`,
		`{"v":1,"event":"start","at":"2026-10-01T10:10:00Z","repository":"/r","branch":"44-c","pane_id":"w3:p1","issue":44,"origin":"git@github.com:o/r.git","agent_kind":"claude","agent_name":"wt-44-c-x"}`,
		`{"v":1,"event":"gate","at":"2026-10-01T10:01:00Z","targets":[{"name":"wt-42-a-x","pane_id":"w1:p1"},{"name":"wt-43-b-x","pane_id":"w2:p1"}],"until":["done"]}`,
		`{"v":1,"event":"report","at":"2026-10-01T10:10:00Z","repository":"/r","branch":"42-a","status":"planned"}`,
		`{"v":1,"event":"report","at":"2026-10-01T10:20:00Z","repository":"/r","branch":"43-b","status":"done","pr":8}`,
		`{"v":1,"event":"report","at":"2026-10-01T10:30:00Z","repository":"/r","branch":"42-a","status":"done","pr":7}`,
		`{"v":1,"event":"report","at":"2026-10-01T10:31:00Z","repository":"/r","branch":"44-c","status":"done","pr":9}`,
		`{"v":1,"event":"report","at":"2026-10-01T10:32:00Z","repository":"/r","branch":"unstarted","status":"done","pr":99}`,
		`{"v":1,"event":"gate","at":"2026-10-01T10:40:00Z","targets":[{"name":"w1:p1","pane_id":"w1:p1"}],"until":["done"]}`,
		`{"v":1,"event":"report","at":"2026-10-01T11:00:00Z","repository":"/r","branch":"42-a","status":"done","pr":7}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	inbox := filepath.Join(filepath.Dir(path), "inbox")
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, at := range []string{"2026-10-01T09:00:00Z", "2026-10-01T10:15:00Z", "2026-10-01T10:45:00Z", "2026-10-01T10:50:00Z"} {
		if err := appendInboxMessage(inbox, "42", inboxMessage{From: "c", Note: "n", At: at}); err != nil {
			t.Fatal(err)
		}
	}

	// gh answers from files and cat, never echo: dash's echo rewrites
	// backslashes, and a JSON answer is where they turn up.
	answers := t.TempDir()
	herdrtest.WriteFile(t, filepath.Join(answers, "7.json"), `{"state":"MERGED","additions":150,"deletions":50}`)
	herdrtest.WriteFile(t, filepath.Join(answers, "8.json"), `{"state":"MERGED","additions":10,"deletions":0}`)
	herdrtest.FakeGh(t, `echo "$*" >> '`+filepath.Join(answers, "calls")+`'
f='`+answers+`'/"$3".json
if [ -f "$f" ]; then cat "$f"; else echo "no pull requests found" >&2; exit 1; fi`)
	return answers
}

func TestStatsDerivesTheFiguresPerMergedPR(t *testing.T) {
	answers := statsFixture(t)

	var out strings.Builder
	if err := statsCommand([]string{"--json"}, &out); err != nil {
		t.Fatalf("stats --json: %v", err)
	}
	var doc statsJSON
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("not a document: %v\n%s", err, out.String())
	}
	if len(doc.Dispatches) != 3 {
		t.Fatalf("got %d dispatches, want 3 — a report nothing started belongs to none", len(doc.Dispatches))
	}

	a := doc.Dispatches[0]
	if a.Issue != 42 || a.Attempts != 2 || a.PR == nil || *a.PR != 7 {
		t.Errorf("#42: issue %d, attempts %d, pr %v; want 42, 2, 7", a.Issue, a.Attempts, a.PR)
	}
	if len(a.Reports) != 3 || a.Reports[0].Status != "planned" {
		t.Errorf("#42 reports = %+v, want planned, done, done in order", a.Reports)
	}
	if a.Merged == nil || !*a.Merged {
		t.Errorf("#42 merged = %v, want true", a.Merged)
	}
	if a.WallClock == nil || *a.WallClock != 3600 {
		t.Errorf("#42 wall clock = %v, want 3600 — start to the LAST done", a.WallClock)
	}
	if a.GateRuns != 2 {
		t.Errorf("#42 gate runs = %d, want 2 (one by agent name, one by pane)", a.GateRuns)
	}
	if a.InboxMessages != 3 {
		t.Errorf("#42 inbox = %d, want 3 — the message before it started is not its ceremony", a.InboxMessages)
	}
	// (2 gates + 3 inbox) per 100 of 200 changed lines.
	if a.CeremonyRatio == nil || *a.CeremonyRatio != 2.5 {
		t.Errorf("#42 ceremony ratio = %v, want 2.5", a.CeremonyRatio)
	}
	if a.Role == nil || *a.Role != "eng" || a.AgentKind != "claude" {
		t.Errorf("#42 role %v kind %q, want eng and claude", a.Role, a.AgentKind)
	}

	b := doc.Dispatches[1]
	if b.Attempts != 1 || b.GateRuns != 1 || b.InboxMessages != 0 || b.WallClock == nil || *b.WallClock != 900 {
		t.Errorf("#43 = %+v, want one attempt, one gate, no inbox, 900s", b)
	}
	if b.Role != nil {
		t.Errorf("#43 role = %v, want null for a start with no role", *b.Role)
	}

	c := doc.Dispatches[2]
	if c.Merged != nil || c.Error == nil || c.Additions != nil || c.CeremonyRatio != nil {
		t.Errorf("#44: gh could not answer, so merged, diff and ratio are null and error is set; got %+v", c)
	}

	s := doc.Summary
	if s.Dispatches != 3 || s.WithPR != 3 || s.Merged != 2 {
		t.Errorf("summary counts = %+v, want 3 dispatches, 3 with PR, 2 merged", s)
	}
	if s.AttemptsPerMergedPR == nil || *s.AttemptsPerMergedPR != 1.5 {
		t.Errorf("attempts per merged PR = %v, want 1.5", s.AttemptsPerMergedPR)
	}
	if s.ReworkRate == nil || *s.ReworkRate != 0.5 {
		t.Errorf("rework rate = %v, want 0.5", s.ReworkRate)
	}
	if s.MedianWallClock == nil || *s.MedianWallClock != 2250 {
		t.Errorf("median wall clock = %v, want 2250", s.MedianWallClock)
	}

	calls, err := os.ReadFile(filepath.Join(answers, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pr view 7 --json state,additions,deletions,title,mergedAt,mergeCommit --repo git@github.com:o/r.git", "pr view 9 "} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("gh was not asked %q; calls:\n%s", want, calls)
		}
	}
	if strings.Contains(string(calls), "pr view 99") {
		t.Error("gh was asked about a pull request no dispatch owns")
	}
}

func TestStatsPrintsATableAndASummary(t *testing.T) {
	statsFixture(t)

	var out strings.Builder
	if err := statsCommand(nil, &out); err != nil {
		t.Fatalf("stats: %v", err)
	}
	for _, want := range []string{
		"CEREMONY/100L", "#42", "42-a", "eng", "1h0m0s", "2.50",
		"3 dispatches, 3 with a pull request, 2 merged",
		"rework rate 50%",
		"gh could not answer for 1 pull request(s)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output is missing %q:\n%s", want, out.String())
		}
	}
}

// Nothing dispatched yet is an answer, not an error, and in JSON it is a
// document with an empty list and null averages.
func TestStatsWithNoLedgerIsEmptyNotAnError(t *testing.T) {
	withLedger(t)
	herdrtest.FakeGh(t, "exit 1")

	var out strings.Builder
	if err := statsCommand([]string{"--json"}, &out); err != nil {
		t.Fatalf("stats --json: %v", err)
	}
	if !strings.Contains(out.String(), `"dispatches": []`) || !strings.Contains(out.String(), `"rework_rate": null`) {
		t.Errorf("want an empty list and null figures:\n%s", out.String())
	}
}

// A pr-feedback round is a retry the worker was sent on (#199): it counts as
// an attempt, as a floor under the done reports rather than on top of them.
func TestStatsCountsFeedbackRoundsAsAttempts(t *testing.T) {
	path := withLedger(t)
	herdrtest.FakeGh(t, "exit 1")
	lines := []string{
		`{"v":1,"event":"start","at":"2026-10-01T10:00:00Z","repository":"/r","branch":"42-a","issue":42}`,
		`{"v":1,"event":"report","at":"2026-10-01T10:30:00Z","repository":"/r","branch":"42-a","status":"done"}`,
		`{"v":1,"event":"feedback","at":"2026-10-01T10:40:00Z","repository":"/r","branch":"42-a","pr":7}`,
		`{"v":1,"event":"feedback","at":"2026-10-01T10:50:00Z","repository":"/r","branch":"42-a","pr":7}`,
		`{"v":1,"event":"feedback","at":"2026-10-01T10:55:00Z","repository":"/r","branch":"other","pr":8}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := statsCommand([]string{"--json"}, &out); err != nil {
		t.Fatalf("stats --json: %v", err)
	}
	var doc statsJSON
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("not a document: %v\n%s", err, out.String())
	}
	if len(doc.Dispatches) != 1 || doc.Dispatches[0].Rounds != 2 || doc.Dispatches[0].Attempts != 3 {
		t.Fatalf("got %+v, want one dispatch with 2 feedback rounds and 3 attempts", doc.Dispatches)
	}
}

// statsRoleOf runs stats both ways and returns the role each one shows for
// issue: the ROLE column of its table row, and its dispatch's role in --json.
func statsRoleOf(t *testing.T, issue int) (table string, doc *string) {
	t.Helper()

	var out strings.Builder
	if err := statsCommand(nil, &out); err != nil {
		t.Fatalf("stats: %v", err)
	}
	prefix := "#" + strconv.Itoa(issue) + " "
	for _, line := range strings.Split(out.String(), "\n") {
		if fields := strings.Fields(line); strings.HasPrefix(line, prefix) && len(fields) > 2 {
			table = fields[2]
		}
	}
	if table == "" {
		t.Fatalf("stats has no row for #%d:\n%s", issue, out.String())
	}

	out.Reset()
	if err := statsCommand([]string{"--json"}, &out); err != nil {
		t.Fatalf("stats --json: %v", err)
	}
	var parsed statsJSON
	if err := json.Unmarshal([]byte(out.String()), &parsed); err != nil {
		t.Fatalf("not a document: %v\n%s", err, out.String())
	}
	for _, d := range parsed.Dispatches {
		if d.Issue == issue {
			return table, d.Role
		}
	}
	t.Fatalf("stats --json has no dispatch for #%d:\n%s", issue, out.String())
	return "", nil
}

// The role start chose reaches stats from the ledger it wrote, not from a
// fixture of what that ledger ought to hold.
func TestStatsShowsTheRoleStartChose(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	fakeStart(t, repo, "w9", "w9:p1", "working")
	writeRoles(t, uxRoles, "ux-designer")
	herdrtest.FakeGh(t, herdrtest.CatScript(t, `{"number":42,"title":"Fix the thing","labels":[]}`))

	if err := startCommand([]string{"42", "--role", "ux"}, new(strings.Builder)); err != nil {
		t.Fatalf("start: %v", err)
	}

	table, doc := statsRoleOf(t, 42)
	if table != "ux" {
		t.Errorf("table role = %q, want ux", table)
	}
	if doc == nil || *doc != "ux" {
		t.Errorf("--json role = %v, want ux", doc)
	}
}

// factory runs start with no --role, so the role is the one the issue's labels
// pick, and the ledger records it the same as a start typed by hand.
func TestStatsShowsTheRoleOfAFactoryStart(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	factoryFleet(t, repo, nil, []queued{{5, "Five", []string{"role:ux"}}})
	writeRoles(t, uxRoles, "ux-designer")

	passFactory(t)

	table, doc := statsRoleOf(t, 5)
	if table != "ux" {
		t.Errorf("table role = %q, want ux", table)
	}
	if doc == nil || *doc != "ux" {
		t.Errorf("--json role = %v, want ux", doc)
	}
}
