package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

func TestReconciliationSpoolSelectsPreferredCandidateInSQL(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "sessions.db")
	spool, err := newReconciliationSpool(archivePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spool.CloseAndRemove()) })

	ctx := t.Context()
	require.NoError(t, spool.Add(ctx, reconciliationCandidate{
		Provider: parser.AgentClaude, Identity: "same", Path: "/sessions/old.jsonl",
		StoredPath:  "remote:/sessions/old.jsonl",
		Preference1: 100, Preference2: 1,
	}))
	require.NoError(t, spool.Add(ctx, reconciliationCandidate{
		Provider: parser.AgentClaude, Identity: "same", Path: "/sessions/new.jsonl",
		StoredPath: "remote:/sessions/new.jsonl", MemberIdentity: "stable",
		Preference1: 200, Preference2: 2,
	}))
	require.NoError(t, spool.Add(ctx, reconciliationCandidate{
		Provider: parser.AgentCodex, Identity: "uuid", Path: "/archived/rollout-uuid.jsonl",
		Preference1: 0,
	}))
	require.NoError(t, spool.Add(ctx, reconciliationCandidate{
		Provider: parser.AgentCodex, Identity: "uuid", Path: "/sessions/2026/07/14/rollout-uuid.jsonl",
		Preference1: 1,
	}))

	page, err := spool.Page(ctx, reconciliationCursor{}, reconciliationPageSize)
	require.NoError(t, err)
	require.Len(t, page, 2)
	assert.Equal(t, "/sessions/new.jsonl", page[0].Path)
	assert.Equal(t, "remote:/sessions/new.jsonl", page[0].StoredPath)
	assert.Equal(t, "/sessions/2026/07/14/rollout-uuid.jsonl", page[1].Path)
	present, err := spool.ContainsSource(
		ctx, parser.AgentClaude, "remote:/sessions/new.jsonl",
	)
	require.NoError(t, err)
	assert.True(t, present)
	present, err = spool.ContainsSource(
		ctx, parser.AgentClaude, "remote:/sessions/old.jsonl",
	)
	require.NoError(t, err)
	assert.False(t, present,
		"membership must follow the preferred candidate selected by the spool")
	present, err = spool.ContainsSourceIdentity(
		ctx, parser.AgentClaude, "remote:/sessions/new.jsonl", "stable",
	)
	require.NoError(t, err)
	assert.True(t, present)
	present, err = spool.ContainsSourceIdentity(
		ctx, parser.AgentClaude, "remote:/sessions/new.jsonl", "different",
	)
	require.NoError(t, err)
	assert.False(t, present,
		"path reuse by a different identity must not prove source membership")
}

func TestReconciliationSpoolDSNEscapesPortablePaths(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "posix special characters",
			path: "/tmp/archive #?%/reconcile.db",
			want: "file:/tmp/archive%20%23%3F%25/reconcile.db",
		},
		{
			name: "windows drive",
			path: `C:\Data\archive #?%\reconcile.db`,
			want: "file:/C:/Data/archive%20%23%3F%25/reconcile.db",
		},
		{
			name: "windows UNC",
			path: `\\server\share\archive #%\reconcile.db`,
			want: `\\server\share\archive #%\reconcile.db`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, reconciliationSpoolDSN(tc.path))
		})
	}
}

func TestReconciliationSpoolPagesStayBoundedAndCleanup(t *testing.T) {
	dir := t.TempDir()
	spool, err := newReconciliationSpool(filepath.Join(dir, "sessions.db"))
	require.NoError(t, err)
	path := spool.path
	assert.Equal(t, dir, filepath.Dir(path))

	for i := range reconciliationPageSize*3 + 17 {
		require.NoError(t, spool.Add(t.Context(), reconciliationCandidate{
			Provider: parser.AgentClaude,
			Identity: assertSessionIdentity(i),
			Path:     filepath.Join(dir, assertSessionIdentity(i)+".jsonl"),
		}))
	}

	var cursor reconciliationCursor
	var total int
	for {
		page, err := spool.Page(t.Context(), cursor, reconciliationPageSize)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(page), reconciliationPageSize)
		if len(page) == 0 {
			break
		}
		total += len(page)
		cursor = page[len(page)-1].Cursor()
	}
	assert.Equal(t, reconciliationPageSize*3+17, total)
	assert.Equal(t, reconciliationPageSize, spool.Metrics().MaxSpoolPageRows)

	require.NoError(t, spool.CloseAndRemove())
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_, err := os.Stat(path + suffix)
		assert.ErrorIs(t, err, os.ErrNotExist)
	}
}

func TestReconciliationSpoolCancellationAndClosedErrors(t *testing.T) {
	spool, err := newReconciliationSpool(filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	path := spool.path

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.ErrorIs(t, spool.Add(ctx, reconciliationCandidate{
		Provider: parser.AgentClaude, Identity: "cancelled", Path: "/cancelled",
	}), context.Canceled)

	require.NoError(t, spool.closeDB())
	err = spool.Add(t.Context(), reconciliationCandidate{
		Provider: parser.AgentClaude, Identity: "closed", Path: "/closed",
	})
	assert.Error(t, err)
	assert.False(t, errors.Is(err, context.Canceled))
	_, err = spool.Page(t.Context(), reconciliationCursor{}, reconciliationPageSize)
	assert.Error(t, err)

	require.NoError(t, spool.CloseAndRemove())
	_, err = os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func assertSessionIdentity(i int) string {
	return fmt.Sprintf("session-%06d", i)
}
