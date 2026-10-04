package main

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed frontend/dist
var adminBundle embed.FS

func adminStaticHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	spaPath := strings.TrimSuffix(r.URL.Path, "/")
	switch spaPath {
	case "/admin", "/admin/accounts", "/admin/import", "/admin/logs", "/admin/model-visibility", "/admin/providers", "/admin/settings", "/admin/settings/general", "/admin/settings/api-keys", "/admin/settings/security", "/admin/settings/models", "/admin/settings/upstreams", "/admin/settings/advanced", "/admin/about":
		content, err := fs.ReadFile(adminBundle, "frontend/dist/index.html")
		if err != nil {
			http.Error(w, "admin frontend unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(content)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/admin/")
	if !strings.HasPrefix(name, "assets/") || !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	content, err := fs.ReadFile(adminBundle, "frontend/dist/"+name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, path.Base(name), time.Time{}, bytes.NewReader(content))
}
