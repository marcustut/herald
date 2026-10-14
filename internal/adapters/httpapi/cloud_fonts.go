package httpapi

import (
	"embed"
	"net/http"
	"path"
	"strings"
)

// fontAssets holds the four brand woff2 files so the dashboard renders in
// Instrument Serif / DM Sans / JetBrains Mono without any third-party request.
//
//go:embed assets/fonts/*.woff2
var fontAssets embed.FS

// handleDashboardFont serves one embedded font. The name is a single ServeMux
// path segment and is checked against the embedded directory, so a traversal or
// an unknown file is simply a 404.
func (c *Cloud) handleDashboardFont(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name != path.Base(name) || !strings.HasSuffix(name, ".woff2") {
		http.NotFound(w, r)
		return
	}
	data, err := fontAssets.ReadFile("assets/fonts/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "font/woff2")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(data)
}
