package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrtest"
	"github.com/steig/worktender/internal/reconcile"
)

// withLedger points the plugin state at a disposable directory and returns the
// ledger path inside it.
func withLedger(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HERDR_PLUGIN_STATE_DIR", dir)
	return filepath.Join(dir, "ledger.jsonl")
}

func ledgerEvents(t *testing.T, path string) []ledgerEvent {
	t.Helper()
	events, skipped, err := readLedger(path)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("%d ledger lines did not decode", skipped)
	}
	return events
}

// Workers and coordinators append from separate processes at once. One write
// per line on an O_APPEND file is what keeps their lines whole, so a burst of
// concurrent appends has to read back as exactly that many decodable lines.
func TestLedgerAppendsStayWholeUnderConcurrentWriters(t *testing.T) {
	path := withLedger(t)
	const writers = 50

	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := ledgerEvent{Event: ledgerReport, Branch: strings.Repeat("b", 2000), PR: i + 1, Status: "done"}
			if err := appendLedger(e); err != nil {
				t.Errorf("append: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := len(ledgerEvents(t, path)); got != writers {
		t.Errorf("read back %d events, want %d", got, writers)
	}
}

// The ledger is a statistic about the work, never a reason the work fails. A
// report whose ledger cannot be written still reports.
func TestReportSucceedsWhenTheLedgerCannotBeWritten(t *testing.T) {
	// A file where the state directory should be: nothing can be created under
	// it.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_PLUGIN_STATE_DIR", blocker)
	t.Setenv("HERDR_SOCKET_PATH", "")
	t.Setenv(paneEnv, "")

	if err := reportCommand([]string{"--status", "done", "--note", "ok"}, new(strings.Builder)); err != nil {
		t.Fatalf("report failed because the ledger could not be written: %v", err)
	}
}

// A report is attributed by the checkout it was made from, so it records the
// main repository and the branch, not the worktree path.
func TestReportRecordsItsRepositoryAndBranch(t *testing.T) {
	path := withLedger(t)
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("42", "42-fix-the-thing")
	t.Chdir(checkout)
	t.Setenv("HERDR_SOCKET_PATH", "")
	t.Setenv(paneEnv, "")

	if err := reportCommand([]string{"--status", "done", "--pr", "7", "--note", "ok"}, new(strings.Builder)); err != nil {
		t.Fatalf("report: %v", err)
	}

	events := ledgerEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	e := events[0]
	if e.Event != ledgerReport || e.Status != "done" || e.PR != 7 {
		t.Errorf("recorded %+v, want a done report with pr 7", e)
	}
	if e.Repository != gitx.Resolve(repo.Root) || e.Branch != "42-fix-the-thing" {
		t.Errorf("recorded %s on %s, want %s on 42-fix-the-thing", e.Repository, e.Branch, repo.Root)
	}
	if e.At == "" {
		t.Error("a report must be stamped with when it happened")
	}
}

// start records the dispatch once the agent has its brief, with everything only
// start knows: the issue, the branch, the agent and its name.
func TestStartRecordsTheDispatch(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	fakeStart(t, repo, "w9", "w9:p1", "working")
	herdrtest.FakeGh(t, `cat <<'JSON'
{"number":42,"title":"Fix the thing"}
JSON`)

	if err := startCommand([]string{"42"}, new(strings.Builder)); err != nil {
		t.Fatalf("start: %v", err)
	}

	events := ledgerEvents(t, filepath.Join(os.Getenv("HERDR_PLUGIN_STATE_DIR"), "ledger.jsonl"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	e := events[0]
	want := ledgerEvent{
		V: ledgerVersion, Event: ledgerStart, At: e.At, Repository: gitx.Resolve(repo.Root),
		Branch: "42-fix-the-thing", PaneID: "w9:p1", Issue: 42, AgentKind: "claude",
		AgentName: reconcile.AgentName(repo.Root, "42-fix-the-thing"),
	}
	if !equalEvents(e, want) {
		t.Errorf("recorded\n  %+v\nwant\n  %+v", e, want)
	}
}

// A gate run is recorded when it opens, naming every worker it covers both ways
// a coordinator might have named it.
func TestGateRecordsTheWaitItOpened(t *testing.T) {
	path := withLedger(t)
	w := newGateWorker(t, "working", "")
	go w.prints(report{status: "done", pr: 12, note: "green"})

	if _, err, _ := w.gate(t, "--target", "worker", "--timeout", "10s"); err != nil {
		t.Fatalf("gate: %v", err)
	}

	events := ledgerEvents(t, path)
	if len(events) != 1 || events[0].Event != ledgerGate {
		t.Fatalf("got %+v, want one gate event", events)
	}
	if got := events[0].Targets; len(got) != 1 || got[0].Name != "worker" || got[0].PaneID == "" {
		t.Errorf("targets = %+v, want worker with its pane", got)
	}
	if got := events[0].Until; len(got) != 1 || got[0] != "done" {
		t.Errorf("until = %v, want [done]", got)
	}
}

// A torn or foreign line costs that line, not the ledger.
func TestReadLedgerSkipsLinesItCannotRead(t *testing.T) {
	events, skipped, err := decodeLedger(strings.NewReader(
		`{"v":1,"event":"start","at":"2026-01-01T00:00:00Z","issue":1}` + "\n" +
			`{"v":1,"event":"rep` + "\n" +
			`{"v":99,"event":"start"}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || skipped != 2 {
		t.Errorf("got %d events and %d skipped, want 1 and 2", len(events), skipped)
	}
}

func equalEvents(a, b ledgerEvent) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// A report records the session herdr names for the worker's pane, which is
// what stats finds its transcript by; a session given as a path is not one.
func TestReportRecordsTheWorkersSession(t *testing.T) {
	for _, tc := range []struct {
		kind, value, want string
	}{
		{"id", "0f3c-sess_1", "0f3c-sess_1"},
		{"path", "/tmp/x.jsonl", ""},
		{"id", "../x", ""},
	} {
		t.Run(tc.kind+" "+tc.value, func(t *testing.T) {
			path := withLedger(t)
			server := herdrtest.NewServer(t)
			t.Setenv(socketEnv, server.SocketPath)
			t.Setenv(paneEnv, "w1:p1")
			server.HandleResult("pane.get", map[string]any{"type": "pane_info", "pane": map[string]any{"pane_id": "w1:p1"}})
			server.HandleResult("pane.report_metadata", map[string]any{})
			server.HandleResult("agent.get", map[string]any{"type": "agent_info", "agent": map[string]any{
				"pane_id": "w1:p1", "agent_status": "working",
				"agent_session": map[string]any{"agent": "claude", "kind": tc.kind, "source": "herdr", "value": tc.value},
			}})

			_ = reportCommand([]string{"--status", "planned", "--note", "ok"}, new(strings.Builder))

			events := ledgerEvents(t, path)
			if len(events) != 1 || events[0].Session != tc.want {
				t.Errorf("recorded %+v, want session %q", events, tc.want)
			}
		})
	}
}
