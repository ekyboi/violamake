//go:build !unix

package main

import "os/exec"

// configureProcessGroup is a no-op on platforms without POSIX process groups;
// exec.CommandContext still kills the direct child.
func configureProcessGroup(cmd *exec.Cmd) {}
