package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/herdrtest"
	"github.com/steig/worktender/internal/reconcile"
)

// decodeOne parses stdout as exactly one JSON document into v.
func decodeOne(t *testing.T, out string, v any) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(v); err != nil {
		t.Fatalf("stdout is not a JSON document: %v\n%s", err, out)
	}
	if dec.More() {
		t.Fatalf("stdout carried more than one document:\n%s", out)
	}
}

// A worker reporting reads back what was *accepted*, which is not always what
// it intended — the note is refused rather than truncated, and the channels the
// envelope travels on have been observed to drop and mangle what they carry.
func TestReportJSONIsTheEnvelopeAsAccepted(t *testing.T) {
	server := herdrtest.NewServer(t)
	tokens := map[string]string{}
	server.Handle("pane.report_metadata", func(params map[string]any) (any, error) {
		for key, value := range params["tokens"].(map[string]any) {
			if text, ok := value.(string); ok {
				tokens[key] = text
				continue
			}
			delete(tokens, key)
		}
		return map[string]any{"type": "ok"}, nil
	})
	server.Handle("pane.get", func(map[string]any) (any, error) {
		return map[string]any{"type": "pane_info", "pane": map[string]any{
			"pane_id": "w1:p1", "workspace_id": "w1", "tab_id": "w1:t1",
			"terminal_id": "term_1", "agent_status": "working",
			"focused": false, "revision": 1, "tokens": tokens,
		}}, nil
	})

	t.Setenv("HERDR_SOCKET_PATH", server.SocketPath)
	t.Setenv(paneEnv, "w1:p1")

	var out strings.Builder
	if err := run([]string{"report", "--status", "done", "--pr", "4", "--note", "green", "--json"}, &out); err != nil {
		t.Fatalf("report: %v", err)
	}

	var doc reportEnvelopeJSON
	decodeOne(t, out.String(), &doc)

	if doc.Status != "done" {
		t.Errorf("status = %q, want done", doc.Status)
	}
	if doc.PR == nil || *doc.PR != 4 {
		t.Errorf("pr = %v, want 4", doc.PR)
	}
	if doc.Note != "green" {
		t.Errorf("note = %q, want green", doc.Note)
	}
	if doc.NoteLength != len("green") || doc.NoteLimit != noteLimit {
		t.Errorf("note_length/%d note_limit/%d, want %d/%d", doc.NoteLength, doc.NoteLimit, len("green"), noteLimit)
	}
	if doc.PaneID == nil || *doc.PaneID != "w1:p1" {
		t.Errorf("pane_id = %v, want w1:p1", doc.PaneID)
	}
	if !doc.Delivered {
		t.Error("delivered = false, but the metadata channel accepted it")
	}
	if doc.ExitCode != exitOK {
		t.Errorf("exit_code = %d, want %d", doc.ExitCode, exitOK)
	}
}

// Outside herdr there is no pane to attach to. The report is still correct and
// still exits 0 — but a worker that cannot tell "delivered" from "printed"
// cannot tell whether it must make sure the line reaches its terminal.
func TestReportJSONSaysWhenItWasOnlyPrinted(t *testing.T) {
	t.Setenv("HERDR_SOCKET_PATH", "")
	t.Setenv(paneEnv, "")

	var out strings.Builder
	if err := run([]string{"report", "--status", "planned", "--note", "starting", "--json"}, &out); err != nil {
		t.Fatalf("report outside herdr should still succeed: %v", err)
	}

	var doc reportEnvelopeJSON
	decodeOne(t, out.String(), &doc)

	if doc.Delivered {
		t.Error("delivered = true with no pane to deliver to")
	}
	if doc.PaneID != nil {
		t.Errorf("pane_id = %v, want null outside herdr", doc.PaneID)
	}
	if doc.PR != nil {
		t.Errorf("pr = %v, want null when the worker gave none", doc.PR)
	}
	if doc.ExitCode != exitOK {
		t.Errorf("exit_code = %d, want %d — no pane is not a failed report", doc.ExitCode, exitOK)
	}
}

// The envelope must still reach a terminal in JSON mode. It is one of the two
// channels a gate reads, and a worker whose stdout its own tooling captured
// would otherwise have reported to nobody.
func TestReportJSONStillPrintsTheEnvelopeSomewhere(t *testing.T) {
	t.Setenv("HERDR_SOCKET_PATH", "")
	t.Setenv(paneEnv, "")

	var out strings.Builder
	if err := run([]string{"report", "--status", "done", "--note", "n", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), reportHeader) {
		t.Errorf("the envelope was written to stdout beside the document, which is what breaks the consumer:\n%s", out.String())
	}
}

// `start --json` exists so an agent does not have to scrape its own tool's
// prose for the handle on what it just started. The agent name is the field
// that matters: it is a repository-scoped digest, not the branch, and it is
// what a gate takes.
func TestStartJSONCarriesTheHandleOnWhatItStarted(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	fakeStart(t, repo, "w9", "w9:p1", "working")
	herdrtest.FakeGh(t, `cat <<'JSON'
{"number":42,"title":"Fix the thing","body":"it is broken"}
JSON`)

	var out strings.Builder
	if err := startCommand([]string{"42", "--json"}, &out); err != nil {
		t.Fatalf("start: %v", err)
	}

	var doc startJSON
	decodeOne(t, out.String(), &doc)

	if doc.Issue != 42 {
		t.Errorf("issue = %d, want 42", doc.Issue)
	}
	if doc.Branch == nil || *doc.Branch != "42-fix-the-thing" {
		t.Errorf("branch = %v, want 42-fix-the-thing", doc.Branch)
	}
	if doc.PaneID == nil || *doc.PaneID != "w9:p1" {
		t.Errorf("pane_id = %v, want w9:p1", doc.PaneID)
	}
	if doc.WorkspaceID == nil || *doc.WorkspaceID != "w9" {
		t.Errorf("workspace_id = %v, want w9", doc.WorkspaceID)
	}
	want := reconcile.AgentName(repo.Root, "42-fix-the-thing")
	if doc.AgentName == nil || *doc.AgentName != want {
		t.Errorf("agent_name = %v, want %q — the digest, not the branch", doc.AgentName, want)
	}
	if !doc.Briefed {
		t.Error("briefed = false, but the agent took the brief up")
	}
	if doc.GateCommand == nil || !strings.Contains(*doc.GateCommand, "gate --target w9:p1 ") {
		t.Errorf("gate_command = %v, should wait on the pane this start staffed", doc.GateCommand)
	}
	if doc.ForkPoint == nil || *doc.ForkPoint == "" {
		t.Error("fork_point is null; a ref moves and the commit is what does not")
	}
	if doc.ExitCode != exitOK {
		t.Errorf("exit_code = %d, want %d", doc.ExitCode, exitOK)
	}
	if doc.Error != nil {
		t.Errorf("error should be null on success, got %q", *doc.Error)
	}
}

// Nothing but the document may reach stdout. `start` prints five lines of
// progress for a human, and every one of them beside a JSON document is what
// breaks the consumer reading it.
func TestStartJSONKeepsProgressOffStdout(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	fakeStart(t, repo, "w9", "w9:p1", "working")
	herdrtest.FakeGh(t, `cat <<'JSON'
{"number":42,"title":"Fix the thing","body":""}
JSON`)

	var out strings.Builder
	if err := startCommand([]string{"42", "--json"}, &out); err != nil {
		t.Fatalf("start: %v", err)
	}
	for _, prose := range []string{"repository:", "worktree:", "fork point:", "wait for it with"} {
		if strings.Contains(out.String(), prose) {
			t.Errorf("stdout carries the %q progress line beside the document:\n%s", prose, out.String())
		}
	}
}

// `dispatch --json` is small because dispatch is: it staffs one named pane. The
// gate it hands back names that pane, not --name, which herdr can stop resolving
// while the agent runs.
func TestDispatchJSONNamesTheAgentAndPane(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server := fakeStart(t, repo, "w9", "w9:p1", "working")
	// dispatch resolves the pane's workspace before staffing it; nothing else
	// in the start fixture needs pane.get.
	server.HandleResult("pane.get", map[string]any{"type": "pane_info", "pane": map[string]any{
		"pane_id": "w9:p1", "workspace_id": "w9", "tab_id": "t1",
		"terminal_id": "term_1", "agent_status": "idle", "focused": false, "revision": 1,
	}})

	var out strings.Builder
	if err := dispatchCommand([]string{"--pane", "w9:p1", "--name", "worker-one", "--json"}, &out); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	var doc dispatchJSON
	decodeOne(t, out.String(), &doc)

	if doc.PaneID != "w9:p1" {
		t.Errorf("pane_id = %q, want w9:p1", doc.PaneID)
	}
	if doc.AgentName != "worker-one" {
		t.Errorf("agent_name = %q, want worker-one", doc.AgentName)
	}
	if doc.GateCommand == nil || !strings.Contains(*doc.GateCommand, "gate --target w9:p1 ") {
		t.Errorf("gate_command = %v, should wait on the pane just dispatched to", doc.GateCommand)
	}
	if len(doc.Staffing) == 0 {
		t.Error("staffing should carry the action's own result")
	}
	if doc.ExitCode != exitOK {
		t.Errorf("exit_code = %d, want %d", doc.ExitCode, exitOK)
	}
}
