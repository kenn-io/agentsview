package parser

import (
	"encoding/binary"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSQLiteContainerStateIgnoresSubSecondMtimeChanges pins the state's
// timestamp contract: mtime participates at whole-second granularity only,
// so sub-second timestamp instability (filesystems and stat round-trips
// disagree below one second) can never make an unchanged container look
// changed, while a change that crosses a second boundary still registers.
// Content changes are detected by SQLite's own write markers regardless of
// any timestamp.
func TestSQLiteContainerStateIgnoresSubSecondMtimeChanges(t *testing.T) {
	dbPath, _, db := newTestDB(t)
	require.NoError(t, db.Close())

	base := time.Unix(1700000000, 100*int64(time.Millisecond))
	require.NoError(t, os.Chtimes(dbPath, base, base))
	before, ok := StatSQLiteContainerState(dbPath)
	require.True(t, ok, "state must be readable")

	nudged := time.Unix(1700000000, 900*int64(time.Millisecond))
	require.NoError(t, os.Chtimes(dbPath, nudged, nudged))
	within, ok := StatSQLiteContainerState(dbPath)
	require.True(t, ok, "state must be readable after sub-second nudge")
	assert.Equal(t, before, within,
		"a sub-second mtime change must not change the container state")

	crossed := time.Unix(1700000001, 0)
	require.NoError(t, os.Chtimes(dbPath, crossed, crossed))
	afterSecond, ok := StatSQLiteContainerState(dbPath)
	require.True(t, ok, "state must be readable after second boundary")
	assert.NotEqual(t, before, afterSecond,
		"an mtime change across a second boundary must change the state")
}

// TestSQLiteContainerStateRejectsNonSQLiteFiles pins that a file without a
// SQLite header can never be captured, and therefore never trusted as an
// unchanged container.
func TestSQLiteContainerStateRejectsNonSQLiteFiles(t *testing.T) {
	path := t.TempDir() + "/opencode.db"
	require.NoError(t, os.WriteFile(
		path, []byte("not a sqlite database, padded to header size........................................................."), 0o644,
	))
	_, ok := StatSQLiteContainerState(path)
	assert.False(t, ok, "non-SQLite bytes must not produce a container state")
}

// TestSQLiteContainerStateDetectsFileReplacement pins that swapping the
// container for a different file is visible even when every header marker
// and the stat metadata coincide: a byte-identical copy at a new inode with
// a restored mtime must still change the state. Without file identity, a
// restored or replaced database landing in the same second with the same
// size and change counter would be indistinguishable from the original.
func TestSQLiteContainerStateDetectsFileReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file identity is unavailable on Windows")
	}
	dbPath, _, db := newTestDB(t)
	require.NoError(t, db.Close())

	before, ok := StatSQLiteContainerState(dbPath)
	require.True(t, ok, "state must be readable before replacement")

	raw, err := os.ReadFile(dbPath)
	require.NoError(t, err, "read container bytes")
	info, err := os.Stat(dbPath)
	require.NoError(t, err, "stat container")
	// Write the copy while the original still exists so the two files are
	// guaranteed distinct inodes, then rename over the original. A
	// remove-then-recreate at the same path can reuse the freed inode and
	// make the replacement genuinely indistinguishable.
	replacement := dbPath + ".replacement"
	require.NoError(t, os.WriteFile(replacement, raw, 0o644),
		"write replacement container with identical bytes")
	require.NoError(t, os.Rename(replacement, dbPath),
		"swap replacement over container")
	require.NoError(t,
		os.Chtimes(dbPath, info.ModTime(), info.ModTime()),
		"restore container mtime")

	after, ok := StatSQLiteContainerState(dbPath)
	require.True(t, ok, "state must be readable after replacement")
	assert.NotEqual(t, before, after,
		"a replaced container file must change the state")
}

// TestSQLiteContainerStateRejectsUnknownWALFormat pins that the WAL fields
// are only trusted when the WAL header carries the documented magic and
// format version (3007000). The salts and checkpoint sequence are what make
// the equality check sound; under any other format those bytes are opaque,
// so the container must fail closed to "never trusted" instead.
func TestSQLiteContainerStateRejectsUnknownWALFormat(t *testing.T) {
	writeWAL := func(t *testing.T, dbPath string, magic, version uint32) {
		t.Helper()
		header := make([]byte, 40)
		binary.BigEndian.PutUint32(header[0:4], magic)
		binary.BigEndian.PutUint32(header[4:8], version)
		binary.BigEndian.PutUint32(header[12:16], 7)  // checkpoint seq
		binary.BigEndian.PutUint32(header[16:20], 11) // salt-1
		binary.BigEndian.PutUint32(header[20:24], 13) // salt-2
		require.NoError(t, os.WriteFile(dbPath+"-wal", header, 0o644))
	}

	t.Run("documented header is read", func(t *testing.T) {
		dbPath, _, db := newTestDB(t)
		require.NoError(t, db.Close())
		writeWAL(t, dbPath, 0x377f0683, 3007000)
		state, ok := StatSQLiteContainerState(dbPath)
		require.True(t, ok, "documented WAL header must be readable")
		assert.Equal(t, uint32(7), state.WALCkptSeq)
		assert.Equal(t, uint32(11), state.WALSalt1)
		assert.Equal(t, uint32(13), state.WALSalt2)
	})

	t.Run("wrong magic fails closed", func(t *testing.T) {
		dbPath, _, db := newTestDB(t)
		require.NoError(t, db.Close())
		writeWAL(t, dbPath, 0xdeadbeef, 3007000)
		_, ok := StatSQLiteContainerState(dbPath)
		assert.False(t, ok,
			"a WAL without the documented magic must never be trusted")
	})

	t.Run("unknown version fails closed", func(t *testing.T) {
		dbPath, _, db := newTestDB(t)
		require.NoError(t, db.Close())
		writeWAL(t, dbPath, 0x377f0682, 3008000)
		_, ok := StatSQLiteContainerState(dbPath)
		assert.False(t, ok,
			"an unknown WAL format version must never be trusted")
	})
}

// TestOpenCodeProjectsCacheReusesUntilContainerChanges pins the
// project-table cache used by SQLite session parsing: while the container
// state is unchanged the previous load is served back, and any committed
// write invalidates it. Without the cache every parsed session re-queried
// the full project table, which dominated re-parse CPU on large archives.
func TestOpenCodeProjectsCacheReusesUntilContainerChanges(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()
	seeder.AddProject("prj_1", "/home/user/code/app-one")

	first, err := loadOpenCodeProjectsCached(db, dbPath)
	require.NoError(t, err)
	assert.Equal(t,
		map[string]string{"prj_1": "/home/user/code/app-one"}, first)

	// Poison the cached copy to make hits observable: an unchanged
	// container must serve the poisoned entry back, and a changed one must
	// reload the real values from the DB.
	openCodeProjectsCacheMu.Lock()
	entry := openCodeProjectsCache[dbPath]
	entry.projects = map[string]string{"prj_1": "cached-marker"}
	openCodeProjectsCache[dbPath] = entry
	openCodeProjectsCacheMu.Unlock()

	second, err := loadOpenCodeProjectsCached(db, dbPath)
	require.NoError(t, err)
	assert.Equal(t, "cached-marker", second["prj_1"],
		"an unchanged container must be served from the cache")

	_, err = db.Exec(
		"UPDATE project SET worktree = ? WHERE id = ?",
		"/home/user/code/renamed", "prj_1",
	)
	require.NoError(t, err)

	third, err := loadOpenCodeProjectsCached(db, dbPath)
	require.NoError(t, err)
	assert.Equal(t, "/home/user/code/renamed", third["prj_1"],
		"a committed write must invalidate the cached projects")
}
