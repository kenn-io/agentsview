package postgres

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestProjectScopeMoveCandidatesStayBoundedByChangedBatch(t *testing.T) {
	const oldCreatedAt = "2026-07-30T11:00:00.000Z"

	candidateIDs := func(t *testing.T, oldSessionCount int) []string {
		t.Helper()

		local, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "local.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, local.Close()) })

		for i := range oldSessionCount {
			require.NoError(t, local.UpsertSession(t.Context(), db.Session{
				ID: fmt.Sprintf("old-%04d", i), Project: "included",
				Machine: "workstation", Agent: "codex",
				CreatedAt: oldCreatedAt,
			}))
		}
		raw, err := sql.Open("sqlite3", local.Path())
		require.NoError(t, err)
		defer raw.Close()
		_, err = raw.ExecContext(t.Context(), "UPDATE sessions SET created_at = ?, local_modified_at = ?", oldCreatedAt, oldCreatedAt)
		require.NoError(t, err)
		const lastPush = "2026-07-30T12:00:00.000Z"
		require.NoError(t, local.UpsertSession(t.Context(), db.Session{
			ID: "changed-out-of-scope", Project: "excluded",
			Machine: "workstation", Agent: "codex",
			CreatedAt: oldCreatedAt,
		}))

		sessions, err := listPGProjectScopeMoveCandidates(
			t.Context(), local, lastPush,
		)
		require.NoError(t, err)
		ids := make([]string, 0, len(sessions))
		for _, session := range sessions {
			ids = append(ids, session.ID)
		}
		return ids
	}

	small := candidateIDs(t, 1)
	large := candidateIDs(t, 1_000)
	assert.Equal(t, []string{"changed-out-of-scope"}, small)
	assert.Equal(t, small, large,
		"unchanged archive cardinality must not expand per-push reconciliation")
}
