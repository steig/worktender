//go:build !unix

package main

import "os/exec"

// killTree leaves exec's own cancel, which kills sh alone: there is no process
// group to kill here, and WaitDelay bounds the wait on what sh started.
func killTree(cmd *exec.Cmd) {}
