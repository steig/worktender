package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/herdrtest"
)

// queued is one issue in the fake factory:ready queue.
type queued struct {
	number int
	title  string
	labels []string
}

// factoryFleet is a fake herdr with one workspace per entry of busy, each a
// linked worktree whose agent reports that status, and a fake gh whose ready
// queue is ready, listed in the order given.
//
// worktree.create makes the branch for real, as herdr would, so a second pass
// in the same test meets the branch the first one left behind. It returns the
// server and the file gh appends each `issue edit` to.
func factoryFleet(t *testing.T, repo *herdrtest.Repo, busy []string, ready []queued) (*herdrtest.Server, string) {
	t.Helper()

	server := fakeStart(t, repo, "w9", "w9:p1", "working")
	server.Handle("worktree.create", func(params map[string]any) (any, error) {
		branch, _ := params["branch"].(string)
		repo.Git("branch", branch)
		return map[string]any{
			"type": "workspace_created",
			"workspace": map[string]any{
				"workspace_id": "w9", "number": 9, "label": "issue", "focused": false,
				"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "idle",
			},
			"root_pane": map[string]any{"pane_id": "w9:p1", "workspace_id": "w9", "tab_id": "t1", "index": 0},
			"tab":       map[string]any{"tab_id": "t1", "workspace_id": "w9", "index": 0},
		}, nil
	})

	var worktrees, workspaces []map[string]any
	for i, status := range busy {
		id := fmt.Sprintf("b%d", i)
		checkout := filepath.Join(repo.Root, ".claude", "worktrees", "busy-"+id)
		worktrees = append(worktrees, map[string]any{
			"path": checkout, "label": "busy-" + id, "branch": "busy-" + id,
			"is_bare": false, "is_detached": false, "is_prunable": false,
			"is_linked_worktree": true, "open_workspace_id": id,
		})
		workspaces = append(workspaces, map[string]any{
			"workspace_id": id, "number": i + 1, "label": "wt", "focused": false,
			"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": status,
			"worktree": map[string]any{"repo_key": "k", "repo_name": "repo",
				"repo_root": repo.Root, "checkout_path": checkout, "is_linked_worktree": true},
		})
	}
	setFactoryWorktrees(server, repo, worktrees)
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": workspaces})

	return server, fakeFactoryGh(t, ready)
}

// setFactoryWorktrees answers worktree.list with the main checkout plus the
// linked ones given.
func setFactoryWorktrees(server *herdrtest.Server, repo *herdrtest.Repo, linked []map[string]any) {
	all := append([]map[string]any{{
		"path": repo.Root, "label": "repo", "branch": "main",
		"is_bare": false, "is_detached": false, "is_prunable": false, "is_linked_worktree": false,
	}}, linked...)
	server.HandleResult("worktree.list", map[string]any{
		"type": "worktree_list",
		"source": map[string]any{"repo_key": "k", "repo_name": "repo",
			"repo_root": repo.Root, "source_checkout_path": repo.Root},
		"worktrees": all,
	})
}

// fakeFactoryGh answers `issue list` with the queue, `issue view N` with that
// issue, and records every `issue edit`. Answers are files the script cats:
// echo under dash mangles the backslashes JSON escapes with.
func fakeFactoryGh(t *testing.T, ready []queued) string {
	t.Helper()

	dir := t.TempDir()
	write := func(name string, v any) {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	list := []map[string]any{}
	for _, q := range ready {
		labels := []map[string]any{{"name": labelFactoryReady}}
		for _, l := range q.labels {
			labels = append(labels, map[string]any{"name": l})
		}
		entry := map[string]any{"number": q.number, "title": q.title, "labels": labels}
		list = append(list, entry)
		write(fmt.Sprintf("issue-%d.json", q.number), entry)
	}
	write("list.json", list)

	edits := filepath.Join(dir, "edits")
	herdrtest.FakeGh(t, fmt.Sprintf(`case "$1 $2" in
"issue list") cat '%[1]s/list.json' ;;
"issue view") cat '%[1]s/issue-'"$3"'.json' ;;
"issue edit") [ -e '%[1]s/edit-fails' ] && exit 1; printf '%%s\n' "$*" >> '%[2]s' ;;
*) exit 1 ;;
esac`, dir, edits))
	return edits
}

// breakLabelEdits makes every later `issue edit` fail.
func breakLabelEdits(t *testing.T, edits string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(filepath.Dir(edits), "edit-fails"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// createdBranches is every branch worktree.create was asked for, in order.
func createdBranches(server *herdrtest.Server) []string {
	var branches []string
	for _, call := range server.Calls() {
		if call.Method == "worktree.create" {
			branches = append(branches, call.Params["branch"].(string))
		}
	}
	return branches
}

func readEdits(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func passFactory(t *testing.T, args ...string) string {
	t.Helper()
	var out strings.Builder
	if err := factoryCommand(args, &out); err != nil {
		t.Fatalf("factory %v: %v; it printed:\n%s", args, err, out.String())
	}
	return out.String()
}

// One worker is busy and the cap is two, so one slot is free and one issue is
// started — not the three that are ready. An idle and a done worker are not
// busy: they are waiting on a reviewer, and counting them would stall the
// queue until somebody pruned.
func TestFactoryStartsOnlyUpToTheCap(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server, edits := factoryFleet(t, repo, []string{"working", "idle", "done"}, []queued{
		{5, "Five", nil}, {6, "Six", nil}, {7, "Seven", nil},
	})

	out := passFactory(t, "--max", "2")

	if got := createdBranches(server); len(got) != 1 || got[0] != "5-five" {
		t.Errorf("started %v, want only 5-five:\n%s", got, out)
	}
	got := readEdits(t, edits)
	if len(got) != 1 || !strings.Contains(got[0], "issue edit 5 --add-label "+labelFactoryInProgress+" --remove-label "+labelFactoryReady) {
		t.Errorf("label edits = %q, want one swap on #5", got)
	}
}

// A blocked worker is waiting on a person, and that person is who the cap is
// for. At the cap, nothing is started and the pass still succeeds.
func TestFactoryAtTheCapStartsNothing(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server, edits := factoryFleet(t, repo, []string{"working", "blocked"}, []queued{{5, "Five", nil}})

	out := passFactory(t)

	if got := createdBranches(server); len(got) != 0 {
		t.Errorf("started %v at the default cap of %d", got, factoryDefaultMax)
	}
	if readEdits(t, edits) != nil {
		t.Error("relabelled an issue it did not start")
	}
	if !strings.Contains(out, "at the cap") {
		t.Errorf("a pass that did nothing should say why:\n%s", out)
	}
}

// gh lists newest first. The queue is worked oldest first, whatever order the
// answer came in.
func TestFactoryStartsTheOldestReadyIssueFirst(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server, _ := factoryFleet(t, repo, nil, []queued{
		{30, "Thirty", nil}, {10, "Ten", nil}, {20, "Twenty", nil},
	})

	passFactory(t, "--max", "2")

	got := createdBranches(server)
	if len(got) != 2 || got[0] != "10-ten" || got[1] != "20-twenty" {
		t.Errorf("started %v, want 10-ten then 20-twenty", got)
	}
}

// An issue whose work already lives somewhere is never started twice: not with
// a worktree open on it, not with only its branch left — the state a label swap
// that failed after a start leaves behind — and not when the title has been
// edited since, which changes the slug and not the number.
func TestFactorySkipsAnIssueThatAlreadyHasAWorktreeOrBranch(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server, _ := factoryFleet(t, repo, nil, []queued{
		{10, "Ten", nil}, {12, "Twelve", nil}, {13, "Thirteen", []string{labelFactoryInProgress}}, {14, "Fourteen", nil},
	})
	checkout := repo.AddWorktree("10-old-title", "10-old-title")
	setFactoryWorktrees(server, repo, []map[string]any{{
		"path": checkout, "label": "10-old-title", "branch": "10-old-title",
		"is_bare": false, "is_detached": false, "is_prunable": false, "is_linked_worktree": true,
	}})
	repo.Git("branch", "12-twelve")

	out := passFactory(t, "--max", "1")

	if got := createdBranches(server); len(got) != 1 || got[0] != "14-fourteen" {
		t.Errorf("started %v, want only 14-fourteen:\n%s", got, out)
	}
	for _, n := range []string{"#10", "#12", "#13"} {
		if !strings.Contains(out, "skipped "+n) {
			t.Errorf("%s should be reported as skipped:\n%s", n, out)
		}
	}
}

// The ordering argument in factoryCommand, run: the start succeeds, the label
// swap fails, and the pass says so — then the next pass, finding the issue
// still labelled ready, skips it on its branch rather than starting it again.
func TestFactoryNeverStartsAnIssueTwiceAfterAFailedLabelSwap(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server, edits := factoryFleet(t, repo, nil, []queued{{5, "Five", nil}})
	breakLabelEdits(t, edits)

	var out strings.Builder
	err := factoryCommand(nil, &out)
	if err == nil {
		t.Fatalf("a label swap that failed must fail the pass:\n%s", out.String())
	}
	if got := exitCode(err); got != exitEnvironment {
		t.Errorf("exit %d (%v), want exitEnvironment", got, err)
	}

	out.Reset()
	if err := factoryCommand(nil, &out); err != nil {
		t.Fatalf("second pass: %v\n%s", err, out.String())
	}
	if got := createdBranches(server); len(got) != 1 {
		t.Errorf("worktree.create ran %d times across two passes, want once: %v", len(got), got)
	}
	if !strings.Contains(out.String(), "skipped #5") {
		t.Errorf("the second pass should say why it left #5 alone:\n%s", out.String())
	}
}

// start is where an issue's approval is checked, and a refusal there is exit 3.
// The pass skips that issue without relabelling it or spending a slot on it,
// starts the next one, and still exits 3 so the refusal reaches a person.
func TestFactorySkipsAnIssueStartRefusesAndStartsTheNext(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server, edits := factoryFleet(t, repo, nil, []queued{{5, "Five", nil}, {6, "Six", nil}})

	real := factoryStart
	factoryStart = func(args []string, out io.Writer) error {
		if args[0] == "5" {
			return codef(exitNeedsHuman, "#5 was edited after %s was applied; re-apply it", labelFactoryReady)
		}
		return real(args, out)
	}
	t.Cleanup(func() { factoryStart = real })

	var out strings.Builder
	err := factoryCommand([]string{"--max", "1"}, &out)
	if got := exitCode(err); got != exitNeedsHuman {
		t.Errorf("exit %d (%v), want exitNeedsHuman so the refusal is seen:\n%s", got, err, out.String())
	}
	if got := createdBranches(server); len(got) != 1 || got[0] != "6-six" {
		t.Errorf("started %v, want only 6-six: a refused issue must not hold the only slot:\n%s", got, out.String())
	}
	got := readEdits(t, edits)
	if len(got) != 1 || !strings.Contains(got[0], "issue edit 6 ") {
		t.Errorf("label edits = %q, want only #6 relabelled", got)
	}
	if !strings.Contains(out.String(), "skipped #5") {
		t.Errorf("the refusal should be reported as a skip:\n%s", out.String())
	}
}

// Nothing in the queue is the steady state of a factory run from cron, and it
// is not an error.
func TestFactoryWithNoReadyIssuesExitsCleanly(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server, _ := factoryFleet(t, repo, nil, nil)

	out := passFactory(t)

	if got := createdBranches(server); len(got) != 0 {
		t.Errorf("started %v from an empty queue", got)
	}
	if !strings.Contains(out, "no open issues labelled "+labelFactoryReady) {
		t.Errorf("an empty queue should say so:\n%s", out)
	}
}

// A dry run names what a pass would start, under the same cap, and starts
// nothing and relabels nothing.
func TestFactoryDryRunStartsNothing(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server, edits := factoryFleet(t, repo, []string{"working"}, []queued{
		{5, "Five", nil}, {6, "Six", nil}, {7, "Seven", nil},
	})

	out := passFactory(t, "--dry-run", "--max", "3")

	if got := createdBranches(server); len(got) != 0 {
		t.Errorf("a dry run started %v", got)
	}
	if readEdits(t, edits) != nil {
		t.Error("a dry run relabelled an issue")
	}
	if !strings.Contains(out, "would start #5") || !strings.Contains(out, "would start #6") {
		t.Errorf("a dry run should name what it would start:\n%s", out)
	}
	if strings.Contains(out, "#7") {
		t.Errorf("a dry run should stop at the cap, as a real pass would:\n%s", out)
	}
}

// The JSON is one document: start's own, per issue, is carried inside it rather
// than written beside it.
func TestFactoryJSONIsOneDocumentCarryingEachStart(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	factoryFleet(t, repo, []string{"working"}, []queued{{5, "Five", nil}, {6, "Six", nil}})
	repo.Git("branch", "5-five")

	var out strings.Builder
	if err := factoryCommand([]string{"--json"}, &out); err != nil {
		t.Fatalf("factory --json: %v\n%s", err, out.String())
	}

	var doc struct {
		Max     int `json:"max"`
		Running int `json:"running"`
		Issues  []struct {
			Issue        int             `json:"issue"`
			Outcome      string          `json:"outcome"`
			Reason       *string         `json:"reason"`
			LabelSwapped bool            `json:"label_swapped"`
			Start        json.RawMessage `json:"start"`
		} `json:"issues"`
		ExitCode int `json:"exit_code"`
	}
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out.String())
	}
	if doc.Max != 2 || doc.Running != 1 || doc.ExitCode != 0 {
		t.Errorf("max/running/exit = %d/%d/%d, want 2/1/0", doc.Max, doc.Running, doc.ExitCode)
	}
	if len(doc.Issues) != 2 {
		t.Fatalf("issues = %+v, want #5 skipped and #6 started", doc.Issues)
	}
	if skipped := doc.Issues[0]; skipped.Issue != 5 || skipped.Outcome != factorySkipped || skipped.Reason == nil {
		t.Errorf("first = %+v, want #5 skipped with a reason", skipped)
	}
	started := doc.Issues[1]
	if started.Issue != 6 || started.Outcome != factoryStarted || !started.LabelSwapped {
		t.Errorf("second = %+v, want #6 started and relabelled", started)
	}
	var startDoc struct {
		Branch string `json:"branch"`
	}
	if err := json.Unmarshal(started.Start, &startDoc); err != nil || startDoc.Branch != "6-six" {
		t.Errorf("start's document = %s (%v), want one naming 6-six", started.Start, err)
	}
}

// The branch test is on the number alone, and the hyphen is what keeps #2 off
// a branch belonging to #20.
func TestAlreadyStartedMatchesTheIssueNumberOnly(t *testing.T) {
	for _, tc := range []struct {
		taken []string
		want  bool
	}{
		{[]string{"2-old-title"}, true},
		{[]string{"2"}, true},
		{[]string{"20-other"}, false},
		{[]string{"feat/2-thing"}, false},
		{[]string{"12-thing"}, false},
		{nil, false},
	} {
		got := alreadyStarted(issue{Number: 2, Title: "x"}, tc.taken) != ""
		if got != tc.want {
			t.Errorf("alreadyStarted(#2, %q) = %v, want %v", tc.taken, got, tc.want)
		}
	}
}

func TestFactoryRefusesACapBelowOne(t *testing.T) {
	for _, max := range []string{"0", "-1"} {
		err := run([]string{"factory", "--max", max}, &strings.Builder{})
		if got := exitCode(err); got != exitUsage {
			t.Errorf("factory --max %s = exit %d (%v), want exitUsage", max, got, err)
		}
	}
}
