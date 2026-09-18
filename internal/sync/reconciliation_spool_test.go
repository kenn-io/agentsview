package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

func TestReconciliationSpoolSelectsPreferredCandidateInSQL(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	archivePath := filepath.Join(t.TempDir(), "sessions.db")
	spool, err := newReconciliationSpool(archivePath)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(spool.CloseAndRemove()) })

	ctx := t.Context()
	require.NoError(spool.Add(ctx, reconciliationCandidate{
		Provider: parser.AgentClaude, Identity: "same", Path: "/sessions/old.jsonl",
		StoredPath: "remote:/sessions/old.jsonl", Machine: "old-machine",
		Preference1: 100, Preference2: 1,
	}))
	require.NoError(spool.Add(ctx, reconciliationCandidate{
		Provider: parser.AgentClaude, Identity: "same", Path: "/sessions/new.jsonl",
		StoredPath: "remote:/sessions/new.jsonl", MemberIdentity: "stable",
		Machine:     "new-machine",
		Preference1: 200, Preference2: 2,
	}))
	assert.False(spool.LastAddWon(),
		"a winner replacement must not count as a new discovered identity")
	replaced, ok := spool.LastAddReplaced()
	require.True(ok)
	assert.Equal("/sessions/old.jsonl", replaced.Path)
	require.NoError(spool.Add(ctx, reconciliationCandidate{
		Provider: parser.AgentCodex, Identity: "uuid", Path: "/archived/rollout-uuid.jsonl",
		Preference1: 0,
	}))
	require.NoError(spool.Add(ctx, reconciliationCandidate{
		Provider: parser.AgentCodex, Identity: "uuid", Path: "/sessions/2026/07/14/rollout-uuid.jsonl",
		Preference1: 1,
	}))
	require.NoError(spool.Add(ctx, reconciliationCandidate{
		Provider: parser.AgentClaude, Identity: "same", Path: "/sessions/old.jsonl",
		StoredPath: "remote:/sessions/old.jsonl", Machine: "old-machine",
		Preference1: 100, Preference2: 1,
	}))
	assert.False(spool.LastAddWon(),
		"a lower-ranked duplicate must not count as a discovered winner")

	page, err := spool.Page(ctx, reconciliationCursor{}, reconciliationPageSize)
	require.NoError(err)
	require.Len(page, 2)
	assert.Equal("/sessions/new.jsonl", page[0].Path)
	assert.Equal("remote:/sessions/new.jsonl", page[0].StoredPath)
	assert.Equal("new-machine", page[0].Machine)
	assert.Equal("/sessions/2026/07/14/rollout-uuid.jsonl", page[1].Path)
	present, err := spool.ContainsSource(
		ctx, parser.AgentClaude, "remote:/sessions/new.jsonl",
	)
	require.NoError(err)
	assert.True(present)
	present, err = spool.ContainsSource(
		ctx, parser.AgentClaude, "remote:/sessions/old.jsonl",
	)
	require.NoError(err)
	assert.False(present,
		"membership must follow the preferred candidate selected by the spool")
	present, err = spool.ContainsSourceIdentity(
		ctx, parser.AgentClaude, "remote:/sessions/new.jsonl", "stable",
	)
	require.NoError(err)
	assert.True(present)
	present, err = spool.ContainsSourceIdentity(
		ctx, parser.AgentClaude, "remote:/sessions/new.jsonl", "different",
	)
	require.NoError(err)
	assert.False(present,
		"path reuse by a different identity must not prove source membership")
}

func TestReconciliationSpoolCarriesWinningSourceState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	archivePath := filepath.Join(t.TempDir(), "sessions.db")
	spool, err := newReconciliationSpool(archivePath)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(spool.CloseAndRemove()) })

	ctx := t.Context()
	oldState := parser.ReconciliationSourceState{
		Version: 1,
		Payload: []byte("old"),
	}
	newState := parser.ReconciliationSourceState{
		Version: 1,
		Payload: []byte("new"),
	}
	require.NoError(spool.Add(ctx, reconciliationCandidate{
		Provider: parser.AgentOpenCode, Identity: "same", Path: "/old",
		SourceState: oldState, Preference1: 1,
	}))
	require.NoError(spool.Add(ctx, reconciliationCandidate{
		Provider: parser.AgentOpenCode, Identity: "same", Path: "/new",
		SourceState: newState, Preference1: 2,
	}))

	page, err := spool.Page(ctx, reconciliationCursor{}, reconciliationPageSize)
	require.NoError(err)
	require.Len(page, 1)
	assert.Equal("/new", page[0].Path)
	assert.Equal(newState.Version, page[0].SourceState.Version)
	assert.Equal(newState.Payload, page[0].SourceState.Payload)
}

func TestReconciliationSpoolFallsBackForOversizedSourceState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	spool, err := newReconciliationSpool(filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(spool.CloseAndRemove()) })

	err = spool.Add(t.Context(), reconciliationCandidate{
		Provider: parser.AgentOpenCode, Identity: "large", Path: "/large",
		SourceState: parser.ReconciliationSourceState{
			Version: 1,
			Payload: make([]byte, maxReconciliationSourceStateBytes+1),
		},
	})
	require.NoError(err)
	page, err := spool.Page(t.Context(), reconciliationCursor{}, reconciliationPageSize)
	require.NoError(err)
	require.Len(page, 1)
	assert.Zero(page[0].SourceState.Version,
		"oversized optional state must fall back to authoritative rehydration")
	assert.Empty(page[0].SourceState.Payload)
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
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	spool, err := newReconciliationSpool(filepath.Join(dir, "sessions.db"))
	require.NoError(err)
	path := spool.path
	assert.Equal(dir, filepath.Dir(path))

	for i := range reconciliationPageSize*3 + 17 {
		require.NoError(spool.Add(t.Context(), reconciliationCandidate{
			Provider: parser.AgentClaude,
			Identity: assertSessionIdentity(i),
			Path:     filepath.Join(dir, assertSessionIdentity(i)+".jsonl"),
		}))
	}

	var cursor reconciliationCursor
	var total int
	for {
		page, err := spool.Page(t.Context(), cursor, reconciliationPageSize)
		require.NoError(err)
		assert.LessOrEqual(len(page), reconciliationPageSize)
		if len(page) == 0 {
			break
		}
		total += len(page)
		cursor = page[len(page)-1].Cursor()
	}
	assert.Equal(reconciliationPageSize*3+17, total)
	assert.Equal(reconciliationPageSize, spool.Metrics().MaxSpoolPageRows)

	require.NoError(spool.CloseAndRemove())
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_, err := os.Stat(path + suffix)
		assert.ErrorIs(err, os.ErrNotExist)
	}
}

func TestReconciliationSpoolNonAuthoritativeScopesStayPageBounded(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	spool, err := newReconciliationSpool(filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(spool.CloseAndRemove()) })

	ctx := t.Context()
	require.NoError(spool.Add(ctx, reconciliationCandidate{
		Provider: parser.AgentTrae,
		Identity: "candidate",
		Path:     "/trae/candidate/state.vscdb",
	}))
	_, err = spool.Page(ctx, reconciliationCursor{}, reconciliationPageSize)
	require.NoError(err, "candidate paging must seal the discovery transaction")

	const scopeCount = reconciliationPageSize*3 + 17
	for start := 0; start < scopeCount; start += reconciliationPageSize {
		end := min(start+reconciliationPageSize, scopeCount)
		scopes := make([]reconciliationSourceScope, 0, end-start)
		for i := start; i < end; i++ {
			scopes = append(scopes, reconciliationSourceScope{
				Provider: parser.AgentTrae,
				Path:     filepath.Join("/trae", assertSessionIdentity(i), "state.vscdb"),
			})
		}
		require.NoError(spool.AddNonAuthoritativeScopes(ctx, scopes))
	}

	for _, i := range []int{0, reconciliationPageSize, scopeCount - 1} {
		present, queryErr := spool.ContainsNonAuthoritativeScope(
			ctx, parser.AgentTrae,
			filepath.Join("/trae", assertSessionIdentity(i), "state.vscdb"),
		)
		require.NoError(queryErr)
		assert.True(present, "scope %d must remain queryable from disk", i)
	}
	present, err := spool.ContainsNonAuthoritativeScope(
		ctx, parser.AgentTrae, filepath.Join("/trae", "absent", "state.vscdb"),
	)
	require.NoError(err)
	assert.False(present)
	assert.Equal(reconciliationPageSize,
		spool.Metrics().MaxNonAuthoritativeScopeRows,
		"disk-backed cardinality may grow, but one retained write batch must stay page-bounded")
}

func TestReconciliationSpoolCancellationAndClosedErrors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	spool, err := newReconciliationSpool(filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(err)
	path := spool.path

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.ErrorIs(spool.Add(ctx, reconciliationCandidate{
		Provider: parser.AgentClaude, Identity: "cancelled", Path: "/cancelled",
	}), context.Canceled)

	require.NoError(spool.closeDB())
	err = spool.Add(t.Context(), reconciliationCandidate{
		Provider: parser.AgentClaude, Identity: "closed", Path: "/closed",
	})
	assert.Error(err)
	assert.NotErrorIs(err, context.Canceled)
	_, err = spool.Page(t.Context(), reconciliationCursor{}, reconciliationPageSize)
	assert.Error(err)

	require.NoError(spool.CloseAndRemove())
	_, err = os.Stat(path)
	assert.ErrorIs(err, os.ErrNotExist)
}

func assertSessionIdentity(i int) string {
	return fmt.Sprintf("session-%06d", i)
}
