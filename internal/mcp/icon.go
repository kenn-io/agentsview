package mcp

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"net/http"
	"time"
)

// serverIconPNG is copied unchanged from desktop/src-tauri/icons/128x128@2x.png.
//
//go:embed assets/icon.png
var serverIconPNG []byte

// A data URI works for stdio and HTTP without a deployment-specific public URL.
var serverIconSource = "data:image/png;base64," + base64.StdEncoding.EncodeToString(serverIconPNG)

// registerIconRoutes exposes only the public mark, outside MCP authentication.
// Both aliases serve the PNG declared in serverInfo, including /favicon.ico.
func registerIconRoutes(mux *http.ServeMux) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeContent(w, r, "icon.png", time.Time{}, bytes.NewReader(serverIconPNG))
	})
	mux.Handle("/icon.png", handler)    //nolint:kennlint // nohttpmux: the MCP listener has no typed API router; this is a static asset.
	mux.Handle("/favicon.ico", handler) //nolint:kennlint // nohttpmux: the MCP listener has no typed API router; this is a static asset.
}
