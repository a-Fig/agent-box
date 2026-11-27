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
// when a tick sees the distro running again. A successful termination
// arms it. The drop runs in the background so a slow `sync` cannot hold
// up the mode loop. A failed drop is logged once and left to WSL reclaim.
//
// The drop calls wsl.exe, so it coordinates with Release memory like the
// background reclaim work does: observe starts nothing while a release
// runs (busy), and stop cancels an in-flight drop and waits for it, so
// its call cannot start the distro again during the next termination.
type cacheDrop struct {
	hostUp func(context.Context) bool
	run    func(context.Context) error
	logf   func(format string, args ...any)
	busy   func() bool // a Release memory is in progress; nil is never

	mu     sync.Mutex
	armed  bool
	paused bool
	cancel context.CancelFunc
	done   chan struct{}
}

// arm records that a Release memory terminated the distro: the next
// running tick owes one drop.
func (d *cacheDrop) arm() {
	d.mu.Lock()
	d.armed = true
	d.mu.Unlock()
}

// observe schedules the drop from a tick that reports an up distro.
func (d *cacheDrop) observe(st mode.Status) {
	d.mu.Lock()
	if !d.armed || d.paused || d.done != nil || st.Down || (d.busy != nil && d.busy()) {
		d.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	d.cancel, d.done = cancel, done
	d.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			d.mu.Lock()
			d.cancel, d.done = nil, nil
			close(done)
			d.mu.Unlock()
		}()
		// The report can come from a tick that finished before termination.
		// Check again without starting WSL or spending the arm on a down distro.
		if !d.hostUp(ctx) {
			return
		}
		d.mu.Lock()
		if ctx.Err() != nil || d.paused || (d.busy != nil && d.busy()) {
			d.mu.Unlock()
			return
		}
		d.armed = false
		d.mu.Unlock()
		if err := d.run(ctx); err != nil {
			d.logf("release memory: dropping the page cache: %v", err)
			return
		}
		d.logf("release memory: dropped the distro page cache")
	}()
}

// stop cancels an in-flight drop and waits for its wsl.exe call to end.
// The caller blocks new drops during Release memory or a tray handoff.
func (d *cacheDrop) stop() {
	d.mu.Lock()
	cancel, done := d.cancel, d.done
	d.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// setPaused keeps the drop out of Quit and self-update. A failed update
// clears the pause so the current tray can resume.
func (d *cacheDrop) setPaused(paused bool) {
	d.mu.Lock()
	d.paused = paused
	d.mu.Unlock()
	if paused {
		d.stop()
	}
}
