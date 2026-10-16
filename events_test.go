package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrtest"
	"github.com/steig/worktender/internal/repolock"
)

// armEvent sets the environment herdr sets when it invokes an [[events]] hook.
//
// The two names are deliberately in different namespaces, which is measured
// behaviour rather than a guess: herdr sets HERDR_PLUGIN_EVENT to the dotted
// manifest name and the `event` field inside the envelope to the underscored
// payload discriminator, in the same invocation.
func armEvent(t *testing.T, checkout, branch, repoRoot, workspaceID string) {
	t.Helper()

	worktree := map[string]any{
		"path": checkout, "branch": branch, "label": filepath.Base(checkout),
		"is_bare": false, "is_detached": false, "is_prunable": false,
		"is_linked_worktree": true,
	}
	if workspaceID != "" {
		worktree["open_workspace_id"] = workspaceID
	}

	envelope := map[string]any{
		"event": "worktree_opened",
		"data": map[string]any{
			"type":         "worktree_opened",
			"already_open": false,
			"worktree":     worktree,
			"workspace": map[string]any{
				"workspace_id": workspaceID, "number": 7, "label": "wt",
				"focused": false, "pane_count": 1, "tab_count": 1,
				"active_tab_id": "t1", "agent_status": "unknown",
				"worktree": map[string]any{
					"repo_key": repoRoot + "/.git", "repo_name": filepath.Base(repoRoot),
					"repo_root": repoRoot, "checkout_path": checkout,
					"is_linked_worktree": true,
				},
			},
		},
	}

	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	t.Setenv("HERDR_PLUGIN_EVENT", "worktree.opened")
	t.Setenv("HERDR_PLUGIN_EVENT_JSON", string(raw))
}

func called(t *testing.T, server *herdrtest.Server, method string) bool {
	t.Helper()

	for _, call := range server.Calls() {
		if call.Method == method {
			return true
		}
	}
	return false
}

// unadoptedRepo is a repository with one linked worktree herdr has no workspace
// for — the state a full plan would adopt and then staff.
func unadoptedRepo(t *testing.T) (*herdrtest.Repo, string, *herdrtest.Server) {
	t.Helper()

	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("wip", "wip")

	server := fakeSession(t, repo)
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "wip", ""))
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})
	server.HandleResult("worktree.open", map[string]any{"type": "workspace_created"})
	return repo, checkout, server
}

// The safety property, and the reason this test is first.
//
// steig.worktender is linked into a live herdr session straight from the working
// checkout, so an [[events]] block is armed the moment it is saved. It is also
// the correct shipping default: a marketplace plugin must not start autonomous
// coding agents on install without being asked.
func TestEventHandlerIsOffByDefault(t *testing.T) {
	repo, checkout, server := unadoptedRepo(t)
	armEvent(t, checkout, "wip", repo.RealRoot, "")
	// t.Setenv, not os.Unsetenv: only the former is restored when the test ends,
	// and leaking this variable would silently disarm every test after it.
	t.Setenv(eventsEnv, "")

	var out strings.Builder
	if err := onEventCommand(nil, &out); err != nil {
		t.Fatalf("a disabled handler must exit 0, not fail: %v", err)
	}

	for _, method := range []string{"worktree.open", "agent.start", "worktree.remove"} {
		if called(t, server, method) {
			t.Errorf("handler called %s while opt-in was unset", method)
		}
	}
	// A silent no-op is the failure mode this whole codebase avoids.
	if !strings.Contains(out.String(), "WORKTENDER_EVENTS") {
		t.Errorf("a disabled handler must say why it did nothing, got: %q", out.String())
	}
}

// The inversion this gate used to have, pinned as a table.
//
// The old rule was `"", "0", "false" -> off, everything else -> on`, so
// WORKTENDER_EVENTS=off armed the trigger it was typed to disarm — as did "no",
// "False", "disabled" and a lone space. That is a switch that starts coding
// agents in response to being told not to, and the README's whole safety
// argument rests on it not doing that.
func TestEventsGateReadsFalseySpellingsAsOff(t *testing.T) {
	for _, value := range []string{
		"0", "false", "FALSE", "False",
		"no", "NO", "n", "N",
		"off", "OFF", "Off",
		"disabled", "DISABLED",
		" ", "\t", " off ", " 0 ",
	} {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			t.Setenv(eventsEnv, value)
			if eventsEnabled() {
				t.Errorf("%s=%q enabled autonomous agent starts", eventsEnv, value)
			}
		})
	}
}

func TestEventsGateReadsTruthySpellingsAsOn(t *testing.T) {
	for _, value := range []string{
		"1", "true", "TRUE", "True",
		"yes", "YES", "y", "Y",
		"on", "ON", "enabled", " 1 ",
	} {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			t.Setenv(eventsEnv, value)
			if !eventsEnabled() {
				t.Errorf("%s=%q did not opt in", eventsEnv, value)
			}
		})
	}
}

// A value no rule covers is not a request to start agents. Falling closed costs
// a mistyped opt-in its opt-in, so the cost is paid back in the output: the
// handler names the value it did not understand.
func TestEventsGateFailsClosedOnAnUnrecognisedValueAndSaysSo(t *testing.T) {
	repo, checkout, server := unadoptedRepo(t)
	armEvent(t, checkout, "wip", repo.RealRoot, "")
	t.Setenv(eventsEnv, "ture")

	var out strings.Builder
	if err := onEventCommand(nil, &out); err != nil {
		t.Fatalf("an unrecognised value must decline, not fail: %v", err)
	}

	for _, method := range []string{"worktree.open", "agent.start", "worktree.remove"} {
		if called(t, server, method) {
			t.Errorf("%s=ture enabled %s; an unparsed value must not arm the trigger", eventsEnv, method)
		}
	}
	if !strings.Contains(out.String(), "ture") {
		t.Errorf("declining must name the value it did not understand, got: %q", out.String())
	}
}

// The other half of the same bargain: an opt-OUT is understood, so it gets no
// "I did not understand you" line. Only the plain off-by-default explanation.
func TestEventsGateDoesNotScoldADeliberateOptOut(t *testing.T) {
	repo, checkout, _ := unadoptedRepo(t)
	armEvent(t, checkout, "wip", repo.RealRoot, "")
	t.Setenv(eventsEnv, "off")

	var out strings.Builder
	if err := onEventCommand(nil, &out); err != nil {
		t.Fatalf("opting out must not fail: %v", err)
	}

	if strings.Contains(out.String(), "not a value this gate recognises") {
		t.Errorf("%s=off is a recognised opt-out, got: %q", eventsEnv, out.String())
	}
}

// The rename's one real hazard. Someone who exported the old name months ago
// has a variable that is still set and an opt-in that no longer exists, and the
// failure is quiet: events simply stop. The old name must enable nothing and
// must be named in the output, so the silence has a printed cause.
// Every superseded name is covered, not just the most recent one: whoever
// opted in under the oldest spelling skipped the intermediate era entirely and
// is the likeliest to still be carrying a dead export.
func TestEventHandlerRefusesTheOldEnvNameAndSaysSo(t *testing.T) {
	for _, legacy := range legacyEventsEnvs {
		t.Run(legacy, func(t *testing.T) {
			repo, checkout, server := unadoptedRepo(t)
			armEvent(t, checkout, "wip", repo.RealRoot, "")
			t.Setenv(eventsEnv, "")
			t.Setenv(legacy, "1")

			var out strings.Builder
			if err := onEventCommand(nil, &out); err != nil {
				t.Fatalf("the old name must decline, not fail: %v", err)
			}

			for _, method := range []string{"worktree.open", "agent.start", "worktree.remove"} {
				if called(t, server, method) {
					t.Errorf("the old env name enabled %s; it must not be an alias", method)
				}
			}
			for _, want := range []string{legacy, eventsEnv} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("declining on the old name must mention %s, got: %q", want, out.String())
				}
			}
		})
	}
}

func TestEventHandlerActsWhenOptedIn(t *testing.T) {
	repo, checkout, server := unadoptedRepo(t)
	armEvent(t, checkout, "wip", repo.RealRoot, "")
	t.Setenv("WORKTENDER_EVENTS", "1")

	var out strings.Builder
	if err := onEventCommand(nil, &out); err != nil {
		t.Fatalf("onEvent: %v", err)
	}

	if !called(t, server, "worktree.open") {
		t.Errorf("handler should have adopted the worktree; output:\n%s", out.String())
	}
}

// The event path runs the same plan as `sync`, filtered to adopt and staff.
// Removal stays a deliberate human action: an event-triggered prune is exactly
// the stray autonomous removal the separate prune/prune-apply actions exist to
// prevent.
func TestEventHandlerNeverPrunes(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("done", "done")
	repo.CommitIn(checkout, "done.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")

	// An authoritative merged verdict — the one thing that CAN justify a prune.
	herdrtest.FakeGhPRState(t, "MERGED")

	server := fakeSession(t, repo)
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "done", ""))
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})
	server.HandleResult("worktree.open", map[string]any{"type": "workspace_created"})

	armEvent(t, checkout, "done", repo.RealRoot, "")
	t.Setenv("WORKTENDER_EVENTS", "1")

	var out strings.Builder
	if err := onEventCommand(nil, &out); err != nil {
		t.Fatalf("onEvent: %v", err)
	}

	if called(t, server, "worktree.remove") {
		t.Error("the event path asked herdr to remove a worktree")
	}
	if !repo.Exists(checkout) {
		t.Fatal("the event path removed a worktree")
	}
}

// Prune actions are filtered out of the event path, so the PR lookup that would
// authorise one buys nothing and costs a network round trip per worktree per
// event. It must not run at all.
func TestEventHandlerMakesNoGhCalls(t *testing.T) {
	repo, checkout, _ := unadoptedRepo(t)

	sentinel := filepath.Join(t.TempDir(), "gh-was-called")
	herdrtest.FakeGh(t, "touch "+sentinel+"; "+herdrtest.GhPRScript(t, "OPEN"))

	armEvent(t, checkout, "wip", repo.RealRoot, "")
	t.Setenv("WORKTENDER_EVENTS", "1")

	var out strings.Builder
	if err := onEventCommand(nil, &out); err != nil {
		t.Fatalf("onEvent: %v", err)
	}

	if _, err := os.Stat(sentinel); err == nil {
		t.Error("the event path shelled out to gh; it must make no network calls")
	}
}

// The payload names the repository the event is about. The invocation context
// describes what happened to be focused, which is a different question — and
// for a pane event, often a different repository.
func TestEventHandlerScopesFromPayloadNotContext(t *testing.T) {
	repo, checkout, server := unadoptedRepo(t)

	// Point the context somewhere else entirely.
	elsewhere := herdrtest.NewRepo(t)
	t.Setenv("HERDR_PLUGIN_CONTEXT_JSON", `{"workspace_cwd":"`+elsewhere.Root+`"}`)

	armEvent(t, checkout, "wip", repo.RealRoot, "")
	t.Setenv("WORKTENDER_EVENTS", "1")

	var out strings.Builder
	if err := onEventCommand(nil, &out); err != nil {
		t.Fatalf("onEvent: %v", err)
	}

	for _, call := range server.Calls() {
		if call.Method != "worktree.list" {
			continue
		}
		cwd, _ := call.Params["cwd"].(string)
		if !strings.Contains(cwd, filepath.Base(repo.RealRoot)) {
			t.Errorf("worktree.list scoped to %q, want the payload's repository %q", cwd, repo.RealRoot)
		}
		return
	}
	t.Fatal("handler never asked herdr for the worktree list")
}

// outOfRootRepo is a repository whose only worktree sits outside it, the shape
// herdr's own worktree creation produces (~/.herdr/worktrees/<repo>/...).
func outOfRootRepo(t *testing.T) (*herdrtest.Repo, string, *herdrtest.Server) {
	t.Helper()

	repo := herdrtest.NewRepo(t)
	outside := filepath.Join(t.TempDir(), "worktree-brave-valley-66f8")
	checkout := repo.AddWorktreeAt(outside, "worktree/brave-valley-66f8")

	if strings.HasPrefix(checkout, repo.Root) {
		t.Fatalf("checkout %s is inside %s; this test proves nothing", checkout, repo.Root)
	}

	server := fakeSession(t, repo)
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "worktree/brave-valley-66f8", ""))
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})
	server.HandleResult("worktree.open", map[string]any{"type": "workspace_created"})
	return repo, checkout, server
}

// The fast path must handle a checkout that is not under the repository root.
// On this machine that is not hypothetical: it is the one worktree the fast path
// would reach first.
func TestEventHandlerActsOnAWorktreeOutsideTheRepoRoot(t *testing.T) {
	repo, checkout, server := outOfRootRepo(t)
	armEvent(t, checkout, "worktree/brave-valley-66f8", repo.RealRoot, "")
	t.Setenv(eventsEnv, "1")

	var out strings.Builder
	if err := onEventCommand(nil, &out); err != nil {
		t.Fatalf("onEvent: %v", err)
	}

	for _, call := range server.Calls() {
		if call.Method != "worktree.open" {
			continue
		}
		if path, _ := call.Params["path"].(string); path != checkout {
			t.Errorf("adopted %q, want %q", path, checkout)
		}
		return
	}
	t.Errorf("the out-of-root worktree was never adopted; output:\n%s", out.String())
}

// The riskier half of the same case: when herdr omits the repository root, the
// handler derives it with git FROM THE CHECKOUT. For a checkout outside the
// repository that derivation is the only thing standing between the event and
// the wrong repository — or no repository at all.
func TestEventHandlerDerivesRootFromAnOutOfRootCheckout(t *testing.T) {
	repo, checkout, server := outOfRootRepo(t)

	// An envelope with no workspace.worktree, so nothing carries repo_root.
	envelope := map[string]any{
		"event": "worktree_opened",
		"data": map[string]any{
			"type": "worktree_opened", "already_open": false,
			"worktree": map[string]any{
				"path": checkout, "branch": "worktree/brave-valley-66f8",
				"label": filepath.Base(checkout), "is_bare": false,
				"is_detached": false, "is_prunable": false, "is_linked_worktree": true,
			},
			"workspace": map[string]any{
				"workspace_id": "w9", "number": 9, "label": "wt", "focused": false,
				"pane_count": 1, "tab_count": 1, "active_tab_id": "t1",
				"agent_status": "unknown",
			},
		},
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	t.Setenv("HERDR_PLUGIN_EVENT", "worktree.opened")
	t.Setenv("HERDR_PLUGIN_EVENT_JSON", string(raw))
	t.Setenv(eventsEnv, "1")

	var out strings.Builder
	if err := onEventCommand(nil, &out); err != nil {
		t.Fatalf("onEvent: %v", err)
	}

	for _, call := range server.Calls() {
		if call.Method != "worktree.list" {
			continue
		}
		cwd, _ := call.Params["cwd"].(string)
		if cwd != repo.RealRoot {
			t.Errorf("derived repository root %q, want %q", cwd, repo.RealRoot)
		}
		return
	}
	t.Fatal("handler never asked herdr for the worktree list")
}

// A second handler arriving while the first is mid-reconcile must stand down
// rather than run the same whole-repository pass again. Without this, a batch of
// worktree events becomes a batch of identical reconciles, into herdr's own
// concurrent-plugin-command limit.
func TestASecondHandlerCoalescesIntoTheRunningPass(t *testing.T) {
	repo, checkout, server := unadoptedRepo(t)
	state := t.TempDir()
	t.Setenv("HERDR_PLUGIN_STATE_DIR", state)
	armEvent(t, checkout, "wip", repo.RealRoot, "")
	t.Setenv(eventsEnv, "1")

	// Stand in for the reconcile already in progress.
	held, err := repolock.Acquire(state, gitx.Resolve(repo.RealRoot))
	if err != nil || held == nil {
		t.Fatalf("could not simulate a holder: %v", err)
	}

	var out strings.Builder
	if err := onEventCommand(nil, &out); err != nil {
		t.Fatalf("coalescing must not be an error: %v", err)
	}

	if called(t, server, "worktree.open") {
		t.Error("the second handler reconciled anyway; the lock excluded nothing")
	}
	if !strings.Contains(out.String(), "coalesced") {
		t.Errorf("standing down must say so, got: %q", out.String())
	}

	// And the holder must be told there is more to do, or the event is lost.
	if !held.TakeDirty() {
		t.Error("the coalesced handler left no mark; the holder will finish on a stale snapshot")
	}
}

// The holder picks up work marked during its pass and runs again, so an event
// that arrived mid-reconcile is acted on rather than dropped.
func TestAMarkDuringAPassTriggersAnotherPass(t *testing.T) {
	repo, checkout, server := unadoptedRepo(t)
	state := t.TempDir()
	t.Setenv("HERDR_PLUGIN_STATE_DIR", state)
	armEvent(t, checkout, "wip", repo.RealRoot, "")
	t.Setenv(eventsEnv, "1")

	// Mark the repository from "another handler" the first time herdr is asked
	// for the worktree list, i.e. while the pass is underway.
	passes := 0
	server.Handle("worktree.list", func(map[string]any) (any, error) {
		passes++
		if passes == 1 {
			if err := repolock.MarkDirty(state, gitx.Resolve(repo.RealRoot)); err != nil {
				t.Errorf("mark dirty: %v", err)
			}
		}
		return worktreeListReply(repo, checkout, "wip", ""), nil
	})

	var out strings.Builder
	if err := onEventCommand(nil, &out); err != nil {
		t.Fatalf("onEvent: %v", err)
	}

	if passes < 2 {
		t.Errorf("ran %d pass(es); work marked mid-pass was dropped", passes)
	}
	// Bounded, or a steady arrival rate would spin forever.
	if passes > reconcilePasses {
		t.Errorf("ran %d passes, more than the %d cap", passes, reconcilePasses)
	}
}

// herdr sends something unparseable only when herdr has a bug, which is worth
// surfacing rather than defaulting around — the same call context.go already
// makes for a malformed invocation context.
func TestEventHandlerRejectsAMalformedEnvelope(t *testing.T) {
	repo, _, _ := unadoptedRepo(t)
	_ = repo

	t.Setenv("WORKTENDER_EVENTS", "1")
	t.Setenv("HERDR_PLUGIN_EVENT", "worktree.opened")
	t.Setenv("HERDR_PLUGIN_EVENT_JSON", `{"event":`)

	if err := onEventCommand(nil, &strings.Builder{}); err == nil {
		t.Fatal("a malformed envelope must not be treated as absent")
	}
}

// Subscribing to an event herdr can deliver but this plugin has no handler for
// must be a quiet no-op, not a failure: the manifest and the binary version can
// legitimately differ across an upgrade.
func TestEventHandlerIgnoresAnUnhandledKind(t *testing.T) {
	_, _, server := unadoptedRepo(t)

	t.Setenv("WORKTENDER_EVENTS", "1")
	t.Setenv("HERDR_PLUGIN_EVENT", "layout.updated")
	t.Setenv("HERDR_PLUGIN_EVENT_JSON", `{"event":"layout_updated","data":{"type":"layout_updated"}}`)

	var out strings.Builder
	if err := onEventCommand(nil, &out); err != nil {
		t.Fatalf("an unhandled event kind must not fail: %v", err)
	}
	if called(t, server, "worktree.open") {
		t.Error("an unhandled event kind caused a mutation")
	}
}
