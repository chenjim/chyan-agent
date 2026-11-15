//go:build unix

package tools

import (
	"os/exec"
	"syscall"
)

// setupProcessGroup puts the command in its own process group so a timeout
// can signal every process in the pipeline (bash | tail | …).
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup kills the whole group led by the command.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
