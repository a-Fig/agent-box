package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	cfworker "github.com/a-Fig/agent-box/controller"
	modalrunner "github.com/a-Fig/agent-box/provision/modal"
)

// wranglerPin is the wrangler controller/package.json pins, used through npx
// when wrangler isn't installed.
const wranglerPin = "wrangler@4.135.0"

const setupUsage = `usage:
  box setup modal --token-in <file|-> [--name modal] [--cap 25] [--max-concurrent 4]
          [--max-timeout 1h] [--mode function|sandbox] [--endpoint URL] [--dry-run]
                                            deploy the Modal cloud runner, set its budget, add it to hosts.json
  box setup cloudflare [--dry-run]          deploy the free fleet controller, write controller.json
--dry-run prints every step and the config changes without running modal or
wrangler, touching the network or writing files. docs/cloud.md explains the costs.
`

var (
	modalURLRe  = regexp.MustCompile(`https://[A-Za-z0-9.-]+\.modal\.run\b`)
	workerURLRe = regexp.MustCompile(`https://[A-Za-z0-9.-]+\.workers\.dev\b`)
)

func runSetup(ctx context.Context, a *App, args []string, stdin io.Reader) int {
	if len(args) == 0 {
		fmt.Fprint(a.Err, setupUsage)
		return 2
	}
	fs := flag.NewFlagSet("box setup "+args[0], flag.ContinueOnError)
	fs.SetOutput(a.Err)
	fs.Usage = func() { fmt.Fprint(a.Err, setupUsage) }
	var err error
	switch args[0] {
	case "modal":
		o := modalSetup{}
		fs.StringVar(&o.Name, "name", "modal", "host name in hosts.json")
		fs.Float64Var(&o.CapUSD, "cap", 25, "monthly budget cap in USD")
		fs.IntVar(&o.MaxConcurrent, "max-concurrent", 4, "most jobs at once")
		fs.DurationVar(&o.MaxTimeout, "max-timeout", time.Hour, "longest allowed run")
		fs.StringVar(&o.Mode, "mode", cloudModeFunction, "function (1x rates) or sandbox (3x, box pull works)")
		fs.StringVar(&o.Endpoint, "endpoint", "", "runner URL, when modal deploy's output doesn't show it")
		fs.StringVar(&o.TokenIn, "token-in", "", "file holding the Modal proxy-auth token, or - for stdin")
		fs.BoolVar(&o.DryRun, "dry-run", false, "print the plan only")
		if fs.Parse(args[1:]) != nil || fs.NArg() != 0 {
			fmt.Fprint(a.Err, setupUsage)
			return 2
		}
		o.Stdin = stdin
		err = a.setupModal(ctx, o)
	case "cloudflare":
		o := cloudflareSetup{}
		fs.BoolVar(&o.DryRun, "dry-run", false, "print the plan only")
		if fs.Parse(args[1:]) != nil || fs.NArg() != 0 {
			fmt.Fprint(a.Err, setupUsage)
			return 2
		}
		err = a.setupCloudflare(ctx, o)
	case "-h", "--help", "help":
		fmt.Fprint(a.Out, setupUsage)
		return 0
	default:
		fmt.Fprintf(a.Err, "box: unknown setup target %q (want modal or cloudflare)\n", args[0])
		return 2
	}
	if err != nil {
		fmt.Fprintln(a.Err, "box:", err)
		return 1
	}
	return 0
}

type modalSetup struct {
	Name          string
	CapUSD        float64
	MaxConcurrent int
	MaxTimeout    time.Duration
	Mode          string
	Endpoint      string
	TokenIn       string
	DryRun        bool
	Stdin         io.Reader
}

const modalCostText = `box setup modal: what this costs
  - Modal's Starter plan includes $30 of free compute credit each month.
  - box caps the runner at $%.2f a month (--cap); at the cap it turns new runs
    away to your other hosts and box status says "cap reached".
  - The runner keeps no warm containers (min_containers=0), so idle time is $0.
  - --mode function (the default) bills Modal's 1x Functions rate; --mode
    sandbox bills about 3x for CPU and memory but lets box pull fetch files.
  - Spending past the free credit needs a card on file with Modal. With a card,
    also set a workspace spend limit in Modal's dashboard: it holds even if
    box's own cap is ever bypassed.
`

func (a *App) setupModal(ctx context.Context, o modalSetup) error {
	o.Name = strings.TrimSpace(o.Name)
	if o.Name == "" {
		return errors.New("--name is empty")
	}
	if o.Mode != cloudModeFunction && o.Mode != cloudModeSandbox {
		return fmt.Errorf("--mode must be %s or %s", cloudModeFunction, cloudModeSandbox)
	}
	if o.CapUSD < 0 {
		return errors.New("--cap must not be negative")
	}
	if o.MaxConcurrent < 1 || o.MaxConcurrent > 100 {
		return errors.New("--max-concurrent must be between 1 and 100")
	}
	if o.MaxTimeout < time.Second || o.MaxTimeout%time.Second != 0 {
		return errors.New("--max-timeout must be a whole number of seconds, at least 1s")
	}
	if o.Endpoint != "" && !validCloudEndpoint(o.Endpoint) {
		return errors.New("--endpoint must be an absolute https:// URL without credentials, query, or fragment")
	}
	if o.TokenIn == "" {
		return errors.New("--token-in is required: a file holding the Modal proxy-auth token, or - for stdin (never a flag value)")
	}
	dir, err := agentBoxConfigDir()
	if err != nil {
		return err
	}
	tokenFile := filepath.Join(dir, o.Name+"-token.json")
	hostsPath := filepath.Join(dir, "hosts.json")
	config := map[string]any{
		"capUSD":        o.CapUSD,
		"maxConcurrent": o.MaxConcurrent,
		"maxTimeoutSec": int(o.MaxTimeout / time.Second),
		"mode":          o.Mode,
	}
	configJSON, _ := plainJSON(config, "")
	out := a.Out
	fmt.Fprintf(out, modalCostText, o.CapUSD)
	fmt.Fprintln(out)

	if o.DryRun {
		endpoint := o.Endpoint
		if endpoint == "" {
			endpoint = "<URL from modal deploy>"
		}
		source := o.TokenIn
		if source == "-" {
			source = "stdin"
		}
		fmt.Fprintln(out, "dry run: nothing below runs.")
		fmt.Fprintln(out, "1. run: modal --version")
		fmt.Fprintf(out, "2. read the proxy-auth token from %s\n", source)
		fmt.Fprintf(out, "3. write the runner (%s) to a temporary directory\n", strings.Join(embeddedNames(modalrunner.FS), ", "))
		fmt.Fprintln(out, "4. run: modal deploy <tmp>/agent_box_runner.py")
		fmt.Fprintf(out, "5. write the token to %s (mode 0600)\n", tokenFile)
		fmt.Fprintf(out, "6. PUT %s/v1/config %s\n", strings.TrimRight(endpoint, "/"), configJSON)
		fmt.Fprintf(out, "7. update %s:\n", hostsPath)
		before, after, err := mergeModalHost(hostsPath, a.Remote, a.Project, o.Name, endpoint, tokenFile, o.Mode)
		if err != nil {
			return err
		}
		fmt.Fprint(out, lineDiff(before, after))
		fmt.Fprintln(out, "8. remind you to set Modal's workspace spend limit")
		return nil
	}

	if r, err := a.R.Run(ctx, Cmd{Name: "modal", Args: []string{"--version"}}); err != nil || r.Code != 0 {
		return errors.New("modal CLI not found: install it (pip install modal), run modal setup, then try again")
	}
	headers, err := readModalToken(o.TokenIn, o.Stdin)
	if err != nil {
		return err
	}
	tmp, err := writeEmbedded(modalrunner.FS, "agent-box-modal-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	fmt.Fprintln(out, "deploying the runner: modal deploy agent_box_runner.py")
	r, err := a.R.Run(ctx, Cmd{Name: "modal", Args: []string{"deploy", filepath.Join(tmp, "agent_box_runner.py")}, Dir: tmp})
	if err != nil {
		return fmt.Errorf("modal deploy: %w", err)
	}
	if r.Code != 0 {
		return fmt.Errorf("modal deploy exited %d:\n%s", r.Code, strings.TrimSpace(r.Stdout+"\n"+r.Stderr))
	}
	endpoint := o.Endpoint
	if endpoint == "" {
		endpoint = pickModalURL(r.Stdout + "\n" + r.Stderr)
		if endpoint == "" {
			return errors.New("modal deploy didn't print the runner's URL; run again with --endpoint https://<workspace>--agent-box-runner-web-app.modal.run")
		}
	}
	fmt.Fprintln(out, "runner:", endpoint)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(cloudTokenFile{Headers: headers}, "", "  ")
	if err := writeSecretFile(tokenFile, append(raw, '\n')); err != nil {
		return err
	}
	fmt.Fprintln(out, "token:", tokenFile, "(0600)")
	ch, err := a.newCloudHost(fleetHost{Name: o.Name, Kind: hostKindModal, Endpoint: endpoint, TokenFile: tokenFile, Mode: o.Mode})
	if err != nil {
		return err
	}
	var applied map[string]any
	if err := ch.doJSON(ctx, http.MethodPut, "/v1/config", config, &applied); err != nil {
		return fmt.Errorf("set the runner's budget (PUT /v1/config): %w", err)
	}
	fmt.Fprintf(out, "budget: $%.2f a month, %d at once, runs up to %s, %s mode\n", o.CapUSD, o.MaxConcurrent, o.MaxTimeout, o.Mode)
	before, after, err := mergeModalHost(hostsPath, a.Remote, a.Project, o.Name, endpoint, tokenFile, o.Mode)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(hostsPath, after, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s:\n%s", hostsPath, lineDiff(before, after))
	if _, err := a.configuredHosts(); err != nil {
		return fmt.Errorf("hosts.json no longer parses: %w", err)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Next: set a workspace spend limit in Modal's dashboard (Settings, Usage and")
	fmt.Fprintln(out, "Billing) so Modal itself stops at your limit. box status shows the runner.")
	return nil
}

type cloudflareSetup struct {
	DryRun bool
}

const cloudflareCostText = `box setup cloudflare: what this costs
  - It runs on Cloudflare's Workers Free plan: $0 a month, no card needed.
  - Storage is one SQLite Durable Object (free on Workers Free); it uses no D1,
    KV or R2.
  - Workers Free allows 100,000 requests a day. Each host sends about 2,000 to
    3,000 a day, so a few hosts fit easily; past the limit Cloudflare refuses
    requests until the next day rather than charging.
`

func (a *App) setupCloudflare(ctx context.Context, o cloudflareSetup) error {
	dir, err := agentBoxConfigDir()
	if err != nil {
		return err
	}
	controllerPath := filepath.Join(dir, "controller.json")
	clientTokenFile := filepath.Join(dir, "controller-client-token")
	hostTokenFile := filepath.Join(dir, "controller-host-token")
	out := a.Out
	fmt.Fprint(out, cloudflareCostText)
	fmt.Fprintln(out)

	if o.DryRun {
		fmt.Fprintln(out, "dry run: nothing below runs.")
		fmt.Fprintln(out, "1. run: wrangler --version (or npx --yes "+wranglerPin+" when wrangler isn't installed)")
		fmt.Fprintln(out, "2. run: wrangler whoami (wrangler login first if it says you're not signed in)")
		fmt.Fprintf(out, "3. write the worker (%s) to a temporary directory\n", strings.Join(embeddedNames(cfworker.FS), ", "))
		fmt.Fprintln(out, "4. run: wrangler deploy")
		fmt.Fprintln(out, "5. make HOST_TOKEN and CLIENT_TOKEN (32 random bytes each)")
		fmt.Fprintln(out, "6. run: wrangler secret put HOST_TOKEN, then CLIENT_TOKEN (each token on stdin)")
		fmt.Fprintf(out, "7. write %s and %s (mode 0600)\n", clientTokenFile, hostTokenFile)
		fmt.Fprintf(out, "8. update %s:\n", controllerPath)
		before, after, err := mergeControllerConfig(controllerPath, "<URL from wrangler deploy>", clientTokenFile)
		if err != nil {
			return err
		}
		fmt.Fprint(out, lineDiff(before, after))
		fmt.Fprintln(out, "9. print the boxd.json snippet for each host")
		return nil
	}

	wrangler := []string{"wrangler"}
	if _, err := a.R.Run(ctx, Cmd{Name: "wrangler", Args: []string{"--version"}}); err != nil {
		wrangler = []string{"npx", "--yes", wranglerPin}
	}
	wr := func(dir string, stdin io.Reader, args ...string) (Result, error) {
		return a.R.Run(ctx, Cmd{Name: wrangler[0], Args: append(append([]string{}, wrangler[1:]...), args...), Dir: dir, Stdin: stdin})
	}
	if r, err := wr("", nil, "whoami"); err != nil || r.Code != 0 || strings.Contains(strings.ToLower(r.Stdout+r.Stderr), "not authenticated") {
		return errors.New("wrangler isn't signed in to Cloudflare: run wrangler login (or npx --yes " + wranglerPin + " login), then try again")
	}
	tmp, err := writeEmbedded(cfworker.FS, "agent-box-controller-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	fmt.Fprintln(out, "deploying the controller: wrangler deploy")
	r, err := wr(tmp, nil, "deploy")
	if err != nil {
		return fmt.Errorf("wrangler deploy: %w", err)
	}
	if r.Code != 0 {
		return fmt.Errorf("wrangler deploy exited %d:\n%s", r.Code, strings.TrimSpace(r.Stdout+"\n"+r.Stderr))
	}
	url := workerURLRe.FindString(r.Stdout + "\n" + r.Stderr)
	if url == "" {
		return errors.New("wrangler deploy didn't print a workers.dev URL; check the worker's route in Cloudflare's dashboard")
	}
	fmt.Fprintln(out, "controller:", url)
	tokens := map[string]string{}
	for _, name := range []string{"HOST_TOKEN", "CLIENT_TOKEN"} {
		tok, err := randomToken()
		if err != nil {
			return err
		}
		tokens[name] = tok
		r, err := wr(tmp, strings.NewReader(tok+"\n"), "secret", "put", name)
		if err != nil {
			return fmt.Errorf("wrangler secret put %s: %w", name, err)
		}
		if r.Code != 0 {
			return fmt.Errorf("wrangler secret put %s exited %d:\n%s", name, r.Code, strings.TrimSpace(r.Stdout+"\n"+r.Stderr))
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeSecretFile(clientTokenFile, []byte(tokens["CLIENT_TOKEN"]+"\n")); err != nil {
		return err
	}
	if err := writeSecretFile(hostTokenFile, []byte(tokens["HOST_TOKEN"]+"\n")); err != nil {
		return err
	}
	before, after, err := mergeControllerConfig(controllerPath, url, clientTokenFile)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(controllerPath, after, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s:\n%s", controllerPath, lineDiff(before, after))
	snippet, _ := plainJSON(map[string]any{"controller": map[string]string{
		"url": url, "token_file": "/etc/agent-box/controller-host-token", "id": "<host-id>",
	}}, "  ")
	fmt.Fprintf(out, `
Each host publishes its state with the host token, which is in
  %s (0600; it is never printed).
On each host (the PC's agents distro):
  1. Copy that file to /etc/agent-box/controller-host-token, readable only by
     boxd's user (chmod 600).
  2. Add this to /etc/agent-box/boxd.json, then restart boxd. For the id, use
     the host's name in hosts.json (pc without hosts.json); a-z, 0-9 and -,
     up to 40 characters. If they differ, set "controller_id" on that host.
%s
Running box setup cloudflare again makes new tokens; copy the host token again.
`, hostTokenFile, indent(strings.TrimSpace(string(snippet)), "     "))
	return nil
}

// agentBoxConfigDir is agent-box's folder in the user's config directory,
// where hosts.json and controller.json live.
func agentBoxConfigDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find the config directory: %w", err)
	}
	return filepath.Join(dir, "agent-box"), nil
}

// readModalToken reads proxy-auth headers from a file or stdin ("-"). It
// takes the token file's own {"headers":{...}} form, a flat JSON object,
// "Name: value" lines, or Modal's two bare lines (token ID, then secret).
func readModalToken(src string, stdin io.Reader) (map[string]string, error) {
	var raw []byte
	var err error
	if src == "-" {
		if stdin == nil {
			return nil, errors.New("--token-in -: no stdin")
		}
		raw, err = io.ReadAll(io.LimitReader(stdin, 64<<10))
	} else {
		raw, err = os.ReadFile(src)
	}
	if err != nil {
		return nil, fmt.Errorf("read the token: %w", err)
	}
	headers, err := parseModalToken(raw)
	if err != nil {
		return nil, fmt.Errorf("read the token from %s: %w", src, err)
	}
	return headers, nil
}

func parseModalToken(raw []byte) (map[string]string, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil, errors.New("it is empty")
	}
	headers := map[string]string{}
	if strings.HasPrefix(text, "{") {
		var wrapped cloudTokenFile
		if json.Unmarshal([]byte(text), &wrapped) == nil && len(wrapped.Headers) > 0 {
			headers = wrapped.Headers
		} else if err := json.Unmarshal([]byte(text), &headers); err != nil {
			return nil, errors.New(`want {"headers":{"Modal-Key":"...","Modal-Secret":"..."}}, "Name: value" lines, or the token ID and secret on two lines`)
		}
	} else {
		var bare []string
		sc := bufio.NewScanner(strings.NewReader(text))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			if k, v, ok := strings.Cut(line, ":"); ok && !strings.Contains(k, " ") {
				headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
			} else {
				bare = append(bare, line)
			}
		}
		switch {
		case len(bare) == 2 && len(headers) == 0:
			headers["Modal-Key"], headers["Modal-Secret"] = bare[0], bare[1]
		case len(bare) != 0:
			return nil, errors.New(`want "Name: value" lines, or the token ID and secret on two lines`)
		}
	}
	for k, v := range headers {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return nil, errors.New("a header name or value is empty")
		}
	}
	if len(headers) == 0 {
		return nil, errors.New("it holds no headers")
	}
	return headers, nil
}

// pickModalURL finds the runner's web endpoint in modal deploy's output,
// preferring the web_app function's URL.
func pickModalURL(output string) string {
	urls := modalURLRe.FindAllString(output, -1)
	for _, u := range urls {
		if strings.Contains(u, "web-app") {
			return u
		}
	}
	if len(urls) > 0 {
		return urls[0]
	}
	return ""
}

// mergeModalHost returns hosts.json before and after adding or updating the
// named modal host. Other hosts, their fields, and other top-level keys stay
// as they were; an existing entry keeps its fields (priority included) and
// gets the new endpoint, token file and mode. With no hosts yet, box used
// the default remote alone, so that host goes in first and keeps working.
func mergeModalHost(hostsPath, remote, project, name, endpoint, tokenFile, mode string) (before, after []byte, err error) {
	before, err = os.ReadFile(hostsPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	top := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(before)) > 0 {
		if err := json.Unmarshal(before, &top); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", hostsPath, err)
		}
	}
	var hosts []map[string]json.RawMessage
	if raw, ok := top["hosts"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &hosts); err != nil {
			return nil, nil, fmt.Errorf("parse %s hosts: %w", hostsPath, err)
		}
	}
	set := func(h map[string]json.RawMessage, k string, v any) {
		h[k], _ = plainJSON(v, "")
	}
	if len(hosts) == 0 && remote != "" {
		if remote == name {
			return nil, nil, fmt.Errorf("--name %q is the default host's name; pick another", name)
		}
		h := map[string]json.RawMessage{}
		set(h, "name", remote)
		set(h, "remote", remote)
		set(h, "project", project)
		hosts = append(hosts, h)
	}
	found := false
	for _, h := range hosts {
		var n, kind string
		_ = json.Unmarshal(h["name"], &n)
		_ = json.Unmarshal(h["kind"], &kind)
		if strings.TrimSpace(n) != name {
			continue
		}
		if kind != hostKindModal {
			return nil, nil, fmt.Errorf("%s already has a host named %q that isn't a modal host; pick another --name", hostsPath, name)
		}
		set(h, "endpoint", endpoint)
		set(h, "token_file", tokenFile)
		set(h, "mode", mode)
		if _, ok := h["priority"]; !ok {
			set(h, "priority", -1)
		}
		found = true
	}
	if !found {
		h := map[string]json.RawMessage{}
		set(h, "name", name)
		set(h, "kind", hostKindModal)
		set(h, "endpoint", endpoint)
		set(h, "token_file", tokenFile)
		set(h, "mode", mode)
		set(h, "priority", -1)
		hosts = append(hosts, h)
	}
	if top["hosts"], err = plainJSON(hosts, ""); err != nil {
		return nil, nil, err
	}
	after, err = plainJSON(top, "  ")
	return before, after, err
}

// mergeControllerConfig returns controller.json before and after setting its
// url and token_file, keeping any other keys.
func mergeControllerConfig(p, url, tokenFile string) (before, after []byte, err error) {
	before, err = os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	top := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(before)) > 0 {
		if err := json.Unmarshal(before, &top); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", p, err)
		}
	}
	top["url"], _ = plainJSON(url, "")
	top["token_file"], _ = plainJSON(tokenFile, "")
	after, err = plainJSON(top, "  ")
	return before, after, err
}

// plainJSON marshals v without escaping <, > and &, so placeholders read as
// typed. With an indent it ends in a newline, as a config file does.
func plainJSON(v any, indent string) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", indent)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if indent == "" {
		return bytes.TrimRight(b.Bytes(), "\n"), nil
	}
	return b.Bytes(), nil
}

// lineDiff is a small line diff for config files: unchanged lines start
// with two spaces, removed ones with "- ", added ones with "+ ".
func lineDiff(before, after []byte) string {
	split := func(b []byte) []string {
		s := strings.TrimRight(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
		if s == "" {
			return nil
		}
		return strings.Split(s, "\n")
	}
	x, y := split(before), split(after)
	lcs := make([][]int, len(x)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var b strings.Builder
	i, j := 0, 0
	for i < len(x) || j < len(y) {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			b.WriteString("  " + x[i] + "\n")
			i, j = i+1, j+1
		case j < len(y) && (i == len(x) || lcs[i][j+1] >= lcs[i+1][j]):
			b.WriteString("+ " + y[j] + "\n")
			j++
		default:
			b.WriteString("- " + x[i] + "\n")
			i++
		}
	}
	return b.String()
}

// writeEmbedded copies an embedded tree into a new temporary directory.
func writeEmbedded(src fs.FS, prefix string) (string, error) {
	tmp, err := os.MkdirTemp("", prefix)
	if err != nil {
		return "", err
	}
	err = fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return err
		}
		dst := filepath.Join(tmp, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		data, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o600)
	})
	if err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	return tmp, nil
}

func embeddedNames(src fs.FS) []string {
	var names []string
	_ = fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, path.Clean(p))
		}
		return nil
	})
	sort.Strings(names)
	return names
}

// writeSecretFile writes data readable only by this user, replacing any
// file that was there (and tightening its mode).
func writeSecretFile(p string, data []byte) error {
	return writeFileAtomic(p, data, 0o600)
}

// writeFileAtomic writes data beside p and renames it into place, so a
// failed write never leaves half a config.
func writeFileAtomic(p string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".tmp-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
