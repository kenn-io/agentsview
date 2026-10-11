package sync

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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
	context.Context, parser.SourceRef,
) (parser.ReconciliationSourceState, bool) {
	return p.state, true
}

func (p *reconciliationSourceStateTestProvider) SourcesForChangedPath(
	context.Context, parser.ChangedPathRequest,
) ([]parser.SourceRef, error) {
	return []parser.SourceRef{p.source}, nil
}

func (p *reconciliationSourceStateTestProvider) ApplyReconciliationSourceState(
	_ context.Context, _ *parser.SourceRef, state parser.ReconciliationSourceState,
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
	engine := NewEngine(t.Context(), archive, EngineConfig{
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
	candidate, ok := engine.reconciliationCandidate(t.Context(),
		discoveryProvider, source, []string{root}, nil,
	)
	require.True(t, ok)

	spool, err := newReconciliationSpool(t.Context(), archive.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spool.CloseAndRemove(t.Context())) })
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
			_, err := conn.ExecContext(t.Context(), "INSERT INTO session (id) VALUES ('ses_a')")
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
	_, err := conn.ExecContext(t.Context(), "INSERT INTO session (id) VALUES ('ses_a')")
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
	_, err = conn.ExecContext(t.Context(), "CREATE TABLE session (id TEXT PRIMARY KEY)")
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
	_, err = conn.ExecContext(t.Context(), `
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
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: sessionID, Agent: "opencode", Project: "project",
		Machine: "local", FilePath: &virtualPath, FileMtime: &storedMtime,
	}))
	require.NoError(t, database.SetSessionDataVersion(t.Context(),
		sessionID, db.CurrentDataVersion(),
	))
}

func TestStoredMemberFreshnessPagerEmitsOnlyVouchableRows(t *testing.T) {
	for _, allStale := range []bool{false, true} {
		t.Run(strconv.FormatBool(allStale), func(t *testing.T) {
			database := openTestDB(t)
			const container = "/data/opencode.db"
			for _, id := range []string{"a", "b", "c"} {
				path := container + "#" + id
				mtime := int64(100) * 1_000_000
				hash := ""
				if id == "b" && !allStale {
					mtime, hash = 900*1_000_000, "opencode-child:v1:900:20:30:1:2:abcd"
				}
				if id == "c" && allStale {
					mtime = 500 * 1_000_000
				}
				require.NoError(t, database.UpsertSession(t.Context(), db.Session{
					ID: "opencode:" + id, Agent: "opencode", Project: "project", Machine: "local",
					FilePath: &path, FileMtime: &mtime, FileHash: &hash,
				}))
				current := id != "c"
				if allStale {
					current = id == "c"
				}
				if current {
					require.NoError(t, database.SetSessionDataVersion(t.Context(), "opencode:"+id, db.CurrentDataVersion()))
				}
			}
			limit := 10
			if allStale {
				limit = 2
			}
			e := &Engine{db: database, machine: "local"}
			rows, done, err := e.storedMemberFreshnessPager(container, "opencode:", nil)(t.Context(), "", limit)
			require.NoError(t, err)
			assert.True(t, done)
			if allStale {
				require.Len(t, rows, 1, "advance past the all-stale first page")
				assert.Equal(t, container+"#c", rows[0].Path)
				assert.Equal(t, int64(500)*1_000_000, rows[0].CoveredThroughNS)
			} else {
				require.Len(t, rows, 2, "omit stale-version rows")
				assert.Equal(t, container+"#a", rows[0].Path)
				assert.Equal(t, int64(100)*1_000_000, rows[0].CoveredThroughNS)
				assert.Equal(t, container+"#b", rows[1].Path)
				assert.Equal(t, int64(30)*1_000_000, rows[1].CoveredThroughNS)
			}
		})
	}
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
		_, err := conn.ExecContext(t.Context(),
			"INSERT INTO session (id, project_id, time_created, time_updated)"+
				" VALUES (?, 'proj', ?, ?)",
			id, base, base,
		)
		require.NoError(t, err, "insert session row")
	}

	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
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
	_, err := conn.ExecContext(t.Context(),
		"INSERT INTO session (id, project_id, time_created, time_updated)"+
			" VALUES ('ses-1', 'proj', ?, ?)",
		sessionRow, sessionRow,
	)
	require.NoError(t, err, "insert session row")
	_, err = conn.ExecContext(t.Context(),
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

	engine := NewEngine(t.Context(), openTestDB(t), EngineConfig{
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
	engine := withTestSources(&Engine{}, &engineSources{
		agentDirs: map[parser.AgentType][]string{
			parser.AgentOpenCode: {
				filepath.Dir(firstDB),
				filepath.Dir(secondDB),
			},
		},
	})

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
	for _, agent := range openCodeFamilySQLiteAgents {
		t.Run(string(agent), func(t *testing.T) {
			path := firstDB
			if agent != parser.AgentOpenCode {
				path = filepath.Join(t.TempDir(), openCodeFormatDBName(agent))
				data, err := os.ReadFile(firstDB)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, data, 0o600))
				engine.sources().agentDirs[agent] = []string{filepath.Dir(path)}
			}
			outside := filepath.Join(t.TempDir(), openCodeFormatDBName(agent))
			statPaths = nil
			states := engine.captureSQLiteContainerStates([]string{path + "#ses-1", path + "#ses-2", outside + "#ses-1"})
			require.Len(t, states, 1)
			require.Contains(t, states, path)
			assert.Equal(t, []string{filepath.Clean(path)}, statPaths)
		})
	}
}

// A write during a pass blocks promotion but keeps prior authority, so active
// use does not force the next worker into a full child scan.
func TestOpenCodeWriteDuringPassKeepsVerification(t *testing.T) {
	now := time.Unix(1000, 0)
	prior := now.Add(-time.Minute)
	origNow := openCodeContainerDigestVerifyNow
	t.Cleanup(func() { openCodeContainerDigestVerifyNow = origNow })
	openCodeContainerDigestVerifyNow = func() time.Time { return now }

	for _, tc := range []struct {
		name                   string
		incomplete, digestList bool
	}{
		{name: "watcher pass", incomplete: true},
		{name: "watermark pass"},
		{name: "digest pass", digestList: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath, conn := newContainerTestDB(t)
			capture, ok := parser.StatSQLiteContainerState(dbPath)
			require.True(t, ok)
			states := map[string]parser.SQLiteContainerState{dbPath: capture}
			e := &Engine{db: openTestDB(t)}
			writeContainerVerification(t, e.db, dbPath, capture, prior)
			require.NoError(t, e.prepareSQLiteContainerVerification(t.Context(), states))
			file := parser.DiscoveredFile{Agent: parser.AgentOpenCode, Path: dbPath + "#ses-1"}
			e.beginSQLiteContainerPass([]parser.DiscoveredFile{file}, states)
			e.containerPass.fullDigestListed[dbPath] = tc.digestList
			e.noteSQLiteContainerResult(file.Path, true)
			_, err := conn.ExecContext(t.Context(), "INSERT INTO session (id) VALUES ('during')")
			require.NoError(t, err)
			e.finishSQLiteContainerPass(t.Context(), tc.incomplete, false)

			record, found := readContainerVerification(t, e.db, dbPath)
			require.True(t, found)
			assert.Equal(t, capture, record.State, "a write during the pass must not advance trust")
			assert.True(t, prior.Equal(record.VerifiedAt), "a write during the pass must not renew verification")
			current, ok := parser.StatSQLiteContainerState(dbPath)
			require.True(t, ok)
			require.NotEqual(t, capture, current)
			next := map[string]parser.SQLiteContainerState{dbPath: current}
			restarted := &Engine{db: e.db}
			require.NoError(t, restarted.prepareSQLiteContainerVerification(t.Context(), next))
			assert.True(t, restarted.sqliteContainerListsWatermarkOnly(next)(dbPath),
				"the next worker keeps timestamp-first discovery within the original window")
		})
	}
}

// Preparation needs the writer only to consume live authority, so passes
// during a writer handoff still run when every record is already revoked.
func TestOpenCodePrepareWritesOnlyToConsumeAuthority(t *testing.T) {
	now := time.Unix(1000, 0)
	prior := now.Add(-time.Minute)
	origNow := openCodeContainerDigestVerifyNow
	t.Cleanup(func() { openCodeContainerDigestVerifyNow = origNow })
	openCodeContainerDigestVerifyNow = func() time.Time { return now }

	for _, authority := range []bool{false, true} {
		t.Run(strconv.FormatBool(authority), func(t *testing.T) {
			dbPath, _ := newContainerTestDB(t)
			capture, ok := parser.StatSQLiteContainerState(dbPath)
			require.True(t, ok)
			stored := parser.SQLiteContainerState{}
			if authority {
				stored = capture
			}
			archive := openTestDB(t)
			writeContainerVerification(t, archive, dbPath, stored, prior)
			require.NoError(t, archive.CloseWriter())
			t.Cleanup(func() { require.NoError(t, archive.ReopenWriter()) })

			e := &Engine{db: archive}
			err := e.prepareSQLiteContainerVerification(t.Context(), map[string]parser.SQLiteContainerState{dbPath: capture})
			if authority {
				require.ErrorIs(t, err, db.ErrWriterClosed)
				return
			}
			require.NoError(t, err)
			assert.True(t, prior.Equal(e.digestVerifiedAt[dbPath]), "revoked records still restore verification history")
			assert.NotContains(t, e.trustedSQLiteContainers, dbPath)
		})
	}
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
// poisoned pass revokes every captured one while retaining verification history.
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
		lateWrite        bool
		durable          bool
		writeFails       bool
		canceledWrite    bool
		watermarkAdvance bool
		unloaded         bool
		result           resultKind
		finish           finishKind
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
			name: "completion write fails", durable: true, priorTrustCapture: true, priorStamp: true,
			writeFails: true, capture: captureLive, finish: finishScoped, wantTrust: wantFresh, wantStamp: wantPrior,
		},
		{
			name: "expired completion canceled", durable: true, priorTrustCapture: true, priorStamp: true,
			canceledWrite: true, capture: captureLive, digestList: true, finish: finishScoped, wantTrust: wantFresh, wantStamp: wantFresh,
		},
		{
			name: "watermark publication preserves deadline", durable: true, priorTrustCapture: true, priorStamp: true,
			watermarkAdvance: true, capture: captureLive, finish: finishScoped, wantTrust: wantFresh, wantStamp: wantPrior,
		},
		{
			name: "full pass drops the undiscovered container", durable: true, priorTrustCapture: true, priorStamp: true,
			unloaded: true, sibling: true, capture: captureLive, digestList: true, wantTrust: wantFresh, wantStamp: wantFresh,
		},
		{
			name: "scoped pass keeps the entry", durable: true, priorTrustCapture: true, priorStamp: true,
			unloaded: true, sibling: true, capture: captureLive, finish: finishScoped, wantTrust: wantFresh, wantStamp: wantPrior,
			wantSiblingTrust: wantFresh, wantSiblingStamp: wantPrior,
		},
		{
			name: "incomplete pass keeps the entry", durable: true, priorTrustCapture: true, priorStamp: true,
			unloaded: true, sibling: true, capture: captureLive, finish: finishIncomplete, wantTrust: wantFresh, wantStamp: wantPrior,
			wantSiblingTrust: wantFresh, wantSiblingStamp: wantPrior,
		},
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
			// Matching trust must be revoked when discovery observes a stale capture.
			name:              "capture-discovery mismatch blocks gate skip under matching trust",
			priorTrustCapture: true,
			priorStamp:        true,
			capture:           captureStale,
			probeMemberStale:  true,
			wantTrust:         wantAbsent,
			wantStamp:         wantPrior,
		},
		{
			name:       "missing capture invalidates prior verification",
			priorTrust: true,
			priorStamp: true,
			capture:    captureMissing,
			wantTrust:  wantAbsent,
			wantStamp:  wantPrior,
		},
		{
			name:              "post-discovery write blocks promotion, per container",
			priorTrust:        true,
			priorStamp:        true,
			capture:           captureLive,
			digestList:        true,
			lateWrite:         true,
			sibling:           true,
			siblingDiscovered: true,
			wantTrust:         wantPrior,
			wantStamp:         wantPrior,
			wantSiblingTrust:  wantFresh,
			wantSiblingStamp:  wantFresh,
		},
		{
			name:       "failed digest pass revokes authority and preserves history",
			priorTrust: true,
			priorStamp: true,
			capture:    captureLive,
			digestList: true,
			result:     resultFailed,
			wantTrust:  wantAbsent,
			wantStamp:  wantPrior,
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
			name:              "attributed failure clears only its own container",
			priorTrustCapture: true,
			priorStamp:        true,
			capture:           captureLive,
			result:            resultFailed,
			finish:            finishIncomplete,
			sibling:           true,
			wantStamp:         wantPrior,
			wantSiblingStamp:  wantPrior,
			wantSiblingTrust:  wantFresh,
		},
		{
			name:              "poisoned incomplete pass revokes every captured authority",
			durable:           true,
			priorTrustCapture: true,
			priorStamp:        true,
			capture:           captureLive,
			result:            resultPoisoned,
			finish:            finishIncomplete,
			sibling:           true,
			wantStamp:         wantPrior,
			wantSiblingStamp:  wantPrior,
		},
		{
			name:              "poisoned complete pass clears instead of promoting",
			priorTrustCapture: true,
			priorStamp:        true,
			capture:           captureLive,
			digestList:        true,
			result:            resultPoisoned,
			finish:            finishFull,
			sibling:           true,
			wantTrust:         wantAbsent,
			wantStamp:         wantPrior,
			wantSiblingStamp:  wantPrior,
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
			now = time.Unix(200, 0)
			if tc.canceledWrite {
				now = prior.Add(openCodeContainerDigestVerifyInterval)
			}
			dbPath, conn := newContainerTestDB(t)
			capture, ok := parser.StatSQLiteContainerState(dbPath)
			require.True(t, ok, "container state must be readable")
			if tc.capture == captureStale {
				_, err := conn.ExecContext(t.Context(), "INSERT INTO session (id) VALUES ('w1')")
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
				if tc.priorTrust || tc.priorTrustCapture {
					e.trustedSQLiteContainers[siblingPath] = trustedSQLiteContainer{state: siblingState}
				}
				if e.digestVerifiedAt == nil {
					e.digestVerifiedAt = map[string]time.Time{}
				}
				e.digestVerifiedAt[siblingPath] = prior
			}

			if tc.durable {
				e.db = openTestDB(t)
				for path, state := range states {
					writeContainerVerification(t, e.db, path, state, prior)
				}
				require.NoError(t, e.prepareSQLiteContainerVerification(t.Context(), states))
			}
			const unloaded = "/data/legacy/opencode.db"
			if tc.unloaded {
				writeContainerVerification(t, e.db, unloaded, parser.SQLiteContainerState{}, prior)
				e.digestVerifiedAt[unloaded] = prior
			}
			if tc.watermarkAdvance {
				_, err := conn.ExecContext(t.Context(), "INSERT INTO session (id) VALUES ('advanced')")
				require.NoError(t, err)
				advanced, ok := parser.StatSQLiteContainerState(dbPath)
				require.True(t, ok)
				require.Greater(t, advanced.DBChangeCounter, capture.DBChangeCounter)
				states[dbPath] = advanced
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
				assert.False(t, e.sqliteContainerSourceFresh(t.Context(), fileA),
					"an unobserved container must not gate-skip its sessions")
			}
			if tc.lateWrite {
				_, err := conn.ExecContext(t.Context(), "INSERT INTO session (id) VALUES ('w2')")
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
			if tc.writeFails {
				raw, err := sql.Open("sqlite3", e.db.Path())
				require.NoError(t, err)
				defer raw.Close()
				_, err = raw.ExecContext(t.Context(), `CREATE TRIGGER block_verification_write BEFORE INSERT ON pg_sync_state WHEN NEW.key LIKE 'sqlite-container:%' BEGIN SELECT RAISE(ABORT, 'blocked verification write'); END`)
				require.NoError(t, err)
			}
			ctx := t.Context()
			if tc.canceledWrite {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			switch tc.finish {
			case finishFull:
				e.finishSQLiteContainerPass(ctx, false, true)
			case finishScoped:
				e.finishSQLiteContainerPass(ctx, false, false)
			case finishIncomplete:
				e.finishSQLiteContainerPass(ctx, true, false)
			}

			if tc.unloaded {
				for _, path := range []string{siblingPath, unloaded} {
					_, found := readContainerVerification(t, e.db, path)
					assert.Equal(t, tc.finish != finishFull, found)
				}
				_, stamped := e.digestVerifiedAt[unloaded]
				assert.Equal(t, tc.finish != finishFull, stamped)
			} else if tc.watermarkAdvance {
				record, found := readContainerVerification(t, e.db, dbPath)
				require.True(t, found)
				assert.Equal(t, states[dbPath], record.State)
				assert.True(t, prior.Equal(record.VerifiedAt), "watermark passes preserve the original deadline")
			} else if tc.durable {
				for path := range states {
					record, found := readContainerVerification(t, e.db, path)
					require.True(t, found)
					assert.Zero(t, record.State)
					assert.True(t, prior.Equal(record.VerifiedAt))
				}
				restarted := &Engine{db: e.db}
				require.NoError(t, restarted.prepareSQLiteContainerVerification(t.Context(), states))
				for path := range states {
					assert.False(t, restarted.sqliteContainerListsWatermarkOnly(states)(path))
					assert.True(t, prior.Equal(restarted.digestVerifiedAt[path]))
				}
				if tc.writeFails {
					now = prior.Add(openCodeContainerDigestVerifyInterval)
					fresh := &Engine{db: e.db}
					require.NoError(t, fresh.prepareSQLiteContainerVerification(t.Context(), states))
					assert.False(t, fresh.sqliteContainerListsWatermarkOnly(states)(dbPath))
					assert.False(t, fresh.digestVerifiedAt[dbPath].IsZero(), "failed invalidation writes cannot erase due work")
				}
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
					assert.True(t, prior.Equal(got),
						"%s: stamp must keep its prior age", label)
				case wantFresh:
					require.True(t, ok, "%s: stamp must be written", label)
					assert.True(t, now.Equal(got),
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

		_, err := conn.ExecContext(t.Context(), "INSERT INTO session (id) VALUES ('ses_a')")
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
		require.NoError(t, archive.UpsertSession(t.Context(), session))
		require.NoError(t, archive.SetSessionDataVersion(t.Context(), session.ID, db.CurrentDataVersion()))
	}
	e.beginSQLiteContainerPass(
		[]parser.DiscoveredFile{verified},
		map[string]parser.SQLiteContainerState{dbPath: state},
	)
	e.noteSQLiteContainerResult(verified.Path, true)
	e.finishSQLiteContainerPass(t.Context(), false, true)
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
	assert.True(t, e.sqliteContainerSourceFresh(t.Context(), verified),
		"the verified session must still gate-skip")
	assert.False(t, e.sqliteContainerSourceFresh(t.Context(), exposed),
		"a newly exposed row must parse despite the unchanged container")
}

func TestSQLiteContainerGateUsesCarriedSourcePath(t *testing.T) {
	archive := openTestDB(t)
	e := &Engine{
		db: archive,
		providerMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentOpenCode: parser.ProviderMigrationProviderAuthoritative,
		},
	}
	dbPath, _ := newContainerTestDB(t)
	state, ok := parser.StatSQLiteContainerState(dbPath)
	require.True(t, ok, "container state must be readable")
	virtualPath := parser.VirtualSourcePath(dbPath, "ses-1")
	shadowPath := filepath.Join(t.TempDir(), "ses-1.json")
	require.NoError(t, os.WriteFile(shadowPath, []byte("{}"), 0o600))

	filePath := virtualPath
	session := db.Session{
		ID: "opencode:ses-1", Agent: string(parser.AgentOpenCode),
		Project: "project", Machine: "local", FilePath: &filePath,
	}
	require.NoError(t, archive.UpsertSession(t.Context(), session))
	require.NoError(t, archive.SetSessionDataVersion(t.Context(),
		session.ID, db.CurrentDataVersion(),
	))

	virtualSource := parser.SourceRef{
		Provider: parser.AgentOpenCode, DisplayPath: virtualPath,
		FingerprintKey: virtualPath, Key: virtualPath,
	}
	e.trustedSQLiteContainers = map[string]trustedSQLiteContainer{
		dbPath: {state: state},
	}
	e.beginSQLiteContainerPass(
		[]parser.DiscoveredFile{{
			Agent: parser.AgentOpenCode, Path: virtualPath,
			ProviderSource: &virtualSource,
		}},
		map[string]parser.SQLiteContainerState{dbPath: state},
	)
	gateFile := parser.DiscoveredFile{
		Agent:          parser.AgentOpenCode,
		Path:           virtualPath,
		ProviderSource: &virtualSource,
	}
	require.True(t, e.sqliteContainerSourceFresh(t.Context(), gateFile), "the carried virtual source must be fresh")

	result, used := e.processProviderFile(t.Context(), parser.DiscoveredFile{
		Agent:           parser.AgentOpenCode,
		Path:            virtualPath,
		ProviderSource:  &virtualSource,
		ProviderProcess: true,
	})
	require.True(t, used)
	require.NoError(t, result.err)
	assert.True(t, result.skip)
	assert.Equal(t, virtualPath, result.sqliteContainerResultPath,
		"an early gate return must retain its carried source path")

	shadowSource := virtualSource
	shadowSource.DisplayPath = shadowPath
	shadowSource.FingerprintKey = shadowPath
	shadowSource.Key = shadowPath
	shadow := parser.DiscoveredFile{
		Agent: parser.AgentOpenCode, Path: virtualPath,
		ProviderSource: &shadowSource,
	}
	assert.False(t, e.sqliteContainerSourceFresh(t.Context(), shadow),
		"a resolved storage shadow must not use the SQLite container gate")
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
	t.Run("newer record stays untouched", func(t *testing.T) {
		archive := openTestDB(t)
		path, _ := newContainerTestDB(t)
		state, ok := parser.StatSQLiteContainerState(path)
		require.True(t, ok)
		raw := `{"version":2,"verified_at":"1970-01-01T00:16:40Z"}`
		require.NoError(t, archive.SetSyncState(t.Context(), sqliteContainerSyncStatePrefix+path, raw))
		e := &Engine{db: archive}
		for _, listed := range []bool{true, false} {
			e.beginStreamingSQLiteContainerPass(map[string]parser.SQLiteContainerState{path: state})
			if listed {
				e.containerPass.discovered[path], e.containerPass.completed[path] = 1, 1
				e.containerPass.fullDigestListed[path] = true
			}
			e.finishSQLiteContainerPass(t.Context(), false, true)
			got, err := archive.GetSyncState(t.Context(), sqliteContainerSyncStatePrefix+path)
			require.NoError(t, err)
			assert.Equal(t, raw, got, "publication and cleanup preserve newer records")
		}
	})
}

func TestOpenCodeEphemeralVerificationIsLocal(t *testing.T) {
	for _, seeded := range []bool{false, true} {
		t.Run(strconv.FormatBool(seeded), func(t *testing.T) {
			path, _ := seededCompositeContainerTestDB(t)
			archive := openTestDB(t)
			if seeded {
				state, ok := parser.StatSQLiteContainerState(path)
				require.True(t, ok)
				writeContainerVerification(t, archive, path, state, time.Now())
			}
			raw, err := archive.GetSyncState(t.Context(), sqliteContainerSyncStatePrefix+path)
			require.NoError(t, err)
			e := NewEngine(t.Context(), archive, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentOpenCode: {filepath.Dir(path)}},
				Machine:   "local", Ephemeral: true,
			})
			t.Cleanup(e.Close)
			scans := parser.OpenCodeContainerChildScans()
			stats := e.SyncAll(t.Context(), nil)
			require.False(t, stats.Aborted)
			assert.Equal(t, 1, stats.Synced)
			assert.Greater(t, parser.OpenCodeContainerChildScans(), scans, "ephemeral discovery must not restore durable trust")
			after, err := archive.GetSyncState(t.Context(), sqliteContainerSyncStatePrefix+path)
			require.NoError(t, err)
			assert.Equal(t, raw, after, "ephemeral verification must leave durable state untouched")
		})
	}
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

// Cutoff recovery requires a full digest listing and existing verification history.
func TestOpenCodeDigestListingForm(t *testing.T) {
	now := time.Unix(1000, 0)
	origNow := openCodeContainerDigestVerifyNow
	t.Cleanup(func() { openCodeContainerDigestVerifyNow = origNow })
	openCodeContainerDigestVerifyNow = func() time.Time { return now }

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

	t.Run("interleaved quick-sync groups span pages", func(t *testing.T) {
		archive := openTestDB(t)
		root := t.TempDir()
		states := make(map[string]parser.SQLiteContainerState)
		var files []parser.DiscoveredFile
		require.NoError(t, archive.Update(t.Context(), func(tx *sql.Tx) error {
			for i := range 513 {
				for _, agent := range []parser.AgentType{parser.AgentOpenCode, parser.AgentMiMoCode} {
					container := filepath.Join(root, openCodeFormatDBName(agent))
					states[container] = parser.SQLiteContainerState{DBInode: 1, DBDevice: 1}
					path := fmt.Sprintf("%s#ses-%04d", container, i)
					source := parser.SourceRef{Provider: agent, DisplayPath: path}
					files = append(files, parser.DiscoveredFile{Agent: agent, Path: path, ProviderSource: &source})
					if _, err := tx.ExecContext(t.Context(), `INSERT INTO sessions (id, agent, project, file_path, deleted_at)
						VALUES (?, ?, 'project', ?, '2026-01-01T00:00:00Z')`, fmt.Sprintf("%s:%d", agent, i), agent, path); err != nil {
						return err
					}
				}
			}
			return nil
		}))
		originalStat := statSQLiteContainerState
		statSQLiteContainerState = func(path string) (parser.SQLiteContainerState, bool) {
			state, ok := states[path]
			return state, ok
		}
		t.Cleanup(func() { statSQLiteContainerState = originalStat })
		e := &Engine{db: archive, digestVerifiedAt: make(map[string]time.Time)}
		e.beginSQLiteContainerPass(files, states)
		for container := range states {
			e.digestVerifiedAt[container] = now.Add(-openCodeContainerDigestVerifyInterval)
			e.containerPass.fullDigestListed[container] = true
		}
		pages := countSessionFileHashReads(t, archive)
		assert.Empty(t, e.filterQuickSyncFiles(t.Context(), files, now))
		assert.Equal(t, int32(4), pages.Load(), "each container/agent group reads two pages once")

		canceled, cancel := context.WithCancel(t.Context())
		cancel()
		assert.Len(t, e.filterQuickSyncFiles(canceled, files, now), len(files),
			"unreadable stored freshness leaves every due member for workers")
	})
}

func TestOpenCodeRestartedQuickSyncReconcilesAtOriginalDeadline(t *testing.T) {
	for _, recovery := range []string{"no failure", "same engine", "fresh engine", "no native identity"} {
		t.Run(recovery, func(t *testing.T) {
			dbPath, conn := seededCompositeContainerTestDB(t)
			if recovery == "no native identity" {
				originalStat := statSQLiteContainerState
				statSQLiteContainerState = func(path string) (parser.SQLiteContainerState, bool) {
					state, ok := originalStat(path)
					state.DBDevice = 0
					return state, ok
				}
				t.Cleanup(func() { statSQLiteContainerState = originalStat })
			}
			archive := openTestDB(t)
			factory, ok := parser.ProviderFactoryByType(parser.AgentOpenCode)
			require.True(t, ok)
			failing := &failingDBBackedProvider{
				Def: parser.AgentDef{Type: parser.AgentClaude, DisplayName: "Claude", FileBased: true},
				err: errors.New("provider discovery unavailable"),
			}
			otherRoot := t.TempDir()
			newEngine := func() *Engine {
				engine := NewEngine(t.Context(), archive, EngineConfig{
					AgentDirs: map[parser.AgentType][]string{parser.AgentOpenCode: {filepath.Dir(dbPath)}, parser.AgentClaude: {otherRoot}}, Machine: "local",
					ProviderFactories:      []parser.ProviderFactory{factory, failingDBBackedFactory{provider: failing}},
					ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{parser.AgentClaude: parser.ProviderMigrationProviderAuthoritative},
				})
				t.Cleanup(engine.Close)
				return engine
			}
			verifiedAt := time.UnixMilli(1779100000000)
			now := verifiedAt
			originalNow := openCodeContainerDigestVerifyNow
			openCodeContainerDigestVerifyNow = func() time.Time { return now }
			t.Cleanup(func() { openCodeContainerDigestVerifyNow = originalNow })
			initial := newEngine()
			require.Equal(t, 1, initial.SyncAll(t.Context(), nil).Synced)
			childUpdated := verifiedAt.Add(30 * time.Second).UnixMilli()
			if recovery == "no native identity" {
				childUpdated = 1779012500001
				now = verifiedAt.Add(30 * time.Second)
				require.NoError(t, initial.SyncPathsContext(t.Context(), []string{dbPath + "#ses-1"}))
				record, found := readContainerVerification(t, archive, dbPath)
				require.True(t, found)
				assert.True(t, verifiedAt.Equal(record.VerifiedAt), "partial work preserves verification age")
			}
			_, err := conn.ExecContext(t.Context(),
				`UPDATE part SET data = replace(data, 'original', 'modified'), time_updated = ?`,
				childUpdated)
			require.NoError(t, err)
			if recovery == "no native identity" {
				record, found := readContainerVerification(t, archive, dbPath)
				assert.True(t, found, "complete checks retain history without native identity")
				assert.Zero(t, record.State)
				engine := initial
				for pass := range 2 {
					if pass == 1 {
						_, err := conn.ExecContext(t.Context(), `UPDATE part SET data=replace(data, 'modified', 'repaired'), time_updated=time_updated+1`)
						require.NoError(t, err)
						engine = newEngine()
					}
					now = verifiedAt.Add(time.Duration(pass+1) * time.Minute)
					scans := parser.OpenCodeContainerChildScans()
					require.Equal(t, 1, engine.SyncAllSince(t.Context(), now.Add(-10*time.Second), nil).Synced)
					assert.Equal(t, int64(1), parser.OpenCodeContainerChildScans()-scans)
					record, found = readContainerVerification(t, archive, dbPath)
					require.True(t, found)
					assert.Zero(t, record.State)
					assert.True(t, now.Equal(record.VerifiedAt))
				}
				matches, err := archive.SearchSession(t.Context(), "opencode:ses-1", "repaired")
				require.NoError(t, err)
				assert.Len(t, matches, 2)
				return
			}
			previousStart, lastVerified := verifiedAt, verifiedAt
			var failedEngine *Engine
			recovered := false
			for minute := 1; minute <= 6; minute++ {
				now = verifiedAt.Add(time.Duration(minute) * time.Minute)
				engine := newEngine()
				if recovery == "same engine" && failedEngine != nil {
					engine = failedEngine
				}
				if minute == 2 && recovery != "no failure" {
					failing.failOnCall = failing.calls.Load() + 1
					failedEngine = engine
				}
				scans := parser.OpenCodeContainerChildScans()
				stats := engine.SyncAllSince(t.Context(), previousStart.Add(-10*time.Second), nil)
				record, found := readContainerVerification(t, archive, dbPath)
				require.True(t, found, "discovery failure preserves verification history")
				if minute == 2 && recovery != "no failure" {
					require.False(t, stats.ProcessingComplete())
					assert.Equal(t, parser.SQLiteContainerState{}, record.State)
				} else {
					require.True(t, stats.ProcessingComplete())
					due := !recovered && (minute == 5 || (minute == 3 && recovery != "no failure"))
					if due {
						require.Equal(t, 1, stats.Synced, "complete verification bypasses the advanced cutoff")
						assert.Equal(t, int64(1), parser.OpenCodeContainerChildScans()-scans)
						lastVerified, recovered = now, true
					} else {
						require.Zero(t, stats.Synced)
						assert.Zero(t, parser.OpenCodeContainerChildScans()-scans, "successful verification ends pending complete checks")
					}
				}
				assert.True(t, lastVerified.Equal(record.VerifiedAt), "only successful complete checks renew verification")
				previousStart = now
			}
			matches, err := archive.SearchSession(t.Context(), "opencode:ses-1", "modified")
			require.NoError(t, err)
			assert.Len(t, matches, 2)
		})
	}
}

func TestOpenCodeCheckpointedChildEditReconcilesAcrossRestart(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		streamed, matchingMarkers, exact bool
	}{
		{"full/matching markers", false, true, false},
		{"streamed/matching markers", true, true, false},
		{"exact member/advanced markers", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath, conn := seededCompositeContainerTestDB(t)
			_, err := conn.ExecContext(t.Context(), "PRAGMA journal_mode=WAL; PRAGMA wal_checkpoint(TRUNCATE)")
			require.NoError(t, err)
			recordedSecond := time.Unix(1700000000, 0)
			require.NoError(t, os.Chtimes(dbPath, recordedSecond, recordedSecond))
			before, ok := parser.StatSQLiteContainerState(dbPath)
			require.True(t, ok)
			archive := openTestDB(t)
			newEngine := func() *Engine {
				engine := NewEngine(t.Context(), archive, EngineConfig{
					AgentDirs: map[parser.AgentType][]string{parser.AgentOpenCode: {filepath.Dir(dbPath)}}, Machine: "local",
				})
				t.Cleanup(engine.Close)
				return engine
			}
			origNow := openCodeContainerDigestVerifyNow
			t.Cleanup(func() { openCodeContainerDigestVerifyNow = origNow })
			verifiedAt := time.Unix(1000, 0)
			now := verifiedAt
			openCodeContainerDigestVerifyNow = func() time.Time { return now }
			require.Equal(t, 1, newEngine().SyncAll(t.Context(), nil).Synced)
			_, err = conn.ExecContext(t.Context(), `UPDATE part SET data = replace(data, 'original', 'modified'), time_updated = time_updated+1`)
			require.NoError(t, err)
			if tc.matchingMarkers {
				_, err = conn.ExecContext(t.Context(), "PRAGMA wal_checkpoint(TRUNCATE)")
				require.NoError(t, err)
				require.NoError(t, os.Chtimes(dbPath, recordedSecond, recordedSecond))
			}
			after, ok := parser.StatSQLiteContainerState(dbPath)
			require.True(t, ok)
			if tc.matchingMarkers {
				require.Equal(t, before, after, "same-size checkpointed WAL edits can preserve every captured marker")
			} else {
				require.NotEqual(t, before, after, "ordinary child writes change container markers")
			}
			now = verifiedAt.Add(time.Minute)
			recent := newEngine()
			scans := parser.OpenCodeContainerChildScans()
			if tc.streamed {
				require.NoError(t, recent.ReconcileWatchRoots(t.Context(), []string{filepath.Dir(dbPath)}, false))
				require.Zero(t, recent.LastSyncStats().Synced)
			} else {
				require.Zero(t, recent.SyncAll(t.Context(), nil).Synced)
			}
			matches, err := archive.SearchSession(t.Context(), "opencode:ses-1", "modified")
			require.NoError(t, err)
			assert.Empty(t, matches, "child-only edits wait for the original verification deadline")
			assert.Zero(t, parser.OpenCodeContainerChildScans()-scans)
			if tc.exact {
				record, found := readContainerVerification(t, archive, dbPath)
				require.True(t, found)
				require.Equal(t, after, record.State)
				require.True(t, verifiedAt.Equal(record.VerifiedAt))
			}
			now = verifiedAt.Add(openCodeContainerDigestVerifyInterval)
			due := newEngine()
			scans = parser.OpenCodeContainerChildScans()
			lookups := parser.OpenCodeSessionChildLookups()
			if tc.exact {
				require.NoError(t, due.SyncPathsContext(t.Context(), []string{dbPath + "#ses-1"}))
				assert.Equal(t, 1, due.LastSyncStats().Synced)
			} else if tc.streamed {
				require.NoError(t, due.ReconcileWatchRoots(t.Context(), []string{filepath.Dir(dbPath)}, false))
				assert.Equal(t, 1, due.LastSyncStats().Synced)
			} else {
				assert.Equal(t, 1, due.SyncAll(t.Context(), nil).Synced)
			}
			if tc.exact {
				assert.Zero(t, parser.OpenCodeContainerChildScans()-scans)
				assert.Equal(t, int64(1), parser.OpenCodeSessionChildLookups()-lookups)
			} else {
				assert.Equal(t, int64(1), parser.OpenCodeContainerChildScans()-scans)
			}
			messages, err := archive.GetAllMessages(t.Context(), "opencode:ses-1")
			require.NoError(t, err)
			require.Len(t, messages, 2)
			assert.Equal(t, "modified prompt", messages[0].Content)
			assert.Equal(t, "modified answer", messages[1].Content)
			matches, err = archive.SearchSession(t.Context(), "opencode:ses-1", "modified")
			require.NoError(t, err)
			assert.Len(t, matches, 2)
		})
	}
}

func seededCompositeContainerTestDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dbPath, conn := newCompositeContainerTestDB(t)
	_, err := conn.ExecContext(t.Context(), `
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

	return dbPath, conn
}

func TestOpenCodePersistedVerificationAdmission(t *testing.T) {
	archive := openTestDB(t)
	now := time.Unix(1000, 0)
	origNow := openCodeContainerDigestVerifyNow
	t.Cleanup(func() { openCodeContainerDigestVerifyNow = origNow })
	openCodeContainerDigestVerifyNow = func() time.Time { return now }
	for _, tc := range []struct {
		name                               string
		age                                time.Duration
		setup                              func(*Engine, *parser.SQLiteContainerState)
		absent, canceled                   bool
		localLegacy, localTimed, wantTrust bool
		raw                                string
		version                            int
		want                               bool
		wantNil, concurrent                bool
	}{
		{name: "watermark publication preserves deadline", age: time.Minute, want: true},
		{name: "force parse overrides a current stamp", wantNil: true, setup: func(e *Engine, _ *parser.SQLiteContainerState) { e.forceParse = true }},
		{name: "force full parse overrides a current stamp", wantNil: true, setup: func(e *Engine, _ *parser.SQLiteContainerState) { e.forceFullParse = true }},
		{name: "expired", age: openCodeContainerDigestVerifyInterval},
		{name: "future timestamp", age: -time.Second},
		{name: "absent", absent: true},
		{name: "untimed legacy trust", absent: true, localLegacy: true, wantTrust: true},
		{name: "timed authority without record", absent: true, localLegacy: true, localTimed: true},
		{name: "legacy trust with revoked record", raw: `{"version":1,"verified_at":"1970-01-01T00:16:40Z"}`, localLegacy: true},
		{name: "legacy trust with malformed record", raw: "{", localLegacy: true},
		{name: "changed counter", want: true, setup: func(_ *Engine, s *parser.SQLiteContainerState) { s.DBChangeCounter++ }},
		{name: "counter rollback", setup: func(_ *Engine, s *parser.SQLiteContainerState) { s.DBChangeCounter-- }},
		{name: "malformed record", raw: "{"},
		{name: "malformed timestamp", raw: `{"version":1,"verified_at":"invalid"}`},
		{name: "missing timestamp", version: 1},
		{name: "future record version", age: time.Minute, version: 2},
		{name: "replaced", setup: func(_ *Engine, s *parser.SQLiteContainerState) { s.DBInode++ }},
		{name: "missing identity", setup: func(_ *Engine, s *parser.SQLiteContainerState) { s.DBInode = 0 }},
		{name: "rewritten path", setup: func(e *Engine, _ *parser.SQLiteContainerState) { e.pathRewriter = func(s string) string { return s } }},
		{name: "read only", setup: func(e *Engine, _ *parser.SQLiteContainerState) {
			reader, err := db.OpenReadOnly(t.Context(), archive.Path())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reader.Close()) })
			e.db = reader
		}},
		{name: "read fails", canceled: true},
		{name: "one concurrent owner", concurrent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, _ := newContainerTestDB(t)
			state, ok := parser.StatSQLiteContainerState(path)
			require.True(t, ok)
			if !tc.absent {
				writeContainerVerification(t, archive, path, state, now.Add(-tc.age))
			}
			if tc.raw != "" {
				require.NoError(t, archive.SetSyncState(t.Context(), sqliteContainerSyncStatePrefix+path, tc.raw))
			}
			if tc.version != 0 {
				at := now.Add(-tc.age)
				if tc.name == "missing timestamp" {
					at = time.Time{}
				}
				raw, err := json.Marshal(sqliteContainerVerification{Version: tc.version, State: state, VerifiedAt: at})
				require.NoError(t, err)
				tc.raw = string(raw)
				require.NoError(t, archive.SetSyncState(t.Context(), sqliteContainerSyncStatePrefix+path, tc.raw))
			}
			e := &Engine{db: archive}
			if tc.localLegacy {
				e.trustedSQLiteContainers = map[string]trustedSQLiteContainer{path: {state: state}}
				if tc.localTimed {
					e.digestVerifiedAt = map[string]time.Time{path: now}
				}
			}
			if tc.setup != nil {
				tc.setup(e, &state)
			}
			ctx := t.Context()
			if tc.canceled {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			states := map[string]parser.SQLiteContainerState{path: state}
			if tc.concurrent {
				results := make(chan bool, 2)
				for range 2 {
					go func() {
						engine := &Engine{db: archive}
						err := engine.prepareSQLiteContainerVerification(t.Context(), states)
						results <- err == nil && engine.sqliteContainerListsWatermarkOnly(states)(path)
					}()
				}
				owners := 0
				for range 2 {
					if <-results {
						owners++
					}
				}
				assert.Equal(t, 1, owners)
				return
			}
			err := e.prepareSQLiteContainerVerification(ctx, states)
			if tc.canceled {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			fn := e.sqliteContainerListsWatermarkOnly(states)
			if tc.wantNil {
				assert.Nil(t, fn)
			}
			got := fn != nil && fn(path)
			assert.Equal(t, tc.want, got)
			if tc.version > 1 {
				raw, err := archive.GetSyncState(t.Context(), sqliteContainerSyncStatePrefix+path)
				require.NoError(t, err)
				assert.Equal(t, tc.raw, raw, "admission leaves unsupported records unchanged")
			}

			if tc.localLegacy {
				_, trusted := e.trustedSQLiteContainers[path]
				assert.Equal(t, tc.wantTrust, trusted)
				if tc.wantTrust {
					assert.Empty(t, e.digestVerifiedAt)
				}
			}
			if tc.canceled {
				require.NoError(t, e.prepareSQLiteContainerVerification(t.Context(), states))
				assert.True(t, e.sqliteContainerListsWatermarkOnly(map[string]parser.SQLiteContainerState{path: state})(path), "a successful read can restore trust after a transient error")
			}
			if tc.want {
				assert.True(t, now.Add(-tc.age).Equal(e.digestVerifiedAt[path]))
			}
		})
	}
}

type openCodeMemberFailureFactory struct {
	parser.ProviderFactory
	calls       *atomic.Int32
	blockedPath *string
}

func (f openCodeMemberFailureFactory) NewProvider(cfg parser.ProviderConfig) parser.Provider {
	return openCodeMemberFailureProvider{failureFingerprintProvider{Provider: f.ProviderFactory.NewProvider(cfg), calls: f.calls}, f.blockedPath}
}

type openCodeMemberFailureProvider struct {
	failureFingerprintProvider
	blockedPath *string
}

func (p openCodeMemberFailureProvider) SourcesForChangedPath(ctx context.Context, req parser.ChangedPathRequest) ([]parser.SourceRef, error) {
	if req.Path == *p.blockedPath {
		return nil, errors.New("container classification failed")
	}
	return p.Provider.SourcesForChangedPath(ctx, req)
}

func TestOpenCodePartialVerificationRejectsRollbackAcrossRestart(t *testing.T) {
	for _, mode := range []string{"partial", "cutoff", "single session"} {
		t.Run(mode, func(t *testing.T) {
			path, conn := seededCompositeContainerTestDB(t)
			_, err := conn.ExecContext(t.Context(), `INSERT INTO session SELECT 'ses-2', project_id, parent_id, title, time_created, time_updated FROM session;
				INSERT INTO message SELECT 'msg-2', 'ses-2', data, time_created, time_updated FROM message WHERE id='msg-user';
				INSERT INTO part SELECT 'part-2', 'ses-2', 'msg-2', data, time_created, time_updated FROM part WHERE id='part-user'`)
			require.NoError(t, err)
			archive := openTestDB(t)
			cfg := EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentOpenCode: {filepath.Dir(path)}}, Machine: "local"}
			engine := NewEngine(t.Context(), archive, cfg)
			t.Cleanup(engine.Close)
			require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
			original, found := readContainerVerification(t, archive, path)
			require.True(t, found)
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), `UPDATE session SET time_updated=time_updated+1000 WHERE id='ses-1'; UPDATE part SET data=replace(data, 'original', 'modified'), time_updated=time_updated+1 WHERE session_id='ses-1'`)
			require.NoError(t, err)
			switch mode {
			case "partial":
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path + "#ses-1"}))
			case "single session":
				require.NoError(t, engine.SyncSingleSessionContext(t.Context(), "opencode:ses-1"))
			default:
				stats := engine.SyncAllSince(t.Context(), time.UnixMilli(1779099999500), nil)
				require.True(t, stats.ProcessingComplete())
			}
			messages, err := archive.GetAllMessages(t.Context(), "opencode:ses-1")
			require.NoError(t, err)
			require.Len(t, messages, 2)
			require.Equal(t, "modified prompt", messages[0].Content)
			advanced, ok := parser.StatSQLiteContainerState(path)
			require.True(t, ok)
			require.Greater(t, advanced.DBChangeCounter, original.State.DBChangeCounter)
			record, found := readContainerVerification(t, archive, path)
			require.True(t, found)
			assert.Equal(t, original.State, record.State, "partial work cannot authorize skipping untouched members")
			assert.True(t, original.VerifiedAt.Equal(record.VerifiedAt))
			require.NoError(t, conn.Close())
			require.NoError(t, os.WriteFile(path, before, 0o600))
			restored, ok := parser.StatSQLiteContainerState(path)
			require.True(t, ok)
			require.Equal(t, advanced.DBInode, restored.DBInode)
			require.Less(t, restored.DBChangeCounter, advanced.DBChangeCounter)
			restarted := NewEngine(t.Context(), archive, cfg)
			t.Cleanup(restarted.Close)
			require.Equal(t, 1, restarted.SyncAllSince(t.Context(), time.Now(), nil).Synced)
			messages, err = archive.GetAllMessages(t.Context(), "opencode:ses-1")
			require.NoError(t, err)
			require.Len(t, messages, 2)
			assert.Equal(t, "original prompt", messages[0].Content)
		})
	}
}

func (p openCodeMemberFailureProvider) Parse(ctx context.Context, req parser.ParseRequest) (parser.ParseOutcome, error) {
	if req.Source.DisplayPath == *p.blockedPath {
		var data any
		err := json.Unmarshal([]byte("!"), &data)
		return parser.ParseOutcome{}, fmt.Errorf("decode %s: %w", req.Source.DisplayPath, err)
	}
	return p.Provider.Parse(ctx, req)
}

func TestOpenCodePartialPassConsumesVerification(t *testing.T) {
	for _, mode := range []string{"failed first partial pass", "preparation write fails", "initial capture fails", "planned virtual member", "classification failure", "restarted classification failure", "single session failure", "single session preparation write fails"} {
		t.Run(mode, func(t *testing.T) {
			preparationFails := mode == "preparation write fails" || mode == "single session preparation write fails"
			classificationFails := mode == "classification failure" || mode == "restarted classification failure"
			path, conn := seededCompositeContainerTestDB(t)
			archive := openTestDB(t)
			factory, ok := parser.ProviderFactoryByType(parser.AgentOpenCode)
			require.True(t, ok)
			var calls atomic.Int32
			blockedPath := ""
			cfg := EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentOpenCode: {filepath.Dir(path)}}, Machine: "local",
				ProviderFactories: []parser.ProviderFactory{openCodeMemberFailureFactory{factory, &calls, &blockedPath}},
			}
			first := NewEngine(t.Context(), archive, cfg)
			t.Cleanup(first.Close)
			require.Equal(t, 1, first.SyncAll(t.Context(), nil).Synced)
			original, found := readContainerVerification(t, archive, path)
			require.True(t, found)
			if !classificationFails {
				_, err := conn.ExecContext(t.Context(), `UPDATE session SET time_updated=time_updated+1; UPDATE part SET data=replace(data, 'original', 'modified'), time_updated=time_updated+1 WHERE id='part-user'`)
				require.NoError(t, err)
			}
			if preparationFails {
				raw, err := sql.Open("sqlite3", archive.Path())
				require.NoError(t, err)
				t.Cleanup(func() { raw.Close() })
				_, err = raw.ExecContext(t.Context(), `CREATE TRIGGER block_consumption BEFORE UPDATE ON pg_sync_state BEGIN SELECT RAISE(ABORT, 'blocked authority consumption'); END`)
				require.NoError(t, err)
			}
			blockedPath = path + "#ses-1"
			if classificationFails {
				blockedPath = path
			}
			fresh := NewEngine(t.Context(), archive, cfg)
			t.Cleanup(fresh.Close)
			calls.Store(0)
			if mode == "initial capture fails" {
				originalStat := statSQLiteContainerState
				failed := false
				statSQLiteContainerState = func(p string) (parser.SQLiteContainerState, bool) {
					if p == path && !failed {
						failed = true
						return parser.SQLiteContainerState{}, false
					}
					return originalStat(p)
				}
				t.Cleanup(func() { statSQLiteContainerState = originalStat })
			}
			if mode == "planned virtual member" {
				provider := factory.NewProvider(parser.ProviderConfig{Roots: []string{filepath.Dir(path)}})
				source, found, err := provider.FindSource(t.Context(), parser.FindSourceRequest{StoredFilePath: blockedPath})
				require.NoError(t, err)
				require.True(t, found)
				_, err = fresh.SyncChangedPathPlanWithOptionsContext(t.Context(), ChangedPathPlan{Files: []parser.DiscoveredFile{{
					Agent: parser.AgentOpenCode, Path: blockedPath, ProviderSource: &source, ProviderProcess: true,
				}}}, ChangedPathSyncOptions{}, nil)
				require.Error(t, err)
			} else if strings.HasPrefix(mode, "single session") {
				require.Error(t, fresh.SyncSingleSessionContext(t.Context(), "opencode:ses-1"))
			} else {
				changed := path
				require.Error(t, fresh.SyncPathsContext(t.Context(), []string{changed}))
			}
			record, found := readContainerVerification(t, archive, path)
			require.True(t, found)
			assert.True(t, original.VerifiedAt.Equal(record.VerifiedAt))
			if preparationFails {
				assert.Zero(t, calls.Load(), "preparation failure aborts before worker fingerprinting")
				assert.Equal(t, original.State, record.State)
				return
			}
			assert.Equal(t, parser.SQLiteContainerState{}, record.State, "first partial failure cannot leave durable authority")
			if classificationFails {
				assert.Zero(t, fresh.LastSync(), "a failed classification is not a completed sync")
				_, err := conn.ExecContext(t.Context(), `UPDATE part SET data=replace(data, 'original', 'repaired'), time_updated=time_updated+1`)
				require.NoError(t, err)
				blockedPath = ""
				if mode == "restarted classification failure" {
					fresh = NewEngine(t.Context(), archive, cfg)
					t.Cleanup(fresh.Close)
				}
				require.Equal(t, 1, fresh.SyncAllSince(t.Context(), time.Now(), nil).Synced)
				matches, err := archive.SearchSession(t.Context(), "opencode:ses-1", "repaired")
				require.NoError(t, err)
				assert.Len(t, matches, 2)
			}
		})
	}
}

func TestOpenCodeFailedContainerRetriesOnlyUnresolvedMembers(t *testing.T) {
	path, conn := seededCompositeContainerTestDB(t)
	_, err := conn.ExecContext(t.Context(), `
        WITH RECURSIVE copies(n) AS (VALUES(2) UNION ALL SELECT n+1 FROM copies WHERE n<33)
        INSERT INTO session (id, project_id, time_created, time_updated)
        SELECT 'ses-'||n, project_id, time_created, time_updated FROM session, copies WHERE id='ses-1';
        INSERT INTO message (id, session_id, data, time_created, time_updated)
        SELECT 'msg-'||id, id, '{"role":"user"}', time_created, time_updated FROM session WHERE id!='ses-1';
        INSERT INTO part (id, session_id, message_id, data, time_created, time_updated)
        SELECT 'part-'||id, session_id, id, '{"type":"text","text":"original prompt"}', time_created, time_updated FROM message WHERE session_id!='ses-1'
    `)
	require.NoError(t, err)
	archive := openTestDB(t)
	factory, ok := parser.ProviderFactoryByType(parser.AgentOpenCode)
	require.True(t, ok)
	var calls atomic.Int32
	blockedPath := ""
	engine := NewEngine(t.Context(), archive, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentOpenCode: {filepath.Dir(path)}}, Machine: "local",
		ProviderFactories: []parser.ProviderFactory{openCodeMemberFailureFactory{factory, &calls, &blockedPath}},
	})
	t.Cleanup(engine.Close)
	originalNow := openCodeContainerDigestVerifyNow
	now := time.UnixMilli(1779100000000)
	openCodeContainerDigestVerifyNow = func() time.Time { return now }
	t.Cleanup(func() { openCodeContainerDigestVerifyNow = originalNow })
	require.Equal(t, 33, engine.SyncAll(t.Context(), nil).Synced)
	_, err = conn.ExecContext(t.Context(), `UPDATE part SET time_updated=time_updated+1 WHERE id='part-user'; UPDATE part SET data=replace(data, 'original', 'modified'), time_updated=time_updated+1 WHERE session_id='ses-2'`)
	require.NoError(t, err)
	blockedPath = path + "#ses-1"
	now = now.Add(openCodeContainerDigestVerifyInterval)
	calls.Store(0)
	first := engine.SyncAllSince(t.Context(), now.Add(-10*time.Second), nil)
	assert.False(t, first.ProcessingComplete())
	require.Equal(t, 1, first.Synced)
	assert.Equal(t, int32(2), calls.Load(), "unchanged members finish before worker dispatch")
	calls.Store(0)
	second := engine.SyncAllSince(t.Context(), now.Add(-10*time.Second), nil)
	assert.Zero(t, second.Synced)
	assert.Equal(t, int32(1), calls.Load(), "a persistent failure retries only the unresolved member")
	_, err = conn.ExecContext(t.Context(), `UPDATE part SET data='{"type":"text","text":"repaired prompt"}', time_updated=time_updated+1 WHERE id='part-user'`)
	require.NoError(t, err)
	blockedPath = ""
	require.Equal(t, 1, engine.SyncAllSince(t.Context(), now.Add(-10*time.Second), nil).Synced)
	calls.Store(0)
	settled := engine.SyncAllSince(t.Context(), now.Add(-10*time.Second), nil)
	require.True(t, settled.ProcessingComplete())
	assert.Zero(t, calls.Load(), "successful complete verification ends recovery work")
	matches, err := archive.SearchSession(t.Context(), "opencode:ses-2", "modified")
	require.NoError(t, err)
	assert.Len(t, matches, 1)
}

func writeContainerVerification(t *testing.T, archive *db.DB, path string, state parser.SQLiteContainerState, at time.Time) {
	t.Helper()
	raw, err := json.Marshal(sqliteContainerVerification{Version: 1, State: state, VerifiedAt: at})
	require.NoError(t, err)
	require.NoError(t, archive.SetSyncState(t.Context(), sqliteContainerSyncStatePrefix+path, string(raw)))
}

func readContainerVerification(t *testing.T, archive *db.DB, path string) (sqliteContainerVerification, bool) {
	t.Helper()
	raw, err := archive.GetSyncState(t.Context(), sqliteContainerSyncStatePrefix+path)
	require.NoError(t, err)
	var record sqliteContainerVerification
	if raw == "" {
		return record, false
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &record))
	return record, true
}
