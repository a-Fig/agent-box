package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// lockLeases serializes lease changes between processes. The lock file stays
// in place so each opener locks the same file, including after a crash.
func (a *App) lockLeases(ctx context.Context) (*os.File, error) {
	if a.StateDir == "" {
		return nil, errors.New("no state directory")
	}
	if err := os.MkdirAll(a.StateDir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(a.StateDir, ".lease-lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for ctx.Err() == nil {
		locked, err := tryLeaseLockFile(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		if locked {
			return f, nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Millisecond):
		}
	}
	f.Close()
	return nil, ctx.Err()
}

// Housekeeping is best-effort. A suspended lock holder should not strand a
// cancelled watcher or delay an unrelated tool call.
func (a *App) lockLeasesBriefly() (*os.File, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	return a.lockLeases(ctx)
}
