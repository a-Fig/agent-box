package main

import "time"

const (
	iconRetryEvery = 2 * time.Second
	iconRetryLimit = 2 * time.Minute
)

// iconRetry decides when to keep trying to add the notification icon. At logon
// Explorer's taskbar may not exist yet, so the first Shell_NotifyIcon call can
// fail; the tray used to exit then, leaving nothing enforcing the modes. Now
// the caller tries on a timer until the icon is in or limit has passed, and
// the mode loop runs regardless. It holds no Win32 state, so tests drive it
// with a fake add and clock.
type iconRetry struct {
	add   func() error
	now   func() time.Time
	limit time.Duration
	logf  func(format string, args ...any)

	start  time.Time
	tries  int
	added  bool
	gaveUp bool
}

// restart begins a fresh window of attempts, for instance when Explorer
// announces a new taskbar (which also wipes an icon that was in).
func (r *iconRetry) restart() {
	r.start = r.now()
	r.tries = 0
	r.added = false
	r.gaveUp = false
}

// try makes one attempt and reports whether another should be scheduled.
func (r *iconRetry) try() (again bool) {
	if r.added {
		return false
	}
	err := r.add()
	r.tries++
	if err == nil {
		r.added = true
		r.logf("notification icon added (attempt %d)", r.tries)
		return false
	}
	r.logf("adding the notification icon failed (attempt %d): %v", r.tries, err)
	if r.now().Sub(r.start) >= r.limit {
		r.gaveUp = true
		r.logf("giving up on the notification icon after %s; the mode loop keeps running without it, and a new taskbar (Explorer restart) will try again", r.limit)
		return false
	}
	return true
}
