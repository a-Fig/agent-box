package main

import (
	"context"
	"sync"

	"github.com/a-Fig/agent-box/internal/mode"
)

// dropCachesScript frees the distro's page cache and compacts its memory
// once it runs again after a Release memory (#214): `wsl --terminate` stops
// the distro, not the WSL VM, and the restarted distro's startup cache
// (about 1GiB of the ~3GiB vmmemWSL then held) would otherwise sit until
// autoMemoryReclaim=gradual handed it back. It has no double quotes or
// backslashes, so it passes through wsl.exe's command line intact.
const dropCachesScript = `sync; echo 1 > /proc/sys/vm/drop_caches; echo 1 > /proc/sys/vm/compact_memory`

// cacheDrop drops the distro's page cache exactly once after a Release
// memory, when the first tick sees the distro running again. It is armed
// only by a release whose terminate ran; a distro that comes up on its
// own (a boot, a box starting it) drops nothing. The drop runs in the
// background so a slow `sync` cannot hold up the mode loop, and a failure
// is logged, not retried: the cache costs memory until reclaim, not
// correctness.
type cacheDrop struct {
	run  func(context.Context) error
	logf func(format string, args ...any)

	mu    sync.Mutex
	armed bool
}

// arm records that a Release memory terminated the distro: the next
// running tick owes one drop.
func (d *cacheDrop) arm() {
	d.mu.Lock()
	d.armed = true
	d.mu.Unlock()
}

// observe is the tray's per-tick report hook: on the first tick with the
// distro up after arm, it spends the arm and starts the one drop.
func (d *cacheDrop) observe(st mode.Status) {
	d.mu.Lock()
	if !d.armed || st.Down {
		d.mu.Unlock()
		return
	}
	d.armed = false
	d.mu.Unlock()
	go func() {
		if err := d.run(context.Background()); err != nil {
			d.logf("release memory: dropping the page cache: %v", err)
			return
		}
		d.logf("release memory: dropped the distro page cache")
	}()
}
