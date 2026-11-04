package main

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/execute"
	"github.com/steig/worktender/internal/herdrtest"
)

func mcpConfigFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func startWorkerArgs(t *testing.T) ([]string, error) {
	t.Helper()
	t.Setenv(execute.WorkerTokenEnv, "")
	repo := herdrtest.NewRepo(t)
	server := fakeStart(t, repo, "w5", "w5:p1", "working")
	herdrtest.FakeGh(t, herdrtest.CatScript(t, `{"number":5,"title":"t","body":"b"}`))
	err := startCommand([]string{"5"}, io.Discard)
	for _, call := range server.Calls() {
		if err != nil && call.Method == "worktree.create" {
			t.Errorf("start created a worktree before refusing: %v", err)
		}
		if err != nil && call.Method == "agent.start" {
			t.Errorf("start launched an agent after refusing: %v", err)
		}
	}
	if err != nil {
		return nil, err
	}
	return agentStartArgs(t, server), nil
}

func TestWorkerMCPConfigUnsetKeepsNoMCP(t *testing.T) {
	t.Setenv(workerMCPConfigEnv, "")
	args, err := workerArgsFor("", "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(args, " "), "--strict-mcp-config --disallowedTools mcp__*"; got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestWorkerMCPConfigPassesTheFile(t *testing.T) {
	path := mcpConfigFile(t, `{"mcpServers":{"serena":{"command":"serena"}}}`)
	t.Setenv(workerMCPConfigEnv, path)
	args, err := startWorkerArgs(t)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	i := slices.Index(args, "--mcp-config")
	if i < 0 || i+1 >= len(args) || args[i+1] != path {
		t.Errorf("args = %v, want --mcp-config %s", args, path)
	}
	if !slices.Contains(args, "--strict-mcp-config") {
		t.Errorf("args = %v, want --strict-mcp-config", args)
	}
	if j := slices.Index(args, "--disallowedTools"); j >= 0 && strings.Contains(args[j+1], "mcp__") {
		t.Errorf("args = %v, want no mcp__* in --disallowedTools", args)
	}
	k := slices.Index(args, "--settings")
	if k < 0 || !strings.Contains(args[k+1], `"ENABLE_CLAUDEAI_MCP_SERVERS":"false"`) {
		t.Errorf("args = %v, want the claude.ai connectors still off", args)
	}
}

func TestWorkerMCPConfigRefusesAMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone.json")
	notJSON := mcpConfigFile(t, `not json`)
	noServers := mcpConfigFile(t, `{"servers":{}}`)
	for _, value := range []string{missing, notJSON, noServers} {
		t.Setenv(workerMCPConfigEnv, value)
		_, err := startWorkerArgs(t)
		if err == nil || !strings.Contains(err.Error(), workerMCPConfigEnv) || !strings.Contains(err.Error(), value) {
			t.Errorf("value %q: err = %v, want a refusal naming the variable and the value", value, err)
		}
	}
}

func TestWorkerMCPConfigRefusesARelativePath(t *testing.T) {
	// The file exists, so only the rule about relative paths can refuse it.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv(workerMCPConfigEnv, "mcp.json")
	_, err := workerMCP()
	if err == nil || !strings.Contains(err.Error(), workerMCPConfigEnv) || !strings.Contains(err.Error(), "mcp.json") {
		t.Errorf("err = %v, want a refusal naming the variable and the value", err)
	}
}

func TestReviewIgnoresWorkerMCPConfig(t *testing.T) {
	t.Setenv(workerMCPConfigEnv, mcpConfigFile(t, `{"mcpServers":{}}`))
	server, _, _ := fakeReview(t, greenPR)
	stubReviewAwait(t, gateOutcomeReleased, &report{status: "done", pr: 17, note: "pass: all met"}, nil)

	if err := reviewCommand([]string{"17"}, io.Discard); err != nil {
		t.Fatalf("review: %v", err)
	}
	if args := agentStartArgs(t, server); slices.Contains(args, "--mcp-config") {
		t.Errorf("review args = %v, want no --mcp-config", args)
	}
}
