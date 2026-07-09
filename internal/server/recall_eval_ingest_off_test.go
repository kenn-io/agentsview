//go:build !evalingest

package server_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestEvalIngestRouteNotServedWithoutBuildTag confirms that the lab-only
// raw-trajectory eval ingest endpoint is absent from the default binary:
// without the evalingest build tag, registerEvalIngestRoutes is a no-op, so
// the path is unregistered. The mux's only match left is the catch-all "/"
// SPA route, which serves the SPA shell (200 text/html) instead of a JSON
// ingest result for any unrecognized path, including this one.
func TestEvalIngestRouteNotServedWithoutBuildTag(t *testing.T) {
	te := setup(t)
	w := te.post(t, "/api/v1/recall/eval/trajectories", `{}`)
	assertStatus(t, w, http.StatusOK)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html",
		"unregistered route without the evalingest tag must fall through to the SPA handler, not a JSON ingest result")
}
