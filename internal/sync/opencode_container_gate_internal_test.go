package sync

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

type reconciliationSourceStateTestProvider struct {
	parser.Provider
	source   parser.SourceRef
	state    parser.ReconciliationSourceState
	applied  parser.ReconciliationSourceState
	applyErr error
}

func (p *reconciliationSourceStateTestProvider) Definition() parser.AgentDef {
	return parser.AgentDef{Type: parser.AgentOpenCode}
}

func (p *reconciliationSourceStateTestProvider) SourceForReconciliation(
	context.Context, string, string,
) (parser.SourceRef, bool, error) {
	return p.source, true, nil
}

func (p *reconciliationSourceStateTestProvider) ReconciliationSourceState(
	parser.SourceRef,
) (parser.ReconciliationSourceState, bool) {
	return p.state, true
}

func (p *reconciliationSourceStateTestProvider) SourcesForChangedPath(
	context.Context, parser.ChangedPathRequest,
) ([]parser.SourceRef, error) {
	return []parser.SourceRef{p.source}, nil
}

func (p *reconciliationSourceStateTestProvider) ApplyReconciliationSourceState(
	_ *parser.SourceRef, state parser.ReconciliationSourceState,
) error {
	if p.applyErr != nil {
		return p.applyErr
	}
	p.applied = state
	return nil
}

func TestReconciliationCandidateCarriesStateAcrossSpool(t *testing.T) {
	container, _ := newContainerTestDB(t)
	root := filepath.Dir(container)
	archive := openTestDB(t)
	engine := NewEngine(archive, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOpenCode: {root},
		},
	})
	t.Cleanup(engine.Close)

	source := parser.SourceRef{
		Provider:       parser.AgentOpenCode,
		DisplayPath:    container + "#ses_a",
		FingerprintKey: container + "#ses_a",
		Key:            container + "#ses_a",
	}
	state := parser.ReconciliationSourceState{
		Version: 1,
		Payload: []byte("full-discovery-state"),
	}
	discoveryProvider := &reconciliationSourceStateTestProvider{
		source: source,
		state:  state,
	}
	candidate, ok := engine.reconciliationCandidate(
		discoveryProvider, source, []string{root}, nil,
	)
	require.True(t, ok)

	spool, err := newReconciliationSpool(archive.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spool.CloseAndRemove()) })
	require.NoError(t, spool.Add(t.Context(), candidate))
	page, err := spool.Page(t.Context(), reconciliationCursor{}, 1)
	require.NoError(t, err)
	require.Len(t, page, 1)

	rehydrationProvider := &reconciliationSourceStateTestProvider{
		source: source,
	}
	files, err := engine.rehydrateReconciliationPage(
		t.Context(), page,
		map[parser.AgentType]parser.Provider{
			parser.AgentOpenCode: rehydrationProvider,
		},
		false,
	)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, state, rehydrationProvider.applied)
}

func TestReconciliationMalformedStateFallsBackToAuthoritativeSource(t *testing.T) {
	source := parser.SourceRef{
		Provider:       parser.AgentOpenCode,
		DisplayPath:    "/data/opencode.db#ses_a",
		FingerprintKey: "/data/opencode.db#ses_a",
		Key:            "/data/opencode.db#ses_a",
	}
	provider := &reconciliationSourceStateTestProvider{
		source: source,
		state: parser.ReconciliationSourceState{
			Version: 1,
			Payload: []byte("malformed"),
		},
		applyErr: errors.New("invalid state"),
	}
	candidate := reconciliationCandidate{
		Provider:    parser.AgentOpenCode,
		Identity:    "ses_a",
		Path:        source.DisplayPath,
		SourceState: provider.state,
	}

	files, err := (&Engine{}).rehydrateReconciliationPage(
		t.Context(), []reconciliationCandidate{candidate},
		map[parser.AgentType]parser.Provider{
			parser.AgentOpenCode: provider,
		},
		false,
	)
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.NotNil(t, files[0].ProviderSource)
	assert.Equal(t, source.DisplayPath, files[0].ProviderSource.DisplayPath)
	assert.Empty(t, provider.applied,
		"malformed optional state must not be applied")
}

func TestReconciliationStateFallsBackAfterContainerChanges(t *testing.T) {
	container, conn := newContainerTestDB(t)
	source := parser.SourceRef{
		Provider:       parser.AgentOpenCode,
		DisplayPath:    container + "#ses_a",
		FingerprintKey: container + "#ses_a",
		Key:            container + "#ses_a",
	}
	provider := &reconciliationSourceStateTestProvider{
		source: source,
		state: parser.ReconciliationSourceState{
			Version: 1,
			Payload: []byte("discovery-state"),
		},
	}
	before, ok := parser.StatSQLiteContainerState(container)
	require.True(t, ok, "container state must be readable")
	engine := &Engine{}
	engine.beginStreamingSQLiteContainerPass(
		map[string]parser.SQLiteContainerState{container: before},
	)

	origStat := statSQLiteContainerState
	t.Cleanup(func() { statSQLiteContainerState = origStat })
	statCalls := 0
	statSQLiteContainerState = func(path string) (parser.SQLiteContainerState, bool) {
		state, ok := origStat(path)
		statCalls++
		if statCalls == 1 {
			_, err := conn.Exec("INSERT INTO session (id) VALUES ('ses_a')")
			require.NoError(t, err, "change container after page refresh")
		}
		return state, ok
	}
	files, err := engine.rehydrateReconciliationPage(
		t.Context(), []reconciliationCandidate{{
			Provider:    parser.AgentOpenCode,
			Identity:    "ses_a",
			Path:        source.DisplayPath,
			SourceState: provider.state,
		}},
		map[parser.AgentType]parser.Provider{
			parser.AgentOpenCode: provider,
		},
		false,
	)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Empty(t, provider.applied,
		"stale discovery state must not be applied after container change")
	assert.True(t, engine.containerPass.failed[container],
		"changed container must fail the current reconciliation pass")
}

func TestFailedSQLiteContainerPassDropsCarriedProviderState(t *testing.T) {
	container, _ := newContainerTestDB(t)
	source := parser.SourceRef{
		Provider:       parser.AgentOpenCode,
		DisplayPath:    container + "#ses_a",
		FingerprintKey: container + "#ses_a",
		Key:            container + "#ses_a",
	}
	engine := &Engine{}
	engine.beginStreamingSQLiteContainerPass(
		map[string]parser.SQLiteContainerState{
			container: {},
		},
	)
	engine.containerPass.failed[container] = true
	file := parser.DiscoveredFile{
		Agent:          parser.AgentOpenCode,
		Path:           source.DisplayPath,
		ProviderSource: &source,
	}

	engine.discardStaleSQLiteProviderSource(&file)

	assert.Nil(t, file.ProviderSource,
		"failed container must re-resolve instead of using carried state")
}

// TestMidPassContainerWriteDropsCarriedProviderState pins the worker-boundary
// recheck: a write landing after the post-discovery recapture invalidates the
// carried full-digest source, so the changed session re-resolves live instead
// of skipping on its pre-change digest.
func TestMidPassContainerWriteDropsCarriedProviderState(t *testing.T) {
	container, conn := newContainerTestDB(t)
	source := parser.SourceRef{
		Provider:       parser.AgentOpenCode,
		DisplayPath:    container + "#ses_a",
		FingerprintKey: container + "#ses_a",
		Key:            container + "#ses_a",
	}
	pre, ok := parser.StatSQLiteContainerState(container)
	require.True(t, ok, "container state must be readable")
	engine := &Engine{}
	engine.beginStreamingSQLiteContainerPass(
		map[string]parser.SQLiteContainerState{container: pre},
	)
	file := parser.DiscoveredFile{
		Agent:          parser.AgentOpenCode,
		Path:           source.DisplayPath,
		ProviderSource: &source,
	}

	engine.discardStaleSQLiteProviderSource(&file)
	require.NotNil(t, file.ProviderSource,
		"an unchanged capture keeps the carried source")

	_, err := conn.Exec("INSERT INTO session (id) VALUES ('ses_a')")
	require.NoError(t, err, "write container after recapture")

	engine.discardStaleSQLiteProviderSource(&file)
	assert.Nil(t, file.ProviderSource,
		"a mid-pass container write must drop the carried source")
	assert.True(t, engine.containerPass.failed[container],
		"the recheck must fail the container for the rest of the pass")

	// A failure recorded by another worker after this file's recheck must
	// still drop the carried source at the consumption boundary.
	late := parser.DiscoveredFile{
		Agent:          parser.AgentOpenCode,
		Path:           source.DisplayPath,
		ProviderSource: &source,
	}
	engine.discardFailedSQLiteProviderSource(&late)
	assert.Nil(t, late.ProviderSource,
		"a recorded container failure must drop carried sources without a stat")
}

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
			parent_id TEXT,
			title TEXT,
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
	e.digestVerifiedAt = map[string]time.Time{
		dbPath: time.Unix(100, 0),
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
	assert.NotContains(t, e.digestVerifiedAt, dbPath,
		"a mismatched capture must invalidate verification age")

	t.Run("missing pre-discovery capture invalidates old verification", func(t *testing.T) {
		e := &Engine{
			trustedSQLiteContainers: map[string]trustedSQLiteContainer{
				dbPath: {state: pre},
			},
			digestVerifiedAt: map[string]time.Time{
				dbPath: time.Unix(100, 0),
			},
		}
		e.beginSQLiteContainerPass(
			[]parser.DiscoveredFile{file}, map[string]parser.SQLiteContainerState{},
		)
		e.noteSQLiteContainerResult(file.Path, true)
		e.finishSQLiteContainerPass(false, true)
		assert.NotContains(t, e.digestVerifiedAt, dbPath,
			"a missing capture must invalidate verification age")
	})
}

// TestSQLiteContainerPassRechecksDigestCaptureAtFinalization ensures a late
// write invalidates only its own full-digest promotion.
func TestSQLiteContainerPassRechecksDigestCaptureAtFinalization(t *testing.T) {
	changedDB, changedConn := newContainerTestDB(t)
	unchangedDB, _ := newContainerTestDB(t)
	changedState, ok := parser.StatSQLiteContainerState(changedDB)
	require.True(t, ok, "changed container state must be readable")
	unchangedState, ok := parser.StatSQLiteContainerState(unchangedDB)
	require.True(t, ok, "unchanged container state must be readable")

	verifiedAt := time.Unix(100, 0)
	now := time.Unix(200, 0)
	origNow := openCodeContainerDigestVerifyNow
	t.Cleanup(func() { openCodeContainerDigestVerifyNow = origNow })
	openCodeContainerDigestVerifyNow = func() time.Time { return now }

	e := &Engine{
		trustedSQLiteContainers: map[string]trustedSQLiteContainer{
			changedDB:   {state: changedState},
			unchangedDB: {state: unchangedState},
		},
		digestVerifiedAt: map[string]time.Time{
			changedDB:   verifiedAt,
			unchangedDB: verifiedAt,
		},
	}
	files := []parser.DiscoveredFile{
		{Agent: parser.AgentOpenCode, Path: changedDB + "#changed"},
		{Agent: parser.AgentOpenCode, Path: unchangedDB + "#unchanged"},
	}
	e.beginStreamingSQLiteContainerPass(map[string]parser.SQLiteContainerState{
		changedDB: changedState, unchangedDB: unchangedState,
	})
	for _, file := range files {
		e.noteSQLiteContainerDiscovery(file)
	}
	e.containerPass.fullDigestListed[changedDB] = true
	e.containerPass.fullDigestListed[unchangedDB] = true
	e.finishStreamingSQLiteContainerDiscovery()

	_, err := changedConn.Exec("INSERT INTO session (id) VALUES ('changed')")
	require.NoError(t, err, "mutate changed container after discovery")
	changedAfter, ok := parser.StatSQLiteContainerState(changedDB)
	require.True(t, ok, "changed container state must be readable after mutation")
	require.NotEqual(t, changedState, changedAfter,
		"the mutation must change the container state")

	for _, file := range files {
		e.noteSQLiteContainerResult(file.Path, true)
	}
	e.finishSQLiteContainerPass(false, true)

	assert.NotContains(t, e.digestVerifiedAt, changedDB,
		"a changed full-digest container must lose verification")
	assert.Equal(t, changedState, e.trustedSQLiteContainers[changedDB].state,
		"a changed container must retain its previous trusted state")
	assert.Equal(t, unchangedState, e.trustedSQLiteContainers[unchangedDB].state)
	assert.Equal(t, now, e.digestVerifiedAt[unchangedDB],
		"an unchanged sibling must still receive a verification stamp")
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

// TestSQLiteContainerPassClearsVerificationOnlyOnEvidence ensures the
// bounded watermark-only window survives passes that never observed the
// container changing: a clean partial pass and an uncovered container in
// a clean scoped pass keep their verification age, while an attributed
// failure clears only its container and a poisoned pass clears every
// captured one.
func TestSQLiteContainerPassClearsVerificationOnlyOnEvidence(t *testing.T) {
	dbPath, _ := newContainerTestDB(t)
	state, ok := parser.StatSQLiteContainerState(dbPath)
	require.True(t, ok, "container state must be readable")
	siblingPath, _ := newContainerTestDB(t)
	siblingState, ok := parser.StatSQLiteContainerState(siblingPath)
	require.True(t, ok, "sibling container state must be readable")
	file := parser.DiscoveredFile{
		Agent: parser.AgentOpenCode, Path: dbPath + "#ses-1",
	}
	verified := time.Unix(100, 0)

	newEngine := func() *Engine {
		e := &Engine{digestVerifiedAt: map[string]time.Time{
			dbPath:      verified,
			siblingPath: verified,
		}}
		e.beginSQLiteContainerPass(
			[]parser.DiscoveredFile{file},
			map[string]parser.SQLiteContainerState{
				dbPath:      state,
				siblingPath: siblingState,
			},
		)
		return e
	}

	t.Run("clean partial pass preserves verification without promoting", func(t *testing.T) {
		e := newEngine()
		e.noteSQLiteContainerResult(file.Path, true)
		e.finishSQLiteContainerPass(true, false)
		assert.Equal(t, verified, e.digestVerifiedAt[dbPath],
			"a clean partial pass must keep the bounded verification window")
		assert.NotContains(t, e.trustedSQLiteContainers, dbPath,
			"a partial pass must never promote trust")
	})

	t.Run("failed container clears only its own verification", func(t *testing.T) {
		e := newEngine()
		e.noteSQLiteContainerResult(file.Path, false)
		e.finishSQLiteContainerPass(true, false)
		assert.NotContains(t, e.digestVerifiedAt, dbPath,
			"an attributed failure must clear its container")
		assert.Equal(t, verified, e.digestVerifiedAt[siblingPath],
			"an attributed failure must not clear unaffected containers")
	})

	t.Run("poisoned pass clears every captured verification", func(t *testing.T) {
		e := newEngine()
		e.noteSQLiteContainerResult(file.Path, true)
		e.poisonSQLiteContainerPass()
		e.finishSQLiteContainerPass(true, false)
		assert.NotContains(t, e.digestVerifiedAt, dbPath,
			"a poisoned pass must clear captured verification")
		assert.NotContains(t, e.digestVerifiedAt, siblingPath,
			"a poisoned pass must clear captured verification")
	})

	t.Run("clean scoped pass preserves uncovered verification", func(t *testing.T) {
		e := newEngine()
		e.noteSQLiteContainerResult(file.Path, true)
		e.finishSQLiteContainerPass(false, false)
		assert.Equal(t, verified, e.digestVerifiedAt[siblingPath],
			"an uncovered container in a clean pass must keep its window")
		assert.NotContains(t, e.trustedSQLiteContainers, siblingPath,
			"an uncovered container must not be promoted")
	})
}

func TestOpenCodeDigestVerificationStampedOnlyByDigestPass(t *testing.T) {
	origNow := openCodeContainerDigestVerifyNow
	t.Cleanup(func() { openCodeContainerDigestVerifyNow = origNow })
	now := time.Unix(100, 0)
	openCodeContainerDigestVerifyNow = func() time.Time { return now }

	dbPath, _ := newContainerTestDB(t)
	state, ok := parser.StatSQLiteContainerState(dbPath)
	require.True(t, ok, "container state must be readable")
	file := parser.DiscoveredFile{
		Agent: parser.AgentOpenCode,
		Path:  dbPath + "#ses-1",
	}

	e := &Engine{}
	e.beginStreamingSQLiteContainerPass(
		map[string]parser.SQLiteContainerState{dbPath: state},
	)
	e.noteSQLiteContainerDiscovery(file)
	e.containerPass.fullDigestListed[dbPath] = true
	e.noteSQLiteContainerResult(file.Path, true)
	e.finishSQLiteContainerPass(false, true)
	verifiedAt, ok := e.digestVerifiedAt[dbPath]
	require.True(t, ok, "a complete digest-listed pass must stamp verification")
	assert.Equal(t, now, verifiedAt)

	now = now.Add(time.Minute)
	e.beginStreamingSQLiteContainerPass(
		map[string]parser.SQLiteContainerState{dbPath: state},
	)
	e.noteSQLiteContainerDiscovery(file)
	e.noteSQLiteContainerResult(file.Path, true)
	e.finishSQLiteContainerPass(false, true)
	assert.Equal(t, verifiedAt, e.digestVerifiedAt[dbPath],
		"a watermark-listed pass must not refresh verification age")

	e.beginStreamingSQLiteContainerPass(
		map[string]parser.SQLiteContainerState{dbPath: state},
	)
	e.noteSQLiteContainerDiscovery(file)
	e.containerPass.fullDigestListed[dbPath] = true
	e.noteSQLiteContainerResult(file.Path, false)
	e.finishSQLiteContainerPass(false, true)
	assert.NotContains(t, e.digestVerifiedAt, dbPath,
		"a failed digest pass must invalidate verification age")
}

func TestOpenCodeContainerDiscoveryReplacementMovesAdmission(t *testing.T) {
	const (
		oldDB = "/data/opencode.db"
		newDB = "/data/opencode-legacy.db"
	)
	e := &Engine{}
	e.beginStreamingSQLiteContainerPass(map[string]parser.SQLiteContainerState{
		oldDB: {}, newDB: {},
	})
	oldFile := parser.DiscoveredFile{
		Agent: parser.AgentOpenCode,
		Path:  oldDB + "#session",
	}
	newFile := parser.DiscoveredFile{
		Agent: parser.AgentOpenCode,
		Path:  newDB + "#session",
	}
	e.noteSQLiteContainerDiscovery(oldFile)
	e.unNoteSQLiteContainerDiscovery(oldFile)
	e.noteSQLiteContainerDiscovery(newFile)

	assert.Zero(t, e.containerPass.discovered[oldDB])
	assert.Equal(t, 1, e.containerPass.discovered[newDB])
}

func TestOpenCodeChildOnlyEditReconcilesAtVerificationInterval(t *testing.T) {
	dbPath, conn := newCompositeContainerTestDB(t)
	origStat := statSQLiteContainerState
	t.Cleanup(func() { statSQLiteContainerState = origStat })
	statSQLiteContainerState = func(path string) (parser.SQLiteContainerState, bool) {
		state, ok := parser.StatSQLiteContainerState(path)
		if ok {
			// The Windows path-stat implementation intentionally reports no
			// stable identity. This test exercises the available-identity
			// interval contract; the unavailable-identity fail-closed policy
			// is covered separately below.
			state.DBInode = 1
			state.DBDevice = 1
		}
		return state, ok
	}
	_, err := conn.Exec(`
		INSERT INTO project (id, worktree, time_updated)
		VALUES ('proj', '/home/user/code/app', 1779012000000);
		INSERT INTO session
			(id, project_id, time_created, time_updated)
		VALUES ('ses-1', 'proj', 1779012000000, 1779099999000);
		INSERT INTO message
			(id, session_id, data, time_created, time_updated)
		VALUES
			('msg-user', 'ses-1', '{"role":"user"}', 1779012000000, 1779012500000),
			('msg-assistant', 'ses-1', '{"role":"assistant"}', 1779012000001, 1779012500001);
		INSERT INTO part
			(id, session_id, message_id, data, time_created, time_updated)
		VALUES
			('part-user', 'ses-1', 'msg-user',
			 '{"type":"text","text":"original prompt"}',
			 1779012000000, 1779012500000),
			('part-assistant', 'ses-1', 'msg-assistant',
			 '{"type":"text","text":"original answer"}',
			 1779012000001, 1779012500001)
	`)
	require.NoError(t, err, "seed composite container")

	archive := openTestDB(t)
	e := NewEngine(archive, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOpenCode: {filepath.Dir(dbPath)},
		},
		Machine: "local",
	})
	t.Cleanup(e.Close)
	assertContent := func(want ...string) {
		messages, err := archive.GetAllMessages(t.Context(), "opencode:ses-1")
		require.NoError(t, err, "read archived messages")
		require.Len(t, messages, len(want))
		for i, content := range want {
			assert.Equal(t, content, messages[i].Content, "messages[%d]", i)
		}
	}

	origNow := openCodeContainerDigestVerifyNow
	t.Cleanup(func() { openCodeContainerDigestVerifyNow = origNow })
	verifiedAt := time.Unix(100, 0)
	openCodeContainerDigestVerifyNow = func() time.Time { return verifiedAt }
	initial := e.SyncAll(t.Context(), nil)
	require.False(t, initial.Aborted, "initial sync aborted: %+v", initial)
	assert.Equal(t, 1, initial.Synced)
	assertContent("original prompt", "original answer")

	_, err = conn.Exec(`
		UPDATE message SET id = CASE id
			WHEN 'msg-user' THEN 'msg-user-v2'
			WHEN 'msg-assistant' THEN 'msg-assistant-v2'
		END
		WHERE id IN ('msg-user', 'msg-assistant');
		UPDATE part SET
			id = CASE id
				WHEN 'part-user' THEN 'part-user-v2'
				WHEN 'part-assistant' THEN 'part-assistant-v2'
			END,
			message_id = CASE message_id
				WHEN 'msg-user' THEN 'msg-user-v2'
				WHEN 'msg-assistant' THEN 'msg-assistant-v2'
			END,
			data = CASE id
				WHEN 'part-user' THEN '{"type":"text","text":"changed prompt"}'
				WHEN 'part-assistant' THEN '{"type":"text","text":"changed answer"}'
			END
		WHERE id IN ('part-user', 'part-assistant')
	`)
	require.NoError(t, err, "apply child-only edit")
	scansBefore := parser.OpenCodeContainerChildScans()
	recent := e.SyncAll(t.Context(), nil)
	require.False(t, recent.Aborted, "recent sync aborted: %+v", recent)
	assert.Zero(t, parser.OpenCodeContainerChildScans()-scansBefore,
		"a recent watermark pass must avoid the container child scan")
	assert.Zero(t, recent.Synced,
		"a child-only edit may remain deferred inside the verification interval")
	assertContent("original prompt", "original answer")

	verifiedAt = verifiedAt.Add(openCodeContainerDigestVerifyInterval)
	scansBefore = parser.OpenCodeContainerChildScans()
	due := e.SyncAll(t.Context(), nil)
	require.False(t, due.Aborted, "due sync aborted: %+v", due)
	assert.Equal(t, 1, due.Synced,
		"the due full digest pass must reconcile the child-only edit")
	assertContent("changed prompt", "changed answer")
	assert.Equal(t, int64(1), parser.OpenCodeContainerChildScans()-scansBefore,
		"the due pass must perform the full container child scan")
}

// TestOpenCodeQuickSyncCutoffExemptsLapsedVerification pins the quick-sync
// cutoff exemption: once a container's verification stamp lapses, a cutoff
// pass processes the full digest listing it already paid for, so a
// backdated child-only edit reconciles and the stamp refreshes. A container
// with no stamp stays subject to the cutoff.
func TestOpenCodeQuickSyncCutoffExemptsLapsedVerification(t *testing.T) {
	dbPath, conn := newCompositeContainerTestDB(t)
	origStat := statSQLiteContainerState
	t.Cleanup(func() { statSQLiteContainerState = origStat })
	statSQLiteContainerState = func(path string) (parser.SQLiteContainerState, bool) {
		state, ok := parser.StatSQLiteContainerState(path)
		if ok {
			state.DBInode = 1
			state.DBDevice = 1
		}
		return state, ok
	}
	_, err := conn.Exec(`
		INSERT INTO project (id, worktree, time_updated)
		VALUES ('proj', '/home/user/code/app', 1779012000000);
		INSERT INTO session
			(id, project_id, time_created, time_updated)
		VALUES ('ses-1', 'proj', 1779012000000, 1779012000000);
		INSERT INTO message
			(id, session_id, data, time_created, time_updated)
		VALUES ('msg-1', 'ses-1', '{"role":"user"}', 1779012000000, 1779012000000);
		INSERT INTO part
			(id, session_id, message_id, data, time_created, time_updated)
		VALUES ('part-1', 'ses-1', 'msg-1',
			'{"type":"text","text":"original prompt"}',
			1779012000000, 1779012000000)
	`)
	require.NoError(t, err, "seed composite container")

	// Every row timestamp sits below this cutoff, so the ordinary quick-sync
	// filter would drop the session.
	cutoff := time.UnixMilli(1_779_100_000_000)

	archive := openTestDB(t)
	e := NewEngine(archive, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOpenCode: {filepath.Dir(dbPath)},
		},
		Machine: "local",
	})
	t.Cleanup(e.Close)
	assertContent := func(want string) {
		messages, err := archive.GetAllMessages(t.Context(), "opencode:ses-1")
		require.NoError(t, err, "read archived messages")
		require.Len(t, messages, 1)
		assert.Equal(t, want, messages[0].Content)
	}

	origNow := openCodeContainerDigestVerifyNow
	t.Cleanup(func() { openCodeContainerDigestVerifyNow = origNow })
	verifyNow := time.Unix(100, 0)
	openCodeContainerDigestVerifyNow = func() time.Time { return verifyNow }
	initial := e.SyncAll(t.Context(), nil)
	require.False(t, initial.Aborted, "initial sync aborted: %+v", initial)
	require.Equal(t, 1, initial.Synced)
	assertContent("original prompt")

	// Backdated child-only edit: content and identity change, every
	// timestamp stays put, so the composite mtime stays below the cutoff.
	_, err = conn.Exec(`
		UPDATE part SET
			id = 'part-1-v2',
			data = '{"type":"text","text":"changed prompt"}'
		WHERE id = 'part-1'
	`)
	require.NoError(t, err, "apply backdated child-only edit")

	recent := e.SyncAllSince(t.Context(), cutoff, nil)
	require.False(t, recent.Aborted, "recent quick sync aborted: %+v", recent)
	assert.Zero(t, recent.Synced,
		"inside the window a quick sync defers the child-only edit")
	assertContent("original prompt")

	verifyNow = verifyNow.Add(openCodeContainerDigestVerifyInterval)
	due := e.SyncAllSince(t.Context(), cutoff, nil)
	require.False(t, due.Aborted, "due quick sync aborted: %+v", due)
	assert.Equal(t, 1, due.Synced,
		"a lapsed container's sessions must bypass the cutoff")
	assertContent("changed prompt")
	assert.Equal(t, verifyNow, e.digestVerifiedAt[dbPath],
		"the completed due pass must refresh the verification stamp")

	// A fresh engine has no stamp, so the same quick sync keeps the cutoff:
	// the backdated session is listed, filtered, and never parsed.
	fresh := NewEngine(openTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOpenCode: {filepath.Dir(dbPath)},
		},
		Machine: "local",
	})
	t.Cleanup(fresh.Close)
	unstamped := fresh.SyncAllSince(t.Context(), cutoff, nil)
	require.False(t, unstamped.Aborted, "fresh quick sync aborted: %+v", unstamped)
	assert.Zero(t, unstamped.Synced,
		"an unstamped container's old sessions stay behind the cutoff")
	assert.Empty(t, fresh.digestVerifiedAt,
		"an incomplete cutoff pass must not stamp verification")
}

// TestLapsedVerificationExemptionRequiresDigestListing pins the exemption to
// discovery's listing form: an expired stamp alone must not exempt a
// container whose pass listed watermark-only, as when the interval boundary
// falls between discovery and the cutoff filter.
func TestLapsedVerificationExemptionRequiresDigestListing(t *testing.T) {
	dbPath, _ := newContainerTestDB(t)
	state, ok := parser.StatSQLiteContainerState(dbPath)
	require.True(t, ok, "container state must be readable")
	origNow := openCodeContainerDigestVerifyNow
	t.Cleanup(func() { openCodeContainerDigestVerifyNow = origNow })
	now := time.Unix(1000, 0)
	openCodeContainerDigestVerifyNow = func() time.Time { return now }

	file := parser.DiscoveredFile{
		Agent: parser.AgentOpenCode, Path: dbPath + "#ses-1",
	}
	e := &Engine{digestVerifiedAt: map[string]time.Time{
		dbPath: now.Add(-2 * openCodeContainerDigestVerifyInterval),
	}}
	e.beginSQLiteContainerPass(
		[]parser.DiscoveredFile{file},
		map[string]parser.SQLiteContainerState{dbPath: state},
	)
	assert.Empty(t,
		e.lapsedDigestVerificationContainers([]parser.DiscoveredFile{file}),
		"a watermark-listed pass must not exempt an expired stamp")

	e.containerPass.fullDigestListed[dbPath] = true
	assert.Equal(t, map[string]bool{dbPath: true},
		e.lapsedDigestVerificationContainers([]parser.DiscoveredFile{file}),
		"a digest-listed pass exempts the expired stamp")
}

func TestOpenCodeFirstPassAfterProcessStartUsesDigestListing(t *testing.T) {
	dbPath, _ := newContainerTestDB(t)
	e := &Engine{
		trustedSQLiteContainers: map[string]trustedSQLiteContainer{
			dbPath: {state: parser.SQLiteContainerState{}},
		},
	}
	predicate := e.sqliteContainerListsWatermarkOnly(
		map[string]parser.SQLiteContainerState{dbPath: {}},
	)
	assert.False(t, predicate(dbPath),
		"a fresh process has no digest verification timestamp")
}

func TestOpenCodeResyncClearsDigestVerification(t *testing.T) {
	e := &Engine{
		trustedSQLiteContainers: map[string]trustedSQLiteContainer{
			"/data/opencode.db": {},
		},
		digestVerifiedAt: map[string]time.Time{
			"/data/opencode.db": time.Unix(100, 0),
		},
	}
	e.clearTrustedSQLiteContainers()
	assert.Nil(t, e.trustedSQLiteContainers)
	assert.Nil(t, e.digestVerifiedAt,
		"resync must clear the container verification timestamps")
}

func TestOpenCodeReplacementCannotReuseVerification(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "opencode.db")
	origNow := openCodeContainerDigestVerifyNow
	t.Cleanup(func() { openCodeContainerDigestVerifyNow = origNow })
	now := time.Unix(100, 0)
	openCodeContainerDigestVerifyNow = func() time.Time { return now }

	previous := parser.SQLiteContainerState{
		DBInode: 10, DBDevice: 20, DBChangeCounter: 10,
	}
	watermarkAdmission := func(
		trusted, current parser.SQLiteContainerState,
	) bool {
		e := &Engine{
			trustedSQLiteContainers: map[string]trustedSQLiteContainer{
				dbPath: {state: trusted},
			},
			digestVerifiedAt: map[string]time.Time{dbPath: now},
		}
		return e.sqliteContainerListsWatermarkOnly(
			map[string]parser.SQLiteContainerState{dbPath: current},
		)(dbPath)
	}

	t.Run("new file identity", func(t *testing.T) {
		replacement := previous
		replacement.DBInode = 11
		assert.False(t, watermarkAdmission(previous, replacement),
			"a replacement container must require a new digest verification")
	})

	t.Run("identity unavailable", func(t *testing.T) {
		unavailable := previous
		unavailable.DBInode = 0
		unavailable.DBDevice = 0
		assert.False(t, watermarkAdmission(unavailable, unavailable),
			"unavailable identity must fail closed to full digest listing")
	})

	t.Run("normal in-place transaction", func(t *testing.T) {
		advanced := previous
		advanced.DBChangeCounter++
		assert.True(t, watermarkAdmission(previous, advanced),
			"a normal in-place transaction may retain the fast path")
	})
}

func TestOpenCodeForcePathsAlwaysListDigestForm(t *testing.T) {
	const dbPath = "/data/opencode.db"
	pre := map[string]parser.SQLiteContainerState{dbPath: {}}
	for _, tc := range []struct {
		name  string
		force func(*Engine)
	}{
		{name: "force parse", force: func(e *Engine) { e.forceParse = true }},
		{name: "force full parse", force: func(e *Engine) { e.forceFullParse = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{}
			tc.force(e)
			assert.Nil(t, e.sqliteContainerListsWatermarkOnly(pre),
				"force paths must not authorize watermark-only listing")
		})
	}
}
