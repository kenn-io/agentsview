package db

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBunSemanticSessionScopeOverSQLiteVarLimit forces the reader pool's
// SQLite bind-variable limit down to 999 (mirroring
// forceReaderVarLimit's rationale in activityreport_test.go: some builds
// compile against SQLite's older 999 default), then asks
// BunStore's semantic scope filter to handle 1002 candidate session IDs — a
// count a deep semantic overfetch (thousands of hits from distinct
// sessions) can plausibly produce and single-shot IN (...) query would
// exceed. It must chunk the query and still return exactly the real,
// filter-passing sessions.
func TestBunSemanticSessionScopeOverSQLiteVarLimit(t *testing.T) {
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

	hits := []ContentSearchHit{{SessionID: "real-1"}, {SessionID: "real-2"}}
	for i := range 1000 {
		hits = append(hits, ContentSearchHit{SessionID: fmt.Sprintf("fake-%d", i)})
	}

	f := ContentSearchFilter{IncludeOneShot: true, IncludeAutomated: true}
	allowed, err := d.filterContentHitsBySessionScope(
		ctx, d.bunReader, f, hits,
	)
	require.NoError(t, err)

	require.Len(t, allowed, 2, "no nonexistent id should appear in the result")
	assert.Equal(t, "real-1", allowed[0].SessionID)
	assert.Equal(t, "real-2", allowed[1].SessionID)
}

// TestBunHydrateSemanticHitsOverSQLiteVarLimit forces the reader pool's SQLite
// bind-variable limit down to 999, then asks BunStore to hydrate
// 1002 (session_id, ordinal) hits — each binding 2 params in the VALUES CTE,
// so 2004 total, well past a single query's budget. It must chunk and still
// resolve exactly the hits with a real backing message/session row.
func TestBunHydrateSemanticHitsOverSQLiteVarLimit(t *testing.T) {
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

	hits := []ContentSearchHit{
		{SessionID: "real-sess", Ordinal: 0, Location: "message"},
		{SessionID: "real-sess", Ordinal: 1, Location: "message"},
	}
	for i := range 1000 {
		hits = append(hits, ContentSearchHit{
			SessionID: fmt.Sprintf("no-such-session-%d", i), Ordinal: i,
			Location: "message",
		})
	}

	page, err := d.hydrateContentSearchHits(
		ctx, d.bunReader,
		ContentSearchFilter{Mode: "semantic", IncludeOneShot: true, Limit: 500},
		hits,
	)
	require.NoError(t, err)

	require.Len(t, page.Matches, 2)
	assert.Equal(t, "hello there", page.Matches[0].Snippet)
	assert.Equal(t, "hi back", page.Matches[1].Snippet)
}
