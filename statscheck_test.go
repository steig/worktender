package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/steig/worktender/internal/herdrtest"
)

func runCheck(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out strings.Builder
	err := statsCommand(append([]string{"--check"}, args...), &out)
	if err != nil && exitCode(err) != exitNeedsHuman {
		t.Fatalf("stats --check: %v (exit %d), want nil or exit 3", err, exitCode(err))
	}
	return out.String(), err
}

// ceremonyFixture is two dispatches with small diffs: #70 changed 10 lines
// under six gate runs (60 per 100 lines), #71 changed one line under two
// (200 per 100, but only two events — under the floor).
func ceremonyFixture(t *testing.T) {
	t.Helper()
	path := withLedger(t)
	lines := []string{
		`{"v":1,"event":"start","at":"2026-10-01T10:00:00Z","repository":"/r","branch":"70-a","pane_id":"w1:p1","issue":70,"origin":"git@github.com:o/r.git","agent_kind":"claude","agent_name":"wt-70"}`,
		`{"v":1,"event":"start","at":"2026-10-01T10:00:00Z","repository":"/r","branch":"71-b","pane_id":"w2:p1","issue":71,"origin":"git@github.com:o/r.git","agent_kind":"claude","agent_name":"wt-71"}`,
		`{"v":1,"event":"report","at":"2026-10-01T10:30:00Z","repository":"/r","branch":"70-a","status":"planned","pr":7}`,
		`{"v":1,"event":"report","at":"2026-10-01T10:30:00Z","repository":"/r","branch":"71-b","status":"planned","pr":8}`,
	}
	for i := 0; i < 6; i++ {
		lines = append(lines, `{"v":1,"event":"gate","at":"2026-10-01T10:10:00Z","targets":[{"name":"wt-70","pane_id":"w1:p1"}],"until":["done"]}`)
	}
	for i := 0; i < 2; i++ {
		lines = append(lines, `{"v":1,"event":"gate","at":"2026-10-01T10:10:00Z","targets":[{"name":"wt-71","pane_id":"w2:p1"}],"until":["done"]}`)
	}
	herdrtest.WriteFile(t, path, strings.Join(lines, "\n")+"\n")
	herdrtest.FakeGh(t, `case "$3" in
7) `+herdrtest.CatScript(t, `{"state":"OPEN","additions":8,"deletions":2}`)+` ;;
8) `+herdrtest.CatScript(t, `{"state":"OPEN","additions":1,"deletions":0}`)+` ;;
*) exit 1 ;;
esac`)
}

func TestStatsCheckAlertsOnCeremony(t *testing.T) {
	ceremonyFixture(t)
	out, err := runCheck(t, "--since", "0")
	if exitCode(err) != exitNeedsHuman {
		t.Fatalf("exit %d, want 3", exitCode(err))
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "alert: ceremony #70 70-a: 60.00 ") || !strings.Contains(lines[0], "over 20") {
		t.Errorf("want exactly one ceremony alert for #70 (#71 is under the floor):\n%s", out)
	}
}

func TestStatsCheckThresholdsAreFlags(t *testing.T) {
	ceremonyFixture(t)
	out, err := runCheck(t, "--since", "0", "--max-ceremony-ratio", "60")
	if err != nil || out != "" {
		t.Errorf("at a threshold of 60, a ratio of exactly 60 does not exceed it; got %q, %v", out, err)
	}
}

// Under --check with no --since, a month-old dispatch is out of scope.
func TestStatsCheckLooksBackAMonthByDefault(t *testing.T) {
	ceremonyFixture(t)
	restore := ledgerNow
	ledgerNow = func() time.Time { return time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { ledgerNow = restore })

	out, err := runCheck(t)
	if err != nil || out != "" {
		t.Errorf("a dispatch two months old raised %q, %v", out, err)
	}
}

func TestStatsCheckAlertsOnEscapeRate(t *testing.T) {
	escapesFixture(t)
	out, err := runCheck(t, "--since", "0")
	if exitCode(err) != exitNeedsHuman {
		t.Fatalf("exit %d, want 3", exitCode(err))
	}
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "alert: escape rate: 3 of 5 merged PRs (60%)") {
		t.Errorf("want one escape-rate alert:\n%s", out)
	}

	var buf strings.Builder
	err = statsCommand([]string{"--check", "--since", "0", "--json"}, &buf)
	if exitCode(err) != exitNeedsHuman {
		t.Fatalf("--json exit %d, want 3", exitCode(err))
	}
	var doc statsJSON
	if err := json.Unmarshal([]byte(buf.String()), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Alerts) != 1 || doc.Alerts[0].Kind != alertEscapeRate || doc.Alerts[0].Issue != nil || doc.Alerts[0].Value != 0.6 {
		t.Errorf("alerts = %+v, want one fleet-wide escape_rate at 0.6", doc.Alerts)
	}
}

// proofLoopFixture starts a dispatch on a real branch, whose only commit is
// "now", and lays its done report and the activity after it at the offsets
// given from now.
func proofLoopFixture(t *testing.T, done time.Duration, after []time.Duration) {
	t.Helper()
	repo := herdrtest.NewRepo(t)
	repo.Git("branch", "60-loop")
	now := time.Now().UTC()
	stamp := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339Nano) }

	path := withLedger(t)
	lines := []string{
		`{"v":1,"event":"start","at":"` + stamp(-3*time.Hour) + `","repository":"` + repo.Root + `","branch":"60-loop","pane_id":"w1:p1","issue":60,"agent_kind":"claude","agent_name":"wt-60"}`,
		`{"v":1,"event":"report","at":"` + stamp(done) + `","repository":"` + repo.Root + `","branch":"60-loop","status":"done"}`,
	}
	for i, d := range after {
		if i%2 == 0 {
			lines = append(lines, `{"v":1,"event":"gate","at":"`+stamp(d)+`","targets":[{"name":"wt-60","pane_id":"w1:p1"}],"until":["done"]}`)
		} else {
			lines = append(lines, `{"v":1,"event":"report","at":"`+stamp(d)+`","repository":"`+repo.Root+`","branch":"60-loop","status":"done"}`)
		}
	}
	herdrtest.WriteFile(t, path, strings.Join(lines, "\n")+"\n")
	herdrtest.FakeGhUnavailable(t)
}

func TestStatsCheckAlertsOnARecursiveProofLoop(t *testing.T) {
	// Done a minute from now, after the branch's last commit, and four rounds
	// of gating and re-reporting since with nothing committed.
	proofLoopFixture(t, time.Minute, []time.Duration{2 * time.Minute, 3 * time.Minute, 4 * time.Minute, 5 * time.Minute})
	out, err := runCheck(t)
	if exitCode(err) != exitNeedsHuman {
		t.Fatalf("exit %d, want 3:\n%s", exitCode(err), out)
	}
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "alert: proof loop #60 60-loop: 4 gate runs and reports since it said done") {
		t.Errorf("want one proof-loop alert:\n%s", out)
	}
}

// The same four rounds, but the branch moved after them: the worker was
// working, not re-proving.
func TestStatsCheckQuietWhenTheBranchMoved(t *testing.T) {
	proofLoopFixture(t, -time.Hour, []time.Duration{-50 * time.Minute, -40 * time.Minute, -30 * time.Minute, -20 * time.Minute})
	out, err := runCheck(t)
	if err != nil || out != "" {
		t.Errorf("want quiet, got %q, %v", out, err)
	}
}

// Nothing over any threshold: no output, exit 0, and in JSON an empty list
// rather than null.
func TestStatsCheckQuiet(t *testing.T) {
	statsFixture(t)
	out, err := runCheck(t, "--since", "0")
	if err != nil || out != "" {
		t.Errorf("want quiet, got %q, %v", out, err)
	}

	var buf strings.Builder
	if err := statsCommand([]string{"--check", "--since", "0", "--json"}, &buf); err != nil {
		t.Fatalf("--check --json: %v", err)
	}
	if !strings.Contains(buf.String(), `"alerts": []`) {
		t.Errorf("want an empty alerts list:\n%s", buf.String())
	}
}
