package sync_test

import (
	"database/sql"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// skipCacheKeyForPath returns the one entry in a skip-cache snapshot whose
// filesystem-path component (everything before the "?agent=" qualifier)
// equals path exactly, so a container key is never confused with one of its
// "<db>#<composerID>" member keys.
func skipCacheKeyForPath(entries map[string]int64, path string) (string, bool) {
	for key := range entries {
		base, _, _ := strings.Cut(key, "?agent=")
		if base == path {
			return key, true
		}
	}
	return "", false
}

// TestReconcileWatchRootsAfterLostEventsSkipsUnchangedCursorIDEContainer
// covers case (a) from the overflow-keep-cursor-container-cache fix: after an
// initial sync and a lost-events reconcile, an unchanged Cursor IDE container
// is not re-parsed, and its whole-container skip-cache entry survives both in
// memory and in the persisted skipped_files table.
func TestReconcileWatchRootsAfterLostEventsSkipsUnchangedCursorIDEContainer(t *testing.T) {
	root, dbPath, composers := seedCursorIDEFreshness(t, 20)
	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, len(composers), engine.SyncAll(t.Context(), nil).Synced)

	containerKey, ok := skipCacheKeyForPath(engine.SnapshotSkipCache(), dbPath)
	require.True(t, ok, "a clean initial sync must cache the whole-container entry")
	persisted, err := database.LoadSkippedFiles(t.Context())
	require.NoError(t, err)
	assert.Contains(t, persisted, containerKey,
		"the container entry must be persisted to skipped_files")

	before := readCursorIDECounters()
	require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{root}, false,
	))
	delta := before.since()

	assert.Zero(t, delta.parses,
		"an unchanged container must not be re-parsed after lost-events recovery")
	assert.Zero(t, delta.digests,
		"an unchanged container must not re-digest any composer")

	afterCache := engine.SnapshotSkipCache()
	stillCached, ok := skipCacheKeyForPath(afterCache, dbPath)
	require.True(t, ok, "the container entry must survive in memory")
	assert.Equal(t, containerKey, stillCached,
		"the key, which carries the container hash and data version, must be unchanged")
	afterPersisted, err := database.LoadSkippedFiles(t.Context())
	require.NoError(t, err)
	assert.Contains(t, afterPersisted, containerKey,
		"the container entry must survive in skipped_files")

	for _, c := range composers {
		sess, err := database.GetSession(t.Context(), cursorIDESessionID(c))
		require.NoError(t, err)
		require.NotNil(t, sess, c.id)
	}
}

// TestReconcileWatchRootsAfterLostEventsReparsesChangedCursorIDEContainer
// covers case (b): a Cursor IDE container whose content changed after the
// initial sync (so its skip-cache key's source hash changes) is still fully
// reparsed by a lost-events reconcile, and the clean reparse re-caches the
// container (cache-after-write), so the next pass does not repeat the parse.
func TestReconcileWatchRootsAfterLostEventsReparsesChangedCursorIDEContainer(t *testing.T) {
	root, dbPath, composers := seedCursorIDEFreshness(t, 20)
	engine, database := newCursorIDESyncEngine(t, root)
	require.Equal(t, len(composers), engine.SyncAll(t.Context(), nil).Synced)

	containerKeyBefore, ok := skipCacheKeyForPath(engine.SnapshotSkipCache(), dbPath)
	require.True(t, ok)

	edited := appendCursorIDEReply(composers[0], 1782029000000)
	cursorIDEExec(t, dbPath, func(tx *sql.Tx) error {
		return putCursorIDEComposer(t, tx, edited)
	})

	before := readCursorIDECounters()
	require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{root}, false,
	))
	delta := before.since()

	assert.Equal(t, int64(len(composers)), delta.parses,
		"a changed container must still be fully reparsed after lost-events recovery")
	sess, err := database.GetSession(t.Context(), cursorIDESessionID(edited))
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 2, sess.MessageCount)

	containerKeyAfter, ok := skipCacheKeyForPath(engine.SnapshotSkipCache(), dbPath)
	require.True(t, ok,
		"the clean reparse triggered by lost events must re-cache the container")
	assert.NotEqual(t, containerKeyBefore, containerKeyAfter,
		"the changed container must get a new hash-bearing key")
	persisted, err := database.LoadSkippedFiles(t.Context())
	require.NoError(t, err)
	assert.Contains(t, persisted, containerKeyAfter,
		"the re-cached container entry must be persisted to skipped_files")

	// A second, now-unchanged lost-events reconcile must not reparse again:
	// the fix's two halves (retained entry, unforced ForceParse) only help
	// together.
	before = readCursorIDECounters()
	require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{root}, false,
	))
	assert.Zero(t, before.since().parses,
		"the newly re-cached container must be skipped on the following pass")
}
