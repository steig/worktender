// Package fleet reads the fleet ledger and builds the board over it.
//
// The ledger is the fleet-coordinator's append-only record of what it asked
// for and what came back. This package implements the reading half of
// docs/fleet-ledger-contract.md: the writer lives in the coordinator skill,
// and neither side owns the format alone.
package fleet

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Version is the ledger format this reader understands. The contract has the
// board reading the current version and the previous one; there is no previous
// yet, so an entry carrying any other version renders raw with a warning.
const Version = 1

// LedgerPath is where the contract fixes the ledger: machine-local XDG state,
// in no repository, never synced.
func LedgerPath() string {
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			// No home means no fixed path to answer with; an empty path makes
			// Load report "no ledger", which is the honest reading of a machine
			// where the convention the path hangs off does not exist.
			return ""
		}
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "fleet", "ledger.jsonl")
}

// Entry is one ledger line. The envelope fields are always set on an entry
// that parsed; the type-specific fields are zero when the line did not carry
// them, which for a well-formed writer means "not this entry's type".
type Entry struct {
	V    int
	TS   time.Time
	Task string
	Type string
	By   string

	// dispatch
	Executor string
	Target   string
	Repo     string
	Issue    int
	// dispatch and deadline both name a deadline; ack, report, verify and
	// deadline all carry a result-shaped word. One field each, because they
	// are one JSON field each.
	Deadline time.Time
	Result   string
	Status   string
	PR       int
	Severity string
	Reason   string
	Evidence string
	Note     string
	Action   string

	// Raw is the line as written, kept because the contract has unknown types
	// and unknown versions rendering raw rather than being interpreted.
	Raw string
}

// Ledger is one read of the file, with the counts a board must say out loud
// rather than fold into silence.
type Ledger struct {
	Entries []Entry
	// Malformed counts lines that were not JSON or lacked an envelope field.
	// Counted and kept out of Entries: one bad line must not cost the board
	// the fleet, and must not pass as an entry either.
	Malformed int
	// UnknownVersion is set when some entry carried a v this reader does not
	// know. Those entries are in Entries with their Raw intact, but nothing
	// folds them into tasks — under a breaking bump the fields cannot be
	// trusted to mean what v1 says they mean.
	UnknownVersion bool
	// Truncated is set when the final line lacked its newline: a write in
	// flight, dropped per the contract, not corruption.
	Truncated bool
}

// wireEntry is the decode target. Pointers where absence must be told apart
// from the zero value — a `v` of 0 is a malformed envelope, not version zero.
type wireEntry struct {
	V    *int    `json:"v"`
	TS   *string `json:"ts"`
	Task *string `json:"task"`
	Type *string `json:"type"`
	By   *string `json:"by"`

	Executor string `json:"executor"`
	Target   string `json:"target"`
	Repo     string `json:"repo"`
	Issue    int    `json:"issue"`
	Deadline string `json:"deadline"`
	Result   string `json:"result"`
	Verdict  string `json:"verdict"`
	Status   string `json:"status"`
	PR       int    `json:"pr"`
	Severity string `json:"severity"`
	Reason   string `json:"reason"`
	Evidence string `json:"evidence"`
	Note     string `json:"note"`
	Action   string `json:"action"`
}

// Load reads the ledger at path. A missing file is found=false and no error:
// the ledger does not exist until a controller writes its first entry, and a
// fleet with no controller yet is an ordinary state for the board to show.
func Load(path string) (Ledger, bool, error) {
	if path == "" {
		return Ledger{}, false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Ledger{}, false, nil
		}
		return Ledger{}, false, err
	}
	return Parse(data), true, nil
}

// Parse reads ledger bytes per the contract: one JSON object per line, final
// unterminated line dropped as a write in flight.
func Parse(data []byte) Ledger {
	var l Ledger
	if len(data) > 0 && data[len(data)-1] != '\n' {
		l.Truncated = true
		cut := bytes.LastIndexByte(data, '\n')
		if cut < 0 {
			return l
		}
		data = data[:cut+1]
	}

	for line := range bytes.Lines(data) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		entry, ok := parseEntry(line)
		if !ok {
			l.Malformed++
			continue
		}
		if entry.V != Version {
			l.UnknownVersion = true
		}
		l.Entries = append(l.Entries, entry)
	}
	return l
}

// parseEntry decodes one line. The envelope is required in full: a line
// missing any of the five fields is malformed, not an entry with gaps — the
// contract says every line carries all five, so a gap is a writer bug worth
// counting rather than a shape worth accommodating.
func parseEntry(line []byte) (Entry, bool) {
	var w wireEntry
	if err := json.Unmarshal(line, &w); err != nil {
		return Entry{}, false
	}
	if w.V == nil || w.TS == nil || w.Task == nil || w.Type == nil || w.By == nil {
		return Entry{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, *w.TS)
	if err != nil {
		return Entry{}, false
	}

	entry := Entry{
		V: *w.V, TS: ts, Task: *w.Task, Type: *w.Type, By: *w.By,
		Executor: w.Executor, Target: w.Target, Repo: w.Repo, Issue: w.Issue,
		Result: w.Result, Status: w.Status, PR: w.PR,
		Severity: w.Severity, Reason: w.Reason, Evidence: w.Evidence,
		Note: w.Note, Action: w.Action,
		Raw: string(line),
	}
	// Deployed writers spell two result-shaped words differently than the
	// contract does: verify entries arrive with `verdict` and ack entries
	// with `status` where the contract says `result`. The reader accepts
	// both spellings — a board that renders every pass as a fail because of
	// a field name is wrong in the way that matters, and the additive rule
	// already commits this reader to tolerating fields it did not expect.
	if entry.Result == "" {
		switch entry.Type {
		case "verify":
			entry.Result = w.Verdict
		case "ack":
			entry.Result = w.Status
		}
	}
	// A deadline that does not parse is dropped rather than failing the line:
	// it is a type-specific field, and the contract only makes the envelope
	// load-bearing.
	if w.Deadline != "" {
		if d, err := time.Parse(time.RFC3339Nano, w.Deadline); err == nil {
			entry.Deadline = d
		}
	}
	return entry, true
}
