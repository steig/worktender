package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/herdrtest"
)

// Every command that grew a --json must actually take it. A flag that parses
// nowhere is the same as no flag, and each command owns its own flag set.
func TestEveryJSONCommandAcceptsTheFlag(t *testing.T) {
	repo := herdrtest.NewRepo(t)

	for _, command := range []string{"ls", "doctor", "sync", "prune", "prune-apply", "stats"} {
		t.Run(command, func(t *testing.T) {
			// Pinned to a fixture repository, not just to an absent herdr. The
			// degraded path falls back to the process working directory, so
			// without a t.Chdir the ones that survive would enumerate whatever
			// checkout `go test` was run from — the maintainer's own, with a
			// `gh pr list` per linked branch and the real transcript directory.
			// Green on CI, where there is one checkout and no worktrees, and
			// slow and machine-dependent on every machine this tool is for.
			//
			// The assertion is only that they failed, or did not, for a reason
			// other than the flag.
			herdrtest.HerdrDown(t)
			t.Chdir(repo.Root)

			err := run([]string{command, "--json"}, new(bytes.Buffer))
			if err != nil && strings.Contains(err.Error(), "flag provided but not defined") {
				t.Errorf("%s does not accept --json: %v", command, err)
			}
		})
	}
}

// The reconcile commands print a "repository:" line above the table. On a stdout
// being parsed it is a syntax error, so it has to become a field instead — and
// the fact still has to be there, because it is the one those two commands were
// given for saying out loud.
func TestPruneJSONCarriesTheRepositoryInsteadOfPrintingIt(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("done", "done")
	repo.CommitIn(checkout, "done.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")

	herdrtest.FakeGhPRState(t, "MERGED")

	server := fakeSession(t, repo)
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "done", ""))
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})

	var out bytes.Buffer
	if err := pruneCommand([]string{"--json"}, &out, false); err != nil {
		t.Fatalf("prune --json: %v", err)
	}

	var document reconcileJSON
	if err := json.Unmarshal(out.Bytes(), &document); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out.String())
	}
	if document.Repository != repo.RealRoot {
		t.Errorf("repository = %q, want %q", document.Repository, repo.RealRoot)
	}
	if len(document.Results) != 1 {
		t.Fatalf("want one planned prune, got %+v", document.Results)
	}
	if got := document.Results[0]; got.Status != "planned" || got.Target != "done" {
		t.Errorf("result = %+v", got)
	}
	// The dry run must still be a dry run.
	if !repo.Exists(checkout) {
		t.Fatal("prune --json removed a worktree")
	}
}

// A reconcile runs its body up to reconcilePasses times and the text mode
// prints a table per pass. Doing that with JSON would put several documents on
// stdout, which every parser reads as a truncated one.
func TestSyncJSONWritesOneDocumentAcrossPasses(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("feature", "feature")

	server := fakeSession(t, repo)
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "feature", ""))
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})
	server.HandleResult("worktree.open", map[string]any{"type": "workspace_created"})

	var out bytes.Buffer
	if err := syncCommand([]string{"--json"}, &out); err != nil {
		t.Fatalf("sync --json: %v", err)
	}

	if n := bytes.Count(out.Bytes(), []byte("\"repository\"")); n != 1 {
		t.Fatalf("want exactly one document on stdout, found %d:\n%s", n, out.String())
	}
	var document reconcileJSON
	if err := json.Unmarshal(out.Bytes(), &document); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "repository: ") {
		t.Errorf("the human line was printed beside the document:\n%s", out.String())
	}
	if len(document.Results) != 1 || document.Results[0].Kind != "adopt" {
		t.Errorf("want the one adoption, got %+v", document.Results)
	}
}

// The report has to be in the document even when the command exits 1 — a
// consumer that only sees a non-zero exit learns nothing about which action
// failed, which is the whole reason it asked for the document.
func TestSyncJSONWritesTheReportEvenWhenAnActionFails(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("wip", "wip")

	server := fakeSession(t, repo)
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "wip", "w2"))
	server.HandleResult("workspace.list", workspaceListReply(repo, checkout, "w2"))
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})
	server.HandleResult("pane.list", map[string]any{"type": "pane_list",
		"panes": []map[string]any{{"pane_id": "w2:p1"}}})
	server.Handle("agent.start", func(map[string]any) (any, error) { return nil, errBusyPane{} })

	var out bytes.Buffer
	if err := syncCommand([]string{"--json"}, &out); err == nil {
		t.Fatalf("sync should fail when staffing failed; output was:\n%s", out.String())
	}

	var document reconcileJSON
	if err := json.Unmarshal(out.Bytes(), &document); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out.String())
	}
	if len(document.Results) != 1 || document.Results[0].Status != "failed" {
		t.Errorf("the failure must be in the document, got %+v", document.Results)
	}
}

// doctor's own failure mode: it exits 1 when herdr is unreachable, and a
// consumer reading only stdout would otherwise see the checks and an empty
// repository list — which is what a healthy herdr with nothing open looks like.
func TestDoctorJSONSaysWhyItCouldNotFinish(t *testing.T) {
	// Clearing the variable is not enough to make herdr unreachable and never
	// was: doctor now resolves the default socket like every other command, so
	// on a machine with herdr running this test used to assert a failure that
	// only happened on CI. HerdrDown makes the state real.
	herdrtest.HerdrDown(t)

	var out bytes.Buffer
	if err := doctorCommand([]string{"--json"}, &out); err == nil {
		t.Fatal("doctor should fail when herdr cannot be reached")
	}

	var document doctorJSON
	if err := json.Unmarshal(out.Bytes(), &document); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out.String())
	}
	if document.Error == nil || !strings.Contains(*document.Error, "cannot reach herdr") {
		t.Errorf("error = %v, want the reason nothing else could be checked", document.Error)
	}
	if document.Repositories != nil {
		t.Errorf("repositories must be null rather than empty when unknown, got %+v", document.Repositories)
	}
	// The checks are the half that is still answerable, so they must be there.
	names := map[string]bool{}
	for _, c := range document.Checks {
		names[c.Name] = true
	}
	for _, want := range []string{"version", "herdr", "gh", "events"} {
		if !names[want] {
			t.Errorf("check %q missing from the document:\n%s", want, out.String())
		}
	}
}

// The counts answer "how many" and blocked is the status where that is no
// answer at all: the session has stopped and only a person can restart it, so
// the document names the worktree it was.
func TestDoctorJSONNamesBlockedWorktreesRatherThanOnlyCountingThem(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("79-stuck", "79-stuck")

	server := fakeSession(t, repo)
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "79-stuck", "w2"))
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{{
		"workspace_id": "w2", "number": 2, "label": "wt", "focused": false,
		"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "blocked",
		"worktree": map[string]any{"repo_key": "k", "repo_name": "repo",
			"repo_root": repo.Root, "checkout_path": checkout, "is_linked_worktree": true},
	}}})

	var out bytes.Buffer
	if err := doctorCommand([]string{"--json"}, &out); err != nil {
		t.Fatalf("doctor --json: %v", err)
	}

	var document doctorJSON
	if err := json.Unmarshal(out.Bytes(), &document); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out.String())
	}
	if len(document.Repositories) != 1 {
		t.Fatalf("want one repository, got %+v", document.Repositories)
	}

	got := document.Repositories[0]
	if got.Agents["blocked"] != 1 {
		t.Errorf("agents = %v, want one blocked", got.Agents)
	}
	if len(got.Blocked) != 1 {
		t.Fatalf("blocked = %+v, want the one worktree it is", got.Blocked)
	}
	if branch := got.Blocked[0].Branch; branch == nil || *branch != "79-stuck" {
		t.Errorf("blocked branch = %v, want 79-stuck", branch)
	}
}

// The table prints a basename and a sentence. The document has to carry the
// same counts as numbers, and the path the basename came from.
func TestDoctorJSONReportsRepositories(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("feature", "feature")

	server := fakeSession(t, repo)
	server.HandleResult("workspace.list", workspaceListReply(repo, checkout, "w2"))
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "feature", "w2"))

	var out bytes.Buffer
	if err := doctorCommand([]string{"--json"}, &out); err != nil {
		t.Fatalf("doctor --json: %v", err)
	}

	var document doctorJSON
	if err := json.Unmarshal(out.Bytes(), &document); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out.String())
	}
	if len(document.Repositories) != 1 {
		t.Fatalf("want one repository, got %+v", document.Repositories)
	}

	got := document.Repositories[0]
	if got.Error != nil {
		t.Fatalf("repository could not be read: %v", *got.Error)
	}
	if got.Root != repo.RealRoot {
		t.Errorf("root = %q, want %q", got.Root, repo.RealRoot)
	}
	if got.Worktrees == nil || *got.Worktrees != 1 {
		t.Errorf("worktrees = %v, want 1", got.Worktrees)
	}
	if got.Agents["idle"] != 1 {
		t.Errorf("agents = %v, want one idle", got.Agents)
	}
	// One or the other, never both.
	if strings.Contains(out.String(), "run it from a shell with") {
		t.Errorf("the text report was written beside the document:\n%s", out.String())
	}
}
