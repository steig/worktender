package main

import (
	"strings"
	"testing"

	"github.com/steig/worktender/internal/herdrtest"
)

func TestHookBranchPrefix(t *testing.T) {
	for _, tc := range []struct {
		name, path, body, want string
	}{
		{"no hook", "", "", ""},
		{"a husky branch check", ".husky/pre-commit", "branch=$(git branch --show-current)\n[[ $branch =~ ^(feat|fix|chore)/ ]] || exit 1\n", "feat/"},
		{"a git hook without feat", ".git/hooks/pre-push", "grep -E '^(fix|docs)/' <<<\"$branch\"\n", "fix/"},
		{"a commit rule is not a branch rule", ".husky/commit-msg", "grep -E '^(feat|fix)/' \"$1\"\n", ""},
		{"a sample is ignored", ".git/hooks/pre-push.sample", "branch (feat|fix)/\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := herdrtest.NewRepo(t)
			if tc.path != "" {
				repo.Write(tc.path, tc.body)
			}
			if got := hookBranchPrefix(repo.Root); got != tc.want {
				t.Errorf("hookBranchPrefix = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStartPrefixesTheBranchTheHookRequires(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	repo.Write(".husky/pre-commit", "[[ $(git branch --show-current) =~ ^(feat|fix)/ ]] || exit 1\n")
	server := fakeStart(t, repo, "w9", "w9:p1", "working")
	herdrtest.FakeGh(t, `cat <<'JSON'
{"number":42,"title":"Fix the thing","body":"it is broken"}
JSON`)

	if err := startCommand([]string{"42"}, &strings.Builder{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	for _, call := range server.Calls() {
		if call.Method == "worktree.create" {
			if call.Params["branch"] != "feat/42-fix-the-thing" {
				t.Errorf("branch = %v, want feat/42-fix-the-thing", call.Params["branch"])
			}
			return
		}
	}
	t.Fatal("start never created a worktree")
}

func TestIssueBranchPatternReadsATypePrefix(t *testing.T) {
	if m := issueBranchPattern.FindStringSubmatch("feat/42-fix-the-thing"); m == nil || m[1] != "42" {
		t.Errorf("match = %v, want issue 42", m)
	}
}
