// Package ui serves the desktop app's web frontend, embedded in the
// binary. `npm --prefix web run build` writes it to dist/app; without
// it, the committed dist/index.html placeholder says how to build it.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:dist
var files embed.FS

// csp lets the page load only its own files. Inline styles are needed by
// the terminal widget.
const csp = "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:"

// Built reports whether the real frontend is embedded.
func Built() bool {
	_, ok := pick(files)
	return ok
}

// pick returns the built frontend (dist/app) if present, else the
// placeholder (dist).
func pick(fsys fs.FS) (fs.FS, bool) {
	if _, err := fs.Stat(fsys, "dist/app/index.html"); err == nil {
		sub, _ := fs.Sub(fsys, "dist/app")
		return sub, true
	}
	sub, _ := fs.Sub(fsys, "dist")
	return sub, false
}

// Handler serves the frontend (GET and HEAD only).
func Handler() http.Handler {
	fsys, _ := pick(files)
	static := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		static.ServeHTTP(w, r)
	})
}
