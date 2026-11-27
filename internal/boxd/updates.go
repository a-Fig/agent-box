package boxd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/update"
)

// updateCheckTimeout caps one GitHub release lookup below the page's 15s fetch
// timeout so boxd answers with a clear 502 error before the browser gives up.
const updateCheckTimeout = 10 * time.Second

// ReleaseLookup finds the latest stable release. update.Client implements it;
// tests pass a fake so go test never calls GitHub.
type ReleaseLookup interface {
	Latest(ctx context.Context) (update.Release, error)
}

// UpdateChecker queries the stable release channel on demand, caches the last
// successful result in memory, and coalesces concurrent checks into one lookup.
// Checking is strictly read-only and leaves running builds alone.
type UpdateChecker struct {
	Lookup  ReleaseLookup
	Now     func() time.Time // defaults to time.Now
	BaseCtx context.Context  // optional shutdown context; defaults to context.Background()

	mu       sync.Mutex
	info     dash.UpdateInfo
	inflight *inflightCheck
}

type inflightCheck struct {
	done chan struct{}
	info dash.UpdateInfo
	err  error
}

// Each caller can stop waiting without canceling the shared lookup.
func (c *UpdateChecker) check(ctx context.Context, notify func(dash.UpdateInfo)) (dash.UpdateInfo, error) {
	if err := ctx.Err(); err != nil {
		return dash.UpdateInfo{}, err
	}
	c.mu.Lock()
	call := c.inflight
	if call == nil {
		call = &inflightCheck{done: make(chan struct{})}
		c.inflight = call
		go c.run(call, notify)
	}
	c.mu.Unlock()
	select {
	case <-call.done:
		return call.info, call.err
	case <-ctx.Done():
		return dash.UpdateInfo{}, ctx.Err()
	}
}

func (c *UpdateChecker) run(call *inflightCheck, notify func(dash.UpdateInfo)) {
	base := c.BaseCtx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, updateCheckTimeout)
	defer cancel()

	rel, err := c.Lookup.Latest(ctx)
	var info dash.UpdateInfo
	switch {
	case errors.Is(err, update.ErrNoStable):
		err = nil
	case err == nil && !update.StableTag(rel.Tag):
		err = fmt.Errorf("latest release %q is not a stable release tag", rel.Tag)
	case err == nil:
		info.Latest = rel.Tag
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		info.CheckedAt = c.now().UTC()
		c.info = info
		if notify != nil {
			notify(info)
		}
	}
	call.info, call.err = info, err
	c.inflight = nil
	close(call.done)
}

func (c *UpdateChecker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (s *Server) state() dash.State {
	if s.Updates == nil {
		return s.W.State()
	}
	s.Updates.mu.Lock()
	defer s.Updates.mu.Unlock()
	st := s.W.State()
	info := s.Updates.info
	st.Updates = &info
	return st
}

func (s *Server) subscribe() (dash.State, <-chan dash.Event, func()) {
	if s.Updates == nil {
		return s.W.Subscribe()
	}
	// Use the same lock order as publishing so no update event predates the snapshot.
	s.Updates.mu.Lock()
	defer s.Updates.mu.Unlock()
	st, ch, cancel := s.W.Subscribe()
	info := s.Updates.info
	st.Updates = &info
	return st, ch, cancel
}

func (s *Server) emitUpdates(info dash.UpdateInfo) {
	s.W.mu.Lock()
	s.W.emit(dash.Event{Name: dash.EventUpdates, Data: info})
	s.W.mu.Unlock()
}

func (s *Server) checkUpdates(w http.ResponseWriter, r *http.Request) {
	if s.Updates == nil || s.Updates.Lookup == nil {
		writeError(w, http.StatusBadGateway, dash.CodeUpdateCheck, "Update checks are unavailable on this server.")
		return
	}
	info, err := s.Updates.check(r.Context(), s.emitUpdates)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		log.Printf("check for updates: %v", err)
		writeError(w, http.StatusBadGateway, dash.CodeUpdateCheck, dash.UpdateCheckFailed)
		return
	}
	writeJSON(w, http.StatusOK, info)
}
