//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// countReopenAliasFiles globs dir for the hardlink files openMirrorAlias
// creates (path.reopen-N). A non-zero count after a Store has fully settled
// and closed means an alias leaked instead of being removed by swapHandle
// or Store.Close.
func countReopenAliasFiles(t *testing.T, dir string) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.reopen-*"))
	require.NoError(t, err)
	return len(matches)
}

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

// TestStoreServesLatestAfterTwoConsecutiveMirrorReplacements chains a second
// rebuild-driven swap onto the reopen contract TestStoreReopensAfterMirrorReplacement
// covers once: a Store must not just survive one swap, it must keep working
// after the alias it swapped onto is itself replaced, and it must not leak
// the openMirrorAlias hardlink files that changeover uses (see mirror_watch.go).
func TestStoreServesLatestAfterTwoConsecutiveMirrorReplacements(t *testing.T) {
	skipReopenTestOnWindows(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "m.duckdb")
	buildMirrorFixture(t, path, "gen-1")

	store, err := NewStore(path)
	require.NoError(t, err)

	watchCtx, cancelWatch := context.WithCancel(context.Background())
	store.WatchMirrorReplacement(watchCtx, 20*time.Millisecond, nil)

	assert.Equal(t, []string{"gen-1"}, listMirrorSessionIDs(t, store))

	gen2Path := filepath.Join(dir, "gen2.duckdb")
	buildMirrorFixtureAt(t, gen2Path, "gen-2")
	require.NoError(t, os.Rename(gen2Path, path))
	require.Eventually(t, func() bool {
		ids := listMirrorSessionIDs(t, store)
		return len(ids) == 1 && ids[0] == "gen-2"
	}, 5*time.Second, 50*time.Millisecond, "store must adopt the first replacement")

	gen3Path := filepath.Join(dir, "gen3.duckdb")
	buildMirrorFixtureAt(t, gen3Path, "gen-3")
	require.NoError(t, os.Rename(gen3Path, path))
	require.Eventually(t, func() bool {
		ids := listMirrorSessionIDs(t, store)
		return len(ids) == 1 && ids[0] == "gen-3"
	}, 5*time.Second, 50*time.Millisecond, "store must adopt the second, consecutive replacement")

	// Stop polling and close before checking for leaks: Store.Close removes
	// the currently active alias (the one backing gen-3's handle), and the
	// gen-2 alias was already removed by the second swap itself.
	cancelWatch()
	require.NoError(t, store.Close())
	assert.Equal(t, 0, countReopenAliasFiles(t, dir),
		"no *.reopen-* hardlink files may remain once the store has settled and closed")
}

// TestStoreConcurrentReadsNeverFailAcrossMirrorReplacements is the FINDING 4
// regression: queryContext/queryRowContext used to snapshot the *sql.DB
// under a read lock, release the lock, and only then start the query.
// WatchMirrorReplacement's swapHandle could Close() the snapshotted handle
// in that gap, so readers racing a mirror adoption intermittently failed
// with "sql: database is closed". The read lock now spans the query start,
// which makes that window impossible; this test hammers several concurrent
// readers through a loop of rename-replacements (each adopted by the
// watcher's swapHandle) and requires zero read errors.
func TestStoreConcurrentReadsNeverFailAcrossMirrorReplacements(t *testing.T) {
	skipReopenTestOnWindows(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "m.duckdb")
	buildMirrorFixture(t, path, "gen-0")

	store, err := NewStore(path)
	require.NoError(t, err)
	defer store.Close()

	store.WatchMirrorReplacement(t.Context(), time.Millisecond, nil)

	readerCtx, cancelReaders := context.WithCancel(context.Background())
	var readErrs atomic.Int32
	var firstErr atomic.Pointer[string]
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for readerCtx.Err() == nil {
				if _, err := store.GetStats(
					context.Background(), false, false,
				); err != nil {
					readErrs.Add(1)
					msg := err.Error()
					firstErr.CompareAndSwap(nil, &msg)
				}
			}
		})
	}

	for gen := 1; gen <= 5; gen++ {
		sessionID := fmt.Sprintf("gen-%d", gen)
		next := filepath.Join(dir, fmt.Sprintf("next-%d.duckdb", gen))
		buildMirrorFixtureAt(t, next, sessionID)
		require.NoError(t, os.Rename(next, path))
		require.Eventually(t, func() bool {
			ids := listMirrorSessionIDs(t, store)
			return len(ids) == 1 && ids[0] == sessionID
		}, 5*time.Second, 2*time.Millisecond,
			"store must adopt replacement generation %d", gen)
	}

	cancelReaders()
	wg.Wait()
	errDetail := ""
	if msg := firstErr.Load(); msg != nil {
		errDetail = *msg
	}
	assert.Zero(t, readErrs.Load(),
		"concurrent reads must never fail across mirror adoptions; first error: %s",
		errDetail)
}

// TestStoreAdoptsGoodMirrorAfterIncompatibleReplacement extends
// TestStoreKeepsOldHandleWhenReplacementIncompatible: after the watcher
// rejects one incompatible replacement and reports it via onEvent, it must
// keep polling and pick up a subsequent good replacement rather than getting
// stuck refusing every future swap.
func TestStoreAdoptsGoodMirrorAfterIncompatibleReplacement(t *testing.T) {
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
	store.WatchMirrorReplacement(ctx, 20*time.Millisecond, onEvent)

	badPath := filepath.Join(dir, "bad.duckdb")
	buildIncompatibleMirrorFixture(t, badPath)
	require.NoError(t, os.Rename(badPath, path))

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(events) > 0
	}, 5*time.Second, 100*time.Millisecond, "onEvent should report the incompatible replacement")
	assert.Equal(t, []string{"old-session"}, listMirrorSessionIDs(t, store),
		"store must keep serving the old handle while the replacement is incompatible")

	goodPath := filepath.Join(dir, "good.duckdb")
	buildMirrorFixtureAt(t, goodPath, "recovered-session")
	require.NoError(t, os.Rename(goodPath, path))

	require.Eventually(t, func() bool {
		ids := listMirrorSessionIDs(t, store)
		return len(ids) == 1 && ids[0] == "recovered-session"
	}, 5*time.Second, 100*time.Millisecond,
		"store must adopt a good mirror that arrives after an incompatible one")
}

// TestSweepStaleMirrorReopenAliasesRemovesLeftoverAliases simulates the
// crash-leftover case SweepStaleMirrorReopenAliases exists to clean up: a
// previous serve process died without reaching Store.Close or a
// mirror-replacement swap, leaving its path.reopen-N hardlink behind. The
// next serve process must remove it (and any other reopen aliases for the
// same mirror) before opening its own handle, and must leave unrelated
// files alone.
func TestSweepStaleMirrorReopenAliasesRemovesLeftoverAliases(t *testing.T) {
	skipReopenTestOnWindows(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "m.duckdb")
	buildMirrorFixture(t, path, "session-1")

	require.NoError(t, os.Link(path, path+".reopen-1"))
	require.NoError(t, os.Link(path, path+".reopen-2"))
	otherPath := filepath.Join(dir, "other.duckdb")
	buildMirrorFixture(t, otherPath, "session-2")
	require.NoError(t, os.Link(otherPath, otherPath+".reopen-1"))
	// User files that merely share the literal ".reopen-" prefix are not
	// generated aliases (openMirrorAlias appends UnixNano digits only) and
	// must survive, as must a bare empty-suffix name.
	userBackup := path + ".reopen-backup"
	require.NoError(t, os.WriteFile(userBackup, []byte("keep me"), 0o644))
	emptySuffix := path + ".reopen-"
	require.NoError(t, os.WriteFile(emptySuffix, []byte("keep me"), 0o644))

	require.NoError(t, SweepStaleMirrorReopenAliases(path))

	assert.NoFileExists(t, path+".reopen-1")
	assert.NoFileExists(t, path+".reopen-2")
	assert.FileExists(t, otherPath+".reopen-1",
		"sweeping path's aliases must leave other.duckdb's alias untouched")
	assert.FileExists(t, userBackup,
		"a user file sharing the prefix but with a non-digit suffix must survive")
	assert.FileExists(t, emptySuffix,
		"a bare path.reopen- name (empty suffix) is not a generated alias and must survive")
	assert.FileExists(t, path, "sweep must not remove the mirror file itself")
	assert.FileExists(t, MirrorMarkerPath(path),
		"the sidecar ownership marker shares the mirror path prefix but must never be swept")
}

// TestSweepStaleMirrorReopenAliasesEmptyPathIsNoOp guards against a caller
// passing an empty path (e.g. a NewStoreFromDB / remote Quack config that
// has no local mirror file to sweep): it must return nil without globbing
// or attempting any filesystem operation.
func TestSweepStaleMirrorReopenAliasesEmptyPathIsNoOp(t *testing.T) {
	require.NoError(t, SweepStaleMirrorReopenAliases(""))
}

// TestSweepStaleMirrorReopenAliasesHandlesGlobMetacharactersInDirectory is
// the FIX7 regression: SweepStaleMirrorReopenAliases used to build its
// match pattern with filepath.Glob(path+".reopen-*"), so a project or
// archive directory name containing glob metacharacters ([, ?, *) would be
// interpreted as glob syntax instead of literal characters, breaking or
// over-matching the sweep. A literal os.ReadDir + prefix-match sweep must
// work the same way regardless of what characters appear in the directory
// name.
func TestSweepStaleMirrorReopenAliasesHandlesGlobMetacharactersInDirectory(t *testing.T) {
	skipReopenTestOnWindows(t)
	dir := filepath.Join(t.TempDir(), "proj[1]")
	require.NoError(t, os.Mkdir(dir, 0o755))
	path := filepath.Join(dir, "m.duckdb")
	buildMirrorFixture(t, path, "session-1")

	require.NoError(t, os.Link(path, path+".reopen-1"))

	require.NoError(t, SweepStaleMirrorReopenAliases(path))

	assert.NoFileExists(t, path+".reopen-1",
		"a reopen alias in a glob-metacharacter directory must still be swept")
	assert.FileExists(t, path, "sweep must not remove the mirror file itself")
}
