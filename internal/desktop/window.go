//go:build windows || (linux && desktop)

package desktop

import (
	"context"
	_ "embed"
	"log"

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
	after := beforeWindow()
	var w glaze.WebView
	w, err := glaze.NewWithOptions(glaze.Options{
		Debug:   opts.Debug,
		OnClose: func() { w.Terminate() },
	})
	after()
	if err != nil {
		return err
	}
	defer w.Destroy()
	w.SetTitle(Title)
	w.SetSize(1280, 820, glaze.HintNone)
	n := newNative(w)
	n.frame(true) // the page's default theme is Dim; it says otherwise once loaded

	if err := w.Bind("boxOpen", func(link string) {
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
	w.Init(initScript)

	// The window starts on the page's embedded copy, which shows "Connecting to the PC…" while it
	// asks, and goes to the PC as soon as it answers.
	offline, stop, err := serveOffline(opts.URL, func() {
		w.Dispatch(func() { w.Navigate(opts.URL) })
	})
	if err != nil {
		return err
	}
	defer stop()
	w.Navigate(offline)

	// Ctrl-C on `box app --wait` closes the window, as it would end any other command.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			w.Terminate()
		case <-done:
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

// initScript runs at the start of every document in the window. A link to another site opens in
// the browser instead: the window is for the dashboard, and a page from elsewhere in it would be
// handed the window's bindings. It also tells the window the page's theme, for the title bar.
const initScript = `(() => {
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
  addEventListener("DOMContentLoaded", () => {
    const root = document.documentElement;
    const tell = () => window.boxTheme(root.dataset.theme !== "light");
    new MutationObserver(tell).observe(root, { attributes: true, attributeFilter: ["data-theme"] });
    tell();
  });
})();`
