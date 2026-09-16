package wt_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/herdrtest"
	"github.com/steig/worktender/internal/wt"
)

func ptr(s string) *string { return &s }

// agentList is an agent.list result carrying one agent per pane, each stamped
// with the state counter given. The other fields are the ones herdr requires,
// not ones this plugin reads.
func agentList(seqs map[string]uint64) map[string]any {
	agents := make([]map[string]any, 0, len(seqs))
	for pane, seq := range seqs {
		workspace, _, _ := strings.Cut(pane, ":")
		agents = append(agents, map[string]any{
			"pane_id": pane, "workspace_id": workspace, "tab_id": workspace + ":t1",
			"terminal_id": "term-" + pane, "focused": false, "revision": 1,
			"agent_status": "idle", "state_change_seq": seq,
		})
	}
	return map[string]any{"type": "agent_list", "agents": agents}
}

func TestRowsJoinsWorkspaceAgentStatus(t *testing.T) {
	worktrees := &herdrapi.WorktreeListResponse{Worktrees: []herdrapi.WorktreeInfo{
		{Path: "/repo", Branch: ptr("main"), IsLinkedWorktree: false},
		{Path: "/repo/.claude/worktrees/fix-auth", Branch: ptr("fix-auth"),
			IsLinkedWorktree: true, OpenWorkspaceID: ptr("w2")},
	}}
	workspaces := &herdrapi.WorkspaceListResponse{Workspaces: []herdrapi.WorkspaceInfo{
		{WorkspaceID: "w2", AgentStatus: herdrapi.AgentStatusWorking},
	}}

	rows := wt.Rows(worktrees, workspaces)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}

	if !rows[0].Main {
		t.Error("first worktree is the main checkout, want Main=true")
	}
	if rows[0].WorkspaceID != "" || rows[0].AgentStatus != "" {
		t.Errorf("unopened worktree should have no workspace/status, got %+v", rows[0])
	}

	// Rows joins two herdr calls and no more: the pane and pull request columns
	// each cost their own lookup, so they are left empty for WithPanes and
	// WithPRs to fill.
	want := wt.Row{Main: false, Branch: "fix-auth", WorkspaceID: "w2",
		AgentStatus: "working", Dir: "fix-auth"}
	if rows[1] != want {
		t.Errorf("row 1:\n got %+v\nwant %+v", rows[1], want)
	}
}

// A worktree can point at a workspace herdr has already closed. The status must
// degrade to absent rather than reporting a stale or zero-valued agent state.
func TestRowsHandlesStaleWorkspaceReference(t *testing.T) {
	worktrees := &herdrapi.WorktreeListResponse{Worktrees: []herdrapi.WorktreeInfo{
		{Path: "/repo/wt/gone", Branch: ptr("gone"), IsLinkedWorktree: true,
			OpenWorkspaceID: ptr("w-closed")},
	}}
	workspaces := &herdrapi.WorkspaceListResponse{}

	rows := wt.Rows(worktrees, workspaces)
	if rows[0].AgentStatus != "" {
		t.Errorf("stale workspace ref should yield no status, got %q", rows[0].AgentStatus)
	}
	if rows[0].WorkspaceID != "w-closed" {
		t.Errorf("workspace id should still be shown, got %q", rows[0].WorkspaceID)
	}
}

func TestRowsDetachedHeadHasNoBranch(t *testing.T) {
	worktrees := &herdrapi.WorktreeListResponse{Worktrees: []herdrapi.WorktreeInfo{
		{Path: "/repo/wt/detached", Branch: nil, IsDetached: true, IsLinkedWorktree: true},
	}}

	rows := wt.Rows(worktrees, nil)
	if rows[0].Branch != "" {
		t.Errorf("detached worktree has no branch, got %q", rows[0].Branch)
	}
}

func TestRenderAlignsColumns(t *testing.T) {
	var buf bytes.Buffer
	err := wt.Render(&buf, []wt.Row{
		{Main: true, Branch: "main", Dir: "repo"},
		{Branch: "a-much-longer-branch", WorkspaceID: "w2", PaneID: "p1", AgentStatus: "idle", Dir: "wt"},
	}, wt.Columns{})
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), buf.String())
	}
	if !strings.HasPrefix(lines[0], "*") {
		t.Errorf("main checkout should be marked with *, got %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], " ") {
		t.Errorf("linked worktree should not be marked, got %q", lines[1])
	}
	// Alignment is the reason tabwriter is here at all.
	if strings.Index(lines[0], "-") != strings.Index(lines[1], "w2") {
		t.Errorf("columns not aligned:\n%q\n%q", lines[0], lines[1])
	}
}

// `start` derives the directory from the branch, so for every worktree this
// plugin made the two columns held the same string — and it is the widest
// string in the table. The default listing overflowed 120 columns on the
// duplication alone, before any optional column was asked for.
func TestRenderDoesNotPrintTheBranchNameTwice(t *testing.T) {
	branch := "133-ls-prints-the-branch-name-twice"

	var buf bytes.Buffer
	if err := wt.Render(&buf, []wt.Row{
		{Branch: branch, WorkspaceID: "w2", PaneID: "w2:p1", AgentStatus: "working", Dir: branch},
	}, wt.Columns{}); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	if strings.Count(out, branch) != 1 {
		t.Errorf("the branch name should appear once, got %d:\n%s",
			strings.Count(out, branch), out)
	}
	if !strings.Contains(out, branch) {
		t.Errorf("the row must still say which branch it is:\n%s", out)
	}
}

// The column earns its place on exactly the rows where it disagrees with the
// branch. A checkout herdr adopted rather than created sits in a directory with
// no relation to its branch, and that is the row a reader most needs it for —
// so the collapse is per row, never a dropped column.
func TestRenderKeepsADirectoryThatIsNotTheBranch(t *testing.T) {
	var buf bytes.Buffer
	if err := wt.Render(&buf, []wt.Row{
		{Branch: "133-ls-prints-the-branch-name-twice", Dir: "133-ls-prints-the-branch-name-twice"},
		{Branch: "worktree/brave-valley", Dir: "brave-valley-66f8"},
	}, wt.Columns{}); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), buf.String())
	}
	if !strings.Contains(lines[1], "brave-valley-66f8") {
		t.Errorf("an adopted checkout must still print its directory: %q", lines[1])
	}
	if !strings.HasSuffix(strings.TrimRight(lines[0], " "), "-") {
		t.Errorf("the collapsed row should end in the absent-cell dash: %q", lines[0])
	}
}

// git refuses ASCII control characters in a ref name but accepts a bidi
// override, so `evil<U+202E>hctap` is a branch anyone who can open a pull
// request can create — and a terminal draws it as `evilpatch`. The row is
// escaped rather than dropped: this listing is what a human reads before
// pruning, so a worktree missing from it is the same hiding by another route.
func TestRenderEscapesABranchNameThatDrawsAsAnother(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	repo.Git("branch", "--", "evil‮hctap")

	branch := repo.Git("branch", "--list", "--format=%(refname:short)", "evil‮hctap")
	if !strings.ContainsRune(branch, '‮') {
		t.Fatalf("git dropped the override, so there is nothing to defend against: %q", branch)
	}

	var buf bytes.Buffer
	if err := wt.Render(&buf, []wt.Row{
		{Branch: branch, Dir: "wt"},
	}, wt.Columns{}); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	if strings.ContainsRune(out, '‮') {
		t.Errorf("the override reached the terminal: %q", out)
	}
	if !strings.Contains(out, `evil\u{202E}hctap`) {
		t.Errorf("the row must still say which branch it is, got %q", out)
	}
}

// End to end over the wire: real git worktrees on disk, a fake herdr answering
// the same NDJSON protocol as the real one.
func TestLsAgainstFakeHerdr(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("fix-auth", "fix-auth")

	server := herdrtest.NewServer(t)
	server.HandleResult("worktree.list", map[string]any{
		"type":   "worktree_list",
		"source": map[string]any{"repo_key": "k", "repo_name": "repo", "repo_root": repo.Root, "source_checkout_path": repo.Root},
		"worktrees": []map[string]any{
			{"path": repo.Root, "label": "repo", "is_bare": false, "is_detached": false,
				"is_prunable": false, "is_linked_worktree": false, "branch": "main"},
			{"path": checkout, "label": "fix-auth", "is_bare": false, "is_detached": false,
				"is_prunable": false, "is_linked_worktree": true, "branch": "fix-auth",
				"open_workspace_id": "w2"},
		},
	})
	server.HandleResult("workspace.list", map[string]any{
		"type": "workspace_list",
		"workspaces": []map[string]any{
			{"workspace_id": "w2", "number": 2, "label": "fix-auth", "focused": true,
				"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "working"},
		},
	})

	server.HandleResult("pane.list", map[string]any{
		"type": "pane_list",
		"panes": []map[string]any{
			{"pane_id": "w2:p1", "workspace_id": "w2", "tab_id": "t1", "index": 0},
		},
	})
	server.HandleResult("agent.list", agentList(map[string]uint64{"w2:p1": 412}))

	var buf bytes.Buffer
	client := herdrapi.NewWithSocket(server.SocketPath)
	// Called from inside the linked worktree: the listing must still cover the
	// whole repository, which is what RepoRoot's --git-common-dir is for.
	if err := wt.Ls(client, "", checkout, nil, wt.Options{}, &buf); err != nil {
		t.Fatalf("Ls: %v", err)
	}

	out := buf.String()
	// The pane is here because it is what `dispatch --pane` takes; a listing
	// that stops at the workspace leaves that step with nowhere to get it.
	// The counter is here because "working" carries no time and this is the only
	// thing herdr has that moves when the worker does.
	for _, want := range []string{"main", "fix-auth", "w2", "w2:p1", "working", "412"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// No lookup was passed, so gh was not consulted and the column is absent
	// rather than dashed — a "-" there reads as "no pull request".
	if strings.Contains(out, "OPEN") || strings.Contains(out, "MERGED") {
		t.Errorf("pull request state must not appear unless asked for:\n%s", out)
	}

	calls := server.Calls()
	if len(calls) != 4 {
		t.Fatalf("want 4 calls, got %d: %+v", len(calls), calls)
	}
	if calls[0].Method != "worktree.list" || calls[1].Method != "workspace.list" ||
		calls[2].Method != "pane.list" || calls[3].Method != "agent.list" {
		t.Errorf("unexpected methods: %+v", calls)
	}
	// The cwd must be the MAIN checkout, not the linked worktree we ran from.
	if got := calls[0].Params["cwd"]; got != repo.RealRoot {
		t.Errorf("worktree.list cwd = %v, want repo root %s", got, repo.RealRoot)
	}
}

// fleet is two repositories herdr has worktree workspaces for, one of which it
// cannot read. It is the shape `--all-repos` exists for, and the shape that
// makes the failure rule matter.
func fleet(t *testing.T) (*herdrtest.Server, *herdrtest.Repo, string) {
	t.Helper()

	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("77-cross-repo", "77-cross-repo")
	const unreadable = "/nowhere/lighthouse"

	server := herdrtest.NewServer(t)
	server.Handle("worktree.list", func(params map[string]any) (any, error) {
		if params["cwd"] == unreadable {
			return nil, errors.New("not a git repository")
		}
		return map[string]any{
			"type":   "worktree_list",
			"source": map[string]any{"repo_key": "k", "repo_name": "repo", "repo_root": repo.Root, "source_checkout_path": repo.Root},
			"worktrees": []map[string]any{
				{"path": repo.Root, "label": "repo", "is_bare": false, "is_detached": false,
					"is_prunable": false, "is_linked_worktree": false, "branch": "main"},
				{"path": checkout, "label": "77-cross-repo", "is_bare": false, "is_detached": false,
					"is_prunable": false, "is_linked_worktree": true, "branch": "77-cross-repo",
					"open_workspace_id": "w2"},
			},
		}, nil
	})
	server.HandleResult("workspace.list", map[string]any{
		"type": "workspace_list",
		"workspaces": []map[string]any{
			{"workspace_id": "w2", "number": 2, "label": "77-cross-repo", "focused": false,
				"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "blocked"},
		},
	})
	server.HandleResult("pane.list", map[string]any{
		"type":  "pane_list",
		"panes": []map[string]any{{"pane_id": "w2:p1", "workspace_id": "w2", "tab_id": "t1", "index": 0}},
	})
	server.HandleResult("agent.list", agentList(map[string]uint64{"w2:p1": 412}))
	return server, repo, unreadable
}

// The whole complaint #77 makes: someone with agents in six repositories has to
// visit six directories to see them. One listing covers all of them — and one
// repository failing must not cost the others their rows, which is the rule
// doctor already follows.
func TestLsAllGroupsEveryRepositoryAndKeepsGoingPastAFailure(t *testing.T) {
	server, repo, unreadable := fleet(t)

	var buf bytes.Buffer
	client := herdrapi.NewWithSocket(server.SocketPath)
	if err := wt.LsAll(client, []string{repo.RealRoot, unreadable}, wt.Options{}, &buf); err != nil {
		t.Fatalf("LsAll: %v", err)
	}

	out := buf.String()
	for _, want := range []string{repo.RealRoot, "77-cross-repo", "w2:p1", "blocked", unreadable, "not a git repository"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// The full root rather than the basename: two checkouts on one machine
	// routinely share one, and this listing is read across all of them.
	if !strings.Contains(out, repo.RealRoot+"\n") {
		t.Errorf("the repository heading must be the root on a line of its own:\n%s", out)
	}
	// Rows belong to the repository above them, so they are indented under it.
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, "77-cross-repo") && !strings.HasPrefix(line, "  ") {
			t.Errorf("row is not indented under its repository: %q", line)
		}
	}
}

// `--blocked` across six repositories is a question with a short answer, and
// five headings with nothing under them bury it. A repository that could not be
// read is never skipped: that is the row the reader most needs.
func TestLsAllBlockedSkipsQuietRepositoriesButNeverAFailedOne(t *testing.T) {
	server, repo, unreadable := fleet(t)
	// Nothing is blocked now, so the readable repository has nothing to show.
	server.HandleResult("workspace.list", map[string]any{
		"type": "workspace_list",
		"workspaces": []map[string]any{
			{"workspace_id": "w2", "number": 2, "label": "77-cross-repo", "focused": false,
				"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "working"},
		},
	})

	var buf bytes.Buffer
	client := herdrapi.NewWithSocket(server.SocketPath)
	if err := wt.LsAll(client, []string{repo.RealRoot, unreadable}, wt.Options{Blocked: true}, &buf); err != nil {
		t.Fatalf("LsAll: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, repo.RealRoot) {
		t.Errorf("a repository with nothing blocked should not be drawn as an empty heading:\n%s", out)
	}
	if !strings.Contains(out, unreadable) || !strings.Contains(out, "not a git repository") {
		t.Errorf("a repository that could not be read must still be named:\n%s", out)
	}
}

// Nothing blocked anywhere has to say so. Silence is the same output as a
// listing that reached no repositories at all.
func TestLsAllBlockedSaysNothingIsBlockedRatherThanPrintingNothing(t *testing.T) {
	server, repo, _ := fleet(t)
	server.HandleResult("workspace.list", map[string]any{
		"type": "workspace_list", "workspaces": []map[string]any{},
	})

	var buf bytes.Buffer
	client := herdrapi.NewWithSocket(server.SocketPath)
	if err := wt.LsAll(client, []string{repo.RealRoot}, wt.Options{Blocked: true}, &buf); err != nil {
		t.Fatalf("LsAll: %v", err)
	}

	if got, want := buf.String(), "no blocked agents in 1 repository\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// herdr having nothing open is a different answer from every repository being
// quiet, and both are different from a listing that failed.
func TestLsAllSaysWhenHerdrHasNothingOpen(t *testing.T) {
	server := herdrtest.NewServer(t)

	var buf bytes.Buffer
	client := herdrapi.NewWithSocket(server.SocketPath)
	if err := wt.LsAll(client, nil, wt.Options{}, &buf); err != nil {
		t.Fatalf("LsAll: %v", err)
	}

	if !strings.Contains(buf.String(), "no worktree workspaces open") {
		t.Errorf("got %q", buf.String())
	}
	// Not one call: with no repositories to read there is nothing to ask about.
	if calls := server.Calls(); len(calls) != 0 {
		t.Errorf("herdr was asked %d question(s) about no repositories: %+v", len(calls), calls)
	}
}

// #90's complaint, in one assertion: `idle` is a point-in-time enum and the
// same cell for a worker that finished two seconds ago and one that never
// received its brief at all. The counter is what the two rows no longer share.
func TestTheCounterTellsTwoIdleWorkersApart(t *testing.T) {
	finished, stalled := uint64(1057), uint64(812)
	rows := []wt.Row{
		{Branch: "just-finished", PaneID: "w1:p1", AgentStatus: "idle", AgentStatusSeq: &finished, Dir: "a"},
		{Branch: "never-started", PaneID: "w2:p1", AgentStatus: "idle", AgentStatusSeq: &stalled, Dir: "b"},
	}

	var buf bytes.Buffer
	if err := wt.Render(&buf, rows, wt.Columns{}); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %q", buf.String())
	}
	if !strings.Contains(lines[0], "1057") || !strings.Contains(lines[1], "812") {
		t.Errorf("each row must carry its own counter:\n%s", buf.String())
	}

	// Both rows say `idle`, so before this the two lines were the same listing
	// twice over. The counter is the whole distinction, and a JSON consumer must
	// get it as a number rather than have to parse the column back out.
	got := wt.JSON(rows)
	if got[0].AgentStatusSeq == nil || got[1].AgentStatusSeq == nil ||
		*got[0].AgentStatusSeq == *got[1].AgentStatusSeq {
		t.Errorf("the two idle rows are still indistinguishable: %+v", got)
	}
}

// The same assertion pointed the other way, and the answer is the opposite one
// (#112). Two `working` rows stuck on one counter — one thinking, one wedged —
// are the same listing twice over, because the counter stamps state *changes*
// and neither worker is changing state. Measured on a live agent: fifteen
// minutes of continuous work, the counter frozen throughout, and every other
// number herdr has for that pane frozen with it.
//
// Pinned rather than fixed, because the fix is not here: nothing in a herdr
// listing separates those two, and what does — cumulative spend — exists only
// as characters drawn in the pane. This fails the day a row can tell them
// apart, which is the day the documented limit stops being true.
func TestTwoWorkingRowsOnOneCounterAreIndistinguishable(t *testing.T) {
	frozen := uint64(1213)
	thinking := wt.Row{Branch: "long-turn", WorkspaceID: "w1", PaneID: "w1:p1",
		AgentStatus: "working", AgentStatusSeq: &frozen, Dir: "a"}
	wedged := wt.Row{Branch: "wedged", WorkspaceID: "w2", PaneID: "w2:p1",
		AgentStatus: "working", AgentStatusSeq: &frozen, Dir: "b"}

	// Everything the two rows do not share is who they are rather than how they
	// are: blank that, and what is left is everything a consumer could read the
	// worker's health off.
	health := func(row wt.Row) string {
		row.Branch, row.WorkspaceID, row.PaneID, row.Dir = "", "", "", ""
		raw, err := json.Marshal(wt.JSON([]wt.Row{row}))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	if alive, stopped := health(thinking), health(wedged); alive != stopped {
		t.Errorf("a listing now separates a long turn from a wedge, so docs/json.md"+
			" and the coordinator skill are wrong:\n%s\n%s", alive, stopped)
	}
}

// The counter is session-wide, so it is one question no matter how many
// repositories are being listed — the rule AllRows already follows for the
// workspace list.
func TestWithAgentSeqsAsksOnceForEveryListing(t *testing.T) {
	server := herdrtest.NewServer(t)
	server.HandleResult("agent.list", agentList(map[string]uint64{"w2:p1": 7, "w9:p1": 91}))

	first := []wt.Row{{Branch: "a", PaneID: "w2:p1"}}
	second := []wt.Row{{Branch: "b", PaneID: "w9:p1"}, {Branch: "unstaffed", PaneID: "w3:p1"}}

	wt.WithAgentSeqs(herdrapi.NewWithSocket(server.SocketPath), first, second)

	if calls := server.Calls(); len(calls) != 1 || calls[0].Method != "agent.list" {
		t.Fatalf("want one agent.list for both listings, got %+v", calls)
	}
	if first[0].AgentStatusSeq == nil || *first[0].AgentStatusSeq != 7 {
		t.Errorf("first listing: %+v", first[0])
	}
	if second[0].AgentStatusSeq == nil || *second[0].AgentStatusSeq != 91 {
		t.Errorf("second listing: %+v", second[0])
	}
	// A pane herdr has no agent in gets no counter rather than a zero, which
	// would read as an agent that has never done anything.
	if second[1].AgentStatusSeq != nil {
		t.Errorf("a pane with no agent has no counter, got %d", *second[1].AgentStatusSeq)
	}
}

// A listing with no panes in it has nothing to decorate, and asking anyway is a
// round trip spent on an answer with nowhere to go.
func TestWithAgentSeqsAsksNothingWhenNoRowHasAPane(t *testing.T) {
	server := herdrtest.NewServer(t)
	server.HandleResult("agent.list", agentList(map[string]uint64{"w2:p1": 7}))

	wt.WithAgentSeqs(herdrapi.NewWithSocket(server.SocketPath), []wt.Row{{Branch: "unopened"}})

	if calls := server.Calls(); len(calls) != 0 {
		t.Errorf("herdr was asked about a listing with no panes: %+v", calls)
	}
}

// The counter is one column. Losing it must cost the listing nothing else —
// unlike the workspace list, whose failure has to fail the command, because a
// degraded status column reads as a session with nothing open.
func TestLsKeepsItsRowsWhenTheCounterCannotBeRead(t *testing.T) {
	server, repo, _ := fleet(t)
	server.Handle("agent.list", func(map[string]any) (any, error) {
		return nil, errors.New("herdr is not answering")
	})

	var buf bytes.Buffer
	client := herdrapi.NewWithSocket(server.SocketPath)
	if err := wt.Ls(client, repo.RealRoot, "", nil, wt.Options{}, &buf); err != nil {
		t.Fatalf("Ls: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "77-cross-repo") || !strings.Contains(out, "w2:p1") {
		t.Errorf("the listing lost its rows over one missing column:\n%s", out)
	}
	if strings.Contains(out, "412") {
		t.Errorf("a counter appeared from a lookup that failed:\n%s", out)
	}
}

// blockedRows is the fixture both filter tests read: one of each status herdr
// can report, so a filter that keeps too much is caught by what it kept.
var blockedRows = []wt.Row{
	{Branch: "working", AgentStatus: "working"},
	{Branch: "stuck", AgentStatus: "blocked"},
	{Branch: "idle", AgentStatus: "idle"},
	{Branch: "finished", AgentStatus: "done"},
	{Branch: "no-agent"},
}

func TestOnlyBlockedKeepsHerdrsBlockedAndNothingElse(t *testing.T) {
	kept := wt.OnlyBlocked(blockedRows)

	if len(kept) != 1 || kept[0].Branch != "stuck" {
		t.Fatalf("want only the blocked row, got %+v", kept)
	}
}

// The filter has to run before the lookups, not after. A pane is a round trip
// per workspace, and the whole point of `--blocked` is that it is the question
// you ask across everything you have open.
func TestLsBlockedAsksForNoPaneItWillNotPrint(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	working := repo.AddWorktree("working", "working")
	stuck := repo.AddWorktree("stuck", "stuck")

	server := herdrtest.NewServer(t)
	server.HandleResult("worktree.list", map[string]any{
		"type":   "worktree_list",
		"source": map[string]any{"repo_key": "k", "repo_name": "repo", "repo_root": repo.Root, "source_checkout_path": repo.Root},
		"worktrees": []map[string]any{
			{"path": working, "label": "working", "is_bare": false, "is_detached": false,
				"is_prunable": false, "is_linked_worktree": true, "branch": "working",
				"open_workspace_id": "w2"},
			{"path": stuck, "label": "stuck", "is_bare": false, "is_detached": false,
				"is_prunable": false, "is_linked_worktree": true, "branch": "stuck",
				"open_workspace_id": "w3"},
		},
	})
	server.HandleResult("workspace.list", map[string]any{
		"type": "workspace_list",
		"workspaces": []map[string]any{
			{"workspace_id": "w2", "number": 2, "label": "working", "focused": false,
				"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "working"},
			{"workspace_id": "w3", "number": 3, "label": "stuck", "focused": false,
				"pane_count": 1, "tab_count": 1, "active_tab_id": "t2", "agent_status": "blocked"},
		},
	})
	server.HandleResult("pane.list", map[string]any{
		"type":  "pane_list",
		"panes": []map[string]any{{"pane_id": "w3:p1", "workspace_id": "w3", "tab_id": "t2", "index": 0}},
	})

	var buf bytes.Buffer
	client := herdrapi.NewWithSocket(server.SocketPath)
	if err := wt.Ls(client, repo.RealRoot, "", nil, wt.Options{Blocked: true}, &buf); err != nil {
		t.Fatalf("Ls: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "stuck") || !strings.Contains(out, "w3:p1") {
		t.Errorf("the blocked worktree and the pane that can restart it must both be there:\n%s", out)
	}
	if strings.Contains(out, "working") {
		t.Errorf("a working agent is not blocked:\n%s", out)
	}

	var panes []herdrtest.Call
	for _, call := range server.Calls() {
		if call.Method == "pane.list" {
			panes = append(panes, call)
		}
	}
	if len(panes) != 1 {
		t.Fatalf("want one pane lookup, got %d: %+v", len(panes), panes)
	}
	if got := panes[0].Params["workspace_id"]; got != "w3" {
		t.Errorf("pane looked up for workspace %v, want the blocked one w3", got)
	}
}

// Nothing blocked is an answer, and an empty table is not how to give it: it is
// the same output as a listing that reached nothing at all.
func TestLsBlockedSaysSoWhenNothingIsBlocked(t *testing.T) {
	repo := herdrtest.NewRepo(t)

	server := herdrtest.NewServer(t)
	server.HandleResult("worktree.list", map[string]any{
		"type":   "worktree_list",
		"source": map[string]any{"repo_key": "k", "repo_name": "repo", "repo_root": repo.Root, "source_checkout_path": repo.Root},
		"worktrees": []map[string]any{
			{"path": repo.Root, "label": "repo", "is_bare": false, "is_detached": false,
				"is_prunable": false, "is_linked_worktree": false, "branch": "main"},
		},
	})
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})

	var buf bytes.Buffer
	client := herdrapi.NewWithSocket(server.SocketPath)
	if err := wt.Ls(client, repo.RealRoot, "", nil, wt.Options{Blocked: true}, &buf); err != nil {
		t.Fatalf("Ls: %v", err)
	}

	if !strings.Contains(buf.String(), "no blocked agents") {
		t.Errorf("a filter that matched nothing must say so, got %q", buf.String())
	}
}

// A workspace list failure must fail the command. Degrading the status column
// to "-" would render a herdr that did not answer as a session with no
// workspaces at all, which reads as an instruction to run sync.
func TestLsFailsWhenWorkspaceListFails(t *testing.T) {
	repo := herdrtest.NewRepo(t)

	server := herdrtest.NewServer(t)
	server.HandleResult("worktree.list", map[string]any{
		"type":   "worktree_list",
		"source": map[string]any{"repo_key": "k", "repo_name": "repo", "repo_root": repo.Root, "source_checkout_path": repo.Root},
		"worktrees": []map[string]any{
			{"path": repo.Root, "label": "repo", "is_bare": false, "is_detached": false,
				"is_prunable": false, "is_linked_worktree": false, "branch": "main"},
		},
	})
	// workspace.list deliberately unhandled -> server returns an error.

	var buf bytes.Buffer
	client := herdrapi.NewWithSocket(server.SocketPath)
	if err := wt.Ls(client, "", repo.Root, nil, wt.Options{}, &buf); err == nil {
		t.Fatal("Ls should fail when the workspace list fails")
	}
	if buf.Len() != 0 {
		t.Errorf("no listing should be printed when the status is unknown:\n%s", buf.String())
	}
}

// worktreeList and workspaceList build the two herdr replies Rows joins.
func worktreeList(root string, paths ...string) *herdrapi.WorktreeListResponse {
	resp := &herdrapi.WorktreeListResponse{
		Source: herdrapi.WorktreeSourceInfo{RepoRoot: root, SourceCheckoutPath: root},
	}
	for _, p := range paths {
		branch := filepath.Base(p)
		resp.Worktrees = append(resp.Worktrees, herdrapi.WorktreeInfo{
			Path: p, Branch: &branch, IsLinkedWorktree: true,
		})
	}
	return resp
}

func workspaceOn(id, repoRoot, checkout string) herdrapi.WorkspaceInfo {
	return herdrapi.WorkspaceInfo{
		WorkspaceID: id, AgentStatus: "unknown",
		Worktree: &herdrapi.WorkspaceWorktreeInfo{
			RepoRoot: repoRoot, CheckoutPath: checkout, IsLinkedWorktree: true,
		},
	}
}

// A workspace can outlive its checkout, and a listing built only from git's
// worktrees cannot see that: the row is simply absent, which is the output a
// clean repository gets. This listing exists to say which of the things on disk
// are real, so the one state it could not name was the one it is for.
func TestRowsReportsAWorkspaceWhoseCheckoutIsGone(t *testing.T) {
	rows := wt.Rows(
		worktreeList("/repo", "/repo/wt/live"),
		&herdrapi.WorkspaceListResponse{Workspaces: []herdrapi.WorkspaceInfo{
			workspaceOn("w2", "/repo", "/repo/wt/live"),
			workspaceOn("w35", "/repo", "/repo/wt/gone"),
		}},
	)

	if len(rows) != 2 {
		t.Fatalf("want the live worktree and the ghost, got %d: %+v", len(rows), rows)
	}
	ghost := rows[1]
	if !ghost.Ghost {
		t.Errorf("the workspace with no worktree should be a ghost row: %+v", ghost)
	}
	if ghost.WorkspaceID != "w35" {
		t.Errorf("the row must name the workspace, got %q", ghost.WorkspaceID)
	}
	if ghost.Branch != "" {
		t.Errorf("there is no checkout left to have a branch, got %q", ghost.Branch)
	}
	if ghost.Dir != "gone" {
		t.Errorf("the row should name the path herdr still believes in, got %q", ghost.Dir)
	}
	if rows[0].Ghost {
		t.Error("a worktree that is right there is not a ghost")
	}
}

// workspace.list is session-wide. Without the repository filter every other
// repository's worktrees arrive as ghosts of this one, which would turn a
// listing that missed three rows into one that invents a dozen.
func TestRowsDoesNotReportAnotherRepositorysWorkspacesAsGhosts(t *testing.T) {
	rows := wt.Rows(
		worktreeList("/repo", "/repo/wt/live"),
		&herdrapi.WorkspaceListResponse{Workspaces: []herdrapi.WorkspaceInfo{
			workspaceOn("w2", "/repo", "/repo/wt/live"),
			workspaceOn("w9", "/elsewhere", "/elsewhere/wt/other"),
		}},
	)

	for _, row := range rows {
		if row.Ghost {
			t.Errorf("another repository's workspace is not this one's ghost: %+v", row)
		}
	}
}

// The marker column carries it, because the table is already the widest thing
// this tool prints and a seventh column blank on every ordinary listing earns
// nothing.
func TestRenderMarksAGhostRow(t *testing.T) {
	var buf bytes.Buffer
	if err := wt.Render(&buf, []wt.Row{
		{Main: true, Branch: "main", Dir: "repo"},
		{WorkspaceID: "w35", AgentStatus: "unknown", Dir: "gone", Ghost: true},
	}, wt.Columns{}); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if !strings.HasPrefix(lines[0], "*") {
		t.Errorf("main checkout should still be marked with *, got %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "?") {
		t.Errorf("a ghost row should be marked, got %q", lines[1])
	}
	if !strings.Contains(lines[1], "gone") {
		t.Errorf("the ghost row must name the checkout herdr holds open, got %q", lines[1])
	}
}

// #179: the table printed eight columns and said what none of them were, so
// every reading of it started by counting cells against the README. The labels
// are the fix, and they have to be the columns actually drawn — a header naming
// a PR column the table left out would be worse than none.
func TestRenderLabelsTheColumnsItDraws(t *testing.T) {
	var buf bytes.Buffer
	err := wt.Render(&buf, []wt.Row{
		{Branch: "fix-auth", WorkspaceID: "w2", PaneID: "w2:p1",
			AgentStatus: "working", AgentStatusSeq: u64(412), Dir: "fix-auth-wt"},
	}, wt.Columns{Header: true})
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want a label row and one row, got %d: %q", len(lines), buf.String())
	}
	header, row := lines[0], lines[1]
	for _, want := range []string{"BRANCH", "WORKSPACE", "PANE", "STATUS", "SEQ", "DIR"} {
		if !strings.Contains(header, want) {
			t.Errorf("label row missing %q: %q", want, header)
		}
	}
	// A label in the wrong place is a wrong label. tabwriter puts it right only
	// if the header has the same cell count as the rows, which is the mistake
	// this asserts against.
	for label, value := range map[string]string{
		"BRANCH": "fix-auth", "WORKSPACE": "w2", "PANE": "w2:p1",
		"STATUS": "working", "SEQ": "412", "DIR": "fix-auth-wt",
	} {
		if strings.Index(header, label) != strings.Index(row, value) {
			t.Errorf("%s is not above %s:\n%q\n%q", label, value, header, row)
		}
	}
}

// The two optional columns are omitted rather than dashed when nobody asked for
// them, so their labels have to be too.
func TestRenderHeaderNamesOnlyTheOptionalColumnsDrawn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cols  wt.Columns
		want  []string
		avoid []string
	}{
		{"neither", wt.Columns{Header: true}, nil, []string{"REPORT", "PR"}},
		{"reports", wt.Columns{Header: true, Reports: true}, []string{"REPORT"}, []string{"PR"}},
		{"pr", wt.Columns{Header: true, PR: true}, []string{"PR"}, []string{"REPORT"}},
		{"both", wt.Columns{Header: true, Reports: true, PR: true}, []string{"REPORT", "PR"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := wt.Render(&buf, []wt.Row{{Branch: "b", Dir: "b"}}, tc.cols); err != nil {
				t.Fatal(err)
			}
			header := strings.SplitN(buf.String(), "\n", 2)[0]
			for _, want := range tc.want {
				if !strings.Contains(header, want) {
					t.Errorf("label row missing %q: %q", want, header)
				}
			}
			for _, avoid := range tc.avoid {
				if strings.Contains(header, avoid) {
					t.Errorf("label row names a column it does not draw (%q): %q", avoid, header)
				}
			}
		})
	}
}

// Anything parsing the table by line — and `--no-header` exists because such
// things exist — must get exactly what it got before the labels landed.
func TestRenderWithoutAHeaderDrawsNothingButRows(t *testing.T) {
	rows := []wt.Row{{Main: true, Branch: "main", Dir: "repo"}, {Branch: "wip", Dir: "wip"}}

	var buf bytes.Buffer
	if err := wt.Render(&buf, rows, wt.Columns{}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != len(rows) {
		t.Fatalf("want %d lines and no label row, got %d: %q", len(rows), len(lines), buf.String())
	}
	if strings.Contains(buf.String(), "BRANCH") {
		t.Errorf("no header was asked for:\n%s", buf.String())
	}
}

// Per repository rather than once at the top, and not for the aesthetics: the
// repository heading carries no tab, which ends tabwriter's column block, so a
// single header above the first heading would be aligned against nothing.
func TestRenderReposLabelsEveryGroup(t *testing.T) {
	repos := []wt.Repo{
		{Root: "/code/worktender", Rows: []wt.Row{{Branch: "main", Main: true, Dir: "worktender"}}},
		{Root: "/code/lighthouse", Rows: []wt.Row{{Branch: "main", Main: true, Dir: "lighthouse"}}},
	}

	var buf bytes.Buffer
	if err := wt.RenderRepos(&buf, repos, wt.Options{Header: true}); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	if got := strings.Count(out, "BRANCH"); got != len(repos) {
		t.Fatalf("want one label row per repository (%d), got %d:\n%s", len(repos), got, out)
	}
	// Directly under the heading it belongs to, not floating anywhere in the
	// group: a label row below a data row labels nothing.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for i, line := range lines {
		if strings.Contains(line, "BRANCH") {
			if i == 0 || !strings.HasPrefix(lines[i-1], "/code/") {
				t.Errorf("label row at line %d does not sit under a repository heading:\n%s", i, out)
			}
		}
	}
}

// A repository that could not be read has no rows, so labelling its group would
// name columns that are not there.
func TestRenderReposDoesNotLabelARepositoryItCouldNotRead(t *testing.T) {
	var buf bytes.Buffer
	err := wt.RenderRepos(&buf, []wt.Repo{{Root: "/code/lighthouse", Err: "not a git repository"}},
		wt.Options{Header: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "BRANCH") {
		t.Errorf("a failed repository has no columns to label:\n%s", buf.String())
	}
}

// The listing is watched on a refresh loop, and what it is watched for is the
// row that has stopped and needs a person. Ranked by that rather than
// alphabetically — alphabetical only happens to put blocked first, and would
// stop the moment herdr names a new status.
func TestSortStatusPutsTheRowsThatWantAPersonFirst(t *testing.T) {
	rows := []wt.Row{
		{Branch: "unstaffed"},
		{Branch: "working", AgentStatus: "working"},
		{Branch: "done", AgentStatus: "done"},
		{Branch: "idle", AgentStatus: "idle"},
		{Branch: "surprise", AgentStatus: "reticulating"},
		{Branch: "blocked", AgentStatus: "blocked"},
	}

	wt.Sort(rows, wt.SortStatus)

	want := []string{"blocked", "idle", "done", "working", "surprise", "unstaffed"}
	if got := branches(rows); !slices.Equal(got, want) {
		t.Errorf("status order:\n got %v\nwant %v", got, want)
	}
}

// The counter only ever answers "who stopped moving first", so the low end is
// the interesting one. A row with no counter has no agent to be stale and goes
// to the bottom rather than to the top with the genuinely stalled ones.
func TestSortSeqPutsTheWorkerHerdrSawMoveLongestAgoFirst(t *testing.T) {
	rows := []wt.Row{
		{Branch: "recent", AgentStatusSeq: u64(1057)},
		{Branch: "none"},
		{Branch: "stalled", AgentStatusSeq: u64(812)},
		{Branch: "middling", AgentStatusSeq: u64(1055)},
	}

	wt.Sort(rows, wt.SortSeq)

	want := []string{"stalled", "middling", "recent", "none"}
	if got := branches(rows); !slices.Equal(got, want) {
		t.Errorf("seq order:\n got %v\nwant %v", got, want)
	}
}

// A ghost and a detached head have no branch, and they are not what someone
// sorting by branch went looking for.
func TestSortBranchPutsTheBranchlessLast(t *testing.T) {
	rows := []wt.Row{
		{Branch: "zeta"},
		{Ghost: true, Dir: "brave-valley-66f8"},
		{Branch: "alpha"},
		{Branch: "main", Main: true},
	}

	wt.Sort(rows, wt.SortBranch)

	want := []string{"alpha", "main", "zeta", ""}
	if got := branches(rows); !slices.Equal(got, want) {
		t.Errorf("branch order:\n got %v\nwant %v", got, want)
	}
}

// This listing is read on a refresh loop, where a row that moves for no reason
// costs the reader the scan they were in the middle of. Ties keep git's order.
func TestSortKeepsGitsOrderForRowsThatTie(t *testing.T) {
	rows := []wt.Row{
		{Branch: "third", AgentStatus: "idle"},
		{Branch: "first", AgentStatus: "idle"},
		{Branch: "second", AgentStatus: "idle"},
	}
	before := branches(rows)

	wt.Sort(rows, wt.SortStatus)

	if got := branches(rows); !slices.Equal(got, before) {
		t.Errorf("rows on one status were reordered:\n got %v\nwant %v", got, before)
	}
}

// SortNone is the default, and the default has to be the order the listing had
// before --sort existed.
func TestSortNoneLeavesTheRowsAlone(t *testing.T) {
	rows := []wt.Row{
		{Branch: "zeta", AgentStatus: "working"},
		{Branch: "alpha", AgentStatus: "blocked"},
	}

	wt.Sort(rows, wt.SortNone)

	if got := branches(rows); !slices.Equal(got, []string{"zeta", "alpha"}) {
		t.Errorf("no sort was asked for, got %v", got)
	}
}

func TestParseSortTakesEveryFieldItAdvertises(t *testing.T) {
	for _, field := range wt.SortFields {
		got, err := wt.ParseSort(string(field))
		if err != nil {
			t.Errorf("ParseSort(%q) is advertised but refused: %v", field, err)
		}
		if got != field {
			t.Errorf("ParseSort(%q) = %q", field, got)
		}
	}
}

// An unknown field must not fall through to "git's order": that is a listing
// that silently ignored what it was asked for.
func TestParseSortNamesTheFieldsItWouldHaveTaken(t *testing.T) {
	_, err := wt.ParseSort("pid")
	if err == nil {
		t.Fatal("an unknown sort field was accepted")
	}
	for _, field := range wt.SortFields {
		if !strings.Contains(err.Error(), string(field)) {
			t.Errorf("the error should name %q: %v", field, err)
		}
	}
}

// The JSON is a view of exactly the rows the table renders, so the sort has to
// happen before the two part company — otherwise `--sort --json` is a flag that
// does nothing and says nothing about it.
func TestLsSortsTheJSONTheSameWayAsTheTable(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	for _, branch := range []string{"zeta", "alpha", "mid"} {
		repo.AddWorktree(branch, branch)
	}
	// No herdr: the branch column comes from git, which is all this sort reads.
	t.Setenv("HERDR_SOCKET_PATH", "")

	var table, doc bytes.Buffer
	opts := wt.Options{Sort: wt.SortBranch}
	if err := wt.Ls(nil, repo.RealRoot, repo.RealRoot, nil, opts, &table); err != nil {
		t.Fatalf("Ls: %v", err)
	}
	opts.JSON = true
	if err := wt.Ls(nil, repo.RealRoot, repo.RealRoot, nil, opts, &doc); err != nil {
		t.Fatalf("Ls --json: %v", err)
	}

	var listing struct {
		Worktrees []struct {
			Branch *string `json:"branch"`
		} `json:"worktrees"`
	}
	if err := json.Unmarshal(doc.Bytes(), &listing); err != nil {
		t.Fatalf("decode: %v\n%s", err, doc.String())
	}
	var fromJSON []string
	for _, row := range listing.Worktrees {
		fromJSON = append(fromJSON, *row.Branch)
	}
	want := []string{"alpha", "main", "mid", "zeta"}
	if !slices.Equal(fromJSON, want) {
		t.Errorf("json order:\n got %v\nwant %v", fromJSON, want)
	}

	// And the table agrees, which is the claim that matters: one order, two
	// renderings of it.
	var fromTable []string
	for _, line := range strings.Split(strings.TrimSpace(table.String()), "\n") {
		fields := strings.Fields(line)
		// The main checkout's marker cell is the only one with anything in it.
		if fields[0] == "*" {
			fields = fields[1:]
		}
		fromTable = append(fromTable, fields[0])
	}
	if !slices.Equal(fromTable, want) {
		t.Errorf("table order:\n got %v\nwant %v", fromTable, want)
	}
}

// branches is the branch column of every row, in order.
func branches(rows []wt.Row) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Branch)
	}
	return out
}

func u64(n uint64) *uint64 { return &n }
