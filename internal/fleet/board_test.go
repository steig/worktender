package fleet

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/steig/worktender/internal/wt"
)

// fixtureRepos is a two-repository live fleet: one staffed worktree matching
// a ledger dispatch, one nobody dispatched, and a second repository.
func fixtureRepos() []wt.Repo {
	return []wt.Repo{
		{Root: "/home/x/proj", Rows: []wt.Row{
			{Main: true, Branch: "main", Dir: "proj"},
			{Branch: "42-fix-the-thing", Dir: "42-fix-the-thing", WorkspaceID: "w1", PaneID: "w1:p1", AgentStatus: "working"},
			{Branch: "9-untracked", Dir: "9-untracked", WorkspaceID: "w2", PaneID: "w2:p1", AgentStatus: "idle"},
		}},
		{Root: "/home/x/other", Rows: []wt.Row{
			{Main: true, Branch: "main", Dir: "other"},
		}},
	}
}

func fixtureLedger() Ledger {
	return Ledger{Entries: []Entry{
		entry(0, "t1", "dispatch", func(e *Entry) { e.Executor = "worker"; e.Repo = "/home/x/proj"; e.Issue = 42 }),
		entry(1, "t1", "report", func(e *Entry) { e.Status = "done"; e.PR = 7 }),
		entry(2, "t2", "dispatch", func(e *Entry) { e.Executor = "peer"; e.Repo = "/home/x/gone"; e.Issue = 3 }),
		entry(3, "t3", "dispatch", func(e *Entry) { e.Repo = "/home/x/proj"; e.Issue = 42 }),
		entry(4, "t3", "escalate", func(e *Entry) { e.Severity = "ordinary"; e.Reason = "no report" }),
		entry(5, "t4", "escalate", func(e *Entry) { e.Severity = "safety"; e.Reason = "guard file touched" }),
	}}
}

func TestBuildMergesLedgerTasksOntoLiveRows(t *testing.T) {
	b := Build(fixtureRepos(), fixtureLedger(), "/state/ledger.jsonl", true, at.Add(time.Hour), nil)

	// t1 dispatched repo /home/x/proj issue 42 → the 42-fix-the-thing row.
	// t3 matched the same row but is escalated, so it renders on top instead.
	var matched *LiveRow
	for _, repo := range b.Repos {
		for _, row := range repo.Rows {
			if row.Branch == "42-fix-the-thing" {
				matched = row
			} else if row.Task != nil {
				t.Errorf("row %q matched task %q; nothing dispatched it", row.Branch, row.Task.ID)
			}
		}
	}
	if matched == nil || matched.Task == nil {
		t.Fatal("the dispatched worktree did not get its task")
	}

	// Escalations on top: safety first, then ordinary. The ordinary one
	// matched a live row, so it carries navigation coordinates.
	if len(b.Escalations) != 2 {
		t.Fatalf("got %d escalations, want 2", len(b.Escalations))
	}
	if b.Escalations[0].Task.ID != "t4" || b.Escalations[1].Task.ID != "t3" {
		t.Errorf("escalations ordered %s, %s; safety belongs first", b.Escalations[0].Task.ID, b.Escalations[1].Task.ID)
	}
	if b.Escalations[1].Live == nil || b.Escalations[1].Live.PaneID != "w1:p1" {
		t.Error("the escalation that matched a live row lost its pane")
	}

	// t2's repository has no live worktree: an orphan.
	if len(b.Orphans) != 1 || b.Orphans[0].Task.ID != "t2" {
		t.Errorf("orphans = %v, want t2", b.Orphans)
	}
}

// Issue 4 must not match branch 42-…: the prefix ends at the number.
func TestIssueMatchingStopsAtTheNumberBoundary(t *testing.T) {
	row := &LiveRow{Row: wt.Row{Branch: "42-fix-the-thing", Dir: "42-fix-the-thing"}}
	if carriesIssue(row, 4) {
		t.Error("issue 4 matched branch 42-fix-the-thing")
	}
	if !carriesIssue(row, 42) {
		t.Error("issue 42 did not match its own branch")
	}
	bare := &LiveRow{Row: wt.Row{Branch: "42"}}
	if !carriesIssue(bare, 42) {
		t.Error("a bare-number branch is what start makes of an unsluggable title")
	}
}

// The gh lookup runs only for rows where the fleet claims a pull request
// exists. Every lookup is a serial `gh` call and the watch loop repeats them.
func TestBuildLooksUpPRStateOnlyWhereOneIsClaimed(t *testing.T) {
	repos := fixtureRepos()
	// The worker in w2:p1 reported done #9 on its pane; nothing else claims a
	// PR from the pane side.
	repos[0].Rows[2].Report = &wt.Report{Found: true, Status: "done", PR: 9}

	var asked []string
	lookup := func(root, branch string) (string, error) {
		asked = append(asked, branch)
		return "OPEN", nil
	}
	b := Build(repos, fixtureLedger(), "", true, at.Add(time.Hour), lookup)

	// Two claims: the pane report on 9-untracked, and the ledger report on
	// t1 which matched 42-fix-the-thing.
	if len(asked) != 2 {
		t.Fatalf("asked gh about %v, want the two claiming rows", asked)
	}
	for _, repo := range b.Repos {
		for _, row := range repo.Rows {
			claimed := row.Branch == "9-untracked" || row.Branch == "42-fix-the-thing"
			if got := row.PR != nil; got != claimed {
				t.Errorf("row %q: pr looked up = %v, claimed = %v", row.Branch, got, claimed)
			}
		}
	}
}

func TestLinesPutEscalationsAboveEverything(t *testing.T) {
	b := Build(fixtureRepos(), fixtureLedger(), "/state/ledger.jsonl", true, at.Add(time.Hour), nil)
	lines := Lines(b)

	var texts []string
	for _, l := range lines {
		texts = append(texts, l.Text)
	}
	all := strings.Join(texts, "\n")

	esc := strings.Index(all, "escalations")
	repo := strings.Index(all, "/home/x/proj")
	orphan := strings.Index(all, "ledger tasks with no live worktree")
	if esc < 0 || repo < 0 || orphan < 0 {
		t.Fatalf("a section is missing:\n%s", all)
	}
	if !(esc < repo && repo < orphan) {
		t.Errorf("sections out of order:\n%s", all)
	}
	if !strings.Contains(all, "guard file touched") {
		t.Errorf("the safety escalation's reason is not on the board:\n%s", all)
	}

	// Every selectable line carries a target; headings carry none.
	for _, line := range lines {
		if line.Target != nil && line.Target.Label == "" {
			t.Errorf("target without a label on %q", line.Text)
		}
	}
}

// The header is the board's one-line answer; a board with nothing must still
// say something rather than render emptiness that reads as a broken screen.
func TestLinesOnAnEmptyFleetStillSpeak(t *testing.T) {
	b := Build(nil, Ledger{}, "/state/ledger.jsonl", false, at, nil)
	lines := Lines(b)
	all := ""
	for _, l := range lines {
		all += l.Text + "\n"
	}
	if !strings.Contains(all, "no repositories") {
		t.Errorf("an empty fleet renders as:\n%s", all)
	}
	if !strings.Contains(all, "ledger: none at /state/ledger.jsonl") {
		t.Errorf("a missing ledger went unmentioned:\n%s", all)
	}
}

func TestBoardJSONRoundTrips(t *testing.T) {
	b := Build(fixtureRepos(), fixtureLedger(), "/state/ledger.jsonl", true, at.Add(time.Hour), nil)
	raw, err := json.Marshal(JSON(b))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc struct {
		Ledger struct {
			Found     bool `json:"found"`
			Malformed int  `json:"malformed"`
		} `json:"ledger"`
		Escalations []struct {
			Task     string  `json:"task"`
			Severity *string `json:"severity"`
			PaneID   *string `json:"pane_id"`
		} `json:"escalations"`
		Repositories []struct {
			Root      string `json:"root"`
			Worktrees []struct {
				Branch *string `json:"branch"`
				Task   *struct {
					Summary string `json:"summary"`
					PR      *int   `json:"pr"`
				} `json:"task"`
			} `json:"worktrees"`
		} `json:"repositories"`
		Unmatched []struct {
			Task string  `json:"task"`
			Repo *string `json:"repo"`
		} `json:"unmatched_tasks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !doc.Ledger.Found {
		t.Error("ledger.found lost in projection")
	}
	if len(doc.Escalations) != 2 || doc.Escalations[0].Severity == nil || *doc.Escalations[0].Severity != "safety" {
		t.Errorf("escalations misprojected: %+v", doc.Escalations)
	}
	if doc.Escalations[1].PaneID == nil || *doc.Escalations[1].PaneID != "w1:p1" {
		t.Error("the matched escalation lost its pane coordinates")
	}
	if len(doc.Unmatched) != 1 || doc.Unmatched[0].Task != "t2" {
		t.Errorf("unmatched tasks misprojected: %+v", doc.Unmatched)
	}

	var taskSummaries int
	for _, repo := range doc.Repositories {
		for _, row := range repo.Worktrees {
			if row.Task != nil {
				taskSummaries++
				if row.Task.PR == nil || *row.Task.PR != 7 {
					t.Errorf("the matched task's ledger PR did not project: %+v", row.Task)
				}
			}
		}
	}
	if taskSummaries != 1 {
		t.Errorf("%d rows carry tasks, want 1", taskSummaries)
	}
}
