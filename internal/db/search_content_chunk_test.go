package db

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBunSemanticSessionScopeChunksLargeCandidateSets verifies the owned
// chunking contract directly. Bun formats SQLite values before driver
// execution, so connection-local bind limits are not an observable proxy.
func TestBunSemanticSessionScopeChunksLargeCandidateSets(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	hook := new(countingQueryHook)
	store := d.bunReader.WithQueryHook(hook)

	insertSession(t, d, "real-1", "proj")
	insertSession(t, d, "real-2", "proj")

	hits := []ContentSearchHit{{SessionID: "real-1"}, {SessionID: "real-2"}}
	for i := range 1000 {
		hits = append(hits, ContentSearchHit{SessionID: fmt.Sprintf("fake-%d", i)})
	}

	f := ContentSearchFilter{IncludeOneShot: true, IncludeAutomated: true}
	allowed, err := d.filterContentHitsBySessionScope(
		ctx, store, f, hits,
	)
	require.NoError(t, err)

	require.Len(t, allowed, 2, "no nonexistent id should appear in the result")
	assert.Equal(t, "real-1", allowed[0].SessionID)
	assert.Equal(t, "real-2", allowed[1].SessionID)
	assert.Greater(t, hook.selects, 1, "candidate scope is queried in chunks")
}

// TestBunHydrateSemanticHitsChunksLargeCandidateSets verifies that 1002
// (session_id, ordinal) hits are resolved in multiple bounded queries.
func TestBunHydrateSemanticHitsChunksLargeCandidateSets(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	hook := new(countingQueryHook)
	store := d.bunReader.WithQueryHook(hook)

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
		ctx, store,
		ContentSearchFilter{Mode: "semantic", IncludeOneShot: true, Limit: 500},
		hits,
	)
	require.NoError(t, err)

	require.Len(t, page.Matches, 2)
	assert.Equal(t, "hello there", page.Matches[0].Snippet)
	assert.Equal(t, "hi back", page.Matches[1].Snippet)
	assert.Greater(t, hook.selects, 1, "semantic hits are hydrated in chunks")
}
