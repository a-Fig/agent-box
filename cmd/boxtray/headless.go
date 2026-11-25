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

// noGames wraps Sensors so the headless enforcer never yields: nobody can be
// gaming at the login screen, so it always runs Full.
type noGames struct{ Sensors }

func (noGames) Gaming() (bool, string) { return false, "" }
