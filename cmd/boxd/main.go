// boxd is the agent-box dashboard server. It runs in the agents distro on the
// PC under systemd (agent-box-boxd.service), listens on 127.0.0.1:7766 and is
// put on the tailnet by `tailscale serve`. See docs/ui.md.
package main

import (
	"flag"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/a-Fig/agent-box/internal/boxd"
)

func main() {
	cfgPath := flag.String("config", "/etc/agent-box/boxd.json", "config file")
	flag.Parse()
	cfg, err := boxd.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("boxd: %v", err)
	}
	if len(cfg.Owners) == 0 {
		log.Printf("boxd: no owners in %s: every request will be refused", *cfgPath)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<!doctype html><title>agent-box</title><p>boxd is up. Signed in as %s.</p>\n",
			html.EscapeString(r.Header.Get(boxd.LoginHeader)))
	})
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           boxd.SecurityHeaders(boxd.Owners(cfg.Owners, mux)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("boxd: listening on %s for %d owner(s)", cfg.Listen, len(cfg.Owners))
	if err := srv.ListenAndServe(); err != nil {
		log.Printf("boxd: %v", err)
		os.Exit(1)
	}
}
