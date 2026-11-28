// Command devserver serves the dashboard's page and a fake API from one of the fixture scenarios,
// so the page is built without boxd or a PC, as Murmur's page is built against its devserver:
//
//	go run ./internal/dash/devserver -scenario busy
//
// It plays the scenario forward on a timer: running jobs write to their logs and finish, new boxes
// and jobs appear, and the tray applies mode requests, so the stream has something to show. Its controls
// answer as boxd's will, with the same status codes and errors. -unreachable starts it with the
// API down, for the "PC unreachable" screen; GET /dev/down and /dev/up switch that while it runs.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/dash/fixture"
	"github.com/a-Fig/agent-box/internal/dash/static"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7767", "loopback address to listen on")
	scenario := flag.String("scenario", "busy", "the fixture to start from: "+strings.Join(fixture.Names(), ", "))
	tick := flag.Duration("tick", 2*time.Second, "how often the scenario moves on")
	still := flag.Bool("still", false, "don't play the scenario forward (for screenshots)")
	down := flag.Bool("unreachable", false, "start with the API down, as when the PC is asleep")
	fail := flag.Bool("fail", false, "every control fails with 502, as when Incus does")
	dir := flag.String("static", "", "serve the page from this folder rather than the embedded copy, so an edit shows on reload (internal/dash/static)")
	flag.Parse()
	if *dir != "" {
		pageFS = os.DirFS(*dir)
	}
	host, _, err := net.SplitHostPort(*addr)
	if err != nil || (host != "127.0.0.1" && host != "localhost") {
		log.Fatal("-addr must be on 127.0.0.1, like 127.0.0.1:7767")
	}
	sc, err := fixture.Build(*scenario, time.Now())
	if err != nil {
		log.Fatal(err)
	}
	s := newServer(sc, time.Now)
	s.down, s.fail = *down, *fail
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, "devserver: %s on http://%s/\n", *scenario, ln.Addr())
	if *still {
		go s.heartbeat()
	} else {
		go s.play(*tick)
	}
	log.Fatal(http.Serve(ln, s.routes()))
}

// pageFS is where the page's files come from: the embedded copy unless -static says otherwise.
var pageFS fs.FS = static.FS

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	page := static.Handler(pageFS)
	mux.Handle("/", page)
	api := func(pattern string, fn http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			s.mu.Lock()
			isDown := s.down
			s.mu.Unlock()
			if isDown {
				// What a browser sees when the PC is asleep: no answer at all.
				if hj, ok := w.(http.Hijacker); ok {
					if c, _, err := hj.Hijack(); err == nil {
						c.Close()
						return
					}
				}
				http.Error(w, "unreachable", http.StatusBadGateway)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			fn(w, r)
		})
	}
	api("GET /api/state", s.handleState)
	api("GET /api/events", s.handleEvents)
	api("GET /api/jobs", s.handleJobs)
	api("GET /api/jobs/{key}", s.handleJob)
	api("GET /api/jobs/{key}/log", s.handleLog)
	api("GET /api/history", s.handleHistory)
	api("POST /api/mode", s.control(s.handleMode))
	api("POST /api/machine/name", s.control(s.handleName))
	api("POST /api/jobs/{key}/cancel", s.control(s.handleCancel))
	api("POST /api/boxes/{name}/delete", s.control(s.handleDelete))
	api("POST /api/boxes/{name}/extend", s.control(s.handleExtend))
	api("POST /api/refusals", s.control(s.handleRefusal))
	api("POST /api/jobs/ended", s.control(s.handleJobEnded))
	api("POST /api/updates/check", s.control(s.handleUpdatesCheck))
	api("POST /api/cloud/usage", s.control(s.handleCloudUsage))
	api("POST /api/cloud/pause", s.control(s.handleCloudPause))
	api("POST /api/queue/{job}/cancel", s.control(s.handleQueueCancel))
	api("POST /api/queue/{job}/front", s.control(s.handleQueueFront))
	api("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, dash.CodeNotFound, "no such endpoint")
	})
	mux.HandleFunc("GET /dev/down", func(w http.ResponseWriter, r *http.Request) { s.setDown(true); fmt.Fprintln(w, "API down") })
	mux.HandleFunc("GET /dev/up", func(w http.ResponseWriter, r *http.Request) { s.setDown(false); fmt.Fprintln(w, "API up") })
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, dash.ErrorBody{Error: msg, Code: code})
}

// apiError is a control's failure, as boxd would answer it.
type apiError struct {
	status int
	code   string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func fail(status int, code, format string, args ...any) error {
	return &apiError{status, code, fmt.Sprintf(format, args...)}
}

// control checks what boxd checks of every POST, then runs fn: the header another site can't
// send without a preflight, and a JSON body. fn returns the status and body to answer with.
func (s *server) control(fn func(r *http.Request, body []byte) (int, any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(dash.ControlHeader) != "1" {
			writeErr(w, http.StatusForbidden, dash.CodeForbidden, "controls need the "+dash.ControlHeader+": 1 header")
			return
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			writeErr(w, http.StatusBadRequest, dash.CodeBadRequest, "controls take a JSON body")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			writeErr(w, http.StatusBadRequest, dash.CodeBadRequest, "couldn't read the body")
			return
		}
		if r.URL.Path != "/api/refusals" && r.URL.Path != "/api/jobs/ended" && r.URL.Path != "/api/updates/check" && s.fail {
			writeErr(w, http.StatusBadGateway, dash.CodeIncus, "Incus didn't answer: Post \"http://unix.socket/1.0/instances\": context deadline exceeded")
			return
		}
		status, out, err := fn(r, body)
		var ae *apiError
		switch {
		case errors.As(err, &ae):
			writeErr(w, ae.status, ae.code, ae.msg)
		case err != nil:
			writeErr(w, http.StatusInternalServerError, dash.CodeIncus, err.Error())
		case out == nil:
			w.WriteHeader(status)
		default:
			writeJSON(w, status, out)
		}
	}
}
