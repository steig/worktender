package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/herdrtest"
)

// stubClaude puts a `claude` running body first on PATH, so no test runs a
// real one.
func stubClaude(t *testing.T, body string) {
	t.Helper()

	dir := t.TempDir()
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func startManifest(t *testing.T, repo *herdrtest.Repo, extra ...string) (startJSON, error) {
	t.Helper()

	herdrtest.FakeGh(t, `cat <<'JSON'
{"number":42,"title":"Fix the thing","body":"it is broken"}
JSON`)
	var out strings.Builder
	err := startCommand(append([]string{"42", "--json"}, extra...), &out)
	var doc startJSON
	decodeOne(t, out.String(), &doc)
	return doc, err
}

func TestStartManifestNamesTheClaudeVersion(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	fakeStart(t, repo, "w9", "w9:p1", "working")
	stubClaude(t, "echo '2.1.0 (Claude Code)'; echo second line")

	doc, err := startManifest(t, repo)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if doc.ClaudeVersion == nil || *doc.ClaudeVersion != "2.1.0 (Claude Code)" {
		t.Errorf("claude_version = %v, want the first line only", doc.ClaudeVersion)
	}
}

func TestStartManifestClaudeVersionIsNullWhenClaudeFails(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	fakeStart(t, repo, "w9", "w9:p1", "working")
	stubClaude(t, "echo boom; exit 1")

	doc, err := startManifest(t, repo)
	if err != nil {
		t.Fatalf("a failing claude must not fail the start: %v", err)
	}
	if doc.ClaudeVersion != nil {
		t.Errorf("claude_version = %q, want null", *doc.ClaudeVersion)
	}
	if !doc.Briefed {
		t.Error("briefed = false")
	}
}

func TestStartManifestCarriesTheClaudeMdBlobAtTheForkPoint(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	repo.Write("CLAUDE.md", "one\n")
	repo.Git("add", ".")
	repo.Git("commit", "-m", "claude one")
	a := repo.Git("rev-parse", "HEAD")
	repo.Write("CLAUDE.md", "two\n")
	repo.Git("add", ".")
	repo.Git("commit", "-m", "claude two")
	fakeStart(t, repo, "w9", "w9:p1", "working")

	doc, err := startManifest(t, repo, "--base", a)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	atA, atB := repo.Git("rev-parse", a+":CLAUDE.md"), repo.Git("rev-parse", "HEAD:CLAUDE.md")
	if doc.ClaudeMdBlob == nil || *doc.ClaudeMdBlob != atA {
		t.Errorf("claude_md_blob = %v, want %s (the fork point's)", doc.ClaudeMdBlob, atA)
	}
	if doc.ClaudeMdBlob != nil && *doc.ClaudeMdBlob == atB {
		t.Error("claude_md_blob is the later commit's CLAUDE.md, not the fork point's")
	}
}

func TestStartManifestClaudeMdBlobIsNullWithoutTheFile(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	fakeStart(t, repo, "w9", "w9:p1", "working")

	doc, err := startManifest(t, repo)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if doc.ClaudeMdBlob != nil {
		t.Errorf("claude_md_blob = %q, want null", *doc.ClaudeMdBlob)
	}
}
