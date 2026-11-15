//go:build !unix

package tools

import "os/exec"

// setupProcessGroup is a no-op where process groups are unavailable; the
// command context still kills the direct child.
func setupProcessGroup(_ *exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
