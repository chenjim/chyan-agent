//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// DETACHED_PROCESS keeps the spawned daemon off the CLI's console.
const detachedProcess = 0x00000008

// detachProcess starts the daemon without a console attached.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess}
}
