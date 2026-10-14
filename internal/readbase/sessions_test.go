package readbase

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

type findingPageBackend struct {
	SessionsBackend
	database *sql.DB
}

func (b findingPageBackend) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return b.database.QueryContext(ctx, query, args...)
}

func (findingPageBackend) ListSecretFindingsSQL(f db.SecretFindingFilter) (string, []any) {
	return `SELECT 'session-' || column1, 'rule', 'high', 'message',
  0, 0, 0, 0, 1, 0, 'redacted', 'v1', 'project', 'agent'
  FROM (VALUES (1), (2), (3)) LIMIT ? OFFSET ?`, []any{f.Limit + 1, f.Cursor}
}

func TestSessionsFindingPagination(t *testing.T) {
	database, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer database.Close()
	sessions := NewSessions(findingPageBackend{database: database})
	first, err := sessions.ListSecretFindings(t.Context(), db.SecretFindingFilter{Limit: 2})
	require.NoError(t, err)
	require.Len(t, first.Findings, 2)
	assert.Equal(t, "session-1", first.Findings[0].SessionID)
	assert.Equal(t, "session-2", first.Findings[1].SessionID)
	assert.Equal(t, 2, first.NextCursor)
	second, err := sessions.ListSecretFindings(t.Context(), db.SecretFindingFilter{Limit: 2, Cursor: first.NextCursor})
	require.NoError(t, err)
	require.Len(t, second.Findings, 1)
	assert.Equal(t, "session-3", second.Findings[0].SessionID)
	assert.Zero(t, second.NextCursor)
	empty, err := sessions.ListSecretFindings(t.Context(), db.SecretFindingFilter{Limit: 2, Cursor: 3})
	require.NoError(t, err)
	assert.Nil(t, empty.Findings)
	assert.Zero(t, empty.NextCursor)
}
