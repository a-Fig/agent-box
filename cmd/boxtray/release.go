package main

import (
	"context"
	"sync"

	"github.com/a-Fig/agent-box/internal/mode"
)

// dropCachesScript frees the page cache and compacts memory once the
// distro runs again after a Release memory (#214): `wsl --terminate` stops
// the distro, not the WSL VM, and the restarted distro's startup cache
// (about 1GiB of the ~3GiB vmmemWSL then held) would otherwise sit until
// autoMemoryReclaim=gradual handed it back. The cache is the WSL VM
// kernel's, shared by every distro in the VM, so the drop is VM-wide.
// The commands chain with &&, so a failed drop cannot log success just
// because compaction ran. It has no double quotes or backslashes, so it
// passes through wsl.exe's command line intact.
const dropCachesScript = `sync && echo 1 > /proc/sys/vm/drop_caches && echo 1 > /proc/sys/vm/compact_memory`

// cacheDrop drops the page cache exactly once after a Release memory,
// when the first tick sees the distro running again. It is armed only by
// a release whose terminate ran; a distro that comes up on its own (a
// boot, a box starting it) drops nothing. The drop runs in the
// background so a slow `sync` cannot hold up the mode loop, and a
// failure is logged, not retried: the cache costs memory until reclaim,
// not correctness.
//
// Best effort, tray-open only: the arm lives only in this tray process,
// so the tray must stay open until the distro returns. Quitting (or any
// tray exit) before then discards a pending drop, and the headless
// enforcer that brings the distro back has no drop of its own. Nothing
// is persisted, by design: the cache is a cost, not correctness.
//
// The drop calls wsl.exe, so it coordinates with Release memory like the
// background reclaim work does: observe starts nothing while a release
// runs (busy), and stop cancels an in-flight drop and waits for it, so
// its call cannot start the distro again during the next termination.
type cacheDrop struct {
	run  func(context.Context) error
	logf func(format string, args ...any)
	busy func() bool // a Release memory is in progress; nil is never

	mu     sync.Mutex
	armed  bool
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// arm records that a Release memory terminated the distro: the next
// running tick owes one drop.
func (d *cacheDrop) arm() {
	d.mu.Lock()
	d.armed = true
	d.mu.Unlock()
}

// observe is the tray's per-tick report hook: on the first tick with the
// distro up after arm (and no release running), it spends the arm and
// starts the one drop.
func (d *cacheDrop) observe(st mode.Status) {
	d.mu.Lock()
	if !d.armed || st.Down || (d.busy != nil && d.busy()) {
		d.mu.Unlock()
		return
	}
	d.armed = false
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.wg.Add(1)
	d.mu.Unlock()
	go func() {
		defer d.wg.Done()
		defer func() {
			d.mu.Lock()
			d.cancel = nil
			d.mu.Unlock()
		}()
		if err := d.run(ctx); err != nil {
			d.logf("release memory: dropping the page cache: %v", err)
			return
		}
		if ctx.Err() == nil {
			d.logf("release memory: dropped the distro page cache")
		}
	}()
}

// stop cancels an in-flight drop and waits for its wsl.exe call to end.
// The tray calls it after BeginRelease, before `wsl --terminate`, like
// BeginRelease itself waits for the background reclaims.
func (d *cacheDrop) stop() {
	d.mu.Lock()
	cancel := d.cancel
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	d.wg.Wait()
}
