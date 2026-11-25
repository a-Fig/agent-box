//go:build windows

package main

import (
	"context"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/a-Fig/agent-box/internal/mode"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const distro = "agents"

var (
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procQueryNotifyState = shell32.NewProc("SHQueryUserNotificationState")
	procPowerCreate      = kernel32.NewProc("PowerCreateRequest")
	procPowerSet         = kernel32.NewProc("PowerSetRequest")
	procPowerClear       = kernel32.NewProc("PowerClearRequest")
)

// hidden stops wsl.exe from flashing a console window.
func hidden(cmd *exec.Cmd) *exec.Cmd {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd
}

// Every wsl.exe call has a deadline, so a hung WSL can't stall the mode
// loop for good: the next tick tries again.
const (
	wslIncusTimeout     = 30 * time.Second
	wslRootTimeout      = mode.ReclaimTimeout + 15*time.Second // the reclaim's own bound, plus start-up
	wslListTimeout      = 10 * time.Second
	wslTerminateTimeout = time.Minute
)

// wslIncus runs `incus <args>` inside the agents distro. Inside WSL the
// root user talks to Incus over the local socket, which is admin. It runs
// with --exec, without a shell: a shell ate the quotes of a JSON project
// value (the chosen mode and the mode request), found live on 2026-11-24.
func wslIncus(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, wslIncusTimeout)
	defer cancel()
	full := append([]string{"-d", distro, "-u", "root", "--exec", "incus"}, args...)
	return hidden(exec.CommandContext(ctx, "wsl.exe", full...)).Output()
}

// wslRoot runs a command as root inside the agents distro, without a shell
// in between (--exec), for the cgroup writes Incus has no command for.
func wslRoot(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, wslRootTimeout)
	defer cancel()
	full := append([]string{"-d", distro, "-u", "root", "--exec"}, args...)
	return hidden(exec.CommandContext(ctx, "wsl.exe", full...)).Output()
}

// winSensors implements Sensors with Windows APIs.
type winSensors struct {
	mu      sync.Mutex
	request windows.Handle // power request handle, 0 when not held
}

// Gaming checks Steam's RunningAppID, then the shell's notification state.
func (*winSensors) Gaming() (bool, string) {
	var appID uint32
	if k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Valve\Steam`, registry.QUERY_VALUE); err == nil {
		if v, _, err := k.GetIntegerValue("RunningAppID"); err == nil {
			appID = uint32(v)
		}
		k.Close()
	}
	state := 0
	var s int32
	if hr, _, _ := procQueryNotifyState.Call(uintptr(unsafe.Pointer(&s))); hr == 0 {
		state = int(s)
	}
	return classify(appID, state)
}

// HostUp asks wsl.exe which distros run. It never starts one.
func (*winSensors) HostUp(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, wslListTimeout)
	defer cancel()
	out, err := hidden(exec.CommandContext(ctx, "wsl.exe", "-l", "--running", "-q")).Output()
	return err == nil && distroListed(out, distro)
}

// reasonContext is REASON_CONTEXT with a simple string. The union is as big
// as its largest member (the detailed form), hence the padding.
type reasonContext struct {
	version uint32
	flags   uint32
	reason  *uint16
	_       [2]uintptr
}

// HoldAwake keeps the PC from sleeping while on is true, with a Windows power
// request (unlike SetThreadExecutionState it isn't tied to one OS thread).
func (w *winSensors) HoldAwake(on bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if on && w.request == 0 {
		reason, _ := windows.UTF16PtrFromString("Agent Box instances are running")
		rc := reasonContext{flags: 1 /* POWER_REQUEST_CONTEXT_SIMPLE_STRING */, reason: reason}
		h, _, _ := procPowerCreate.Call(uintptr(unsafe.Pointer(&rc)))
		if windows.Handle(h) == windows.InvalidHandle || h == 0 {
			return
		}
		const powerRequestSystemRequired = 1
		if ok, _, _ := procPowerSet.Call(h, powerRequestSystemRequired); ok == 0 {
			windows.CloseHandle(windows.Handle(h))
			return
		}
		w.request = windows.Handle(h)
	} else if !on && w.request != 0 {
		procPowerClear.Call(uintptr(w.request), 1)
		windows.CloseHandle(w.request)
		w.request = 0
	}
}
