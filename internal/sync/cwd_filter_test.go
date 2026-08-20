package sync

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

type cwdFilterCase struct {
	name     string
	prefixes []string
	cwd      string
	want     bool
}

func TestCwdPrefixFilterAllows(t *testing.T) {
	tests := []cwdFilterCase{
		{"empty filter allows anything", nil, "/anywhere", true},
		{"empty filter allows empty cwd", nil, "", true},
		{"exact match", []string{"/a/b"}, "/a/b", true},
		{"child path", []string{"/a/b"}, "/a/b/c/d", true},
		{"sibling with shared prefix", []string{"/a/b"}, "/a/bc", false},
		{"outside prefix", []string{"/a/b"}, "/x", false},
		{"empty cwd rejected when filter set", []string{"/a/b"}, "", false},
		{"second prefix matches", []string{"/a/b", "/x/y"}, "/x/y/z", true},
		{"trailing separator normalized", []string{"/a/b/"}, "/a/b/c", true},
		{"prefix longer than cwd", []string{"/a/b/c"}, "/a/b", false},
		{"case sensitive", []string{"/a/B"}, "/a/b/c", false},
		{"blank entries ignored", []string{"  ", ""}, "/anywhere", true},
		{"root prefix allows any cwd", []string{"/"}, "/anywhere", true},
		{"dot-dot escaping the prefix rejected", []string{"/a/b"}, "/a/b/../c", false},
		{"dot-dot staying inside allowed", []string{"/a/b"}, "/a/b/c/../d", true},
		{"dot-dot in prefix cleaned", []string{"/a/b/../c"}, "/a/c/d", true},
	}
	if runtime.GOOS == "windows" {
		tests = append(tests,
			cwdFilterCase{"backslash boundary", []string{`C:\work`}, `C:\work\repo`, true},
			cwdFilterCase{"drive sibling", []string{`C:\work`}, `C:\workspace`, false},
			cwdFilterCase{"mixed separators normalized", []string{`C:/work`}, `C:\work\repo`, true},
		)
	} else {
		// On POSIX a backslash is an ordinary filename character:
		// "b\evil" is a sibling of "b" under /a, not a child of /a/b.
		tests = append(tests, cwdFilterCase{
			"backslash is not a separator", []string{"/a/b"}, `/a/b\evil`, false,
		})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCwdPrefixFilter(tt.prefixes)
			assert.Equal(t, tt.want, f.allows(tt.cwd))
		})
	}
}

// exclusionGateJob builds a syncJob whose parse supersedes the
// archived "stale" row (via excludedSessionIDs) with a replacement
// session recorded at the given cwd.
func exclusionGateJob(cwd string) syncJob {
	return syncJob{
		path: "/src/session.jsonl",
		processResult: processResult{
			excludedSessionIDs: []string{"stale"},
			results: []parser.ParseResult{
				{Session: parser.ParsedSession{
					ID:      "replacement",
					Agent:   parser.AgentClaude,
					Machine: "local",
					Project: "proj",
					Cwd:     cwd,
				}},
			},
		},
	}
}

// A parse whose sessions are all outside the cwd allow-list must not
// delete the archived rows its exclusion list supersedes: the
// replacement write is vetoed, so the delete would erase a session
// the filter promises to preserve.
func TestCollectAndBatchGatesParserExclusionsByCwdFilter(t *testing.T) {
	ctx := context.Background()

	t.Run("filtered source keeps archived row", func(t *testing.T) {
		database := openTestDB(t)
		require.NoError(t, database.UpsertSession(db.Session{
			ID: "stale", Project: "proj", Machine: "local", Agent: "claude",
		}))
		e := NewEngine(database, EngineConfig{
			Machine:            "local",
			IncludeCwdPrefixes: []string{"/allowed"},
		})

		results := make(chan syncJob, 1)
		results <- exclusionGateJob("/outside/repo")
		close(results)
		stats := e.collectAndBatch(
			ctx, results, 1, 1, nil, syncWriteDefault,
		)

		gotStale, err := database.GetSession(ctx, "stale")
		require.NoError(t, err)
		assert.NotNil(t, gotStale,
			"archived row must survive exclusions from a filtered source")
		gotNew, err := database.GetSession(ctx, "replacement")
		require.NoError(t, err)
		assert.Nil(t, gotNew, "filtered replacement must not be written")
		assert.Empty(t, stats.parserExcludedIDs,
			"frozen exclusions must not reach resync orphan-copy exclusion")
		assert.Equal(t, 1, stats.cwdFilteredSessions, "filtered sessions")
		assert.Equal(t, 1, stats.cwdFilteredFiles, "filtered files")
		assert.Equal(t, 0, stats.Synced, "synced")
	})

	t.Run("allowed source deletes superseded row", func(t *testing.T) {
		database := openTestDB(t)
		require.NoError(t, database.UpsertSession(db.Session{
			ID: "stale", Project: "proj", Machine: "local", Agent: "claude",
		}))
		e := NewEngine(database, EngineConfig{
			Machine:            "local",
			IncludeCwdPrefixes: []string{"/allowed"},
		})

		results := make(chan syncJob, 1)
		results <- exclusionGateJob("/allowed/repo")
		close(results)
		stats := e.collectAndBatch(
			ctx, results, 1, 1, nil, syncWriteDefault,
		)

		gotStale, err := database.GetSession(ctx, "stale")
		require.NoError(t, err)
		assert.Nil(t, gotStale,
			"superseded row must be deleted for an allowed source")
		gotNew, err := database.GetSession(ctx, "replacement")
		require.NoError(t, err)
		assert.NotNil(t, gotNew, "allowed replacement must be written")
		assert.Equal(t, []string{"stale"}, stats.parserExcludedIDs)
		assert.Equal(t, 0, stats.cwdFilteredSessions, "filtered sessions")
		assert.Equal(t, 1, stats.Synced, "synced")
	})
}

func TestCollectAndBatchKeepsAllowedSourceSiblingCurrent(t *testing.T) {
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		Machine:            "local",
		IncludeCwdPrefixes: []string{"/allowed"},
	})
	results := make(chan syncJob, 1)
	results <- syncJob{
		agent: parser.AgentCowork,
		path:  "/src/shared.jsonl",
		processResult: processResult{results: []parser.ParseResult{
			{Session: parser.ParsedSession{
				ID: "cowork:allowed", Agent: parser.AgentCowork,
				Machine: "local", Project: "proj", Cwd: "/allowed/repo",
				File: parser.FileInfo{Path: "/src/shared.jsonl"},
			}},
			{Session: parser.ParsedSession{
				ID: "cowork:filtered", Agent: parser.AgentCowork,
				Machine: "local", Project: "proj", Cwd: "/outside/repo",
				File: parser.FileInfo{Path: "/src/shared.jsonl"},
			}},
		}},
	}
	close(results)

	stats := engine.collectAndBatch(
		context.Background(), results, 1, 1, nil, syncWriteDefault,
	)

	assert.Equal(t, 1, stats.Synced)
	assert.Equal(t, 1, stats.cwdFilteredSessions)
	allowed, err := database.GetSession(context.Background(), "cowork:allowed")
	require.NoError(t, err)
	require.NotNil(t, allowed)
	assert.Equal(t, db.CurrentDataVersion(), allowed.DataVersion,
		"a sibling's cwd veto must not leave the allowed session stale")
	filtered, err := database.GetSession(context.Background(), "cowork:filtered")
	require.NoError(t, err)
	assert.Nil(t, filtered)
}

func TestCollectAndBatchBaselinesAllowedMissingMemberForMixedCwdSource(
	t *testing.T,
) {
	ctx := context.Background()
	database := openTestDB(t)
	path := "/src/mixed.jsonl"
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "stale-allowed", Project: "proj", Machine: "local", Agent: "claude",
		Cwd: "/allowed/stale", FilePath: &path,
	}))
	engine := NewEngine(database, EngineConfig{
		Machine:            "local",
		IncludeCwdPrefixes: []string{"/allowed"},
	})

	syncSource := func() SyncStats {
		results := make(chan syncJob, 1)
		results <- syncJob{
			agent:   parser.AgentClaude,
			path:    path,
			machine: "local",
			processResult: processResult{
				results: []parser.ParseResult{
					{Session: parser.ParsedSession{
						ID: "replacement-allowed", Agent: parser.AgentClaude,
						Machine: "local", Project: "proj", Cwd: "/allowed/new",
						File: parser.FileInfo{Path: path},
					}},
					{Session: parser.ParsedSession{
						ID: "replacement-filtered", Agent: parser.AgentClaude,
						Machine: "local", Project: "proj", Cwd: "/outside/new",
						File: parser.FileInfo{Path: path},
					}},
				},
				sourceMissingMembers: []sourceMissingMember{{
					sessionID: "stale-allowed",
					filePath:  path,
					machine:   "local",
				}},
			},
		}
		close(results)
		return engine.collectAndBatch(
			ctx, results, 1, 1, nil, syncWriteDefault,
		)
	}

	first := syncSource()
	require.Zero(t, first.Failed)
	var baselineCount int
	require.NoError(t, database.Reader().QueryRow(`
		SELECT count(*) FROM local_session_source_baselines
		WHERE session_id = 'stale-allowed'`,
	).Scan(&baselineCount))
	assert.Equal(t, 1, baselineCount,
		"an allowed missing member needs exact proof when a sibling is filtered")

	second := syncSource()
	require.Zero(t, second.Failed)
	stale, err := database.GetSessionFull(ctx, "stale-allowed")
	require.NoError(t, err)
	require.NotNil(t, stale)
	require.NotNil(t, stale.DeletionCause,
		"the next mixed-CWD parse must retire the admitted missing member")
	assert.Equal(t, "source_missing", *stale.DeletionCause)
}

func TestCollectAndBatchCancellationRevokesRejectedMissingMemberBaseline(
	t *testing.T,
) {
	database := openTestDB(t)
	path := "/src/cancelled-mixed.jsonl"
	parentID := "cancelled-mixed"
	allowedID := parentID + "-11111111-2222-4333-8444-555555555555"
	rejectedID := parentID + "-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	for id, cwd := range map[string]string{
		allowedID:  "/workspace/work/project",
		rejectedID: "/workspace/personal/project",
	} {
		require.NoError(t, database.UpsertSession(db.Session{
			ID: id, Project: "project", Machine: "local", Agent: "claude",
			Cwd: cwd, ParentSessionID: &parentID, RelationshipType: "fork",
			FilePath: &path,
		}))
		require.NoError(t, database.SetSessionDataVersion(id, 0))
	}
	require.NoError(t, database.BaselineActiveSessionSourcePaths(
		t.Context(), "local",
		[]db.SessionSourcePath{{Agent: "claude", FilePath: path}},
	))

	engine := NewEngine(database, EngineConfig{
		Machine:            "local",
		IncludeCwdPrefixes: []string{"/workspace/work"},
	})
	t.Cleanup(engine.Close)
	ctx, cancel := context.WithCancel(t.Context())
	results := make(chan syncJob, 2)
	results <- syncJob{
		agent: parser.AgentClaude, path: path, machine: "local",
		processResult: processResult{sourceMissingMembers: []sourceMissingMember{
			{sessionID: allowedID, machine: "local", filePath: path},
			{sessionID: rejectedID, machine: "local", filePath: path},
		}},
	}
	results <- syncJob{processResult: processResult{err: context.Canceled}}
	close(results)

	stats := engine.collectAndBatch(
		ctx, results, 2, 2,
		func(progress Progress) {
			if progress.SessionsDone == 1 {
				cancel()
			}
		},
		syncWriteDefault,
	)

	assert.True(t, stats.Aborted)
	var rejectedBaseline int
	require.NoError(t, database.Reader().QueryRow(`
		SELECT count(*) FROM local_session_source_baselines
		WHERE session_id = ?`, rejectedID,
	).Scan(&rejectedBaseline))
	assert.Zero(t, rejectedBaseline,
		"cancellation must not leave deletion proof on a CWD-rejected member")
	rejected, err := database.GetSession(t.Context(), rejectedID)
	require.NoError(t, err)
	assert.NotNil(t, rejected,
		"the CWD-rejected stale fork must remain active")
}

func TestCollectAndBatchFailureRevokesOnlyRejectedMissingMemberBaseline(
	t *testing.T,
) {
	database := openTestDB(t)
	path := "/src/failed-mixed.jsonl"
	parentID := "failed-mixed"
	failingID := parentID + "-11111111-2222-4333-8444-555555555555"
	rejectedID := parentID + "-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	for id, cwd := range map[string]string{
		failingID:  "/workspace/work/project",
		rejectedID: "/workspace/personal/project",
	} {
		require.NoError(t, database.UpsertSession(db.Session{
			ID: id, Project: "project", Machine: "local", Agent: "claude",
			Cwd: cwd, ParentSessionID: &parentID, RelationshipType: "fork",
			FilePath: &path,
		}))
		require.NoError(t, database.SetSessionDataVersion(id, 0))
	}
	require.NoError(t, database.BaselineActiveSessionSourceOwnerships(
		t.Context(), []db.SessionSourceOwnership{
			{ID: failingID, Machine: "local", Agent: "claude", FilePath: path},
			{ID: rejectedID, Machine: "local", Agent: "claude", FilePath: path},
		},
	))
	require.NoError(t, database.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			CREATE TRIGGER fail_first_source_missing_tombstone
			BEFORE UPDATE OF deleted_at ON sessions
			WHEN NEW.id = 'failed-mixed-11111111-2222-4333-8444-555555555555'
			 AND NEW.deletion_cause = 'source_missing'
			BEGIN
				SELECT RAISE(FAIL, 'injected first-member tombstone failure');
			END`)
		return err
	}))

	engine := NewEngine(database, EngineConfig{
		Machine:            "local",
		IncludeCwdPrefixes: []string{"/workspace/work"},
	})
	t.Cleanup(engine.Close)
	results := make(chan syncJob, 1)
	results <- syncJob{
		agent: parser.AgentClaude, path: path, machine: "local",
		processResult: processResult{sourceMissingMembers: []sourceMissingMember{
			{sessionID: failingID, machine: "local", filePath: path},
			{sessionID: rejectedID, machine: "local", filePath: path},
		}},
	}
	close(results)

	stats := engine.collectAndBatch(
		t.Context(), results, 1, 1, nil, syncWriteDefault,
	)

	assert.Equal(t, 1, stats.Failed)
	var failingBaseline, rejectedBaseline int
	require.NoError(t, database.Reader().QueryRow(`
		SELECT count(*) FROM local_session_source_baselines
		WHERE session_id = ?`, failingID,
	).Scan(&failingBaseline))
	require.NoError(t, database.Reader().QueryRow(`
		SELECT count(*) FROM local_session_source_baselines
		WHERE session_id = ?`, rejectedID,
	).Scan(&rejectedBaseline))
	assert.Equal(t, 1, failingBaseline,
		"exact cleanup must preserve proof for the admitted member whose write failed")
	assert.Zero(t, rejectedBaseline,
		"an earlier write failure must not preserve proof for a later CWD rejection")
}

func seedPartialSourceMissingFailure(
	t *testing.T, database *db.DB, parentID, path string,
) (string, string) {
	t.Helper()
	firstID := "partial-success-a"
	failingID := "partial-success-b"
	for _, id := range []string{firstID, failingID} {
		require.NoError(t, database.UpsertSession(db.Session{
			ID: id, Project: "project", Machine: "local", Agent: "claude",
			ParentSessionID: &parentID, RelationshipType: "fork", FilePath: &path,
		}))
		require.NoError(t, database.SetSessionDataVersion(id, 0))
	}
	require.NoError(t, database.BaselineActiveSessionSourceOwnerships(
		t.Context(), []db.SessionSourceOwnership{
			{ID: firstID, Machine: "local", Agent: "claude", FilePath: path},
			{ID: failingID, Machine: "local", Agent: "claude", FilePath: path},
		},
	))
	require.NoError(t, database.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			CREATE TRIGGER fail_second_source_missing_tombstone
			BEFORE UPDATE OF deleted_at ON sessions
			WHEN NEW.id = 'partial-success-b'
			 AND NEW.deletion_cause = 'source_missing'
			BEGIN
				SELECT RAISE(FAIL, 'injected later-member tombstone failure');
			END`)
		return err
	}))
	return firstID, failingID
}

func TestSyncAllCountsAndEmitsPartialSourceMissingTombstones(t *testing.T) {
	fx := newEngineFixture(t)
	emitter := &fakeEmitter{}
	fx.engineWithEmitter(emitter)
	path := fx.writeClaudeSession(t, "project", "partial-batch.jsonl", "first")
	require.Equal(t, 1, fx.engine.SyncAll(t.Context(), nil).Synced)
	emitter.mu.Lock()
	emitter.scopes = nil
	emitter.mu.Unlock()

	firstID, failingID := seedPartialSourceMissingFailure(
		t, fx.db, fx.sessionIDFor(t, path), path,
	)
	fx.appendClaudeMessage(t, path, "changed")
	stats := fx.engine.SyncAll(t.Context(), nil)

	assert.Equal(t, 1, stats.Failed)
	assert.Equal(t, 1, stats.Tombstoned,
		"a committed tombstone must remain visible in failed-pass statistics")
	assert.Equal(t, []string{"sessions"}, emitter.got(),
		"a failed pass must notify clients about its committed tombstone")
	first, err := fx.db.GetSessionFull(t.Context(), firstID)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.NotNil(t, first.DeletionCause)
	assert.Equal(t, "source_missing", *first.DeletionCause)
	failing, err := fx.db.GetSession(t.Context(), failingID)
	require.NoError(t, err)
	assert.NotNil(t, failing, "the member that failed to tombstone must remain active")
}

func TestSyncThenRunEmitsPartialSourceMissingTombstones(t *testing.T) {
	fx := newEngineFixture(t)
	emitter := &fakeEmitter{}
	fx.engineWithEmitter(emitter)
	path := fx.writeClaudeSession(t, "project", "partial-coordinated.jsonl", "first")
	require.Equal(t, 1, fx.engine.SyncAll(t.Context(), nil).Synced)
	emitter.mu.Lock()
	emitter.scopes = nil
	emitter.mu.Unlock()

	firstID, failingID := seedPartialSourceMissingFailure(
		t, fx.db, fx.sessionIDFor(t, path), path,
	)
	fx.appendClaudeMessage(t, path, "changed")
	stats, err := fx.engine.SyncThenRun(
		t.Context(), false, nil, func(bool) error { return nil },
	)

	require.NoError(t, err)
	assert.Equal(t, 1, stats.Failed)
	assert.Equal(t, 1, stats.Tombstoned,
		"a committed tombstone must survive coordinated completion")
	assert.Equal(t, []string{"sync"}, emitter.got(),
		"coordinated completion must notify clients about its committed tombstone")
	first, err := fx.db.GetSessionFull(t.Context(), firstID)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.NotNil(t, first.DeletionCause)
	assert.Equal(t, "source_missing", *first.DeletionCause)
	failing, err := fx.db.GetSession(t.Context(), failingID)
	require.NoError(t, err)
	assert.NotNil(t, failing, "the member that failed to tombstone must remain active")
}

func TestSyncSingleSessionEmitsPartialSourceMissingTombstones(t *testing.T) {
	fx := newEngineFixture(t)
	emitter := &fakeEmitter{}
	fx.engineWithEmitter(emitter)
	path := fx.writeClaudeSession(t, "project", "partial-single.jsonl", "first")
	require.Equal(t, 1, fx.engine.SyncAll(t.Context(), nil).Synced)
	emitter.mu.Lock()
	emitter.scopes = nil
	emitter.mu.Unlock()

	firstID, failingID := seedPartialSourceMissingFailure(
		t, fx.db, fx.sessionIDFor(t, path), path,
	)
	fx.appendClaudeMessage(t, path, "changed")
	err := fx.engine.SyncSingleSession(fx.sessionIDFor(t, path))

	require.ErrorContains(t, err, "injected later-member tombstone failure")
	assert.Equal(t, []string{"sessions"}, emitter.got(),
		"a failed single-session sync must notify clients about its committed tombstone")
	first, err := fx.db.GetSessionFull(t.Context(), firstID)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.NotNil(t, first.DeletionCause)
	assert.Equal(t, "source_missing", *first.DeletionCause)
	failing, err := fx.db.GetSession(t.Context(), failingID)
	require.NoError(t, err)
	assert.NotNil(t, failing, "the member that failed to tombstone must remain active")
}

func TestSyncSingleSessionRevokesRejectedBaselineOnLaterMemberFailure(
	t *testing.T,
) {
	fx := newEngineFixture(t)
	path := fx.writeClaudeSession(t, "project", "partial-filtered.jsonl", "first")
	require.Equal(t, 1, fx.engine.SyncAll(t.Context(), nil).Synced)

	rejectedID, failingID := seedPartialSourceMissingFailure(
		t, fx.db, fx.sessionIDFor(t, path), path,
	)
	require.NoError(t, fx.db.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			UPDATE sessions
			SET cwd = CASE id
				WHEN ? THEN '/outside/project'
				WHEN ? THEN '/workspace/allowed/project'
				ELSE cwd
			END
			WHERE id IN (?, ?)`,
			rejectedID, failingID, rejectedID, failingID,
		)
		return err
	}))
	fx.engine.Close()
	fx.engine = NewEngine(fx.db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {fx.claudeDir},
		},
		Machine:            "local",
		IncludeCwdPrefixes: []string{"/workspace/allowed"},
	})
	t.Cleanup(fx.engine.Close)

	fx.appendClaudeMessage(t, path, "changed")
	err := fx.engine.SyncSingleSession(fx.sessionIDFor(t, path))

	require.ErrorContains(t, err, "injected later-member tombstone failure")
	var rejectedBaseline int
	require.NoError(t, fx.db.Reader().QueryRow(`
		SELECT count(*) FROM local_session_source_baselines
		WHERE session_id = ?`, rejectedID,
	).Scan(&rejectedBaseline))
	assert.Zero(t, rejectedBaseline,
		"a later member failure must not retain deletion proof for a CWD-rejected fork")
	var primaryBaseline int
	require.NoError(t, fx.db.Reader().QueryRow(`
		SELECT count(*) FROM local_session_source_baselines
		WHERE session_id = ?`, fx.sessionIDFor(t, path),
	).Scan(&primaryBaseline))
	assert.Equal(t, 1, primaryBaseline,
		"exact exception cleanup must preserve unrelated source ownership proof")
	rejected, err := fx.db.GetSession(t.Context(), rejectedID)
	require.NoError(t, err)
	assert.NotNil(t, rejected,
		"the CWD-rejected stale fork must remain active")
}

func TestReconcileWatchRootsRevokesRejectedBaselineOnPageFailure(
	t *testing.T,
) {
	fx := newEngineFixture(t)
	path := fx.writeClaudeSession(t, "project", "reconcile-filtered.jsonl", "first")
	filteredPath := fx.writeClaudeSession(
		t, "project", "reconcile-source-filtered.jsonl", "first",
	)
	require.Equal(t, 2, fx.engine.SyncAll(t.Context(), nil).Synced)
	primaryID := fx.sessionIDFor(t, path)
	filteredID := fx.sessionIDFor(t, filteredPath)

	rejectedID, failingID := seedPartialSourceMissingFailure(
		t, fx.db, primaryID, path,
	)
	require.NoError(t, fx.db.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			UPDATE sessions
			SET cwd = CASE id
				WHEN ? THEN '/outside/project'
				WHEN ? THEN '/workspace/allowed/project'
				ELSE cwd
			END
			WHERE id IN (?, ?)`,
			rejectedID, failingID, rejectedID, failingID,
		)
		return err
	}))
	require.NoError(t, fx.db.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			"UPDATE sessions SET cwd = '/outside/project' WHERE id = ?",
			filteredID,
		)
		return err
	}))
	require.NoError(t, fx.db.RemoveSessionSourceOwnershipBaselines(
		t.Context(), []db.SessionSourceOwnership{{
			ID: primaryID, Machine: "local", Agent: "claude", FilePath: path,
		}},
	))
	fx.engine.Close()
	fx.engine = NewEngine(fx.db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {fx.claudeDir},
		},
		Machine:            "local",
		IncludeCwdPrefixes: []string{"/workspace/allowed"},
	})
	t.Cleanup(fx.engine.Close)

	fx.appendClaudeMessage(t, path, "changed")
	err := fx.engine.ReconcileWatchRoots(
		t.Context(), []string{fx.claudeDir}, false,
	)

	require.ErrorContains(t, err, "failed processing page: 1 failures")
	var rejectedBaseline int
	require.NoError(t, fx.db.Reader().QueryRow(`
		SELECT count(*) FROM local_session_source_baselines
		WHERE session_id = ?`, rejectedID,
	).Scan(&rejectedBaseline))
	assert.Zero(t, rejectedBaseline,
		"a failed page must revoke proof from a CWD-rejected fork")
	var primaryBaseline int
	require.NoError(t, fx.db.Reader().QueryRow(`
		SELECT count(*) FROM local_session_source_baselines
		WHERE session_id = ?`, primaryID,
	).Scan(&primaryBaseline))
	assert.Zero(t, primaryBaseline,
		"failed-page cleanup must not grant new source proof")
	var filteredBaseline int
	require.NoError(t, fx.db.Reader().QueryRow(`
		SELECT count(*) FROM local_session_source_baselines
		WHERE session_id = ?`, filteredID,
	).Scan(&filteredBaseline))
	assert.Zero(t, filteredBaseline,
		"a mixed page failure must revoke source-wide proof rejected by CWD")
	rejected, err := fx.db.GetSession(t.Context(), rejectedID)
	require.NoError(t, err)
	assert.NotNil(t, rejected,
		"the CWD-rejected stale fork must remain active")

	require.NoError(t, fx.db.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec("DROP TRIGGER fail_second_source_missing_tombstone")
		return err
	}))
	require.NoError(t, os.Remove(filteredPath))
	require.NoError(t, fx.engine.ReconcileWatchRoots(
		t.Context(), []string{fx.claudeDir}, false,
	))
	preserved, err := fx.db.GetSession(t.Context(), filteredID)
	require.NoError(t, err)
	assert.NotNil(t, preserved,
		"a source removed before retry must not tombstone its filtered session")
}

func TestReconcileWatchRootsRevokesSourceWideRejectedBaselineOnFinalizationFailure(
	t *testing.T,
) {
	fx := newEngineFixture(t)
	path := fx.writeClaudeSession(t, "project", "finalize-filtered.jsonl", "first")
	require.Equal(t, 1, fx.engine.SyncAll(t.Context(), nil).Synced)
	primaryID := fx.sessionIDFor(t, path)
	require.NoError(t, fx.db.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			UPDATE sessions
			SET cwd = '/outside/project', machine = 'legacy-machine'
			WHERE id = ?`,
			primaryID,
		)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`
			UPDATE local_session_source_baselines
			SET machine = 'legacy-machine'
			WHERE session_id = ?`,
			primaryID,
		)
		return err
	}))
	fx.engine.Close()
	fx.engine = NewEngine(fx.db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {fx.claudeDir},
		},
		Machine:            "local",
		IncludeCwdPrefixes: []string{"/workspace/allowed"},
	})
	t.Cleanup(fx.engine.Close)
	lookupErr := errors.New("injected baseline attribution failure")
	lookupCalls := 0
	fx.engine.sourceAttributionLookupOverride = func(
		_ context.Context, requested []db.SessionSourcePath,
	) ([]db.SessionSourceAttribution, error) {
		lookupCalls++
		assert.Equal(t, []db.SessionSourcePath{{
			Agent: "claude", FilePath: path,
		}}, requested)
		return nil, lookupErr
	}

	err := fx.engine.ReconcileWatchRoots(
		t.Context(), []string{fx.claudeDir}, false,
	)

	require.ErrorIs(t, err, lookupErr)
	assert.NotZero(t, lookupCalls)
	var primaryBaseline int
	require.NoError(t, fx.db.Reader().QueryRow(`
		SELECT count(*) FROM local_session_source_baselines
		WHERE session_id = ?`, primaryID,
	).Scan(&primaryBaseline))
	assert.Zero(t, primaryBaseline,
		"failed finalization must revoke source-wide proof rejected by CWD")

	fx.engine.sourceAttributionLookupOverride = nil
	require.NoError(t, os.Remove(path))
	require.NoError(t, fx.engine.ReconcileWatchRoots(
		t.Context(), []string{fx.claudeDir}, false,
	))
	preserved, err := fx.db.GetSession(t.Context(), primaryID)
	require.NoError(t, err)
	assert.NotNil(t, preserved,
		"a source removed before retry must not tombstone its filtered session")
}

func TestReconcileWatchRootsRevokesRejectedBaselineOnSpoolFailure(
	t *testing.T,
) {
	for _, tc := range []struct {
		name      string
		failWrite bool
	}{
		{name: "marker write", failWrite: true},
		{name: "marker query"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newEngineFixture(t)
			path := fx.writeClaudeSession(t, "project", "spool-filtered.jsonl", "first")
			require.Equal(t, 1, fx.engine.SyncAll(t.Context(), nil).Synced)
			primaryID := fx.sessionIDFor(t, path)
			rejectedID := primaryID + "-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			require.NoError(t, fx.db.UpsertSession(db.Session{
				ID: rejectedID, Project: "project", Machine: "local", Agent: "claude",
				Cwd: "/outside/project", ParentSessionID: &primaryID,
				RelationshipType: "fork", FilePath: &path,
			}))
			require.NoError(t, fx.db.SetSessionDataVersion(rejectedID, 0))
			require.NoError(t, fx.db.BaselineActiveSessionSourceOwnerships(
				t.Context(), []db.SessionSourceOwnership{{
					ID: rejectedID, Machine: "local", Agent: "claude", FilePath: path,
				}},
			))
			fx.engine.Close()
			fx.engine = NewEngine(fx.db, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentClaude: {fx.claudeDir},
				},
				Machine:            "local",
				IncludeCwdPrefixes: []string{"/workspace/allowed"},
			})
			t.Cleanup(fx.engine.Close)
			injected := errors.New("injected non-authoritative spool failure")
			fx.engine.reconciliationSpoolFactory = func(
				path string,
			) (reconciliationSpoolStore, error) {
				spool, err := newReconciliationSpool(path)
				if err != nil {
					return nil, err
				}
				return &nonAuthoritativeFailureSpool{
					reconciliationSpoolStore: spool,
					err:                      injected,
					failWrite:                tc.failWrite,
				}, nil
			}

			fx.appendClaudeMessage(t, path, "changed")
			err := fx.engine.ReconcileWatchRoots(
				t.Context(), []string{fx.claudeDir}, false,
			)

			require.ErrorIs(t, err, injected)
			var rejectedBaseline int
			require.NoError(t, fx.db.Reader().QueryRow(`
				SELECT count(*) FROM local_session_source_baselines
				WHERE session_id = ?`, rejectedID,
			).Scan(&rejectedBaseline))
			assert.Zero(t, rejectedBaseline,
				"a spool failure must revoke proof from a CWD-rejected fork")
			rejected, getErr := fx.db.GetSession(t.Context(), rejectedID)
			require.NoError(t, getErr)
			assert.NotNil(t, rejected,
				"the CWD-rejected stale fork must remain active")
		})
	}
}

func TestReconcileWatchRootsRevokesSourceWideRejectedBaselineOnSpoolFailure(
	t *testing.T,
) {
	for _, tc := range []struct {
		name      string
		failWrite bool
	}{
		{name: "marker write", failWrite: true},
		{name: "marker query"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newEngineFixture(t)
			path := fx.writeClaudeSession(
				t, "project", "spool-source-filtered.jsonl", "first",
			)
			require.Equal(t, 1, fx.engine.SyncAll(t.Context(), nil).Synced)
			sessionID := fx.sessionIDFor(t, path)
			require.NoError(t, fx.db.Update(func(tx *sql.Tx) error {
				_, err := tx.Exec(
					"UPDATE sessions SET cwd = '/outside/project' WHERE id = ?",
					sessionID,
				)
				return err
			}))
			fx.engine.Close()
			fx.engine = NewEngine(fx.db, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentClaude: {fx.claudeDir},
				},
				Machine:            "local",
				IncludeCwdPrefixes: []string{"/workspace/allowed"},
			})
			t.Cleanup(fx.engine.Close)
			defaultFactory := fx.engine.reconciliationSpoolFactory
			injected := errors.New("injected non-authoritative spool failure")
			fx.engine.reconciliationSpoolFactory = func(
				path string,
			) (reconciliationSpoolStore, error) {
				spool, err := defaultFactory(path)
				if err != nil {
					return nil, err
				}
				return &nonAuthoritativeFailureSpool{
					reconciliationSpoolStore: spool,
					err:                      injected,
					failWrite:                tc.failWrite,
				}, nil
			}

			err := fx.engine.ReconcileWatchRoots(
				t.Context(), []string{fx.claudeDir}, false,
			)
			require.ErrorIs(t, err, injected)
			var baselineCount int
			require.NoError(t, fx.db.Reader().QueryRow(`
				SELECT count(*) FROM local_session_source_baselines
				WHERE session_id = ?`, sessionID,
			).Scan(&baselineCount))
			assert.Zero(t, baselineCount,
				"a spool failure must revoke source-wide proof rejected by CWD")

			fx.engine.reconciliationSpoolFactory = defaultFactory
			require.NoError(t, os.Remove(path))
			require.NoError(t, fx.engine.ReconcileWatchRoots(
				t.Context(), []string{fx.claudeDir}, false,
			))
			preserved, getErr := fx.db.GetSession(t.Context(), sessionID)
			require.NoError(t, getErr)
			assert.NotNil(t, preserved,
				"a source removed before retry must not tombstone its filtered session")
		})
	}
}

type nonAuthoritativeFailureSpool struct {
	reconciliationSpoolStore
	err       error
	failWrite bool
}

func (spool *nonAuthoritativeFailureSpool) AddNonAuthoritativeScopes(
	ctx context.Context, scopes []reconciliationSourceScope,
) error {
	if spool.failWrite {
		return spool.err
	}
	return spool.reconciliationSpoolStore.AddNonAuthoritativeScopes(ctx, scopes)
}

func (spool *nonAuthoritativeFailureSpool) HasNonAuthoritativeScopes(
	ctx context.Context, agent parser.AgentType,
) (bool, error) {
	if !spool.failWrite {
		return false, spool.err
	}
	return spool.reconciliationSpoolStore.HasNonAuthoritativeScopes(ctx, agent)
}

func TestShouldAbortResyncSwap(t *testing.T) {
	tests := []struct {
		name            string
		stats           SyncStats
		oldFileSessions int
		trashedCopied   int
		want            bool
	}{
		{
			name: "clean run proceeds",
			stats: SyncStats{
				TotalSessions: 5, Synced: 5,
				filesOK: 5, nonContainerDiscovered: 5,
			},
			oldFileSessions: 5,
		},
		{
			name:            "cancelled run aborts",
			stats:           SyncStats{Aborted: true, Synced: 5},
			oldFileSessions: 5,
			want:            true,
		},
		{
			name:            "empty discovery with old data aborts",
			stats:           SyncStats{},
			oldFileSessions: 3,
			want:            true,
		},
		{
			name: "zero writes unexplained aborts",
			stats: SyncStats{
				TotalSessions: 3, nonContainerDiscovered: 3,
			},
			oldFileSessions: 3,
			want:            true,
		},
		{
			name: "more failures than successes aborts",
			stats: SyncStats{
				TotalSessions: 6, Synced: 1, Failed: 5,
				filesOK: 1, nonContainerDiscovered: 6,
			},
			oldFileSessions: 6,
			want:            true,
		},
		{
			name: "parser-excluded-only run proceeds",
			stats: SyncStats{
				TotalSessions: 3, filesOK: 3,
				parserExcludedFiles: 3, nonContainerDiscovered: 3,
			},
			oldFileSessions: 3,
		},
		{
			name: "all-cwd-filtered run proceeds",
			stats: SyncStats{
				TotalSessions: 2, filesOK: 2,
				cwdFilteredFiles: 2, cwdFilteredSessions: 2,
				nonContainerDiscovered: 2,
			},
			oldFileSessions: 2,
		},
		{
			name: "cwd-filtered mixed with parser-excluded proceeds",
			stats: SyncStats{
				TotalSessions: 4, filesOK: 4,
				cwdFilteredFiles: 2, cwdFilteredSessions: 3,
				parserExcludedFiles: 2, nonContainerDiscovered: 4,
			},
			oldFileSessions: 4,
		},
		{
			name: "cwd-filtered with unaccounted OK file aborts",
			stats: SyncStats{
				TotalSessions: 3, filesOK: 2,
				cwdFilteredFiles: 1, cwdFilteredSessions: 1,
				nonContainerDiscovered: 3,
			},
			oldFileSessions: 3,
			want:            true,
		},
		{
			name: "cwd-filtered with failures aborts",
			stats: SyncStats{
				TotalSessions: 2, Failed: 1, filesOK: 1,
				cwdFilteredFiles: 1, cwdFilteredSessions: 1,
				nonContainerDiscovered: 2,
			},
			oldFileSessions: 2,
			want:            true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldAbortResyncSwap(
				tt.stats, tt.oldFileSessions, tt.trashedCopied,
			)
			assert.Equal(t, tt.want, got)
		})
	}
}
