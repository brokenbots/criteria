package runstate

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:viewer/dist
var distFS embed.FS

// NewViewer returns the embedded run-viewer static bundle as an SPA handler:
// the bundle is served at / with unknown paths falling back onto
// index.html so client-side routes render on deep links.
//
// The bundle is built by the castle repo (parapet packages/run-viewer
// standalone build) and vendored into dist/ as a release artifact kept
// versioned with the engine (dist/version.txt). The checked-in placeholder
// renders a plain notice until a real bundle is vendored.
func NewViewer() (http.Handler, error) {
	sub, err := fs.Sub(distFS, "viewer/dist")
	if err != nil {
		return nil, err
	}
	fileServer := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(sub, p); err != nil {
			// SPA fallback: unknown paths render the app shell.
			r.URL.Path = "/"
		}
		fileServer.ServeHTTP(w, r)
	}), nil
}