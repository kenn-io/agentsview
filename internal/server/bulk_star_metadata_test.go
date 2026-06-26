package server_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBulkStarAppendsMetadataEvents(t *testing.T) {
	te := setup(t, withArtifactOrigin("desktop-d4e5f6"))
	te.seedSession(t, "s1", "alpha", 2)
	te.seedSession(t, "s2", "beta", 2)

	w := te.requestJSON(t, http.MethodPost, "/api/v1/starred/bulk",
		`{"session_ids":["s1","s2","missing"]}`)
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	// Both existing sessions are starred; the missing one is skipped.
	w = te.get(t, "/api/v1/starred")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	list := decode[starredHandlerResponse](t, w)
	assert.ElementsMatch(t, []string{"s1", "s2"}, list.SessionIDs)

	// A star metadata event artifact was written for each session actually
	// starred, so the migrated stars converge through artifact sync. The missing
	// session produces no event.
	metaDir := filepath.Join(te.dataDir, "artifacts", "desktop-d4e5f6", "meta")
	entries, err := os.ReadDir(metaDir)
	require.NoError(t, err)
	assert.Len(t, entries, 2, "one star event per existing session")
}
