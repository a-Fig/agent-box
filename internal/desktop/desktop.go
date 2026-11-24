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

// offlineHandler serves the page's embedded copy. Every /api call fails as an unreachable PC
// does, with 502, so the page shows that screen; /api/state first asks whether the PC answers
// now (answers), and if it does, calls up and holds the request until the window has moved on.
func offlineHandler(answers func(context.Context) bool, up func()) http.Handler {
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
	return mux
}

// serveOffline serves offlineHandler on a loopback port, and returns its address and how to stop
// it.
func serveOffline(page string, up func()) (addr string, stop func(), err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	c := &http.Client{}
	srv := &http.Server{
		Handler:           offlineHandler(func(ctx context.Context) bool { return probe(ctx, c, page) }, up),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go srv.Serve(ln)
	return "http://" + ln.Addr().String() + "/", func() { srv.Close() }, nil
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
