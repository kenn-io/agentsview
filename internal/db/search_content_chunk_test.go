package db

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSemanticAllowedSessionIDsOverSQLiteVarLimit forces the reader pool's
// SQLite bind-variable limit down to 999 (mirroring
// forceReaderVarLimit's rationale in activityreport_test.go: some builds
// compile against SQLite's older 999 default), then asks
// semanticAllowedSessionIDs to scope a candidate set of 1002 session IDs — a
// count a deep semantic overfetch (thousands of hits from distinct
// sessions) can plausibly produce and single-shot IN (...) query would
// exceed. It must chunk the query and still return exactly the real,
// filter-passing sessions.
func TestSemanticAllowedSessionIDsOverSQLiteVarLimit(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	forceReaderVarLimit(t, d, 999)

	// Guard: prove the lowered limit is live on the pool, so a setup that
	// failed to constrain it cannot mask the regression checked below.
	overLimitPh, overLimitArgs := inPlaceholders(make([]string, 1001))
	_, probeErr := d.getReader().QueryContext(
		ctx, "SELECT 1 WHERE '' IN "+overLimitPh, overLimitArgs...)
	require.Error(t, probeErr, "reader variable limit was not constrained")

	insertSession(t, d, "real-1", "proj")
	insertSession(t, d, "real-2", "proj")

	ids := []string{"real-1", "real-2"}
	for i := range 1000 {
		ids = append(ids, fmt.Sprintf("fake-%d", i))
	}

	f := ContentSearchFilter{IncludeOneShot: true, IncludeAutomated: true}
	allowed, err := d.semanticAllowedSessionIDs(ctx, f, ids)
	require.NoError(t, err)

	assert.True(t, allowed["real-1"])
	assert.True(t, allowed["real-2"])
	assert.Len(t, allowed, 2, "no nonexistent id should appear in the result")
}

// TestEnrichSemanticHitsOverSQLiteVarLimit forces the reader pool's SQLite
// bind-variable limit down to 999, then asks enrichSemanticHits to enrich
// 1002 (session_id, ordinal) hits — each binding 2 params in the VALUES CTE,
// so 2004 total, well past a single query's budget. It must chunk and still
// resolve exactly the hits with a real backing message/session row.
func TestEnrichSemanticHitsOverSQLiteVarLimit(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	forceReaderVarLimit(t, d, 999)

	overLimitPh, overLimitArgs := inPlaceholders(make([]string, 1001))
	_, probeErr := d.getReader().QueryContext(
		ctx, "SELECT 1 WHERE '' IN "+overLimitPh, overLimitArgs...)
	require.Error(t, probeErr, "reader variable limit was not constrained")

	insertSession(t, d, "real-sess", "proj")
	insertMessages(t, d,
		Message{
			SessionID: "real-sess", Ordinal: 0, Role: "user",
			Content: "hello there", ContentLength: len("hello there"),
			Timestamp: tsZero,
		},
		Message{
			SessionID: "real-sess", Ordinal: 1, Role: "assistant",
			Content: "hi back", ContentLength: len("hi back"),
			Timestamp: tsZeroS1,
		},
	)

	hits := []VectorHit{
		{SessionID: "real-sess", Ordinal: 0},
		{SessionID: "real-sess", Ordinal: 1},
	}
	for i := range 1000 {
		hits = append(hits, VectorHit{
			SessionID: fmt.Sprintf("no-such-session-%d", i), Ordinal: i,
		})
	}

	meta, err := d.enrichSemanticHits(ctx, hits)
	require.NoError(t, err)

	require.Len(t, meta, 2)
	assert.Equal(t, "hello there",
		meta[semanticHitKey{"real-sess", 0}].content)
	assert.Equal(t, "hi back",
		meta[semanticHitKey{"real-sess", 1}].content)
}
