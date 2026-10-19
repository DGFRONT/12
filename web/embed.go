// Package web embeds the browser frontend (plain HTML, CSS and JS, no build step).
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed *.html static
var files embed.FS

// Static serves /static/* assets.
func Static() http.Handler {
	return http.FileServer(http.FS(files))
}

// Page serves a single HTML page.
func Page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := fs.ReadFile(files, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(data)
	}
}
