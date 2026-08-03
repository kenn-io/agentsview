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
		ddl := store.NewCreateTable().Model(table.Model).IfNotExists().String()
		for _, column := range criticalTableColumns[table.Name] {
			assert.Contains(t, ddl, `"`+column+`"`, "%s.%s", table.Name, column)
		}
		_, err := store.NewCreateTable().Model(table.Model).IfNotExists().Exec(t.Context())
		require.NoError(t, err, table.Name)
	}
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
