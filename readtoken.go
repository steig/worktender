package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/steig/worktender/internal/execute"
)

// readCommand builds a gh command for a read about a repository under owner.
// While any WORKTENDER_READ_GH_TOKEN_* variable is set, the command runs with
// that owner's token as GH_TOKEN, and an owner with none is an error rather
// than a fall back to gh's own login. With none set it is a plain gh command.
//
// The token goes in cmd.Env only: never an argument, an error or a log line.
// Writes use ghOutput and never get it.
func readCommand(owner, dir string, args []string) (*exec.Cmd, error) {
	cmd := exec.Command("gh", args...)
	cmd.Dir = dir
	if !execute.ReadTokensSet() {
		return cmd, nil
	}
	if owner == "" {
		return nil, errors.New("read tokens are set but the repository's owner cannot be read from its origin to pick one by")
	}
	name := execute.ReadTokenEnv(owner)
	token := os.Getenv(name)
	if token == "" {
		return nil, fmt.Errorf("read tokens are set but %s is not", name)
	}
	cmd.Env = append(os.Environ(), "GH_TOKEN="+token)
	return cmd, nil
}

// ghReadOutput is ghOutput for a read about the repository at root.
func ghReadOutput(root string, args ...string) ([]byte, error) {
	cmd, err := readCommand(execute.RemoteOwner(root), root, args)
	if err != nil {
		return nil, err
	}
	return runGh(cmd)
}
