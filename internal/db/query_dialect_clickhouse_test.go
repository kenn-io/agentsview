package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClickHouseDialectRendersUncorrelatedFilters(t *testing.T) {
	filter := SessionFilter{
		Starred: true, IncludeChildren: true, IncludeOrphans: true,
		Project: "p", DateTo: "2026-06-30", Timezone: "UTC",
		AutomatedScope: "automated",
	}
	got, args := BuildSessionFilterSQL(filter, ClickHouseQueryDialect())
	normalized := normalizeSQL(got)

	assert.Contains(t, normalized, ") UNION ALL SELECT s.id FROM sessions s")
	assert.Contains(t, normalized, "AND root_session.is_automated = true")
	assert.NotContains(t, normalized, ") UNION SELECT")
	assert.Contains(t, normalized,
		"root_session.id IN (SELECT session_id FROM starred_sessions)")
	assert.NotContains(t, normalized, "EXISTS (")
	assert.Contains(t, normalized,
		"(root_session.parent_session_id IS NULL OR root_session.parent_session_id NOT IN (SELECT id FROM sessions))")
	assert.Contains(t, normalized,
		"COALESCE(root_session.started_at, root_session.created_at) < parseDateTime64BestEffort(?, 6, 'UTC')")
	assert.Equal(t, []any{"p", "2026-07-01T00:00:00Z"}, args)
}

func TestClickHouseDialectDateEndUsesLastMessageAt(t *testing.T) {
	got, _ := BuildSessionFilterSQL(SessionFilter{
		DateFrom: "2026-06-01", Timezone: "UTC",
	}, ClickHouseQueryDialect())
	assert.Contains(t, normalizeSQL(got),
		"COALESCE(ended_at, last_message_at, started_at, created_at) >= parseDateTime64BestEffort(?, 6, 'UTC')")
	assert.NotContains(t, got, "SELECT MAX(m.timestamp)")
}

func TestClickHouseDialectPredicatesAndCursorCasts(t *testing.T) {
	b := NewQueryBuilder(ClickHouseQueryDialect(), 0)
	assert.Equal(t, "content ILIKE ? ", b.ContainsPredicate("content", "a_b%"))
	assert.Equal(t, "match(content, concat('(?i)', ?))", b.RegexPredicate("content", "he+llo"))
	assert.Equal(t, []any{`%a\_b\%%`, "he+llo"}, b.Args())

	filter := SessionFilter{OrderBy: "recent,messages"}
	rs := ResolveSort(filter)
	require.Len(t, rs, 2)
	cursor := NewQueryBuilder(ClickHouseQueryDialect(), 0)
	pred := cursor.CursorPredicate(rs, filter, []any{"2026-06-08T12:00:00Z", int64(3)}, "sess-1")
	assert.Contains(t, pred,
		"COALESCE(ended_at, started_at, created_at) < parseDateTime64BestEffort(?, 6, 'UTC')")
	assert.Contains(t, pred, "message_count > toInt64(?)")
	assert.Equal(t,
		"ORDER BY COALESCE(ended_at, started_at, created_at) DESC, message_count ASC, id ASC",
		cursor.OrderByClause(rs, filter))
}
