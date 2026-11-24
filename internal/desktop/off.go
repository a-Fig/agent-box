//go:build !windows && !(linux && desktop)

package desktop

import (
	"context"
	"errors"
)

// Supported says whether this build has the window. Windows has it. Linux builds leave it out
// unless built with -tags desktop: the web view is loaded through purego, which on Linux makes the
// Go runtime link glibc dynamically, and every box command would then fail where glibc isn't at
// the usual path. macOS would need its window on the process's first thread and native code to
// match, for a system nobody here runs box on yet.
const Supported = false

// Run reports that this build has no window.
func Run(context.Context, Options) error {
	return errors.New("this build of box has no window (Windows has one, and Linux with -tags desktop); box ui opens the dashboard in the browser")
}
