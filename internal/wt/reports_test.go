package wt_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/wt"
)

// The report column exists so a coordinator whose context was cleared can ask
// the fleet what it last said instead of having written it down. The envelope
// is already on the pane — `report` put it there and a gate reads it — and was
// reachable from nowhere but inside a running gate.
func TestReportColumnShowsWhatEachWorkerLastSaid(t *testing.T) {
	rows := []wt.Row{
		{Branch: "a", WorkspaceID: "w1", PaneID: "w1:p1", AgentStatus: "idle", Dir: "a"},
		{Branch: "b", WorkspaceID: "w2", PaneID: "w2:p1", AgentStatus: "working", Dir: "b"},
		{Branch: "c", WorkspaceID: "w3", PaneID: "w3:p1", AgentStatus: "idle", Dir: "c"},
		{Branch: "d", Dir: "d"},
	}
	wt.WithReports(rows, func(pane string) (wt.Report, error) {
		switch pane {
		case "w1:p1":
			return wt.Report{Found: true, Status: "done", PR: 4, Note: "landed"}, nil
		case "w2:p1":
			return wt.Report{Found: true, Status: "planned", Note: "reading the issue"}, nil
		default:
			return wt.Report{}, nil
		}
	})

	var buf strings.Builder
	if err := wt.Render(&buf, rows, wt.Columns{Reports: true}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if !strings.Contains(out, "done #4") {
		t.Errorf("a worker that reported done with a pull request should show both:\n%s", out)
	}
	if !strings.Contains(out, "planned") {
		t.Errorf("a worker that reported planned should show it:\n%s", out)
	}
	// The note runs to 200 characters of text a stranger may have written. A
	// table is the wrong frame for it and the JSON is the right one.
	if strings.Contains(out, "landed") || strings.Contains(out, "reading the issue") {
		t.Errorf("the note reached the table; it belongs only in the JSON:\n%s", out)
	}
}

// A worktree with no pane cannot carry a report, so it is never asked — and it
// must not be recorded as having been asked and answered nothing.
func TestAWorktreeWithNoPaneIsNeverAsked(t *testing.T) {
	rows := []wt.Row{{Branch: "orphan", Dir: "orphan"}}
	asked := 0
	wt.WithReports(rows, func(string) (wt.Report, error) {
		asked++
		return wt.Report{}, nil
	})
	if asked != 0 {
		t.Errorf("a paneless worktree was asked %d times; a report is attached to a pane", asked)
	}
	if rows[0].Report != nil {
		t.Errorf("report = %+v, want nil so the JSON can say no lookup ran", rows[0].Report)
	}
}

// Unreachable and nothing-to-say are different facts. The table has room for
// neither, exactly as with the pull request column — but the JSON must not
// collapse them, because one is an ordinary answer and the other is a listing
// that failed to reach something.
func TestTheJSONSeparatesUnreadableFromUnreported(t *testing.T) {
	rows := []wt.Row{
		{Branch: "quiet", PaneID: "w1:p1", Dir: "quiet"},
		{Branch: "broken", PaneID: "w2:p1", Dir: "broken"},
	}
	wt.WithReports(rows, func(pane string) (wt.Report, error) {
		if pane == "w2:p1" {
			return wt.Report{}, errors.New("pane_not_found")
		}
		return wt.Report{}, nil
	})

	docs := wt.JSON(rows)

	quiet := docs[0].Report
	if quiet == nil || quiet.Found || quiet.Error != nil {
		t.Errorf("a pane with no report should be found=false with no error, got %+v", quiet)
	}
	broken := docs[1].Report
	if broken == nil || broken.Error == nil || !strings.Contains(*broken.Error, "pane_not_found") {
		t.Errorf("an unreadable pane should carry its error, got %+v", broken)
	}

	var buf strings.Builder
	if err := wt.Render(&buf, rows, wt.Columns{Reports: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "pane_not_found") {
		t.Errorf("the error reached the table, which has no room to explain it:\n%s", buf.String())
	}
}

// The column is omitted rather than dashed when it was not asked for. A "-" is
// the same cell "nothing to report" prints, so a listing of dashes would read
// as a fleet that has all gone quiet.
func TestTheReportColumnIsAbsentUntilAskedFor(t *testing.T) {
	rows := []wt.Row{{Branch: "a", WorkspaceID: "w1", PaneID: "w1:p1", AgentStatus: "idle", Dir: "a"}}

	var without strings.Builder
	if err := wt.Render(&without, rows, wt.Columns{}); err != nil {
		t.Fatal(err)
	}
	var with strings.Builder
	wt.WithReports(rows, func(string) (wt.Report, error) {
		return wt.Report{Found: true, Status: "done"}, nil
	})
	if err := wt.Render(&with, rows, wt.Columns{Reports: true}); err != nil {
		t.Fatal(err)
	}

	if n := len(strings.Fields(without.String())); n != 7 {
		t.Errorf("the plain listing drew %d columns, want the usual 7: %q", n, without.String())
	}
	if n := len(strings.Fields(with.String())); n != 8 {
		t.Errorf("--reports should add exactly one column, got %d: %q", n, with.String())
	}
	if docs := wt.JSON([]wt.Row{{Branch: "a", Dir: "a"}}); docs[0].Report != nil {
		t.Error("report should be null in the JSON when no lookup ran")
	}
}
