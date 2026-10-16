package herdrtest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The helpers below own the shape of a faked `gh pr list --json state` answer,
// so a test names the states it means and never a JSON literal. When the
// question changes shape again — as it did in #96, from an object answer to an
// array one — every fake moves with it, because there is only one of them.
//
// They are built on FakeGh, which stays for the call sites that need something
// other than an answer: an exit code, a message on stderr, a record of argv.

// ghPR is one entry of what `gh pr list --json state` answers with.
type ghPR struct {
	State string `json:"state"`
}

// FakeGhPRState fakes a branch carrying one pull request in the given state.
func FakeGhPRState(t *testing.T, state string) {
	t.Helper()

	FakeGhPRStates(t, state)
}

// FakeGhPRStates fakes a branch carrying several pull requests, listed in the
// order gh would hand them back.
func FakeGhPRStates(t *testing.T, states ...string) {
	t.Helper()

	FakeGh(t, GhPRScript(t, states...))
}

// FakeGhNoPR fakes the answer for a branch that was never opened as a pull
// request: an empty list, and a success. That is not the same as gh failing,
// and the two mean opposite things to whoever reads the verdict.
func FakeGhNoPR(t *testing.T) {
	t.Helper()

	FakeGhPRStates(t)
}

// FakeGhUnavailable fakes a gh that cannot answer at all — not installed, not
// logged in, no such repository. Use FakeGh directly when the test cares which
// of those it was, or what gh said about it.
func FakeGhUnavailable(t *testing.T) {
	t.Helper()

	FakeGh(t, "exit 1")
}

// GhPRScript is the shell body the FakeGhPR* helpers install, for the few call
// sites that need to fake a side effect alongside the answer.
func GhPRScript(t *testing.T, states ...string) string {
	t.Helper()

	prs := make([]ghPR, len(states))
	for i, state := range states {
		prs[i].State = state
	}

	answer, err := json.Marshal(prs)
	if err != nil {
		panic("marshal fake gh answer: " + err.Error())
	}
	return CatScript(t, string(answer))
}

// CatScript is a shell command that prints payload byte-for-byte: it writes
// payload to a file in the test's temp dir and returns `cat` of that file.
//
// A fake's answer must not go through echo. Fakes run under /bin/sh, which on
// Ubuntu is dash, and dash's echo expands backslash escapes, so the `\n` a
// JSON string carries for a newline arrives as a raw newline and the answer is
// no longer JSON. Bash's echo leaves it alone, so the breakage shows only in
// CI (#211). Nor is printf a fix unless the payload stays out of the format
// string, and quoting the payload into the script invites the same class of
// bug for any quote it holds. A file sidesteps the shell entirely.
func CatScript(t *testing.T, payload string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatalf("write fake payload: %v", err)
	}
	return "cat '" + path + "'"
}
