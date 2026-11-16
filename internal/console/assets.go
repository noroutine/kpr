package console

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
)

//go:embed all:static
var staticFiles embed.FS

// staticFS is the embedded SvelteKit build output (see
// web/console, copied by `just web-build`): one static
// export, no server rendering — the page fetches /metrics
// and /api/overview at runtime, so the bundle carries no
// build-time data.
func staticFS() (fs.FS, error) {
	return fs.Sub(staticFiles, "static")
}

// indexHandler serves the built SPA shell. A dying connection
// mid-write logs the refusal instead of panicking.
func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write(data); err != nil {
		log.Printf("Error writing dashboard: %v", err)
	}
}

// faviconHandler serves the tab icon browsers request unprompted
// (/favicon.ico): without it every page view logs a 404. SVG under
// the .ico path — every modern browser accepts it — so the icon
// stays a readable text file, not a binary blob. GET only.
func faviconHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data, err := staticFiles.ReadFile("static/favicon.svg")
	if err != nil {
		http.Error(w, "icon unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(data)
}
