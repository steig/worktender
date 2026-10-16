package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// CLAUDE.md is what a worker in a worktree reads on every start, so it is held
// to a fixed budget, and a rule naming a path that has moved is caught here
// rather than by a worker sent looking for it. CODEOWNERS is checked the same
// way: a renamed file silently drops out of a human owner's review.
const claudeMdBudget = 60

func TestClaudeMdStaysWithinBudget(t *testing.T) {
	body, err := os.ReadFile("CLAUDE.md")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(body), "\n"); n > claudeMdBudget {
		t.Errorf("CLAUDE.md is %d lines; the budget is %d. Replace a rule rather than add one.", n, claudeMdBudget)
	}
}

func TestEveryPathClaudeMdNamesExists(t *testing.T) {
	body, err := os.ReadFile("CLAUDE.md")
	if err != nil {
		t.Fatal(err)
	}
	// A backticked name with a file extension, or with a slash in it.
	named := regexp.MustCompile("`([A-Za-z0-9_.-]+\\.(?:go|py|md|toml|yml)|[A-Za-z0-9_.-]*(?:/[A-Za-z0-9_.-]+)+/?)`").FindAllStringSubmatch(string(body), -1)
	if len(named) == 0 {
		t.Fatal("found no paths in CLAUDE.md; the pattern no longer matches how it names them")
	}
	for _, m := range named {
		if _, err := os.Stat(m[1]); err != nil {
			t.Errorf("CLAUDE.md names %s, which does not exist", m[1])
		}
	}
}

func TestEveryPathCodeownersNamesExists(t *testing.T) {
	body, err := os.ReadFile(".github/CODEOWNERS")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if _, err := os.Stat(strings.TrimPrefix(fields[0], "/")); err != nil {
			t.Errorf("CODEOWNERS names %s, which does not exist", fields[0])
		}
	}
}
