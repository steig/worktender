package main

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// claudeVersion is the first line of `claude --version` for the claude on
// worktender's own PATH, or "" when it cannot be read. It annotates a start
// that has to go ahead either way, so a failure is an absence, not an error.
func claudeVersion() string {
	path, err := exec.LookPath("claude")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(first)
}
