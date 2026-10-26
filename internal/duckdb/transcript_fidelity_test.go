//go:build !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"testing"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTranscriptFidelityRoundTripsViaDuckDBPush verifies that
// transcript_fidelity is preserved across a DuckDB push + read cycle.
func TestTranscriptFidelityRoundTripsViaDuckDBPush(t *testing.T) {
	ctx := t.Context()
	local := newLocalDB(t)

	sessionID := "fidelity-round-trip"
	sess := syncSession(sessionID, "alpha", "fidelity first", "2026-01-20T00:00:00.000Z", 1)
	sess.TranscriptFidelity = "high"

	_, err := local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
		Session:         sess,
		Messages:        []db.Message{syncMessage(sessionID, 0, "user", "fidelity first", "2026-01-20T00:00:00.000Z")},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)

	syncer := newTestSync(t, filepath.Join(t.TempDir(), "fidelity.duckdb"), local, storage.MirrorPushOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(t, err)

	store := NewStoreFromDB(syncer.DB())
	got, err := store.GetSession(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "high", got.TranscriptFidelity, "transcript_fidelity must survive push+read")
}
