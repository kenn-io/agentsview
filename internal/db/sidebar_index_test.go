package db

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSidebarParentAutomationIndexLifecycle(t *testing.T) {
	for _, lifecycle := range []string{"new archive", "existing archive", "bulk import"} {
		t.Run(lifecycle, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "root", "project", func(s *Session) {
				s.UserMessageCount = 2
			})
			humanIDs := []string{"root"}
			for i := range 40 {
				id := fmt.Sprintf("child-%02d", i)
				insertSession(t, d, id, "project", func(s *Session) {
					s.ParentSessionID = new("root")
					s.RelationshipType = "subagent"
					if i%2 == 1 {
						s.FirstMessage = new("You are a code reviewer. Review the code.")
					}
				})
				if i%2 == 0 {
					humanIDs = append(humanIDs, id)
				}
			}
			// Other small trees make the parent lookup selective. With only
			// one parent SQLite can reasonably prefer scanning the tiny table.
			for i := range 40 {
				parent := fmt.Sprintf("other-root-%02d", i)
				insertSession(t, d, parent, "other")
				for j := range 2 {
					insertSession(t, d, fmt.Sprintf("%s-child-%d", parent, j), "other", func(s *Session) {
						s.ParentSessionID = &parent
						s.RelationshipType = "subagent"
					})
				}
			}
			if lifecycle == "existing archive" {
				_, err := d.getWriter().Exec(t.Context(),
					`DROP INDEX IF EXISTS idx_sessions_parent_automated`)
				require.NoError(t, err)
				path := d.Path()
				require.NoError(t, d.Close())
				needsResync, err := ArchiveNeedsResync(t.Context(), path)
				require.NoError(t, err)
				assert.False(t, needsResync, "a missing lookup index does not require reparsing sessions")
				for reopen := range 2 {
					d, err = OpenIsolated(t.Context(), path)
					require.NoError(t, err)
					if reopen == 0 {
						require.NoError(t, d.Close())
					}
				}
				t.Cleanup(func() { require.NoError(t, d.Close()) })
			} else if lifecycle == "bulk import" {
				require.NoError(t, d.DropBulkImportIndexes(t.Context()))
				require.NoError(t, d.RebuildBulkImportIndexes(t.Context()))
			}

			// The sidebar's recursive step must look up children by both
			// parent and automation status, rather than repeatedly scanning
			// every human session for each node in a large session tree.
			plan := queryPlanOf(t, d, `WITH RECURSIVE tree(id) AS (
				SELECT id FROM sessions WHERE id = ?
				UNION
				SELECT s.id FROM sessions s JOIN tree t ON s.parent_session_id = t.id
				WHERE s.message_count > 0 AND s.deleted_at IS NULL AND s.is_automated = 0
			) SELECT id FROM tree`, "root")
			assert.Contains(t, plan, "USING INDEX idx_sessions_parent_automated (parent_session_id=? AND is_automated=?)")

			index, err := d.GetSidebarSessionIndex(t.Context(), SessionFilter{
				Project: "project", ExcludeAutomated: true, Limit: 1,
			})
			require.NoError(t, err)
			requireSidebarIndexIDs(t, index.Sessions, humanIDs)
			assert.Equal(t, 1, index.Total)
			index, err = d.GetSidebarSessionIndex(t.Context(), SessionFilter{Project: "project", Limit: 1})
			require.NoError(t, err)
			assert.Len(t, index.Sessions, 41)
			var version int
			require.NoError(t, d.getReader().QueryRow(t.Context(), "PRAGMA user_version").Scan(&version))
			assert.Equal(t, CurrentDataVersion(), version)
		})
	}
}

func TestFreshArchiveIncludesSidebarParentAutomationIndex(t *testing.T) {
	// Full rebuilds initialize a fresh archive without historical migrations.
	path := filepath.Join(t.TempDir(), "fresh.db")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	d, err := OpenFreshIsolatedContext(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	plan := queryPlanOf(t, d, `SELECT id FROM sessions
		WHERE parent_session_id = ? AND is_automated = 0`, "root")
	assert.Contains(t, plan, "USING INDEX idx_sessions_parent_automated (parent_session_id=? AND is_automated=?)")
}
