package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/herdrtest"
)

// verifyRepo is a repository whose main is the base and whose checked-out
// branch adds added.txt, with an approved #42 carrying checks. An untracked
// file sits in the checkout, which no export holds.
func verifyRepo(t *testing.T, checks ...string) *herdrtest.Repo {
	t.Helper()

	repo := herdrtest.NewRepo(t)
	repo.Git("checkout", "-b", "42-the-thing")
	repo.Write("added.txt", "hello from head\n")
	repo.Git("add", "added.txt")
	repo.Git("commit", "-m", "add the thing")
	repo.Write("untracked.txt", "only in the checkout\n")

	body := "## Problem\nIt is missing.\n\n## Acceptance\n"
	for _, c := range checks {
		body += "- [ ] " + c + "\n"
	}
	newFakeGitHub(t, readyMeta, snapshotJSON(t, readyTitle, body, comment{"steig", approvalOf(t, readyTitle, body)}),
		map[string]string{"steig": "admin"})
	return repo
}

func TestVerifyClassifiesEachCheckOnBaseAndHead(t *testing.T) {
	repo := verifyRepo(t,
		"`test -f added.txt` exits 0",
		"`cat added.txt` prints hello from head",
		"`test -f README.md` exits 0",
		"`test -f untracked.txt` exits 1",
		"`test -f nowhere.txt` exits 0",
		"`true` exits quickly",
	)
	base := strings.TrimSpace(repo.Git("rev-parse", "main"))
	head := strings.TrimSpace(repo.Git("rev-parse", "HEAD"))

	var out strings.Builder
	err := verifyCommand([]string{"42", "--repo", repo.Root}, &out)
	if exitCode(err) != exitNeedsHuman {
		t.Fatalf("verify with a check failing on head: %v (exit %d); want exit 3\n%s", err, exitCode(err), out.String())
	}
	want := []string{
		"1. discriminating base " + shortOid(base) + " failed (exit 1), head " + shortOid(head) + " passed (exit 0): `test -f added.txt` exits 0",
		"2. discriminating base " + shortOid(base) + " failed (exit 1), head " + shortOid(head) + " passed (exit 0)",
		"3. guard          base " + shortOid(base) + " passed (exit 0), head " + shortOid(head) + " passed (exit 0)",
		"4. guard ",
		"5. failing        base " + shortOid(base) + " failed (exit 1), head " + shortOid(head) + " failed (exit 1)",
		"6. unjudged ",
	}
	for _, w := range want {
		if !strings.Contains(out.String(), w) {
			t.Errorf("output lacks %q:\n%s", w, out.String())
		}
	}
	if !strings.Contains(err.Error(), "1 check(s) failing on head, 1 unjudged") {
		t.Errorf("error = %v; want it to count the failing and unjudged checks", err)
	}
}

func TestVerifyNeedsHumanWhenNoCheckDiscriminates(t *testing.T) {
	repo := verifyRepo(t, "`test -f README.md` exits 0", "`test -f nowhere.txt` exits 1")

	var out strings.Builder
	err := verifyCommand([]string{"42", "--repo", repo.Root}, &out)
	if exitCode(err) != exitNeedsHuman {
		t.Fatalf("verify with only guards: %v (exit %d); want exit 3\n%s", err, exitCode(err), out.String())
	}
	if !strings.Contains(err.Error(), "every check already passes on base") {
		t.Errorf("error = %v; want it to say no check shows the change", err)
	}
}

func TestVerifyPassesWhenOneDiscriminatesAndNoneFails(t *testing.T) {
	repo := verifyRepo(t, "`test -f added.txt` exits 0", "`test -f README.md` exits 0")

	var out strings.Builder
	if err := verifyCommand([]string{"42", "--repo", repo.Root, "--json"}, &out); err != nil {
		t.Fatalf("verify: %v\n%s", err, out.String())
	}
	var doc verifyJSON
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("verify --json is not a document: %v\n%s", err, out.String())
	}
	if len(doc.Checks) != 2 || doc.Checks[0].Class != verifyDiscriminating || doc.Checks[1].Class != verifyGuard {
		t.Fatalf("checks = %+v; want one discriminating, one guard", doc.Checks)
	}
	if doc.Base == nil || doc.Head == nil || doc.Checks[0].Head.Commit != *doc.Head || doc.ExitCode != 0 {
		t.Errorf("document = %s; want both commits and exit 0", out.String())
	}
}

func TestVerifyTimesOutACheckAsUnjudged(t *testing.T) {
	repo := verifyRepo(t, "`test -f added.txt` exits 0", "`exec sleep 30` exits 0")

	var out strings.Builder
	err := verifyCommand([]string{"42", "--repo", repo.Root, "--timeout", "200ms"}, &out)
	if exitCode(err) != exitNeedsHuman || !strings.Contains(out.String(), "2. unjudged       base") ||
		!strings.Contains(out.String(), "timed out") {
		t.Fatalf("verify with a check that hangs: %v (exit %d); want it unjudged and timed out\n%s", err, exitCode(err), out.String())
	}
}

func TestVerifyRefusesAnIssueEditedSinceApproval(t *testing.T) {
	repo := verifyRepo(t, "`test -f added.txt` exits 0")
	edited := "## Acceptance\n- [ ] `true` exits 0\n"
	newFakeGitHub(t, readyMeta, snapshotJSON(t, readyTitle, edited, comment{"steig", approvalOf(t, readyTitle, "## Acceptance\n- [ ] `false` exits 1\n")}),
		map[string]string{"steig": "admin"})

	var out strings.Builder
	err := verifyCommand([]string{"42", "--repo", repo.Root}, &out)
	if exitCode(err) != exitNeedsHuman || strings.Contains(out.String(), "base:") {
		t.Fatalf("verify of an unapproved issue: %v (exit %d); want 3 before any check runs\n%s", err, exitCode(err), out.String())
	}
}
