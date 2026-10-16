package wt

import (
	"os"
	"path/filepath"
	"time"

	"github.com/steig/worktender/internal/herdrapi"
)

// sessionLogActivity is when the agent's own session log was last written, and
// false when the log cannot be found.
//
// This is the clock herdr does not have. herdr names each agent's session —
// `agent_session` on agent.list, measured on herdr 0.9.5 as `kind: "id"` with
// the agent's own session id as the value — and the agent appends to a log
// file named after that id as it works. The file's mtime is the last time the
// agent wrote anything, which is the closest thing to "last active" that exists
// without asking the agent.
//
// Only the mtime is read, never the file. These logs are the agent's whole
// conversation, and a listing has no business with any of it; a stat answers
// the one question asked and cannot leak the rest.
//
// The file is found by globbing rather than by recomputing the path. Claude
// files a session under a directory derived from the cwd it started in, and
// that derivation is Claude's to change; the session id alone is what is stable.
// Codex files under sessions/YYYY/MM/DD/, so the glob is that fixed depth rather
// than a recursive walk over every rollout ever written.
//
// Nothing is guessed. An agent kind with no known layout, a session given as
// anything but an id, an id that is not a plain token, and a log that is not
// where it should be all answer false, and the column stays empty.
func sessionLogActivity(home string, session *herdrapi.AgentSessionInfo) (time.Time, bool) {
	if home == "" || session == nil || session.Kind != herdrapi.AgentSessionRefKindID || !plainSessionID(session.Value) {
		return time.Time{}, false
	}

	var pattern string
	switch session.Agent {
	case "claude":
		pattern = filepath.Join(home, ".claude", "projects", "*", session.Value+".jsonl")
	case "codex":
		pattern = filepath.Join(home, ".codex", "sessions", "*", "*", "*", "rollout-*-"+session.Value+".jsonl")
	default:
		return time.Time{}, false
	}

	matches, err := filepath.Glob(pattern)
	if err != nil {
		return time.Time{}, false
	}
	// More than one match is the same session filed twice — a resume from a
	// different directory does that — so the latest write is still this
	// session's last activity rather than a guess between two agents.
	var latest time.Time
	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	return latest, !latest.IsZero()
}

// plainSessionID reports whether a session id is safe to put in a glob. The id
// comes from herdr, which had it from the agent, and a separator or a glob
// metacharacter in it would turn a lookup of one file into a search of others.
func plainSessionID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
