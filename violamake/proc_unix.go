//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup places the child in its own process group so that
// cancelling the context tears down the entire subtree. OMR tools frequently
// spawn JVM helpers that would otherwise outlive an interrupted run.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative PID signals the whole group.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
