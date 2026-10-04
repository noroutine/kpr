package app

import (
	"embed"
	"html/template"
	"io/fs"
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

// GetStaticFS returns the embedded static file system
func GetStaticFS() (http.FileSystem, error) {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, err
	}
	return http.FS(sub), nil
}
