package wt_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/herdrtest"
	"github.com/steig/worktender/internal/wt"
)

// session is one agent's agent_session as herdr 0.9.5 reports it.
type session struct {
	agent, kind, value string
}

// agentListWithSessions is agentList with each agent also naming its session,
// which is the field the last-active time is found from.
func agentListWithSessions(sessions map[string]session) map[string]any {
	agents := make([]map[string]any, 0, len(sessions))
	for pane, s := range sessions {
		workspace, _, _ := strings.Cut(pane, ":")
		agents = append(agents, map[string]any{
			"pane_id": pane, "workspace_id": workspace, "tab_id": workspace + ":t1",
			"terminal_id": "term-" + pane, "focused": false, "revision": 1,
			"agent_status": "idle", "state_change_seq": 1, "agent": s.agent,
			"agent_session": map[string]any{
				"agent": s.agent, "kind": s.kind, "source": "herdr:" + s.agent, "value": s.value,
			},
		})
	}
	return map[string]any{"type": "agent_list", "agents": agents}
}

// writeLog puts an empty file at path under home with the given mtime. Empty,
// because the lookup must never need the contents — a log that answers only by
// its mtime is the proof.
func writeLog(t *testing.T, home, path string, mtime time.Time) {
	t.Helper()
	full := filepath.Join(home, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(full, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// #197: herdr has no clock, but it does name each agent's session, and the
// agent's own log for that session is written as it works. Both layouts the
// issue names are found by id alone — the Claude slug directory is globbed, not
// recomputed — and the age is that file's mtime, to the second.
func TestWithAgentSeqsReadsTheLastActiveTimeOffTheSessionLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	claudeAt := time.Now().Add(-3*time.Hour - 30*time.Minute).Truncate(time.Second)
	codexAt := time.Now().Add(-90 * time.Second).Truncate(time.Second)
	const claudeID = "130d3bc6-6be9-4bab-b203-88c6584e7290"
	const codexID = "0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"
	writeLog(t, home, ".claude/projects/-home-you-code-worktender/"+claudeID+".jsonl", claudeAt)
	writeLog(t, home, ".codex/sessions/2026/10/15/rollout-2026-10-15T10-00-00-"+codexID+".jsonl", codexAt)

	server := herdrtest.NewServer(t)
	server.HandleResult("agent.list", agentListWithSessions(map[string]session{
		"w2:p1": {"claude", "id", claudeID},
		"w3:p1": {"codex", "id", codexID},
	}))
	rows := []wt.Row{
		{Branch: "claude-worker", PaneID: "w2:p1", AgentStatus: "idle", Dir: "claude-worker"},
		{Branch: "codex-worker", PaneID: "w3:p1", AgentStatus: "working", Dir: "codex-worker"},
	}
	wt.WithAgentSeqs(herdrapi.NewWithSocket(server.SocketPath), rows)

	if !rows[0].AgentLastActive.Equal(claudeAt) {
		t.Errorf("claude: last active %v, want the log's mtime %v", rows[0].AgentLastActive, claudeAt)
	}
	if !rows[1].AgentLastActive.Equal(codexAt) {
		t.Errorf("codex: last active %v, want the log's mtime %v", rows[1].AgentLastActive, codexAt)
	}

	// The table prints an age, coarse on purpose; the JSON the timestamp.
	var buf bytes.Buffer
	if err := wt.Render(&buf, rows, wt.Columns{}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if f := strings.Fields(lines[0]); len(f) < 6 || f[5] != "3h" {
		t.Errorf("claude row should show an age of 3h in the AGE column: %q", lines[0])
	}
	if f := strings.Fields(lines[1]); len(f) < 6 || f[5] != "1m" {
		t.Errorf("codex row should show an age of 1m in the AGE column: %q", lines[1])
	}

	docs := wt.JSON(rows)
	if got := docs[0].AgentLastActive; got == nil || *got != claudeAt.UTC().Format(time.RFC3339) {
		t.Errorf("claude agent_last_active = %v, want %s", got, claudeAt.UTC().Format(time.RFC3339))
	}
	if got := docs[1].AgentLastActive; got == nil || *got != codexAt.UTC().Format(time.RFC3339) {
		t.Errorf("codex agent_last_active = %v, want %s", got, codexAt.UTC().Format(time.RFC3339))
	}
}

// Never guess. Every way of not finding the log leaves the field empty — the
// table's "-" and the JSON's null — rather than borrowing a time from anywhere
// else: a log that is not there, an agent with no known layout, a session given
// as a path, and an id that would turn the glob into a search of other files.
func TestWithAgentSeqsLeavesTheAgeEmptyWhenTheLogIsNotFound(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A real log for a different session, so a lookup that matched loosely would
	// have something to wrongly find.
	writeLog(t, home, ".claude/projects/-x/other-session.jsonl", time.Now())
	writeLog(t, home, ".codex/sessions/2026/10/15/rollout-2026-10-15T10-00-00-other-session.jsonl", time.Now())

	server := herdrtest.NewServer(t)
	server.HandleResult("agent.list", agentListWithSessions(map[string]session{
		"w1:p1": {"claude", "id", "no-such-session"},
		"w2:p1": {"codex", "id", "no-such-session"},
		"w3:p1": {"gemini", "id", "other-session"},
		"w4:p1": {"claude", "path", "other-session"},
		"w5:p1": {"claude", "id", "*"},
		"w6:p1": {"claude", "id", "../projects/-x/other-session"},
	}))
	var rows []wt.Row
	for _, pane := range []string{"w1:p1", "w2:p1", "w3:p1", "w4:p1", "w5:p1", "w6:p1"} {
		rows = append(rows, wt.Row{Branch: pane, PaneID: pane, AgentStatus: "idle", Dir: pane})
	}
	wt.WithAgentSeqs(herdrapi.NewWithSocket(server.SocketPath), rows)

	for _, row := range rows {
		if !row.AgentLastActive.IsZero() {
			t.Errorf("%s: last active %v, want none", row.PaneID, row.AgentLastActive)
		}
	}

	raw, err := json.Marshal(wt.JSON(rows[:1]))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"agent_last_active":null`) {
		t.Errorf("a log that was not found must be null on the wire, present rather than omitted:\n%s", raw)
	}

	var buf bytes.Buffer
	if err := wt.Render(&buf, rows[:1], wt.Columns{}); err != nil {
		t.Fatal(err)
	}
	if f := strings.Fields(buf.String()); len(f) < 6 || f[5] != "-" {
		t.Errorf("AGE should be - when the log was not found: %q", buf.String())
	}
}
