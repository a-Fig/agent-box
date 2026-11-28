package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	cfworker "github.com/a-Fig/agent-box/controller"
	modalrunner "github.com/a-Fig/agent-box/provision/modal"
)

// setupRunner records each command, with what it read on stdin and whether
// the files it was pointed at existed then.
type setupRunner struct {
	calls []setupCall
	reply func(c Cmd) (Result, error)
}

type setupCall struct {
	line  string
	cmd   Cmd
	stdin string
}

func (r *setupRunner) Run(_ context.Context, c Cmd) (Result, error) {
	in := ""
	if c.Stdin != nil {
		b, _ := io.ReadAll(c.Stdin)
		in = string(b)
	}
	r.calls = append(r.calls, setupCall{line: c.Name + " " + strings.Join(c.Args, " "), cmd: c, stdin: in})
	if r.reply != nil {
		return r.reply(c)
	}
	return Result{}, nil
}

func setupConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("APPDATA", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	cfg, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(cfg, "agent-box")
}

func assertNoSecretInArgs(t *testing.T, calls []setupCall, secrets ...string) {
	t.Helper()
	for _, c := range calls {
		for _, s := range secrets {
			if s == "" {
				t.Fatal("empty secret in assertion")
			}
			for _, arg := range append([]string{c.cmd.Name, c.cmd.Dir}, append(c.cmd.Args, c.cmd.Env...)...) {
				if strings.Contains(arg, s) {
					t.Fatalf("secret appears in command %q", c.line)
				}
			}
		}
	}
}

func assertMode0600(t *testing.T, p string) {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("%s mode %v, want 0600", p, st.Mode().Perm())
	}
}

const existingHosts = `{
  "hosts": [
    {"name": "pc", "remote": "pc", "project": "agents", "dashboard_url": "https://pc.example:8443", "priority": 5, "controller_id": "gaming-pc", "note": "kept"}
  ],
  "comment": "top-level keys survive"
}
`

func TestSetupModal(t *testing.T) {
	dir := setupConfigDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	hostsPath := filepath.Join(dir, "hosts.json")
	if err := os.WriteFile(hostsPath, []byte(existingHosts), 0o644); err != nil {
		t.Fatal(err)
	}
	const key, secret = "wk-SECRETKEY123", "ws-SECRETVALUE456"
	const url = "https://me--agent-box-runner-web-app.modal.run"
	var deployDir string
	r := &setupRunner{reply: func(c Cmd) (Result, error) {
		if len(c.Args) > 0 && c.Args[0] == "deploy" {
			deployDir = c.Dir
			for _, f := range []string{"agent_box_runner.py", "core.py", "rates.py"} {
				if _, err := os.Stat(filepath.Join(c.Dir, f)); err != nil {
					t.Errorf("deploy dir lacks %s", f)
				}
			}
			return Result{Stdout: "✓ Created web function web_app => " + url + "\n✓ App deployed!"}, nil
		}
		return Result{Stdout: "modal client version: 1.6.0"}, nil
	}}
	var put map[string]any
	var putHeaders http.Header
	var out, errb bytes.Buffer
	a := &App{R: r, Out: &out, Err: &errb, HTTP: func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPut || req.URL.String() != url+"/v1/config" {
			t.Errorf("request %s %s", req.Method, req.URL)
		}
		putHeaders = req.Header
		_ = json.NewDecoder(req.Body).Decode(&put)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"capUSD":10,"maxConcurrent":2,"maxTimeoutSec":1800,"keepSec":0,"mode":"function"}`)), Header: http.Header{}}, nil
	}}
	stdin := strings.NewReader(key + "\n" + secret + "\n")
	code := run(context.Background(), a, []string{"setup", "modal", "--cap", "10", "--max-concurrent", "2", "--max-timeout", "30m", "--token-in", "-"}, stdin)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var lines []string
	for _, c := range r.calls {
		lines = append(lines, c.line)
	}
	want := []string{"modal --version", "modal deploy " + filepath.Join(deployDir, "agent_box_runner.py")}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	assertNoSecretInArgs(t, r.calls, key, secret)
	if _, err := os.Stat(deployDir); !os.IsNotExist(err) {
		t.Errorf("temporary deploy dir %s left behind", deployDir)
	}
	if putHeaders.Get("Modal-Key") != key || putHeaders.Get("Modal-Secret") != secret {
		t.Errorf("PUT headers %v", putHeaders)
	}
	if put["capUSD"] != 10.0 || put["maxConcurrent"] != 2.0 || put["maxTimeoutSec"] != 1800.0 || put["mode"] != "function" {
		t.Errorf("PUT body %v", put)
	}
	if strings.Contains(out.String()+errb.String(), secret) || strings.Contains(out.String()+errb.String(), key) {
		t.Error("output shows the token")
	}
	if !strings.Contains(out.String(), "workspace spend limit") {
		t.Error("no reminder about Modal's spend limit")
	}

	tokenFile := filepath.Join(dir, "modal-token.json")
	assertMode0600(t, tokenFile)
	headers, err := loadCloudHeaders(tokenFile)
	if err != nil || headers["Modal-Key"] != key || headers["Modal-Secret"] != secret {
		t.Fatalf("token file headers %v, %v", headers, err)
	}

	var got struct {
		Hosts   []map[string]any `json:"hosts"`
		Comment string           `json:"comment"`
	}
	raw, _ := os.ReadFile(hostsPath)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Comment != "top-level keys survive" || len(got.Hosts) != 2 {
		t.Fatalf("hosts.json:\n%s", raw)
	}
	pc := got.Hosts[0]
	if pc["name"] != "pc" || pc["priority"] != 5.0 || pc["controller_id"] != "gaming-pc" || pc["note"] != "kept" || pc["dashboard_url"] != "https://pc.example:8443" {
		t.Errorf("existing host changed: %v", pc)
	}
	m := got.Hosts[1]
	if m["name"] != "modal" || m["kind"] != "modal" || m["endpoint"] != url || m["token_file"] != tokenFile || m["mode"] != "function" || m["priority"] != -1.0 {
		t.Errorf("modal host: %v", m)
	}
	hosts, err := a.configuredHosts()
	if err != nil || len(hosts) != 2 || hosts[1].Priority != -1 {
		t.Fatalf("configuredHosts %+v, %v", hosts, err)
	}
}

func TestSetupModalUpdatesExistingEntry(t *testing.T) {
	dir := setupConfigDir(t)
	hostsPath := filepath.Join(dir, "hosts.json")
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(hostsPath, []byte(`{"hosts":[{"name":"modal","kind":"modal","endpoint":"https://old.modal.run","token_file":"t.json","priority":3,"extra":true}]}`), 0o644)
	_, after, err := mergeModalHost(hostsPath, "pc", "agents", "modal", "https://new.modal.run", "/x/modal-token.json", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Hosts []map[string]any }
	_ = json.Unmarshal(after, &got)
	if len(got.Hosts) != 1 {
		t.Fatalf("%s", after)
	}
	h := got.Hosts[0]
	if h["endpoint"] != "https://new.modal.run" || h["mode"] != "sandbox" || h["priority"] != 3.0 || h["extra"] != true {
		t.Errorf("merged: %v", h)
	}

	_ = os.WriteFile(hostsPath, []byte(`{"hosts":[{"name":"modal","remote":"pc"}]}`), 0o644)
	if _, _, err := mergeModalHost(hostsPath, "pc", "agents", "modal", "https://new.modal.run", "t", "function"); err == nil || !strings.Contains(err.Error(), "isn't a modal host") {
		t.Errorf("name clash err %v", err)
	}
}

func TestSetupModalDryRun(t *testing.T) {
	dir := setupConfigDir(t)
	r := &setupRunner{}
	var out, errb bytes.Buffer
	a := &App{R: r, Remote: "pc", Project: "agents", Out: &out, Err: &errb, HTTP: func(*http.Request) (*http.Response, error) {
		t.Error("dry run touched the network")
		return nil, errors.New("no")
	}}
	code := run(context.Background(), a, []string{"setup", "modal", "--dry-run", "--token-in", "-", "--mode", "sandbox"}, strings.NewReader("must not be read"))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if len(r.calls) != 0 {
		t.Fatalf("dry run ran %v", r.calls)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("dry run wrote the config dir")
	}
	s := out.String()
	for _, want := range []string{
		"$30 of free compute credit", "$25.00 a month", "min_containers=0", "about 3x",
		"card on file", "dry run: nothing below runs.",
		"1. run: modal --version", "2. read the proxy-auth token from stdin",
		"agent_box_runner.py, core.py, rates.py", "4. run: modal deploy <tmp>/agent_box_runner.py",
		filepath.Join(dir, "modal-token.json") + " (mode 0600)",
		`PUT <URL from modal deploy>/v1/config {"capUSD":25,"maxConcurrent":4,"maxTimeoutSec":3600,"mode":"sandbox"}`,
		`+       "kind": "modal",`, `+       "priority": -1,`, `+       "mode": "sandbox",`,
		`+       "remote": "pc"`, `+       "project": "agents",`,
		"workspace spend limit",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("dry run lacks %q:\n%s", want, s)
		}
	}
}

func TestSetupModalRefusesWithoutToken(t *testing.T) {
	setupConfigDir(t)
	var out, errb bytes.Buffer
	a := &App{R: &setupRunner{}, Out: &out, Err: &errb}
	if code := run(context.Background(), a, []string{"setup", "modal"}, nil); code != 1 || !strings.Contains(errb.String(), "--token-in is required") {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
}

func TestParseModalToken(t *testing.T) {
	for _, tc := range []struct{ name, in, wantErr string }{
		{"wrapped", `{"headers":{"Modal-Key":"k","Modal-Secret":"s"}}`, ""},
		{"flat", `{"Modal-Key":"k","Modal-Secret":"s"}`, ""},
		{"header lines", "Modal-Key: k\r\nModal-Secret: s\r\n", ""},
		{"bare lines", "k\ns\n", ""},
		{"empty", "  \n", "empty"},
		{"one bare line", "k\n", "two lines"},
		{"empty value", "Modal-Key: k\nModal-Secret:\n", "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := parseModalToken([]byte(tc.in))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || h["Modal-Key"] != "k" || h["Modal-Secret"] != "s" {
				t.Fatalf("%v, %v", h, err)
			}
		})
	}
}

func TestSetupCloudflare(t *testing.T) {
	for _, installed := range []bool{true, false} {
		t.Run(map[bool]string{true: "wrangler", false: "npx"}[installed], func(t *testing.T) {
			dir := setupConfigDir(t)
			_ = os.MkdirAll(dir, 0o700)
			controllerPath := filepath.Join(dir, "controller.json")
			_ = os.WriteFile(controllerPath, []byte(`{"url":"https://old.workers.dev","token_file":"old","note":"kept"}`), 0o644)
			const url = "https://agent-box-controller.me.workers.dev"
			var deployDir string
			r := &setupRunner{}
			r.reply = func(c Cmd) (Result, error) {
				if c.Name == "wrangler" && !installed {
					return Result{}, exec.ErrNotFound
				}
				args := strings.Join(c.Args, " ")
				switch {
				case strings.HasSuffix(args, "whoami"):
					return Result{Stdout: "You are logged in with an OAuth Token."}, nil
				case strings.HasSuffix(args, "deploy"):
					deployDir = c.Dir
					for _, f := range []string{"wrangler.jsonc", "package.json", "src/index.js", "src/fleet.js", "src/http.js"} {
						if _, err := os.Stat(filepath.Join(c.Dir, filepath.FromSlash(f))); err != nil {
							t.Errorf("deploy dir lacks %s", f)
						}
					}
					return Result{Stdout: "Uploaded agent-box-controller\nDeployed agent-box-controller triggers\n  " + url + "\n"}, nil
				}
				return Result{}, nil
			}
			var out, errb bytes.Buffer
			a := &App{R: r, Out: &out, Err: &errb}
			if code := run(context.Background(), a, []string{"setup", "cloudflare"}, nil); code != 0 {
				t.Fatalf("exit %d: %s", code, errb.String())
			}
			w := "wrangler"
			var want []string
			if installed {
				want = []string{"wrangler --version"}
			} else {
				w = "npx --yes " + wranglerPin
				want = []string{"wrangler --version"}
			}
			want = append(want, w+" whoami", w+" deploy", w+" secret put HOST_TOKEN", w+" secret put CLIENT_TOKEN")
			var lines []string
			for _, c := range r.calls {
				lines = append(lines, c.line)
			}
			if strings.Join(lines, "\n") != strings.Join(want, "\n") {
				t.Fatalf("commands:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
			}
			for _, c := range r.calls[len(r.calls)-3:] {
				if c.cmd.Dir != deployDir {
					t.Errorf("%q ran in %q, want the worker dir", c.line, c.cmd.Dir)
				}
			}
			if _, err := os.Stat(deployDir); !os.IsNotExist(err) {
				t.Error("temporary worker dir left behind")
			}
			hostTok := strings.TrimSpace(r.calls[len(r.calls)-2].stdin)
			clientTok := strings.TrimSpace(r.calls[len(r.calls)-1].stdin)
			if len(hostTok) != 64 || len(clientTok) != 64 || hostTok == clientTok {
				t.Fatalf("tokens %q %q", hostTok, clientTok)
			}
			assertNoSecretInArgs(t, r.calls, hostTok, clientTok)
			if strings.Contains(out.String()+errb.String(), hostTok) || strings.Contains(out.String()+errb.String(), clientTok) {
				t.Error("output shows a token")
			}
			for p, tok := range map[string]string{"controller-client-token": clientTok, "controller-host-token": hostTok} {
				p = filepath.Join(dir, p)
				assertMode0600(t, p)
				if b, _ := os.ReadFile(p); strings.TrimSpace(string(b)) != tok {
					t.Errorf("%s holds the wrong token", p)
				}
			}
			var cfg map[string]any
			raw, _ := os.ReadFile(controllerPath)
			_ = json.Unmarshal(raw, &cfg)
			if cfg["url"] != url || cfg["token_file"] != filepath.Join(dir, "controller-client-token") || cfg["note"] != "kept" {
				t.Errorf("controller.json %s", raw)
			}
			c, tok, err := configuredController()
			if err != nil || c == nil || tok != clientTok {
				t.Fatalf("configuredController %v %v", c, err)
			}
			if !strings.Contains(out.String(), `"token_file": "/etc/agent-box/controller-host-token"`) {
				t.Errorf("no boxd.json snippet:\n%s", out.String())
			}
		})
	}
}

func TestSetupCloudflareNotSignedIn(t *testing.T) {
	setupConfigDir(t)
	r := &setupRunner{reply: func(c Cmd) (Result, error) {
		if strings.HasSuffix(strings.Join(c.Args, " "), "whoami") {
			return Result{Stdout: "You are not authenticated. Please run `wrangler login`."}, nil
		}
		return Result{}, nil
	}}
	var out, errb bytes.Buffer
	a := &App{R: r, Out: &out, Err: &errb}
	if code := run(context.Background(), a, []string{"setup", "cloudflare"}, nil); code != 1 || !strings.Contains(errb.String(), "wrangler login") {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if len(r.calls) != 2 {
		t.Errorf("ran past whoami: %v", r.calls)
	}
}

func TestSetupCloudflareDryRun(t *testing.T) {
	dir := setupConfigDir(t)
	r := &setupRunner{}
	var out, errb bytes.Buffer
	a := &App{R: r, Out: &out, Err: &errb}
	if code := run(context.Background(), a, []string{"setup", "cloudflare", "--dry-run"}, nil); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if len(r.calls) != 0 {
		t.Fatalf("dry run ran %v", r.calls)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("dry run wrote the config dir")
	}
	s := out.String()
	for _, want := range []string{
		"Workers Free plan: $0 a month", "no D1", "100,000 requests a day", "2,000 to\n    3,000 a day",
		"dry run: nothing below runs.", "npx --yes " + wranglerPin, "wrangler whoami",
		"package.json, src/fleet.js, src/http.js, src/index.js, wrangler.jsonc", "4. run: wrangler deploy",
		"each token on stdin", filepath.Join(dir, "controller-host-token") + " (mode 0600)",
		`+   "url": "<URL from wrangler deploy>"`, "boxd.json",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("dry run lacks %q:\n%s", want, s)
		}
	}
}

func TestEmbeddedSources(t *testing.T) {
	for _, tc := range []struct {
		name string
		fsys fs.FS
		want string
	}{
		{"modal", modalrunner.FS, "agent_box_runner.py core.py rates.py"},
		{"cloudflare", cfworker.FS, "package.json src/fleet.js src/http.js src/index.js wrangler.jsonc"},
	} {
		if got := strings.Join(embeddedNames(tc.fsys), " "); got != tc.want {
			t.Errorf("%s embeds %q, want %q", tc.name, got, tc.want)
		}
	}
	runner, _ := fs.ReadFile(modalrunner.FS, "agent_box_runner.py")
	if !bytes.Contains(runner, []byte("requires_proxy_auth=True")) || !bytes.Contains(runner, []byte("min_containers=0")) {
		t.Error("embedded runner lacks proxy auth or min_containers=0")
	}
	wcfg, _ := fs.ReadFile(cfworker.FS, "wrangler.jsonc")
	if !bytes.Contains(wcfg, []byte(`"new_sqlite_classes"`)) || bytes.Contains(wcfg, []byte("d1_databases")) || bytes.Contains(wcfg, []byte("kv_namespaces")) || bytes.Contains(wcfg, []byte("r2_buckets")) {
		t.Errorf("wrangler.jsonc isn't the free-plan SQLite-only config:\n%s", wcfg)
	}
	pkg, _ := fs.ReadFile(cfworker.FS, "package.json")
	if !bytes.Contains(pkg, []byte(strings.TrimPrefix(wranglerPin, "wrangler@"))) {
		t.Errorf("wranglerPin %s doesn't match controller/package.json", wranglerPin)
	}
}

func TestLineDiff(t *testing.T) {
	got := lineDiff([]byte("a\nb\nc\n"), []byte("a\nx\nc\nd\n"))
	want := "  a\n+ x\n- b\n  c\n+ d\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if got := lineDiff(nil, []byte("{}\n")); got != "+ {}\n" {
		t.Errorf("from nothing: %q", got)
	}
}
