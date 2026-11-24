package main

import "fmt"

// SHQueryUserNotificationState values that mean a game (or similar) owns the screen.
const (
	qunsBusy                 = 2
	qunsRunningD3DFullScreen = 3
)

// classify turns the raw Windows signals into "is a game running", with a
// short reason for the tooltip. A signal that couldn't be read passes 0 and
// counts as no signal, never as gaming.
func classify(steamAppID uint32, notifyState int) (bool, string) {
	if steamAppID != 0 {
		return true, fmt.Sprintf("steam:%d", steamAppID)
	}
	switch notifyState {
	case qunsRunningD3DFullScreen:
		return true, "fullscreen"
	case qunsBusy:
		return true, "busy"
	}
	return false, ""
}
