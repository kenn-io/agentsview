package sync

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

// newContainerTestDB creates a real SQLite file named like an OpenCode
// container, so the pass's post-discovery recapture has something to stat.
func newContainerTestDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "open container db")
	t.Cleanup(func() { _ = conn.Close() })
	_, err = conn.Exec("CREATE TABLE session (id TEXT PRIMARY KEY)")
	require.NoError(t, err, "create session table")
	return path, conn
}

// TestSQLiteContainerPassPromotesOnlyPreDiscoveryCaptures pins the gate's
// ordering invariant: the state promoted to trusted must have been captured
// BEFORE discovery listed the container's sessions. Discovery reads the
// session rows first, so a state captured afterwards can be newer than the
// discovered set — a session written in between would then be gate-skipped
// forever without ever being parsed. Containers with no pre-discovery
// capture must therefore never be promoted, and promoted states must be
// exactly the pre-discovery ones.
func TestSQLiteContainerPassPromotesOnlyPreDiscoveryCaptures(t *testing.T) {
	t.Run("missing pre-discovery capture blocks promotion", func(t *testing.T) {
		e := &Engine{}
		files := []parser.DiscoveredFile{
			{Agent: parser.AgentOpenCode, Path: "/data/opencode.db#ses-1"},
			{Agent: parser.AgentOpenCode, Path: "/data/opencode.db#ses-2"},
		}
		e.beginSQLiteContainerPass(
			files, map[string]parser.SQLiteContainerState{},
		)
		e.noteSQLiteContainerResult("/data/opencode.db#ses-1", true)
		e.noteSQLiteContainerResult("/data/opencode.db#ses-2", true)
		e.finishSQLiteContainerPass(false)
		assert.Empty(t, e.trustedSQLiteContainers,
			"a container without a pre-discovery capture must not be trusted")
	})

	t.Run("promoted state is the pre-discovery capture", func(t *testing.T) {
		e := &Engine{}
		dbPath, _ := newContainerTestDB(t)
		pre, ok := parser.StatSQLiteContainerState(dbPath)
		require.True(t, ok, "container state must be readable")
		files := []parser.DiscoveredFile{
			{Agent: parser.AgentOpenCode, Path: dbPath + "#ses-1"},
			{Agent: parser.AgentOpenCode, Path: dbPath + "#ses-2"},
		}
		e.beginSQLiteContainerPass(
			files,
			map[string]parser.SQLiteContainerState{dbPath: pre},
		)
		e.noteSQLiteContainerResult(dbPath+"#ses-1", true)
		e.noteSQLiteContainerResult(dbPath+"#ses-2", true)
		e.finishSQLiteContainerPass(false)
		require.Contains(t, e.trustedSQLiteContainers, dbPath)
		assert.Equal(t, pre, e.trustedSQLiteContainers[dbPath],
			"trusted state must be exactly the pre-discovery capture")
	})
}

// TestSQLiteContainerPassFailsOnCaptureDiscoveryMismatch pins the pass's
// recapture check: a container that changed between the pre-discovery
// capture and pass begin must neither gate-skip nor be promoted. The
// discovered session set may already include the change, so gating against
// the pre-discovery state — which still matches the trusted state — would
// skip the changed sessions for the whole pass.
func TestSQLiteContainerPassFailsOnCaptureDiscoveryMismatch(t *testing.T) {
	e := &Engine{}
	dbPath, conn := newContainerTestDB(t)
	pre, ok := parser.StatSQLiteContainerState(dbPath)
	require.True(t, ok, "container state must be readable")
	// The container is trusted at the pre-discovery state, as after a
	// fully verified idle pass.
	e.trustedSQLiteContainers = map[string]parser.SQLiteContainerState{
		dbPath: pre,
	}

	// The container changes inside the capture-discovery window.
	_, err := conn.Exec("INSERT INTO session (id) VALUES ('ses-1')")
	require.NoError(t, err, "write session inside the window")

	file := parser.DiscoveredFile{
		Agent: parser.AgentOpenCode, Path: dbPath + "#ses-1",
	}
	e.beginSQLiteContainerPass(
		[]parser.DiscoveredFile{file},
		map[string]parser.SQLiteContainerState{dbPath: pre},
	)

	assert.False(t, e.sqliteContainerSourceFresh(file),
		"a mismatched container must not gate-skip its sessions")

	e.noteSQLiteContainerResult(file.Path, true)
	e.finishSQLiteContainerPass(false)
	assert.Equal(t, pre, e.trustedSQLiteContainers[dbPath],
		"a mismatched container must not be promoted past its trusted state")
}
