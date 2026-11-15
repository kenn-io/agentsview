//go:build pgtest

package postgres

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/storage"
)

func TestPGSchemaRepairsLeaveSessionVersionsSettled(t *testing.T) {
	pgURL := testPGURL(t)
	local := testDB(t)
	ps, err := New(t.Context(), pgURL, "agentsview", local, "test-machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	defer ps.Close()
	ctx := t.Context()
	require.NoError(t, EnsureSchema(ctx, ps.DB(), "agentsview"))

	for _, tc := range []struct {
		name   string
		repair func() error
	}{
		{"EnsureSchema", func() error { return EnsureSchema(ctx, ps.DB(), "agentsview") }},
		{"runSchemaDataRepairsPG", func() error { return runSchemaDataRepairsPG(ctx, ps.DB()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ps.DB().ExecContext(ctx, `DELETE FROM sessions`)
			require.NoError(t, err)
			_, err = ps.DB().ExecContext(ctx, `INSERT INTO sessions (id, machine, project, agent) VALUES ('one', 'host', 'project', 'codex'), ('two', 'host', 'project', 'codex')`)
			require.NoError(t, err)
			_, err = ps.DB().ExecContext(ctx, `INSERT INTO messages (session_id, ordinal, role, content) VALUES ('one', 0, 'user', 'hello'), ('two', 0, 'user', 'hello')`)
			require.NoError(t, err)
			_, err = ps.DB().ExecContext(ctx, `UPDATE messages SET usage_output_tokens = NULL, usage_web_search_requests = NULL`)
			require.NoError(t, err)
			_, err = ps.DB().ExecContext(ctx, `UPDATE sessions SET updated_at = NOW()`)
			require.NoError(t, err)
			require.NoError(t, tc.repair())
			var unstamped int
			require.NoError(t, ps.DB().QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE automation_audit_hash IS NULL`).Scan(&unstamped))
			assert.Zero(t, unstamped)
			snapshot := func() map[string]string {
				t.Helper()
				rows, err := ps.DB().QueryContext(ctx, `SELECT s.id, `+frustrationSessionVersion("s.xmin")+` FROM sessions s`)
				require.NoError(t, err)
				defer rows.Close()
				versions := map[string]string{}
				for rows.Next() {
					var id string
					var version sql.NullString
					require.NoError(t, rows.Scan(&id, &version))
					require.True(t, version.Valid)
					versions[id] = version.String
				}
				require.NoError(t, rows.Err())
				require.Len(t, versions, 2)
				return versions
			}
			before := snapshot()
			require.NoError(t, tc.repair())
			assert.Equal(t, before, snapshot())
		})
	}
}
