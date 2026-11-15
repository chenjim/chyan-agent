//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detachProcess puts the spawned daemon in its own session so it survives the
// CLI exiting and keeps no controlling terminal.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
