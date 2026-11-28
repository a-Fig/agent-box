package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/a-Fig/agent-box/internal/controller"
	"github.com/a-Fig/agent-box/internal/dash"
)

const controllerTimeout = 3 * time.Second

type controllerConfig struct {
	URL       string `json:"url"`
	TokenFile string `json:"token_file"`
}

// configuredController reads agent-box/controller.json in the user's config
// directory. Missing config or an empty URL returns nil, "", nil so controller
// integration remains strictly opt-in.
func configuredController() (*controllerConfig, string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, "", nil
	}
	path := filepath.Join(dir, "agent-box", "controller.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}

	var cfg controllerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, "", fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.URL = strings.TrimSpace(cfg.URL)
	cfg.TokenFile = strings.TrimSpace(cfg.TokenFile)
	if cfg.URL == "" {
		return nil, "", nil
	}
	if !controller.ValidControllerURL(cfg.URL) {
		return nil, "", fmt.Errorf("%s: url must be https:// or loopback http://", path)
	}
	if cfg.TokenFile == "" {
		return nil, "", fmt.Errorf("%s: token_file is required", path)
	}
	rawTok, err := os.ReadFile(cfg.TokenFile)
	if err != nil {
		return nil, "", fmt.Errorf("%s: read token_file %s: %w", path, cfg.TokenFile, err)
	}
	token := strings.TrimSpace(string(rawTok))
	if token == "" {
		return nil, "", fmt.Errorf("%s: token_file %s is empty", path, cfg.TokenFile)
	}
	return &cfg, token, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func (a *App) controllerClient() *controller.Client {
	cfg, token, err := configuredController()
	if err != nil {
		if a.Warn != nil || a.Err != nil {
			a.warn(fmt.Sprintf("box: controller config: %v", err))
		}
		return nil
	}
	if cfg == nil {
		return nil
	}
	hc := &http.Client{Timeout: controllerTimeout}
	if a.HTTP != nil {
		hc.Transport = roundTripFunc(a.HTTP)
	}
	return &controller.Client{
		URL:   cfg.URL,
		Token: token,
		HTTP:  hc,
	}
}

func (a *App) enrichControllerStatus(ctx context.Context, hosts []fleetHost, rows []fleetHostStatus, current *StatusReport, selected int, listOnly bool) {
	client := a.controllerClient()
	if client == nil {
		return
	}

	anyUnreachable := !listOnly && current != nil && !current.Reachable
	for _, row := range rows {
		if !row.Reachable {
			anyUnreachable = true
			break
		}
	}
	if !anyUnreachable {
		return
	}

	cctx, cancel := context.WithTimeout(ctx, controllerTimeout)
	defer cancel()

	var hostsResp controller.HostsResponse
	var hostsErr error
	var usageResp controller.UsageResponse
	var usageErr error

	var wg sync.WaitGroup
	wg.Go(func() {
		hostsResp, hostsErr = client.Hosts(cctx)
	})
	if !listOnly && current != nil {
		wg.Go(func() {
			usageResp, usageErr = client.Usage(cctx)
		})
	}
	wg.Wait()

	if !listOnly && current != nil && usageErr == nil {
		u := usageResp
		current.ControllerUsage = &u
	}

	if !anyUnreachable || hostsErr != nil {
		return
	}

	now := time.Now().UTC()
	if a.Now != nil {
		now = a.Now().UTC()
	}
	if !hostsResp.Now.IsZero() {
		now = hostsResp.Now.UTC()
	}
	for i := range rows {
		if rows[i].Reachable || i >= len(hosts) {
			continue
		}
		if rec, ok := matchControllerHost(hostsResp.Hosts, hosts[i].Name, hosts[i].ControllerID); ok {
			seen := rec.SeenAt.UTC()
			st := rec.State
			rows[i].LastSeen = &seen
			rows[i].LastState = &st
			rows[i].LastStateStale = rec.Stale
			rows[i].now = now
		}
	}
	if !listOnly && current != nil && !current.Reachable {
		hostName, controllerID := a.Remote, ""
		if selected >= 0 && selected < len(hosts) {
			hostName = hosts[selected].Name
			controllerID = hosts[selected].ControllerID
		}
		if rec, ok := matchControllerHost(hostsResp.Hosts, hostName, controllerID); ok {
			seen := rec.SeenAt.UTC()
			st := rec.State
			current.LastSeen = &seen
			current.LastState = &st
			current.LastStateStale = rec.Stale
			current.now = now
		}
	}
}

func matchControllerHost(records []controller.HostRecord, hostName, controllerID string) (controller.HostRecord, bool) {
	if controllerID != "" {
		for _, rec := range records {
			if rec.ID == controllerID {
				return rec, true
			}
		}
		return controller.HostRecord{}, false
	}
	for _, rec := range records {
		if rec.ID == hostName {
			return rec, true
		}
	}
	sanitized := controller.SanitizeHostID(hostName)
	for _, rec := range records {
		if rec.ID == sanitized || rec.Name == hostName {
			return rec, true
		}
	}
	return controller.HostRecord{}, false
}

func formatControllerCachedSummary(seen time.Time, stale bool, st dash.State, now time.Time) string {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	ago := formatControllerAgo(seen, now)
	staleTag := ""
	if stale {
		staleTag = " (stale)"
	}

	modeLabel := "unknown mode"
	if len(st.Machines) > 0 {
		switch st.Machines[0].Mode {
		case dash.ModeShared:
			modeLabel = "Shared"
		case dash.ModeFull:
			modeLabel = "Full"
		case dash.ModeMyPC:
			modeLabel = "My PC"
		}
	}

	boxWord := "boxes"
	if len(st.Boxes) == 1 {
		boxWord = "box"
	}

	running := 0
	queued := 0
	for _, j := range st.Jobs {
		switch j.State {
		case dash.JobRunning:
			running++
		case dash.JobQueued:
			queued++
		}
	}

	summary := fmt.Sprintf("%s · %d %s · %d running", modeLabel, len(st.Boxes), boxWord, running)
	if queued > 0 {
		summary += fmt.Sprintf(" · %d queued", queued)
	}
	return fmt.Sprintf("unreachable, last seen %s%s: %s", ago, staleTag, summary)
}

func formatControllerAgo(seen, now time.Time) string {
	d := now.Sub(seen)
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func formatControllerUsage(u controller.UsageResponse) string {
	if u.Limit > 0 {
		return fmt.Sprintf("controller: %s of %s requests today", formatIntCommas(u.Requests), formatIntCommas(u.Limit))
	}
	return fmt.Sprintf("controller: %s requests today", formatIntCommas(u.Requests))
}

func formatIntCommas(n int) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var out []byte
	rem := len(s) % 3
	if rem > 0 {
		out = append(out, s[:rem]...)
	}
	for i := rem; i < len(s); i += 3 {
		if len(out) > 0 {
			out = append(out, ',')
		}
		out = append(out, s[i:i+3]...)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}
