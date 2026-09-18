package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The dialect hooks added for ClickHouse must leave the row-store dialects
// rendering exactly what they rendered before the hooks existed. These
// strings were captured from the pre-hook builder.
func TestBuildSessionFilterSQLHooksKeepRowStoreDialectsUnchanged(t *testing.T) {
	filter := SessionFilter{
		Starred: true, IncludeChildren: true, IncludeOrphans: true, Project: "p",
	}
	const sqliteWant = "message_count > 0 AND deleted_at IS NULL AND id IN (WITH RECURSIVE tree(id) AS (SELECT root_session.id FROM sessions root_session WHERE root_session.message_count > 0 AND root_session.deleted_at IS NULL AND root_session.project = ? AND EXISTS (SELECT 1 FROM starred_sessions ss WHERE ss.session_id = root_session.id) AND (NOT (root_session.relationship_type IN ('subagent', 'fork', 'continuation')) OR (root_session.relationship_type IN ('subagent', 'fork', 'continuation') AND NOT EXISTS ( SELECT 1 FROM sessions parent WHERE parent.id = root_session.parent_session_id ))) UNION SELECT s.id FROM sessions s JOIN tree t ON s.parent_session_id = t.id WHERE s.message_count > 0 AND s.deleted_at IS NULL) SELECT id FROM tree)"
	const postgresWant = "message_count > 0 AND deleted_at IS NULL AND id IN (WITH RECURSIVE tree(id) AS (SELECT root_session.id FROM sessions root_session WHERE root_session.message_count > 0 AND root_session.deleted_at IS NULL AND root_session.project = $1 AND EXISTS (SELECT 1 FROM starred_sessions ss WHERE ss.session_id = root_session.id) AND (NOT (root_session.relationship_type IN ('subagent', 'fork', 'continuation')) OR (root_session.relationship_type IN ('subagent', 'fork', 'continuation') AND NOT EXISTS ( SELECT 1 FROM sessions parent WHERE parent.id = root_session.parent_session_id ))) UNION SELECT s.id FROM sessions s JOIN tree t ON s.parent_session_id = t.id WHERE s.message_count > 0 AND s.deleted_at IS NULL) SELECT id FROM tree)"

	tests := []struct {
		name    string
		dialect QueryDialect
		want    string
	}{
		{"sqlite", SQLiteQueryDialect(), sqliteWant},
		{"postgres", PostgresQueryDialect(), postgresWant},
		{"duckdb", DuckDBQueryDialect(), sqliteWant},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, args := BuildSessionFilterSQL(filter, tt.dialect)
			assert.Equal(t, tt.want, normalizeSQL(got))
			assert.Equal(t, []any{"p"}, args)
		})
	}
}

func TestClickHouseDialectRendersUncorrelatedFilters(t *testing.T) {
	filter := SessionFilter{
		Starred: true, IncludeChildren: true, IncludeOrphans: true,
		Project: "p", DateTo: "2026-06-30", Timezone: "UTC",
	}
	got, args := BuildSessionFilterSQL(filter, ClickHouseQueryDialect())
	normalized := normalizeSQL(got)

	assert.Contains(t, normalized, ") UNION ALL SELECT s.id FROM sessions s")
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
