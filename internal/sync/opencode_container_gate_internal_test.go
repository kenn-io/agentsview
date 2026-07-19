package sync

import (
	"database/sql"
	"os"
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

func testGateState(state parser.SQLiteContainerState) sqliteContainerGateState {
	return sqliteContainerGateState{sqlite: state}
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
			files, map[string]sqliteContainerGateState{},
		)
		e.noteSQLiteContainerResult("/data/opencode.db#ses-1", true)
		e.noteSQLiteContainerResult("/data/opencode.db#ses-2", true)
		e.finishSQLiteContainerPass(false, true)
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
			map[string]sqliteContainerGateState{dbPath: testGateState(pre)},
		)
		e.noteSQLiteContainerResult(dbPath+"#ses-1", true)
		e.noteSQLiteContainerResult(dbPath+"#ses-2", true)
		e.finishSQLiteContainerPass(false, true)
		require.Contains(t, e.trustedSQLiteContainers, dbPath)
		trusted := e.trustedSQLiteContainers[dbPath]
		assert.Equal(t, testGateState(pre), trusted.state,
			"trusted state must be exactly the pre-discovery capture")
		assert.Equal(t,
			map[string]struct{}{"ses-1": {}, "ses-2": {}},
			trusted.sessions,
			"trusted set must be exactly the verified session IDs")
	})
}

func TestProviderChangedPathStoredHintRootsTraeScopesToContainer(t *testing.T) {
	root := t.TempDir()
	workspaceWatchRoot := filepath.Join(root, "workspaceStorage")
	globalWatchRoot := filepath.Join(root, "globalStorage")

	workspaceManifest := filepath.Join(
		workspaceWatchRoot, "hash-a", "workspace.json",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(workspaceManifest), 0o755))
	assert.Equal(
		t,
		[]string{filepath.Join(workspaceWatchRoot, "hash-a", "state.vscdb")},
		providerChangedPathStoredHintRoots(
			parser.AgentTrae, workspaceWatchRoot, workspaceManifest,
		),
	)

	globalDB := filepath.Join(globalWatchRoot, "state.vscdb")
	require.NoError(t, os.MkdirAll(filepath.Dir(globalDB), 0o755))
	assert.Equal(
		t,
		[]string{globalDB},
		providerChangedPathStoredHintRoots(
			parser.AgentTrae, globalWatchRoot, globalDB+"-wal",
		),
	)
}

func TestCaptureSQLiteContainerStatesScopesChangedPathToImpactedContainer(t *testing.T) {
	firstDB, _ := newContainerTestDB(t)
	secondDB, _ := newContainerTestDB(t)
	engine := &Engine{
		agentDirs: map[parser.AgentType][]string{
			parser.AgentOpenCode: {
				filepath.Dir(firstDB),
				filepath.Dir(secondDB),
			},
		},
	}

	origStat := statSQLiteContainerState
	t.Cleanup(func() { statSQLiteContainerState = origStat })
	var statPaths []string
	statSQLiteContainerState = func(dbPath string) (parser.SQLiteContainerState, bool) {
		statPaths = append(statPaths, filepath.Clean(dbPath))
		return parser.StatSQLiteContainerState(dbPath)
	}

	states := engine.captureSQLiteContainerStates([]string{firstDB + "-wal"})
	require.Contains(t, states, firstDB)
	require.NotContains(t, states, secondDB)
	assert.Equal(t, []string{filepath.Clean(firstDB)}, uniqueContainerPaths(statPaths))
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
	e.trustedSQLiteContainers = map[string]trustedSQLiteContainer{
		dbPath: {state: testGateState(pre), sessions: map[string]struct{}{"ses-1": {}}},
	}

	// The container changes inside the capture-discovery window.
	_, err := conn.Exec("INSERT INTO session (id) VALUES ('ses-1')")
	require.NoError(t, err, "write session inside the window")

	file := parser.DiscoveredFile{
		Agent: parser.AgentOpenCode, Path: dbPath + "#ses-1",
	}
	e.beginSQLiteContainerPass(
		[]parser.DiscoveredFile{file},
		map[string]sqliteContainerGateState{dbPath: testGateState(pre)},
	)

	assert.False(t, e.sqliteContainerSourceFresh(file),
		"a mismatched container must not gate-skip its sessions")

	e.noteSQLiteContainerResult(file.Path, true)
	e.finishSQLiteContainerPass(false, true)
	assert.Equal(t, testGateState(pre), e.trustedSQLiteContainers[dbPath].state,
		"a mismatched container must not be promoted past its trusted state")
}

// TestSQLiteContainerGateParsesNewlyUnshadowedSession pins the hybrid-root
// invariant: hybrid discovery drops SQLite rows shadowed by a same-ID
// storage JSON, so the discoverable row set can grow — a storage JSON
// removed while the DB is untouched exposes its row — without the container
// state changing. Trust therefore records which session IDs the verified
// pass discovered, and only those may gate-skip; a newly exposed row was
// never verified against the archive and must parse.
func TestSQLiteContainerGateParsesNewlyUnshadowedSession(t *testing.T) {
	e := &Engine{}
	dbPath, _ := newContainerTestDB(t)
	state, ok := parser.StatSQLiteContainerState(dbPath)
	require.True(t, ok, "container state must be readable")

	// A fully verified pass discovered only ses-1; ses-2's row was
	// shadowed by its storage JSON at the time.
	verified := parser.DiscoveredFile{
		Agent: parser.AgentOpenCode, Path: dbPath + "#ses-1",
	}
	e.beginSQLiteContainerPass(
		[]parser.DiscoveredFile{verified},
		map[string]sqliteContainerGateState{dbPath: testGateState(state)},
	)
	e.noteSQLiteContainerResult(verified.Path, true)
	e.finishSQLiteContainerPass(false, true)
	require.Contains(t, e.trustedSQLiteContainers, dbPath)

	// The storage JSON is removed; the DB is untouched. The next pass
	// discovers ses-2's row for the first time.
	exposed := parser.DiscoveredFile{
		Agent: parser.AgentOpenCode, Path: dbPath + "#ses-2",
	}
	e.beginSQLiteContainerPass(
		[]parser.DiscoveredFile{verified, exposed},
		map[string]sqliteContainerGateState{dbPath: testGateState(state)},
	)
	assert.True(t, e.sqliteContainerSourceFresh(verified),
		"the verified session must still gate-skip")
	assert.False(t, e.sqliteContainerSourceFresh(exposed),
		"a newly exposed row must parse despite the unchanged container")
}

// TestSQLiteContainerFullPassDropsUndiscoveredTrust pins the stale-trust
// cleanup: a complete full-discovery pass that finds no sources for a
// trusted container (fully shadowed by storage JSONs, or gone) must drop
// its trusted entry — the session set is no longer being maintained, and
// stale membership would gate-skip a row re-exposed by a later storage
// removal that leaves the DB untouched. Scoped and incomplete passes see
// only a subset of roots, so absence there proves nothing and the entry
// must survive.
func TestSQLiteContainerFullPassDropsUndiscoveredTrust(t *testing.T) {
	trusted := func() map[string]trustedSQLiteContainer {
		return map[string]trustedSQLiteContainer{
			"/data/opencode.db": {
				sessions: map[string]struct{}{"ses-1": {}},
			},
		}
	}

	t.Run("full pass drops the undiscovered container", func(t *testing.T) {
		e := &Engine{}
		e.trustedSQLiteContainers = trusted()
		e.beginSQLiteContainerPass(nil, nil)
		e.finishSQLiteContainerPass(false, true)
		assert.Empty(t, e.trustedSQLiteContainers,
			"a full pass with no discovered sources must drop the trust")
	})

	t.Run("scoped pass keeps the entry", func(t *testing.T) {
		e := &Engine{}
		e.trustedSQLiteContainers = trusted()
		e.beginSQLiteContainerPass(nil, nil)
		e.finishSQLiteContainerPass(false, false)
		assert.Contains(t, e.trustedSQLiteContainers, "/data/opencode.db",
			"a scoped pass must not drop trust for out-of-scope containers")
	})

	t.Run("incomplete pass keeps the entry", func(t *testing.T) {
		e := &Engine{}
		e.trustedSQLiteContainers = trusted()
		e.beginSQLiteContainerPass(nil, nil)
		e.finishSQLiteContainerPass(true, true)
		assert.Contains(t, e.trustedSQLiteContainers, "/data/opencode.db",
			"an incomplete pass must not drop any trust")
	})
}
