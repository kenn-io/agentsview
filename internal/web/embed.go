package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist all:fallback
var assetFS embed.FS

// Assets returns the compiled frontend filesystem when it is
// embedded, or a tracked fallback page for backend-only builds.
func Assets() (fs.FS, error) {
	dist, err := fs.Sub(assetFS, "dist")
	if err == nil {
		if _, statErr := fs.Stat(dist, "index.html"); statErr == nil {
			return dist, nil
		}
	}
	return fs.Sub(assetFS, "fallback")
}
