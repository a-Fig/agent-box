package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// remotes answers incus remote list with a canned listing.
type remotes struct {
	out  string
	code int
}

func (r remotes) Run(_ context.Context, c Cmd) (Result, error) {
	if c.Name != "incus" || strings.Join(c.Args, " ") != "--project agents remote list --format json" {
		return Result{Code: 1, Stderr: "unexpected " + c.Name + " " + strings.Join(c.Args, " ")}, nil
	}
	return Result{Stdout: r.out, Code: r.code, Stderr: "boom"}, nil
}

func TestDashURL(t *testing.T) {
	for _, tc := range []struct {
		name, list, want, err string
	}{
		{"addrs", `{"pc":{"Addrs":["https://pc.example.test:8443"],"Protocol":"incus"},"local":{"Addrs":["unix://"]}}`, "https://pc.example.test/", ""},
		{"addr", `{"pc":{"Addr":"https://pc.example.test:8443"}}`, "https://pc.example.test/", ""},
		{"last working", `{"pc":{"LastWorkingAddr":"https://pc.example.test:8443"}}`, "https://pc.example.test/", ""},
		{"ipv6", `{"pc":{"Addrs":["https://[fd7a::1]:8443"]}}`, "https://[fd7a::1]/", ""},
		{"no remote", `{"local":{"Addrs":["unix://"]}}`, "", `no incus remote "pc"`},
		{"not https", `{"pc":{"Addrs":["unix:///var/lib/incus/unix.socket"]}}`, "", "not an https address"},
		{"bad json", `nope`, "", "incus remote list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := testApp(nil)
			a.R = remotes{out: tc.list}
			got, err := a.DashURL(context.Background())
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("DashURL = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestDashURLIncusFails(t *testing.T) {
	a, _ := testApp(nil)
	a.R = remotes{code: 1}
	if _, err := a.DashURL(context.Background()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestUI(t *testing.T) {
	a, out := testApp(nil)
	a.R = remotes{out: `{"pc":{"Addrs":["https://pc.example.test:8443"]}}`}
	var opened []string
	a.Browse = func(u string) error { opened = append(opened, u); return nil }
	if c := run(context.Background(), a, []string{"ui"}, nil); c != 0 {
		t.Fatalf("exit %d: %s", c, out)
	}
	if strings.Join(opened, " ") != "https://pc.example.test/" || !strings.Contains(out.String(), "https://pc.example.test/") {
		t.Fatalf("opened %q, printed %q", opened, out)
	}

	opened, *out = nil, bytes.Buffer{}
	if c := run(context.Background(), a, []string{"ui", "--url", "http://127.0.0.1:7767/"}, nil); c != 0 || strings.Join(opened, " ") != "http://127.0.0.1:7767/" {
		t.Fatalf("exit %d, opened %q: %s", c, opened, out)
	}

	opened = nil
	if c := run(context.Background(), a, []string{"ui", "--url", "file:///etc/passwd"}, nil); c != 1 || opened != nil {
		t.Fatalf("exit %d, opened %q", c, opened)
	}
}

func TestHelpListsUIAndApp(t *testing.T) {
	for _, cmd := range []string{"box ui", "box app"} {
		if !strings.Contains(usageText(), cmd) {
			t.Errorf("usage lacks %s", cmd)
		}
	}
}
