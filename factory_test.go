package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrtest"
)

// queued is one issue in the fake factory:ready queue.
type queued struct {
	number int
	title  string
	labels []string
}

// factoryFleet is a fake herdr with one workspace per entry of busy, each a
// linked worktree whose agent has that herdr status and, after a slash, that
// last report. The fake gh's ready queue is ready, listed in the order given.
//
// worktree.create makes the worktree for real, as herdr would, so a second pass
// in the same test meets the branch the first one left behind. It returns the
// server and the file gh appends each `issue edit` to.
func factoryFleet(t *testing.T, repo *herdrtest.Repo, busy []string, ready []queued) (*herdrtest.Server, string) {
	t.Helper()

	server := fakeStart(t, repo, "w9", "w9:p1", "working")
	server.Handle("worktree.create", createWorktree(repo))
	bareOrigin(t, repo)

	// Each workspace's pane is "<id>:p1", and its report is whatever followed
	// the slash in its entry of busy: "idle/done" is an idle agent that has
	// reported done.
	reports := map[string]map[string]string{}
	server.Handle("pane.list", func(params map[string]any) (any, error) {
		ws, _ := params["workspace_id"].(string)
		return map[string]any{"type": "pane_list", "panes": []map[string]any{
			{"pane_id": ws + ":p1", "workspace_id": ws, "tab_id": "t1", "index": 0},
		}}, nil
	})
	server.Handle("pane.get", func(params map[string]any) (any, error) {
		pane, _ := params["pane_id"].(string)
		return map[string]any{"type": "pane_info", "pane": map[string]any{
			"pane_id": pane, "workspace_id": strings.TrimSuffix(pane, ":p1"), "tab_id": "t1",
			"terminal_id": "term_1", "agent_status": "idle", "focused": false, "revision": 1,
			"tokens": reports[pane],
		}}, nil
	})

	var worktrees, workspaces []map[string]any
	for i, entry := range busy {
		id := fmt.Sprintf("b%d", i)
		status, reported, _ := strings.Cut(entry, "/")
		if reported != "" {
			reports[id+":p1"] = reportTokens(t, reported)
		}
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

// createWorktree answers worktree.create by making the worktree for real, as
// herdr would.
func createWorktree(repo *herdrtest.Repo) herdrtest.Handler {
	return func(params map[string]any) (any, error) {
		branch, _ := params["branch"].(string)
		repo.AddWorktree(branch, branch)
		return map[string]any{
			"type": "workspace_created",
			"workspace": map[string]any{
				"workspace_id": "w9", "number": 9, "label": "issue", "focused": false,
				"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "idle",
			},
			"root_pane": map[string]any{"pane_id": "w9:p1", "workspace_id": "w9", "tab_id": "t1", "index": 0},
			"tab":       map[string]any{"tab_id": "t1", "workspace_id": "w9", "index": 0},
		}, nil
	}
}

// bareOrigin gives repo a local bare repository as origin, holding main, so
// claims are pushed somewhere real without reaching the network.
func bareOrigin(t *testing.T, repo *herdrtest.Repo) {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "origin.git")
	repo.Git("init", "--bare", "-b", "main", origin)
	repo.Git("remote", "add", "origin", origin)
	repo.Git("push", "-q", "origin", "main")
}

// claims is every claim ref on repo's origin.
func claims(repo *herdrtest.Repo) []string {
	var refs []string
	for _, line := range strings.Split(repo.Git("ls-remote", "origin", claimRefPrefix+"*"), "\n") {
		if _, ref, ok := strings.Cut(line, "\t"); ok {
			refs = append(refs, ref)
		}
	}
	return refs
}

// reportTokens is the pane metadata a worker's `report --status <status>`
// leaves behind, in the layout report.go writes.
func reportTokens(t *testing.T, status string) map[string]string {
	t.Helper()
	encoded, err := encodeReport(report{status: status, note: "x"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	tokens := map[string]string{}
	for k, v := range encoded {
		if s, ok := v.(string); ok {
			tokens[k] = s
		}
	}
	return tokens
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

// factoryBody is every queued issue's body: an Acceptance section start's
// grammar accepts, so the approval check has a payload to hash.
const factoryBody = "## Problem\nIt is broken.\n\n## Acceptance\n- [ ] `go test ./...` exits 0\n"

// fakeFactoryGh answers `issue list` with the queue, `issue view N` with that
// issue, `run list` with a green ci run on main, and records every `issue
// edit`. It also answers the calls start's approval check makes: the
// repository, the issue snapshot, and the permission of the account that
// approved it. Every issue is approved by
// steig, an admin, unless unapprove says otherwise.
//
// Answers are files the script cats: echo under dash mangles the backslashes
// JSON escapes with.
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
		write(fmt.Sprintf("snapshot-%d.json", q.number), factorySnapshot(t, q, true))
	}
	write("list.json", list)
	write("closed.json", []any{})
	write("runs.json", []map[string]any{{"conclusion": "success", "url": "https://github.com/steig/worktender/actions/runs/1"}})
	write("repo.json", map[string]any{"name": "worktender", "owner": map[string]any{"login": "steig"}})
	write("perm-steig.json", map[string]any{"permission": "admin"})

	edits := filepath.Join(dir, "edits")
	herdrtest.FakeGh(t, fmt.Sprintf(`D='%[1]s'
answer() { [ -f "$D/$1" ] || { printf 'gh: HTTP 404\n' >&2; exit 1; }; cat "$D/$1"; }
case "$1 $2" in
"issue list") case "$*" in *"--state closed"*) answer closed.json ;; *) answer list.json ;; esac ;;
"issue view") answer "issue-$3.json" ;;
"run list") case "$*" in *"--workflow ci --branch main --status completed --limit 5"*) answer runs.json ;; *) exit 1 ;; esac ;;
"repo view") answer repo.json ;;
"api graphql")
  for a in "$@"; do case "$a" in number=*) n=${a#number=} ;; esac; done
  answer "snapshot-$n.json" ;;
"api repos/steig/worktender/collaborators/"*/permission)
  login=${2#repos/steig/worktender/collaborators/}; answer "perm-${login%%/permission}.json" ;;
"issue edit") [ -e "$D/edit-fails" ] && exit 1; printf '%%s\n' "$*" >> '%[2]s' ;;
*) printf 'unexpected gh call: %%s\n' "$*" >&2; exit 1 ;;
esac`, dir, edits))
	return edits
}

// factorySnapshot is the GraphQL answer start's approval check reads for q,
// carrying steig's approval of exactly that content when approved is set.
func factorySnapshot(t *testing.T, q queued, approved bool) map[string]any {
	t.Helper()

	nodes := []map[string]any{}
	if approved {
		_, hash, _, err := approvalPayload(q.number, q.title, factoryBody)
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"author": map[string]any{"login": "steig"}, "body": approvalCommentPrefix + hash})
	}
	return map[string]any{"data": map[string]any{"repository": map[string]any{"issue": map[string]any{
		"number": q.number, "title": q.title, "body": factoryBody, "comments": map[string]any{"nodes": nodes},
	}}}}
}

// unapprove makes q's snapshot carry no approval, so start refuses it the way
// it refuses an issue edited since `worktender approve` or never approved.
func unapprove(t *testing.T, edits string, q queued) {
	t.Helper()
	raw, err := json.Marshal(factorySnapshot(t, q, false))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(edits), fmt.Sprintf("snapshot-%d.json", q.number)), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// closeInProgress makes gh's list of closed issues labelled
// factory:in-progress answer with numbers, and only with them.
func closeInProgress(t *testing.T, edits string, numbers ...int) {
	t.Helper()
	list := []map[string]any{}
	for _, n := range numbers {
		list = append(list, map[string]any{"number": n, "title": fmt.Sprintf("Closed %d", n),
			"labels": []map[string]any{{"name": labelFactoryInProgress}}})
	}
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(edits), "closed.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// setMainCI makes gh's completed ci runs on main the runs given, newest first,
// or, with none, makes gh fail to list runs at all.
func setMainCI(t *testing.T, edits string, runs ...map[string]any) {
	t.Helper()
	path := filepath.Join(filepath.Dir(edits), "runs.json")
	if len(runs) == 0 {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := json.Marshal(runs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
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

// Two of four workers hold a slot and the cap is three, so one issue is
// started — not the three that are ready. The worker that reported done has
// freed its slot, and so has the workspace herdr sees no agent in; the
// working one and the blocked one that reported only a plan have not.
func TestFactoryStartsOnlyUpToTheCap(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server, edits := factoryFleet(t, repo, []string{"working", "idle/done", "unknown", "blocked/planned"}, []queued{
		{5, "Five", nil}, {6, "Six", nil}, {7, "Seven", nil},
	})

	out := passFactory(t, "--max", "3")

	if got := createdBranches(server); len(got) != 1 || got[0] != "5-five" {
		t.Errorf("started %v, want only 5-five:\n%s", got, out)
	}
	got := readEdits(t, edits)
	if len(got) != 1 || !strings.Contains(got[0], "issue edit 5 --add-label "+labelFactoryInProgress+" --remove-label "+labelFactoryReady) {
		t.Errorf("label edits = %q, want one swap on #5", got)
	}
}

// An idle worker is between turns, not finished: waiting on its checks, a
// review or a feedback notice. Two of them fill the default cap whether or not
// they have reported anything yet, and only a done report would free one. If
// idle freed a slot, every pass would start more while these waited.
func TestFactoryAtTheCapStartsNothing(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server, edits := factoryFleet(t, repo, []string{"idle", "idle/planned"}, []queued{{5, "Five", nil}})

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
// #5 carries no approval of its current content — edited since `worktender
// approve`, or never approved — so start refuses it. The pass skips it without
// relabelling it or spending a slot on it, passes start's message through,
// starts the next one, and still exits 3 so the refusal reaches a person.
func TestFactorySkipsAnIssueStartRefusesAndStartsTheNext(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	five := queued{5, "Five", nil}
	server, edits := factoryFleet(t, repo, nil, []queued{five, {6, "Six", nil}})
	unapprove(t, edits, five)

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
	if !strings.Contains(out.String(), "skipped #5") || !strings.Contains(out.String(), "worktender approve 5") {
		t.Errorf("the refusal should be reported as a skip, in start's own words:\n%s", out.String())
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

// A closed issue still labelled in progress has the label taken off, and an
// open one carrying it — a worker still on it — is left alone. The pass is at
// the cap, the pass that runs most while workers are out, and clears anyway.
func TestFactoryClearsInProgressFromClosedIssuesOnly(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	_, edits := factoryFleet(t, repo, []string{"working"}, []queued{
		{8, "Eight", []string{labelFactoryInProgress}},
	})
	closeInProgress(t, edits, 7)

	out := passFactory(t, "--max", "1")

	got := readEdits(t, edits)
	if len(got) != 1 || !strings.Contains(got[0], "issue edit 7 --remove-label "+labelFactoryInProgress) || strings.Contains(got[0], "--add-label") {
		t.Errorf("label edits = %q, want only %s removed from closed #7", got, labelFactoryInProgress)
	}
	if !strings.Contains(out, "removed "+labelFactoryInProgress+" from closed #7") {
		t.Errorf("a pass should say what it cleared:\n%s", out)
	}
	if strings.Contains(out, "#8") {
		t.Errorf("an open issue should not be mentioned, let alone touched:\n%s", out)
	}
}

// A dry run names the closed issues it would clear, and clears none.
func TestFactoryDryRunClearsNothing(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	_, edits := factoryFleet(t, repo, nil, nil)
	closeInProgress(t, edits, 7)

	out := passFactory(t, "--dry-run")

	if got := readEdits(t, edits); got != nil {
		t.Errorf("a dry run edited labels: %q", got)
	}
	if !strings.Contains(out, "would remove "+labelFactoryInProgress+" from closed #7") {
		t.Errorf("a dry run should name what it would clear:\n%s", out)
	}
}

// A failed clear is a warning, not a stall: the queue is still worked, and the
// pass exits non-zero so the log shows it.
func TestFactoryFailedClearStillStartsAndExitsNonZero(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server, edits := factoryFleet(t, repo, nil, []queued{{5, "Five", nil}})
	closeInProgress(t, edits, 7)
	breakLabelEdits(t, edits)

	var out strings.Builder
	err := factoryCommand([]string{"--json"}, &out)
	if exitCode(err) != exitEnvironment {
		t.Errorf("exit = %d (%v), want %d", exitCode(err), err, exitEnvironment)
	}
	if got := createdBranches(server); len(got) != 1 || got[0] != "5-five" {
		t.Errorf("started %v, want 5-five despite the failed clear", got)
	}
	var doc struct {
		Cleared []struct {
			Issue   int     `json:"issue"`
			Removed bool    `json:"removed"`
			Reason  *string `json:"reason"`
		} `json:"cleared"`
	}
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out.String())
	}
	if len(doc.Cleared) != 1 || doc.Cleared[0].Issue != 7 || doc.Cleared[0].Removed || doc.Cleared[0].Reason == nil {
		t.Errorf("cleared = %+v, want #7 not removed, with a reason", doc.Cleared)
	}
}

// A red main is a reason to start nothing at all, dry run or not: every worker
// would fork from it, and its checks could not tell its failures from main's.
func TestFactoryWillNotDispatchWhileMainIsRed(t *testing.T) {
	const url = "https://github.com/steig/worktender/actions/runs/9"
	for _, args := range [][]string{nil, {"--dry-run"}} {
		repo := herdrtest.NewRepo(t)
		server, edits := factoryFleet(t, repo, nil, []queued{{5, "Five", nil}})
		setMainCI(t, edits, map[string]any{"conclusion": "failure", "url": url})

		var out strings.Builder
		err := factoryCommand(args, &out)
		if exitCode(err) != exitNeedsHuman || !strings.Contains(fmt.Sprint(err), url) {
			t.Errorf("factory %v: exit %d (%v), want %d naming %s", args, exitCode(err), err, exitNeedsHuman, url)
		}
		if got := createdBranches(server); len(got) != 0 {
			t.Errorf("factory %v started %v while main was red", args, got)
		}
		if strings.Contains(out.String(), "would start") {
			t.Errorf("factory %v planned a start while main was red:\n%s", args, out.String())
		}
	}
}

// A cancelled run judged nothing, so the failed run before it still stands.
func TestFactoryLooksPastACancelledRunToARedOne(t *testing.T) {
	const url = "https://github.com/steig/worktender/actions/runs/8"
	repo := herdrtest.NewRepo(t)
	server, edits := factoryFleet(t, repo, nil, []queued{{5, "Five", nil}})
	setMainCI(t, edits,
		map[string]any{"conclusion": "cancelled", "url": "https://github.com/steig/worktender/actions/runs/9"},
		map[string]any{"conclusion": "failure", "url": url},
	)

	var out strings.Builder
	err := factoryCommand(nil, &out)
	if exitCode(err) != exitNeedsHuman || !strings.Contains(fmt.Sprint(err), url) {
		t.Errorf("exit %d (%v), want %d naming %s", exitCode(err), err, exitNeedsHuman, url)
	}
	if got := createdBranches(server); len(got) != 0 {
		t.Errorf("started %v behind a cancelled run on a red main", got)
	}
}

// A red pass still tidies the board first, and its JSON says what it cleared.
func TestFactoryRedMainJSONCarriesWhatWasCleared(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	_, edits := factoryFleet(t, repo, nil, []queued{{5, "Five", nil}})
	closeInProgress(t, edits, 7)
	setMainCI(t, edits, map[string]any{"conclusion": "failure", "url": "https://github.com/steig/worktender/actions/runs/9"})

	var out strings.Builder
	err := factoryCommand([]string{"--json"}, &out)
	if exitCode(err) != exitNeedsHuman {
		t.Errorf("exit = %d (%v), want %d", exitCode(err), err, exitNeedsHuman)
	}
	var doc struct {
		Cleared []struct {
			Issue   int  `json:"issue"`
			Removed bool `json:"removed"`
		} `json:"cleared"`
		ExitCode int `json:"exit_code"`
	}
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out.String())
	}
	if len(doc.Cleared) != 1 || doc.Cleared[0].Issue != 7 || !doc.Cleared[0].Removed || doc.ExitCode != exitNeedsHuman {
		t.Errorf("doc = %+v, want #7 cleared and exit %d", doc, exitNeedsHuman)
	}
}

// A gh that cannot say how main stands is warned about, not obeyed: the pass
// works the queue as it would have without the check.
func TestFactoryStartsWhenMainsCIIsUnknown(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server, edits := factoryFleet(t, repo, nil, []queued{{5, "Five", nil}})
	setMainCI(t, edits)

	out := passFactory(t)

	if got := createdBranches(server); len(got) != 1 || got[0] != "5-five" {
		t.Errorf("started %v, want 5-five:\n%s", got, out)
	}
	if !strings.Contains(out, "warning: could not read the default branch's latest ci run") {
		t.Errorf("an unknown ci state should be warned about:\n%s", out)
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

// A dry run resolves the repository exactly as a real pass does. Outside a
// plugin action there is no context, and a real pass refuses rather than guess
// from the working directory; a dry run that guessed instead would report a
// plan for a pass that cannot run. Both must refuse, with the same message.
func TestFactoryDryRunRefusesWhereARealPassRefuses(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	factoryFleet(t, repo, []string{"working"}, []queued{{5, "Five", nil}})
	t.Setenv("HERDR_PLUGIN_CONTEXT_JSON", "")
	// Standing in the repository, so a fallback to the working directory would
	// find one and succeed.
	t.Chdir(repo.Root)

	var refusals []string
	for _, args := range [][]string{{"--max", "1"}, {"--max", "1", "--dry-run"}} {
		var out strings.Builder
		err := factoryCommand(args, &out)
		if err == nil {
			t.Fatalf("factory %v with no context succeeded; it printed:\n%s", args, out.String())
		}
		if got := exitCode(err); got != exitUsage {
			t.Errorf("factory %v = exit %d (%v), want exitUsage", args, got, err)
		}
		if !strings.Contains(err.Error(), "--repo <path>") {
			t.Errorf("factory %v must name the way past the refusal, got %v", args, err)
		}
		refusals = append(refusals, err.Error())
	}
	if refusals[0] != refusals[1] {
		t.Errorf("a dry run refused differently from a real pass:\n real: %s\n  dry: %s", refusals[0], refusals[1])
	}
}

// Given --repo, both paths resolve it with no context at all, so the refusal
// above is the only thing standing between a scheduler and a working pass.
func TestFactoryDryRunAndRealPassResolveTheRepositoryTheyAreGiven(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	factoryFleet(t, repo, []string{"working"}, []queued{})
	t.Setenv("HERDR_PLUGIN_CONTEXT_JSON", "")
	t.Chdir(t.TempDir())

	root := gitx.Resolve(repo.Root)
	for _, args := range [][]string{{"--repo", repo.Root}, {"--repo", repo.Root, "--dry-run"}} {
		if out := passFactory(t, args...); !strings.Contains(out, "repository: "+root) {
			t.Errorf("factory %v should act on the repository it was given (%s):\n%s", args, root, out)
		}
	}
}

// The claim is on origin before start makes the worktree, which is what makes
// it a lock: a second coordinator asking origin during the worker's whole run
// finds it there.
func TestFactoryClaimsAnIssueBeforeStartingIt(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server, _ := factoryFleet(t, repo, nil, []queued{{5, "Five", nil}})
	create := createWorktree(repo)
	var heldAtCreate []string
	server.Handle("worktree.create", func(params map[string]any) (any, error) {
		heldAtCreate = claims(repo)
		return create(params)
	})

	out := passFactory(t)

	if got := createdBranches(server); len(got) != 1 || got[0] != "5-five" {
		t.Fatalf("started %v, want 5-five:\n%s", got, out)
	}
	if want := []string{claimRef(5)}; !slices.Equal(heldAtCreate, want) {
		t.Errorf("claims on origin when the worktree was made = %q, want %q", heldAtCreate, want)
	}
	if got := repo.Git("branch", "--list", "--all", "*claims*"); got != "" {
		t.Errorf("a claim must never read as a branch, got %q", got)
	}
}

// Another coordinator's claim is skipped whatever commit it names. #5's names
// the same base this pass would push, which git reports as "up to date" and
// exits 0 for, so the exit status alone would read it as this pass's own claim.
// #6's names another commit, which the create-only push is refused for.
func TestFactorySkipsAnIssueAnotherCoordinatorClaimed(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server, edits := factoryFleet(t, repo, nil, []queued{{5, "Five", nil}, {6, "Six", nil}, {7, "Seven", nil}})
	repo.Git("push", "-q", "origin", "main:"+claimRef(5))
	repo.Git("commit", "-q", "--allow-empty", "-m", "elsewhere")
	repo.Git("push", "-q", "origin", "HEAD:"+claimRef(6))
	repo.Git("reset", "-q", "--hard", "HEAD~1")

	var out strings.Builder
	if err := factoryCommand([]string{"--max", "1", "--json"}, &out); err != nil {
		t.Fatalf("factory: %v\n%s", err, out.String())
	}

	if got := createdBranches(server); len(got) != 1 || got[0] != "7-seven" {
		t.Errorf("started %v, want only 7-seven: a claimed issue must not hold the slot", got)
	}
	if got := readEdits(t, edits); len(got) != 1 || !strings.Contains(got[0], "issue edit 7 ") {
		t.Errorf("label edits = %q, want only #7 relabelled", got)
	}
	var doc struct {
		Issues []struct {
			Issue   int    `json:"issue"`
			Outcome string `json:"outcome"`
		} `json:"issues"`
	}
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out.String())
	}
	var outcomes []string
	for _, i := range doc.Issues {
		outcomes = append(outcomes, fmt.Sprintf("#%d %s", i.Issue, i.Outcome))
	}
	want := []string{"#5 " + factoryClaimedElsewhere, "#6 " + factoryClaimedElsewhere, "#7 " + factoryStarted}
	if !slices.Equal(outcomes, want) {
		t.Errorf("outcomes = %q, want %q", outcomes, want)
	}
}

// A start that fails gives its claim back, so the issue is not held by nobody.
func TestFactoryReleasesTheClaimOfAFailedStart(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	five := queued{5, "Five", nil}
	_, edits := factoryFleet(t, repo, nil, []queued{five})
	unapprove(t, edits, five)

	var out strings.Builder
	if err := factoryCommand(nil, &out); exitCode(err) != exitNeedsHuman {
		t.Fatalf("exit %d (%v), want start's refusal:\n%s", exitCode(err), err, out.String())
	}
	if got := claims(repo); len(got) != 0 {
		t.Errorf("claims on origin = %q, want none after a failed start:\n%s", got, out.String())
	}
}

// The pass that takes the label off a closed issue deletes its claim too, and
// leaves an open issue's claim alone. #3 closed with no claim at all, as an
// issue started before claims existed did.
func TestFactoryReleasesTheClaimOfAClosedIssue(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	_, edits := factoryFleet(t, repo, []string{"idle", "idle"}, nil)
	repo.Git("push", "-q", "origin", "main:"+claimRef(4), "main:"+claimRef(9))
	closeInProgress(t, edits, 3, 4)

	out := passFactory(t)

	if got, want := claims(repo), []string{claimRef(9)}; !slices.Equal(got, want) {
		t.Errorf("claims on origin = %q, want only %q:\n%s", got, want, out)
	}
	if got := readEdits(t, edits); len(got) != 2 {
		t.Errorf("label edits = %q, want #3 and #4 cleared", got)
	}
}
