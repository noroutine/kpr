package app

import (
	"embed"
	"html/template"
	"io/fs"
	"net/http"
)

//go:embed all:static
var staticFiles embed.FS

// indexTmpl renders the front page with the app base path (see
// KPR_APP_BASE_PATH): parsed once, so a broken embed fails the boot
// instead of every request.
var indexTmpl = func() *template.Template {
	raw, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		panic(err)
	}
	return template.Must(template.New("index").Parse(string(raw)))
}()

// GetStaticFS returns the embedded static file system
func GetStaticFS() (http.FileSystem, error) {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, err
	}
	return http.FS(sub), nil
}
