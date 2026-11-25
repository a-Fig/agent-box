package main

import "strings"

// headlessRequested reports whether the command line asks for the headless
// enforcer (`boxtray.exe --headless`).
func headlessRequested(args []string) bool {
	for _, a := range args {
		if strings.EqualFold(a, "--headless") || strings.EqualFold(a, "-headless") {
			return true
		}
	}
	return false
}
