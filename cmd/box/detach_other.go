//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// hideWindow keeps a child command from opening a console window (Windows).
func hideWindow(cmd *exec.Cmd) {}

// detach makes cmd outlive its parent by giving it its own session.
func detach(cmd *exec.Cmd, breakaway bool) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// breakawayDenied is Windows only: elsewhere detach has no breakaway to deny.
func breakawayDenied(err error) bool { return false }
