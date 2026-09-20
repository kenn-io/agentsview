//go:build !evalingest

package server_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The default binary must reject POST requests to the lab-only ingest route.
func TestEvalIngestRouteNotServedWithoutBuildTag(t *testing.T) {
	te := setup(t)
	w := te.post(t, "/api/v1/recall/eval/trajectories", `{}`)
	assertStatus(t, w, http.StatusMethodNotAllowed)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/plain",
		"the default binary must reject ingest instead of serving the SPA")
}
