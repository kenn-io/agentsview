package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resolvedFor builds the resolved sort terms for a spec string, for renderer
// tests that need the []ResolvedSort the stores pass to the query builder.
func resolvedFor(t *testing.T, spec string) []ResolvedSort {
	t.Helper()
	keys, err := ParseSortSpec(spec)
	require.NoError(t, err)
	return ResolveSort(SessionFilter{Sort: keys})
}

// TestOrderByClause_MultiKey locks the multi-column ORDER BY rendering,
// including the implicit id tie-breaker in the last term's direction.
func TestOrderByClause_MultiKey(t *testing.T) {
	rs := resolvedFor(t, "messages:asc,started:desc")

	b := newBunFilterArgs(sqliteTimestampOrderExpr)
	assert.Equal(t,
		"message_count ASC, "+
			"COALESCE(julianday(NULLIF(started_at, '')), "+
			"julianday(NULLIF(created_at, ''))) DESC, id DESC",
		bunOrderByClause(b, rs, SessionFilter{}))

	bpg := newBunFilterArgs(nil)
	assert.Equal(t,
		"message_count ASC, "+
			"COALESCE(started_at, created_at) DESC, id DESC",
		bunOrderByClause(bpg, rs, SessionFilter{}))
}

// TestOrderByClause_IDOnly keeps the single id-sort form free of a duplicate id
// tie-breaker.
func TestOrderByClause_IDOnly(t *testing.T) {
	rs := resolvedFor(t, "id:desc")
	b := newBunFilterArgs(sqliteTimestampOrderExpr)
	assert.Equal(t, "id DESC", bunOrderByClause(b, rs, SessionFilter{}))
}

// TestCursorPredicate_MultiKey locks the lexicographic OR-expansion that backs
// keyset pagination under mixed per-key directions, plus the per-kind casts.
func TestCursorPredicate_MultiKey(t *testing.T) {
	rs := resolvedFor(t, "messages:asc,started:desc")
	values := []any{int64(5), "2024-01-01T00:00:00Z"}

	b := newBunFilterArgs(sqliteTimestampOrderExpr)
	gotSQLite := bunCursorPredicate(b, rs, SessionFilter{}, values, "sid")
	assert.Equal(t,
		"((message_count > ?) OR "+
			"(message_count = ? AND "+
			"COALESCE(julianday(NULLIF(started_at, '')), "+
			"julianday(NULLIF(created_at, ''))) < julianday(NULLIF(?, ''))) OR "+
			"(message_count = ? AND "+
			"COALESCE(julianday(NULLIF(started_at, '')), "+
			"julianday(NULLIF(created_at, ''))) = julianday(NULLIF(?, '')) AND id < ?))",
		gotSQLite)
	// Six bound params: one comparison at level 0, two at level 1, three at
	// level 2 (the id tie-break being the last).
	assert.Len(t, b.values(), 6)

	bpg := newBunFilterArgs(nil)
	gotPG := bunCursorPredicate(bpg, rs, SessionFilter{}, values, "sid")
	assert.Equal(t,
		"((message_count > ?) OR "+
			"(message_count = ? AND "+
			"COALESCE(started_at, created_at) < ?) OR "+
			"(message_count = ? AND "+
			"COALESCE(started_at, created_at) = ? AND id < ?))",
		gotPG)
}

// TestCursorPredicate_SingleKeyEquivalentToRowValue documents that the
// single-key OR-expansion is the logical equivalent of the previous row-value
// comparison: value-compare OR (equal AND id-compare).
func TestCursorPredicate_SingleKeyRecent(t *testing.T) {
	rs := resolvedFor(t, "recent:desc")
	b := newBunFilterArgs(sqliteTimestampOrderExpr)
	got := bunCursorPredicate(
		b, rs, SessionFilter{}, []any{"2024-05-01T00:00:00Z"}, "sid",
	)
	activity := "COALESCE(julianday(NULLIF(ended_at, '')), " +
		"julianday(NULLIF(started_at, '')), julianday(NULLIF(created_at, '')))"
	parameter := "julianday(NULLIF(?, ''))"
	assert.Equal(t,
		"(("+activity+" < "+parameter+") OR ("+activity+" = "+parameter+
			" AND id < ?))",
		got)
}
