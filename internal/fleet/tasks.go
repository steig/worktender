package fleet

import (
	"sort"
	"strconv"
	"time"
)

// Horizon bounds how far back the board reads open loops, per the contract's
// replay rule. Tasks still open beyond it are counted as stale and said so
// once, rather than rendered as if freshly in flight.
const Horizon = 7 * 24 * time.Hour

// Task is everything the ledger says about one task id, folded in file order.
//
// File order rather than timestamp order, deliberately: appends are the
// authority on what happened after what — a sole writer emits them in the
// order it acted — and timestamps are whatever clock that writer had.
type Task struct {
	ID string
	// By is the controller that last wrote about this task.
	By string
	// Target is the executor the work last went to — the dispatch's target,
	// or the session a steer or nudge named. Kept on the task because a task
	// steered to a peer may never have a dispatch at all, and "who has this"
	// is then the only place the board can put it.
	Target string

	// Latest entry of each type the board renders. Nil when the task has none.
	// A new dispatch resets Ack, Report and Verify: it is a new assignment,
	// and the previous assignment's answers do not speak for it.
	Dispatch *Entry
	Ack      *Entry
	Report   *Entry
	Verify   *Entry

	// Escalate is the latest escalation, and Acked whether a `human` entry
	// arrived after it. An acknowledged escalation closes the task per the
	// contract's replay rule; a fresh escalate after the acknowledgement
	// reopens the question.
	Escalate *Entry
	Acked    bool

	// Deadline is the latest deadline named for this task — on the dispatch
	// or on a `deadline set` — and DeadlineState the latest `deadline`
	// entry's word: "set", "expired" or "met". Empty when no deadline entry
	// arrived; the dispatch's own deadline sets only the time.
	Deadline      time.Time
	DeadlineState string

	// Last is the final entry in file order, whatever its type — including
	// the types this board does not know, which is how an unknown type still
	// moves a task's age and surfaces raw.
	Last  Entry
	First time.Time
}

// Terminal reports whether the ledger considers this task closed: verified,
// or escalated and acknowledged. An `ack declined` alone is deliberately NOT
// terminal — the contract closes it only once a reassignment dispatch exists,
// and a same-id redispatch reopens the fold above, so a task whose story ends
// at "declined" is an open loop someone must reassign.
func (t *Task) Terminal() bool {
	if t.Verify != nil {
		return true
	}
	return t.Escalate != nil && t.Acked
}

// Escalated reports whether this task has an escalation nobody has
// acknowledged — the rows the board puts on top.
func (t *Task) Escalated() bool {
	return t.Escalate != nil && !t.Acked
}

// Declined reports whether the latest assignment was declined and nothing has
// been done about it.
func (t *Task) Declined() bool {
	return t.Ack != nil && t.Ack.Result == "declined"
}

// Fold groups entries by task id, in first-appearance order.
//
// Entries carrying a version this reader does not know are skipped: under a
// breaking bump the fields cannot be trusted to mean what v1 says, and the
// caller already knows to warn from Ledger.UnknownVersion.
func Fold(entries []Entry) []*Task {
	byID := map[string]*Task{}
	var tasks []*Task

	for i := range entries {
		e := &entries[i]
		if e.V != Version {
			continue
		}
		t := byID[e.Task]
		if t == nil {
			t = &Task{ID: e.Task, First: e.TS}
			byID[e.Task] = t
			tasks = append(tasks, t)
		}
		fold(t, e)
	}
	return tasks
}

// fold applies one entry to its task.
func fold(t *Task, e *Entry) {
	switch e.Type {
	case "dispatch":
		t.Dispatch = e
		// A new assignment: the previous one's answers no longer speak.
		t.Ack, t.Report, t.Verify = nil, nil, nil
		if !e.Deadline.IsZero() {
			t.Deadline = e.Deadline
			t.DeadlineState = ""
		}
	case "ack":
		t.Ack = e
	case "report":
		t.Report = e
	case "verify":
		t.Verify = e
	case "escalate":
		t.Escalate = e
		t.Acked = false
	case "human":
		// Only the acknowledgement resolves an escalation. The other human
		// actions — a merge, a permission answer, an instruction — can land
		// on an escalated task about something else entirely, and treating
		// any of them as the acknowledgement would silently drop a safety
		// escalation nobody looked at.
		if t.Escalate != nil && e.Action == "escalation-acknowledged" {
			t.Acked = true
		}
	case "deadline":
		t.DeadlineState = e.Result
		if !e.Deadline.IsZero() {
			t.Deadline = e.Deadline
		}
	case "steer", "nudge":
		// Nothing to fold beyond Last: they say the controller talked, and
		// the board's age column is where that shows.
	default:
		// An unknown type still belongs to its task. Nothing is interpreted
		// — additive evolution means new types arrive under v1 — but Last
		// below still moves, and the renderer shows the raw line.
	}
	if e.Target != "" {
		t.Target = e.Target
	}
	t.By = e.By
	t.Last = *e
}

// Open splits the folded tasks into the ones the board renders and a count of
// stale ones: open loops whose last entry is older than the horizon. Stale
// loops are counted rather than listed, per the contract's stale-loops-digest
// rule — surfacing them once, not re-arming them forever.
func Open(tasks []*Task, now time.Time) (open []*Task, stale int) {
	for _, t := range tasks {
		if t.Terminal() {
			continue
		}
		if now.Sub(t.Last.TS) > Horizon {
			stale++
			continue
		}
		open = append(open, t)
	}
	return open, stale
}

// RecentLimit bounds the RECENTLY LANDED section. Ten rows is a screenful of
// history beside the live sections; the rest is the ledger's to keep, not the
// board's to scroll.
const RecentLimit = 10

// Landed is the terminal tasks whose story ended inside the horizon, newest
// last-entry first, capped at RecentLimit. It is the idle board's content:
// with nothing in flight, "what just landed" is the answer the person opening
// the board is owed instead of a blank screen.
func Landed(tasks []*Task, now time.Time) []*Task {
	var out []*Task
	for _, t := range tasks {
		if !t.Terminal() || now.Sub(t.Last.TS) > Horizon {
			continue
		}
		out = append(out, t)
	}
	// Stable, so tasks whose last entries share a timestamp keep file order —
	// the appends are the authority on what happened after what.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Last.TS.After(out[j].Last.TS)
	})
	if len(out) > RecentLimit {
		out = out[:RecentLimit]
	}
	return out
}

// Summary is the one phrase the board's task column prints: the latest fact
// worth acting on, most urgent first.
func (t *Task) Summary() string {
	switch {
	case t.Escalated():
		return "escalated " + t.Escalate.Severity
	case t.DeadlineState == "expired":
		return "deadline expired"
	case t.Verify != nil:
		return "verify " + t.Verify.Result
	case t.Declined():
		return "declined"
	case t.Report != nil:
		s := "report " + t.Report.Status
		if t.Report.PR > 0 {
			s += " #" + strconv.Itoa(t.Report.PR)
		}
		return s
	case t.Ack != nil:
		return t.Ack.Result
	case t.Dispatch != nil && t.Dispatch.Executor != "":
		return "dispatched " + t.Dispatch.Executor
	case t.Dispatch != nil:
		return "dispatched"
	case knownType(t.Last.Type):
		return t.Last.Type
	default:
		// An unknown type renders raw per the contract — but a table cell has
		// no room for a JSON line, so the cell names the type and the raw
		// line stays in the JSON.
		return "?" + t.Last.Type
	}
}

// knownType reports whether v1 of the contract defines this entry type.
func knownType(kind string) bool {
	switch kind {
	case "dispatch", "ack", "report", "verify", "escalate", "steer", "nudge", "deadline", "human":
		return true
	}
	return false
}
