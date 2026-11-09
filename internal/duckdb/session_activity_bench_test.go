//go:build !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Session activity benchmark archive: 20,000 sessions of 50 messages, except
// the first, which holds sessionActivityBenchLong messages. Every assistant
// message carries thinking text, token usage, and one tool call whose input
// and result follow a coding session's mix of short commands and multi-KB
// edits, writes, and file reads.
const (
	sessionActivityBenchSessions = 20_000
	sessionActivityBenchMessages = 50
	sessionActivityBenchLong     = 4_000
)

func seedSessionActivityBenchStore(b *testing.B) *Store {
	b.Helper()
	ctx := b.Context()
	conn, err := openDuckDB(filepath.Join(b.TempDir(), "session-activity-bench.duckdb"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, conn.Close()) })
	require.NoError(b, configureDuckDBThreads(ctx, conn))
	require.NoError(b, EnsureSchema(ctx, conn))

	seedArgs := []any{
		sessionActivityBenchSessions, sessionActivityBenchLong, sessionActivityBenchMessages,
	}
	for _, step := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO sessions (
			id, project, machine, agent, started_at, ended_at,
			message_count, user_message_count, data_version
		)
		SELECT 'bench-' || lpad(CAST(i AS VARCHAR), 6, '0'),
			'project-' || CAST(i % 50 AS VARCHAR), 'bench', 'claude',
			TIMESTAMP '2026-01-01 00:00:00' + to_seconds(i * 1300),
			TIMESTAMP '2026-01-01 06:00:00' + to_seconds(i * 1300),
			CASE WHEN i = 0 THEN $2 ELSE $3 END,
			CASE WHEN i = 0 THEN $2 ELSE $3 END // 2, 1
		FROM range($1) t(i)`, seedArgs},
		{`INSERT INTO messages (
			id, session_id, ordinal, role, content, thinking_text, timestamp,
			has_thinking, has_tool_use, content_length, model, token_usage
		)
		SELECT s.i * $2 + o.j + 1,
			'bench-' || lpad(CAST(s.i AS VARCHAR), 6, '0'), o.j,
			CASE WHEN o.j % 2 = 0 THEN 'user' ELSE 'assistant' END,
			CASE WHEN o.j % 2 = 0 THEN 'please continue with step ' || CAST(o.j AS VARCHAR)
				ELSE repeat('Updated the handler and reran the tests. ', 12) || CAST(o.j AS VARCHAR) END,
			CASE WHEN o.j % 2 = 1 THEN repeat('Considering which file to change next. ', 25) ELSE '' END,
			TIMESTAMP '2026-01-01 00:00:00' + to_seconds(s.i * 1300 + o.j * 5),
			o.j % 2 = 1, o.j % 2 = 1, 120,
			CASE WHEN o.j % 2 = 1 THEN 'claude-sonnet-4-5' ELSE '' END,
			CASE WHEN o.j % 2 = 1 THEN '{"input_tokens":' || CAST(100 + o.j AS VARCHAR) ||
				',"output_tokens":420,"cache_read_input_tokens":21000,"cache_creation_input_tokens":800,' ||
				'"cache_creation":{"ephemeral_5m_input_tokens":800,"ephemeral_1h_input_tokens":0},' ||
				'"server_tool_use":{"web_search_requests":0},"service_tier":"standard"}' ELSE '' END
		FROM range($1) s(i), range($2) o(j)
		WHERE o.j < CASE WHEN s.i = 0 THEN $2 ELSE $3 END`, seedArgs},
		{`INSERT INTO tool_calls (
			id, message_id, session_id, tool_name, category, call_index,
			tool_use_id, input_json, result_content_length, result_content
		)
		SELECT m.id, m.id, m.session_id, c.tool, c.tool, 0, 'toolu_' || CAST(m.id AS VARCHAR),
			CASE c.tool
				WHEN 'Edit' THEN '{"file_path":"/work/src/module_' || CAST(m.ordinal % 97 AS VARCHAR) ||
					'.go","old_string":"' || repeat('old line ', 170) ||
					'","new_string":"' || repeat('new line ', 170) || CAST(m.id AS VARCHAR) || '"}'
				WHEN 'Write' THEN '{"file_path":"/work/src/generated_' || CAST(m.id AS VARCHAR) ||
					'.go","content":"' || repeat('package main\n', 400) || '"}'
				WHEN 'Bash' THEN '{"command":"go test ./internal/... -run TestStep' ||
					CAST(m.ordinal AS VARCHAR) || '","description":"Run the focused tests"}'
				ELSE '{"file_path":"/work/src/module_' || CAST(m.ordinal % 97 AS VARCHAR) || '.go"}'
			END,
			1800,
			CASE c.tool
				WHEN 'Read' THEN repeat('    return handleStep(ctx, req)\n', 60) || CAST(m.id AS VARCHAR)
				WHEN 'Bash' THEN repeat('ok  \tgo.example/internal/step\t0.412s\n', 20) || CAST(m.id AS VARCHAR)
				ELSE 'The file has been updated successfully.'
			END
		FROM messages m
		JOIN (VALUES (0, 'Edit'), (1, 'Bash'), (2, 'Read'), (3, 'Write'), (4, 'Bash')) c(k, tool)
			ON c.k = (m.ordinal // 2) % 5
		WHERE m.role = 'assistant'`, nil},
		{`CHECKPOINT`, nil},
	} {
		_, err = conn.ExecContext(ctx, step.sql, step.args...)
		require.NoError(b, err)
	}
	return NewStoreFromDB(conn)
}

// BenchmarkDuckDBSessionActivityLong measures GetSessionActivity for the
// 4,000-message session in a 20,000-session archive of about 1,000,000
// messages and 500,000 tool calls.
func BenchmarkDuckDBSessionActivityLong(b *testing.B) {
	store := seedSessionActivityBenchStore(b)
	b.ReportAllocs()
	for b.Loop() {
		activity, err := store.GetSessionActivity(b.Context(), "bench-000000")
		require.NoError(b, err)
		require.Equal(b, sessionActivityBenchLong, activity.TotalMessages)
	}
}
