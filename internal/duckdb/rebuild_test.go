//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func seedRebuildFixture(t *testing.T, local *db.DB) []string {
	t.Helper()
	ids := []string{"rebuild-a", "rebuild-b", "rebuild-c"}
	writes := make([]db.SessionBatchWrite, 0, len(ids))
	for i, id := range ids {
		ts := "2026-01-1" + string(rune('0'+i)) + "T00:00:00.000Z"
		writes = append(writes, db.SessionBatchWrite{
			Session: syncSession(id, "alpha", id+" first", ts, 1),
			Messages: []db.Message{
				syncMessage(id, 0, "user", id+" first", ts),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		})
	}
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(t, err)
	ok, err := local.StarSession(ids[0])
	require.NoError(t, err)
	require.True(t, ok)
	return ids
}

func TestRebuildMirrorCreatesFreshMirrorWithFingerprintsAndMetadata(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	ids := seedRebuildFixture(t, local)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")

	result, err := rebuildMirror(ctx, path, local, "test-machine", SyncOptions{}, nil)
	require.NoError(t, err)

	assert.Equal(t, len(ids), result.SessionsPushed)
	assert.Equal(t, len(ids), result.MessagesPushed)
	assert.Equal(t, 0, result.Errors)
	assert.True(t, result.Diagnostics.Full)
	assert.FileExists(t, path)

	probe, err := ProbeMirror(ctx, path)
	require.NoError(t, err)
	assert.True(t, probe.FileExists)
	assert.True(t, probe.ShapeOK)
	assert.Equal(t, SchemaVersion, probe.SchemaVersion)
	assert.Equal(t, db.CurrentDataVersion(), probe.DataVersion)
	assert.Equal(t, "", probe.Scope)
	assert.NotEmpty(t, probe.LastPushCutoff)
	assert.NotEmpty(t, probe.LastPushAt)

	conn, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	assertDuckDBCount(t, conn, "sessions", len(ids))
	assertDuckDBCount(t, conn, "messages", len(ids))
	assertDuckDBCount(t, conn, "starred_sessions", 1)

	var fingerprintCount int
	require.NoError(t, conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sessions
		WHERE agentsview_push_fingerprint IS NOT NULL
		  AND agentsview_push_fingerprint != ''`,
	).Scan(&fingerprintCount))
	assert.Equal(t, len(ids), fingerprintCount,
		"every rebuilt session row must carry a push fingerprint")
}

func TestRebuildMirrorReplacesPreExistingTargetFileContent(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")

	stale := newTestSync(t, path, local, SyncOptions{})
	require.NoError(t, stale.EnsureSchema(ctx))
	_, err := stale.DB().ExecContext(ctx, `
		INSERT INTO sessions (id, project, machine, agent, created_at)
		VALUES ('stale-session', 'alpha', 'test-machine', 'claude', current_timestamp)`)
	require.NoError(t, err)
	require.NoError(t, stale.Close())

	seedRebuildFixture(t, local)

	result, err := rebuildMirror(ctx, path, local, "test-machine", SyncOptions{}, nil)
	require.NoError(t, err)
	assert.Equal(t, 3, result.SessionsPushed)

	conn, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	assertDuckDBCountWhere(t, conn, "sessions", "id = ?", "stale-session", 0)
	assertDuckDBCount(t, conn, "sessions", 3)
}

func TestRebuildMirrorLeavesNoTempFilesOnSwapFailure(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedRebuildFixture(t, local)
	dir := t.TempDir()
	// A directory in place of the destination file makes the final
	// os.Rename fail deterministically (EISDIR/ENOTDIR) on every platform,
	// simulating a swap failure (e.g. Windows sharing violation) without
	// needing to inject a fake rename.
	path := filepath.Join(dir, "mirror-as-dir.duckdb")
	require.NoError(t, os.Mkdir(path, 0o755))

	_, err := rebuildMirror(ctx, path, local, "test-machine", SyncOptions{}, nil)

	require.Error(t, err)
	assert.DirExists(t, path, "swap failure must leave the destination untouched")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, entry := range entries {
		assert.NotContains(t, entry.Name(), ".tmp-",
			"failed rebuild must not leave a temp mirror file behind")
	}
}

func TestSwapMirrorFileRetriesThenFailsWithActionableError(t *testing.T) {
	dir := t.TempDir()
	tmpPath := filepath.Join(dir, "source.duckdb")
	require.NoError(t, os.WriteFile(tmpPath, []byte("mirror bytes"), 0o644))
	dstPath := filepath.Join(dir, "dst-is-a-dir")
	require.NoError(t, os.Mkdir(dstPath, 0o755))

	err := swapMirrorFile(tmpPath, dstPath)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "agentsview duckdb serve")
	assert.FileExists(t, tmpPath, "source file must survive a failed swap")
	content, readErr := os.ReadFile(tmpPath)
	require.NoError(t, readErr)
	assert.Equal(t, "mirror bytes", string(content))
}

func TestRebuildMirrorScopesToProjectFilters(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session:         syncSession("rebuild-scope-alpha", "alpha", "a", "2026-01-10T00:00:00.000Z", 1),
			Messages:        []db.Message{syncMessage("rebuild-scope-alpha", 0, "user", "a", "2026-01-10T00:00:00.000Z")},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session:         syncSession("rebuild-scope-beta", "beta", "b", "2026-01-10T00:00:00.000Z", 1),
			Messages:        []db.Message{syncMessage("rebuild-scope-beta", 0, "user", "b", "2026-01-10T00:00:00.000Z")},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "scoped.duckdb")

	opts := SyncOptions{Projects: []string{"alpha"}}
	result, err := rebuildMirror(ctx, path, local, "test-machine", opts, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.SessionsPushed)

	probe, err := ProbeMirror(ctx, path)
	require.NoError(t, err)
	assert.Equal(t, canonicalPushScope(opts.Projects, opts.ExcludeProjects), probe.Scope)

	conn, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	assertDuckDBCountWhere(t, conn, "sessions", "id = ?", "rebuild-scope-alpha", 1)
	assertDuckDBCountWhere(t, conn, "sessions", "id = ?", "rebuild-scope-beta", 0)
}

func TestValidateBuiltMirrorRejectsBadMirrors(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name         string
		setupMirror  func(t *testing.T, path string) int // returns wantSessions
		wantSessions int
		expectError  bool
		errorPattern string
	}{
		{
			name: "session count mismatch",
			setupMirror: func(t *testing.T, path string) int {
				conn, err := Open(path)
				require.NoError(t, err)
				require.NoError(t, createSchema(ctx, conn))
				require.NoError(t, writeMirrorMetadata(ctx, conn, mirrorMetadata{
					SchemaVersion:  SchemaVersion,
					DataVersion:    1,
					Scope:          "",
					LastPushCutoff: "2026-07-18T00:00:00.000Z",
				}))
				// Insert 2 sessions
				_, err = conn.ExecContext(ctx, `
					INSERT INTO sessions (id, project, machine, agent, created_at)
					VALUES ('sess-1', 'alpha', 'test-machine', 'claude', current_timestamp),
					       ('sess-2', 'alpha', 'test-machine', 'claude', current_timestamp)`)
				require.NoError(t, err)
				require.NoError(t, conn.Close())
				return 2 // actual sessions in mirror
			},
			wantSessions: 3, // but we claim to want 3
			expectError:  true,
			errorPattern: "has 2 sessions, want 3",
		},
		{
			name: "missing metadata table",
			setupMirror: func(t *testing.T, path string) int {
				conn, err := Open(path)
				require.NoError(t, err)
				require.NoError(t, createSchema(ctx, conn))
				_, err = conn.ExecContext(ctx, `DROP TABLE sync_metadata`)
				require.NoError(t, err)
				require.NoError(t, conn.Close())
				return 0
			},
			wantSessions: 0,
			expectError:  true,
			errorPattern: "shape incompatible",
		},
		{
			name: "wrong schema version",
			setupMirror: func(t *testing.T, path string) int {
				conn, err := Open(path)
				require.NoError(t, err)
				require.NoError(t, createSchema(ctx, conn))
				require.NoError(t, writeMirrorMetadata(ctx, conn, mirrorMetadata{
					SchemaVersion:  2, // wrong version
					DataVersion:    1,
					Scope:          "",
					LastPushCutoff: "2026-07-18T00:00:00.000Z",
				}))
				require.NoError(t, conn.Close())
				return 0
			},
			wantSessions: 0,
			expectError:  true,
			errorPattern: "schema version 2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.duckdb")
			actSessions := tt.setupMirror(t, path)

			err := validateBuiltMirror(ctx, path, tt.wantSessions)

			if tt.expectError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errorPattern)
			} else {
				require.NoError(t, err)
			}

			// Verify file still exists after validation (read-only check)
			_, statErr := os.Stat(path)
			require.NoError(t, statErr, "mirror file must exist after validation")

			// For count mismatch case, verify file content unchanged
			if tt.name == "session count mismatch" {
				conn, err := Open(path)
				require.NoError(t, err)
				assertDuckDBCount(t, conn, "sessions", actSessions)
				require.NoError(t, conn.Close())
			}
		})
	}
}
