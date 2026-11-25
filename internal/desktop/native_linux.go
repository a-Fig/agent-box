//go:build linux && desktop

package desktop

import "github.com/crgimenes/glaze"

// On Linux the desktop's theme draws the title bar, and the window keeps the web view's icon.

type linuxNative struct{}

func beforeWindow() (after func()) { return func() {} }

func newNative(glaze.WebView) native { return linuxNative{} }

func (linuxNative) frame(bool) {}
