package main

import (
	"strings"
	"testing"

	"github.com/steig/worktender/internal/herdrtest"
)

func TestLoadRepoConf(t *testing.T) {
	str := func(s string) *string { return &s }
	for _, tc := range []struct {
		name, file string
		prefix     *string
		base       *string
		warn       string
		err        string
	}{
		{name: "missing"},
		{name: "both", file: "# how we work\nbranch_prefix = \"chore/\"\nbase = 'release'  # not main\n", prefix: str("chore/"), base: str("release")},
		{name: "empty prefix declares none", file: "branch_prefix = \"\"\n", prefix: str("")},
		{name: "unknown key warns", file: "mode = \"direct-pr\"\n", warn: `.worktender.toml:1: unknown key "mode" ignored`},
		{name: "no equals", file: "base = \"x\"\nbranch_prefix\n", err: `.worktender.toml:2: want key = "value"`},
		{name: "table", file: "[start]\n", err: ".worktender.toml:1: want key"},
		{name: "unquoted", file: "\nbase = main\n", err: ".worktender.toml:2: base: want a quoted string"},
		{name: "unterminated", file: "base = \"main\n", err: ".worktender.toml:1: base: unterminated string"},
		{name: "trailing junk", file: "base = \"main\" x\n", err: ".worktender.toml:1: base: unexpected"},
		{name: "bad prefix", file: "branch_prefix = \"Feat//\"\n", err: ".worktender.toml:1: branch_prefix"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := herdrtest.NewRepo(t)
			if tc.file != "" {
				repo.Write(repoConfName, tc.file)
			}
			var warn strings.Builder
			conf, err := loadRepoConf(repo.Root, &warn)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) || exitCode(err) != exitUsage {
					t.Fatalf("err = %v, want usage error containing %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if !samePtr(conf.BranchPrefix, tc.prefix) || !samePtr(conf.Base, tc.base) {
				t.Errorf("conf = %v/%v, want %v/%v", confVal(conf.BranchPrefix), confVal(conf.Base), confVal(tc.prefix), confVal(tc.base))
			}
			if tc.warn == "" && warn.Len() != 0 || !strings.Contains(warn.String(), tc.warn) {
				t.Errorf("warnings = %q, want %q", warn.String(), tc.warn)
			}
		})
	}
}

func samePtr(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func confVal(s *string) string {
	if s == nil {
		return "<unset>"
	}
	return *s
}

// startWith runs `start 42` in a repo carrying file and returns the
// worktree.create params.
func startWith(t *testing.T, file string, args ...string) map[string]any {
	t.Helper()
	repo := herdrtest.NewRepo(t)
	repo.Write(".husky/pre-commit", "[[ $(git branch --show-current) =~ ^(feat|fix)/ ]] || exit 1\n")
	repo.Write(repoConfName, file)
	server := fakeStart(t, repo, "w9", "w9:p1", "working")
	herdrtest.FakeGh(t, `cat <<'JSON'
{"number":42,"title":"Fix the thing","body":"it is broken"}
JSON`)

	if err := startCommand(append([]string{"42"}, args...), &strings.Builder{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	for _, call := range server.Calls() {
		if call.Method == "worktree.create" {
			return call.Params
		}
	}
	t.Fatal("start never created a worktree")
	return nil
}

func TestStartReadsTheRepoFile(t *testing.T) {
	params := startWith(t, "branch_prefix = \"chore\"\nbase = \"release\"\n")
	if params["branch"] != "chore/42-fix-the-thing" {
		t.Errorf("branch = %v, want the file's prefix over the hook's", params["branch"])
	}
	if params["base"] != "release" {
		t.Errorf("base = %v, want release", params["base"])
	}
}

func TestStartFlagsBeatTheRepoFile(t *testing.T) {
	params := startWith(t, "branch_prefix = \"chore/\"\nbase = \"release\"\n", "--branch-prefix", "docs", "--base", "next")
	if params["branch"] != "docs/42-fix-the-thing" || params["base"] != "next" {
		t.Errorf("branch = %v, base = %v, want docs/42-fix-the-thing on next", params["branch"], params["base"])
	}
}

func TestStartRefusesAMalformedRepoFile(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	repo.Write(repoConfName, "base = main\n")
	server := fakeStart(t, repo, "w9", "w9:p1", "working")
	herdrtest.FakeGh(t, `echo '{"number":42,"title":"x","body":"y"}'`)

	err := startCommand([]string{"42"}, &strings.Builder{})
	if exitCode(err) != exitUsage || !strings.Contains(err.Error(), ".worktender.toml:1") {
		t.Errorf("err = %v, want a usage error naming line 1", err)
	}
	for _, call := range server.Calls() {
		if call.Method == "worktree.create" {
			t.Error("start created a worktree despite the bad file")
		}
	}
}
