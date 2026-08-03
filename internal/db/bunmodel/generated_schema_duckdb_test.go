//go:build !(windows && arm64)

package bunmodel

import (
	"database/sql"
	"testing"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/duckdb/bundialect"
)

func TestCommonTablesGeneratedSchemaExecutesInDuckDB(t *testing.T) {
	raw, err := sql.Open("duckdb", "")
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	store := bun.NewDB(raw, bundialect.New())

	for _, table := range CommonTables() {
		ddl := registeredCreateTable(store, table, false).String()
		for _, column := range criticalTableColumns[table.Name] {
			assert.Contains(t, ddl, `"`+column+`"`, "%s.%s", table.Name, column)
		}
		_, err := registeredCreateTable(store, table, false).Exec(t.Context())
		require.NoError(t, err, table.Name)
		require.NoError(t, createRegisteredIndexes(t.Context(), store, table))
	}
}

func TestCommonTablesDuckDBAllowsSourceOptionalIDsAndDeduplicatesNonEmptyKeys(t *testing.T) {
	raw, err := sql.Open("duckdb", "")
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	store := bun.NewDB(raw, bundialect.New())
	for _, table := range CommonTables() {
		_, err := registeredCreateTable(store, table, false).Exec(t.Context())
		require.NoError(t, err, table.Name)
		require.NoError(t, createRegisteredIndexes(t.Context(), store, table))
	}

	_, err = raw.ExecContext(t.Context(), `
		INSERT INTO cursor_usage_events (occurred_at, model, dedup_key)
		VALUES
			('2026-08-02 12:00:00', 'model', ''),
			('2026-08-02 12:01:00', 'model', '');
		INSERT INTO secret_findings (
			session_id, rule_name, confidence, location_kind,
			message_ordinal, match_start, match_end, match_index,
			redacted_match, rules_version, created_at
		) VALUES (
			'session-1', 'rule', 'high', 'message', 1,
			0, 4, 0, '[REDACTED]', 'v1', '2026-08-02 12:02:00'
		);
	`)
	require.NoError(t, err, "DuckDB mirror IDs are source-optional")

	_, err = raw.ExecContext(t.Context(), `
		INSERT INTO cursor_usage_events (occurred_at, model, dedup_key)
		VALUES ('2026-08-02 12:03:00', 'model', 'same')`)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `
		INSERT INTO cursor_usage_events (occurred_at, model, dedup_key)
		VALUES ('2026-08-02 12:04:00', 'model', 'same')`)
	require.Error(t, err)

	_, err = raw.ExecContext(t.Context(), `
		INSERT INTO source_archives VALUES ('archive-1', 'salt-1');
		INSERT INTO sessions (
			id, project, machine, agent, created_at,
			source_archive_id, source_database_generation
		) VALUES (
			'session-1', 'project', 'machine', 'agent',
			'2026-08-02 12:00:00', 'archive-1', 'generation-1'
		);
		INSERT INTO secret_findings (
			session_id, rule_name, confidence, location_kind,
			message_ordinal, match_start, match_end, match_index,
			redacted_match, rules_version, created_at
		) VALUES (
			'session-1', 'rule', 'high', 'message', 1,
			0, 4, 0, '[REDACTED]', 'v1', '2026-08-02 12:02:00'
		);
	`)
	require.NoError(t, err)
}

func TestCommonTablesDuckDBPreservesNativeBooleansJSONAndOptionalMessageID(t *testing.T) {
	raw, err := sql.Open("duckdb", "")
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	store := bun.NewDB(raw, bundialect.New())
	_, err = store.NewCreateTable().Model((*Message)(nil)).Exec(t.Context())
	require.NoError(t, err)

	_, err = raw.ExecContext(t.Context(), `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			has_thinking, has_tool_use, is_system, token_usage
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"session-1", 8, "assistant", "done",
		"2026-08-02 16:30:00", true, false, true,
		`{"output_tokens":7}`,
	)
	require.NoError(t, err)

	var got Message
	require.NoError(t, store.NewSelect().Model(&got).
		Where("session_id = ? AND ordinal = ?", "session-1", 8).
		Scan(t.Context()))
	assert.Nil(t, got.ID)
	assert.True(t, got.HasThinking)
	assert.False(t, got.HasToolUse)
	assert.True(t, got.IsSystem)
	assert.JSONEq(t, `{"output_tokens":7}`, string(got.TokenUsage))
}
