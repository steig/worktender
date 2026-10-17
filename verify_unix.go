//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// killTree puts the check in its own process group and has a cancel kill the
// group, so a `go test` sh started does not outlive the timeout.
func killTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
