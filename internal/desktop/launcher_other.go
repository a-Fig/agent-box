//go:build !windows

package desktop

// Only Windows has a launcher: a Linux build's window keeps the web view's identity.

func launcherPath() string { return "" }

func installLauncher(exe, dir, where string) error { return nil }
