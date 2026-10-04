package console

import (
	_ "embed"
	"html/template"
	"log"
	"net/http"
)

//go:embed static/index.html
var indexHTML string

// indexTmpl renders the dashboard (see pageData): parsed once at
// init, so a broken page fails the boot instead of every request.
var indexTmpl = template.Must(template.New("index").Parse(indexHTML))

// renderIndex writes the dashboard for data. The execute fails only
// when the client goes away mid-write; that refusal is logged,
// never a 500 after a partial page.
func renderIndex(w http.ResponseWriter, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTmpl.Execute(w, data); err != nil {
		log.Printf("Error executing dashboard: %v", err)
	}
}
