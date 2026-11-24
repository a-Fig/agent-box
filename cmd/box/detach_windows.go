//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

const (
	createNewProcessGroup  = 0x00000200
	createBreakawayFromJob = 0x01000000
	createNoWindow         = 0x08000000
)

// detach makes cmd outlive its parent: its own process group, a hidden
// console its children (t3.cmd runs cmd.exe) share instead of opening
// windows, and, when breakaway is set, a way out of the parent's job object,
// which would otherwise kill it with the MCP server. Breakaway fails if the
// job doesn't allow it; the caller then tries without.
func detach(cmd *exec.Cmd, breakaway bool) {
	flags := uint32(createNewProcessGroup | createNoWindow)
	if breakaway {
		flags |= createBreakawayFromJob
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
}
