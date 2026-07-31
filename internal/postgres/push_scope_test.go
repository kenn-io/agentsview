package postgres

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestProjectScopeMoveCandidatesStayBoundedByChangedBatch(t *testing.T) {
	const oldCreatedAt = "2026-07-30T11:00:00.000Z"

	candidateIDs := func(t *testing.T, oldSessionCount int) []string {
		t.Helper()
		local, err := db.Open(filepath.Join(t.TempDir(), "local.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, local.Close()) })

		for i := range oldSessionCount {
			require.NoError(t, local.UpsertSession(db.Session{
				ID: fmt.Sprintf("old-%04d", i), Project: "included",
				Machine: "workstation", Agent: "codex",
				CreatedAt: oldCreatedAt,
			}))
		}
		time.Sleep(5 * time.Millisecond)
		lastPush := time.Now().UTC().Format(LocalSyncTimestampLayout)
		time.Sleep(5 * time.Millisecond)
		require.NoError(t, local.UpsertSession(db.Session{
			ID: "changed-out-of-scope", Project: "excluded",
			Machine: "workstation", Agent: "codex",
			CreatedAt: oldCreatedAt,
		}))

		sessions, err := listPGProjectScopeMoveCandidates(
			context.Background(), local, lastPush,
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
