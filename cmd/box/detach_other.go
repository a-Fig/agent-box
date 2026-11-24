//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detach makes cmd outlive its parent by giving it its own session.
func detach(cmd *exec.Cmd, breakaway bool) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
