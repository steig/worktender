package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// CLAUDE.md is the one channel that reaches a worker in its worktree: the
// repo's conventions otherwise live in a coordinator's memory, keyed to the
// main checkout's path, which no worktree ever sees. Two ways it rots are
// checked here.
//
// It grows. Every rule added is read by every worker on every start, and a
// long file is skimmed, so the budget is fixed and a new rule has to displace
// an old one.
//
// It points at things that moved. A rule naming a file that no longer exists
// sends a worker looking for it, and teaches it the file's name is a guess.
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

func TestEveryFileClaudeMdNamesExists(t *testing.T) {
	body, err := os.ReadFile("CLAUDE.md")
	if err != nil {
		t.Fatal(err)
	}
	named := regexp.MustCompile("`([A-Za-z0-9_./-]+\\.(?:go|py|md|toml|yml))`").FindAllStringSubmatch(string(body), -1)
	if len(named) == 0 {
		t.Fatal("found no file names in CLAUDE.md; the pattern no longer matches how it names files")
	}
	for _, m := range named {
		if _, err := os.Stat(m[1]); err != nil {
			t.Errorf("CLAUDE.md names %s, which does not exist", m[1])
		}
	}
}
