package sync

import (
	"context"
	"database/sql"
	"errors"
	"os"
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

// TestReconciliationShadowPromotionSurvivesContainerChange pins the capture
// check to the resolved source representation: a candidate promoted to its
// storage shadow does not depend on the SQLite container, so a container
// change mid-pass must not reject its state application into the
// path-matching fallback, which cannot match the promoted path.
func TestReconciliationShadowPromotionSurvivesContainerChange(t *testing.T) {
	container, conn := newContainerTestDB(t)
	shadowPath := filepath.Join(t.TempDir(), "ses_a.json")
	require.NoError(t, os.WriteFile(shadowPath, []byte("{}"), 0o600))
	source := parser.SourceRef{
		Provider:       parser.AgentOpenCode,
		DisplayPath:    shadowPath,
		FingerprintKey: shadowPath,
		Key:            shadowPath,
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
	_, err := conn.Exec("INSERT INTO session (id) VALUES ('ses_a')")
	require.NoError(t, err, "change container before rehydration")

	files, err := engine.rehydrateReconciliationPage(
		t.Context(), []reconciliationCandidate{{
			Provider:    parser.AgentOpenCode,
			Identity:    "ses_a",
			Path:        container + "#ses_a",
			SourceState: provider.state,
		}},
		map[parser.AgentType]parser.Provider{
			parser.AgentOpenCode: provider,
		},
		false,
	)
	require.NoError(t, err,
		"a shadow-promoted candidate must survive a container change")
	require.Len(t, files, 1)
	require.NotNil(t, files[0].ProviderSource)
	assert.Equal(t, shadowPath, files[0].ProviderSource.DisplayPath,
		"the promoted storage shadow source must be kept")
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

// TestSQLiteContainerTrustLifecycle drives the gate's one trust invariant
// through a matrix: trust — promotion, verification stamps, gate skips,
// carried provider sources — is granted only by a pass that observed the
// container's current bytes, and failures, mismatches, changes,
// replacements, and resyncs clear exactly the affected containers.
//
// Prior trust is seeded with a sentinel state distinct from every capture, so
// "kept the prior state" and "promoted the capture" are distinguishable
// outcomes. Every row also scripts a sibling container where the invariant is
// per-container: attributed evidence clears only its own container, while a
// poisoned pass clears every captured one.
func TestSQLiteContainerTrustLifecycle(t *testing.T) {
	type captureKind int
	const (
		captureLive captureKind = iota
		captureStale
		captureMissing
	)
	type resultKind int
	const (
		resultComplete resultKind = iota
		resultFailed
		resultPoisoned
	)
	type finishKind int
	const (
		finishFull finishKind = iota
		finishScoped
		finishIncomplete
	)
	type stateWant int
	const (
		wantAbsent stateWant = iota
		wantPrior
		wantFresh
	)

	prior := time.Unix(100, 0)
	now := time.Unix(200, 0)
	origNow := openCodeContainerDigestVerifyNow
	t.Cleanup(func() { openCodeContainerDigestVerifyNow = origNow })
	openCodeContainerDigestVerifyNow = func() time.Time { return now }

	for _, tc := range []struct {
		name       string
		priorTrust bool
		// priorTrustCapture seeds trust with the capture itself, so a gate
		// probe cannot pass on mere state inequality.
		priorTrustCapture bool
		priorStamp        bool
		capture           captureKind
		digestList        bool
		// lateWrite mutates the container after discovery's recapture, so
		// only finalization revalidation can observe it.
		lateWrite bool
		result    resultKind
		finish    finishKind
		// sibling adds a second captured, stamped container; discovered
		// siblings are digest-listed and complete cleanly.
		sibling           bool
		siblingDiscovered bool
		probeMemberStale  bool
		wantTrust         stateWant
		wantStamp         stateWant
		wantSiblingTrust  stateWant
		wantSiblingStamp  stateWant
	}{
		{
			// A capture taken after discovery could be newer than the listed
			// session set; promoting it would gate-skip an unparsed write.
			name:      "complete full pass without a capture earns no trust",
			capture:   captureMissing,
			wantTrust: wantAbsent,
		},
		{
			name:      "clean complete full pass promotes exactly the pre-discovery capture",
			capture:   captureLive,
			wantTrust: wantFresh,
		},
		{
			name:       "digest-listed clean pass stamps verification",
			capture:    captureLive,
			digestList: true,
			wantTrust:  wantFresh,
			wantStamp:  wantFresh,
		},
		{
			name:       "watermark-listed pass never refreshes the stamp",
			priorStamp: true,
			capture:    captureLive,
			wantTrust:  wantFresh,
			wantStamp:  wantPrior,
		},
		{
			// The discovered session set may already include the change, so
			// gating against the pre-discovery state would skip it while it
			// still matches the trusted state.
			name:             "capture-discovery mismatch fails the container for the pass",
			priorTrust:       true,
			priorStamp:       true,
			capture:          captureStale,
			probeMemberStale: true,
			wantTrust:        wantPrior,
			wantStamp:        wantAbsent,
		},
		{
			// With trust equal to the capture, only the mismatch-failed flag
			// can block the gate skip, so this row pins the guard itself; the
			// sentinel row above pins state retention. The retained trust
			// equals the capture here, so wantFresh reads as retention.
			name:              "capture-discovery mismatch blocks gate skip under matching trust",
			priorTrustCapture: true,
			capture:           captureStale,
			probeMemberStale:  true,
			wantTrust:         wantFresh,
			wantStamp:         wantAbsent,
		},
		{
			name:       "missing capture invalidates prior verification",
			priorTrust: true,
			priorStamp: true,
			capture:    captureMissing,
			wantTrust:  wantPrior,
			wantStamp:  wantAbsent,
		},
		{
			name:              "post-discovery write is caught at finalization, per container",
			priorTrust:        true,
			priorStamp:        true,
			capture:           captureLive,
			digestList:        true,
			lateWrite:         true,
			sibling:           true,
			siblingDiscovered: true,
			wantTrust:         wantPrior,
			wantStamp:         wantAbsent,
			wantSiblingTrust:  wantFresh,
			wantSiblingStamp:  wantFresh,
		},
		{
			name:       "failed digest pass clears the stamp",
			priorTrust: true,
			priorStamp: true,
			capture:    captureLive,
			digestList: true,
			result:     resultFailed,
			wantTrust:  wantPrior,
			wantStamp:  wantAbsent,
		},
		{
			name:       "clean incomplete pass preserves verification without promoting",
			priorStamp: true,
			capture:    captureLive,
			finish:     finishIncomplete,
			wantTrust:  wantAbsent,
			wantStamp:  wantPrior,
		},
		{
			name:             "attributed failure clears only its own container",
			priorStamp:       true,
			capture:          captureLive,
			result:           resultFailed,
			finish:           finishIncomplete,
			sibling:          true,
			wantStamp:        wantAbsent,
			wantSiblingStamp: wantPrior,
		},
		{
			name:             "poisoned incomplete pass clears every captured verification",
			priorStamp:       true,
			capture:          captureLive,
			result:           resultPoisoned,
			finish:           finishIncomplete,
			sibling:          true,
			wantStamp:        wantAbsent,
			wantSiblingStamp: wantAbsent,
		},
		{
			name:             "poisoned complete pass clears instead of promoting",
			priorStamp:       true,
			capture:          captureLive,
			digestList:       true,
			result:           resultPoisoned,
			finish:           finishFull,
			sibling:          true,
			wantTrust:        wantAbsent,
			wantStamp:        wantAbsent,
			wantSiblingStamp: wantAbsent,
		},
		{
			name:             "clean scoped pass preserves uncovered verification",
			priorStamp:       true,
			capture:          captureLive,
			finish:           finishScoped,
			sibling:          true,
			wantTrust:        wantFresh,
			wantStamp:        wantPrior,
			wantSiblingTrust: wantAbsent,
			wantSiblingStamp: wantPrior,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath, conn := newContainerTestDB(t)
			capture, ok := parser.StatSQLiteContainerState(dbPath)
			require.True(t, ok, "container state must be readable")
			if tc.capture == captureStale {
				_, err := conn.Exec("INSERT INTO session (id) VALUES ('w1')")
				require.NoError(t, err,
					"write inside the capture-discovery window")
			}
			sentinel := capture
			sentinel.DBChangeCounter += 100

			e := &Engine{}
			if tc.priorTrust {
				e.trustedSQLiteContainers = map[string]trustedSQLiteContainer{
					dbPath: {state: sentinel},
				}
			}
			if tc.priorTrustCapture {
				e.trustedSQLiteContainers = map[string]trustedSQLiteContainer{
					dbPath: {state: capture},
				}
			}
			if tc.priorStamp {
				e.digestVerifiedAt = map[string]time.Time{dbPath: prior}
			}
			states := map[string]parser.SQLiteContainerState{}
			if tc.capture != captureMissing {
				states[dbPath] = capture
			}
			var siblingPath string
			if tc.sibling {
				siblingPath, _ = newContainerTestDB(t)
				siblingState, ok := parser.StatSQLiteContainerState(siblingPath)
				require.True(t, ok, "sibling state must be readable")
				states[siblingPath] = siblingState
				if e.digestVerifiedAt == nil {
					e.digestVerifiedAt = map[string]time.Time{}
				}
				e.digestVerifiedAt[siblingPath] = prior
			}

			fileA := parser.DiscoveredFile{
				Agent: parser.AgentOpenCode, Path: dbPath + "#ses-1",
			}
			files := []parser.DiscoveredFile{fileA}
			fileB := parser.DiscoveredFile{
				Agent: parser.AgentOpenCode, Path: siblingPath + "#ses-1",
			}
			if tc.siblingDiscovered {
				files = append(files, fileB)
			}
			e.beginSQLiteContainerPass(files, states)
			if tc.digestList {
				e.containerPass.fullDigestListed[dbPath] = true
			}
			if tc.siblingDiscovered {
				e.containerPass.fullDigestListed[siblingPath] = true
			}
			if tc.probeMemberStale {
				assert.False(t, e.sqliteContainerSourceFresh(fileA),
					"an unobserved container must not gate-skip its sessions")
			}
			if tc.lateWrite {
				_, err := conn.Exec("INSERT INTO session (id) VALUES ('w2')")
				require.NoError(t, err, "write after discovery")
			}
			switch tc.result {
			case resultComplete:
				e.noteSQLiteContainerResult(fileA.Path, true)
			case resultFailed:
				e.noteSQLiteContainerResult(fileA.Path, false)
			case resultPoisoned:
				e.noteSQLiteContainerResult(fileA.Path, true)
				e.poisonSQLiteContainerPass()
			}
			if tc.siblingDiscovered {
				e.noteSQLiteContainerResult(fileB.Path, true)
			}
			switch tc.finish {
			case finishFull:
				e.finishSQLiteContainerPass(false, true)
			case finishScoped:
				e.finishSQLiteContainerPass(false, false)
			case finishIncomplete:
				e.finishSQLiteContainerPass(true, false)
			}

			assertTrust := func(path string, want stateWant, label string) {
				t.Helper()
				got, ok := e.trustedSQLiteContainers[path]
				switch want {
				case wantAbsent:
					assert.False(t, ok, "%s: trust must be absent", label)
				case wantPrior:
					require.True(t, ok, "%s: trust must survive", label)
					assert.Equal(t, sentinel, got.state,
						"%s: trust must stay the prior state", label)
				case wantFresh:
					require.True(t, ok, "%s: trust must be promoted", label)
					assert.Equal(t, states[path], got.state,
						"%s: trust must be exactly the pre-discovery capture",
						label)
				}
			}
			assertStamp := func(path string, want stateWant, label string) {
				t.Helper()
				got, ok := e.digestVerifiedAt[path]
				switch want {
				case wantAbsent:
					assert.False(t, ok, "%s: stamp must be cleared", label)
				case wantPrior:
					require.True(t, ok, "%s: stamp must survive", label)
					assert.Equal(t, prior, got,
						"%s: stamp must keep its prior age", label)
				case wantFresh:
					require.True(t, ok, "%s: stamp must be written", label)
					assert.Equal(t, now, got,
						"%s: stamp must be refreshed", label)
				}
			}
			assertTrust(dbPath, tc.wantTrust, "container")
			assertStamp(dbPath, tc.wantStamp, "container")
			if tc.sibling {
				assertTrust(siblingPath, tc.wantSiblingTrust, "sibling")
				assertStamp(siblingPath, tc.wantSiblingStamp, "sibling")
			}
		})
	}

	t.Run("replacement admission", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "opencode.db")
		previous := parser.SQLiteContainerState{
			DBInode: 10, DBDevice: 20, DBChangeCounter: 10,
		}
		admits := func(trusted, current parser.SQLiteContainerState) bool {
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
		newIdentity := previous
		newIdentity.DBInode = 11
		noIdentity := previous
		noIdentity.DBInode, noIdentity.DBDevice = 0, 0
		rolledBack := previous
		rolledBack.DBChangeCounter--
		advanced := previous
		advanced.DBChangeCounter++
		assert.False(t, admits(previous, newIdentity),
			"a replacement container must require a new digest verification")
		assert.False(t, admits(noIdentity, noIdentity),
			"unavailable identity must fail closed to full digest listing")
		assert.False(t, admits(previous, rolledBack),
			"a change-counter rollback is a restore and must re-verify")
		assert.True(t, admits(previous, advanced),
			"a normal in-place transaction may retain the fast path")
	})

	// A write landing after the post-discovery recapture invalidates the
	// carried full-digest source, so the changed session re-resolves live
	// instead of skipping on its pre-change digest; once the failure is
	// recorded, both discard paths drop later carried sources without
	// another stat.
	t.Run("carried source drops on container evidence", func(t *testing.T) {
		container, conn := newContainerTestDB(t)
		pre, ok := parser.StatSQLiteContainerState(container)
		require.True(t, ok, "container state must be readable")
		e := &Engine{}
		e.beginStreamingSQLiteContainerPass(
			map[string]parser.SQLiteContainerState{container: pre},
		)
		carried := func() parser.DiscoveredFile {
			source := parser.SourceRef{
				Provider:       parser.AgentOpenCode,
				DisplayPath:    container + "#ses_a",
				FingerprintKey: container + "#ses_a",
				Key:            container + "#ses_a",
			}
			return parser.DiscoveredFile{
				Agent:          parser.AgentOpenCode,
				Path:           source.DisplayPath,
				ProviderSource: &source,
			}
		}

		file := carried()
		e.discardStaleSQLiteProviderSource(&file)
		require.NotNil(t, file.ProviderSource,
			"an unchanged capture keeps the carried source")

		_, err := conn.Exec("INSERT INTO session (id) VALUES ('ses_a')")
		require.NoError(t, err, "write container after recapture")
		e.discardStaleSQLiteProviderSource(&file)
		assert.Nil(t, file.ProviderSource,
			"a mid-pass container write must drop the carried source")
		assert.True(t, e.containerPass.failed[container],
			"the recheck must fail the container for the rest of the pass")

		// The recorded failure alone must decide both discard paths: a
		// forbidden-stat stub proves neither one re-stats the container.
		origStat := statSQLiteContainerState
		t.Cleanup(func() { statSQLiteContainerState = origStat })
		statCalls := 0
		statSQLiteContainerState = func(
			path string,
		) (parser.SQLiteContainerState, bool) {
			statCalls++
			return origStat(path)
		}
		next := carried()
		e.discardStaleSQLiteProviderSource(&next)
		assert.Nil(t, next.ProviderSource,
			"a recorded failure must short-circuit the stat-based recheck")
		late := carried()
		e.discardFailedSQLiteProviderSource(&late)
		assert.Nil(t, late.ProviderSource,
			"a recorded failure must drop carried sources without a stat")
		assert.Zero(t, statCalls,
			"a recorded failure must be honored before any stat")
	})

	t.Run("resync clears all trust", func(t *testing.T) {
		e := &Engine{
			trustedSQLiteContainers: map[string]trustedSQLiteContainer{
				"/data/opencode.db": {},
			},
			digestVerifiedAt: map[string]time.Time{
				"/data/opencode.db": prior,
			},
		}
		e.clearTrustedSQLiteContainers()
		assert.Nil(t, e.trustedSQLiteContainers)
		assert.Nil(t, e.digestVerifiedAt,
			"resync must clear the container verification timestamps")
	})
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

// TestOpenCodeDigestListingForm pins, in one matrix, which paths must
// produce the full digest listing form instead of the bounded watermark
// form, what a lapsed verification stamp requires of discovery, and what it
// buys a quick-sync cutoff pass.
func TestOpenCodeDigestListingForm(t *testing.T) {
	now := time.Unix(1000, 0)
	origNow := openCodeContainerDigestVerifyNow
	t.Cleanup(func() { openCodeContainerDigestVerifyNow = origNow })
	openCodeContainerDigestVerifyNow = func() time.Time { return now }

	t.Run("discovery listing predicate", func(t *testing.T) {
		// A native path: the predicate cleans its argument before the map
		// lookup, so a foreign-separator literal would never match.
		dbPath := filepath.Join(t.TempDir(), "opencode.db")
		state := parser.SQLiteContainerState{
			DBInode: 1, DBDevice: 1, DBChangeCounter: 1,
		}
		trust := func(e *Engine, stampAge time.Duration) {
			e.trustedSQLiteContainers = map[string]trustedSQLiteContainer{
				dbPath: {state: state},
			}
			e.digestVerifiedAt = map[string]time.Time{
				dbPath: now.Add(-stampAge),
			}
		}
		for _, tc := range []struct {
			name  string
			setup func(e *Engine)
			// wantNil: force paths must disable the callback entirely, not
			// merely answer false per container.
			wantNil       bool
			wantWatermark bool
		}{
			{name: "a current stamp lists watermark-only", setup: func(e *Engine) {
				trust(e, 0)
			}, wantWatermark: true},
			{name: "a fresh process has no stamp", setup: func(e *Engine) {
				e.trustedSQLiteContainers = map[string]trustedSQLiteContainer{
					dbPath: {state: state},
				}
			}},
			{name: "an expired stamp is due for the digest form", setup: func(e *Engine) {
				trust(e, 2*openCodeContainerDigestVerifyInterval)
			}},
			{name: "force parse overrides a current stamp", setup: func(e *Engine) {
				trust(e, 0)
				e.forceParse = true
			}, wantNil: true},
			{name: "force full parse overrides a current stamp", setup: func(e *Engine) {
				trust(e, 0)
				e.forceFullParse = true
			}, wantNil: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				e := &Engine{}
				tc.setup(e)
				fn := e.sqliteContainerListsWatermarkOnly(
					map[string]parser.SQLiteContainerState{dbPath: state},
				)
				if tc.wantNil {
					assert.Nil(t, fn,
						"this path must not authorize watermark-only listing at all")
					return
				}
				require.NotNil(t, fn)
				assert.Equal(t, tc.wantWatermark, fn(dbPath))
			})
		}
	})

	// The exemption must match discovery's listing form: an expired stamp
	// alone must not exempt a container whose pass listed watermark-only, as
	// when the interval boundary falls between discovery and the cutoff
	// filter — that pass has no digest findings to process.
	t.Run("lapsed exemption requires the digest listing", func(t *testing.T) {
		dbPath, _ := newContainerTestDB(t)
		state, ok := parser.StatSQLiteContainerState(dbPath)
		require.True(t, ok, "container state must be readable")
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
	})

	// End to end through SyncAllSince: a backdated child-only edit keeps
	// every timestamp below the cutoff, so only a lapsed stamp's exemption
	// can let the paid-for digest listing reconcile it and refresh the
	// stamp. A live stamp defers the edit; no stamp keeps the cutoff.
	t.Run("quick-sync cutoff", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			stamped     bool
			age         time.Duration
			wantSynced  int
			wantContent string
			wantStamp   bool
			// wantScans pins the listing form the quick sync paid for: a
			// deferring watermark pass must not aggregate child tables, and
			// a digest pass must do so exactly once.
			wantScans int64
		}{
			{
				name: "a live stamp defers the child-only edit", stamped: true,
				age: time.Second, wantSynced: 0,
				wantContent: "original prompt", wantStamp: true, wantScans: 0,
			},
			{
				name: "a lapsed stamp bypasses the cutoff and restamps", stamped: true,
				age: openCodeContainerDigestVerifyInterval, wantSynced: 1,
				wantContent: "changed prompt", wantStamp: true, wantScans: 1,
			},
			{
				name: "no stamp keeps the cutoff", stamped: false,
				wantSynced: 0, wantStamp: false, wantScans: 1,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dbPath, conn := newCompositeContainerTestDB(t)
				origStat := statSQLiteContainerState
				t.Cleanup(func() { statSQLiteContainerState = origStat })
				statSQLiteContainerState = func(
					path string,
				) (parser.SQLiteContainerState, bool) {
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
					VALUES ('msg-1', 'ses-1', '{"role":"user"}',
						1779012000000, 1779012000000);
					INSERT INTO part
						(id, session_id, message_id, data, time_created, time_updated)
					VALUES ('part-1', 'ses-1', 'msg-1',
						'{"type":"text","text":"original prompt"}',
						1779012000000, 1779012000000)
				`)
				require.NoError(t, err, "seed composite container")
				// Every row timestamp sits below this cutoff, so the
				// ordinary quick-sync filter would drop the session.
				cutoff := time.UnixMilli(1_779_100_000_000)

				archive := openTestDB(t)
				e := NewEngine(archive, EngineConfig{
					AgentDirs: map[parser.AgentType][]string{
						parser.AgentOpenCode: {filepath.Dir(dbPath)},
					},
					Machine: "local",
				})
				t.Cleanup(e.Close)

				verifyNow := now
				openCodeContainerDigestVerifyNow = func() time.Time {
					return verifyNow
				}
				if tc.stamped {
					initial := e.SyncAll(t.Context(), nil)
					require.False(t, initial.Aborted,
						"initial sync aborted: %+v", initial)
					require.Equal(t, 1, initial.Synced)
				}

				// Backdated child-only edit: content and identity change,
				// every timestamp stays put, so the composite mtime stays
				// below the cutoff.
				_, err = conn.Exec(`
					UPDATE part SET
						id = 'part-1-v2',
						data = '{"type":"text","text":"changed prompt"}'
					WHERE id = 'part-1'
				`)
				require.NoError(t, err, "apply backdated child-only edit")

				verifyNow = verifyNow.Add(tc.age)
				scansBefore := parser.OpenCodeContainerChildScans()
				stats := e.SyncAllSince(t.Context(), cutoff, nil)
				require.False(t, stats.Aborted,
					"quick sync aborted: %+v", stats)
				assert.Equal(t, tc.wantSynced, stats.Synced)
				assert.Equal(t, tc.wantScans,
					parser.OpenCodeContainerChildScans()-scansBefore,
					"the quick sync must use the row's listing form")
				if tc.wantContent != "" {
					messages, err := archive.GetAllMessages(
						t.Context(), "opencode:ses-1",
					)
					require.NoError(t, err, "read archived messages")
					require.Len(t, messages, 1)
					assert.Equal(t, tc.wantContent, messages[0].Content)
				}
				if !tc.wantStamp {
					assert.Empty(t, e.digestVerifiedAt,
						"an incomplete cutoff pass must not stamp verification")
				} else if tc.wantSynced > 0 {
					assert.Equal(t, verifyNow, e.digestVerifiedAt[dbPath],
						"the completed due pass must refresh the stamp")
				} else {
					assert.Equal(t, now, e.digestVerifiedAt[dbPath],
						"a deferring pass must keep the stamp's prior age")
				}
			})
		}
	})
}
