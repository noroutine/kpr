package app

import (
	"embed"
	"html/template"
	"io/fs"
	"log"
	"net/http"
)

//go:embed all:static
var staticFiles embed.FS

//go:embed static/index.html
var indexHTML string

// indexTmpl renders the front page with the app base path (see
// KPR_APP_BASE_PATH): parsed once at init, so a broken page fails
// the boot instead of every request. No error branches — an
// embedded string always parses or the build is broken.
var indexTmpl = template.Must(template.New("index").Parse(indexHTML))

// renderIndex writes the front page with the asset hrefs joined to
// base. The execute fails only when the client goes away mid-write;
// that refusal is logged, never a 500 after a partial page.
func renderIndex(w http.ResponseWriter, base string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTmpl.Execute(w, map[string]string{"BasePath": base}); err != nil {
		log.Printf("Error executing index.html: %v", err)
	}
}

// GetStaticFS returns the embedded static file system
func GetStaticFS() (http.FileSystem, error) {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, err
	}
	return http.FS(sub), nil
}
