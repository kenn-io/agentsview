package bunmodel

import (
	"database/sql"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/schema"
)

var criticalTableColumns = map[string][]string{
	"sessions": {
		"id", "project", "agent", "started_at", "created_at",
		"source_archive_id", "source_database_generation",
	},
	"messages": {
		"id", "session_id", "ordinal", "token_usage", "timestamp",
	},
	"usage_events":        {"session_id", "source", "occurred_at"},
	"cursor_usage_events": {"occurred_at", "model", "is_headless"},
	"tool_calls":          {"session_id", "message_ordinal", "call_index"},
	"tool_result_events":  {"session_id", "tool_call_message_ordinal", "event_index"},
	"secret_findings":     {"session_id", "message_ordinal", "redacted_match"},
	"model_pricing":       {"model_pattern", "input_microdollars_per_mtok", "updated_at"},
	"model_pricing_bands": {"model_pattern", "above_input_tokens", "updated_at"},
	"starred_sessions":    {"session_id", "created_at"},
	"pinned_messages":     {"session_id", "ordinal", "source_uuid"},
	"excluded_sessions":   {"id", "created_at"},
	"session_aliases":     {"session_id", "alias_id"},
	"insights":            {"type", "content", "created_at"},
	"source_archives":     {"source_archive_id", "source_archive_salt"},
	"source_project_identity_observations": {
		"source_archive_id", "project", "observed_at", "key",
	},
	"source_session_project_identity_snapshots": {
		"source_archive_id", "source_database_generation",
		"source_session_id", "observed_at",
	},
	"source_worktree_project_mappings": {
		"source_archive_id", "machine", "path_prefix", "enabled",
	},
}

func TestCommonTablesContainCanonicalServingSchema(t *testing.T) {
	want := []string{
		"cursor_usage_events",
		"excluded_sessions",
		"insights",
		"messages",
		"model_pricing",
		"model_pricing_bands",
		"pinned_messages",
		"secret_findings",
		"session_aliases",
		"sessions",
		"source_archives",
		"source_project_identity_observations",
		"source_session_project_identity_snapshots",
		"source_worktree_project_mappings",
		"starred_sessions",
		"tool_calls",
		"tool_result_events",
		"usage_events",
	}

	got := make([]string, 0, len(CommonTables()))
	for _, table := range CommonTables() {
		got = append(got, table.Name)
	}
	sort.Strings(got)

	assert.Equal(t, want, got)
	assert.Subset(t, ModelColumns((*Session)(nil)), []string{
		"agent", "created_at", "deleted_at", "ended_at", "id", "machine",
		"message_count", "project", "source_archive_id",
		"source_database_generation", "started_at", "transcript_revision",
	})
}

func TestCommonTablesGenerateCanonicalMessageCompositeKey(t *testing.T) {
	for name, dialect := range map[string]schema.Dialect{
		"postgresql": pgdialect.New(),
		"sqlite":     sqlitedialect.New(),
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := sql.Open("sqlite3", ":memory:")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, raw.Close()) })
			store := bun.NewDB(raw, dialect)

			ddl := store.NewCreateTable().Model((*Message)(nil)).String()
			assert.Contains(t, ddl, `PRIMARY KEY ("session_id", "ordinal")`)
			assert.NotContains(t, ddl, `"id" INTEGER NOT NULL PRIMARY KEY`)
			assert.NotContains(t, ddl, `"id" BIGINT NOT NULL PRIMARY KEY`)
		})
	}
}

func TestCommonTablesGeneratedSchemaExecutesInSQLite(t *testing.T) {
	raw, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	store := bun.NewDB(raw, sqlitedialect.New())

	for _, table := range CommonTables() {
		ddl := store.NewCreateTable().Model(table.Model).IfNotExists().String()
		for _, column := range criticalTableColumns[table.Name] {
			assert.Contains(t, ddl, `"`+column+`"`, "%s.%s", table.Name, column)
		}
		_, err := store.NewCreateTable().Model(table.Model).IfNotExists().Exec(t.Context())
		require.NoError(t, err, table.Name)
	}
}

func TestCommonTablesGeneratedSchemaRendersForPostgreSQL(t *testing.T) {
	raw, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	store := bun.NewDB(raw, pgdialect.New())

	for _, table := range CommonTables() {
		ddl := store.NewCreateTable().Model(table.Model).IfNotExists().String()
		assert.True(t, strings.HasPrefix(ddl, "CREATE TABLE IF NOT EXISTS "))
		for _, column := range criticalTableColumns[table.Name] {
			assert.Contains(t, ddl, `"`+column+`"`, "%s.%s", table.Name, column)
		}
	}
}

func TestBunRowTimestampScannerNormalizesSupportedInputsToUTC(t *testing.T) {
	want := time.Date(2026, 8, 2, 16, 30, 0, 123_456_000, time.UTC)
	tests := map[string]any{
		"native time": time.Date(
			2026, 8, 2, 12, 30, 0, 123_456_000,
			time.FixedZone("EDT", -4*60*60),
		),
		"RFC3339 text":       "2026-08-02T12:30:00.123456-04:00",
		"SQLite text":        "2026-08-02 12:30:00.123456-04:00",
		"SQLite bytes":       []byte("2026-08-02 16:30:00.123456"),
		"millisecond Z text": "2026-08-02T16:30:00.123456Z",
	}

	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			var got Timestamp
			require.NoError(t, got.Scan(input))
			assert.Equal(t, want, got.Time)

			value, err := got.Value()
			require.NoError(t, err)
			assert.Equal(t, want, value)
		})
	}
}

func TestBunRowTimestampScannerPreservesDatabaseNull(t *testing.T) {
	var got Timestamp
	require.NoError(t, got.Scan(nil))
	assert.True(t, got.IsZero())
}

func TestBunRowSQLiteAliasesPreserveBooleansJSONAndOptionalMessageID(t *testing.T) {
	raw, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	store := bun.NewDB(raw, sqlitedialect.New())
	_, err = store.NewCreateTable().Model((*Message)(nil)).Exec(t.Context())
	require.NoError(t, err)

	_, err = raw.ExecContext(t.Context(), `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			has_thinking, has_tool_use, is_system, token_usage
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"session-1", 4, "assistant", "done",
		"2026-08-02 12:30:00-04:00", 1, 0, 1,
		`{"input_tokens":12}`,
	)
	require.NoError(t, err)

	var got Message
	require.NoError(t, store.NewSelect().Model(&got).
		Where("session_id = ? AND ordinal = ?", "session-1", 4).
		Scan(t.Context()))
	assert.Nil(t, got.ID)
	assert.True(t, got.HasThinking)
	assert.False(t, got.HasToolUse)
	assert.True(t, got.IsSystem)
	assert.JSONEq(t, `{"input_tokens":12}`, string(got.TokenUsage))
	require.NotNil(t, got.Timestamp)
	assert.Equal(t, time.Date(2026, 8, 2, 16, 30, 0, 0, time.UTC), got.Timestamp.Time)
}
