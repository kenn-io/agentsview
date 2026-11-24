package parser

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/testutil"
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
				testutil.WriteCodexState(t, home, "state_2.sqlite", thread, thread)
				testutil.WriteCodexState(t, home, "state_5.sqlite", tc.row, tc.rollout)
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

func TestCodexIndexedSessionKeyRejectsFutureIndex(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "state_999.sqlite"), nil, 0o600))
	_, err := CodexIndexedSessionKey(t.Context(), home, "thread")
	require.ErrorIs(t, err, ErrUnsupportedCodexStateIndex)
	require.ErrorContains(t, err, "unsupported Codex state index")
	const thread = "22222222-2222-4222-8222-222222222222"
	testutil.WriteCodexState(t, home, "state_5.sqlite", thread, thread)
	_, err = CodexIndexedSessionKey(t.Context(), home, thread)
	require.ErrorIs(t, err, ErrUnsupportedCodexStateIndex)
	require.ErrorContains(t, err, "unsupported Codex state index")
}

func TestCodexIndexedSessionKeyRejectsFutureMigration(t *testing.T) {
	home := t.TempDir()
	const thread = "22222222-2222-4222-8222-222222222222"
	testutil.WriteCodexState(t, home, "state_5.sqlite", thread, thread)
	state, err := sql.Open("sqlite3", filepath.Join(home, "state_5.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = state.Close() })
	_, err = state.ExecContext(t.Context(), `INSERT INTO _sqlx_migrations VALUES (55)`)
	require.NoError(t, err)
	_, err = CodexIndexedSessionKey(t.Context(), home, thread)
	require.ErrorIs(t, err, ErrUnsupportedCodexStateIndex)
	require.ErrorContains(t, err, "unsupported Codex state migration 55")
}
