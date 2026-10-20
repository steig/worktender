package main

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/herdrtest"
)

func TestWorkspaceNameCarriesRepoIssueAndShortTitle(t *testing.T) {
	if got := workspaceName("worktender", 324, "Name herdr workspaces"); got != "worktender #324 Name herdr workspaces" {
		t.Errorf("name = %q", got)
	}
}

// The title is the issue author's text and ends up in a tab bar.
func TestWorkspaceNameEscapesAndCapsTheTitle(t *testing.T) {
	got := workspaceName("r", 1, "evil\n‮title "+strings.Repeat("x", 100))
	if strings.ContainsFunc(got, func(r rune) bool { return r == '\n' || r == '‮' }) {
		t.Errorf("name %q holds a control or bidi rune", got)
	}
	if n := utf8.RuneCountInString(got); n > len("r #1 ")+nameTitleMax {
		t.Errorf("name is %d runes, over the cap", n)
	}
}

func TestStatusNameReplacesThePreviousStatus(t *testing.T) {
	for _, tc := range []struct {
		current string
		r       report
		want    string
	}{
		{"repo #9 title", report{status: "planned"}, "… repo #9 title"},
		{"… repo #9 title", report{status: "blocked"}, "! repo #9 title"},
		{"! repo #9 title", report{status: "done", pr: 14}, "✓ PR 14 repo #9 title"},
		{"✓ PR 14 repo #9 title", report{status: "done", pr: 15}, "✓ PR 15 repo #9 title"},
		{"✓ PR 14 repo #9 title", report{status: "blocked"}, "! repo #9 title"},
	} {
		got, ok := statusName(tc.current, tc.r)
		if !ok || got != tc.want {
			t.Errorf("statusName(%q, %s) = %q, %v; want %q", tc.current, tc.r.status, got, ok, tc.want)
		}
	}
}

func TestStatusNameLeavesAPersonsOwnNameAlone(t *testing.T) {
	if got, ok := statusName("scratch", report{status: "done"}); ok {
		t.Errorf("renamed a workspace worktender never named to %q", got)
	}
}

func fakeWorkspaceNames(t *testing.T, label string) (*herdrtest.Server, *herdrapi.Client) {
	t.Helper()
	server := herdrtest.NewServer(t)
	server.HandleResult("pane.get", map[string]any{"type": "pane_info", "pane": map[string]any{
		"pane_id": "w1:p1", "workspace_id": "w1", "tab_id": "t1"}})
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{{
		"workspace_id": "w1", "number": 1, "label": label, "focused": false,
		"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "idle"}}})
	server.HandleResult("workspace.rename", map[string]any{"type": "ok"})
	return server, herdrapi.NewWithSocket(server.SocketPath)
}

func TestAReportRelabelsTheReportingWorkspace(t *testing.T) {
	server, client := fakeWorkspaceNames(t, "repo #9 title")
	if err := relabelWorkspace(client, "w1:p1", report{status: "done", pr: 14}); err != nil {
		t.Fatal(err)
	}
	renames := calledWith(server, "workspace.rename")
	if len(renames) != 1 || renames[0].Params["label"] != "✓ PR 14 repo #9 title" || renames[0].Params["workspace_id"] != "w1" {
		t.Errorf("renames = %v", renames)
	}
}

func TestAReportDoesNotRenameAWorkspaceWorktenderDidNotName(t *testing.T) {
	server, client := fakeWorkspaceNames(t, "scratch")
	if err := relabelWorkspace(client, "w1:p1", report{status: "blocked"}); err != nil {
		t.Fatal(err)
	}
	if n := len(calledWith(server, "workspace.rename")); n != 0 {
		t.Errorf("%d renames of a workspace worktender did not name", n)
	}
}

func TestStartNamesTheWorkspaceTabAndPane(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server := fakeStart(t, repo, "w9", "w9:p1", "working")
	for _, m := range []string{"workspace.rename", "tab.rename", "pane.rename"} {
		server.HandleResult(m, map[string]any{"type": "ok"})
	}
	herdrtest.FakeGh(t, `cat <<'JSON'
{"number":42,"title":"Fix the thing","body":"it is broken"}
JSON`)

	if err := startCommand([]string{"42"}, &strings.Builder{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	want := map[string]string{"workspace.rename": "#42 Fix the thing", "tab.rename": "worker", "pane.rename": "worker"}
	for method, label := range want {
		calls := calledWith(server, method)
		if len(calls) != 1 || !strings.HasSuffix(calls[0].Params["label"].(string), label) {
			t.Errorf("%s calls = %v, want label ending %q", method, calls, label)
		}
	}
}

// A rename is cosmetic; a herdr that refuses it must not undo a start.
func TestStartSurvivesAFailedRename(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server := fakeStart(t, repo, "w9", "w9:p1", "working")
	for _, m := range []string{"workspace.rename", "tab.rename", "pane.rename"} {
		server.Handle(m, func(map[string]any) (any, error) {
			return nil, &herdrtest.CodedError{Code: "nope", Message: "no"}
		})
	}
	herdrtest.FakeGh(t, `cat <<'JSON'
{"number":42,"title":"Fix the thing","body":"it is broken"}
JSON`)

	if err := startCommand([]string{"42"}, &strings.Builder{}); err != nil {
		t.Fatalf("a failed rename failed the start: %v", err)
	}
}
