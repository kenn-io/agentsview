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

	b := NewQueryBuilder(SQLiteQueryDialect(), 0)
	assert.Equal(t,
		"ORDER BY message_count ASC, "+
			"COALESCE(NULLIF(started_at, ''), created_at) DESC, id DESC",
		b.OrderByClause(rs, SessionFilter{}))

	bpg := NewQueryBuilder(PostgresQueryDialect(), 0)
	assert.Equal(t,
		"ORDER BY message_count ASC, "+
			"COALESCE(started_at, created_at) DESC, id DESC",
		bpg.OrderByClause(rs, SessionFilter{}))
}

// TestOrderByClause_IDOnly keeps the single id-sort form free of a duplicate id
// tie-breaker.
func TestOrderByClause_IDOnly(t *testing.T) {
	rs := resolvedFor(t, "id:desc")
	b := NewQueryBuilder(SQLiteQueryDialect(), 0)
	assert.Equal(t, "ORDER BY id DESC", b.OrderByClause(rs, SessionFilter{}))
}

// TestCursorPredicate_MultiKey locks the lexicographic OR-expansion that backs
// keyset pagination under mixed per-key directions, plus the per-kind casts.
func TestCursorPredicate_MultiKey(t *testing.T) {
	rs := resolvedFor(t, "messages:asc,started:desc")
	values := []any{int64(5), "2024-01-01T00:00:00Z"}

	b := NewQueryBuilder(SQLiteQueryDialect(), 0)
	gotSQLite := b.CursorPredicate(rs, SessionFilter{}, values, "sid")
	assert.Equal(t,
		"((message_count > ?) OR "+
			"(message_count = ? AND "+
			"COALESCE(NULLIF(started_at, ''), created_at) < ?) OR "+
			"(message_count = ? AND "+
			"COALESCE(NULLIF(started_at, ''), created_at) = ? AND id < ?))",
		gotSQLite)
	// Six bound params: one comparison at level 0, two at level 1, three at
	// level 2 (the id tie-break being the last).
	assert.Len(t, b.Args(), 6)

	bpg := NewQueryBuilder(PostgresQueryDialect(), 0)
	gotPG := bpg.CursorPredicate(rs, SessionFilter{}, values, "sid")
	assert.Equal(t,
		"((message_count > $1::bigint) OR "+
			"(message_count = $2::bigint AND "+
			"COALESCE(started_at, created_at) < $3::timestamptz) OR "+
			"(message_count = $4::bigint AND "+
			"COALESCE(started_at, created_at) = $5::timestamptz AND id < $6))",
		gotPG)
}

// TestCursorPredicate_SingleKeyEquivalentToRowValue documents that the
// single-key OR-expansion is the logical equivalent of the previous row-value
// comparison: value-compare OR (equal AND id-compare).
func TestCursorPredicate_SingleKeyRecent(t *testing.T) {
	rs := resolvedFor(t, "recent:desc")
	b := NewQueryBuilder(SQLiteQueryDialect(), 0)
	got := b.CursorPredicate(rs, SessionFilter{}, []any{"2024-05-01T00:00:00Z"}, "sid")
	activity := "COALESCE(NULLIF(ended_at, ''), NULLIF(started_at, ''), created_at)"
	assert.Equal(t,
		"(("+activity+" < ?) OR ("+activity+" = ? AND id < ?))",
		got)
}
