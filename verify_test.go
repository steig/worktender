package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

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
		"`cat added.txt` prints hello",
		"`test -f README.md` exits 0",
		"`test -f untracked.txt` exits 1",
		"`test -f nowhere.txt` exits 0",
		"`true` exits quickly",
		"`true` prints nothing and exits 0",
		"`kill -KILL $$` exits non-zero",
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
		"7. unjudged ",
		"8. unjudged       base " + shortOid(base) + " killed by a signal, head " + shortOid(head) + " killed by a signal",
	}
	for _, w := range want {
		if !strings.Contains(out.String(), w) {
			t.Errorf("output lacks %q:\n%s", w, out.String())
		}
	}
	if !strings.Contains(err.Error(), "1 check(s) failing on head, 3 unjudged") {
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
	// sh stays sleep's parent, and sleep holds the output open, so killing sh
	// alone would leave verify waiting out WaitDelay on each side.
	repo := verifyRepo(t, "`test -f added.txt` exits 0", "`sleep 30; true` exits 0")

	var out strings.Builder
	started := time.Now()
	err := verifyCommand([]string{"42", "--repo", repo.Root, "--timeout", "200ms"}, &out)
	if exitCode(err) != exitNeedsHuman || !strings.Contains(out.String(), "2. unjudged       base") ||
		!strings.Contains(out.String(), "timed out") {
		t.Fatalf("verify with a check that hangs: %v (exit %d); want it unjudged and timed out\n%s", err, exitCode(err), out.String())
	}
	if took := time.Since(started); took > 3*time.Second {
		t.Errorf("verify took %s; the timeout should kill the check's children, not wait on them", took)
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

// A check that writes a file does not leave it for the next, so a check's
// class does not depend on the order it was approved in.
func TestVerifyRunsEachCheckInAFreshExport(t *testing.T) {
	repo := verifyRepo(t, "`touch made.txt` exits 0", "`test -f made.txt` exits 1", "`test -f added.txt` exits 0")

	var out strings.Builder
	if err := verifyCommand([]string{"42", "--repo", repo.Root}, &out); err != nil {
		t.Fatalf("verify: %v\n%s", err, out.String())
	}
}

func TestVerifyRefusesAPullRequestForAnotherIssue(t *testing.T) {
	for _, tc := range []struct {
		name, branch, closes string
		want                 int
	}{
		{"unrelated", "7-other-work", "[]", exitNeedsHuman},
		{"named for the issue", "42-the-thing", "[]", exitOK},
		{"named for the issue under a type/", "feat/42-the-thing", "[]", exitOK},
		{"closes the issue", "some-branch", `[{"number":42}]`, exitOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := verifyRepo(t, "`test -f added.txt` exits 0")
			herdrtest.WriteFile(t, os.Getenv("FAKE_GH_DIR")+"/pr.json", `{"number":3,"state":"OPEN","headRefName":"`+tc.branch+
				`","headRefOid":"`+strings.TrimSpace(repo.Git("rev-parse", "HEAD"))+
				`","baseRefOid":"`+strings.TrimSpace(repo.Git("rev-parse", "main"))+`","closingIssuesReferences":`+tc.closes+`}`)

			var out strings.Builder
			err := verifyCommand([]string{"42", "--repo", repo.Root, "--pr", "3"}, &out)
			if exitCode(err) != tc.want {
				t.Fatalf("verify --pr 3 on %s: %v (exit %d); want exit %d\n%s", tc.branch, err, exitCode(err), tc.want, out.String())
			}
		})
	}
}

// A test the worker wrote is graded only by passing, so verify runs the
// changed tests with the changed code put back as it was on base: a test that
// still passes there does not test the change.
func TestVerifyRefusesATestThatPassesWithTheCodeReverted(t *testing.T) {
	t.Setenv("PATH", filepath.Join(runtime.GOROOT(), "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tc := range []struct {
		name, test, says string
		want             int
	}{
		{"hollow", "func TestDouble(t *testing.T) { if Double(0) != 0 { t.Fatal(Double(0)) } }",
			"hollow: tests pass with the change reverted (TestDouble)", exitNeedsHuman},
		{"needs the change", "func TestDouble(t *testing.T) { if Double(2) != 4 { t.Fatal(Double(2)) } }", "", exitOK},
		{"calls something new", "func TestTriple(t *testing.T) { if Triple(2) != 6 { t.Fatal(Triple(2)) } }",
			"do not build with the change reverted", exitNeedsHuman},
		{"skipped", "func TestDouble(t *testing.T) { t.Skip(\"later\") }", "no changed test ran", exitNeedsHuman},
		{"unfound", "var _ = testing.Short\n\nfunc TestDouble(t *T) {}\n\ntype T = testing.T",
			"hold no test verify can find", exitNeedsHuman},
		// The README beside the Go change is not code, so it keeps head's text
		// and a test that reads only it passes on the revert.
		{"README left alone", "import \"os\"\n\nfunc TestReadme(t *testing.T) { if b, _ := os.ReadFile(\"../README.md\"); string(b) != \"head\\n\" { t.Fatal(string(b)) } }",
			"hollow: tests pass with the change reverted (TestReadme)", exitNeedsHuman},
		{"go.mod reverted", "import (\"os\"; \"strings\")\n\nfunc TestGoMod(t *testing.T) { if b, _ := os.ReadFile(\"../go.mod\"); !strings.Contains(string(b), \"// head\") { t.Fatal(string(b)) } }",
			"", exitOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := verifyRepo(t, "`test -f added.txt` exits 0")
			repo.Git("checkout", "main")
			repo.Write("go.mod", "module example.com/m\n\ngo 1.21\n")
			repo.Write("lib/lib.go", "package lib\n\nfunc Double(x int) int { return x }\n")
			repo.Git("add", ".")
			repo.Git("commit", "-m", "the library")
			repo.Git("checkout", "42-the-thing")
			repo.Git("rebase", "main")
			repo.Write("lib/lib.go", "package lib\n\nfunc Double(x int) int { return 2 * x }\n\nfunc Triple(x int) int { return 3 * x }\n")
			repo.Write("README.md", "head\n")
			repo.Write("go.mod", "module example.com/m\n\ngo 1.21\n\n// head\n")
			repo.Write("lib/lib_test.go", "package lib\n\nimport \"testing\"\n\n"+tc.test+"\n")
			repo.Git("add", ".")
			repo.Git("commit", "-m", "double it")

			var out strings.Builder
			err := verifyCommand([]string{"42", "--repo", repo.Root}, &out)
			if exitCode(err) != tc.want {
				t.Fatalf("verify: %v (exit %d); want exit %d\n%s", err, exitCode(err), tc.want, out.String())
			}
			if tc.want == exitNeedsHuman && !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error = %v; want it to say %q", err, tc.says)
			}
			if tc.want == exitOK && !strings.Contains(out.String(), "revert: the changed tests fail with") {
				t.Errorf("output does not say the tests failed with the change reverted:\n%s", out.String())
			}
		})
	}
}

func TestVerifySkipsTheRevertWithNoGoSourceChange(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	repo.Git("checkout", "-b", "42-the-thing")
	repo.Write("lib_test.go", "package lib\n\nimport \"testing\"\n\nfunc TestNothing(t *testing.T) {}\n")
	repo.Write("README.md", "a README edit is not code the test could need\n")
	repo.Git("add", ".")
	repo.Git("commit", "-m", "a test alone")
	body := "## Acceptance\n- [ ] `test -f lib_test.go` exits 0\n"
	newFakeGitHub(t, readyMeta, snapshotJSON(t, readyTitle, body, comment{"steig", approvalOf(t, readyTitle, body)}),
		map[string]string{"steig": "admin"})

	var out strings.Builder
	if err := verifyCommand([]string{"42", "--repo", repo.Root}, &out); err != nil {
		t.Fatalf("verify: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "revert: skipped, the change touches no Go source, go.mod or go.sum") {
		t.Errorf("output does not say the revert was skipped:\n%s", out.String())
	}
}

// fakePackageManager puts a pnpm on PATH that logs its arguments and makes a
// node_modules, exiting with code.
func fakePackageManager(t *testing.T, code int) (log string) {
	t.Helper()
	bin := t.TempDir()
	log = filepath.Join(bin, "pnpm.log")
	fake := "#!/bin/sh\necho \"$@\" >> " + log + "\nmkdir node_modules\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "pnpm"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func commitLockfile(repo *herdrtest.Repo) {
	repo.Write("pnpm-lock.yaml", "lockfileVersion: '9.0'\n")
	repo.Git("add", "pnpm-lock.yaml")
	repo.Git("commit", "-m", "lock")
}

// An export holds no node_modules, so a check that needs installed packages
// fails there until the lockfile's dependencies are installed. The install runs
// once per commit, not once per check, and runs no lifecycle scripts.
func TestVerifyInstallsDependenciesOncePerCommitWithoutScripts(t *testing.T) {
	log := fakePackageManager(t, 0)
	repo := verifyRepo(t, "`test -d node_modules` exits 0", "`test -f added.txt` exits 0")
	commitLockfile(repo)

	var out strings.Builder
	if err := verifyCommand([]string{"42", "--repo", repo.Root}, &out); err != nil {
		t.Fatalf("verify: %v\n%s", err, out.String())
	}
	got, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	// Only head tracks the lockfile; per check it would have run twice.
	if len(lines) != 1 {
		t.Errorf("pnpm ran %d times for 2 checks; want once:\n%s", len(lines), got)
	}
	for _, l := range lines {
		if l != "install --frozen-lockfile --ignore-scripts" {
			t.Errorf("pnpm ran with %q; want install --frozen-lockfile --ignore-scripts", l)
		}
	}
}

func TestVerifyRecordsAFailedInstallAsFailingChecks(t *testing.T) {
	fakePackageManager(t, 1)
	repo := verifyRepo(t, "`test -f added.txt` exits 0")
	commitLockfile(repo)

	var out strings.Builder
	err := verifyCommand([]string{"42", "--repo", repo.Root}, &out)
	if exitCode(err) != exitNeedsHuman {
		t.Fatalf("verify with an install that fails: %v (exit %d); want exit 3\n%s", err, exitCode(err), out.String())
	}
	if !strings.Contains(out.String(), "1. failing") {
		t.Errorf("output lacks a failing check:\n%s", out.String())
	}
}

func TestVerifyInstallsNothingWithoutALockfile(t *testing.T) {
	log := fakePackageManager(t, 0)
	repo := verifyRepo(t, "`test -f added.txt` exits 0")

	var out strings.Builder
	if err := verifyCommand([]string{"42", "--repo", repo.Root}, &out); err != nil {
		t.Fatalf("verify: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(log); err == nil {
		t.Errorf("pnpm ran with no lockfile tracked")
	}
}

func fakePnpm(t *testing.T, script string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "pnpm"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestVerifyNotesTheTailOfAFailedInstall(t *testing.T) {
	fakePnpm(t, "echo 'ERR_PNPM_OUTDATED_LOCKFILE the lockfile is stale'\nexit 1")
	repo := verifyRepo(t, "`test -f added.txt` exits 0")
	commitLockfile(repo)

	var out strings.Builder
	err := verifyCommand([]string{"42", "--repo", repo.Root}, &out)
	if exitCode(err) != exitNeedsHuman {
		t.Fatalf("verify: %v (exit %d); want exit 3\n%s", err, exitCode(err), out.String())
	}
	for _, want := range []string{"failed (pnpm-lock.yaml: exit 1)", "ERR_PNPM_OUTDATED_LOCKFILE the lockfile is stale"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestVerifyNotesAnInstallThatTimedOut(t *testing.T) {
	fakePnpm(t, "echo resolving\nexec sleep 30")
	old := installTimeout
	installTimeout = 2 * time.Second
	t.Cleanup(func() { installTimeout = old })
	repo := verifyRepo(t, "`test -f added.txt` exits 0")
	commitLockfile(repo)

	var out strings.Builder
	err := verifyCommand([]string{"42", "--repo", repo.Root}, &out)
	if exitCode(err) != exitNeedsHuman {
		t.Fatalf("verify: %v (exit %d); want exit 3\n%s", err, exitCode(err), out.String())
	}
	for _, want := range []string{"failed (pnpm-lock.yaml: timed out)", "resolving"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// A lockfile whose installer is not installed is the machine's problem, not a
// finding about the commit, so verify stops with the environment code.
func TestVerifyStopsWhenTheInstallerIsMissing(t *testing.T) {
	repo := verifyRepo(t, "`test -f added.txt` exits 0")
	commitLockfile(repo)
	bin := t.TempDir()
	for _, tool := range []string{"git", "gh", "cat", "sh"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)

	var out strings.Builder
	err := verifyCommand([]string{"42", "--repo", repo.Root}, &out)
	if exitCode(err) != exitEnvironment {
		t.Fatalf("verify: %v (exit %d); want exit 2\n%s", err, exitCode(err), out.String())
	}
	if !strings.Contains(err.Error(), "install dependencies on") {
		t.Errorf("error = %v; want it to name the install step", err)
	}
}

func TestVerifyNotesAnUnsupportedLockfile(t *testing.T) {
	for _, lock := range []string{"yarn.lock", "bun.lockb"} {
		t.Run(lock, func(t *testing.T) {
			log := fakePackageManager(t, 0)
			repo := verifyRepo(t, "`test -f added.txt` exits 0")
			repo.Write(lock, "# lock\n")
			repo.Git("add", lock)
			repo.Git("commit", "-m", "lock")

			var out strings.Builder
			if err := verifyCommand([]string{"42", "--repo", repo.Root}, &out); err != nil {
				t.Fatalf("verify: %v\n%s", err, out.String())
			}
			if want := lock + " is not supported"; !strings.Contains(out.String(), want) {
				t.Errorf("output lacks %q:\n%s", want, out.String())
			}
			if _, err := os.Stat(log); err == nil {
				t.Errorf("an installer ran for %s", lock)
			}
		})
	}
}

func TestVerifyEscapesAndBoundsTheInstallOutputItNotes(t *testing.T) {
	// \r alone separates the progress lines; a line longer than any cap; a
	// credential an .npmrc dump would print.
	fakePnpm(t, `printf '\033[31mred\342\200\256gnp\n'
echo '//registry.npmjs.org/:_authToken=npm_abcdefghijklmnop1234'
echo 'NPM_TOKEN=hunter2hunter2'
printf 'progress 1\rprogress 2\r'
head -c 5000 /dev/zero | tr '\0' 'x'
echo
exit 1`)
	repo := verifyRepo(t, "`test -f added.txt` exits 0")
	commitLockfile(repo)

	var out strings.Builder
	if err := verifyCommand([]string{"42", "--repo", repo.Root}, &out); exitCode(err) != exitNeedsHuman {
		t.Fatalf("verify: %v (exit %d); want exit 3\n%s", err, exitCode(err), out.String())
	}
	got := out.String()
	for _, want := range []string{`\u{001B}[31mred\u{202E}gnp`, "progress 2", "_authToken=***", "NPM_TOKEN=***"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"\x1b", "\u202e", "\r", "npm_abcdefghijklmnop1234", "hunter2", strings.Repeat("x", 300)} {
		if strings.Contains(got, bad) {
			t.Errorf("output holds %q:\n%s", bad, got)
		}
	}
	if len(got) > 6000 {
		t.Errorf("notes are %d bytes; a 5000-byte line was meant to be cut", len(got))
	}
}

func TestTailLinesMasksURLCredentialsBasicAuthAndQuotedValues(t *testing.T) {
	for in, leaked := range map[string]string{
		"fetch https://deploy:s3cr3tpw@registry.example.com/pkg":    "s3cr3tpw",
		"fetch https://ghp_onlytokenvalue@registry.example.com/":    "onlytokenvalue",
		"Authorization: Basic dXNlcjpodW50ZXIy":                     "dXNlcjpodW50ZXIy",
		"fetch https://u:p@ss@registry.example.com/pkg":             "ss@registry",
		"fetch https://u:pa/ss@registry.example.com/pkg":            "pa/ss",
		`TOKEN="unterminated two words`:                             "two words",
		"Authorization: Digest username=\"u\", response=\"abc123\"": "abc123",
		"Authorization: Token abcdef123456":                         "abcdef123456",
		`NPM_TOKEN="hunter two three" next`:                         "two three",
		`_password='hunter two'`:                                    "hunter two",
	} {
		got := tailLines(in, 5)
		if strings.Contains(got, leaked) || !strings.Contains(got, "***") {
			t.Errorf("tailLines(%q) = %q; want %q masked", in, got, leaked)
		}
	}
	if got := tailLines("fetch https://registry.example.com/pkg", 5); strings.Contains(got, "***") {
		t.Errorf("a URL with no credentials was masked: %q", got)
	}
}

func TestVerifyPutsTheFailedInstallInJSON(t *testing.T) {
	fakePnpm(t, "echo 'ERR_PNPM_OUTDATED_LOCKFILE stale'\necho 'NPM_TOKEN=hunter2hunter2'\nexit 1")
	repo := verifyRepo(t, "`test -f added.txt` exits 0")
	commitLockfile(repo)

	var out strings.Builder
	if err := verifyCommand([]string{"42", "--repo", repo.Root, "--json"}, &out); exitCode(err) != exitNeedsHuman {
		t.Fatalf("verify: %v (exit %d); want exit 3\n%s", err, exitCode(err), out.String())
	}
	var doc verifyJSON
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("verify --json is not a document: %v\n%s", err, out.String())
	}
	if len(doc.Install) == 0 || doc.Install[0].Lockfile != "pnpm-lock.yaml" || doc.Install[0].Outcome != "exit 1" ||
		!strings.Contains(doc.Install[0].Output, "ERR_PNPM_OUTDATED_LOCKFILE") {
		t.Fatalf("install = %+v; want the failed pnpm install with its output", doc.Install)
	}
	if strings.Contains(out.String(), "hunter2") {
		t.Errorf("the JSON holds an unmasked credential:\n%s", out.String())
	}
}

func TestVerifyNotesAFailedInstallWithNoOutputWithoutAColon(t *testing.T) {
	fakePnpm(t, "printf '\\r\\n\\r\\n'\nexit 1")
	repo := verifyRepo(t, "`test -f added.txt` exits 0")
	commitLockfile(repo)

	var out strings.Builder
	if err := verifyCommand([]string{"42", "--repo", repo.Root}, &out); exitCode(err) != exitNeedsHuman {
		t.Fatalf("verify: %v (exit %d); want exit 3\n%s", err, exitCode(err), out.String())
	}
	if !strings.Contains(out.String(), "(pnpm-lock.yaml: exit 1)\n") || strings.Contains(out.String(), "pnpm-lock.yaml: exit 1):") {
		t.Errorf("a failed install with no output left a dangling colon:\n%s", out.String())
	}
}
