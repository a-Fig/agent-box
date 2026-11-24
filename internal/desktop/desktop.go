// Package desktop is `box app`: the dashboard boxd serves on the PC, in a window of its own on the
// laptop, as `mur app` does for Murmur's page. The window adds what a browser tab can't: its own
// icon and title, a title bar that follows the page's theme, and links to other sites that open in
// the browser rather than in the window.
//
// The window is the system's web view (WebView2 on Windows, WebKitGTK on Linux) through glaze,
// which loads it at run time with purego. There is no cgo, so box still cross-compiles, and a
// machine without a web view runs every other command. docs/DESIGN.md has why.
//
// When the PC doesn't answer, the window shows the page's "PC unreachable" screen from the copy
// of internal/dash/static built into box, served on loopback. Its calls to /api/state find out
// whether the PC answers yet, and the page's own retry, every 10s, keeps asking; once the PC
// answers, the window goes to it.
package desktop

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"sync"
	"time"

	"github.com/a-Fig/agent-box/internal/dash/static"
)

// Title is the window's title.
const Title = "agent-box"

// Options is what Run needs from the command.
type Options struct {
	// URL is the dashboard's address, https://<host>/ of the pc remote, or the devserver's.
	URL string
	// Open opens a link in the person's browser.
	Open func(url string) error
	// Debug turns on the web view's developer tools.
	Debug bool
}

// probeTimeout bounds one check of whether the PC answers. The page's own fetch gives up after
// 15s, so a probe answers well inside it.
const probeTimeout = 8 * time.Second

// probe says whether the dashboard at page answers: any answer below 500 is boxd's. Tailscale
// serve answers 502 while boxd is down, which counts as not answering.
func probe(ctx context.Context, c *http.Client, page string) bool {
	u, err := url.JoinPath(page, "api/state")
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/json")
	res, err := c.Do(req)
	if err != nil {
		return false
	}
	res.Body.Close()
	return res.StatusCode < 500
}

// holdFor is how long the offline page's /api/state waits for the window to leave once the PC
// answers, so the page doesn't flash "PC unreachable" on its way out.
const holdFor = 10 * time.Second

// offlineHandler serves the page's embedded copy to the window, at host. Every /api call fails as
// an unreachable PC does, with 502, so the page shows that screen; /api/state first asks whether
// the PC answers now (answers), and if it does, calls up and holds the request until the window
// has moved on. A request for any other host (another site's page that points its own name at
// 127.0.0.1) is refused.
func offlineHandler(host string, answers func(context.Context) bool, up func()) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/state" && answers(r.Context()) {
			up()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(holdFor):
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"code":"unreachable","error":"The PC didn't answer."}` + "\n"))
	})
	mux.Handle("/", static.Handler(static.FS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != host {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// serveOffline serves offlineHandler on a loopback port, and returns its address and how to stop
// it.
func serveOffline(page string, up func()) (addr string, stop func(), err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	c := &http.Client{}
	host := ln.Addr().String()
	srv := &http.Server{
		Handler:           offlineHandler(host, func(ctx context.Context) bool { return probe(ctx, c, page) }, up),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go srv.Serve(ln)
	return "http://" + host + "/", func() { srv.Close() }, nil
}

// How the window keeps to the dashboard or the offline page. Every pingEvery the window asks the
// document it shows to say where it is (keeper.here). A document that doesn't answer within
// answerWithin (a load that failed: an error page has no bindings), or a trip to the dashboard
// that hasn't arrived within arriveWithin, sends the window back to the offline page, whose own
// retry then finds the PC again.
const (
	pingEvery    = 10 * time.Second
	answerWithin = 25 * time.Second
	arriveWithin = 20 * time.Second
)

// keeper decides where the window goes. Its methods are safe from any goroutine; navigate must
// hand the address to the UI thread.
type keeper struct {
	dash, offline string // addresses
	navigate      func(string)
	now           func() time.Time

	mu      sync.Mutex
	pending time.Time // when the window was sent to the dashboard and hasn't said it got there
	seen    time.Time // when a document last answered, or the window last moved
}

func newKeeper(dash, offline string, navigate func(string)) *keeper {
	k := &keeper{dash: dash, offline: offline, navigate: navigate, now: time.Now}
	k.seen = k.now()
	return k
}

// up is the offline page's news that the PC answers. A trip already on its way isn't started
// again.
func (k *keeper) up() {
	k.mu.Lock()
	now := k.now()
	if !k.pending.IsZero() && now.Sub(k.pending) < arriveWithin {
		k.mu.Unlock()
		return
	}
	k.pending, k.seen = now, now
	k.mu.Unlock()
	k.navigate(k.dash)
}

// here is a document's answer: its origin, and whether it is the page as it should be (not a 5xx,
// not some other page at that address). Anything but the dashboard or the offline page sends the
// window back to the offline page.
func (k *keeper) here(origin string, ok bool) {
	k.mu.Lock()
	k.seen = k.now()
	switch {
	case ok && origin == originOf(k.dash):
		k.pending = time.Time{}
		k.mu.Unlock()
		return
	case ok && origin == originOf(k.offline):
		k.mu.Unlock()
		return
	}
	k.mu.Unlock()
	k.back()
}

// tick runs every pingEvery, after the window has asked its document to answer.
func (k *keeper) tick() {
	k.mu.Lock()
	now := k.now()
	stale := now.Sub(k.seen) > answerWithin
	late := !k.pending.IsZero() && now.Sub(k.pending) > arriveWithin
	k.mu.Unlock()
	if stale || late {
		k.back()
	}
}

func (k *keeper) back() {
	k.mu.Lock()
	k.pending, k.seen = time.Time{}, k.now()
	k.mu.Unlock()
	k.navigate(k.offline)
}

// originOf is scheme://host of an address, as a page's location.origin says it.
func originOf(addr string) string {
	u, err := url.Parse(addr)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// outside says whether link is an http or https address the window should hand to the browser.
func outside(link string) (string, bool) {
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	return u.String(), true
}

// waited is the end of cmd's process, once it comes.
func waited(cmd *exec.Cmd) <-chan error {
	c := make(chan error, 1)
	go func() { c <- cmd.Wait() }()
	return c
}
