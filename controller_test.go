package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/herdrtest"
	"github.com/steig/worktender/internal/reconcile"
)

// controllerSession is a fake herdr plus the isolation the one-shot's inputs
// need: the fleet state directory (XDG_STATE_HOME) and the transcript store
// (~/.claude/projects, via HOME) both land in per-test directories, so no test
// reads this machine's fleet or arms anything outside its own scope.
//
// The gate is deliberately NOT set here: each test injects the value it is
// about, in test scope only.
func controllerSession(t *testing.T) *herdrtest.Server {
	t.Helper()

	server := herdrtest.NewServer(t)
	t.Setenv("HERDR_SOCKET_PATH", server.SocketPath)
	t.Setenv("HERDR_PLUGIN_CONTEXT_JSON", "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	return server
}

// staffableFleet registers the replies a successful cold staffing walks
// through: no agents anywhere, no workspaces, workspace.create answers with a
// fresh workspace and root pane, and the agent started there reports working
// as soon as the brief lands.
func staffableFleet(server *herdrtest.Server) {
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
	server.HandleResult("workspace.create", map[string]any{
		"type": "workspace_created",
		"workspace": map[string]any{
			"workspace_id": "wf", "number": 9, "label": "fleet", "focused": false,
			"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "unknown",
		},
		"root_pane": map[string]any{"pane_id": "wf:p1", "workspace_id": "wf", "tab_id": "t1", "index": 0},
		"tab":       map[string]any{"tab_id": "t1", "workspace_id": "wf", "index": 0},
	})
	server.HandleResult("pane.list", map[string]any{"type": "pane_list", "panes": []map[string]any{
		{"pane_id": "wf:p1", "workspace_id": "wf", "tab_id": "t1", "index": 0},
	}})
	server.HandleResult("agent.start", map[string]any{"type": "agent_started"})
	server.HandleResult("pane.send_text", map[string]any{"type": "ok"})
	server.HandleResult("pane.send_keys", map[string]any{"type": "ok"})
	server.HandleResult("agent.get", map[string]any{
		"type": "agent_info",
		"agent": map[string]any{
			"terminal_id": "term_1", "agent": "claude", "agent_status": "working",
			"workspace_id": "wf", "tab_id": "t1", "pane_id": "wf:p1",
			"focused": false, "revision": 1,
		},
	})
}

// callParams returns the params of the first call to method, and whether one
// was made.
func callParams(server *herdrtest.Server, method string) (map[string]any, bool) {
	for _, call := range server.Calls() {
		if call.Method == method {
			return call.Params, true
		}
	}
	return nil, false
}

func TestFleetStaffRejectsArguments(t *testing.T) {
	// Rejected before the gate is read, so the argument is never reported as
	// accepted — no herdr and no env needed to prove it.
	err := run([]string{"fleet", "staff", "stray"}, new(bytes.Buffer))
	if err == nil {
		t.Fatal("fleet staff with an argument should have been refused")
	}
	if exitCode(err) != exitUsage {
		t.Errorf("exit code %d, want usage", exitCode(err))
	}
}

// Unarmed is the default, and the default does nothing whatsoever: no herdr
// call is made, so the test runs against a machine with no herdr at all.
func TestFleetStaffDoesNothingUnarmed(t *testing.T) {
	herdrtest.HerdrDown(t)
	t.Setenv(fleetControllerEnv, "")

	var out strings.Builder
	if err := fleetStaffCommand(nil, &out); err != nil {
		t.Fatalf("unarmed fleet staff should be a quiet success, got %v", err)
	}
	if !strings.Contains(out.String(), fleetControllerEnv) {
		t.Errorf("the notice should name the gate:\n%s", out.String())
	}
	if strings.Contains(out.String(), "not a value this gate recognises") {
		t.Errorf("unset is a recognised state and owes no unrecognised-value notice:\n%s", out.String())
	}
}

// The gate fails closed: a value no rule covers is off, and the run says which
// value it refused — eventsEnv's semantics, on this gate.
func TestFleetStaffUnrecognisedValueStaysDown(t *testing.T) {
	herdrtest.HerdrDown(t)
	t.Setenv(fleetControllerEnv, "yes please")

	var out strings.Builder
	if err := fleetStaffCommand(nil, &out); err != nil {
		t.Fatalf("an unrecognised gate value must fail closed, not fail: %v", err)
	}
	if !strings.Contains(out.String(), `"yes please"`) {
		t.Errorf("the notice should name the refused value:\n%s", out.String())
	}
}

// Every spelling the events gate reads as off, this gate reads as off — the
// two share one parser precisely so they cannot drift.
func TestFleetStaffOptOutSpellings(t *testing.T) {
	herdrtest.HerdrDown(t)
	for _, value := range []string{"0", "false", "no", "off", " OFF "} {
		t.Setenv(fleetControllerEnv, value)

		var out strings.Builder
		if err := fleetStaffCommand(nil, &out); err != nil {
			t.Errorf("%s=%q should be a quiet off, got %v", fleetControllerEnv, value, err)
		}
		if strings.Contains(out.String(), "not a value this gate recognises") {
			t.Errorf("%s=%q is a recognised opt-out and owes no notice:\n%s", fleetControllerEnv, value, out.String())
		}
	}
}

// Armed but no herdr is an environment failure, like every herdr-only command.
func TestFleetStaffWithoutHerdrExitsEnvironment(t *testing.T) {
	herdrtest.HerdrDown(t)
	t.Setenv(fleetControllerEnv, "1")

	err := fleetStaffCommand(nil, new(strings.Builder))
	if err == nil {
		t.Fatal("fleet staff answered without a herdr")
	}
	if exitCode(err) != exitEnvironment {
		t.Errorf("exit code %d, want environment", exitCode(err))
	}
}

// The lease: a live agent named `fleet` means the controller exists, and the
// one-shot must not staff — wherever that agent lives, whatever it is doing,
// and regardless of what workspaces and panes look like. Pane existence is
// never consulted: herdr restores panes across a restart but not processes,
// so presence of the agent is the only fact that tracks the controller.
func TestFleetStaffStandsDownWhileTheLeaseHolds(t *testing.T) {
	server := controllerSession(t)
	t.Setenv(fleetControllerEnv, "1")
	for _, status := range []string{"working", "blocked", "idle"} {
		server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{{
			"terminal_id": "term_9", "name": "fleet", "agent": "claude", "agent_status": status,
			"workspace_id": "wx", "tab_id": "t1", "pane_id": "wx:p1", "focused": false, "revision": 1,
		}}})

		var out strings.Builder
		if err := fleetStaffCommand(nil, &out); err != nil {
			t.Fatalf("standing down is a success, got %v", err)
		}
		if called(t, server, "agent.start") {
			t.Fatalf("a live fleet agent (%s) holds the lease; nothing may be staffed", status)
		}
		if called(t, server, "workspace.create") {
			t.Fatal("standing down should not create workspaces either")
		}
		if !strings.Contains(out.String(), "nothing to staff") {
			t.Errorf("the stand-down should say so:\n%s", out.String())
		}
	}
}

// A worker's agent presence is not the controller's: worktree agent names can
// never be `fleet` (they always end in a hex digest), and a fleet of busy
// workers must not stop the controller that runs them from being staffed.
func TestFleetStaffIgnoresWorkerAgents(t *testing.T) {
	server := controllerSession(t)
	t.Setenv(fleetControllerEnv, "1")
	staffableFleet(server)
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{{
		"terminal_id": "term_2", "name": "fix-login-a1b2c3", "agent": "claude", "agent_status": "working",
		"workspace_id": "w2", "tab_id": "t2", "pane_id": "w2:p1", "focused": false, "revision": 1,
	}}})
	// The staffing re-check reads pane.list for the fleet workspace, which
	// must not contain the worker's pane.
	server.HandleResult("pane.list", map[string]any{"type": "pane_list", "panes": []map[string]any{
		{"pane_id": "wf:p1", "workspace_id": "wf", "tab_id": "t1", "index": 0},
	}})

	if err := fleetStaffCommand(nil, new(strings.Builder)); err != nil {
		t.Fatalf("fleet staff: %v", err)
	}
	if !called(t, server, "agent.start") {
		t.Fatal("a busy worker is not the controller; the controller should have been staffed")
	}
}

// First run on a machine: no fleet workspace exists, so the one-shot makes
// one — in the fleet state directory, labelled to be found again — and staffs
// its root pane cold, then briefs the session to load its skill.
func TestFleetStaffColdStart(t *testing.T) {
	server := controllerSession(t)
	t.Setenv(fleetControllerEnv, "1")
	staffableFleet(server)

	var out strings.Builder
	if err := fleetStaffCommand(nil, &out); err != nil {
		t.Fatalf("fleet staff: %v", err)
	}

	wantHome := filepath.Join(os.Getenv("XDG_STATE_HOME"), "fleet")
	create, ok := callParams(server, "workspace.create")
	if !ok {
		t.Fatal("no fleet workspace existed; one should have been created")
	}
	if create["label"] != "fleet" {
		t.Errorf("workspace label %v, want fleet", create["label"])
	}
	if create["cwd"] != wantHome {
		t.Errorf("workspace cwd %v, want the fleet state directory %s", create["cwd"], wantHome)
	}
	if create["focus"] != false {
		t.Errorf("staffing at startup must not yank focus; focus was %v", create["focus"])
	}
	if info, err := os.Stat(wantHome); err != nil || !info.IsDir() {
		t.Errorf("the fleet state directory should exist for herdr to open a shell in: %v", err)
	}

	start, ok := callParams(server, "agent.start")
	if !ok {
		t.Fatal("the controller should have been staffed")
	}
	if start["name"] != "fleet" || start["kind"] != "claude" {
		t.Errorf("agent.start name=%v kind=%v, want fleet/claude", start["name"], start["kind"])
	}
	if args, _ := start["args"].([]any); len(args) != 0 {
		t.Errorf("a cold start has no conversation to continue; args were %v", args)
	}

	brief, ok := callParams(server, "pane.send_text")
	if !ok {
		t.Fatal("the controller should have been briefed")
	}
	text, _ := brief["text"].(string)
	if !strings.Contains(text, "fleet-coordinator") {
		t.Errorf("the brief must tell the session to load the fleet-coordinator skill:\n%s", text)
	}
	if strings.Contains(text, fleetControllerEnv) {
		t.Errorf("the brief must never mention the gate — agents do not arm it:\n%s", text)
	}
	if strings.Contains(text, "\n") {
		t.Errorf("the brief must be one line — see PaneSendText:\n%q", text)
	}
	if brief["pane_id"] != "wf:p1" {
		t.Errorf("briefed pane %v, want the staffed pane wf:p1", brief["pane_id"])
	}
}

// A restart: the fleet workspace came back (herdr restores panes) but the
// controller process did not. The workspace is reused, not duplicated, and the
// staffing resumes the controller's own conversation with --continue.
func TestFleetStaffRestaffsWithContinueAfterRestart(t *testing.T) {
	server := controllerSession(t)
	t.Setenv(fleetControllerEnv, "1")
	staffableFleet(server)
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{{
		"workspace_id": "wf", "number": 9, "label": "fleet", "focused": false,
		"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "unknown",
	}}})

	// The controller's prior conversation, keyed the way Claude Code keys
	// them: by the directory it ran in.
	home := filepath.Join(os.Getenv("XDG_STATE_HOME"), "fleet")
	transcripts := filepath.Join(os.Getenv("HOME"), ".claude", "projects", reconcile.TranscriptSlug(home))
	if err := os.MkdirAll(transcripts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transcripts, "session.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := fleetStaffCommand(nil, &out); err != nil {
		t.Fatalf("fleet staff: %v", err)
	}
	if called(t, server, "workspace.create") {
		t.Error("the fleet workspace survived the restart and should be reused, not duplicated")
	}

	start, ok := callParams(server, "agent.start")
	if !ok {
		t.Fatal("the controller should have been re-staffed")
	}
	args, _ := start["args"].([]any)
	if len(args) != 1 || args[0] != "--continue" {
		t.Errorf("re-staffing must resume the prior conversation; args were %v", args)
	}
	if brief, ok := callParams(server, "pane.send_text"); ok {
		if text, _ := brief["text"].(string); !strings.Contains(text, "resumed") {
			t.Errorf("a resumed controller should be told it was resumed:\n%s", text)
		}
	} else {
		t.Fatal("a re-staffed controller still needs its brief")
	}
}

// A worktree workspace whose branch happens to be called `fleet` is somebody's
// checkout, not the controller's ground: the one-shot must look past it and
// make its own workspace.
func TestFleetStaffIgnoresWorktreeWorkspaceLabelledFleet(t *testing.T) {
	server := controllerSession(t)
	t.Setenv(fleetControllerEnv, "1")
	staffableFleet(server)
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{{
		"workspace_id": "wb", "number": 3, "label": "fleet", "focused": false,
		"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "idle",
		"worktree": map[string]any{"repo_key": "k", "repo_name": "repo",
			"repo_root": "/repo", "checkout_path": "/repo/wt/fleet", "is_linked_worktree": true},
	}}})

	if err := fleetStaffCommand(nil, new(strings.Builder)); err != nil {
		t.Fatalf("fleet staff: %v", err)
	}
	if !called(t, server, "workspace.create") {
		t.Fatal("the worktree workspace is not the controller's; a dedicated one should have been created")
	}
	if start, ok := callParams(server, "agent.start"); !ok || start["pane_id"] != "wf:p1" {
		t.Errorf("the controller belongs in its own workspace's pane, got %v", start)
	}
}

// The executor's re-check is the belt to the lease's braces: an agent that
// appeared in the fleet workspace between the lease check and the start makes
// this a skip — and a skipped staffing must not type a brief into the pane of
// a session somebody else started.
func TestFleetStaffDoesNotBriefWhenTheStaffingWasSkipped(t *testing.T) {
	server := controllerSession(t)
	t.Setenv(fleetControllerEnv, "1")
	staffableFleet(server)

	// The lease check sees no agents; the executor's re-check, one call
	// later, finds the fleet workspace's pane occupied.
	first := true
	server.Handle("agent.list", func(map[string]any) (any, error) {
		if first {
			first = false
			return map[string]any{"type": "agent_list", "agents": []map[string]any{}}, nil
		}
		return map[string]any{"type": "agent_list", "agents": []map[string]any{{
			"terminal_id": "term_3", "agent": "claude", "agent_status": "working",
			"workspace_id": "wf", "tab_id": "t1", "pane_id": "wf:p1", "focused": false, "revision": 1,
		}}}, nil
	})
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{{
		"workspace_id": "wf", "number": 9, "label": "fleet", "focused": false,
		"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "unknown",
	}}})

	var out strings.Builder
	if err := fleetStaffCommand(nil, &out); err != nil {
		t.Fatalf("a skipped staffing is not a failure: %v", err)
	}
	if called(t, server, "agent.start") {
		t.Error("the re-check found an occupant; nothing should have been started")
	}
	if called(t, server, "pane.send_text") {
		t.Error("nothing was staffed, so nothing should have been briefed")
	}
}

// The gate never bleeds into the events gate or back: arming the controller
// arms only the controller.
func TestControllerGateIsSeparateFromEvents(t *testing.T) {
	t.Setenv(eventsEnv, "")
	t.Setenv(fleetControllerEnv, "1")
	if eventsEnabled() {
		t.Error("arming the controller must not arm worktree events")
	}

	t.Setenv(eventsEnv, "1")
	t.Setenv(fleetControllerEnv, "")
	if fleetControllerEnabled() {
		t.Error("arming worktree events must not arm the controller")
	}
}
