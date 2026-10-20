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

// The title and the repo name are third-party text and end up in a tab bar.
func TestWorkspaceNameEscapesAndCapsEveryPart(t *testing.T) {
	got := workspaceName("re\u202epo\n"+strings.Repeat("r", 100), 1, "evil\n\u202etitle "+strings.Repeat("x", 100))
	if strings.ContainsFunc(got, func(r rune) bool { return r == '\n' || r == '\u202e' }) {
		t.Errorf("name %q holds a control or bidi rune", got)
	}
	if n := utf8.RuneCountInString(got); n > nameRepoMax+nameTitleMax+len(" #1 ") {
		t.Errorf("name is %d runes, over the cap", n)
	}
}

// The cut is made on the raw title, so an unsafe rune at the cut is escaped
// whole or dropped whole, never left as the front of an escape.
func TestWorkspaceNameNeverCutsInsideAnEscape(t *testing.T) {
	for pad := 0; pad < 4; pad++ {
		title := strings.Repeat("x", nameTitleMax-2+pad) + strings.Repeat("\u202e", 5)
		got := workspaceName("r", 1, title)
		for _, frag := range strings.Split(got, `\u{`)[1:] {
			if !strings.HasPrefix(frag, "202E}") {
				t.Errorf("pad %d: %q holds a broken escape", pad, got)
			}
		}
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
	for _, name := range []string{"scratch", "notes on #12 and more", "my work #3", "! todo"} {
		if got, ok := statusName(name, report{status: "done"}); ok {
			t.Errorf("renamed %q, which worktender never named, to %q", name, got)
		}
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
	assertRenamed(t, server, map[string]string{"workspace.rename": "#42 Fix the thing", "tab.rename": "worker", "pane.rename": "worker"})
}

// assertRenamed fails unless each method was called once with a label ending
// in the one given.
func assertRenamed(t *testing.T, server *herdrtest.Server, want map[string]string) {
	t.Helper()
	for method, label := range want {
		calls := calledWith(server, method)
		if len(calls) != 1 || !strings.HasSuffix(calls[0].Params["label"].(string), label) {
			t.Errorf("%s calls = %v, want one with a label ending %q", method, calls, label)
		}
	}
}

func refuseRenames(server *herdrtest.Server) {
	for _, m := range []string{"workspace.rename", "tab.rename", "pane.rename"} {
		server.Handle(m, func(map[string]any) (any, error) {
			return nil, &herdrtest.CodedError{Code: "nope", Message: "no"}
		})
	}
}

// A rename is cosmetic; a herdr that refuses it must not undo a start.
func TestStartSurvivesAFailedRename(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server := fakeStart(t, repo, "w9", "w9:p1", "working")
	refuseRenames(server)
	herdrtest.FakeGh(t, `cat <<'JSON'
{"number":42,"title":"Fix the thing","body":"it is broken"}
JSON`)

	if err := startCommand([]string{"42"}, &strings.Builder{}); err != nil {
		t.Fatalf("a failed rename failed the start: %v", err)
	}
	// Without these the test passes with the renames never wired.
	for _, m := range []string{"workspace.rename", "tab.rename", "pane.rename"} {
		if n := len(calledWith(server, m)); n != 1 {
			t.Errorf("%s was called %d times, want the one refused attempt", m, n)
		}
	}
}

// The report is delivered before the workspace is relabelled, and a herdr that
// refuses the relabel must not turn a delivered report into a failed one.
func TestAReportSurvivesAFailedRelabel(t *testing.T) {
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
			"pane_id": "w1:p1", "workspace_id": "w1", "tab_id": "w1:t1", "terminal_id": "term_1",
			"agent_status": "working", "focused": false, "revision": 1, "tokens": tokens}}, nil
	})
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{{
		"workspace_id": "w1", "number": 1, "label": "repo #9 title", "focused": false,
		"pane_count": 1, "tab_count": 1, "active_tab_id": "w1:t1", "agent_status": "idle"}}})
	refuseRenames(server)
	t.Setenv(socketEnv, server.SocketPath)
	t.Setenv(paneEnv, "w1:p1")

	missing, err := deliverReport(report{status: "done", pr: 14, note: "green"})
	if err != nil || missing != "" {
		t.Fatalf("deliverReport = %q, %v; a refused relabel failed a delivered report", missing, err)
	}
	if n := len(calledWith(server, "workspace.rename")); n != 1 {
		t.Errorf("workspace.rename was called %d times, want the one refused attempt", n)
	}
}

func TestApprovePaneNamesItsPane(t *testing.T) {
	repo, server := approvePaneServer(t, "feat/42-fix-the-thing", "w9")
	newFakeGitHub(t, readyMeta, snapshotJSON(t, readyTitle, readyBody), nil)
	server.HandleResult("pane.rename", map[string]any{"type": "ok"})

	if err := approveCommand([]string{"42", "--pane", "--repo", repo.Root}, &strings.Builder{}); err != nil {
		t.Fatalf("approve --pane: %v", err)
	}
	calls := calledWith(server, "pane.rename")
	if len(calls) != 1 || calls[0].Params["pane_id"] != "w9:p2" || calls[0].Params["label"] != "approve" {
		t.Errorf("pane.rename calls = %v, want w9:p2 named approve", calls)
	}
}

func TestApprovePaneSurvivesAFailedRename(t *testing.T) {
	repo, server := approvePaneServer(t, "feat/42-fix-the-thing", "w9")
	newFakeGitHub(t, readyMeta, snapshotJSON(t, readyTitle, readyBody), nil)
	refuseRenames(server)

	if err := approveCommand([]string{"42", "--pane", "--repo", repo.Root}, &strings.Builder{}); err != nil {
		t.Fatalf("a failed rename failed approve --pane: %v", err)
	}
	if n := len(calledWith(server, "pane.rename")); n != 1 {
		t.Errorf("pane.rename was called %d times, want the one refused attempt", n)
	}
	if n := len(calledWith(server, "pane.send_text")); n != 1 {
		t.Errorf("the command was typed %d times after a refused rename, want once", n)
	}
}

func TestReviewNamesItsPane(t *testing.T) {
	server, _, _ := fakeReview(t, greenPR)
	server.HandleResult("pane.rename", map[string]any{"type": "ok"})
	stubReviewAwait(t, gateOutcomeReleased, &report{status: "done", pr: 17, note: "pass: all three checks met"}, nil)

	if err := reviewCommand([]string{"17", "--json"}, &strings.Builder{}); err != nil {
		t.Fatalf("review: %v", err)
	}
	calls := calledWith(server, "pane.rename")
	if len(calls) != 1 || calls[0].Params["pane_id"] != "w9:p2" || calls[0].Params["label"] != "reviewer" {
		t.Errorf("pane.rename calls = %v, want w9:p2 named reviewer", calls)
	}
}

func TestReviewSurvivesAFailedRename(t *testing.T) {
	server, _, _ := fakeReview(t, greenPR)
	refuseRenames(server)
	stubReviewAwait(t, gateOutcomeReleased, &report{status: "done", pr: 17, note: "pass: all three checks met"}, nil)

	if err := reviewCommand([]string{"17", "--json"}, &strings.Builder{}); err != nil {
		t.Fatalf("a failed rename failed review: %v", err)
	}
	if n := len(calledWith(server, "pane.rename")); n != 1 {
		t.Errorf("pane.rename was called %d times, want the one refused attempt", n)
	}
}
