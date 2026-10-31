package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/execute"
	"github.com/steig/worktender/internal/herdrtest"
)

// recordGh puts a gh on PATH that logs the GH_TOKEN it saw and its arguments
// and answers "{}". With behind set it then runs the fake gh already first on
// PATH, which the caller must have put there: never a real gh.
func recordGh(t *testing.T, behind bool) (log func() string) {
	t.Helper()

	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	t.Setenv("RECORD_GH_CALLS", calls)
	body := `#!/bin/sh
printf 'token=%s args=%s\n' "$GH_TOKEN" "$*" >> "$RECORD_GH_CALLS"
if [ -n "$RECORD_GH_BEHIND" ]; then PATH=${PATH#"$RECORD_GH_DIR":}; exec gh "$@"; fi
printf '{}'
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RECORD_GH_DIR", dir)
	if behind {
		t.Setenv("RECORD_GH_BEHIND", "1")
	} else {
		t.Setenv("RECORD_GH_BEHIND", "")
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_TOKEN", "")
	return func() string {
		raw, _ := os.ReadFile(calls)
		return string(raw)
	}
}

func repoWithOrigin(t *testing.T, url string) string {
	t.Helper()

	repo := herdrtest.NewRepo(t)
	repo.Git("remote", "add", "origin", url)
	return repo.Root
}

func TestReadTokenPicksTheOwnersToken(t *testing.T) {
	root := repoWithOrigin(t, "git@github.com:lamps-com/thing.git")
	log := recordGh(t, false)
	t.Setenv("WORKTENDER_READ_GH_TOKEN_LAMPS_COM", "read-lamps")
	t.Setenv("WORKTENDER_READ_GH_TOKEN_OTHER", "read-other")

	if _, err := ghReadOutput(root, "issue", "view", "7"); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := log(); got != "token=read-lamps args=issue view 7\n" {
		t.Errorf("gh saw %q, want the lamps-com token", got)
	}
}

func TestReadTokenForAnOwnerWithNoneFailsNamingTheVariable(t *testing.T) {
	root := repoWithOrigin(t, "https://github.com/steig/worktender.git")
	log := recordGh(t, false)
	t.Setenv("WORKTENDER_READ_GH_TOKEN_LAMPSCOM", "read-lampscom")

	_, err := ghReadOutput(root, "issue", "view", "7")
	if err == nil || !strings.Contains(err.Error(), "WORKTENDER_READ_GH_TOKEN_STEIG") {
		t.Fatalf("error = %v, want one naming WORKTENDER_READ_GH_TOKEN_STEIG", err)
	}
	if strings.Contains(err.Error(), "read-lampscom") {
		t.Errorf("error leaks a token: %v", err)
	}
	if got := log(); got != "" {
		t.Errorf("gh ran on its own login: %q", got)
	}
}

func TestReadTokenForAnUnreadableOwnerFails(t *testing.T) {
	root := repoWithOrigin(t, "https://gitlab.example/steig/worktender.git")
	log := recordGh(t, false)
	t.Setenv("WORKTENDER_READ_GH_TOKEN_STEIG", "read-steig")

	if _, err := ghReadOutput(root, "issue", "view", "7"); err == nil {
		t.Fatal("read for an owner that cannot be read succeeded")
	}
	if got := log(); got != "" {
		t.Errorf("gh ran: %q", got)
	}
}

func TestReadTokenUnsetLeavesGhAlone(t *testing.T) {
	root := repoWithOrigin(t, "https://github.com/steig/worktender.git")
	log := recordGh(t, false)
	t.Setenv("GH_TOKEN", "ambient")

	if _, err := ghReadOutput(root, "issue", "view", "7"); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := log(); got != "token=ambient args=issue view 7\n" {
		t.Errorf("gh saw %q, want the ambient token untouched", got)
	}
}

func TestReadTokenIsNotAnOwnerWorkerToken(t *testing.T) {
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "WORKTENDER_WORKER_GH_TOKEN") {
			t.Skip("worker token variables are set in this environment")
		}
	}
	t.Setenv("WORKTENDER_READ_GH_TOKEN_STEIG", "read-steig")
	if execute.OwnerTokensSet() {
		t.Error("OwnerTokensSet sees a read token")
	}
	if !execute.ReadTokensSet() {
		t.Error("ReadTokensSet misses a read token")
	}
	if got := execute.ReadTokenEnv("lamps-com"); got != "WORKTENDER_READ_GH_TOKEN_LAMPS_COM" {
		t.Errorf("ReadTokenEnv = %q", got)
	}
}

func TestReadTokenNeverReachesApprovesWrites(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	repo.Git("remote", "add", "origin", "https://github.com/steig/worktender.git")
	gh := newFakeGitHub(t, readyMeta, snapshotJSON(t, readyTitle, readyBody), nil)
	log := recordGh(t, true)
	t.Setenv("WORKTENDER_READ_GH_TOKEN_STEIG", "read-steig")
	_, hash, _, _ := approvalPayload(42, readyTitle, readyBody)

	if err := approveCommand([]string{"42", "--sha256", hash, "--repo", repo.Root}, &strings.Builder{}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if gh.read("posted") == "" {
		t.Fatal("approve posted nothing")
	}
	var reads, writes int
	for _, line := range strings.Split(strings.TrimSpace(log()), "\n") {
		switch {
		case !strings.HasPrefix(line, "token="):
			// a continuation of a multi-line argument
		case strings.Contains(line, "args=issue comment"), strings.Contains(line, "args=issue edit"):
			writes++
			if !strings.HasPrefix(line, "token= ") {
				t.Errorf("write saw a token: %q", line)
			}
		default:
			reads++
			if !strings.HasPrefix(line, "token=read-steig ") {
				t.Errorf("read did not get the read token: %q", line)
			}
		}
	}
	if reads == 0 || writes != 2 {
		t.Errorf("saw %d reads and %d writes, want some reads and 2 writes", reads, writes)
	}
}

func TestReadTokenStatsReadsByTheRecordedOrigin(t *testing.T) {
	log := recordGh(t, false)
	t.Setenv("WORKTENDER_READ_GH_TOKEN_STEIG", "read-steig")
	var v map[string]any

	err := ghJSON(ledgerEvent{Origin: "https://github.com/steig/worktender.git"}, []string{"pr", "view", "3"}, &v)
	if err != nil {
		t.Fatalf("ghJSON: %v", err)
	}
	if !strings.HasPrefix(log(), "token=read-steig args=pr view 3 --repo ") {
		t.Errorf("gh saw %q", log())
	}
	if err := ghJSON(ledgerEvent{Origin: "https://github.com/other/x.git"}, []string{"pr", "view", "3"}, &v); err == nil ||
		!strings.Contains(err.Error(), "WORKTENDER_READ_GH_TOKEN_OTHER") {
		t.Errorf("error = %v, want one naming WORKTENDER_READ_GH_TOKEN_OTHER", err)
	}
}
