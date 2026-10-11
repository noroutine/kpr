package console

import (
	_ "embed"
	"html/template"
	"log"
	"net/http"
)

//go:embed static/index.html
var indexHTML string

//go:embed static/style.css
var styleCSS string

// styleHandler serves the dashboard stylesheet as CSS: one
// file, one route, no file server to feed. GET only, like the
// other readers; the href carries the base path, so a
// stripped-prefix console resolves it.
func styleHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	_, _ = w.Write([]byte(styleCSS))
}

// indexTmpl renders the dashboard (see pageData): parsed once at
// init, so a broken page fails the boot instead of every request.
// The bulb func hangs the standing color on a card; the model
// decides the standing in keeper, the template only colors it.
var indexTmpl = template.Must(template.New("index").Funcs(template.FuncMap{
	"bulb": bulbClass,
}).Parse(indexHTML))

// renderIndex writes the dashboard for data. The execute fails only
// when the client goes away mid-write; that refusal is logged,
// never a 500 after a partial page.
func renderIndex(w http.ResponseWriter, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTmpl.Execute(w, data); err != nil {
		log.Printf("Error executing dashboard: %v", err)
	}
}
