package fleet

import (
	"strconv"
	"testing"
	"time"
)

// at is a fixed clock for the folding tests; entries step forward from it.
var at = time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)

// entry builds one v1 entry n minutes after the epoch above.
func entry(n int, task, kind string, mut ...func(*Entry)) Entry {
	e := Entry{V: Version, TS: at.Add(time.Duration(n) * time.Minute), Task: task, Type: kind, By: "fleet"}
	for _, m := range mut {
		m(&e)
	}
	return e
}

func TestFoldFollowsOneTaskThroughItsLifecycle(t *testing.T) {
	tasks := Fold([]Entry{
		entry(0, "t1", "dispatch", func(e *Entry) { e.Executor = "worker"; e.Repo = "/r"; e.Issue = 7 }),
		entry(1, "t1", "ack", func(e *Entry) { e.Result = "accepted" }),
		entry(2, "t1", "report", func(e *Entry) { e.Status = "done"; e.PR = 12 }),
	})
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(tasks))
	}
	task := tasks[0]
	if task.Terminal() {
		t.Error("a reported-but-unverified task is not closed; verify is the terminal entry")
	}
	if got := task.Summary(); got != "report done #12" {
		t.Errorf("summary %q, want the report", got)
	}

	tasks = Fold(append([]Entry{
		entry(0, "t1", "dispatch"),
		entry(2, "t1", "report", func(e *Entry) { e.Status = "done" }),
	}, entry(3, "t1", "verify", func(e *Entry) { e.Result = "pass" })))
	if !tasks[0].Terminal() {
		t.Error("a verified task must be terminal")
	}
	if got := tasks[0].Summary(); got != "verify pass" {
		t.Errorf("summary %q, want the verify", got)
	}
}

// A redispatch under the same task id is a new assignment: the previous
// answers must not speak for it, or a declined-then-reassigned task would
// still read "declined" and a re-verified one would close before it ran.
func TestFoldResetsTheAnswersOnRedispatch(t *testing.T) {
	tasks := Fold([]Entry{
		entry(0, "t1", "dispatch", func(e *Entry) { e.Target = "peer-a" }),
		entry(1, "t1", "ack", func(e *Entry) { e.Result = "declined" }),
		entry(2, "t1", "dispatch", func(e *Entry) { e.Target = "peer-b" }),
	})
	task := tasks[0]
	if task.Declined() {
		t.Error("the declined ack survived the reassignment dispatch")
	}
	if task.Terminal() {
		t.Error("a reassigned task is open")
	}
	if got := task.Summary(); got != "dispatched" {
		t.Errorf("summary %q, want the fresh dispatch", got)
	}
	if task.Dispatch.Target != "peer-b" {
		t.Errorf("dispatch is %q, want the reassignment", task.Dispatch.Target)
	}
}

// A declined task with no reassignment stays open: the contract only closes
// it once a reassignment dispatch exists, and until then it is exactly the
// open loop the board exists to show.
func TestADeclinedTaskWithoutReassignmentStaysOpen(t *testing.T) {
	tasks := Fold([]Entry{
		entry(0, "t1", "dispatch"),
		entry(1, "t1", "ack", func(e *Entry) { e.Result = "declined" }),
	})
	if tasks[0].Terminal() {
		t.Error("a declined, unreassigned task was closed")
	}
	if got := tasks[0].Summary(); got != "declined" {
		t.Errorf("summary %q, want %q", got, "declined")
	}
}

func TestAnEscalationIsOpenUntilAHumanEntryResolvesIt(t *testing.T) {
	escalated := Fold([]Entry{
		entry(0, "t1", "dispatch"),
		entry(1, "t1", "escalate", func(e *Entry) { e.Severity = "safety"; e.Reason = "guard file touched" }),
	})
	if !escalated[0].Escalated() {
		t.Error("an unacknowledged escalation did not read as escalated")
	}
	if escalated[0].Terminal() {
		t.Error("an unacknowledged escalation closed the task")
	}
	if got := escalated[0].Summary(); got != "escalated safety" {
		t.Errorf("summary %q, want the escalation", got)
	}

	acked := Fold([]Entry{
		entry(0, "t1", "dispatch"),
		entry(1, "t1", "escalate", func(e *Entry) { e.Severity = "safety" }),
		entry(2, "t1", "human", func(e *Entry) { e.Action = "escalation-acknowledged" }),
	})
	if acked[0].Escalated() {
		t.Error("an acknowledged escalation still reads as escalated")
	}
	if !acked[0].Terminal() {
		t.Error("the contract closes an escalated task on acknowledgement")
	}

	// A human entry about something else — a permission answer, an
	// instruction — is not the acknowledgement, and must not resolve it.
	unrelated := Fold([]Entry{
		entry(0, "t1", "escalate", func(e *Entry) { e.Severity = "safety" }),
		entry(1, "t1", "human", func(e *Entry) { e.Action = "permission-granted" }),
	})
	if !unrelated[0].Escalated() || unrelated[0].Terminal() {
		t.Error("an unrelated human entry resolved a safety escalation nobody acknowledged")
	}

	reopened := Fold([]Entry{
		entry(0, "t1", "escalate"),
		entry(1, "t1", "human", func(e *Entry) { e.Action = "escalation-acknowledged" }),
		entry(2, "t1", "escalate", func(e *Entry) { e.Severity = "ordinary" }),
	})
	if !reopened[0].Escalated() {
		t.Error("a fresh escalation after the acknowledgement must reopen the question")
	}
}

func TestDeadlineEntriesMoveTheDeadlineState(t *testing.T) {
	deadline := at.Add(2 * time.Hour)
	tasks := Fold([]Entry{
		entry(0, "t1", "dispatch", func(e *Entry) { e.Deadline = deadline }),
		entry(1, "t1", "deadline", func(e *Entry) { e.Result = "expired" }),
	})
	task := tasks[0]
	if task.DeadlineState != "expired" || !task.Deadline.Equal(deadline) {
		t.Errorf("deadline fold: state %q time %v", task.DeadlineState, task.Deadline)
	}
	if got := task.Summary(); got != "deadline expired" {
		t.Errorf("summary %q; an expired deadline outranks the dispatch", got)
	}
}

// New types arrive under v1 by the additive rule. They must not break the
// fold, must move the task's age, and must surface as themselves.
func TestAnUnknownTypeStillBelongsToItsTask(t *testing.T) {
	tasks := Fold([]Entry{
		entry(0, "t1", "handover", func(e *Entry) { e.Raw = `{"type":"handover"}` }),
	})
	if len(tasks) != 1 {
		t.Fatalf("an unknown type did not create its task")
	}
	if got := tasks[0].Summary(); got != "?handover" {
		t.Errorf("summary %q, want the marked type name", got)
	}
	if tasks[0].Last.Type != "handover" {
		t.Errorf("Last did not move for the unknown type")
	}
}

func TestOpenAppliesTheHorizon(t *testing.T) {
	now := at.Add(10 * 24 * time.Hour)
	tasks := Fold([]Entry{
		// Old and open: stale.
		entry(0, "old", "dispatch"),
		// Old but verified: terminal, not stale — it is simply closed.
		entry(0, "closed", "dispatch"),
		entry(1, "closed", "verify", func(e *Entry) { e.Result = "pass" }),
		// Fresh: open. Minutes are the helper's unit, so give it days by hand.
	})
	fresh := entry(0, "fresh", "dispatch")
	fresh.TS = now.Add(-time.Hour)
	tasks = append(tasks, Fold([]Entry{fresh})...)

	open, stale := Open(tasks, now)
	if stale != 1 {
		t.Errorf("stale = %d, want 1: the old open loop", stale)
	}
	if len(open) != 1 || open[0].ID != "fresh" {
		t.Errorf("open = %v, want just the fresh task", ids(open))
	}
}

// Landed is the RECENTLY LANDED section's content: terminal tasks inside the
// horizon, newest first, capped so history stays a section and not a scroll.
func TestLandedOrdersNewestFirstAndCaps(t *testing.T) {
	now := at.Add(24 * time.Hour)
	var entries []Entry
	for i := 0; i < RecentLimit+2; i++ {
		id := "t" + strconv.Itoa(i)
		entries = append(entries,
			entry(i, id, "dispatch"),
			entry(100+i, id, "verify", func(e *Entry) { e.Result = "pass" }),
		)
	}
	// Open, and terminal-but-ancient: neither lands.
	entries = append(entries, entry(0, "open", "dispatch"))
	old := entry(0, "old", "verify", func(e *Entry) { e.Result = "pass" })
	old.TS = now.Add(-8 * 24 * time.Hour)
	entries = append(entries, old)

	landed := Landed(Fold(entries), now)
	if len(landed) != RecentLimit {
		t.Fatalf("landed %d tasks, want the %d cap", len(landed), RecentLimit)
	}
	if landed[0].ID != "t"+strconv.Itoa(RecentLimit+1) {
		t.Errorf("newest landing is %q, want the last verify", landed[0].ID)
	}
	for _, task := range landed {
		if task.ID == "open" || task.ID == "old" {
			t.Errorf("%q landed; it is not a fresh terminal task", task.ID)
		}
	}
}

func ids(tasks []*Task) []string {
	var out []string
	for _, t := range tasks {
		out = append(out, t.ID)
	}
	return out
}
