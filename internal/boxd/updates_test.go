package boxd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/update"
)

type fakeReleaseLookup struct {
	fn    func(ctx context.Context) (update.Release, error)
	calls atomic.Int32
}

func (f *fakeReleaseLookup) Latest(ctx context.Context) (update.Release, error) {
	f.calls.Add(1)
	return f.fn(ctx)
}

func newUpdateTestServer(t *testing.T, lookup ReleaseLookup, c *clock) (*Watcher, *fakeIncus, *httptest.Server) {
	t.Helper()
	f := &fakeIncus{cfg: map[string]string{"user.agent-box.mode": dash.ModeShared}}
	w := newWatcher(t, t.TempDir(), f, c)
	w.Version = &dash.Version{Commit: "e823842a1f0c9d6b", Tag: "v0.3.0"}
	w.Poll(context.Background())
	var checker *UpdateChecker
	if lookup != nil {
		checker = &UpdateChecker{Lookup: lookup, Now: c.now}
	}
	srv := &Server{W: w, Updates: checker}
	ts := httptest.NewServer(Owners([]string{"owner@example.com"}, srv.Handler()))
	t.Cleanup(ts.Close)
	return w, f, ts
}

func doUpdateReq(t *testing.T, method, url, login string, controlHeader, jsonCT bool) (int, []byte) {
	t.Helper()
	var body io.Reader
	if method == "POST" {
		body = strings.NewReader(`{}`)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if login != "" {
		req.Header.Set(LoginHeader, login)
	}
	if controlHeader {
		req.Header.Set(dash.ControlHeader, "1")
	}
	if jsonCT {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

func TestUpdateCheckLifecycleAndAuth(t *testing.T) {
	c := &clock{t: t0}
	type response struct {
		release update.Release
		err     error
	}
	responses := make(chan response, 4)
	responses <- response{release: update.Release{Tag: "v0.3.1"}}
	responses <- response{release: update.Release{Tag: "v0.3.2-nightly.20261126.1"}}
	responses <- response{err: errors.New("github unreachable")}
	responses <- response{err: update.ErrNoStable}
	close(responses)
	lookup := &fakeReleaseLookup{fn: func(ctx context.Context) (update.Release, error) {
		next := <-responses
		return next.release, next.err
	}}
	_, f, ts := newUpdateTestServer(t, lookup, c)

	// Before any check, GET /api/state reports no release or timestamp.
	code, body := doUpdateReq(t, "GET", ts.URL+"/api/state", "owner@example.com", false, false)
	if code != 200 {
		t.Fatalf("GET /api/state: %d %s", code, body)
	}
	var st dash.State
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	if st.Updates == nil || st.Updates.Latest != "" || !st.Updates.CheckedAt.IsZero() {
		t.Fatalf("initial updates = %+v, want empty unchecked", st.Updates)
	}

	// Auth and CSRF guards match existing POST controls.
	for _, tc := range []struct {
		name      string
		login     string
		hdr, ct   bool
		wantCode  int
		wantError string
	}{
		{"not owner", "stranger@example.com", true, true, http.StatusForbidden, dash.CodeForbidden},
		{"missing X-Agent-Box", "owner@example.com", false, true, http.StatusForbidden, dash.CodeForbidden},
		{"missing json content-type", "owner@example.com", true, false, http.StatusBadRequest, dash.CodeBadRequest},
	} {
		got, b := doUpdateReq(t, "POST", ts.URL+"/api/updates/check", tc.login, tc.hdr, tc.ct)
		var eb dash.ErrorBody
		json.Unmarshal(b, &eb)
		if got != tc.wantCode || eb.Code != tc.wantError {
			t.Errorf("%s: got %d %q, want %d %q", tc.name, got, eb.Code, tc.wantCode, tc.wantError)
		}
	}
	if lookup.calls.Load() != 0 {
		t.Fatalf("guarded requests triggered %d lookups", lookup.calls.Load())
	}

	// Open an SSE stream to verify EventUpdates broadcasts on successful checks.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sreq, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/events", nil)
	sreq.Header.Set(LoginHeader, "owner@example.com")
	sresp, err := http.DefaultClient.Do(sreq)
	if err != nil {
		t.Fatal(err)
	}
	defer sresp.Body.Close()
	sr := bufio.NewReader(sresp.Body)
	readEvent := func() (string, string) {
		var name, data string
		for {
			line, err := sr.ReadString('\n')
			if err != nil {
				t.Fatalf("SSE read: %v", err)
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case line == "" && name != "":
				return name, data
			}
		}
	}
	if ev, data := readEvent(); ev != dash.EventState || strings.Contains(data, `"checkedAt"`) {
		t.Fatalf("initial SSE event: %q %s", ev, data)
	}

	// Successful check updates state, emits EventUpdates, and touches nothing in Incus.
	code, body = doUpdateReq(t, "POST", ts.URL+"/api/updates/check", "owner@example.com", true, true)
	if code != http.StatusOK {
		t.Fatalf("POST /api/updates/check: %d %s", code, body)
	}
	var got dash.UpdateInfo
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Latest != "v0.3.1" || !got.CheckedAt.Equal(t0) {
		t.Fatalf("check reply = %+v, want v0.3.1 at %v", got, t0)
	}
	if ev, data := readEvent(); ev != dash.EventUpdates {
		t.Fatalf("SSE after check: %q %s", ev, data)
	} else {
		var evInfo dash.UpdateInfo
		if err := json.Unmarshal([]byte(data), &evInfo); err != nil || evInfo.Latest != "v0.3.1" {
			t.Fatalf("SSE updates data: %s (%v)", data, err)
		}
	}
	if len(f.cli) != 0 || f.execs != 0 {
		t.Fatalf("update check touched Incus: cli=%v execs=%d", f.cli, f.execs)
	}

	// Non-stable prerelease tag is refused and keeps the last known stable release.
	c.add(5 * time.Minute)
	code, body = doUpdateReq(t, "POST", ts.URL+"/api/updates/check", "owner@example.com", true, true)
	var eb dash.ErrorBody
	json.Unmarshal(body, &eb)
	if code != http.StatusBadGateway || eb.Code != dash.CodeUpdateCheck {
		t.Fatalf("prerelease tag: %d %s", code, body)
	}

	// Upstream network error returns 502 and preserves the previous successful result.
	code, body = doUpdateReq(t, "POST", ts.URL+"/api/updates/check", "owner@example.com", true, true)
	json.Unmarshal(body, &eb)
	if code != http.StatusBadGateway || eb.Code != dash.CodeUpdateCheck || eb.Error != dash.UpdateCheckFailed {
		t.Fatalf("lookup error: %d %s", code, body)
	}
	_, stateBody := doUpdateReq(t, "GET", ts.URL+"/api/state", "owner@example.com", false, false)
	st = dash.State{}
	json.Unmarshal(stateBody, &st)
	if st.Updates == nil || st.Updates.Latest != "v0.3.1" || !st.Updates.CheckedAt.Equal(t0) {
		t.Fatalf("state after error = %+v, want preserved v0.3.1 at %v", st.Updates, t0)
	}

	// ErrNoStable is a valid check result: checkedAt is recorded, latest is empty.
	c.add(5 * time.Minute)
	code, body = doUpdateReq(t, "POST", ts.URL+"/api/updates/check", "owner@example.com", true, true)
	if code != http.StatusOK {
		t.Fatalf("ErrNoStable: %d %s", code, body)
	}
	got = dash.UpdateInfo{}
	json.Unmarshal(body, &got)
	if got.Latest != "" || !got.CheckedAt.Equal(c.now()) {
		t.Fatalf("ErrNoStable reply = %+v", got)
	}
	if ev, _ := readEvent(); ev != dash.EventUpdates {
		t.Fatalf("SSE after ErrNoStable: %q", ev)
	}
}

type joiningContext struct {
	context.Context
	joined chan struct{}
}

func (c joiningContext) Done() <-chan struct{} {
	close(c.joined)
	return c.Context.Done()
}

func TestUpdateCheckCoalescesConcurrentClicks(t *testing.T) {
	release := make(chan struct{})
	lookup := &fakeReleaseLookup{fn: func(ctx context.Context) (update.Release, error) {
		select {
		case <-release:
			return update.Release{Tag: "v0.4.0"}, nil
		case <-ctx.Done():
			return update.Release{}, ctx.Err()
		}
	}}
	checker := &UpdateChecker{Lookup: lookup}
	var notifications atomic.Int32
	notify := func(dash.UpdateInfo) { notifications.Add(1) }
	type result struct {
		info dash.UpdateInfo
		err  error
	}
	join := func(ctx context.Context) <-chan result {
		t.Helper()
		joined := make(chan struct{})
		out := make(chan result, 1)
		go func() {
			info, err := checker.check(joiningContext{ctx, joined}, notify)
			out <- result{info, err}
		}()
		select {
		case <-joined:
		case <-ctx.Done():
			t.Fatal("caller did not join the check")
		}
		return out
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, cancelFirst := context.WithCancel(ctx)
	defer cancelFirst()
	a, b := join(first), join(ctx)
	cancelFirst()
	if err := (<-a).err; !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller: %v, want context.Canceled", err)
	}
	close(release)
	if got := <-b; got.err != nil || got.info.Latest != "v0.4.0" {
		t.Fatalf("second caller: %+v", got)
	}
	if lookups, events := lookup.calls.Load(), notifications.Load(); lookups != 1 || events != 1 {
		t.Fatalf("coalesced checks: %d lookups, %d events", lookups, events)
	}
	if _, err := checker.check(ctx, notify); err != nil || lookup.calls.Load() != 2 {
		t.Fatalf("next check: err=%v calls=%d", err, lookup.calls.Load())
	}
}

func TestUpdateCheckUnconfigured(t *testing.T) {
	c := &clock{t: t0}
	_, _, ts := newUpdateTestServer(t, nil, c)
	code, body := doUpdateReq(t, "POST", ts.URL+"/api/updates/check", "owner@example.com", true, true)
	var eb dash.ErrorBody
	json.Unmarshal(body, &eb)
	if code != http.StatusBadGateway || eb.Code != dash.CodeUpdateCheck {
		t.Fatalf("unconfigured: %d %s", code, body)
	}
}
