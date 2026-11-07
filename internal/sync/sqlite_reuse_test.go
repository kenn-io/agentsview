package sync

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func countSQLiteQueryPreparations(t *testing.T, database *sql.DB) *atomic.Int64 {
	t.Helper()
	var preparations atomic.Int64
	conn, err := database.Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, conn.Raw(func(raw any) error {
		// SQLite calls the authorizer during SQL compilation.
		raw.(*sqlite3.SQLiteConn).RegisterAuthorizer(func(op int, _, _, _ string) int {
			if op == sqlite3.SQLITE_SELECT {
				preparations.Add(1)
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}))
	require.NoError(t, conn.Close())
	return &preparations
}

func TestReconciliationSpoolReusesSQL(t *testing.T) {
	spool, err := newReconciliationSpool(t.Context(), filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spool.CloseAndRemove(context.WithoutCancel(t.Context()))) })
	preparations := countSQLiteQueryPreparations(t, spool.db)

	for i := range 22 {
		id := fmt.Sprintf("session-%02d", i)
		require.NoError(t, spool.Add(t.Context(), reconciliationCandidate{
			Provider: parser.AgentCodex, Identity: id, Path: id + ".jsonl",
			StoredPath: id + ".jsonl", Project: fmt.Sprintf("project-%d", i),
		}))
		if i == 1 {
			preparations.Store(0)
		}
	}
	assert.Zero(t, preparations.Load(), "later candidates must reuse their compiled lookup")
	page, err := spool.Page(t.Context(), reconciliationCursor{}, reconciliationPageSize)
	require.NoError(t, err)
	require.Len(t, page, 22)
	assert.Equal(t, "session-21.jsonl", page[21].Path)
	assert.Equal(t, "project-21", page[21].Project)

	for i := range 22 {
		found, err := spool.ContainsSource(t.Context(), parser.AgentCodex, fmt.Sprintf("session-%02d.jsonl", i))
		require.NoError(t, err)
		require.True(t, found)
		if i == 1 {
			preparations.Store(0)
		}
	}
	assert.Zero(t, preparations.Load(), "later membership checks must reuse compiled SQL")
}

func TestCodexStagingReusesSQL(t *testing.T) {
	sink, err := newCodexStagingSink(t.Context(), t.TempDir(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sink.Close()) })
	preparations := countSQLiteQueryPreparations(t, sink.scratch)

	for i := range 22 {
		id := fmt.Sprintf("call-%d", i)
		sink.AppendMessage(parser.ParsedMessage{ToolCalls: []parser.ParsedToolCall{{
			ToolUseID: id, ToolName: "exec_command", Category: "Bash",
		}}})
		sink.AppendToolResultEvent(t.Context(), id, nil, parser.ParsedToolResultEvent{
			ToolUseID: id, Source: "function_call_output", Status: "completed",
			Content: fmt.Sprintf("tool result %d", i),
		})
		require.NoError(t, sink.Err())
		if i == 1 {
			preparations.Store(0)
		}
	}
	assert.Zero(t, preparations.Load(), "later results must reuse the compiled deduplication lookup")
	var count int
	require.NoError(t, sink.scratch.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM stage_events`).Scan(&count))
	assert.Equal(t, 22, count)
	var content string
	require.NoError(t, sink.scratch.QueryRowContext(t.Context(),
		`SELECT content FROM stage_events WHERE tool_use_id = 'call-21'`).Scan(&content))
	assert.Equal(t, "tool result 21", content)
}
