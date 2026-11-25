//go:build windows

package main

import (
	"errors"
	"sync"
	"time"
	"unsafe"

	"github.com/a-Fig/agent-box/internal/mode"
	"golang.org/x/sys/windows"
)

// A small notification-area icon with a right-click menu, written directly
// against Win32 so we don't need a tray library for ~150 lines.

var (
	user32  = windows.NewLazySystemDLL("user32.dll")
	gdi32   = windows.NewLazySystemDLL("gdi32.dll")
	shellNI = shell32.NewProc("Shell_NotifyIconW")
)

func u32(name string) *windows.LazyProc { return user32.NewProc(name) }

const (
	wmNull       = 0x0000
	wmDestroy    = 0x0002
	wmClose      = 0x0010
	wmEndSession = 0x0016
	wmTimer      = 0x0113
	wmLButtonUp  = 0x0202
	wmRButtonUp  = 0x0205
	wmTray       = 0x8001 // WM_APP+1: icon callback
	wmRefresh    = 0x8002 // WM_APP+2: status changed
	nimAdd       = 0
	nimModify    = 1
	nimDelete    = 2
	nifMessage   = 1
	nifIcon      = 2
	nifTip       = 4
	mfGrayed     = 1
	mfChecked    = 8
	mfSeparator  = 0x800
	tpmRightBtn  = 0x2
	tpmBottom    = 0x20
	tpmReturnCmd = 0x100
	idModeBase   = 1 // 1..3 select a mode
	idRelease    = 10
	idQuit       = 11
	trayIconID   = 1
	iconTimerID  = 1
	className    = "AgentBoxTray"
)

type notifyIconData struct {
	cbSize           uint32
	hWnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         windows.GUID
	hBalloonIcon     uintptr
}

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type winMsg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

type point struct{ x, y int32 }

type iconInfo struct {
	fIcon    uint32
	xHotspot uint32
	yHotspot uint32
	hbmMask  uintptr
	hbmColor uintptr
}

type trayActions struct {
	pick    func(mode.Mode)
	release func()
}

type tray struct {
	hwnd           uintptr
	nid            notifyIconData
	chosen         func() mode.Mode
	act            trayActions
	taskbarCreated uint32
	icons          map[mode.Mode]uintptr
	retry          *iconRetry

	mu     sync.Mutex
	status mode.Status
}

// the window procedure needs the tray; there is only ever one.
var theTray *tray

func newTray(chosen func() mode.Mode, act trayActions) (*tray, error) {
	t := &tray{chosen: chosen, act: act, icons: map[mode.Mode]uintptr{}}
	theTray = t
	t.status = mode.Status{Chosen: chosen()}
	for m, rgb := range map[mode.Mode][3]byte{
		mode.MyPC: {0x80, 0x80, 0x88}, mode.Shared: {0xE0, 0xA0, 0x20}, mode.Full: {0x30, 0xA0, 0x50},
	} {
		t.icons[m] = dotIcon(rgb)
	}

	inst, _, _ := kernel32.NewProc("GetModuleHandleW").Call(0)
	cls, _ := windows.UTF16PtrFromString(className)
	wc := wndClassEx{lpfnWndProc: windows.NewCallback(wndProc), hInstance: inst, lpszClassName: cls}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	if a, _, err := u32("RegisterClassExW").Call(uintptr(unsafe.Pointer(&wc))); a == 0 {
		return nil, err
	}
	// A hidden ordinary window (not message-only, which misses TaskbarCreated).
	h, _, err := u32("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(cls)), 0, 0, 0, 0, 0, 0, 0, inst, 0)
	if h == 0 {
		return nil, err
	}
	t.hwnd = h
	tb, _ := windows.UTF16PtrFromString("TaskbarCreated")
	r, _, _ := u32("RegisterWindowMessageW").Call(uintptr(unsafe.Pointer(tb)))
	t.taskbarCreated = uint32(r)

	t.nid.cbSize = uint32(unsafe.Sizeof(t.nid))
	t.nid.hWnd = h
	t.nid.uID = trayIconID
	t.nid.uFlags = nifMessage | nifIcon | nifTip
	t.nid.uCallbackMessage = wmTray
	t.retry = &iconRetry{add: t.addIcon, now: time.Now, limit: iconRetryLimit, logf: lg.Printf}
	// A failed first add is not fatal: at logon the taskbar may not be ready.
	t.restartIcon()
	return t, nil
}

// addIcon puts the icon in the notification area (UI thread only).
func (t *tray) addIcon() error {
	t.render()
	if ok, _, err := shellNI.Call(nimAdd, uintptr(unsafe.Pointer(&t.nid))); ok == 0 {
		return errors.Join(errors.New("Shell_NotifyIcon add failed"), err)
	}
	return nil
}

// restartIcon starts a fresh round of add attempts and arms the retry timer
// if the first one failed. Runs on the UI thread, from newTray and when
// Explorer announces a new taskbar.
func (t *tray) restartIcon() {
	t.retry.restart()
	t.stepIcon()
}

func (t *tray) stepIcon() {
	if t.retry.try() {
		u32("SetTimer").Call(t.hwnd, iconTimerID, uintptr(iconRetryEvery/time.Millisecond), 0)
	} else {
		u32("KillTimer").Call(t.hwnd, iconTimerID)
	}
}

// modifyIcon refreshes the icon and tooltip, if the icon is in at all.
func (t *tray) modifyIcon() {
	t.render()
	if t.retry.added {
		shellNI.Call(nimModify, uintptr(unsafe.Pointer(&t.nid)))
	}
}

// loop pumps messages until Quit.
func (t *tray) loop() {
	var m winMsg
	for {
		r, _, _ := u32("GetMessageW").Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) < 0 {
			lg.Printf("GetMessageW failed; leaving the message loop")
		}
		if int32(r) <= 0 {
			break
		}
		u32("TranslateMessage").Call(uintptr(unsafe.Pointer(&m)))
		u32("DispatchMessageW").Call(uintptr(unsafe.Pointer(&m)))
	}
	shellNI.Call(nimDelete, uintptr(unsafe.Pointer(&t.nid)))
}

// close ends the tray from another goroutine: WM_CLOSE destroys the window, WM_DESTROY quits the
// message loop, and loop removes the icon.
func (t *tray) close() { u32("PostMessageW").Call(t.hwnd, wmClose, 0, 0) }

// setStatus is called from the poll goroutine; the icon is updated on the UI thread.
func (t *tray) setStatus(st mode.Status) {
	t.mu.Lock()
	t.status = st
	t.mu.Unlock()
	u32("PostMessageW").Call(t.hwnd, wmRefresh, 0, 0)
}

// render fills nid with the current icon and tooltip.
func (t *tray) render() {
	t.mu.Lock()
	st := t.status
	t.mu.Unlock()
	icon := t.icons[st.Chosen]
	if st.Down || icon == 0 {
		icon = t.icons[mode.MyPC]
	}
	t.nid.hIcon = icon
	tip := windows.StringToUTF16("Agent Box: " + statusText(st))
	if len(tip) > len(t.nid.szTip) {
		tip = append(tip[:len(t.nid.szTip)-1], 0)
	}
	t.nid.szTip = [128]uint16{}
	copy(t.nid.szTip[:], tip)
}

func (t *tray) showMenu() {
	menu, _, _ := u32("CreatePopupMenu").Call()
	defer u32("DestroyMenu").Call(menu)
	add := func(flags uintptr, id uintptr, text string) {
		p, _ := windows.UTF16PtrFromString(text)
		u32("AppendMenuW").Call(menu, flags, id, uintptr(unsafe.Pointer(p)))
	}
	t.mu.Lock()
	st := t.status
	t.mu.Unlock()
	add(mfGrayed, 0, statusText(st))
	add(mfSeparator, 0, "")
	for i, m := range mode.All {
		var flags uintptr
		if m == t.chosen() {
			flags = mfChecked
		}
		add(flags, uintptr(idModeBase+i), m.Label())
	}
	add(mfSeparator, 0, "")
	add(0, idRelease, "Release memory")
	add(0, idQuit, "Quit")

	var pt point
	u32("GetCursorPos").Call(uintptr(unsafe.Pointer(&pt)))
	u32("SetForegroundWindow").Call(t.hwnd) // lets the menu close when clicking away
	id, _, _ := u32("TrackPopupMenu").Call(menu, tpmRightBtn|tpmBottom|tpmReturnCmd,
		uintptr(pt.x), uintptr(pt.y), 0, t.hwnd, 0)
	u32("PostMessageW").Call(t.hwnd, wmNull, 0, 0)

	switch {
	case id >= idModeBase && int(id) < idModeBase+len(mode.All):
		t.act.pick(mode.All[id-idModeBase])
		t.modifyIcon()
	case id == idRelease:
		lg.Printf("Release memory chosen from the menu")
		t.act.release()
	case id == idQuit:
		lg.Printf("Quit chosen from the menu")
		u32("PostQuitMessage").Call(0)
	}
}

func wndProc(hwnd, m, wparam, lparam uintptr) uintptr {
	t := theTray
	switch {
	case t != nil && m == wmTray:
		if ev := lparam & 0xFFFF; ev == wmRButtonUp || ev == wmLButtonUp {
			t.showMenu()
		}
		return 0
	case t != nil && m == wmRefresh:
		t.modifyIcon()
		return 0
	case t != nil && m == wmTimer && wparam == iconTimerID:
		t.stepIcon()
		return 0
	case t != nil && t.taskbarCreated != 0 && uint32(m) == t.taskbarCreated:
		lg.Printf("TaskbarCreated: Explorer (re)started, adding the icon again")
		t.restartIcon()
		return 0
	case m == wmEndSession && wparam != 0:
		lg.Printf("Windows session is ending")
		return 0
	case m == wmDestroy:
		u32("PostQuitMessage").Call(0)
		return 0
	}
	r, _, _ := u32("DefWindowProcW").Call(hwnd, m, wparam, lparam)
	return r
}

// dotIcon draws a 16x16 filled circle in the given colour.
func dotIcon(rgb [3]byte) uintptr {
	const n = 16
	var px [n * n * 4]byte // BGRA
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			dx, dy := 2*x+1-n, 2*y+1-n // doubled offsets from the centre
			if dx*dx+dy*dy <= (n-2)*(n-2) {
				i := (y*n + x) * 4
				px[i], px[i+1], px[i+2], px[i+3] = rgb[2], rgb[1], rgb[0], 0xFF
			}
		}
	}
	var mask [n * n / 8]byte
	color, _, _ := gdi32.NewProc("CreateBitmap").Call(n, n, 1, 32, uintptr(unsafe.Pointer(&px[0])))
	hmask, _, _ := gdi32.NewProc("CreateBitmap").Call(n, n, 1, 1, uintptr(unsafe.Pointer(&mask[0])))
	ii := iconInfo{fIcon: 1, hbmMask: hmask, hbmColor: color}
	icon, _, _ := u32("CreateIconIndirect").Call(uintptr(unsafe.Pointer(&ii)))
	del := gdi32.NewProc("DeleteObject")
	del.Call(color)
	del.Call(hmask)
	return icon
}
