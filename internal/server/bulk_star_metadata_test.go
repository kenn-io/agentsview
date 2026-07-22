package server_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/artifact"
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
	assert.Len(t, readMetadataEvents(t, te), 2,
		"one star event per existing session")
}

func TestBulkStarRetriesRepairPublishedMetadataState(t *testing.T) {
	te := setup(t, withArtifactOrigin("desktop-d4e5f6"))
	te.seedSession(t, "s1", "alpha", 2)

	execTestDDL(t, te, `
CREATE TRIGGER fail_metadata_replay_state_insert
BEFORE INSERT ON metadata_replay_state
BEGIN
	SELECT RAISE(FAIL, 'forced metadata replay failure');
END;
`)

	w := te.requestJSON(t, http.MethodPost, "/api/v1/starred/bulk",
		`{"session_ids":["s1"]}`)
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	ids, err := te.db.ListStarredSessionIDs(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"s1"}, ids)
	assert.Equal(t, 0, serverMetadataTableCount(t, te, "metadata_replay_state", "session_gid = 'desktop-d4e5f6~s1'"))
	events := readMetadataEvents(t, te)
	require.Len(t, events, 1)
	assert.Equal(t, artifact.MetadataOpStar, events[0].Op)

	execTestDDL(t, te, `DROP TRIGGER fail_metadata_replay_state_insert`)
	w = te.requestJSON(t, http.MethodPost, "/api/v1/starred/bulk",
		`{"session_ids":["s1"]}`)
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, artifact.MetadataOpStar,
		serverMetadataReplayOp(t, te, "desktop-d4e5f6~s1", "starred"))
	assert.Len(t, readMetadataEvents(t, te), 1)
}
