//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"go.kenn.io/agentsview/internal/db"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTranscriptFidelityRoundTripsViaDuckDBPush verifies that
// transcript_fidelity is preserved across a DuckDB push + read cycle.
func TestTranscriptFidelityRoundTripsViaDuckDBPush(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)

	sessionID := "fidelity-round-trip"
	sess := syncSession(sessionID, "alpha", "fidelity first", "2026-01-20T00:00:00.000Z", 1)
	sess.TranscriptFidelity = "high"

	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session:         sess,
		Messages:        []db.Message{syncMessage(sessionID, 0, "user", "fidelity first", "2026-01-20T00:00:00.000Z")},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)

	syncer := newTestSync(t, filepath.Join(t.TempDir(), "fidelity.duckdb"), local, SyncOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(t, err)

	store := NewStoreFromDB(syncer.DB())
	got, err := store.GetSession(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "high", got.TranscriptFidelity, "transcript_fidelity must survive push+read")
}

// TestDuckSessionFingerprintFieldsIncludesTranscriptFidelity verifies that
// a change to TranscriptFidelity produces a different fingerprint, so the
// push engine re-syncs when fidelity changes.
func TestDuckSessionFingerprintFieldsIncludesTranscriptFidelity(t *testing.T) {
	base := db.Session{
		ID:               "sess-fidelity",
		Project:          "proj",
		Machine:          "laptop",
		Agent:            "claude",
		MessageCount:     5,
		UserMessageCount: 2,
		CreatedAt:        "2026-03-11T12:00:00Z",
	}
	encode := func(s db.Session) string {
		data, err := json.Marshal(duckSessionFingerprintFields(s, "laptop"))
		require.NoError(t, err)
		return string(data)
	}
	fp1 := encode(base)

	modified := base
	modified.TranscriptFidelity = "low"
	fp2 := encode(modified)

	assert.NotEqual(t, fp1, fp2, "TranscriptFidelity change must alter fingerprint")
}
