package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "protocol", "controller-v1", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func TestGoldenFixtures(t *testing.T) {
	t.Run("health.json", func(t *testing.T) {
		var want HealthResponse
		if err := json.Unmarshal(readFixture(t, "health.json"), &want); err != nil {
			t.Fatal(err)
		}
		if !want.OK || want.Protocol != 1 {
			t.Fatalf("unexpected health fixture: %+v", want)
		}
		srv := httptest.NewServer(&MemServer{HostToken: "host-secret", ClientToken: "client-secret"})
		defer srv.Close()

		c := &Client{URL: srv.URL}
		got, err := c.Health(context.Background())
		if err != nil || got != want {
			t.Fatalf("Health() = %+v, %v; want %+v", got, err, want)
		}
	})

	t.Run("put-state.json", func(t *testing.T) {
		var req PutStateRequest
		if err := json.Unmarshal(readFixture(t, "put-state.json"), &req); err != nil {
			t.Fatal(err)
		}
		if req.ID != "pc" || req.Name != "Gaming PC" || req.Seq != 42 || len(req.State.Machines) != 1 || len(req.State.Boxes) != 1 || len(req.State.Jobs) != 2 {
			t.Fatalf("unexpected put-state fixture decode: %+v", req)
		}

		now := time.Date(2026, 11, 27, 15, 50, 0, 0, time.UTC)
		srv := httptest.NewServer(&MemServer{
			HostToken:   "host-secret",
			ClientToken: "client-secret",
			Now:         func() time.Time { return now },
		})
		defer srv.Close()

		hostClient := &Client{URL: srv.URL, Token: "host-secret"}
		if err := hostClient.PutState(context.Background(), req.ID, req); err != nil {
			t.Fatalf("PutState() error = %v", err)
		}

		readClient := &Client{URL: srv.URL, Token: "client-secret"}
		hosts, err := readClient.Hosts(context.Background())
		if err != nil {
			t.Fatalf("Hosts() error = %v", err)
		}
		if len(hosts.Hosts) != 1 || hosts.Hosts[0].ID != "pc" || hosts.Hosts[0].Stale || !hosts.Hosts[0].SeenAt.Equal(now) {
			t.Fatalf("unexpected hosts response: %+v", hosts)
		}
		if len(hosts.Hosts[0].State.Jobs) != 2 || hosts.Hosts[0].State.Jobs[0].ID != "20261127-154810-7791" {
			t.Fatalf("round-tripped jobs = %+v", hosts.Hosts[0].State.Jobs)
		}
	})

	t.Run("hosts.json", func(t *testing.T) {
		var want HostsResponse
		if err := json.Unmarshal(readFixture(t, "hosts.json"), &want); err != nil {
			t.Fatal(err)
		}
		if len(want.Hosts) != 2 || want.Hosts[0].ID != "pc" || want.Hosts[0].Stale || want.Hosts[1].ID != "lab-linux" || !want.Hosts[1].Stale {
			t.Fatalf("unexpected hosts fixture: %+v", want)
		}

		now := want.Hosts[1].SeenAt
		ms := &MemServer{
			HostToken:   "host-secret",
			ClientToken: "client-secret",
			Now:         func() time.Time { return now },
		}
		srv := httptest.NewServer(ms)
		defer srv.Close()

		hostClient := &Client{URL: srv.URL, Token: "host-secret"}
		for _, h := range want.Hosts {
			now = h.SeenAt
			if err := hostClient.PutState(context.Background(), h.ID, PutStateRequest{
				ID:     h.ID,
				Name:   h.Name,
				Seq:    1,
				SentAt: h.SeenAt,
				State:  h.State,
			}); err != nil {
				t.Fatalf("PutState(%s): %v", h.ID, err)
			}
		}

		now = want.Now
		readClient := &Client{URL: srv.URL, Token: "client-secret"}
		got, err := readClient.Hosts(context.Background())
		if err != nil {
			t.Fatalf("Hosts() error = %v", err)
		}
		if !got.Now.Equal(want.Now) || len(got.Hosts) != 2 {
			t.Fatalf("Hosts() = %+v", got)
		}
		// MemServer sorts by ID: lab-linux then pc.
		if got.Hosts[0].ID != "lab-linux" || !got.Hosts[0].Stale || got.Hosts[1].ID != "pc" || got.Hosts[1].Stale {
			t.Fatalf("staleness mismatch: %+v", got.Hosts)
		}
	})

	t.Run("usage.json", func(t *testing.T) {
		var want UsageResponse
		if err := json.Unmarshal(readFixture(t, "usage.json"), &want); err != nil {
			t.Fatal(err)
		}
		if want.Day != "2026-11-27" || want.Requests != 2412 || want.Limit != 100000 {
			t.Fatalf("unexpected usage fixture: %+v", want)
		}

		now := time.Date(2026, 11, 27, 15, 50, 0, 0, time.UTC)
		srv := httptest.NewServer(&MemServer{
			HostToken:   "host-secret",
			ClientToken: "client-secret",
			Now:         func() time.Time { return now },
		})
		defer srv.Close()

		c := &Client{URL: srv.URL, Token: "client-secret"}
		got, err := c.Usage(context.Background())
		if err != nil {
			t.Fatalf("Usage() error = %v", err)
		}
		if got.Day != want.Day || got.Requests != 1 || got.Limit != want.Limit {
			t.Fatalf("Usage() = %+v", got)
		}
	})

	t.Run("error-unauthorized.json", func(t *testing.T) {
		var want ErrorResponse
		if err := json.Unmarshal(readFixture(t, "error-unauthorized.json"), &want); err != nil {
			t.Fatal(err)
		}
		if want.Error != "unauthorized" || want.Message == "" {
			t.Fatalf("unexpected unauthorized fixture: %+v", want)
		}

		srv := httptest.NewServer(&MemServer{HostToken: "host-secret", ClientToken: "client-secret"})
		defer srv.Close()

		c := &Client{URL: srv.URL}
		_, err := c.Hosts(context.Background())
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized || ae.Code != want.Error {
			t.Fatalf("Hosts() without token = %v, want 401 %q", err, want.Error)
		}
	})

	t.Run("error-too-large.json", func(t *testing.T) {
		var want ErrorResponse
		if err := json.Unmarshal(readFixture(t, "error-too-large.json"), &want); err != nil {
			t.Fatal(err)
		}
		if want.Error != "too_large" || want.Message == "" {
			t.Fatalf("unexpected too-large fixture: %+v", want)
		}

		srv := httptest.NewServer(&MemServer{HostToken: "host-secret", ClientToken: "client-secret"})
		defer srv.Close()

		c := &Client{URL: srv.URL, Token: "host-secret"}
		err := c.PutState(context.Background(), "pc", PutStateRequest{
			ID:     "pc",
			Name:   "Gaming PC",
			Seq:    1,
			SentAt: time.Now().UTC(),
			State: dash.State{
				Jobs: []dash.Job{{Key: "big", Tail: strings.Repeat("x", MaxBodyBytes)}},
			},
		})
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusRequestEntityTooLarge || ae.Code != want.Error {
			t.Fatalf("oversized PutState() = %v, want 413 %q", err, want.Error)
		}
	})
}

func TestMemServerAuthAndValidation(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	ms := &MemServer{
		HostToken:   "host-tok",
		ClientToken: "client-tok",
		Now:         func() time.Time { return now },
	}
	srv := httptest.NewServer(ms)
	defer srv.Close()

	// Wrong role tokens -> 403 forbidden.
	hostClient := &Client{URL: srv.URL, Token: "host-tok"}
	for _, fn := range []func() error{
		func() error { _, err := hostClient.Hosts(context.Background()); return err },
		func() error { _, err := hostClient.Usage(context.Background()); return err },
	} {
		var ae *APIError
		if err := fn(); !errors.As(err, &ae) || ae.Status != http.StatusForbidden || ae.Code != "forbidden" {
			t.Fatalf("host token on client endpoint = %v, want 403 forbidden", err)
		}
	}

	readClient := &Client{URL: srv.URL, Token: "client-tok"}
	err := readClient.PutState(context.Background(), "pc", PutStateRequest{
		ID: "pc", Name: "PC", Seq: 1, SentAt: now,
	})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusForbidden || ae.Code != "forbidden" {
		t.Fatalf("client token on PUT = %v, want 403 forbidden", err)
	}

	// Unknown token -> 401 unauthorized.
	badClient := &Client{URL: srv.URL, Token: "nope"}
	if _, err := badClient.Usage(context.Background()); !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized {
		t.Fatalf("bad token = %v, want 401", err)
	}

	// Non-Bearer auth header -> 401 unauthorized.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/hosts", nil)
	req.Header.Set("Authorization", "Basic abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Basic auth status = %d, want 401", resp.StatusCode)
	}

	// Invalid host IDs -> 400 bad_request.
	for _, badID := range []string{"PC", "bad_id", "has space", strings.Repeat("a", 41)} {
		err := hostClient.PutState(context.Background(), badID, PutStateRequest{
			ID: badID, Name: "PC", Seq: 1, SentAt: now,
		})
		if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest || ae.Code != "bad_request" {
			t.Fatalf("PutState(%q) = %v, want 400 bad_request", badID, err)
		}
	}

	// Mismatched envelope ID or missing name/sentAt -> 400 bad_request.
	for _, tc := range []PutStateRequest{
		{ID: "other", Name: "PC", Seq: 1, SentAt: now},
		{ID: "pc", Name: "", Seq: 1, SentAt: now},
		{ID: "pc", Name: "PC", Seq: 1},
	} {
		err := hostClient.PutState(context.Background(), "pc", tc)
		if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest || ae.Code != "bad_request" {
			t.Fatalf("PutState(%+v) = %v, want 400 bad_request", tc, err)
		}
	}

	// Unknown route -> 404 not_found.
	resp, err = http.Get(srv.URL + "/v1/unknown")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route status = %d, want 404", resp.StatusCode)
	}
}

func TestStalenessBoundaryAndDailyUsageRollover(t *testing.T) {
	now := time.Date(2026, 11, 27, 23, 55, 0, 0, time.UTC)
	ms := &MemServer{
		HostToken:   "host-tok",
		ClientToken: "client-tok",
		Now:         func() time.Time { return now },
	}
	srv := httptest.NewServer(ms)
	defer srv.Close()

	hostClient := &Client{URL: srv.URL, Token: "host-tok"}
	readClient := &Client{URL: srv.URL, Token: "client-tok"}

	if err := hostClient.PutState(context.Background(), "pc", PutStateRequest{
		ID: "pc", Name: "PC", Seq: 1, SentAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// Exactly 180s later is NOT stale (> 180s is stale).
	now = now.Add(StaleAfter)
	hosts, err := readClient.Hosts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if hosts.Hosts[0].Stale {
		t.Fatalf("expected stale=false at exactly %s", StaleAfter)
	}

	// 181s later IS stale.
	now = now.Add(time.Second)
	hosts, err = readClient.Hosts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !hosts.Hosts[0].Stale {
		t.Fatalf("expected stale=true at %s", StaleAfter+time.Second)
	}

	u1, err := readClient.Usage(context.Background())
	if err != nil || u1.Day != "2026-11-27" || u1.Requests != 4 {
		t.Fatalf("usage day 1 = %+v, %v", u1, err)
	}

	// Advance past UTC midnight -> counter resets.
	now = time.Date(2026, 11, 28, 0, 0, 1, 0, time.UTC)
	u2, err := readClient.Usage(context.Background())
	if err != nil || u2.Day != "2026-11-28" || u2.Requests != 1 {
		t.Fatalf("usage day 2 = %+v, %v", u2, err)
	}
}

func TestTrimState(t *testing.T) {
	base := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	var jobs []dash.Job
	// 2 active jobs (running + queued) and 25 finished jobs (done + lost), oldest first.
	jobs = append(jobs,
		dash.Job{Key: "run-1", State: dash.JobRunning, StartedAt: base},
		dash.Job{Key: "q-1", State: dash.JobQueued, StartedAt: base.Add(time.Minute)},
		dash.Job{Key: "pause-1", State: dash.JobPaused, StartedAt: base.Add(2 * time.Minute)},
	)
	for i := 0; i < 25; i++ {
		st := dash.JobDone
		if i%5 == 0 {
			st = dash.JobLost
		}
		ended := base.Add(time.Duration(i+1) * time.Minute)
		jobs = append(jobs, dash.Job{
			Key:       fmt.Sprintf("fin-%02d", i),
			State:     st,
			StartedAt: ended.Add(-30 * time.Second),
			EndedAt:   &ended,
		})
	}
	orig := dash.State{
		Machines: []dash.Machine{{ID: "pc"}},
		Boxes:    []dash.Box{{Name: "b1"}},
		Jobs:     jobs,
		Now:      base,
	}

	trimmed := TrimState(orig)
	if len(orig.Jobs) != 28 {
		t.Fatalf("TrimState mutated input length to %d", len(orig.Jobs))
	}
	if len(trimmed.Jobs) != 3+MaxFinishedJobs {
		t.Fatalf("len(trimmed.Jobs) = %d, want %d", len(trimmed.Jobs), 3+MaxFinishedJobs)
	}
	if trimmed.Jobs[0].Key != "run-1" || trimmed.Jobs[1].Key != "q-1" || trimmed.Jobs[2].Key != "pause-1" {
		t.Fatalf("active jobs not preserved first: %+v", trimmed.Jobs[:3])
	}
	// Newest 20 finished jobs are fin-24 down to fin-05.
	if trimmed.Jobs[3].Key != "fin-24" || trimmed.Jobs[len(trimmed.Jobs)-1].Key != "fin-05" {
		t.Fatalf("finished window = %s .. %s, want fin-24 .. fin-05", trimmed.Jobs[3].Key, trimmed.Jobs[len(trimmed.Jobs)-1].Key)
	}

	// Nil slices stay nil.
	empty := TrimState(dash.State{})
	if empty.Machines != nil || empty.Boxes != nil || empty.Jobs != nil {
		t.Fatalf("TrimState(zero) produced non-nil slices: %+v", empty)
	}
}

func TestValidHostIDAndSanitizeAndURL(t *testing.T) {
	if !ValidHostID("pc") || !ValidHostID("lab-linux-1") || ValidHostID("") || ValidHostID("PC") || ValidHostID("a_b") {
		t.Fatal("ValidHostID mismatch")
	}
	for in, want := range map[string]string{
		"pc":                            "pc",
		"Gaming PC":                     "gaming-pc",
		"Lab_Linux--01!":                "lab-linux-01",
		"---":                           "pc",
		strings.Repeat("a", 50):         strings.Repeat("a", 40),
		strings.Repeat("a", 39) + "-bc": strings.Repeat("a", 39),
	} {
		if got := SanitizeHostID(in); got != want || !ValidHostID(got) {
			t.Errorf("SanitizeHostID(%q) = %q, want %q", in, got, want)
		}
	}

	for raw, want := range map[string]bool{
		"https://ctrl.example.com":      true,
		"https://ctrl.example.com/v1":   true,
		"http://127.0.0.1:8787":         true,
		"http://[::1]:8787":             true,
		"http://localhost:8787":         true,
		"http://ctrl.example.com":       false,
		"http://10.0.0.2:8787":          false,
		"ftp://ctrl.example.com":        false,
		"https://u:p@ctrl.example.com":  false,
		"https://ctrl.example.com?q=1":  false,
		"https://ctrl.example.com#frag": false,
		"/relative":                     false,
		"":                              false,
	} {
		if got := ValidControllerURL(raw); got != want {
			t.Errorf("ValidControllerURL(%q) = %v, want %v", raw, got, want)
		}
	}
}
