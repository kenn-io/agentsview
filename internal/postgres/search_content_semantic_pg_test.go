//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// semanticSearchSchema is an isolated schema for semantic-search tests.
const semanticSearchSchema = "agentsview_semantic_search_test"

// insertSemDoc inserts a vector_documents row, exposing the subordinate flag
// (insertSearchDoc hardcodes FALSE) so the fixture can seed a subordinate unit.
func insertSemDoc(
	t *testing.T, pg *sql.DB,
	docKey, sessionID string, ordinal, ordinalEnd int, subordinate bool,
	offsets, content string,
) {
	t.Helper()
	_, err := pg.Exec(`
INSERT INTO vector_documents (
    doc_key, session_id, source_uuid, ordinal, ordinal_end,
    subordinate, offsets, content, content_hash)
VALUES ($1, $2, '', $3, $4, $5, $6, $7, 'h')`,
		docKey, sessionID, ordinal, ordinalEnd, subordinate, offsets, content)
	require.NoError(t, err, "insert doc "+docKey)
}

// setupSemanticSearch creates a fresh schema with the base vector tables, a
// Store pointing at it, sessions/messages plus aligned vector docs+chunks, and
// returns the store, the generation id, and its chunk table. It skips when the
// pgvector extension is unavailable.
//
// The KNN fixture is scored against query [1,0,0,0]; cosine ranking best-first:
//
//	dsub  [1,0,0,0]     1.000  Ssub, subordinate (subagent child), user ord 0
//	d1    [1,0.2,0,0]   0.981  S1, top-level, user ord 0
//	d2    [1,0.5,0,0]   0.894  S2 (project beta), top-level, user ord 0
//	run1  [1,1,0,0]     0.707  S1, top-level run ord 10-12, anchor member B ord 12
//
// run1's chunk 0 is [0,0,1,0] (score 0) so chunk 1 wins the rollup and, with
// searchMaxInputChars=20, anchors to member B (ordinal 12, snippet runMemberB).
func setupSemanticSearch(t *testing.T) (*Store, int64, string) {
	t.Helper()
	pgURL := testPGURL(t)

	pg, err := Open(pgURL, semanticSearchSchema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()
	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + semanticSearchSchema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, semanticSearchSchema), "EnsureSchema")
	unavailable, err := ensureVectorBaseSchemaPG(ctx, pg)
	require.NoError(t, err, "ensureVectorBaseSchemaPG")
	if unavailable != "" {
		t.Skip(unavailable)
	}

	store, err := NewStore(pgURL, semanticSearchSchema, true)
	require.NoError(t, err, "NewStore")
	t.Cleanup(func() { store.Close() })

	genID, table := seedSemanticFixture(t, store)
	return store, genID, table
}

// seedSemanticFixture inserts the sessions, messages, vector documents, and
// chunks described on setupSemanticSearch, returning the generation id and its
// chunk table.
func seedSemanticFixture(t *testing.T, store *Store) (int64, string) {
	t.Helper()
	ctx := context.Background()
	pg := store.DB()

	const start, end = "2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z"
	insertCSSession(t, store, "S1", "alpha", "claude", start, end)
	insertCSSession(t, store, "S2", "beta", "claude", start, end)
	insertCSChildSession(t, store, "Ssub", "alpha", "claude", "S1", start, end)

	insertCSUnitMessage(t, store, "S1", 0, "user", "hello alpha content", false, false)
	insertCSUnitMessage(t, store, "S1", 10, "assistant", runMemberA, false, false)
	insertCSUnitMessage(t, store, "S1", 12, "assistant", runMemberB, false, false)
	insertCSUnitMessage(t, store, "S2", 0, "user", "beta another content", false, false)
	insertCSUnitMessage(t, store, "Ssub", 0, "assistant", "subordinate hit content", false, false)

	insertSemDoc(t, pg, "d1", "S1", 0, 0, false, "[]", "hello alpha content")
	insertSemDoc(t, pg, "d2", "S2", 0, 0, false, "[]", "beta another content")
	insertSemDoc(t, pg, "run1", "S1", 10, 12, false, runOffsets, runContent)
	insertSemDoc(t, pg, "dsub", "Ssub", 0, 0, true, "[]", "subordinate hit content")

	genID, _, err := ensureVectorGeneration(ctx, pg, "fp-sem", "m", 4)
	require.NoError(t, err, "ensureVectorGeneration")
	require.NoError(t, ensureVectorChunkTable(ctx, pg, genID, 4), "ensureVectorChunkTable")
	extSchema, err := vectorExtensionSchema(ctx, pg)
	require.NoError(t, err, "vectorExtensionSchema")
	halfvec := extSchema + ".halfvec"
	table := vectorChunkTable(genID)

	insertSearchChunk(t, pg, table, halfvec, "dsub", 0, []float32{1, 0, 0, 0})
	insertSearchChunk(t, pg, table, halfvec, "d1", 0, []float32{1, 0.2, 0, 0})
	insertSearchChunk(t, pg, table, halfvec, "d2", 0, []float32{1, 0.5, 0, 0})
	insertSearchChunk(t, pg, table, halfvec, "run1", 0, []float32{0, 0, 1, 0})
	insertSearchChunk(t, pg, table, halfvec, "run1", 1, []float32{1, 1, 0, 0})

	return genID, table
}

// wireSemanticSearcher wires a fixed-query searcher over the fixture and
// returns the store for a semantic SearchContent call.
func wireSemanticSearcher(t *testing.T, store *Store, genID int64) {
	t.Helper()
	store.SetVectorSearcher(NewVectorSearcher(
		store.DB(), genID, 4, searchMaxInputChars, fixedEncoder([]float32{1, 0, 0, 0})))
}

// semKey identifies a match by session and anchor ordinal (S1 contributes two
// anchors, so ordinal alone is not unique).
type semKey struct {
	session string
	ordinal int
}

func semMatchesByKey(
	t *testing.T, page db.ContentSearchPage,
) map[semKey]db.ContentMatch {
	t.Helper()
	out := make(map[semKey]db.ContentMatch, len(page.Matches))
	for _, m := range page.Matches {
		k := semKey{m.SessionID, m.Ordinal}
		_, dup := out[k]
		require.False(t, dup, "duplicate match %+v", k)
		out[k] = m
	}
	return out
}

// TestPGSemanticSearchRankedResults pins the base semantic path: the searcher's
// ranked hits survive the session-scope filter, the one-leg subordinate penalty
// reorders them, snippets come from message content, Location is "message", and
// a run hit carries the unit's ordinal range with the anchor member's snippet.
func TestPGSemanticSearchRankedResults(t *testing.T) {
	store, genID, _ := setupSemanticSearch(t)
	wireSemanticSearcher(t, store, genID)

	page, err := store.SearchContent(context.Background(), db.ContentSearchFilter{
		Pattern: "content", Mode: "semantic", Limit: 50,
	})
	require.NoError(t, err, "SearchContent semantic")
	require.Len(t, page.Matches, 4, "matches")

	// Penalty reorders the searcher order [dsub, d1, d2, run1] to
	// [d1, d2, run1, dsub]: the subordinate unit drops last.
	want := []semKey{{"S1", 0}, {"S2", 0}, {"S1", 12}, {"Ssub", 0}}
	for i, k := range want {
		assert.Equal(t, k.session, page.Matches[i].SessionID, "pos %d session", i)
		assert.Equal(t, k.ordinal, page.Matches[i].Ordinal, "pos %d ordinal", i)
		assert.Equal(t, "message", page.Matches[i].Location, "pos %d location", i)
		require.NotNil(t, page.Matches[i].Score, "pos %d score", i)
	}

	byKey := semMatchesByKey(t, page)
	run := byKey[semKey{"S1", 12}]
	assert.Equal(t, [2]int{10, 12}, run.OrdinalRange, "run hit spans the unit")
	assert.Equal(t, runMemberB, run.Snippet, "member-local snippet from message content")
	assert.False(t, run.Subordinate, "top-level run")
	runTS, err := time.Parse(time.RFC3339Nano, run.Timestamp)
	require.NoError(t, err, "run timestamp must be RFC3339Nano, got %q", run.Timestamp)
	assert.True(t, time.Date(2026, 5, 1, 10, 0, 12, 0, time.UTC).Equal(runTS),
		"run timestamp must equal the anchor member's inserted instant, got %v", runTS)

	d1 := byKey[semKey{"S1", 0}]
	assert.Equal(t, "alpha", d1.Project, "project from sessions join")
	assert.Contains(t, d1.Snippet, "hello alpha content", "snippet from message content")
	assert.InDelta(t, 0.981, *d1.Score, 0.01, "keeps the searcher's cosine score")
	d1TS, err := time.Parse(time.RFC3339Nano, d1.Timestamp)
	require.NoError(t, err, "d1 timestamp must be RFC3339Nano, got %q", d1.Timestamp)
	assert.True(t, time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC).Equal(d1TS),
		"d1 timestamp must equal the inserted instant, got %v", d1TS)

	sub := byKey[semKey{"Ssub", 0}]
	assert.True(t, sub.Subordinate, "subagent unit is subordinate")
	assert.Equal(t, "subagent", sub.Relationship, "lineage from sessions join")
	assert.Equal(t, "S1", sub.ParentSessionID, "parent from sessions join")
}

// TestPGSemanticSearchProjectFilter pins that a project filter drops sessions
// whose metadata does not match, before the ranking is assembled.
func TestPGSemanticSearchProjectFilter(t *testing.T) {
	store, genID, _ := setupSemanticSearch(t)
	wireSemanticSearcher(t, store, genID)

	page, err := store.SearchContent(context.Background(), db.ContentSearchFilter{
		Pattern: "content", Mode: "semantic", Limit: 50, Project: "alpha",
	})
	require.NoError(t, err, "SearchContent semantic")
	require.Len(t, page.Matches, 3, "beta session (S2) excluded")
	for _, m := range page.Matches {
		assert.NotEqual(t, "S2", m.SessionID, "non-matching project excluded")
		assert.Equal(t, "alpha", m.Project, "surviving projects are alpha")
	}
}

// TestPGSemanticSearchScope pins Scope: "top" drops subordinate units and
// "subordinate" keeps only them, applied before the penalty and the limit.
func TestPGSemanticSearchScope(t *testing.T) {
	store, genID, _ := setupSemanticSearch(t)
	wireSemanticSearcher(t, store, genID)
	ctx := context.Background()

	top, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "content", Mode: "semantic", Limit: 50, Scope: "top",
	})
	require.NoError(t, err, "SearchContent scope=top")
	require.Len(t, top.Matches, 3, "subordinate unit dropped")
	for _, m := range top.Matches {
		assert.False(t, m.Subordinate, "scope=top keeps only top-level units")
		assert.NotEqual(t, "Ssub", m.SessionID, "subordinate session excluded")
	}

	sub, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "content", Mode: "semantic", Limit: 50, Scope: "subordinate",
	})
	require.NoError(t, err, "SearchContent scope=subordinate")
	require.Len(t, sub.Matches, 1, "only the subordinate unit survives")
	assert.Equal(t, "Ssub", sub.Matches[0].SessionID)
	assert.True(t, sub.Matches[0].Subordinate)
}

// TestPGSemanticSearchSubordinatePenaltyReorders pins the one-leg RRF fusion:
// the subordinate unit ranks first by cosine score yet lands last after the
// penalty, while every match keeps the searcher's own score (not the fusion
// score).
func TestPGSemanticSearchSubordinatePenaltyReorders(t *testing.T) {
	store, genID, _ := setupSemanticSearch(t)
	wireSemanticSearcher(t, store, genID)

	page, err := store.SearchContent(context.Background(), db.ContentSearchFilter{
		Pattern: "content", Mode: "semantic", Limit: 50,
	})
	require.NoError(t, err, "SearchContent semantic")
	require.Len(t, page.Matches, 4, "matches")

	first := page.Matches[0]
	last := page.Matches[len(page.Matches)-1]
	assert.Equal(t, "S1", first.SessionID, "top-level unit overtakes the subordinate one")
	assert.Equal(t, "Ssub", last.SessionID, "subordinate unit demoted to last")
	require.NotNil(t, first.Score)
	require.NotNil(t, last.Score)
	assert.Greater(t, *last.Score, *first.Score,
		"subordinate kept its higher cosine score despite ranking last")
	assert.InDelta(t, 1.0, *last.Score, 0.01, "subordinate keeps the searcher's score")
}

// TestPGSemanticSearchNoSearcherUnavailable pins that a store with no searcher
// wired returns db.ErrSemanticUnavailable carrying the configured reason.
func TestPGSemanticSearchNoSearcherUnavailable(t *testing.T) {
	store := setupContentSearch(t)
	store.SetSemanticUnavailableReason("no embeddings generation built")

	_, err := store.SearchContent(context.Background(), db.ContentSearchFilter{
		Pattern: "hello", Mode: "semantic", Limit: 50,
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, db.ErrSemanticUnavailable),
		"want ErrSemanticUnavailable, got %v", err)
	assert.Contains(t, err.Error(), "no embeddings generation built",
		"reason surfaced to the caller")
}

// TestPGSemanticSearchInvalidInput pins backend parity: an invalid semantic
// request (cursor pagination or a non-messages source) returns a
// *db.SearchInputError, validated before the capability gate even with a
// searcher wired.
func TestPGSemanticSearchInvalidInput(t *testing.T) {
	store, genID, _ := setupSemanticSearch(t)
	wireSemanticSearcher(t, store, genID)
	ctx := context.Background()

	cases := map[string]db.ContentSearchFilter{
		"cursor": {Pattern: "x", Mode: "semantic", Cursor: 1, Limit: 50},
		"source": {
			Pattern: "x", Mode: "semantic", Sources: []string{"tool_input"}, Limit: 50,
		},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := store.SearchContent(ctx, f)
			require.Error(t, err)
			var inputErr *db.SearchInputError
			assert.True(t, errors.As(err, &inputErr),
				"want *db.SearchInputError, got %T: %v", err, err)
			assert.False(t, errors.Is(err, db.ErrSemanticUnavailable),
				"invalid input must not be masked as ErrSemanticUnavailable")
			assert.NotEmpty(t, strings.TrimSpace(inputErr.Error()))
		})
	}
}
