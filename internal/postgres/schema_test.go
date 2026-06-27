package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	localdb "go.kenn.io/agentsview/internal/db"
)

type schemaProbeDriver struct{}

type schemaProbeConn struct {
	state *schemaProbeState
}

type schemaProbeRows struct {
	columns []string
	values  [][]driver.Value
	next    int
}

type schemaProbeQueryError struct {
	contains string
	err      error
}

type schemaProbeState struct {
	mu                  sync.Mutex
	informationQueries  int
	execs               []string
	alterTableExecs     []string
	currentSchema       string
	existingColumnNames map[string][]string
	existingTables      map[string]bool
	existingIndexes     map[string]bool
	maxDataVersion      int
	maxDataVersionErr   error
	queryErrors         []schemaProbeQueryError
}

var (
	schemaProbeRegisterOnce sync.Once
	schemaProbeStatesMu     sync.Mutex
	schemaProbeStates       = map[string]*schemaProbeState{}
)

func registerSchemaProbeDriver() {
	schemaProbeRegisterOnce.Do(func() {
		sql.Register("agentsview_schema_probe", schemaProbeDriver{})
	})
}

func newSchemaProbeDB(
	t *testing.T,
	existing map[string][]string,
) (*sql.DB, *schemaProbeState) {
	t.Helper()
	registerSchemaProbeDriver()

	state := &schemaProbeState{
		currentSchema:       "agentsview",
		existingColumnNames: existing,
	}
	name := t.Name()

	schemaProbeStatesMu.Lock()
	schemaProbeStates[name] = state
	schemaProbeStatesMu.Unlock()
	t.Cleanup(func() {
		schemaProbeStatesMu.Lock()
		delete(schemaProbeStates, name)
		schemaProbeStatesMu.Unlock()
	})

	db, err := sql.Open("agentsview_schema_probe", name)
	require.NoError(t, err, "open fake schema probe db")
	t.Cleanup(func() { db.Close() })
	return db, state
}

func (schemaProbeDriver) Open(name string) (driver.Conn, error) {
	schemaProbeStatesMu.Lock()
	state := schemaProbeStates[name]
	schemaProbeStatesMu.Unlock()
	return &schemaProbeConn{state: state}, nil
}

func (c *schemaProbeConn) Prepare(string) (driver.Stmt, error) {
	return nil, driver.ErrSkip
}

func (c *schemaProbeConn) Close() error { return nil }

func (c *schemaProbeConn) Begin() (driver.Tx, error) {
	return nil, driver.ErrSkip
}

func (c *schemaProbeConn) ExecContext(
	_ context.Context, query string, _ []driver.NamedValue,
) (driver.Result, error) {
	c.state.mu.Lock()
	c.state.execs = append(c.state.execs, query)
	c.state.mu.Unlock()
	if strings.Contains(strings.ToLower(query), "alter table") {
		c.state.mu.Lock()
		c.state.alterTableExecs = append(
			c.state.alterTableExecs, query,
		)
		c.state.mu.Unlock()
	}
	return driver.RowsAffected(0), nil
}

func (c *schemaProbeConn) QueryContext(
	_ context.Context, query string, args []driver.NamedValue,
) (driver.Rows, error) {
	normalized := strings.ToLower(query)
	for _, queryErr := range c.state.queryErrors {
		if strings.Contains(
			normalized,
			strings.ToLower(queryErr.contains),
		) {
			return nil, queryErr.err
		}
	}
	switch {
	case strings.Contains(normalized, "information_schema.tables"):
		name := ""
		if len(args) > 0 {
			if v, ok := args[0].Value.(string); ok {
				name = v
			}
		}
		if c.state.existingTables[name] {
			return &schemaProbeRows{
				columns: []string{"exists"},
				values:  [][]driver.Value{{int64(1)}},
			}, nil
		}
		return &schemaProbeRows{columns: []string{"exists"}}, nil
	case strings.Contains(normalized, "pg_indexes"):
		name := ""
		if len(args) > 0 {
			if v, ok := args[0].Value.(string); ok {
				name = v
			}
		}
		if c.state.existingIndexes[name] {
			return &schemaProbeRows{
				columns: []string{"exists"},
				values:  [][]driver.Value{{int64(1)}},
			}, nil
		}
		return &schemaProbeRows{columns: []string{"exists"}}, nil
	case strings.Contains(normalized, "information_schema.columns"):
		c.state.mu.Lock()
		c.state.informationQueries++
		c.state.mu.Unlock()
		if strings.Contains(normalized, "select exists") {
			return &schemaProbeRows{
				columns: []string{"exists"},
				values:  [][]driver.Value{{true}},
			}, nil
		}
		var values [][]driver.Value
		for table, columns := range c.state.existingColumnNames {
			for _, column := range columns {
				values = append(values, []driver.Value{
					table, column,
				})
			}
		}
		return &schemaProbeRows{
			columns: []string{"table_name", "column_name"},
			values:  values,
		}, nil
	case strings.Contains(normalized, "select value from sync_metadata"):
		return &schemaProbeRows{
			columns: []string{"value"},
		}, nil
	case strings.Contains(normalized, "max(data_version)"):
		if c.state.maxDataVersionErr != nil {
			return nil, c.state.maxDataVersionErr
		}
		return &schemaProbeRows{
			columns: []string{"max"},
			values:  [][]driver.Value{{int64(c.state.maxDataVersion)}},
		}, nil
	case strings.Contains(normalized, "select id, first_message"):
		return &schemaProbeRows{
			columns: []string{
				"id", "first_message",
				"user_message_count", "is_automated",
			},
		}, nil
	case strings.Contains(normalized, "select exists") &&
		strings.Contains(normalized, "from sync_metadata"):
		return &schemaProbeRows{
			columns: []string{"exists"},
			values:  [][]driver.Value{{true}},
		}, nil
	case strings.Contains(normalized, "select exists"):
		return &schemaProbeRows{
			columns: []string{"exists"},
			values:  [][]driver.Value{{true}},
		}, nil
	default:
		return &schemaProbeRows{columns: []string{"empty"}}, nil
	}
}

func (r *schemaProbeRows) Columns() []string { return r.columns }

func (r *schemaProbeRows) Close() error { return nil }

func (r *schemaProbeRows) Next(dest []driver.Value) error {
	if r.next >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.next])
	r.next++
	return nil
}

func (s *schemaProbeState) informationQueryCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.informationQueries
}

func (s *schemaProbeState) alterTableExecCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.alterTableExecs)
}

func (s *schemaProbeState) execCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.execs)
}

func (s *schemaProbeState) executedSQL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.execs, "\n")
}

func TestEnsureSchemaBatchesColumnIntrospection(t *testing.T) {
	existing := map[string][]string{
		"sessions": {
			"owner_marker",
			"created_at", "deleted_at",
			"total_output_tokens", "peak_context_tokens",
			"has_total_output_tokens",
			"has_peak_context_tokens", "is_automated",
			"tool_failure_signal_count", "tool_retry_count",
			"edit_churn_count", "consecutive_failure_max",
			"outcome", "outcome_confidence",
			"ended_with_role", "final_failure_streak",
			"signals_pending_since", "compaction_count",
			"mid_task_compaction_count",
			"context_pressure_max", "health_score",
			"health_grade", "has_tool_calls",
			"has_context_data", "data_version", "cwd",
			"quality_signal_version", "short_prompt_count",
			"unstructured_start",
			"missing_success_criteria_count",
			"missing_verification_count",
			"duplicate_prompt_count", "no_code_context_count",
			"runaway_tool_loop_count",
			"git_branch", "source_session_id",
			"source_version", "parser_malformed_lines",
			"is_truncated",
		},
		"messages": {
			"model", "token_usage", "context_tokens",
			"output_tokens", "has_context_tokens",
			"has_output_tokens", "claude_message_id",
			"claude_request_id", "source_type",
			"source_subtype", "source_uuid",
			"source_parent_uuid", "is_sidechain",
			"is_compact_boundary", "thinking_text",
		},
		"tool_calls": {
			"call_index",
		},
	}
	db, state := newSchemaProbeDB(t, existing)

	require.NoError(t, EnsureSchema(context.Background(), db, "agentsview"))

	assert.Equal(t, 1, state.informationQueryCount(),
		"information_schema.columns queries")
}

func TestCheckDataVersionCompatRejectsNewerPGRows(t *testing.T) {
	pg, state := newSchemaProbeDB(t, nil)
	state.maxDataVersion = localdb.CurrentDataVersion() + 10

	err := CheckDataVersionCompat(context.Background(), pg)

	require.Error(t, err, "newer PG data version must be rejected")
	assert.True(t, localdb.IsDataVersionTooNew(err),
		"expected too-new data version error")
}

func TestCheckDataVersionCompatAllowsMissingDataVersionColumn(t *testing.T) {
	pg, state := newSchemaProbeDB(t, nil)
	state.maxDataVersionErr = errors.New(
		`ERROR: column "data_version" does not exist (SQLSTATE 42703)`,
	)

	err := CheckDataVersionCompat(context.Background(), pg)

	require.NoError(t, err,
		"legacy PG schemas without sessions.data_version should migrate")
}

func TestEnsureSchemaChecksDataVersionBeforeDDL(t *testing.T) {
	pg, state := newSchemaProbeDB(t, nil)
	state.maxDataVersion = localdb.CurrentDataVersion() + 10

	err := EnsureSchema(context.Background(), pg, "agentsview")

	require.Error(t, err, "newer PG data version must be rejected")
	assert.True(t, localdb.IsDataVersionTooNew(err),
		"expected too-new data version error")
	assert.Equal(t, 0, state.execCount(),
		"EnsureSchema must not mutate PG before data-version refusal")
}

func TestSyncEnsureSchemaSkipsDDLWhenSchemaCompatible(t *testing.T) {
	pg, state := newSchemaProbeDB(t, nil)
	state.existingTables = map[string]bool{
		"model_pricing":       true,
		"cursor_usage_events": true,
	}
	state.existingIndexes = map[string]bool{
		"idx_cursor_usage_events_dedup": true,
	}
	syncer := &Sync{pg: pg, schema: "agentsview"}

	require.NoError(t, syncer.EnsureSchema(context.Background()))

	executed := strings.ToLower(state.executedSQL())
	assert.NotContains(t, executed, "create index",
		"compatible PG schema must skip index DDL")
	assert.NotContains(t, executed, "alter index",
		"compatible PG schema must skip index DDL")
	assert.NotContains(t, executed, "create table",
		"compatible PG schema must skip table DDL")
	assert.Equal(t, 0, state.alterTableExecCount(),
		"compatible PG schema must not run column migrations")
	assert.Contains(t, executed, "insert into sync_metadata",
		"compatible PG schema must still run row-level data repairs")
}

func TestCheckSchemaCompatIgnoresPushOnlySchema(t *testing.T) {
	pg, state := newSchemaProbeDB(t, nil)
	state.queryErrors = []schemaProbeQueryError{
		{contains: "owner_marker", err: errors.New(
			`ERROR: column "owner_marker" does not exist (SQLSTATE 42703)`)},
		{contains: "from sync_metadata", err: errors.New(
			`ERROR: relation "sync_metadata" does not exist (SQLSTATE 42P01)`)},
	}

	require.NoError(t, CheckSchemaCompat(context.Background(), pg),
		"read compatibility must not require push-only schema")
}

func TestSyncEnsureSchemaRunsDDLWhenPushMetadataMissing(t *testing.T) {
	pg, state := newSchemaProbeDB(t, map[string][]string{
		"sessions": {
			"has_total_output_tokens",
			"has_peak_context_tokens",
		},
		"messages": {
			"has_context_tokens",
			"has_output_tokens",
		},
	})
	state.existingTables = map[string]bool{
		"model_pricing":       true,
		"cursor_usage_events": true,
	}
	state.existingIndexes = map[string]bool{
		"idx_cursor_usage_events_dedup": true,
	}
	// Read-compatible with tables and index present, but the push-only
	// owner_marker column is absent, so the push fast path must fall back
	// to EnsureSchema.
	state.queryErrors = []schemaProbeQueryError{{
		contains: "owner_marker",
		err: errors.New(
			`ERROR: column "owner_marker" does not exist (SQLSTATE 42703)`),
	}}
	syncer := &Sync{pg: pg, schema: "agentsview"}

	require.NoError(t, syncer.EnsureSchema(context.Background()))

	assert.Greater(t, state.execCount(), 0,
		"missing push-only column must fall back to migration DDL")
}

func TestSyncEnsureSchemaRunsDDLWhenPushTableMissing(t *testing.T) {
	pg, state := newSchemaProbeDB(t, map[string][]string{
		"sessions": {
			"has_total_output_tokens",
			"has_peak_context_tokens",
		},
		"messages": {
			"has_context_tokens",
			"has_output_tokens",
		},
	})
	// Read-compatible, and cursor_usage_events present, but model_pricing
	// absent: the read probe passes yet a push would fail on model_pricing,
	// so the fast path must fall back to EnsureSchema.
	state.existingTables = map[string]bool{
		"cursor_usage_events": true,
	}
	syncer := &Sync{pg: pg, schema: "agentsview"}

	require.NoError(t, syncer.EnsureSchema(context.Background()))

	assert.Greater(t, state.execCount(), 0,
		"missing push-written table must fall back to migration DDL")
	assert.Contains(t, strings.ToLower(state.executedSQL()),
		"create table",
		"fallback must create missing push tables")
}

func TestSyncEnsureSchemaRunsDDLWhenDedupIndexMissing(t *testing.T) {
	pg, state := newSchemaProbeDB(t, map[string][]string{
		"sessions": {
			"has_total_output_tokens",
			"has_peak_context_tokens",
		},
		"messages": {
			"has_context_tokens",
			"has_output_tokens",
		},
	})
	// Tables present but the cursor dedup unique index is absent, so the
	// read probe passes yet ON CONFLICT DO NOTHING would not dedup cursor
	// usage rows. The fast path must fall back to EnsureSchema.
	state.existingTables = map[string]bool{
		"model_pricing":       true,
		"cursor_usage_events": true,
	}
	syncer := &Sync{pg: pg, schema: "agentsview"}

	require.NoError(t, syncer.EnsureSchema(context.Background()))

	assert.Greater(t, state.execCount(), 0,
		"missing dedup index must fall back to migration DDL")
	assert.Contains(t, strings.ToLower(state.executedSQL()),
		"idx_cursor_usage_events_dedup",
		"fallback must recreate the cursor dedup index")
}

func TestSyncEnsureSchemaRunsDDLWhenSchemaIncompatible(t *testing.T) {
	pg, state := newSchemaProbeDB(t, map[string][]string{
		"sessions": {
			"has_total_output_tokens",
			"has_peak_context_tokens",
		},
		"messages": {
			"has_context_tokens",
			"has_output_tokens",
		},
	})
	state.queryErrors = []schemaProbeQueryError{{
		contains: "data_version",
		err: errors.New(
			`ERROR: column "data_version" does not exist (SQLSTATE 42703)`,
		),
	}}
	syncer := &Sync{pg: pg, schema: "agentsview"}

	require.NoError(t, syncer.EnsureSchema(context.Background()))

	assert.Greater(t, state.execCount(), 0,
		"incompatible PG schema should fall back to migration DDL")
}

func TestEnsureSchemaCreatesAnalyticsCoveringIndexes(t *testing.T) {
	db, state := newSchemaProbeDB(t, map[string][]string{
		"sessions": {
			"owner_marker",
			"total_output_tokens", "peak_context_tokens",
			"has_total_output_tokens",
			"has_peak_context_tokens",
		},
		"messages": {
			"context_tokens", "output_tokens",
			"has_context_tokens", "has_output_tokens",
		},
		"tool_calls": {},
	})

	require.NoError(t, EnsureSchema(context.Background(), db, "agentsview"))

	sql := state.executedSQL()
	assert.Contains(t, sql,
		"CREATE INDEX IF NOT EXISTS idx_tool_calls_session_category")
	assert.Contains(t, sql,
		"CREATE INDEX IF NOT EXISTS idx_messages_velocity")
	assert.Contains(t, sql,
		"CREATE INDEX IF NOT EXISTS idx_messages_usage_covering")
	assert.Contains(t, sql,
		"DROP INDEX IF EXISTS idx_messages_usage_timestamp")
}

func TestEnsureSchemaCreatesSessionTraversalIndex(t *testing.T) {
	db, state := newSchemaProbeDB(t, map[string][]string{
		"sessions": {
			"owner_marker",
			"total_output_tokens", "peak_context_tokens",
			"has_total_output_tokens",
			"has_peak_context_tokens",
		},
		"messages": {
			"context_tokens", "output_tokens",
			"has_context_tokens", "has_output_tokens",
		},
		"tool_calls": {},
	})

	require.NoError(t, EnsureSchema(context.Background(), db, "agentsview"))

	assert.Contains(t, state.executedSQL(),
		"CREATE INDEX IF NOT EXISTS idx_sessions_parent")
}

func TestEnsureSchemaGroupsMissingColumnMigrationsByTable(t *testing.T) {
	db, state := newSchemaProbeDB(t, map[string][]string{
		"sessions": {
			"owner_marker",
			"created_at", "deleted_at",
			"total_output_tokens", "peak_context_tokens",
			"has_total_output_tokens",
			"has_peak_context_tokens", "is_automated",
			"tool_failure_signal_count", "tool_retry_count",
			"edit_churn_count", "consecutive_failure_max",
			"outcome", "outcome_confidence",
			"ended_with_role", "final_failure_streak",
			"signals_pending_since", "compaction_count",
			"mid_task_compaction_count",
			"context_pressure_max", "health_score",
			"health_grade", "has_tool_calls",
			"has_context_data", "data_version", "cwd",
			"quality_signal_version", "short_prompt_count",
			"unstructured_start",
			"missing_success_criteria_count",
			"missing_verification_count",
			"duplicate_prompt_count", "no_code_context_count",
			"runaway_tool_loop_count",
			"git_branch", "source_session_id",
			"source_version", "parser_malformed_lines",
			"is_truncated",
		},
		"messages": {
			"model", "token_usage", "context_tokens",
			"output_tokens", "has_context_tokens",
			"has_output_tokens", "claude_message_id",
			"claude_request_id", "source_type",
			"source_subtype", "source_uuid",
		},
		"tool_calls": {
			"call_index", "file_path",
		},
	})

	require.NoError(t, EnsureSchema(context.Background(), db, "agentsview"))

	// Two tables have missing columns (sessions: termination_status;
	// messages: source_parent_uuid, is_sidechain, is_compact_boundary,
	// thinking_text). Per-table batching means one ALTER each. tool_calls
	// lists all its migration columns (call_index, file_path) as present, so
	// it contributes no ALTER.
	assert.Equal(t, 2, state.alterTableExecCount(), "ALTER TABLE execs")
}
