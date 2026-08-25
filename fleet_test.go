package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steig/worktender/internal/herdrtest"
)

func TestFleetRejectsBadInvocations(t *testing.T) {
	for _, args := range [][]string{
		{"fleet"},
		{"fleet", "digest"},
		{"fleet", "board", "stray"},
		{"fleet", "board", "--watch", "--json"},
		{"fleet", "board", "--nonsense"},
	} {
		if err := run(args, new(bytes.Buffer)); err == nil {
			t.Errorf("run(%q) returned nil, want a usage error", args)
		} else if exitCode(err) != exitUsage {
			t.Errorf("run(%q) exit code %d, want usage", args, exitCode(err))
		}
	}
}

// The live half of the board is herdr's; without it the ledger's claims would
// render beside an empty fleet, which reads as every worker vanished.
func TestFleetBoardRequiresHerdr(t *testing.T) {
	herdrtest.HerdrDown(t)

	err := run([]string{"fleet", "board"}, new(bytes.Buffer))
	if err == nil {
		t.Fatal("fleet board answered without a herdr")
	}
	if exitCode(err) != exitEnvironment {
		t.Errorf("exit code %d, want environment", exitCode(err))
	}
}

// ledgerLine is one contract-shaped entry, n minutes ago.
func ledgerLine(n int, task, kind, extra string) string {
	ts := time.Now().Add(-time.Duration(n) * time.Minute).UTC().Format(time.RFC3339)
	line := fmt.Sprintf(`{"v":1,"ts":%q,"task":%q,"type":%q,"by":"fleet"`, ts, task, kind)
	if extra != "" {
		line += "," + extra
	}
	return line + "}\n"
}

func writeLedger(t *testing.T, lines ...string) {
	t.Helper()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	dir := filepath.Join(state, "fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ledger.jsonl"), []byte(strings.Join(lines, "")), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fleetFixture is a one-repository fleet: worktree 42-fix on workspace w2,
// pane w2:p1, whose worker has reported done #12 on its pane metadata.
func fleetFixture(t *testing.T) *herdrtest.Repo {
	t.Helper()

	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("42-fix", "42-fix")

	server := herdrtest.NewServer(t)
	t.Setenv("HERDR_SOCKET_PATH", server.SocketPath)
	t.Setenv("HERDR_PLUGIN_CONTEXT_JSON", "")
	t.Chdir(t.TempDir())

	server.HandleResult("workspace.list", workspaceListReply(repo, checkout, "w2"))
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "42-fix", "w2"))
	server.HandleResult("pane.list", map[string]any{"type": "pane_list",
		"panes": []map[string]any{{"pane_id": "w2:p1", "workspace_id": "w2", "tab_id": "t1", "index": 0}}})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})
	server.HandleResult("pane.get", map[string]any{
		"type": "pane_info",
		"pane": map[string]any{
			"pane_id": "w2:p1", "workspace_id": "w2", "tab_id": "t1",
			"terminal_id": "term_1", "agent_status": "idle",
			"focused": false, "revision": 1,
			"tokens": map[string]any{
				"worktender_v": "1", "worktender_seq": "1",
				"worktender_status": "done", "worktender_pr": "12",
			},
		},
	})
	return repo
}

// The board end to end: ledger tasks land on the worktrees they dispatched,
// escalations render on top, the worker's pane report shows, and the claimed
// pull request's state is asked of gh.
func TestFleetBoardMergesLedgerAndLiveState(t *testing.T) {
	repo := fleetFixture(t)
	herdrtest.FakeGhPRState(t, "OPEN")
	writeLedger(t,
		ledgerLine(30, "t1", "dispatch",
			fmt.Sprintf(`"executor":"worker","target":"wt-42","repo":%q,"issue":42`, repo.Root)),
		ledgerLine(20, "t1", "report", `"status":"done","pr":12`),
		ledgerLine(10, "t2", "escalate", `"severity":"safety","reason":"guard file touched"`),
		ledgerLine(5, "t3", "dispatch", `"executor":"peer","repo":"/nowhere/gone","issue":9`),
	)

	var out bytes.Buffer
	if err := run([]string{"fleet", "board"}, &out); err != nil {
		t.Fatalf("fleet board: %v", err)
	}
	text := out.String()

	for _, want := range []string{
		"escalations", "guard file touched", // the safety row
		"42-fix", "report done #12", // the matched task on its worktree
		"done #12",                                 // the worker's own pane report
		"OPEN",                                     // gh answered for the claimed PR
		"ledger tasks with no live worktree", "t3", // the orphan
	} {
		if !strings.Contains(text, want) {
			t.Errorf("board is missing %q:\n%s", want, text)
		}
	}

	// Escalations above the repositories: they are what the board is for.
	if esc, repoAt := strings.Index(text, "escalations"), strings.Index(text, repo.RealRoot); esc < 0 || repoAt < 0 || esc > repoAt {
		t.Errorf("escalations are not on top:\n%s", text)
	}
}

func TestFleetBoardSaysWhenThereIsNoLedger(t *testing.T) {
	fleetFixture(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var out bytes.Buffer
	if err := run([]string{"fleet", "board"}, &out); err != nil {
		t.Fatalf("fleet board: %v", err)
	}
	if !strings.Contains(out.String(), "ledger: none at") {
		t.Errorf("a missing ledger went unmentioned:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "42-fix") {
		t.Errorf("the live fleet must still render without a ledger:\n%s", out.String())
	}
}

// The wire-level smoke for --json; the projection itself is pinned in the
// fleet package's tests.
func TestFleetBoardJSON(t *testing.T) {
	repo := fleetFixture(t)
	herdrtest.FakeGhNoPR(t)
	writeLedger(t,
		ledgerLine(30, "t1", "dispatch", fmt.Sprintf(`"repo":%q,"issue":42`, repo.Root)),
		"this line is not json\n",
	)

	var out bytes.Buffer
	if err := run([]string{"fleet", "board", "--json"}, &out); err != nil {
		t.Fatalf("fleet board --json: %v", err)
	}

	var doc struct {
		Ledger struct {
			Found     bool `json:"found"`
			Malformed int  `json:"malformed"`
		} `json:"ledger"`
		Repositories []struct {
			Root      string `json:"root"`
			Worktrees []struct {
				Branch *string `json:"branch"`
				Task   *struct {
					Task string `json:"task"`
				} `json:"task"`
			} `json:"worktrees"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	if !doc.Ledger.Found || doc.Ledger.Malformed != 1 {
		t.Errorf("ledger block misprojected: %+v", doc.Ledger)
	}

	var found bool
	for _, r := range doc.Repositories {
		for _, w := range r.Worktrees {
			if w.Branch != nil && *w.Branch == "42-fix" && w.Task != nil && w.Task.Task == "t1" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("the dispatched worktree does not carry its task:\n%s", out.String())
	}
}
