//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// buildMirrorFixture builds a fresh, on-disk, schema-v3-compatible mirror
// file at path containing a single session (sessionID). It uses the same
// rebuildMirror path production rebuilds use, so the resulting file is a
// realistic fixture: real schema, real metadata, real data.
func buildMirrorFixture(t *testing.T, path, sessionID string) {
	t.Helper()
	local := newLocalDB(t)
	ts := "2026-01-01T00:00:00.000Z"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: syncSession(sessionID, "alpha", sessionID+" first", ts, 1),
		Messages: []db.Message{
			syncMessage(sessionID, 0, "user", sessionID+" first", ts),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)

	_, err = rebuildMirror(
		context.Background(), path, local, "test-machine", SyncOptions{}, nil,
	)
	require.NoError(t, err)
}

// buildMirrorFixtureAt is buildMirrorFixture under a name that reads
// naturally when building a *second* fixture file to swap in later.
func buildMirrorFixtureAt(t *testing.T, path, sessionID string) {
	t.Helper()
	buildMirrorFixture(t, path, sessionID)
}

// buildIncompatibleMirrorFixture writes a DuckDB file at path that opens
// fine but fails CheckSchemaCompat: it has none of the mirror tables.
func buildIncompatibleMirrorFixture(t *testing.T, path string) {
	t.Helper()
	conn, err := Open(path)
	require.NoError(t, err)
	_, err = conn.ExecContext(
		context.Background(),
		`CREATE TABLE not_a_mirror_table (id TEXT)`,
	)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

func listMirrorSessionIDs(t *testing.T, store *Store) []string {
	t.Helper()
	rows, err := store.queryContext(
		context.Background(), "SELECT id FROM sessions ORDER BY id",
	)
	require.NoError(t, err)
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

func skipReopenTestOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("mirror swap relies on POSIX atomic rename semantics")
	}
}

func TestStoreReopensAfterMirrorReplacement(t *testing.T) {
	skipReopenTestOnWindows(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "m.duckdb")
	buildMirrorFixture(t, path, "old-session")

	store, err := NewStore(path)
	require.NoError(t, err)
	defer store.Close()

	ctx := t.Context()
	store.WatchMirrorReplacement(ctx, 50*time.Millisecond, nil)

	assert.Equal(t, []string{"old-session"}, listMirrorSessionIDs(t, store))

	nextPath := filepath.Join(dir, "next.duckdb")
	buildMirrorFixtureAt(t, nextPath, "new-session")
	require.NoError(t, os.Rename(nextPath, path))

	require.Eventually(t, func() bool {
		ids := listMirrorSessionIDs(t, store)
		return len(ids) == 1 && ids[0] == "new-session"
	}, 5*time.Second, 100*time.Millisecond)
}

func TestStoreKeepsOldHandleWhenReplacementIncompatible(t *testing.T) {
	skipReopenTestOnWindows(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "m.duckdb")
	buildMirrorFixture(t, path, "old-session")

	store, err := NewStore(path)
	require.NoError(t, err)
	defer store.Close()

	var mu sync.Mutex
	var events []error
	onEvent := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, err)
	}

	ctx := t.Context()
	store.WatchMirrorReplacement(ctx, 50*time.Millisecond, onEvent)

	badPath := filepath.Join(dir, "bad.duckdb")
	buildIncompatibleMirrorFixture(t, badPath)
	require.NoError(t, os.Rename(badPath, path))

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(events) > 0
	}, 5*time.Second, 100*time.Millisecond, "onEvent should report the incompatible replacement")

	mu.Lock()
	for _, err := range events {
		assert.Error(t, err)
	}
	mu.Unlock()

	assert.Equal(t, []string{"old-session"}, listMirrorSessionIDs(t, store),
		"store must keep serving the old handle when the replacement is incompatible")
}
