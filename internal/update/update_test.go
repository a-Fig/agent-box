package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAfter(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.3.1", "v0.3.0", true},
		{"v0.3.0", "v0.3.1", false},
		{"v0.10.0", "v0.9.9", true},
		{"v0.3.0", "v0.3.0", false},
		{"v0.3.1-nightly.20261125.1", "v0.3.0", true},
		{"v0.3.1-nightly.20261125.1", "v0.3.1", false}, // a nightly comes before its release
		{"v0.3.1", "v0.3.1-nightly.20261126.4", true},
		{"v0.3.1-nightly.20261125.2", "v0.3.1-nightly.20261125.1", true},
		{"v0.3.1-nightly.20261126.1", "v0.3.1-nightly.20261125.9", true},
		{"v0.3.0", "", true},  // anything beats a dev build
		{"", "v0.3.0", false}, // a dev build never comes after
		{"", "", false},
		{"v0.3", "v0.2.0", false},
		{"v0.0.0-test.6", "v0.0.0-test.5", true},
		{"v0.0.0-test.6", "v0.0.0", false},
		{"v0.3.1-test.9", "v0.3.1-nightly.20261125.1", false},
	} {
		if got := After(c.a, c.b); got != c.want {
			t.Errorf("After(%q, %q) = %t", c.a, c.b, got)
		}
	}
}

// fakeGitHub serves one release with a box asset; sums overrides its SHA256SUMS.
func fakeGitHub(t *testing.T, tag, body, sums string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + Repo + "/releases/latest", "/repos/" + Repo + "/releases/tags/" + tag:
			fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":"box-linux-amd64","browser_download_url":"%s/dl/box"},{"name":"SHA256SUMS","browser_download_url":"%s/dl/sums"}]}`, tag, srv.URL, srv.URL)
		case "/dl/box":
			fmt.Fprint(w, body)
		case "/dl/sums":
			fmt.Fprint(w, sums)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLatestAndAsset(t *testing.T) {
	sum := sha256.Sum256([]byte("new box"))
	srv := fakeGitHub(t, "v0.3.1", "new box", hex.EncodeToString(sum[:])+"  box-linux-amd64\nabc  other\n")
	c := Client{API: srv.URL}
	rel, err := c.Latest(context.Background())
	if err != nil || rel.Tag != "v0.3.1" {
		t.Fatalf("latest: %+v %v", rel, err)
	}
	b, err := c.Asset(context.Background(), rel, "box-linux-amd64")
	if err != nil || string(b) != "new box" {
		t.Fatalf("asset: %q %v", b, err)
	}
	if _, err := c.Asset(context.Background(), rel, "box-windows-amd64.exe"); err == nil {
		t.Fatal("a missing asset was found")
	}
	if rel, err := c.ByTag(context.Background(), "v0.3.1"); err != nil || rel.Tag != "v0.3.1" {
		t.Fatalf("by tag: %+v %v", rel, err)
	}
	if _, err := c.ByTag(context.Background(), "../x"); err == nil {
		t.Fatal("a bad tag was looked up")
	}
}

func TestAssetRefusesAWrongSum(t *testing.T) {
	for _, sums := range []string{"0000  box-linux-amd64\n", "", "deadbeef  something-else\n"} {
		srv := fakeGitHub(t, "v0.3.1", "tampered", sums)
		c := Client{API: srv.URL}
		rel, _ := c.Latest(context.Background())
		if _, err := c.Asset(context.Background(), rel, "box-linux-amd64"); err == nil || !strings.Contains(err.Error(), "SHA256SUMS") {
			t.Errorf("sums %q: err %v", sums, err)
		}
	}
}

// Before the first stable release GitHub answers latest with 404: Latest says so plainly.
func TestLatestWithNoStableRelease(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	if _, err := (Client{API: srv.URL}).Latest(context.Background()); !errors.Is(err, ErrNoStable) || err.Error() != "no stable release yet" {
		t.Fatalf("err = %v", err)
	}
	if _, err := (Client{API: srv.URL}).ByTag(context.Background(), "v0.3.0"); err == nil || errors.Is(err, ErrNoStable) {
		t.Fatalf("ByTag err = %v", err)
	}
}
