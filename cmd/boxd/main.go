// boxd is the agent-box dashboard server. It runs in the agents distro on the
// PC under systemd (agent-box-boxd.service), listens on 127.0.0.1:7766 and is
// put on the tailnet by `tailscale serve`. It polls Incus, keeps the history
// and serves the page and its API. See docs/ui.md.
package main

import (
	"bufio"
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/a-Fig/agent-box/internal/boxd"
	"github.com/a-Fig/agent-box/internal/mode"
)

func main() {
	cfgPath := flag.String("config", "/etc/agent-box/boxd.json", "config file")
	socket := flag.String("socket", "/var/lib/incus/unix.socket", "Incus's unix socket")
	page := flag.String("static", "", "serve the page from this folder instead of the embedded copy")
	flag.Parse()
	cfg, err := boxd.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("boxd: %v", err)
	}
	if len(cfg.Owners) == 0 {
		log.Printf("boxd: no owners in %s: every request will be refused", *cfgPath)
	}
	if err := boxd.CheckClient(os.Getenv, exec.LookPath); err != nil {
		log.Printf("boxd: WARNING: job scans, logs and queued starts will fail: %v", err)
	}
	hist, err := boxd.OpenHistory(cfg.HistoryDir, time.Now)
	if err != nil {
		log.Fatalf("boxd: history: %v", err)
	}
	queue, err := boxd.OpenQueue(cfg.QueueDir)
	if err != nil {
		log.Fatalf("boxd: queue: %v", err)
	}
	w := &boxd.Watcher{Queue: queue, Whois: boxd.TailscaleWhois,
		Incus: boxd.NewSocket(*socket, mode.Project), Hist: hist, Now: time.Now,
		MachineID: cfg.MachineID, MachineName: cfg.MachineName, WSLMemory: wslMemory, Logf: log.Printf,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go w.Run(ctx)

	srv := &boxd.Server{W: w}
	if *page != "" {
		srv.Page = os.DirFS(*page)
	}
	hs := &http.Server{
		Addr:              cfg.Listen,
		Handler:           boxd.SecurityHeaders(boxd.Owners(cfg.Owners, srv.Handler())),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}()
	log.Printf("boxd: listening on %s for %d owner(s)", cfg.Listen, len(cfg.Owners))
	if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("boxd: %v", err)
		os.Exit(1)
	}
}

// wslMemory is what the distro's VM holds: the memory Linux isn't leaving
// free, page cache included, since that is what vmmem holds on Windows.
func wslMemory() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	var total, free int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), ":")
		n, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		switch k {
		case "MemTotal":
			total = n << 10
		case "MemFree":
			free = n << 10
		}
	}
	return total - free
}
