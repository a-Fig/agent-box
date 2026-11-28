package main

// The tray's Open dashboard item opens the page boxd serves, in the same window as the laptop's
// `box app` (internal/desktop), started as `boxtray.exe --dashboard --url <url>`. The address is
// boxd's Tailscale serve name, asked of Tailscale inside the agents distro: the repo is public, so
// no name is written here. boxtray.json's dashboard_url, when set, wins over the lookup.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// checkDashURL accepts an http or https address with a host, and nothing that could carry more
// than one argument or a control character to the window's process.
func checkDashURL(s string) (string, error) {
	if s == "" {
		return "", errors.New("empty address")
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", fmt.Errorf("%q: spaces or control characters", s)
		}
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", fmt.Errorf("%q: want an http or https address", s)
	}
	return u.String(), nil
}

// boxdPort is where boxd listens inside the distro; setup-host.sh points Tailscale serve's 443 at it.
const boxdPort = "7766"

// parseServeStatus finds the dashboard in `tailscale serve status --json`: the name whose 443
// handler proxies to boxd, or else the first name served on 443.
func parseServeStatus(b []byte) (string, error) {
	var st struct {
		Web map[string]struct {
			Handlers map[string]struct {
				Proxy string `json:"Proxy"`
			} `json:"Handlers"`
		} `json:"Web"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return "", fmt.Errorf("tailscale serve status: %v", err)
	}
	keys := make([]string, 0, len(st.Web))
	for k := range st.Web {
		keys = append(keys, k)
	}
	sort.Strings(keys) // a map's order would make "the first" differ from run to run
	first := ""
	for _, k := range keys {
		host, ok := strings.CutSuffix(k, ":443")
		if !ok || host == "" {
			continue
		}
		for _, h := range st.Web[k].Handlers {
			if strings.HasSuffix(strings.TrimSuffix(h.Proxy, "/"), ":"+boxdPort) {
				return checkDashURL("https://" + host + "/")
			}
		}
		if first == "" {
			first = host
		}
	}
	if first == "" {
		return "", errors.New("tailscale serve status: nothing served on 443")
	}
	return checkDashURL("https://" + first + "/")
}

// parseTailscaleStatus is the distro's own MagicDNS name in `tailscale status --json`, which is
// the name Tailscale serve answers on.
func parseTailscaleStatus(b []byte) (string, error) {
	var st struct {
		Self struct {
			DNSName string `json:"DNSName"`
		} `json:"Self"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return "", fmt.Errorf("tailscale status: %v", err)
	}
	name := strings.TrimSuffix(st.Self.DNSName, ".")
	if name == "" {
		return "", errors.New("tailscale status: no DNS name")
	}
	return checkDashURL("https://" + name + "/")
}

// dashResolver finds the dashboard's address. It keeps the last one it looked up, so a click while
// WSL is stopped (after Release memory) still opens the window, whose "PC unreachable" screen
// waits for boxd to come back.
type dashResolver struct {
	explicit func() string                                             // boxtray.json's dashboard_url
	up       func(context.Context) bool                                // the distro runs; a lookup would otherwise start it
	run      func(ctx context.Context, args ...string) ([]byte, error) // a command as root in the distro

	mu     sync.Mutex
	cached string
}

// resolve is the address to open: dashboard_url if set, else a lookup in the distro, else the
// last address looked up.
func (d *dashResolver) resolve(ctx context.Context) (string, error) {
	if s := d.explicit(); s != "" {
		u, err := checkDashURL(s)
		if err != nil {
			return "", fmt.Errorf("boxtray.json dashboard_url %v", err)
		}
		return u, nil
	}
	var lookErr error
	if d.up(ctx) {
		u, err := d.refresh(ctx)
		if err == nil {
			return u, nil
		}
		lookErr = err
	} else {
		lookErr = errors.New("WSL isn't running")
	}
	d.mu.Lock()
	u := d.cached
	d.mu.Unlock()
	if u != "" {
		return u, nil
	}
	return "", fmt.Errorf("no dashboard address (%v); boxtray.json's dashboard_url sets one", lookErr)
}

// refresh looks the address up and keeps it.
func (d *dashResolver) refresh(ctx context.Context) (string, error) {
	u, err := d.lookup(ctx)
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	d.cached = u
	d.mu.Unlock()
	return u, nil
}

// lookup asks Tailscale in the distro: the serve config first, the node's own name second.
func (d *dashResolver) lookup(ctx context.Context) (string, error) {
	out, err := d.run(ctx, "tailscale", "serve", "status", "--json")
	if err == nil {
		u, perr := parseServeStatus(out)
		if perr == nil {
			return u, nil
		}
		err = perr
	}
	out, err2 := d.run(ctx, "tailscale", "status", "--json")
	if err2 != nil {
		return "", errors.Join(err, err2)
	}
	u, err2 := parseTailscaleStatus(out)
	if err2 != nil {
		return "", errors.Join(err, err2)
	}
	return u, nil
}

// needsWarm says whether a lookup ahead of a click would help: no dashboard_url, and no address
// looked up yet.
func (d *dashResolver) needsWarm() bool {
	if d.explicit() != "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cached == ""
}

// parseDashboardArgs reads `--dashboard --url <url>`. ok is false when args don't ask for the
// window at all.
func parseDashboardArgs(args []string) (page string, ok bool, err error) {
	if len(args) == 0 || args[0] != "--dashboard" {
		return "", false, nil
	}
	if len(args) != 3 || args[1] != "--url" {
		return "", true, errors.New("usage: boxtray --dashboard --url <url>")
	}
	u, err := checkDashURL(args[2])
	return u, true, err
}
