package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steig/worktender/internal/herdrapi"
)

// gateWithDagr runs a gate on the fleet with the run file pointed into a
// temporary directory, the way gateCommand would have pointed it at the
// checkout.
func (f *gateFleet) gateWithDagr(t *testing.T, path string, args ...string) (string, error) {
	t.Helper()
	opts, err := parseGate(append(args, "--dagr"))
	if err != nil {
		t.Fatalf("parseGate(%q): %v", args, err)
	}
	if !opts.dagr {
		t.Fatal("--dagr did not reach the options")
	}
	opts.dagrPath = path
	f.mu.Lock()
	f.gated = len(opts.targets)
	f.mu.Unlock()
	var out strings.Builder
	err = runGate(f.client, opts, &out)
	return out.String(), err
}

func readDagr(t *testing.T, path string) dagrDoc {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read run file: %v", err)
	}
	var doc dagrDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("run file is not JSON: %v\n%s", err, raw)
	}
	if doc.Dagr != dagrVersion || doc.Run.ID != dagrRunID || doc.GeneratedAt == "" {
		t.Errorf("header = dagr %d, run.id %q, generated_at %q", doc.Dagr, doc.Run.ID, doc.GeneratedAt)
	}
	return doc
}

func dagrTaskNamed(t *testing.T, doc dagrDoc, id string) dagrTask {
	t.Helper()
	for _, task := range doc.Tasks {
		if task.ID == id {
			return task
		}
	}
	t.Fatalf("no task %q in %+v", id, doc.Tasks)
	return dagrTask{}
}

// Each thing a gate can observe lands as the contract's reading of it, at the
// tier the gate reached it by. The worker's own `done` is never drawn verified.
func TestGateDagrDrawsWhatTheGateSaw(t *testing.T) {
	for _, tc := range []struct {
		name     string
		act      func(*gateWorker)
		timeout  string
		task     string
		attempt  string
		evidence string
	}{
		{"a done is the worker's claim", func(w *gateWorker) {
			w.prints(report{status: "done", pr: 12, note: "green"})
		}, "10s", "done", "done", "reported"},

		{"blocked leaves the attempt alive and names who unblocks", func(w *gateWorker) {
			w.prints(report{status: "blocked", note: "needs a decision"})
		}, "10s", "blocked", "working", ""},

		{"a pane herdr says ended fails the task", func(w *gateWorker) {
			w.paneEnds(herdrapi.StreamEventPaneExited)
		}, "10s", "failed", "lost", "verified"},

		{"an agent herdr lost track of is only heuristic", func(w *gateWorker) {
			w.becomes("unknown")
		}, "10s", "blocked", "lost", "heuristic"},

		{"a timeout is the gate's, not the worker's", func(w *gateWorker) {}, "400ms", "working", "working", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".dagr", "run.json")
			w := newGateWorker(t, "working", "")
			go tc.act(w)

			w.fleet.gateWithDagr(t, path, "--target", "worker", "--timeout", tc.timeout)

			task := dagrTaskNamed(t, readDagr(t, path), "worker")
			if task.State != tc.task {
				t.Errorf("task state = %q, want %q", task.State, tc.task)
			}
			if len(task.Attempts) != 1 {
				t.Fatalf("attempts = %+v, want one", task.Attempts)
			}
			a := task.Attempts[0]
			if a.State != tc.attempt || a.N != 1 || a.Cause.Type != "initial" {
				t.Errorf("attempt = %+v, want state %q, n 1, cause initial", a, tc.attempt)
			}
			if tc.evidence == "" {
				if a.Outcome != nil {
					t.Errorf("an unsettled attempt carries outcome %+v", a.Outcome)
				}
			} else if a.Outcome == nil || a.Outcome.Evidence != tc.evidence || a.Outcome.Result != a.State {
				t.Errorf("outcome = %+v, want result %q at evidence %q", a.Outcome, a.State, tc.evidence)
			}
			// A still-open attempt is where the dagr pane focuses from.
			if a.State == "working" && (a.Locator == nil || a.Locator.Pane != gateTestPane) {
				t.Errorf("working attempt locator = %+v, want pane %s", a.Locator, gateTestPane)
			}
			switch tc.task {
			case "blocked":
				if task.Unblock != "coordinator" {
					t.Errorf("unblock = %q, want the coordinator named", task.Unblock)
				}
			case "done":
				if a.Outcome.Receipt != "pr #12" || a.EndedAt == "" {
					t.Errorf("done attempt = %+v, want the pr as receipt and an end time", a)
				}
			}
			if tc.timeout == "400ms" && !strings.Contains(task.Note, "gate timed out") {
				t.Errorf("note = %q, want the timeout said", task.Note)
			}
		})
	}
}

// On a fleet, a report that did not release the gate is still news about its
// worker, and the worker that did release is drawn too.
func TestGateDagrDrawsEveryWorkerInTheWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dagr", "run.json")
	f := newGateWorkers(t, "a", "b", "c")
	go func() {
		f.worker("a").prints(report{status: "planned", note: "reading the code"})
		f.worker("c").prints(report{status: "done", note: "landed"})
	}()

	if out, err := f.gateWithDagr(t, path, "--any", "a,b,c", "--timeout", "10s"); err != nil {
		t.Fatalf("gate did not release: %v\n%s", err, out)
	}

	doc := readDagr(t, path)
	if got := len(doc.Tasks); got != 3 {
		t.Fatalf("tasks = %d, want one per worker", got)
	}
	a := dagrTaskNamed(t, doc, "a")
	if a.State != "working" || a.Attempts[0].Progress == nil || a.Attempts[0].Progress.Note != "planned: reading the code" {
		t.Errorf("a = %+v, want working with its planned report as progress", a)
	}
	if b := dagrTaskNamed(t, doc, "b"); b.State != "working" {
		t.Errorf("b = %q, want working: it said nothing", b.State)
	}
	if c := dagrTaskNamed(t, doc, "c"); c.State != "done" {
		t.Errorf("c = %q, want done", c.State)
	}
}

func dagrTargets(names ...string) []gateTarget {
	var targets []gateTarget
	for i, name := range names {
		targets = append(targets, gateTarget{name: name, pane: "w" + string(rune('1'+i)) + ":p1"})
	}
	return targets
}

// A fleet is gated in rounds, and a later round does not forget what an earlier
// one saw. A worker gated again after it settled starts a second attempt rather
// than moving the first one back to working.
func TestGateDagrCarriesEarlierRoundsForward(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dagr", "run.json")
	clock := func() time.Time { return time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC) }

	first := dagrTargets("a", "b")
	e, err := openDagrExport(path, first, os.Stderr, clock)
	if err != nil {
		t.Fatal(err)
	}
	e.reported(first[1], report{status: "done", note: "landed"})
	e.write()

	// Round two drops b, as the docs say to.
	if _, err := openDagrExport(path, first[:1], os.Stderr, clock); err != nil {
		t.Fatal(err)
	}
	doc := readDagr(t, path)
	if b := dagrTaskNamed(t, doc, "b"); b.State != "done" {
		t.Errorf("b = %q after a round that did not watch it, want done kept", b.State)
	}
	if a := dagrTaskNamed(t, doc, "a"); len(a.Attempts) != 1 {
		t.Errorf("a was still working and gained attempts: %+v", a.Attempts)
	}

	// Round three gates b again.
	if _, err := openDagrExport(path, first[1:], os.Stderr, clock); err != nil {
		t.Fatal(err)
	}
	b := dagrTaskNamed(t, readDagr(t, path), "b")
	if b.State != "working" || len(b.Attempts) != 2 {
		t.Fatalf("b = %+v, want working on a second attempt", b)
	}
	if b.Attempts[0].State != "done" {
		t.Errorf("first attempt = %q, want its done kept", b.Attempts[0].State)
	}
	if a2 := b.Attempts[1]; a2.N != 2 || a2.ID != "b·a2" || a2.Cause.Type != "followup" || a2.Cause.Ref != "b·a1" {
		t.Errorf("second attempt = %+v, want n 2 following up b·a1", a2)
	}
}

// dagr reads one fixed path, so another producer may already own it. Its file
// is refused, untouched, before the wait opens.
func TestGateDagrRefusesAnotherProducersFile(t *testing.T) {
	for name, body := range map[string]string{
		"another run":     `{"dagr": 3, "run": {"id": "someone-else"}, "tasks": []}`,
		"not JSON at all": "not a run file",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "run.json")
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := openDagrExport(path, dagrTargets("a"), os.Stderr, time.Now)
			if err == nil || exitCode(err) != exitUsage {
				t.Fatalf("err = %v (exit %d), want a usage refusal", err, exitCode(err))
			}
			if got, _ := os.ReadFile(path); string(got) != body {
				t.Errorf("the other producer's file was rewritten:\n%s", got)
			}
		})
	}
}

// The run file lands in the user's checkout, so the directory worktender
// creates for it ignores itself. A .dagr the user already had is theirs.
func TestGateDagrKeepsItsFileOutOfGit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".dagr", "run.json")
	if _, err := openDagrExport(path, dagrTargets("a"), os.Stderr, time.Now); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, ".dagr", ".gitignore")); err != nil || string(got) != "*\n" {
		t.Errorf(".gitignore = %q (%v), want one ignoring everything", got, err)
	}
	// No temporary file is left beside the run file.
	entries, _ := os.ReadDir(filepath.Join(dir, ".dagr"))
	if len(entries) != 2 {
		t.Errorf(".dagr holds %d entries, want run.json and .gitignore only", len(entries))
	}

	theirs := filepath.Join(t.TempDir(), ".dagr")
	if err := os.Mkdir(theirs, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := openDagrExport(filepath.Join(theirs, "run.json"), dagrTargets("a"), os.Stderr, time.Now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(theirs, ".gitignore")); err == nil {
		t.Error("a .gitignore was added to a .dagr directory worktender did not create")
	}
}

// The view is an aside: a run file that cannot be written warns and leaves the
// gate's answer alone.
func TestGateDagrWriteFailureDoesNotChangeTheOutcome(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dagr", "run.json")
	w := newGateWorker(t, "working", "")
	go func() {
		// Replace the directory with a file once the gate has opened, so every
		// later write fails.
		<-w.fleet.opened
		dir := filepath.Dir(path)
		os.RemoveAll(dir)
		os.WriteFile(dir, nil, 0o644)
		w.prints(report{status: "done", note: "green"})
	}()

	out, err := w.fleet.gateWithDagr(t, path, "--target", "worker", "--timeout", "10s")
	if err != nil {
		t.Fatalf("a failed view write changed the gate's answer: %v\n%s", err, out)
	}
	if !strings.Contains(out, "--dagr:") || !strings.Contains(out, "the gate is not") {
		t.Errorf("the failed write was not said:\n%s", out)
	}
}
