package desktop

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbe(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   bool
	}{{200, true}, {403, true}, {502, false}, {503, false}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/state" {
				t.Errorf("probed %s", r.URL.Path)
			}
			w.WriteHeader(tc.status)
		}))
		if got := probe(context.Background(), srv.Client(), srv.URL+"/"); got != tc.want {
			t.Errorf("status %d: probe = %v, want %v", tc.status, got, tc.want)
		}
		srv.Close()
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	if probe(context.Background(), dead.Client(), dead.URL+"/") {
		t.Error("a closed server answered")
	}
}

func TestOfflinePage(t *testing.T) {
	var answers atomic.Bool
	var ups atomic.Int32
	srv := httptest.NewUnstartedServer(nil)
	srv.Config.Handler = offlineHandler(srv.Listener.Addr().String(), func(context.Context) bool { return answers.Load() }, func() { ups.Add(1) })
	srv.Start()
	defer srv.Close()

	// Another name for the same address is another site's page: refused.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/state", nil)
	req.Host = "rebound.example.test"
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("other host: %v %v", res, err)
	} else {
		res.Body.Close()
	}

	get := func(path string) (*http.Response, string) {
		t.Helper()
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return res, string(b)
	}
	res, body := get("/")
	if res.StatusCode != 200 || !strings.Contains(body, "<html") || res.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("page: %d %q", res.StatusCode, body[:min(len(body), 80)])
	}
	if res, _ := get("/assets/app.js"); res.StatusCode != 200 {
		t.Fatalf("app.js: %d", res.StatusCode)
	}
	for _, p := range []string{"/api/state", "/api/events", "/api/jobs"} {
		res, body := get(p)
		if res.StatusCode != http.StatusBadGateway || !strings.Contains(body, `"code":"unreachable"`) {
			t.Fatalf("%s: %d %q", p, res.StatusCode, body)
		}
	}
	if ups.Load() != 0 {
		t.Fatal("up called while the PC doesn't answer")
	}

	// Once the PC answers, /api/state sends the window there and holds the request until the
	// window has left, which here is the client giving up.
	answers.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/state", nil)
	if res, err := http.DefaultClient.Do(req); err == nil {
		res.Body.Close()
		t.Fatalf("answered %d rather than holding", res.StatusCode)
	}
	if ups.Load() != 1 {
		t.Fatalf("up called %d times", ups.Load())
	}
	if res, _ := get("/api/events"); res.StatusCode != http.StatusBadGateway || ups.Load() != 1 {
		t.Fatalf("/api/events: %d, ups %d", res.StatusCode, ups.Load())
	}
}

func TestOutside(t *testing.T) {
	for link, want := range map[string]bool{
		"https://github.com/a-Fig/agent-box": true,
		"http://example.test/x":              true,
		"file:///c:/windows":                 false,
		"javascript:alert(1)":                false,
		"/api/state":                         false,
		"https://":                           false,
	} {
		if _, got := outside(link); got != want {
			t.Errorf("outside(%q) = %v, want %v", link, got, want)
		}
	}
}

// keeperAt is a keeper with a clock the test moves, recording where it sends the window.
func keeperAt(t *testing.T) (*keeper, *time.Time, *[]string) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	var went []string
	k := &keeper{dash: "https://pc.example.test/", offline: "http://127.0.0.1:5555/", now: func() time.Time { return now }}
	k.navigate = func(u string) { went = append(went, u) }
	k.seen = now
	return k, &now, &went
}

func TestKeeper(t *testing.T) {
	const dash, offline, dashO, offO = "https://pc.example.test/", "http://127.0.0.1:5555/", "https://pc.example.test", "http://127.0.0.1:5555"

	t.Run("a trip on its way isn't started again", func(t *testing.T) {
		k, now, went := keeperAt(t)
		k.up()
		*now = now.Add(5 * time.Second)
		k.up()
		if len(*went) != 1 || (*went)[0] != dash {
			t.Fatalf("went %q", *went)
		}
		k.here(dashO, true)
		*now = now.Add(10 * time.Second)
		k.tick()
		if len(*went) != 1 {
			t.Fatalf("went %q after arriving", *went)
		}
	})

	t.Run("a trip that doesn't arrive goes back", func(t *testing.T) {
		k, now, went := keeperAt(t)
		k.up()
		*now = now.Add(10 * time.Second)
		k.here(offO, true) // the offline page still answering while the dashboard loads
		k.tick()
		*now = now.Add(11 * time.Second)
		k.tick()
		if len(*went) != 2 || (*went)[1] != offline {
			t.Fatalf("went %q", *went)
		}
		k.up() // and the offline page can send it again
		if len(*went) != 3 || (*went)[2] != dash {
			t.Fatalf("went %q", *went)
		}
	})

	t.Run("a document that stops answering goes back", func(t *testing.T) {
		k, now, went := keeperAt(t)
		k.up()
		k.here(dashO, true)
		for range 2 {
			*now = now.Add(pingEvery)
			k.tick()
		}
		if len(*went) != 1 {
			t.Fatalf("went %q within %v", *went, answerWithin)
		}
		*now = now.Add(pingEvery)
		k.tick()
		if len(*went) != 2 || (*went)[1] != offline {
			t.Fatalf("went %q", *went)
		}
	})

	t.Run("an error or another site goes back", func(t *testing.T) {
		for _, tc := range []struct {
			origin string
			ok     bool
		}{{dashO, false}, {"https://elsewhere.example.test", true}, {offO, false}} {
			k, _, went := keeperAt(t)
			k.here(tc.origin, tc.ok)
			if len(*went) != 1 || (*went)[0] != offline {
				t.Errorf("%s %v: went %q", tc.origin, tc.ok, *went)
			}
		}
	})
}
