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

	// t2 was dispatched to a peer session: the PEERS section, not the
	// no-live-worktree divergence — a peer having no worktree is by design.
	if len(b.Peers) != 1 || b.Peers[0].Task.ID != "t2" {
		t.Errorf("peers = %v, want t2", b.Peers)
	}
	if len(b.Orphans) != 0 {
		t.Errorf("orphans = %v, want none: the only unmatched task is a peer's", b.Orphans)
	}

	// The header's count: t1 and t2 are in flight, t3 and t4 are escalated.
	if b.InFlight != 2 {
		t.Errorf("in flight = %d, want 2", b.InFlight)
	}
}

// A dispatched worker task whose worktree is gone is the divergence the
// orphans section names — unlike a peer's, whose absence is by design.
func TestBuildKeepsWorkerTasksWithNoWorktreeAsOrphans(t *testing.T) {
	ledger := Ledger{Entries: []Entry{
		entry(0, "t9", "dispatch", func(e *Entry) { e.Executor = "worker"; e.Repo = "/home/x/gone"; e.Issue = 5 }),
	}}
	b := Build(fixtureRepos(), ledger, "", true, at.Add(time.Hour), nil)
	if len(b.Orphans) != 1 || b.Orphans[0].Task.ID != "t9" {
		t.Errorf("orphans = %v, want t9", b.Orphans)
	}
	if len(b.Peers) != 0 {
		t.Errorf("peers = %v, want none", b.Peers)
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

func boardText(v View) string {
	var texts []string
	for _, l := range v.Lines {
		texts = append(texts, l.Plain())
	}
	return strings.Join(texts, "\n")
}

// The hierarchy the board promises, stacked in one column when the pane is
// narrow (and in scrollback): ESCALATIONS on top, then the live fleet, then
// what landed, then the peers, then the idle capacity.
func TestRenderKeepsTheSectionHierarchy(t *testing.T) {
	b := Build(fixtureRepos(), fixtureLedger(), "/state/ledger.jsonl", true, at.Add(time.Hour), nil)
	view := Render(b, 0)
	all := boardText(view)

	esc := strings.Index(all, "ESCALATIONS")
	flight := strings.Index(all, "IN FLIGHT")
	worker := strings.Index(all, "proj/42-fix-the-thing")
	landed := strings.Index(all, "RECENTLY LANDED")
	peers := strings.Index(all, "PEERS")
	trees := strings.Index(all, "WORKTREES")
	if esc < 0 || flight < 0 || worker < 0 || landed < 0 || peers < 0 || trees < 0 {
		t.Fatalf("a section is missing:\n%s", all)
	}
	if !(esc < flight && flight < worker && worker < landed && landed < peers && peers < trees) {
		t.Errorf("sections out of order:\n%s", all)
	}
	if !strings.Contains(all, "guard file touched") {
		t.Errorf("the safety escalation's reason is not on the board:\n%s", all)
	}

	// Panels, not banded text: rounded borders with the title embedded in
	// the top border, interiors padded behind the border rune.
	for _, want := range []string{"╭─ ESCALATIONS ", "╭─ IN FLIGHT ", "╭─ RECENTLY LANDED ", "╰─"} {
		if !strings.Contains(all, want) {
			t.Errorf("the board is missing the border %q:\n%s", want, all)
		}
	}

	// The design system holds inside the boxes: panel titles take the
	// accent, borders recede, and status color lands on the glyph — the
	// row's text stays on the neutral base.
	for _, line := range view.Lines {
		text := line.Plain()
		if strings.Contains(text, "ESCALATIONS") && len(line.Spans) > 1 && line.Spans[1].Style != styleKey {
			t.Errorf("the ESCALATIONS title is not accent: %+v", line.Spans[1].Style)
		}
		if strings.Contains(text, "guard file touched") {
			if line.Spans[0].Style != styleBorder {
				t.Errorf("the row does not sit behind a border: %+v", line.Spans[0])
			}
			if line.Spans[1].Style != styleBad {
				t.Errorf("the escalation glyph is not red: %+v", line.Spans[1].Style)
			}
			for _, span := range line.Spans[3:] {
				if span.Style.FG == ColorBad {
					t.Errorf("status color bled past the glyph and severity into %q", span.Text)
				}
			}
		}
	}

	// Navigation order is urgency order: the first hotspot is the safety
	// escalation, and every hotspot carries a labeled target.
	if len(view.Hots) == 0 || !strings.Contains(view.Hots[0].Target.Label, "t4") {
		t.Errorf("the first hotspot is not the safety escalation: %+v", view.Hots)
	}
	for _, h := range view.Hots {
		if h.Target == nil || h.Target.Label == "" {
			t.Errorf("hotspot without a labeled target: %+v", h)
		}
	}

	// The text itself stays pipe-clean: styles live beside the spans, never
	// in them, so the one-shot print carries no ANSI.
	if strings.Contains(all, "\x1b") {
		t.Errorf("escape bytes leaked into the board text:\n%q", all)
	}
}

// At wide widths the board is two columns: IN FLIGHT on the left, the task
// panels stacked on the right — so their top borders share a line.
func TestRenderComposesTwoColumnsWhenWide(t *testing.T) {
	b := Build(fixtureRepos(), fixtureLedger(), "/state/ledger.jsonl", true, at.Add(time.Hour), nil)
	view := Render(b, 160)

	var topRow string
	for _, line := range view.Lines {
		if text := line.Plain(); strings.Contains(text, "╭─ IN FLIGHT ") {
			topRow = text
		}
	}
	if !strings.Contains(topRow, "╭─ ESCALATIONS ") {
		t.Errorf("ESCALATIONS does not sit beside IN FLIGHT at width 160:\n%s", boardText(view))
	}

	// Rows in the right column still highlight within their own panel: the
	// hotspot's span range must not start at the left edge.
	for _, h := range view.Hots {
		if strings.Contains(h.Target.Label, "t4") && h.SpanFrom <= 1 {
			t.Errorf("the right-column escalation's highlight starts at the screen edge: %+v", h)
		}
	}

	// No composed line exceeds the terminal width.
	for _, line := range view.Lines {
		if n := len([]rune(line.Plain())); n > 160 {
			t.Errorf("a composed line is %d cells wide: %q", n, line.Plain())
		}
	}
}

// IN FLIGHT holds only what is flying: working or blocked agents, and open
// ledger tasks. Idle mains and unclaimed checkouts are capacity, listed dim
// under WORKTREES instead.
func TestInFlightExcludesIdleWorktrees(t *testing.T) {
	b := Build(fixtureRepos(), fixtureLedger(), "/state/ledger.jsonl", true, at.Add(time.Hour), nil)
	all := boardText(Render(b, 0))

	flight := strings.Index(all, "╭─ IN FLIGHT ")
	trees := strings.Index(all, "╭─ WORKTREES ")
	if flight < 0 || trees < 0 {
		t.Fatalf("a panel is missing:\n%s", all)
	}
	between := all[flight:trees]
	if strings.Contains(between, "9-untracked") || strings.Contains(between, "proj/main") {
		t.Errorf("idle rows render as in flight:\n%s", between)
	}
	after := all[trees:]
	for _, want := range []string{"proj/9-untracked", "proj/main", "other/main"} {
		if !strings.Contains(after, want) {
			t.Errorf("WORKTREES is missing %q:\n%s", want, after)
		}
	}
}

// The landed verdict is the task's last verify entry: a fail followed by a
// later pass landed as a pass, and only fail-without-later-pass is red.
func TestLandedVerdictIsTheLastVerify(t *testing.T) {
	ledger := Ledger{Entries: []Entry{
		entry(0, "t1", "dispatch", func(e *Entry) { e.Repo = "/home/x/proj"; e.Issue = 42 }),
		entry(1, "t1", "verify", func(e *Entry) { e.Result = "fail" }),
		entry(2, "t1", "verify", func(e *Entry) { e.Result = "pass" }),
	}}
	b := Build(nil, ledger, "", true, at.Add(time.Hour), nil)
	all := boardText(Render(b, 0))
	if !strings.Contains(all, glyphVerified) || !strings.Contains(all, "verify pass") {
		t.Errorf("a fail-then-pass task did not land as a pass:\n%s", all)
	}
	if strings.Contains(all, "verify fail") {
		t.Errorf("the earlier fail still speaks for a passed task:\n%s", all)
	}
}

// A task steered to a peer session never names a repository; its rows say
// which session has it instead of a bare dash.
func TestTaskPlaceFallsBackToTheSteeredTarget(t *testing.T) {
	ledger := Ledger{Entries: []Entry{
		entry(0, "t1", "steer", func(e *Entry) { e.Target = "dbx-eb"; e.Note = "status" }),
		entry(1, "t1", "report", func(e *Entry) { e.Status = "done"; e.PR = 245 }),
		entry(2, "t1", "verify", func(e *Entry) { e.Result = "pass" }),
	}}
	b := Build(nil, ledger, "", true, at.Add(time.Hour), nil)
	all := boardText(Render(b, 0))
	if !strings.Contains(all, "dbx-eb") {
		t.Errorf("the landed peer row lost its session name:\n%s", all)
	}
}

// The idle fleet is the default state and must render as a full screen: the
// summary header says the fleet is idle, and RECENTLY LANDED says what just
// happened — never a blank canvas.
func TestLinesOnAnIdleFleetShowTheSummaryAndWhatLanded(t *testing.T) {
	ledger := Ledger{Entries: []Entry{
		entry(0, "t1", "dispatch", func(e *Entry) { e.Executor = "worker"; e.Repo = "/home/x/proj"; e.Issue = 42 }),
		entry(1, "t1", "report", func(e *Entry) { e.Status = "done"; e.PR = 7 }),
		entry(2, "t1", "verify", func(e *Entry) { e.Result = "pass" }),
		entry(3, "t2", "dispatch", func(e *Entry) { e.Repo = "/home/x/proj"; e.Issue = 9 }),
		entry(4, "t2", "verify", func(e *Entry) { e.Result = "fail"; e.Evidence = "tests red" }),
	}}
	b := Build(nil, ledger, "/state/ledger.jsonl", true, at.Add(34*time.Minute), nil)
	all := boardText(Render(b, 0))

	for _, want := range []string{
		"FLEET", "idle · 0 workers · ledger 30m ago", // the top bar
		"nothing in flight", // IN FLIGHT says it is empty
		"RECENTLY LANDED",
		"✓", "proj#42", "#7", "verify pass", // the landed row: glyph, place, PR, outcome
		"✗", "verify fail",
		"m ago", // relative times, never raw stamps
	} {
		if !strings.Contains(all, want) {
			t.Errorf("the idle board is missing %q:\n%s", want, all)
		}
	}

	// Newest landing first: t2 closed after t1.
	if fail, pass := strings.Index(all, "verify fail"), strings.Index(all, "verify pass"); fail > pass {
		t.Errorf("landings are not newest-first:\n%s", all)
	}
}

// A board with nothing at all must still say something rather than render
// emptiness that reads as a broken screen.
func TestLinesOnAnEmptyFleetStillSpeak(t *testing.T) {
	b := Build(nil, Ledger{}, "/state/ledger.jsonl", false, at, nil)
	all := boardText(Render(b, 0))
	for _, want := range []string{
		"FLEET", "idle · 0 workers · no ledger",
		"nothing in flight",
		"nothing landed in the last 7 days",
		"ledger: none at /state/ledger.jsonl",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("an empty fleet is missing %q:\n%s", want, all)
		}
	}
}

// Narrow panes drop the detail columns rather than wrapping: the row still
// says what it is and how old, and the report/PR detail waits for width.
func TestLinesDropDetailColumnsWhenNarrow(t *testing.T) {
	repos := fixtureRepos()
	repos[0].Rows[1].Report = &wt.Report{Found: true, Status: "done", PR: 9}
	b := Build(repos, fixtureLedger(), "", true, at.Add(time.Hour), nil)

	wideText := boardText(Render(b, 160))
	if !strings.Contains(wideText, "done #9") {
		t.Fatalf("the wide board dropped the report column:\n%s", wideText)
	}
	narrowText := boardText(Render(b, 40))
	if strings.Contains(narrowText, "done #9") {
		t.Errorf("a 40-column board still renders the report column:\n%s", narrowText)
	}
	if !strings.Contains(narrowText, "42-fix-the-thing") {
		t.Errorf("the narrow board lost the row itself:\n%s", narrowText)
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
		Peers []struct {
			Task string `json:"task"`
		} `json:"peer_tasks"`
		Landed []struct {
			Task string `json:"task"`
		} `json:"recently_landed"`
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
	if len(doc.Unmatched) != 0 {
		t.Errorf("unmatched tasks misprojected: %+v, want none — t2 is a peer's", doc.Unmatched)
	}
	if len(doc.Peers) != 1 || doc.Peers[0].Task != "t2" {
		t.Errorf("peer tasks misprojected: %+v", doc.Peers)
	}
	if len(doc.Landed) != 0 {
		t.Errorf("recently landed misprojected: %+v, want none in this fixture", doc.Landed)
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
