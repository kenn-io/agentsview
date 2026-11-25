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
	const page = thread + "_44444444-4444-4444-8444-444444444444"
	testutil.WriteCodexState(t, home, "state_5.sqlite", thread, page)
	state, err := sql.Open("sqlite3", filepath.Join(home, "state_5.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = state.Close() })
	_, err = state.ExecContext(t.Context(), `INSERT INTO _sqlx_migrations VALUES (64)`)
	require.NoError(t, err)
	got, err := CodexIndexedSessionKey(t.Context(), home, thread)
	require.NoError(t, err)
	assert.Equal(t, page, got)
	_, err = state.ExecContext(t.Context(), `INSERT INTO _sqlx_migrations VALUES (65)`)
	require.NoError(t, err)
	_, err = CodexIndexedSessionKey(t.Context(), home, thread)
	require.ErrorIs(t, err, ErrUnsupportedCodexStateIndex)
	require.ErrorContains(t, err, "unsupported Codex state migration 65")
}

func TestCodexIndexedSessionKeyUninitializedAndCorrupt(t *testing.T) {
	const thread = "22222222-2222-4222-8222-222222222222"
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, statePath string)
		wantErr error
	}{
		{
			name: "empty file",
			setup: func(t *testing.T, statePath string) {
				t.Helper()
				require.NoError(t, os.WriteFile(statePath, nil, 0o600))
			},
			wantErr: os.ErrNotExist,
		},
		{
			name: "wal initialized without tables",
			setup: func(t *testing.T, statePath string) {
				t.Helper()
				state, err := sql.Open("sqlite3", statePath)
				require.NoError(t, err)
				t.Cleanup(func() { _ = state.Close() })
				_, err = state.ExecContext(t.Context(), `PRAGMA journal_mode=WAL`)
				require.NoError(t, err)
				require.NoError(t, state.Close())
			},
			wantErr: os.ErrNotExist,
		},
		{
			name: "migrations table without threads",
			setup: func(t *testing.T, statePath string) {
				t.Helper()
				state, err := sql.Open("sqlite3", statePath)
				require.NoError(t, err)
				t.Cleanup(func() { _ = state.Close() })
				_, err = state.ExecContext(t.Context(), `CREATE TABLE _sqlx_migrations (version BIGINT PRIMARY KEY)`)
				require.NoError(t, err)
				require.NoError(t, state.Close())
			},
			wantErr: os.ErrNotExist,
		},
		{
			name: "threads table without migrations",
			setup: func(t *testing.T, statePath string) {
				t.Helper()
				state, err := sql.Open("sqlite3", statePath)
				require.NoError(t, err)
				t.Cleanup(func() { _ = state.Close() })
				_, err = state.ExecContext(t.Context(), `CREATE TABLE threads (id TEXT PRIMARY KEY, rollout_path TEXT NOT NULL)`)
				require.NoError(t, err)
				require.NoError(t, state.Close())
			},
			wantErr: os.ErrNotExist,
		},
		{
			name: "future migration without threads",
			setup: func(t *testing.T, statePath string) {
				t.Helper()
				state, err := sql.Open("sqlite3", statePath)
				require.NoError(t, err)
				t.Cleanup(func() { _ = state.Close() })
				_, err = state.ExecContext(t.Context(), `CREATE TABLE _sqlx_migrations (version BIGINT PRIMARY KEY)`)
				require.NoError(t, err)
				_, err = state.ExecContext(t.Context(), `INSERT INTO _sqlx_migrations VALUES (65)`)
				require.NoError(t, err)
				require.NoError(t, state.Close())
			},
			wantErr: ErrUnsupportedCodexStateIndex,
		},
		{
			name: "corrupt file",
			setup: func(t *testing.T, statePath string) {
				t.Helper()
				require.NoError(t, os.WriteFile(statePath, []byte("corrupt"), 0o600))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			tc.setup(t, filepath.Join(home, "state_5.sqlite"))
			_, err := CodexIndexedSessionKey(t.Context(), home, thread)
			require.Error(t, err)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NotErrorIs(t, err, os.ErrNotExist)
				require.NotErrorIs(t, err, sql.ErrNoRows)
				require.NotErrorIs(t, err, ErrUnsupportedCodexStateIndex)
			}
		})
	}
}
