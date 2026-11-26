package desktop

import (
	"log"
	"os"
	"unsafe"

	"github.com/crgimenes/glaze"
	"golang.org/x/sys/windows"
)

// On Windows the window gets the dashboard's icon, which a Go binary doesn't carry, and a title
// bar as dark as the page. As in Murmur's app (internal/desktop/native_windows.go there).

var (
	user32 = windows.NewLazySystemDLL("user32.dll")

	procCreateIconFromResourceEx = user32.NewProc("CreateIconFromResourceEx")
	procSendMessageW             = user32.NewProc("SendMessageW")
	procDwmSetWindowAttribute    = windows.NewLazySystemDLL("dwmapi.dll").NewProc("DwmSetWindowAttribute")
)

const (
	wmSetIcon = 0x0080
	iconSmall = 0
	iconBig   = 1
)

// beforeWindow runs WebView2's GPU work in its browser process rather than a process of its own,
// as Murmur's app does: about 120 MB less memory at idle, and a dashboard needs no isolated GPU.
// Someone who sets the variable themselves is left to it. The runtime reads it as the web view is
// made; what this returns puts it back, so a browser the window opens doesn't inherit it.
func beforeWindow() (after func()) {
	setProcessAppID() // the Start menu shortcut's (launcher_windows.go)
	const env = "WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS"
	if _, set := os.LookupEnv(env); set {
		return func() {}
	}
	os.Setenv(env, "--in-process-gpu")
	return func() { os.Unsetenv(env) }
}

type winNative struct{ hwnd windows.HWND }

func newNative(w glaze.WebView) native {
	n := winNative{hwnd: windows.HWND(w.Window())}
	if h := iconFrom(iconPNG); h != 0 {
		procSendMessageW.Call(uintptr(n.hwnd), wmSetIcon, iconBig, uintptr(h))
	}
	if h := iconFrom(smallPNG); h != 0 {
		procSendMessageW.Call(uintptr(n.hwnd), wmSetIcon, iconSmall, uintptr(h))
	}
	return n
}

// frame asks for a dark title bar, which Windows 10 (20H1) and 11 draw; earlier ones ignore it.
func (n winNative) frame(dark bool) {
	const dwmwaUseImmersiveDarkMode = 20
	var on int32
	if dark {
		on = 1
	}
	procDwmSetWindowAttribute.Call(uintptr(n.hwnd), dwmwaUseImmersiveDarkMode, uintptr(unsafe.Pointer(&on)), 4)
}

// iconFrom makes an icon from PNG bytes, which Windows reads as an icon resource since Vista.
func iconFrom(png []byte) windows.Handle {
	const lrDefaultColor = 0
	h, _, err := procCreateIconFromResourceEx.Call(uintptr(unsafe.Pointer(&png[0])), uintptr(len(png)), 1, 0x00030000, 0, 0, lrDefaultColor)
	if h == 0 {
		log.Printf("icon: %v", err)
	}
	return windows.Handle(h)
}
