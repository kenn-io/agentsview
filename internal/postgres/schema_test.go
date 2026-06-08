package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

type schemaProbeState struct {
	mu                  sync.Mutex
	informationQueries  int
	execs               []string
	alterTableExecs     []string
	currentSchema       string
	existingColumnNames map[string][]string
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
	switch {
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

func (s *schemaProbeState) executedSQL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.execs, "\n")
}

func TestEnsureSchemaBatchesColumnIntrospection(t *testing.T) {
	existing := map[string][]string{
		"sessions": {
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

func TestEnsureSchemaCreatesAnalyticsCoveringIndexes(t *testing.T) {
	db, state := newSchemaProbeDB(t, map[string][]string{
		"sessions": {
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
		"CREATE INDEX IF NOT EXISTS idx_messages_usage_timestamp")
}

func TestEnsureSchemaCreatesSessionTraversalIndex(t *testing.T) {
	db, state := newSchemaProbeDB(t, map[string][]string{
		"sessions": {
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
			"call_index",
		},
	})

	require.NoError(t, EnsureSchema(context.Background(), db, "agentsview"))

	// Two tables have missing columns (sessions: termination_status;
	// messages: source_parent_uuid, is_sidechain, is_compact_boundary,
	// thinking_text). Per-table batching means one ALTER each.
	assert.Equal(t, 2, state.alterTableExecCount(), "ALTER TABLE execs")
}
