//go:build windows || (linux && desktop)

package desktop

import (
	"context"
	_ "embed"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/crgimenes/glaze"
)

var (
	//go:embed icons/icon.png
	iconPNG []byte // 256 px: the window's and the taskbar's icon
	//go:embed icons/small.png
	smallPNG []byte // 32 px: the title bar's
)

// Supported says whether this build has the window.
const Supported = true

// Run shows the window and returns when it is closed. It must be called from the main goroutine:
// the UI belongs to the thread that made it.
func Run(ctx context.Context, opts Options) error {
	// The window starts on the page's embedded copy, which shows "Connecting to the PC…" while it
	// asks, and goes to the PC as soon as it answers (keeper).
	var k *keeper
	offline, stop, err := serveOffline(opts.URL, func() { k.up() })
	if err != nil {
		return err
	}
	defer stop()
	ours := []string{originOf(opts.URL), originOf(offline)}

	// Where the window is, as the web view says (macOS and Linux; Windows never says): a link is
	// opened only for a document of ours.
	var mu sync.Mutex
	at := ""
	after := beforeWindow()
	var w glaze.WebView
	w, err = glaze.NewWithOptions(glaze.Options{
		Debug:   opts.Debug,
		OnClose: func() { w.Terminate() },
		OnURLChange: func(u string) {
			mu.Lock()
			at = originOf(u)
			mu.Unlock()
		},
	})
	after()
	if err != nil {
		return err
	}
	defer w.Destroy()
	k = newKeeper(opts.URL, offline, func(u string) { w.Dispatch(func() { w.Navigate(u) }) })
	w.SetTitle(Title)
	w.SetSize(1280, 820, glaze.HintNone)
	n := newNative(w)
	n.frame(true) // the page's default theme is Dim; it says otherwise once loaded

	if err := w.Bind("boxOpen", func(link string) {
		mu.Lock()
		where := at
		mu.Unlock()
		if where != "" && where != ours[0] && where != ours[1] {
			return
		}
		if u, ok := outside(link); ok {
			if err := opts.Open(u); err != nil {
				log.Printf("open %s: %v", u, err)
			}
		}
	}); err != nil {
		return err
	}
	if err := w.Bind("boxTheme", func(dark bool) {
		w.Dispatch(func() { n.frame(dark) })
	}); err != nil {
		return err
	}
	if err := w.Bind("boxHere", k.here); err != nil {
		return err
	}
	origins, _ := json.Marshal(ours)
	w.Init(strings.Replace(initScript, "ORIGINS", string(origins), 1))
	w.Navigate(offline)

	// Ctrl-C on `box app --wait` closes the window, as it would end any other command.
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(pingEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				w.Terminate()
				return
			case <-done:
				return
			case <-t.C:
				w.Dispatch(func() { w.Eval("window.boxPing && window.boxPing()") })
				k.tick()
			}
		}
	}()
	w.Run()
	return nil
}

// native is what each platform adds to the window. On the UI thread.
type native interface {
	// frame makes the title bar dark or light, to match the page.
	frame(dark bool)
}

// initScript runs at the start of every document in the window, before the document's own
// scripts. ORIGINS is the dashboard's origin and the offline page's.
//
// In a document of another origin (a redirect, a link followed in place), it cuts the bridge to
// the window, so that page can't use the bindings, and it doesn't answer the window's ping, which
// takes the window back to the offline page.
//
// In ours, a link to another site opens in the browser rather than in the window, the page's
// theme sets the title bar, and the document answers the ping with its origin and whether it is
// the dashboard's page rather than an error (keeper.here).
const initScript = `(() => {
  const ours = ORIGINS;
  if (!ours.includes(location.origin)) {
    const cut = (o, k) => { try { Object.defineProperty(o, k, { value: () => {}, writable: false, configurable: false }); } catch {} };
    const wv = window.chrome && window.chrome.webview;
    if (wv) { cut(wv, "postMessage"); cut(Object.getPrototypeOf(wv), "postMessage"); }
    try { Object.defineProperty(window, "webkit", { value: undefined, writable: false, configurable: false }); } catch {}
    return;
  }
  const outside = (href) => {
    try {
      const u = new URL(href, location.href);
      return (u.protocol === "http:" || u.protocol === "https:") && u.origin !== location.origin ? u.href : "";
    } catch { return ""; }
  };
  const click = (e) => {
    const a = e.target instanceof Element && e.target.closest("a[href]");
    const href = a && outside(a.href);
    if (!href) return;
    e.preventDefault();
    window.boxOpen(href);
  };
  addEventListener("click", click, true);
  addEventListener("auxclick", click, true);
  const open = window.open;
  window.open = (href, ...rest) => {
    const out = outside(href);
    if (out) { window.boxOpen(out); return null; }
    return open.call(window, href, ...rest);
  };
  const page = () => {
    const nav = performance.getEntriesByType("navigation")[0];
    const failed = nav && nav.responseStatus >= 400;
    return !failed && !!document.querySelector('script[src="/assets/app.js"]');
  };
  window.boxPing = () => document.readyState !== "loading" && window.boxHere(location.origin, page());
  addEventListener("DOMContentLoaded", () => {
    window.boxPing();
    const root = document.documentElement;
    const tell = () => window.boxTheme(root.dataset.theme !== "light");
    new MutationObserver(tell).observe(root, { attributes: true, attributeFilter: ["data-theme"] });
    tell();
  });
})();`
