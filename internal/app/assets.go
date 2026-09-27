package app

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:static
var staticFiles embed.FS

// GetStaticFS returns the embedded static file system
func GetStaticFS() (http.FileSystem, error) {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, err
	}
	return http.FS(sub), nil
}
