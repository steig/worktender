package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steig/worktender/internal/herdrtest"
)

type liveDispatch struct {
	Tokens []modelTokens `json:"tokens"`
	Live   bool          `json:"live"`
	AsOf   *string       `json:"as_of"`
	Merged *bool         `json:"merged"`
}

func liveStats(t *testing.T, args ...string) []liveDispatch {
	t.Helper()
	var out strings.Builder
	if err := statsCommand(append([]string{"--json"}, args...), &out); err != nil {
		t.Fatalf("stats --json: %v", err)
	}
	var doc struct {
		Dispatches []liveDispatch `json:"dispatches"`
	}
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("not a document: %v\n%s", err, out.String())
	}
	return doc.Dispatches
}

func liveLedger(t *testing.T, lines ...string) string {
	t.Helper()
	path := withLedger(t)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func liveHome(t *testing.T) string {
	home := t.TempDir()
	t.Setenv("HOME", home)
	return filepath.Join(home, ".claude", "projects", "-wt")
}

const liveStart = `{"v":1,"event":"start","at":"2026-10-01T10:00:00Z","repository":"/r","branch":"42-a","issue":42,"session":"s1"}`

func TestStatsLiveCountsARunningDispatchFromItsStartSession(t *testing.T) {
	liveLedger(t, liveStart)
	projects := liveHome(t)
	transcript := filepath.Join(projects, "s1.jsonl")
	herdrtest.WriteFile(t, transcript, transcriptLine("m1", "opus", 1, 2, 3, 4)+"\n"+transcriptLine("m2", "opus", 10, 20, 30, 40)+"\n")
	at := time.Date(2026, 10, 1, 10, 15, 0, 0, time.UTC)
	if err := os.Chtimes(transcript, at, at); err != nil {
		t.Fatal(err)
	}

	ds := liveStats(t)
	want := []modelTokens{{"opus", tokenCounts{11, 22, 33, 44}}}
	if !reflect.DeepEqual(ds[0].Tokens, want) {
		t.Errorf("tokens = %+v, want %+v", ds[0].Tokens, want)
	}
	if !ds[0].Live {
		t.Error("a dispatch with no report is live")
	}
	if ds[0].AsOf == nil || *ds[0].AsOf != "2026-10-01T10:15:00Z" {
		t.Errorf("as_of = %v, want 2026-10-01T10:15:00Z", ds[0].AsOf)
	}
}

func TestStatsLiveEndsWithADoneOrBlockedReport(t *testing.T) {
	var doneTokens []modelTokens
	for _, tc := range []struct {
		status string
		live   bool
	}{{"planned", true}, {"done", false}, {"blocked", false}} {
		t.Run(tc.status, func(t *testing.T) {
			liveLedger(t, liveStart,
				`{"v":1,"event":"report","at":"2026-10-01T10:10:00Z","repository":"/r","branch":"42-a","status":"`+tc.status+`","session":"s1"}`)
			herdrtest.WriteFile(t, filepath.Join(liveHome(t), "s1.jsonl"), transcriptLine("m1", "opus", 1, 2, 3, 4)+"\n")
			d := liveStats(t)[0]
			if d.Live != tc.live {
				t.Errorf("live = %v, want %v", d.Live, tc.live)
			}
			if tc.status == "done" {
				doneTokens = d.Tokens
			}
		})
	}

	// A finished dispatch's figure is what it was before start named a session.
	liveLedger(t, strings.Replace(liveStart, `,"session":"s1"`, "", 1),
		`{"v":1,"event":"report","at":"2026-10-01T10:10:00Z","repository":"/r","branch":"42-a","status":"done","session":"s1"}`)
	herdrtest.WriteFile(t, filepath.Join(liveHome(t), "s1.jsonl"), transcriptLine("m1", "opus", 1, 2, 3, 4)+"\n")
	if got := liveStats(t)[0].Tokens; got == nil || !reflect.DeepEqual(got, doneTokens) {
		t.Errorf("tokens with no start session = %+v, with one = %+v", got, doneTokens)
	}
}

func TestStatsLiveAgainAfterAFeedbackRound(t *testing.T) {
	liveLedger(t, liveStart,
		`{"v":1,"event":"report","at":"2026-10-01T10:10:00Z","repository":"/r","branch":"42-a","status":"done","pr":7}`,
		`{"v":1,"event":"feedback","at":"2026-10-01T10:20:00Z","repository":"/r","branch":"42-a","pr":7}`)
	liveHome(t)
	herdrtest.FakeGh(t, `echo '{"state":"OPEN"}'`)
	if !liveStats(t)[0].Live {
		t.Error("a feedback round sends the worker back to work, so it is live")
	}
}

func TestStatsLiveIsFalseForASupersededDispatch(t *testing.T) {
	liveLedger(t, liveStart, strings.Replace(liveStart, "10:00:00", "11:00:00", 1))
	liveHome(t)
	ds := liveStats(t)
	if len(ds) != 2 || ds[0].Live || !ds[1].Live {
		t.Errorf("live = %+v, want false then true", ds)
	}
}

func TestStatsLiveAsOfIsNullWithoutATranscript(t *testing.T) {
	liveLedger(t, strings.Replace(liveStart, `"s1"`, `"gone"`, 1))
	liveHome(t)
	d := liveStats(t)[0]
	if d.Tokens != nil || d.AsOf != nil || !d.Live {
		t.Errorf("tokens %+v, as_of %v, live %v; want null, null, true", d.Tokens, d.AsOf, d.Live)
	}
}

func TestStatsLiveNoGhAsksGhNothing(t *testing.T) {
	answers := statsFixture(t)
	herdrtest.WriteFile(t, filepath.Join(liveHome(t), "s42.jsonl"), transcriptLine("m1", "opus", 1, 2, 3, 4)+"\n")
	// Appended to the fixture's ledger: #42's start names a session.
	path, _ := ledgerPath()
	raw, _ := os.ReadFile(path)
	raw = []byte(strings.Replace(string(raw), `"issue":42,`, `"issue":42,"session":"s42",`, 1))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	ds := liveStats(t, "--no-gh")
	if _, err := os.Stat(filepath.Join(answers, "calls")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("--no-gh still called gh (stat: %v)", err)
	}
	for i, d := range ds {
		if d.Merged != nil {
			t.Errorf("dispatch %d merged = %v, want null", i, *d.Merged)
		}
	}
	if ds[0].Tokens == nil {
		t.Error("#42's tokens should still be counted")
	}

	liveStats(t)
	if _, err := os.Stat(filepath.Join(answers, "calls")); err != nil {
		t.Errorf("without --no-gh the fixture should call gh: %v", err)
	}
}

func TestStatsLiveNoGhRefusesCheck(t *testing.T) {
	liveLedger(t, liveStart)
	err := statsCommand([]string{"--no-gh", "--check"}, new(strings.Builder))
	if err == nil || !strings.Contains(err.Error(), "--no-gh") || exitCode(err) != exitUsage {
		t.Errorf("err = %v, want a usage error naming --no-gh", err)
	}
}

func TestStatsLiveAsOfIsTheNewestTranscript(t *testing.T) {
	liveLedger(t, liveStart)
	projects := liveHome(t)
	main := filepath.Join(projects, "s1.jsonl")
	sub := filepath.Join(projects, "s1", "subagents", "agent-a.jsonl")
	herdrtest.WriteFile(t, main, transcriptLine("m1", "opus", 1, 2, 3, 4)+"\n")
	herdrtest.WriteFile(t, sub, transcriptLine("m2", "opus", 10, 20, 30, 40)+"\n")
	// The main transcript is the newer one, and the glob lists it first, so
	// neither the earliest nor the last file read can pass for the newest.
	older := time.Date(2026, 10, 1, 10, 5, 0, 0, time.UTC)
	newer := time.Date(2026, 10, 1, 10, 25, 0, 0, time.UTC)
	for path, at := range map[string]time.Time{main: newer, sub: older} {
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}

	ds := liveStats(t)
	if ds[0].AsOf == nil || *ds[0].AsOf != "2026-10-01T10:25:00Z" {
		t.Errorf("as_of = %v, want 2026-10-01T10:25:00Z", ds[0].AsOf)
	}
}

func TestStatsLiveNoGhSkipsTheEscapeLookup(t *testing.T) {
	liveLedger(t,
		`{"v":1,"event":"start","at":"2026-10-01T10:00:00Z","repository":"/r","branch":"42-a","issue":42,"origin":"git@github.com:o/r.git"}`,
		`{"v":1,"event":"report","at":"2026-10-01T10:30:00Z","repository":"/r","branch":"42-a","status":"done","pr":7}`)
	liveHome(t)
	calls := filepath.Join(t.TempDir(), "calls")
	// Only a merged pull request with a merge time is searched for escapes, so
	// the searches follow from the pull request lookup that --no-gh skips.
	herdrtest.FakeGh(t, `echo "$1 $2" >> '`+calls+`'
if [ "$1 $2" = "pr view" ]; then
  echo '{"state":"MERGED","additions":1,"deletions":1,"mergedAt":"2026-10-01T11:00:00Z"}'
else
  echo '[]'
fi`)

	liveStats(t, "--no-gh")
	if _, err := os.Stat(calls); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("--no-gh still called gh (stat: %v)", err)
	}

	liveStats(t)
	raw, _ := os.ReadFile(calls)
	if got := string(raw); !strings.Contains(got, "pr list") || !strings.Contains(got, "issue list") {
		t.Errorf("without --no-gh gh was asked %q, want the pr view and both escape searches", got)
	}
}
