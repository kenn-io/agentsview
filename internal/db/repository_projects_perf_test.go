package db

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepositoryProjectImportsReuseSQL(t *testing.T) {
	d := testDB(t)
	var nameReads atomic.Int64
	conn, err := d.getWriter().Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, conn.Raw(func(raw any) error {
		raw.(*sqlite3.SQLiteConn).RegisterAuthorizer(func(op int, table, column, _ string) int {
			if op == sqlite3.SQLITE_READ && table == "repository_project_names" && column == "project" {
				nameReads.Add(1)
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}))
	require.NoError(t, conn.Close())

	for i := range 22 {
		s, obs := repositorySession(fmt.Sprintf("import-%d", i), "pr123", "https://example.com/team/sample.git")
		require.NoError(t, d.UpsertSessionWithProjectIdentity(t.Context(), s, obs, s.Project))
		if i == 1 {
			// Warm both the missing-name and saved-name paths before counting preparations.
			nameReads.Store(0)
		}
	}
	assert.Zero(t, nameReads.Load(), "later imports must reuse the compiled repository lookup")
	got, err := d.GetSessionFull(t.Context(), "import-21")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "sample", got.Project)
	snapshots, err := d.ListSessionProjectIdentitySnapshotsByID(t.Context(), []string{"import-21"})
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	assert.Equal(t, "pr123", snapshots["import-21"].Project)
}
