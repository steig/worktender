package fleet

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseReadsTheContractShape(t *testing.T) {
	data := []byte(`{"v":1,"ts":"2026-08-25T10:00:00Z","task":"t1","type":"dispatch","by":"fleet","executor":"worker","target":"wt-abc","repo":"/home/x/proj","issue":42,"deadline":"2026-08-25T12:00:00Z"}
{"v":1,"ts":"2026-08-25T10:05:00Z","task":"t1","type":"report","status":"done","pr":7,"by":"fleet"}
`)
	l := Parse(data)
	if l.Malformed != 0 || l.Truncated || l.UnknownVersion {
		t.Fatalf("clean input flagged: %+v", l)
	}
	if len(l.Entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(l.Entries))
	}

	d := l.Entries[0]
	if d.Type != "dispatch" || d.Task != "t1" || d.By != "fleet" {
		t.Errorf("envelope misread: %+v", d)
	}
	if d.Executor != "worker" || d.Target != "wt-abc" || d.Repo != "/home/x/proj" || d.Issue != 42 {
		t.Errorf("dispatch fields misread: %+v", d)
	}
	if d.Deadline.IsZero() || !d.Deadline.Equal(time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("deadline misread: %v", d.Deadline)
	}
	if r := l.Entries[1]; r.Status != "done" || r.PR != 7 {
		t.Errorf("report fields misread: %+v", r)
	}
}

// The contract's concurrency rule: a final line without its newline is a write
// in flight, and readers MUST discard it rather than read half an entry.
func TestParseDropsTheUnterminatedTail(t *testing.T) {
	data := []byte(`{"v":1,"ts":"2026-08-25T10:00:00Z","task":"t1","type":"nudge","by":"fleet"}
{"v":1,"ts":"2026-08-25T10:01:00Z","task":"t2","type":"disp`)
	l := Parse(data)
	if !l.Truncated {
		t.Error("an unterminated tail went unnoticed")
	}
	if len(l.Entries) != 1 || l.Entries[0].Task != "t1" {
		t.Errorf("got %d entries, want only the terminated line", len(l.Entries))
	}
	// The half-line must not be counted malformed either: it is not a writer
	// bug, it is a write happening.
	if l.Malformed != 0 {
		t.Errorf("the in-flight write was counted as malformed")
	}
}

// A file that is one unterminated line has no entries at all — and must not
// panic reaching for the newline that is not there.
func TestParseSurvivesASingleUnterminatedLine(t *testing.T) {
	l := Parse([]byte(`{"v":1,"ts":"2026-08-25T1`))
	if len(l.Entries) != 0 || !l.Truncated {
		t.Errorf("got %+v, want no entries and Truncated", l)
	}
}

func TestParseCountsMalformedLinesAndKeepsGoing(t *testing.T) {
	data := []byte(`not json at all
{"v":1,"ts":"2026-08-25T10:00:00Z","task":"t1","type":"nudge","by":"fleet"}
{"ts":"2026-08-25T10:00:00Z","task":"t2","type":"nudge","by":"fleet"}
{"v":1,"ts":"yesterday teatime","task":"t3","type":"nudge","by":"fleet"}
`)
	l := Parse(data)
	if l.Malformed != 3 {
		t.Errorf("counted %d malformed lines, want 3 (bad JSON, missing v, bad ts)", l.Malformed)
	}
	if len(l.Entries) != 1 || l.Entries[0].Task != "t1" {
		t.Errorf("the good line did not survive its neighbours: %+v", l.Entries)
	}
}

// An entry with a version this reader does not know is kept — its Raw is what
// renders — but flagged, and Fold leaves it alone.
func TestParseFlagsUnknownVersions(t *testing.T) {
	data := []byte(`{"v":2,"ts":"2026-08-25T10:00:00Z","task":"t1","type":"dispatch","by":"fleet"}
`)
	l := Parse(data)
	if !l.UnknownVersion {
		t.Error("a v2 entry raised no warning")
	}
	if len(l.Entries) != 1 {
		t.Fatalf("the v2 entry was dropped rather than kept raw")
	}
	if tasks := Fold(l.Entries); len(tasks) != 0 {
		t.Errorf("a v2 entry was folded as if its fields meant what v1 says")
	}
}

// Unknown fields must be ignored — that is the contract's additive-evolution
// rule, and the one that lets the writer grow without a release lockstep.
func TestParseIgnoresUnknownFields(t *testing.T) {
	data := []byte(`{"v":1,"ts":"2026-08-25T10:00:00Z","task":"t1","type":"nudge","by":"fleet","novel_field":{"deep":true}}
`)
	l := Parse(data)
	if l.Malformed != 0 || len(l.Entries) != 1 {
		t.Errorf("an unknown field broke the line: %+v", l)
	}
}

func TestLoadTellsMissingApartFromUnreadable(t *testing.T) {
	dir := t.TempDir()

	_, found, err := Load(filepath.Join(dir, "no", "ledger.jsonl"))
	if err != nil || found {
		t.Errorf("a missing ledger should be found=false, no error; got found=%v err=%v", found, err)
	}

	path := filepath.Join(dir, "ledger.jsonl")
	if err := os.WriteFile(path, []byte(`{"v":1,"ts":"2026-08-25T10:00:00Z","task":"t1","type":"nudge","by":"fleet"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, found, err := Load(path)
	if err != nil || !found || len(l.Entries) != 1 {
		t.Errorf("a present ledger should load: found=%v err=%v entries=%d", found, err, len(l.Entries))
	}
}

func TestLedgerPathHonoursXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/xdg-state")
	if got, want := LedgerPath(), filepath.Join("/tmp/xdg-state", "fleet", "ledger.jsonl"); got != want {
		t.Errorf("LedgerPath() = %q, want %q", got, want)
	}
}
