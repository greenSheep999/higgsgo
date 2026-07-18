// Package webui embeds the compiled admin SPA (Vite output under
// webui/dist) into the higgsgo binary and serves it as an http.Handler.
//
// Build workflow: `pnpm --dir webui build` populates webui/dist, and this
// file's //go:embed directive picks it up during `go build`. The embed
// directive requires webui/dist to exist at compile time; when the SPA has
// not been built yet, the fallback build tag in embed_stub.go is used
// instead so the Go binary still compiles.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// Handler returns an http.Handler that serves the embedded SPA. Requests
// for a real file under /dist are served as-is; every other path falls
// back to index.html so client-side routing works from any URL.
//
// The handler does NOT enforce auth — mount it inside a chi.Router group
// that already carries the admin BearerAuth middleware.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// Only happens if dist/ was not embedded — build machine bug.
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "webui bundle missing", http.StatusInternalServerError)
		})
	}
	files := http.FS(sub)
	fileServer := http.FileServer(files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Trim the mount prefix so /webui/foo.js resolves to /foo.js on
		// the embedded FS. Chi's Mount already strips the pattern from
		// r.URL.Path, but the client may hit us with a bare "/" that
		// http.FileServer treats as the root — that's what we want.
		trimmed := strings.TrimPrefix(r.URL.Path, "/")
		if trimmed == "" {
			serveIndex(w, r, files)
			return
		}
		f, err := files.Open(trimmed)
		if err != nil {
			// Not a real file — SPA route. Serve index.html so the
			// TanStack router can pick it up.
			serveIndex(w, r, files)
			return
		}
		_ = f.Close()
		fileServer.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, files http.FileSystem) {
	f, err := files.Open("index.html")
	if err != nil {
		http.Error(w, "index.html missing", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", stat.ModTime(), f.(interface {
		Read([]byte) (int, error)
		Seek(int64, int) (int64, error)
	}))
}
