// Package static is the dashboard's page: plain HTML, CSS and JS with no build step, embedded so
// boxd (and the devserver) serve the files as they are.
package static

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed index.html assets
var FS embed.FS

// CSP is the page's Content-Security-Policy. Everything comes from the page's own origin; data:
// images are Murmur's surface grain, an SVG in the stylesheet. Nothing is inline.
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; " +
	"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'; object-src 'none'"

// Handler serves the page's files from fsys (FS, or a folder while working on the page) with
// its security headers.
func Handler(fsys fs.FS) http.Handler {
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", CSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}
