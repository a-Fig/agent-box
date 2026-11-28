package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a-Fig/agent-box/internal/mode"
)

func TestCheckDashURL(t *testing.T) {
	for _, ok := range []string{"https://box.example.ts.net/", "http://127.0.0.1:7766/"} {
		if _, err := checkDashURL(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "javascript:alert(1)", "file:///C:/x", "https:///path", "https://a b/", "https://a/\n", "box.example.ts.net"} {
		if u, err := checkDashURL(bad); err == nil {
			t.Errorf("%q accepted as %q", bad, u)
		}
	}
}

func TestParseServeStatus(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"boxd's handler", `{"TCP":{"443":{"HTTPS":true}},"Web":{"agent-box.example.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:7766"}}}}}`, "https://agent-box.example.ts.net/"},
		{"boxd's among others", `{"Web":{"a.example.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:9000"}}},"b.example.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:7766/"}}}}}`, "https://b.example.ts.net/"},
	}
	for _, c := range cases {
		got, err := parseServeStatus([]byte(c.in))
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	for _, bad := range []string{``, `not json`, `{}`, `{"Web":{"a.example.ts.net:8443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:7766"}}}}}`,
		`{"Web":{"z.example.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:1"}}},"y.example.ts.net:443":{"Handlers":{"/":{"Text":"hi"}}}}}`} {
		if got, err := parseServeStatus([]byte(bad)); err == nil {
			t.Errorf("%q: got %q", bad, got)
		}
	}
}

func TestParseTailscaleStatus(t *testing.T) {
	for in, want := range map[string]string{
		`{"Self":{"DNSName":"agent-box.example.ts.net."}}`: "https://agent-box.example.ts.net/",
		`{"Self":{"DNSName":"agent-box.example.ts.net"}}`:  "https://agent-box.example.ts.net/",
	} {
		if got, err := parseTailscaleStatus([]byte(in)); err != nil || got != want {
			t.Errorf("%s: got %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{``, `nope`, `{"Self":{}}`, `{"Self":{"DNSName":"."}}`} {
		if got, err := parseTailscaleStatus([]byte(bad)); err == nil {
			t.Errorf("%q: got %q", bad, got)
		}
	}
}

func TestSaveModeKeepsDashboardURL(t *testing.T) {
	p := filepath.Join(t.TempDir(), "boxtray.json")
	if err := os.WriteFile(p, []byte(`{"mode":"my-pc","dashboard_url":"https://dash.example/","other":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveMode(p, mode.Full); err != nil {
		t.Fatal(err)
	}
	if got := loadMode(p); got != mode.Full {
		t.Errorf("mode %q", got)
	}
	if got := loadDashboardURL(p); got != "https://dash.example/" {
		t.Errorf("dashboard_url %q", got)
	}
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), `"other":1`) {
		t.Errorf("other keys lost: %s", b)
	}
	if got := loadDashboardURL(filepath.Join(t.TempDir(), "none.json")); got != "" {
		t.Errorf("missing file: %q", got)
	}
}

// fakeTailscale answers the resolver's two commands.
type fakeTailscale struct {
	serve, status string
	err           error
	calls         int
}

func (f *fakeTailscale) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if len(args) > 1 && args[1] == "serve" {
		return []byte(f.serve), nil
	}
	return []byte(f.status), nil
}

func TestDashResolverPrecedence(t *testing.T) {
	const serve = `{"Web":{"serve.example:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:7766"}}}}}`
	const status = `{"Self":{"DNSName":"self.example."}}`
	ctx := context.Background()
	up := true
	explicit := ""
	ts := &fakeTailscale{serve: serve, status: status}
	d := &dashResolver{
		explicit: func() string { return explicit },
		up:       func(context.Context) bool { return up },
		run:      ts.run,
	}
	want := func(step, w string) {
		t.Helper()
		got, err := d.resolve(ctx)
		if err != nil || got != w {
			t.Fatalf("%s: got %q, %v; want %q", step, got, err, w)
		}
	}

	if !d.needsWarm() {
		t.Error("nothing known yet: should warm")
	}
	want("serve status", "https://serve.example/")
	if d.needsWarm() {
		t.Error("known: shouldn't warm")
	}

	ts.serve = `{}`
	want("tailscale status", "https://self.example/")

	explicit = "https://mine.example/"
	n := ts.calls
	want("dashboard_url", "https://mine.example/")
	if ts.calls != n {
		t.Error("dashboard_url should skip the lookup")
	}
	explicit = "javascript:alert(1)"
	if _, err := d.resolve(ctx); err == nil {
		t.Error("a bad dashboard_url should fail, not fall through")
	}
	explicit = ""

	up = false
	n = ts.calls
	want("cached while WSL is down", "https://self.example/")
	if ts.calls != n {
		t.Error("a lookup while WSL is down would start it")
	}

	up = true
	ts.err = errors.New("wsl failed")
	want("cached when the lookup fails", "https://self.example/")

	empty := &dashResolver{explicit: func() string { return "" }, up: func(context.Context) bool { return false }, run: ts.run}
	if _, err := empty.resolve(ctx); err == nil || !strings.Contains(err.Error(), "dashboard_url") {
		t.Errorf("nothing known: %v", err)
	}
}

func TestParseDashboardArgs(t *testing.T) {
	if _, ok, _ := parseDashboardArgs(nil); ok {
		t.Error("no args")
	}
	if _, ok, _ := parseDashboardArgs([]string{"--headless"}); ok {
		t.Error("--headless")
	}
	page, ok, err := parseDashboardArgs([]string{"--dashboard", "--url", "https://d.example/"})
	if !ok || err != nil || page != "https://d.example/" {
		t.Errorf("got %q %v %v", page, ok, err)
	}
	for _, bad := range [][]string{{"--dashboard"}, {"--dashboard", "--url"}, {"--dashboard", "--url", "file:///x"}, {"--dashboard", "x", "https://d.example/"}} {
		if _, ok, err := parseDashboardArgs(bad); !ok || err == nil {
			t.Errorf("%q: ok %v err %v", bad, ok, err)
		}
	}
}

func TestDashResolverLeavesWSLAlone(t *testing.T) {
	ctx := context.Background()
	ts := &fakeTailscale{serve: `{"Web":{"serve.example:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:7766"}}}}}`}
	up, busy := true, true
	d := &dashResolver{
		explicit: func() string { return "" },
		up:       func(context.Context) bool { return up },
		busy:     func() bool { return busy },
		run:      ts.run,
	}
	// Release memory in progress: neither a click nor a warm-up may run wsl.exe -d agents.
	if _, err := d.resolve(ctx); err == nil {
		t.Error("resolve while releasing, with nothing cached: want an error")
	}
	if _, err := d.refresh(ctx); err == nil {
		t.Error("refresh (warm) while releasing: want an error")
	}
	busy, up = false, false
	if _, err := d.refresh(ctx); err == nil {
		t.Error("refresh (warm) while WSL is down: want an error")
	}
	if ts.calls != 0 {
		t.Fatalf("ran %d commands in a distro that is down or being stopped", ts.calls)
	}

	// A release that starts between the two lookups stops the second.
	up = true
	ts.serve = `{}`
	d.run = func(ctx context.Context, args ...string) ([]byte, error) {
		busy = true
		return ts.run(ctx, args...)
	}
	if _, err := d.refresh(ctx); err == nil {
		t.Error("want an error")
	}
	if ts.calls != 1 {
		t.Errorf("ran %d commands, want 1", ts.calls)
	}
}
