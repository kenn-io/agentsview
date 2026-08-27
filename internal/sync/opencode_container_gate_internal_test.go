package sync

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
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

// newCompositeContainerTestDB creates an OpenCode container whose schema
// carries the composite change-signal columns and session_id indexes, so
// watermark-only listings are supported.
func newCompositeContainerTestDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "open container db")
	t.Cleanup(func() { _ = conn.Close() })
	_, err = conn.Exec(`
		CREATE TABLE project (
			id TEXT PRIMARY KEY,
			worktree TEXT NOT NULL,
			time_updated INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE session (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL
		);
		CREATE TABLE message (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			data TEXT NOT NULL,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE part (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			message_id TEXT NOT NULL,
			data TEXT NOT NULL,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX message_session_idx ON message (session_id);
		CREATE INDEX part_session_idx ON part (session_id);
	`)
	require.NoError(t, err, "create composite schema")
	return path, conn
}

// seedCoveredVirtualMember stores one virtual member whose stored freshness
// fully covers watermarkMS, stamped with the current data version as a
// completed parse would be (UpsertSession seeds data_version 0 by design).
func seedCoveredVirtualMember(
	t *testing.T, database *db.DB, sessionID, virtualPath string,
	watermarkMS int64,
) {
	t.Helper()
	storedMtime := watermarkMS * 1_000_000
	require.NoError(t, database.UpsertSession(db.Session{
		ID: sessionID, Agent: "opencode", Project: "project",
		Machine: "local", FilePath: &virtualPath, FileMtime: &storedMtime,
	}))
	require.NoError(t, database.SetSessionDataVersion(
		sessionID, db.CurrentDataVersion(),
	))
}

// TestStoredMemberFreshnessPagerEmitsOnlyVouchableRows pins the pager's
// translation of stored rows into coverage authority: rows behind the
// current data version are omitted entirely so their sources stay listed,
// a stored child digest yields its embedded session/project metadata
// watermark, and a plain fingerprint falls back to the stored composite.
func TestStoredMemberFreshnessPagerEmitsOnlyVouchableRows(t *testing.T) {
	database := openTestDB(t)
	const container = "/data/opencode.db"
	seedCoveredVirtualMember(t, database, "opencode:a", container+"#a", 100)

	digest := "opencode-child:v1:900:20:30:1:2:abcd"
	digestPath := container + "#b"
	digestMtime := int64(900) * 1_000_000
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "opencode:b", Agent: "opencode", Project: "project",
		Machine: "local", FilePath: &digestPath, FileMtime: &digestMtime,
		FileHash: &digest,
	}))
	require.NoError(t, database.SetSessionDataVersion(
		"opencode:b", db.CurrentDataVersion(),
	))

	stalePath := container + "#c"
	staleMtime := int64(100) * 1_000_000
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "opencode:c", Agent: "opencode", Project: "project",
		Machine: "local", FilePath: &stalePath, FileMtime: &staleMtime,
	}))

	e := &Engine{db: database, machine: "local"}
	rows, done, err := e.storedMemberFreshnessPager(container)(
		t.Context(), "", 10,
	)
	require.NoError(t, err)
	assert.True(t, done)
	require.Len(t, rows, 2,
		"the stale-version row must not be emitted at all")
	assert.Equal(t, container+"#a", rows[0].Path)
	assert.Equal(t, int64(100)*1_000_000, rows[0].CoveredThroughNS,
		"a plain fingerprint falls back to the stored composite")
	assert.Equal(t, container+"#b", rows[1].Path)
	assert.Equal(t, int64(30)*1_000_000, rows[1].CoveredThroughNS,
		"a child digest yields its embedded metadata watermark")
}

// TestStoredMemberFreshnessPagerAdvancesPastAllStalePages pins the pager's
// raw-cursor advance: version-stale rows are withheld from the emitted page,
// and when a whole raw page is stale the pager must keep reading from the
// raw cursor instead of returning an empty not-done page — the merge cursor
// reads that as exhaustion, which would silently un-cover every stored
// member past the first all-stale page and let one event's work scale with
// the remainder of the archive.
func TestStoredMemberFreshnessPagerAdvancesPastAllStalePages(t *testing.T) {
	database := openTestDB(t)
	const container = "/data/opencode.db"
	// Two stale-version members sort before the covered current-version
	// member, so a limit-2 first page is entirely withheld.
	for _, id := range []string{"a", "b"} {
		path := container + "#" + id
		mtime := int64(100) * 1_000_000
		require.NoError(t, database.UpsertSession(db.Session{
			ID: "opencode:" + id, Agent: "opencode", Project: "project",
			Machine: "local", FilePath: &path, FileMtime: &mtime,
		}))
	}
	seedCoveredVirtualMember(t, database, "opencode:c", container+"#c", 500)

	e := &Engine{db: database, machine: "local"}
	rows, done, err := e.storedMemberFreshnessPager(container)(
		t.Context(), "", 2,
	)
	require.NoError(t, err)
	require.Len(t, rows, 1,
		"the pager must advance past the all-stale page to the vouchable row")
	assert.Equal(t, container+"#c", rows[0].Path)
	assert.Equal(t, int64(500)*1_000_000, rows[0].CoveredThroughNS)
	assert.True(t, done)
}

// TestClassifyChangedPathWatermarkMergeRelistsOnStaleCapture pins the
// classification-time capture guard around the merged listing: while the
// container provably has not changed across the listing window, covered
// members are dropped during the stream and a fully covered container
// classifies to nothing; when every recapture differs from the pre-listing
// capture, the merge cannot be trusted and classification re-lists without
// stored authority, keeping every member for the per-file gates.
func TestClassifyChangedPathWatermarkMergeRelistsOnStaleCapture(t *testing.T) {
	dbPath, conn := newCompositeContainerTestDB(t)
	const base = int64(1779012000000)
	for _, id := range []string{"ses-1", "ses-2"} {
		_, err := conn.Exec(
			"INSERT INTO session (id, project_id, time_created, time_updated)"+
				" VALUES (?, 'proj', ?, ?)",
			id, base, base,
		)
		require.NoError(t, err, "insert session row")
	}

	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOpenCode: {filepath.Dir(dbPath)},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	seedCoveredVirtualMember(t, database, "opencode:ses-1", dbPath+"#ses-1", base)
	seedCoveredVirtualMember(t, database, "opencode:ses-2", dbPath+"#ses-2", base)

	files, err := engine.classifyProviderChangedPath(t.Context(), dbPath)
	require.NoError(t, err)
	assert.Empty(t, files,
		"a fully covered container classifies to nothing under a live capture")

	// A capture that never repeats: the post-listing revalidation always
	// mismatches, so the merged listing must be discarded and re-listed
	// without stored authority.
	orig := statSQLiteContainerState
	t.Cleanup(func() { statSQLiteContainerState = orig })
	var drift int64
	statSQLiteContainerState = func(
		path string,
	) (parser.SQLiteContainerState, bool) {
		state, ok := orig(path)
		drift++
		state.DBSize += drift
		return state, ok
	}

	files, err = engine.classifyProviderChangedPath(t.Context(), dbPath)
	require.NoError(t, err)
	assert.Len(t, files, 2,
		"a stale capture must keep every member for the per-file gates")
}

// TestDiscoveredFileWatermarkCutoffRequiresLiveCapture pins cutoff
// filtering's trust in carried session-row watermarks: the carried value may
// decide the incremental cutoff only while the pass's container capture is
// live. A child-only commit landing during discovery leaves the session-row
// watermark behind the live composite; if the stale carried value were
// trusted after the recapture invalidated the pass, the file would fall
// below the cutoff and be dropped before full fingerprinting ever saw the
// update. Without a live capture the effective mtime must resolve the live
// composite instead.
func TestDiscoveredFileWatermarkCutoffRequiresLiveCapture(t *testing.T) {
	dbPath, conn := newCompositeContainerTestDB(t)
	const sessionRow = int64(1779012000000)
	const childWrite = int64(1779012500000)
	_, err := conn.Exec(
		"INSERT INTO session (id, project_id, time_created, time_updated)"+
			" VALUES ('ses-1', 'proj', ?, ?)",
		sessionRow, sessionRow,
	)
	require.NoError(t, err, "insert session row")
	_, err = conn.Exec(
		"INSERT INTO message (id, session_id, data, time_created, time_updated)"+
			" VALUES ('msg-1', 'ses-1', '{}', ?, ?)",
		childWrite, childWrite,
	)
	require.NoError(t, err, "insert message row")

	root := filepath.Dir(dbPath)
	provider, ok := parser.NewProvider(
		parser.AgentOpenCode,
		parser.ProviderConfig{Roots: []string{root}, Machine: "local"},
	)
	require.True(t, ok)
	sources, err := provider.SourcesForChangedPath(
		t.Context(), parser.ChangedPathRequest{
			Path: dbPath, WatchRoot: root, AllowWatermarkOnlySources: true,
		},
	)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	carried, watermarkOnly := parser.SourceWatermarkOnlyMTimeNS(sources[0])
	require.True(t, watermarkOnly)
	require.Equal(t, sessionRow*1_000_000, carried,
		"the carried watermark must be the session row alone")

	engine := NewEngine(openTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOpenCode: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	file := parser.DiscoveredFile{
		Agent:           parser.AgentOpenCode,
		Path:            sources[0].DisplayPath,
		ProviderSource:  &sources[0],
		ProviderProcess: true,
	}

	// No live capture: the stale carried watermark cannot decide the
	// cutoff, so the live composite (dominated by the child write) decides.
	mtime, err := engine.discoveredFileEffectiveMtime(t.Context(), file)
	require.NoError(t, err)
	assert.Equal(t, childWrite*1_000_000, mtime,
		"without a live capture the effective mtime is the live composite")

	// With a live, matching capture the carried watermark is trusted.
	pre, ok := statSQLiteContainerState(dbPath)
	require.True(t, ok)
	engine.beginSQLiteContainerPass(
		[]parser.DiscoveredFile{file},
		map[string]parser.SQLiteContainerState{dbPath: pre},
	)
	mtime, err = engine.discoveredFileEffectiveMtime(t.Context(), file)
	require.NoError(t, err)
	assert.Equal(t, carried, mtime,
		"a live capture lets the carried watermark decide the cutoff")
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
			map[string]parser.SQLiteContainerState{dbPath: pre},
		)
		e.noteSQLiteContainerResult(dbPath+"#ses-1", true)
		e.noteSQLiteContainerResult(dbPath+"#ses-2", true)
		e.finishSQLiteContainerPass(false, true)
		require.Contains(t, e.trustedSQLiteContainers, dbPath)
		trusted := e.trustedSQLiteContainers[dbPath]
		assert.Equal(t, pre, trusted.state,
			"trusted state must be exactly the pre-discovery capture")
	})
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
	assert.Equal(t, []string{filepath.Clean(firstDB)}, statPaths)
}

func TestOpenCodeChannelContainerGateRoutesVirtualPath(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "opencode-local.db")
	virtual := parser.OpenCodeSQLiteVirtualPath(dbPath, "ses-channel")

	gotDB, gotID, ok := sqliteContainerSourceForFile(parser.DiscoveredFile{
		Agent: parser.AgentOpenCode,
		Path:  virtual,
	})
	require.True(t, ok)
	assert.Equal(t, dbPath, gotDB)
	assert.Equal(t, "ses-channel", gotID)
	assert.Equal(t, dbPath, sqliteContainerPathForResultPath(virtual))
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
		dbPath: {state: pre},
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
	e.finishSQLiteContainerPass(false, true)
	assert.Equal(t, pre, e.trustedSQLiteContainers[dbPath].state,
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
	archive := openTestDB(t)
	e := &Engine{db: archive}
	dbPath, _ := newContainerTestDB(t)
	state, ok := parser.StatSQLiteContainerState(dbPath)
	require.True(t, ok, "container state must be readable")

	// A fully verified pass discovered only ses-1; ses-2's row was
	// shadowed by its storage JSON at the time.
	verified := parser.DiscoveredFile{
		Agent: parser.AgentOpenCode, Path: dbPath + "#ses-1",
	}
	verifiedPath := verified.Path
	replacementPath := filepath.Join(t.TempDir(), "ses-2.json")
	for _, session := range []db.Session{
		{ID: "opencode:ses-1", Agent: "opencode", Project: "project", Machine: "local", FilePath: &verifiedPath},
		{ID: "opencode:ses-2", Agent: "opencode", Project: "project", Machine: "local", FilePath: &replacementPath},
	} {
		require.NoError(t, archive.UpsertSession(session))
		require.NoError(t, archive.SetSessionDataVersion(session.ID, db.CurrentDataVersion()))
	}
	e.beginSQLiteContainerPass(
		[]parser.DiscoveredFile{verified},
		map[string]parser.SQLiteContainerState{dbPath: state},
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
		map[string]parser.SQLiteContainerState{dbPath: state},
	)
	assert.True(t, e.sqliteContainerSourceFresh(verified),
		"the verified session must still gate-skip")
	assert.False(t, e.sqliteContainerSourceFresh(exposed),
		"a newly exposed row must parse despite the unchanged container")
}

// TestSQLiteContainerScopedPassDoesNotPromoteUndiscoveredContainer pins the
// promotion precondition: a pass may only trust a container it actually
// verified, meaning it discovered (and completed) at least one of its
// sessions. Scoped reconciliations and scoped syncs capture every configured
// container's state up front (captureSQLiteContainerStates(nil)) but discover
// only in-scope sources, so an out-of-scope container ends the pass with
// discovered == completed == 0. Promoting its freshly captured state would
// mark a change that was never parsed as verified, and the next covering
// pass would gate-skip the changed sessions, leaving the archive stale.
func TestSQLiteContainerScopedPassDoesNotPromoteUndiscoveredContainer(t *testing.T) {
	archive := openTestDB(t)
	e := &Engine{db: archive}
	dbPath, conn := newContainerTestDB(t)
	pre, ok := parser.StatSQLiteContainerState(dbPath)
	require.True(t, ok, "container state must be readable")

	file := parser.DiscoveredFile{
		Agent: parser.AgentOpenCode, Path: dbPath + "#ses-1",
	}
	filePath := file.Path
	session := db.Session{
		ID: "opencode:ses-1", Agent: "opencode", Project: "project",
		Machine: "local", FilePath: &filePath,
	}
	require.NoError(t, archive.UpsertSession(session))
	require.NoError(t, archive.SetSessionDataVersion(
		session.ID, db.CurrentDataVersion(),
	))

	// A fully verified pass trusts the container at its current state.
	e.beginSQLiteContainerPass(
		[]parser.DiscoveredFile{file},
		map[string]parser.SQLiteContainerState{dbPath: pre},
	)
	e.noteSQLiteContainerResult(file.Path, true)
	e.finishSQLiteContainerPass(false, true)
	require.Contains(t, e.trustedSQLiteContainers, dbPath)

	// The container changes after the verified pass.
	_, err := conn.Exec("INSERT INTO session (id) VALUES ('ses-1')")
	require.NoError(t, err, "write session after the verified pass")
	changed, ok := parser.StatSQLiteContainerState(dbPath)
	require.True(t, ok, "changed container state must be readable")
	require.NotEqual(t, pre, changed,
		"the write must change the container state")

	// A scoped pass elsewhere captures every configured container but
	// discovers none of this one's sessions.
	e.beginSQLiteContainerPass(
		nil, map[string]parser.SQLiteContainerState{dbPath: changed},
	)
	e.finishSQLiteContainerPass(false, false)

	// The next covering pass must parse the changed session, not gate-skip
	// it against a state that was never verified.
	e.beginSQLiteContainerPass(
		[]parser.DiscoveredFile{file},
		map[string]parser.SQLiteContainerState{dbPath: changed},
	)
	assert.False(t, e.sqliteContainerSourceFresh(file),
		"a container changed while out of scope must not gate-skip after a scoped pass")
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
			"/data/opencode.db": {},
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
