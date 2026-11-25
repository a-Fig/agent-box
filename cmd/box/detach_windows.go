//go:build windows

package main

import (
	"errors"
	"os/exec"
	"syscall"
)

const (
	createNewProcessGroup  = 0x00000200
	createBreakawayFromJob = 0x01000000
	createNoWindow         = 0x08000000
)

// hideWindow keeps a child command, like incus or t3.cmd, from opening a
// console window when its parent has none.
func hideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}

// detach makes cmd outlive its parent: its own process group, a hidden
// console its children (t3.cmd runs cmd.exe) share instead of opening
// windows, and, when breakaway is set, a way out of the parent's job object,
// which would otherwise kill it with the MCP server. Breakaway fails if the
// job doesn't allow it (breakawayDenied); startDetached then tries without.
func detach(cmd *exec.Cmd, breakaway bool) {
	flags := uint32(createNewProcessGroup | createNoWindow)
	if breakaway {
		flags |= createBreakawayFromJob
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
}

// breakawayDenied says whether a start failed because the job object doesn't
// allow CREATE_BREAKAWAY_FROM_JOB: CreateProcess then fails with access denied.
func breakawayDenied(err error) bool { return errors.Is(err, syscall.ERROR_ACCESS_DENIED) }
