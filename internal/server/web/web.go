package web

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
)

// Files contains the embedded dashboard templates and static assets.
//
//go:embed templates/*.html static/*.css static/*.js
var Files embed.FS

// MustReadTemplate returns an embedded HTML template and panics if the binary was built incorrectly.
func MustReadTemplate(name string) string {
	data, err := Files.ReadFile("templates/" + name)
	if err != nil {
		panic(fmt.Sprintf("read embedded template %s: %v", name, err))
	}
	return string(data)
}

// MustReadStatic returns an embedded static asset. Tests use it to assert migrated UI code.
func MustReadStatic(name string) string {
	data, err := Files.ReadFile("static/" + name)
	if err != nil {
		panic(fmt.Sprintf("read embedded static asset %s: %v", name, err))
	}
	return string(data)
}

// StaticHandler serves embedded static dashboard assets under /static/.
func StaticHandler() http.Handler {
	static, err := fs.Sub(Files, "static")
	if err != nil {
		panic(fmt.Sprintf("open embedded static fs: %v", err))
	}
	return http.StripPrefix("/static/", http.FileServer(http.FS(static)))
}
