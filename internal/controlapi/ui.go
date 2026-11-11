package controlapi

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed ui/*
var uiFiles embed.FS

func serveUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method not allowed", 405)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/ui/")
	if r.URL.Path == "/ui" || name == "" {
		name = "index.html"
	}
	contentType := ""
	switch name {
	case "index.html":
		contentType = "text/html; charset=utf-8"
	case "app.js":
		contentType = "text/javascript; charset=utf-8"
	case "app.css", "pico.min.css":
		contentType = "text/css; charset=utf-8"
	case "icons.svg":
		contentType = "image/svg+xml"
	case "IBMPlexSans-Regular.woff2", "IBMPlexSans-SemiBold.woff2":
		contentType = "font/woff2"
	default:
		http.NotFound(w, r)
		return
	}
	data, err := uiFiles.ReadFile("ui/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}
