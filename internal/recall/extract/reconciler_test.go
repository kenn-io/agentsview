package extract

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func seedServedGeneratedEntry(t *testing.T, d *db.DB, fp, sessionID, entryID string) {
	t.Helper()
	_, err := d.InsertExtractedRecallEntries(context.Background(), []db.RecallEntry{{
		ID: entryID, Type: "fact", ReviewState: "unreviewed_auto",
		Status: "accepted", Title: "t", Body: "b",
		SourceSessionID: sessionID, SourceRunID: fp, ProvenanceOK: true,
	}})
	require.NoError(t, err)
}

// TestReconcilerRetractsIneligibleGeneratedEntries pins that retraction runs
// without a Manager or model client: an activated generation's entries for a
// session that later becomes ineligible are deleted, while an eligible
// session's entries survive.
func TestReconcilerRetractsIneligibleGeneratedEntries(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	fp := "fp-a"
	_, err := d.EnsureExtractGeneration(ctx, db.ExtractGeneration{
		Fingerprint: fp, Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	seedSession(t, d, "sess-ok", turnMessages("keep this", "done"), nil)
	seedSession(t, d, "sess-gone", turnMessages("drop this", "done"), nil)
	seedServedGeneratedEntry(t, d, fp, "sess-ok", "e-ok")
	seedServedGeneratedEntry(t, d, fp, "sess-gone", "e-gone")
	require.NoError(t, d.SoftDeleteSession("sess-gone"))

	started, _, err := NewReconciler(d).TryPass(ctx, PassOptions{})
	require.NoError(t, err)
	require.True(t, started)

	gone, err := d.GetRecallEntry(ctx, "e-gone")
	require.NoError(t, err)
	assert.Nil(t, gone, "an ineligible session's generated entry must be retracted")
	ok, err := d.GetRecallEntry(ctx, "e-ok")
	require.NoError(t, err)
	require.NotNil(t, ok, "an eligible session's entry must survive")
}
