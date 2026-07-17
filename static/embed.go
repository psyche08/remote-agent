package webui

import (
	"embed"
	"net/http"
	"strings"
)

// Files are embedded into the device agent so the full console is versioned
// and deployed with the binary. The relay only needs the stable device host
// from shell.html.
//
//go:embed index.html manifest.webmanifest sw.js icon-192.png icon-512.png
var files embed.FS

// Handler serves the device-owned console. The build version is injected at
// response time so browser/agent version checks always describe the device
// binary that supplied the page.
func Handler(version string) http.Handler {
	assets := http.FileServer(http.FS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			body, err := files.ReadFile("index.html")
			if err != nil {
				http.Error(w, "device UI unavailable", http.StatusInternalServerError)
				return
			}
			if strings.TrimSpace(version) == "" {
				version = "dev"
			}
			body = []byte(strings.ReplaceAll(string(body), "__REMOTE_CODING_STATIC_VERSION__", version))
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(body)
			return
		}
		assets.ServeHTTP(w, r)
	})
}
