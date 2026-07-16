// Package web serves the embedded single-page app.
//
// The SPA build lives at the repo-root web/dist and is embedded into the binary
// at build time. Because go:embed cannot reach a sibling directory, the build is
// selected by a build tag:
//
//   - Default build (no tags): embeds internal/web/dist-placeholder, a tiny
//     "run scripts/build-web.sh" page. This keeps `go build ./...` working with
//     NO Node toolchain and no generated files - the API is fully functional.
//   - `-tags embedui`: embeds internal/web/dist (gitignored), which
//     scripts/build-web.sh populates from the real web/dist build. This is the
//     production binary and is what scripts/build-web.sh produces.
//
// content() returns the correct embedded FS for the active build (see
// content_placeholder.go and content_embed.go).
package web

import (
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// The CSP for the embedded UI. Strict same-origin: scripts only from self,
// styles allow inline (React sets style attributes; the CSS is a self-hosted
// bundle), images allow data: URIs, and the app talks only to its own origin.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"base-uri 'self'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// Handler serves the embedded SPA with a client-side-routing fallback.
type Handler struct {
	fsys fs.FS
}

// New returns a Handler over the embedded build for the active build tag.
func New() *Handler {
	return &Handler{fsys: content()}
}

// ServeHTTP serves a static asset when one exists at the request path, otherwise
// falls back to index.html for client-side routes. A request for a path that
// looks like a file (has an extension) but is missing returns 404, so
// /config.json and missing assets are not masked by the SPA fallback.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	clean := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
	if clean == "" || clean == "." {
		h.serveIndex(w, r)
		return
	}
	if f, err := h.fsys.Open(clean); err == nil {
		info, statErr := f.Stat()
		_ = f.Close()
		if statErr == nil && !info.IsDir() {
			h.serveFile(w, r, clean)
			return
		}
	}
	// Missing. A path with a file extension is a genuine 404 (e.g. /config.json,
	// a stale hashed asset); an extensionless path is a client route.
	if path.Ext(clean) != "" {
		http.NotFound(w, r)
		return
	}
	h.serveIndex(w, r)
}

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
	w.Header().Set("Cache-Control", "no-cache")
	h.serveFile(w, r, "index.html")
}

func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, name string) {
	f, err := h.fsys.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "stat error", http.StatusInternalServerError)
		return
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "not seekable", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, name, info.ModTime(), rs)
}
