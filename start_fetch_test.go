package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func fetchGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fetchClone is a clone of a local bare origin with one commit on main.
func fetchClone(t *testing.T) (bare, clone string) {
	t.Helper()
	dir := t.TempDir()
	bare = filepath.Join(dir, "origin.git")
	clone = filepath.Join(dir, "clone")
	fetchGit(t, dir, "init", "--bare", "-b", "main", bare)
	fetchGit(t, dir, "clone", bare, clone)
	fetchGit(t, clone, "checkout", "-B", "main")
	fetchGit(t, clone, "commit", "--allow-empty", "-m", "one")
	fetchGit(t, clone, "push", "-u", "origin", "main")
	return bare, clone
}

func TestFetchBaseAdvancesARemoteTrackingRef(t *testing.T) {
	bare, clone := fetchClone(t)
	other := filepath.Join(t.TempDir(), "other")
	fetchGit(t, filepath.Dir(other), "clone", bare, other)
	fetchGit(t, other, "commit", "--allow-empty", "-m", "two")
	fetchGit(t, other, "push", "origin", "main")
	want := fetchGit(t, other, "rev-parse", "HEAD")

	if err := fetchBase(clone, "origin/main"); err != nil {
		t.Fatal(err)
	}
	if got := fetchGit(t, clone, "rev-parse", "origin/main"); got != want {
		t.Errorf("origin/main = %s, want %s", got, want)
	}
}

func TestFetchBaseLeavesALocalRefAlone(t *testing.T) {
	_, clone := fetchClone(t)
	fetchGit(t, clone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing"))
	if err := fetchBase(clone, "main"); err != nil {
		t.Errorf("fetchBase(main) = %v, want nil", err)
	}
}

func TestFetchBaseReportsAnUnreachableRemote(t *testing.T) {
	_, clone := fetchClone(t)
	fetchGit(t, clone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing"))
	err := fetchBase(clone, "origin/main")
	if err == nil || !strings.Contains(err.Error(), "origin/main") {
		t.Errorf("fetchBase(origin/main) = %v, want an error naming origin/main", err)
	}
}
