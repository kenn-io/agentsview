package parser

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexIndexedSessionKey(t *testing.T) {
	const thread = "22222222-2222-4222-8222-222222222222"
	const page = thread + "_44444444-4444-4444-8444-444444444444"
	for _, tc := range []struct {
		name, row, rollout, want string
		missing                  bool
		wantErr                  error
	}{
		{name: "missing index", missing: true, wantErr: os.ErrNotExist},
		{name: "missing row", row: "other", wantErr: sql.ErrNoRows},
		{name: "thread", row: thread, rollout: thread, want: thread},
		{name: "revert page", row: thread, rollout: page, want: page},
		{name: "unrelated rollout", row: thread, rollout: "33333333-3333-4333-8333-333333333333"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if !tc.missing {
				for _, file := range []string{"state_2.sqlite", "state_10.sqlite"} {
					conn, err := sql.Open("sqlite3", filepath.Join(home, file))
					require.NoError(t, err)
					_, err = conn.ExecContext(t.Context(), `CREATE TABLE threads (id TEXT PRIMARY KEY, rollout_path TEXT NOT NULL)`)
					require.NoError(t, err)
					if file == "state_10.sqlite" {
						_, err = conn.ExecContext(t.Context(), `INSERT INTO threads VALUES (?, ?)`, tc.row, "rollout-2026-01-01-"+tc.rollout+".jsonl")
						require.NoError(t, err)
					}
					require.NoError(t, conn.Close())
				}
			}
			got, err := CodexIndexedSessionKey(t.Context(), home, thread)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
			}
		})
	}
}
