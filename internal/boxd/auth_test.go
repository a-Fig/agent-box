package boxd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOwnersLetsOnlyListedLoginsInOverLoopback(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hi")) })
	h := SecurityHeaders(Owners([]string{" Owner@Example.com "}, ok))
	for _, c := range []struct {
		name, remote, login string
		want                int
		code                string
	}{
		{"owner", "127.0.0.1:5000", "owner@example.com", 200, ""},
		{"owner, other case", "[::1]:5000", "OWNER@example.com", 200, ""},
		{"no header: a tagged device or Funnel", "127.0.0.1:5000", "", 403, "not-owner"},
		{"a shared-in user from another tailnet", "127.0.0.1:5000", "someone@else.com", 403, "not-owner"},
		{"tagged devices' pseudo-login", "127.0.0.1:5000", "tagged-devices", 403, "not-owner"},
		{"not through tailscale serve", "10.22.127.5:5000", "owner@example.com", 403, "not-loopback"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.login != "" {
			r.Header.Set(LoginHeader, c.login)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want || (c.code != "" && !strings.Contains(w.Body.String(), `"code":"`+c.code+`"`)) {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
		}
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'self'") {
			t.Errorf("%s: no CSP", c.name)
		}
	}
}

func TestNoOwnersFailsClosed(t *testing.T) {
	h := Owners(nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("let in") }))
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set(LoginHeader, "anyone@example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("%d", w.Code)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "boxd.json")
	os.WriteFile(p, []byte(`{"owners":["a@b.c"]}`), 0o644)
	c, err := LoadConfig(p)
	if err != nil || c.Listen != DefaultListen || len(c.Owners) != 1 || c.HistoryDir == "" {
		t.Fatalf("%+v %v", c, err)
	}
	os.WriteFile(p, []byte(`{`), 0o644)
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("bad JSON accepted")
	}
}
