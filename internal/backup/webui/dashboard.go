package webui

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
)

// handleDashboard serves the SPA shell (the built dist/index.html), which
// fetches /api/meta and /api/status (among others) itself and polls from
// there; the page carries no server-rendered state at all any more (see
// metaJSON/handleMeta).
func handleDashboard(html string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, html)
	}
}

// cacheForever wraps next, marking every response immutable and
// long-lived — safe here because Vite content-hashes every file under
// dist/assets, so a given URL's content never changes; a new build simply
// produces new URLs (see dashboardIndexHTML, re-read at each startup, which
// is how a new build's asset URLs actually reach a client).
func cacheForever(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		next.ServeHTTP(w, r)
	})
}

// dashboardDistFS holds the dashboard SPA's production build (see
// frontend/, built by `npm run build` into this directory — not committed,
// see .gitignore), embedded via go:embed rather than fetched at runtime.
// The "all:" prefix matters: Vite can emit dotfiles, which go:embed's
// default patterns skip.
//
//go:embed all:dist
var dashboardDistFS embed.FS

// dashboardAssetsFS serves dist/assets/* at GET /assets/* (see StartWebUI),
// stripped of its "dist/assets/" prefix.
var dashboardAssetsFS = mustSubFS(dashboardDistFS, "dist/assets")

// dashboardIndexHTML is the SPA shell's HTML, read once at package init
// (mirroring the pre-SPA dashboardHTML var's own "build it once, not per
// request" shape) rather than on every GET /.
var dashboardIndexHTML = mustReadFS(dashboardDistFS, "dist/index.html")

// mustSubFS/mustReadFS panic on error rather than returning one: both only
// ever fail if the frontend/ build didn't run before `go build` (see
// frontend/README.md), which is a build-time misconfiguration to fail fast
// and loudly on, not a runtime condition any caller could recover from.
func mustSubFS(fsys embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic("webui: " + err.Error())
	}

	return sub
}

func mustReadFS(fsys embed.FS, name string) string {
	b, err := fsys.ReadFile(name)
	if err != nil {
		panic("webui: " + err.Error())
	}

	return string(b)
}
