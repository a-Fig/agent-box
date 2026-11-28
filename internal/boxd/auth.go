// Package boxd is the dashboard server that runs in the agents distro on the
// PC (docs/ui.md). It listens on 127.0.0.1 only; `tailscale serve` puts it on
// the tailnet with HTTPS and says who is asking.
package boxd

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/a-Fig/agent-box/internal/controller"
	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/dash/static"
)

// ControllerConfig enables optional state publishing to a fleet controller
// (docs/controller-protocol.md). Nil or empty URL disables publishing.
type ControllerConfig struct {
	URL       string `json:"url"`
	TokenFile string `json:"token_file"`
	ID        string `json:"id"`
}

// Config is /etc/agent-box/boxd.json, written by provision/setup-host.sh.
type Config struct {
	// Owners are the tailnet logins (Tailscale-User-Login) allowed in. Empty
	// lets nobody in: the check fails closed.
	Owners []string `json:"owners"`
	// Listen is the loopback address `tailscale serve` proxies to.
	Listen string `json:"listen"`
	// HistoryDir holds the JSON-lines history.
	HistoryDir string `json:"historyDir"`
	// QueueDir holds the queued jobs and their snapshots.
	QueueDir string `json:"queueDir"`
	// MachineID and MachineName are this PC on the page: "pc" and "PC"
	// unless set.
	MachineID   string `json:"machineId"`
	MachineName string `json:"machineName"`
	// ReuseThrowaways enables pooling after all clients on this host are upgraded.
	ReuseThrowaways bool `json:"reuseThrowaways"`
	// CoolIdleThrowaways is enabled after clients learn reserved eviction.
	CoolIdleThrowaways bool `json:"coolIdleThrowaways"`
	// Controller optionally publishes trimmed host state to a fleet controller.
	Controller *ControllerConfig `json:"controller,omitempty"`
}

// DefaultListen is the port docs/ui.md picked, so nothing else on the
// distro takes it.
const DefaultListen = "127.0.0.1:7766"

// LoadConfig reads the config file and fills in defaults.
func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.HistoryDir == "" {
		c.HistoryDir = "/var/lib/agent-box/history"
	}
	if c.QueueDir == "" {
		c.QueueDir = "/var/lib/agent-box/queue"
	}
	if c.MachineID == "" {
		c.MachineID = "pc"
	}
	if c.MachineName == "" {
		c.MachineName = "PC"
	}
	if c.Controller != nil {
		c.Controller.URL = strings.TrimSpace(c.Controller.URL)
		c.Controller.TokenFile = strings.TrimSpace(c.Controller.TokenFile)
		c.Controller.ID = strings.TrimSpace(c.Controller.ID)
		if c.Controller.URL == "" && c.Controller.TokenFile == "" && c.Controller.ID == "" {
			c.Controller = nil
		} else if c.Controller.URL != "" && c.Controller.ID == "" {
			c.Controller.ID = controller.SanitizeHostID(c.MachineID)
		}
	}
	return c, nil
}

// LoginHeader is the header `tailscale serve` sets to the caller's tailnet
// login. It strips any copy the client sent; tagged devices and Funnel
// traffic get none.
const LoginHeader = "Tailscale-User-Login"

// Owners lets a request through only when it came over loopback (from
// `tailscale serve`) with a login on the owner list. Everything else gets 403
// with code forbidden.
func Owners(owners []string, next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range owners {
		if o = strings.TrimSpace(o); o != "" {
			allowed[strings.ToLower(o)] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !fromLoopback(r.RemoteAddr) {
			writeError(w, http.StatusForbidden, dash.CodeForbidden, "boxd answers only through tailscale serve")
			return
		}
		login := strings.ToLower(strings.TrimSpace(r.Header.Get(LoginHeader)))
		if login == "" || !allowed[login] {
			writeError(w, http.StatusForbidden, dash.CodeForbidden, "this tailnet login may not use the dashboard")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func fromLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, dash.ErrorBody{Error: msg, Code: code})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// SecurityHeaders sets the page's strict CSP on every answer, the API's too
// (agents' text is shown, never run), and keeps the page out of frames.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", static.CSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
