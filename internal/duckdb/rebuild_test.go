//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func seedRebuildFixture(t *testing.T, local *db.DB) []string {
	t.Helper()
	ids := []string{"rebuild-a", "rebuild-b", "rebuild-c"}
	writes := make([]db.SessionBatchWrite, 0, len(ids))
	for i, id := range ids {
		ts := "2026-01-1" + string(rune('0'+i)) + "T00:00:00.000Z"
		writes = append(writes, db.SessionBatchWrite{
			Session: syncSession(id, "alpha", id+" first", ts, 1),
			Messages: []db.Message{
				syncMessage(id, 0, "user", id+" first", ts),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		})
	}
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(t, err)
	ok, err := local.StarSession(ids[0])
	require.NoError(t, err)
	require.True(t, ok)
	return ids
}

func TestRebuildMirrorCreatesFreshMirrorWithFingerprintsAndMetadata(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	ids := seedRebuildFixture(t, local)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")

	result, err := rebuildMirror(ctx, path, local, "test-machine", SyncOptions{}, nil)
	require.NoError(t, err)

	assert.Equal(t, len(ids), result.SessionsPushed)
	assert.Equal(t, len(ids), result.MessagesPushed)
	assert.Equal(t, 0, result.Errors)
	assert.True(t, result.Diagnostics.Full)
	assert.FileExists(t, path)

	probe, err := ProbeMirror(ctx, path)
	require.NoError(t, err)
	assert.True(t, probe.FileExists)
	assert.True(t, probe.ShapeOK)
	assert.Equal(t, SchemaVersion, probe.SchemaVersion)
	assert.Equal(t, db.CurrentDataVersion(), probe.DataVersion)
	assert.Equal(t, "", probe.Scope)
	assert.NotEmpty(t, probe.LastPushCutoff)
	assert.NotEmpty(t, probe.LastPushAt)

	conn, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	assertDuckDBCount(t, conn, "sessions", len(ids))
	assertDuckDBCount(t, conn, "messages", len(ids))
	assertDuckDBCount(t, conn, "starred_sessions", 1)

	var fingerprintCount int
	require.NoError(t, conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sessions
		WHERE agentsview_push_fingerprint IS NOT NULL
		  AND agentsview_push_fingerprint != ''`,
	).Scan(&fingerprintCount))
	assert.Equal(t, len(ids), fingerprintCount,
		"every rebuilt session row must carry a push fingerprint")
	assert.Greater(t, result.Duration, time.Duration(0))
}

// TestPushEverythingDoesNotSetDuration is the FIX9 regression: Duration is
// owned by buildMirrorInto (see its doc comment), not pushEverything.
// Identity publication and the mirror metadata write both happen after
// pushEverything returns, so a Duration captured inside pushEverything
// alone would underreport a --full push's real wall time by everything
// after the session push loop. pushEverything itself no longer sets
// Duration at all, leaving it to whichever caller actually spans the full
// operation.
func TestPushEverythingDoesNotSetDuration(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedRebuildFixture(t, local)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")

	s := newTestSync(t, path, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, s.DB()))

	result, err := s.pushEverything(ctx, nil)
	require.NoError(t, err)
	assert.Zero(t, result.Duration,
		"pushEverything must leave Duration to its caller, which spans more than the session push loop")
}

func TestRebuildMirrorReplacesPreExistingTargetFileContent(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")

	stale := newTestSync(t, path, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, stale.DB()))
	_, err := stale.DB().ExecContext(ctx, `
		INSERT INTO sessions (id, project, machine, agent, created_at)
		VALUES ('stale-session', 'alpha', 'test-machine', 'claude', current_timestamp)`)
	require.NoError(t, err)
	require.NoError(t, stale.Close())

	seedRebuildFixture(t, local)

	result, err := rebuildMirror(ctx, path, local, "test-machine", SyncOptions{}, nil)
	require.NoError(t, err)
	assert.Equal(t, 3, result.SessionsPushed)

	conn, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	assertDuckDBCountWhere(t, conn, "sessions", "id = ?", "stale-session", 0)
	assertDuckDBCount(t, conn, "sessions", 3)
}

func TestRebuildMirrorLeavesNoTempFilesOnSwapFailure(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedRebuildFixture(t, local)
	dir := t.TempDir()
	// A directory in place of the destination file makes the final
	// os.Rename fail deterministically (EISDIR/ENOTDIR) on every platform,
	// simulating a swap failure (e.g. Windows sharing violation) without
	// needing to inject a fake rename.
	path := filepath.Join(dir, "mirror-as-dir.duckdb")
	require.NoError(t, os.Mkdir(path, 0o755))

	_, err := rebuildMirror(ctx, path, local, "test-machine", SyncOptions{}, nil)

	require.Error(t, err)
	assert.DirExists(t, path, "swap failure must leave the destination untouched")
	for _, checkDir := range []string{dir, mirrorWorkDirPath(path)} {
		entries, err := os.ReadDir(checkDir)
		require.NoError(t, err)
		for _, entry := range entries {
			assert.NotContains(t, entry.Name(), ".tmp-",
				"failed rebuild must not leave a temp mirror file behind in %s", checkDir)
		}
	}
}

func TestSwapMirrorFileRetriesThenFailsWithActionableError(t *testing.T) {
	dir := t.TempDir()
	tmpPath := filepath.Join(dir, "source.duckdb")
	require.NoError(t, os.WriteFile(tmpPath, []byte("mirror bytes"), 0o644))
	dstPath := filepath.Join(dir, "dst-is-a-dir")
	require.NoError(t, os.Mkdir(dstPath, 0o755))

	err := swapMirrorFile(tmpPath, dstPath)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "agentsview duckdb serve")
	assert.FileExists(t, tmpPath, "source file must survive a failed swap")
	content, readErr := os.ReadFile(tmpPath)
	require.NoError(t, readErr)
	assert.Equal(t, "mirror bytes", string(content))
}

func TestRebuildMirrorScopesToProjectFilters(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session:         syncSession("rebuild-scope-alpha", "alpha", "a", "2026-01-10T00:00:00.000Z", 1),
			Messages:        []db.Message{syncMessage("rebuild-scope-alpha", 0, "user", "a", "2026-01-10T00:00:00.000Z")},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session:         syncSession("rebuild-scope-beta", "beta", "b", "2026-01-10T00:00:00.000Z", 1),
			Messages:        []db.Message{syncMessage("rebuild-scope-beta", 0, "user", "b", "2026-01-10T00:00:00.000Z")},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "scoped.duckdb")

	opts := SyncOptions{Projects: []string{"alpha"}}
	result, err := rebuildMirror(ctx, path, local, "test-machine", opts, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.SessionsPushed)

	probe, err := ProbeMirror(ctx, path)
	require.NoError(t, err)
	assert.Equal(t, canonicalPushScope(opts.Projects, opts.ExcludeProjects), probe.Scope)

	conn, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	assertDuckDBCountWhere(t, conn, "sessions", "id = ?", "rebuild-scope-alpha", 1)
	assertDuckDBCountWhere(t, conn, "sessions", "id = ?", "rebuild-scope-beta", 0)
}

func TestValidateBuiltMirrorRejectsBadMirrors(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name         string
		setupMirror  func(t *testing.T, path string) int // returns actual session count written
		wantSessions int
		expectError  bool
		errorPattern string
	}{
		{
			name: "session count mismatch",
			setupMirror: func(t *testing.T, path string) int {
				conn, err := Open(path)
				require.NoError(t, err)
				require.NoError(t, createSchema(ctx, conn))
				require.NoError(t, writeMirrorMetadata(ctx, conn, mirrorMetadata{
					SchemaVersion:  SchemaVersion,
					DataVersion:    1,
					Scope:          "",
					LastPushCutoff: "2026-07-18T00:00:00.000Z",
				}))
				// Insert 2 sessions
				_, err = conn.ExecContext(ctx, `
					INSERT INTO sessions (id, project, machine, agent, created_at)
					VALUES ('sess-1', 'alpha', 'test-machine', 'claude', current_timestamp),
					       ('sess-2', 'alpha', 'test-machine', 'claude', current_timestamp)`)
				require.NoError(t, err)
				require.NoError(t, conn.Close())
				return 2 // actual sessions in mirror
			},
			wantSessions: 3, // but we claim to want 3
			expectError:  true,
			errorPattern: "has 2 sessions, want 3",
		},
		{
			name: "missing metadata table",
			setupMirror: func(t *testing.T, path string) int {
				conn, err := Open(path)
				require.NoError(t, err)
				require.NoError(t, createSchema(ctx, conn))
				_, err = conn.ExecContext(ctx, `DROP TABLE sync_metadata`)
				require.NoError(t, err)
				require.NoError(t, conn.Close())
				return 0
			},
			wantSessions: 0,
			expectError:  true,
			errorPattern: "shape incompatible",
		},
		{
			name: "wrong schema version",
			setupMirror: func(t *testing.T, path string) int {
				conn, err := Open(path)
				require.NoError(t, err)
				require.NoError(t, createSchema(ctx, conn))
				require.NoError(t, writeMirrorMetadata(ctx, conn, mirrorMetadata{
					SchemaVersion:  2, // wrong version
					DataVersion:    1,
					Scope:          "",
					LastPushCutoff: "2026-07-18T00:00:00.000Z",
				}))
				require.NoError(t, conn.Close())
				return 0
			},
			wantSessions: 0,
			expectError:  true,
			errorPattern: "schema version 2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.duckdb")
			actSessions := tt.setupMirror(t, path)

			err := validateBuiltMirror(ctx, path, tt.wantSessions)

			if tt.expectError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errorPattern)
			} else {
				require.NoError(t, err)
			}

			// Verify file still exists after validation (read-only check)
			_, statErr := os.Stat(path)
			require.NoError(t, statErr, "mirror file must exist after validation")

			// For count mismatch case, verify file content unchanged
			if tt.name == "session count mismatch" {
				conn, err := Open(path)
				require.NoError(t, err)
				assertDuckDBCount(t, conn, "sessions", actSessions)
				require.NoError(t, conn.Close())
			}
		})
	}
}

// TestRebuildMirrorSnapshotsStateBeforeSessionEnumeration is a regression
// test for the rebuild-to-incremental handoff: the cutoff and deletion
// revision written to mirror metadata must reflect state as of BEFORE
// pushEverything enumerates sessions, not state re-read after the push loop
// finishes. A session mutated or hard-deleted while a real rebuild's push
// loop is still running produces a sync_marker (or deletion journal
// revision) that would already be <= a cutoff/revision captured at the end,
// so the very next incremental push would never select it: the mirror
// would silently keep stale or deleted data until the next --full rebuild.
//
// There is no hook into the middle of a running rebuild here, so the race
// is reproduced deterministically instead: capture the snapshot the fixed
// buildMirrorInto captures before pushEverything runs, apply mutations that
// stand in for changes racing an in-flight rebuild, then finish the
// "rebuild" (pushEverything + syncProjectIdentityObservations +
// writeRebuildMetadata) using that pre-mutation snapshot, exactly as
// production code now does. A further content-only edit that never moves
// the mutated session's sync_marker forward again is applied after the
// rebuild "completes", then a real incremental Push must still catch it.
//
// Under the old end-of-rebuild capture semantics, writeRebuildMetadata
// would have re-read the cutoff and deletion revision after pushEverything
// ran, i.e. after both mutations below. The appended session's sync_marker
// would then sit strictly before that late-captured cutoff, permanently
// excluding it from every future incremental window regardless of later
// content changes, and the hard delete's journal revision would already be
// <= the late-captured DeletionRevision, so LoadSessionDeletionDelta's
// (after, through] window would never include its tombstone. Both
// assertions below would fail under that ordering: PushedSessions.Total
// would be 0 instead of 1, and DeletedStaleSessions would be 0 instead of 1.
func TestRebuildMirrorSnapshotsStateBeforeSessionEnumeration(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	ids := seedRebuildFixture(t, local)
	mutatedID, deletedID := ids[0], ids[1]
	path := filepath.Join(t.TempDir(), "mirror.duckdb")

	s := newTestSync(t, path, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, s.DB()))

	// Capture the snapshot BEFORE any mutation, exactly as buildMirrorInto
	// now does before calling pushEverything.
	snapshot, err := captureRebuildSnapshot(ctx, local)
	require.NoError(t, err)

	// Mutations that stand in for changes racing an in-flight rebuild's
	// push loop: one session gets a new message (bumping its sync_marker),
	// another gets hard-deleted (bumping the deletion journal revision).
	// Both happen strictly after the snapshot was captured.
	appendMessage(t, local, mutatedID)
	require.NoError(t, local.DeleteSession(deletedID))

	// The rebuild's full push runs after the mutations, as it would once
	// they land mid-enumeration in a real race, and since it reads local
	// state fresh it already reflects them: mutatedID is pushed with its
	// appended message, deletedID is simply absent from the fresh mirror.
	result, err := s.pushEverything(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, 0, result.Errors)
	identityRevision, err := s.syncProjectIdentityObservations(ctx, 0, true)
	require.NoError(t, err)
	require.NoError(t, s.writeRebuildMetadata(ctx, "", snapshot, identityRevision))
	require.NoError(t, s.Close())

	// A further content-only change to mutatedID, applied after the
	// rebuild "completes": a raw UPDATE that never touches the sessions
	// table, so sync_marker is left exactly where the append put it. Only
	// a correctly pre-captured LastPushCutoff can still catch this.
	mutateSessionContent(t, local, mutatedID)

	res, err := Push(ctx, path, local, "test-machine", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.False(t, res.Diagnostics.Full,
		"a valid mirror with fresh metadata must not force a rebuild")
	assert.Equal(t, 1, res.Diagnostics.PushedSessions.Total,
		"session mutated during rebuild must still be caught by the next incremental push")
	assert.Equal(t, 1, res.Diagnostics.DeletedStaleSessions,
		"session hard-deleted during rebuild must still be reconciled by the next incremental push")
}

// TestRebuildCurationFingerprintCapturedBeforeCurationCopy is the FIX5
// regression, mirroring TestRebuildMirrorSnapshotsStateBeforeSessionEnumeration's
// approach: the curation fingerprint a rebuild stores must be captured
// BEFORE replaceCuration copies curation into the mirror, the same
// pre-capture principle rebuildSnapshot documents for the cutoff/deletion
// revision. Capturing it AFTER the copy instead (as the code previously
// did) can permanently strand a curation edit that races the copy: the
// edit is fingerprinted (a late read already reflects it) but was never
// mirrored (the copy already ran), so the stored fingerprint then matches
// all future local state and refreshCurationIfChanged skips the mirror
// refresh forever, until some other curation edit happens to shake it
// loose.
//
// There is no hook into the middle of a running rebuild here, so — exactly
// like the session-enumeration regression test — the race is reproduced by
// calling the same building blocks pushEverything composes
// (curationFingerprint, replaceCuration, recordMetadataKey) directly
// in the fixed order, with a curation mutation landing between the
// fingerprint capture and the copy.
func TestRebuildCurationFingerprintCapturedBeforeCurationCopy(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	ids := seedRebuildFixture(t, local)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")

	s := newTestSync(t, path, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, s.DB()))

	sessions, err := local.ListSessionsForMirrorWindow(ctx, "", nil, nil)
	require.NoError(t, err)
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
	fingerprints, err := s.sessionFingerprints(ctx, sessions)
	require.NoError(t, err)
	for _, sess := range sessions {
		_, err := s.pushSingleSession(ctx, sess, fingerprints[sess.ID])
		require.NoError(t, err)
	}

	// Capture the curation fingerprint BEFORE the curation copy, exactly as
	// the fixed pushEverything now does.
	fingerprint, err := s.curationFingerprint(ctx)
	require.NoError(t, err)

	// A star lands after the fingerprint capture but before the curation
	// copy below runs — standing in for a curation edit racing an
	// in-flight rebuild's replaceCuration call.
	ok, err := local.StarSession(ids[1])
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, s.replaceCuration(ctx))
	require.NoError(t, recordMetadataKey(
		ctx, s.DB(), curationFingerprintMetadataKey, fingerprint,
	))

	snapshot, err := captureRebuildSnapshot(ctx, local)
	require.NoError(t, err)
	identityRevision, err := s.syncProjectIdentityObservations(ctx, 0, true)
	require.NoError(t, err)
	require.NoError(t, s.writeRebuildMetadata(ctx, "", snapshot, identityRevision))
	require.NoError(t, s.Close())

	// The star already made it into this "rebuild" because
	// pushEverythingCuration re-reads local state fresh — no data was lost —
	// but the fingerprint stored above predates it, so the next incremental
	// push must still detect the mismatch and refresh curation rather than
	// treating the mirror's (already correct) state as unchanged forever.
	assertMirrorTableCountWhere(t, path, "starred_sessions", "session_id = ?", ids[1], 1)

	res, err := Push(ctx, path, local, "test-machine", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.False(t, res.Diagnostics.Full,
		"a valid mirror with fresh metadata must not force a rebuild")
	assert.True(t, res.Diagnostics.CurationRefreshed,
		"a curation change racing the rebuild's fingerprint capture must still trigger a refresh")
	assertMirrorTableCountWhere(t, path, "starred_sessions", "session_id = ?", ids[1], 1)
}

// TestSweepStaleTempFilesRemovesOnlyOldFilesInsideWorkDir covers
// sweepStaleTempFiles' ownership, age, and shape guards. Ownership is
// location: the sweep only ever looks inside the mirror's private work
// directory (<mirror>.agentsview-work), so SIBLING files of the mirror are
// never touched, no matter how exactly their names match the generated
// temp-file shape — the sweep runs before the destination is even probed,
// and a user's own `mirror.duckdb.tmp-123` next to a path that is not our
// mirror must survive it. Inside the work directory, a <base>.tmp-<digits>
// file younger than staleTempFileAge is a rebuild that is (or recently was)
// genuinely in progress and must survive, while an old one is a crash
// leftover (rebuildMirror's own cleanup only fires for its own process; a
// killed process never reaches it) and must be removed. Files that don't
// match the generated shape — os.CreateTemp expands "*" to digits only —
// are left alone regardless of age, and the work directory itself is never
// removed.
func TestSweepStaleTempFilesRemovesOnlyOldFilesInsideWorkDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mirror.duckdb")
	workDir := mirrorWorkDirPath(path)
	require.NoError(t, os.MkdirAll(workDir, 0o755))
	oldTime := time.Now().Add(-2 * staleTempFileAge)
	writeOldFile := func(name string) string {
		t.Helper()
		require.NoError(t, os.WriteFile(name, []byte("x"), 0o644))
		require.NoError(t, os.Chtimes(name, oldTime, oldTime))
		return name
	}

	// Sibling user files: old AND exactly digit-shaped, yet outside the work
	// directory, so the sweep must never touch them.
	siblingTmp := writeOldFile(path + ".tmp-123")
	siblingReopen := writeOldFile(path + ".reopen-456")
	unrelated := writeOldFile(filepath.Join(dir, "unrelated.txt"))

	freshTmp := filepath.Join(workDir, "mirror.duckdb.tmp-123456789")
	require.NoError(t, os.WriteFile(freshTmp, []byte("x"), 0o644))
	staleTmp := writeOldFile(filepath.Join(workDir, "mirror.duckdb.tmp-987654321"))
	userNotes := writeOldFile(filepath.Join(workDir, "mirror.duckdb.tmp-notes.txt"))
	emptySuffix := writeOldFile(filepath.Join(workDir, "mirror.duckdb.tmp-"))

	// writeMirrorMarker binds the marker to the mirror file's identity, so
	// the mirror file itself must exist first. The marker stays a SIBLING of
	// the mirror and must never be swept.
	require.NoError(t, os.WriteFile(path, []byte("mirror"), 0o644))
	marker := MirrorMarkerPath(path)
	require.NoError(t, writeMirrorMarker(path, "m"))
	require.NoError(t, os.Chtimes(marker, oldTime, oldTime))

	require.NoError(t, sweepStaleTempFiles(path))

	assert.FileExists(t, siblingTmp,
		"a user's sibling file matching the temp shape must never be swept")
	assert.FileExists(t, siblingReopen,
		"a user's sibling file matching the reopen shape must never be swept")
	assert.FileExists(t, unrelated, "unrelated sibling files must survive")
	assert.FileExists(t, freshTmp, "a fresh temp file must survive the sweep")
	assert.NoFileExists(t, staleTmp,
		"a work-dir temp file older than staleTempFileAge must be removed")
	assert.FileExists(t, userNotes,
		"a work-dir file with a non-digit suffix must survive")
	assert.FileExists(t, emptySuffix,
		"a bare .tmp- name (empty suffix) is not a generated temp file and must survive")
	assert.DirExists(t, workDir, "the sweep must never remove the work directory itself")
	assert.FileExists(t, marker,
		"the sidecar ownership marker is a sibling and must never be swept")
}

// TestSweepStaleTempFilesMissingWorkDirIsNoOp guards the common case: no
// work directory yet (nothing was ever rebuilt or reopened at this path).
// The sweep must return nil without creating the directory — probes and
// sweeps never create anything.
func TestSweepStaleTempFilesMissingWorkDirIsNoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	require.NoError(t, sweepStaleTempFiles(path))
	assert.NoDirExists(t, mirrorWorkDirPath(path),
		"a sweep must never create the work directory")
}

// TestSweepStaleTempFilesHandlesGlobMetacharactersInDirectory is the FIX7
// regression: sweepStaleTempFiles used to build its match pattern with
// filepath.Glob, so a project or archive directory name containing glob
// metacharacters ([, ?, *) would be interpreted as glob syntax instead of
// literal characters, breaking or over-matching the sweep. A literal
// os.ReadDir + prefix-match sweep must work the same way regardless of what
// characters appear in the directory name.
func TestSweepStaleTempFilesHandlesGlobMetacharactersInDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj[1]")
	require.NoError(t, os.Mkdir(dir, 0o755))
	path := filepath.Join(dir, "mirror.duckdb")
	workDir := mirrorWorkDirPath(path)
	require.NoError(t, os.MkdirAll(workDir, 0o755))

	staleTmp := filepath.Join(workDir, "mirror.duckdb.tmp-424242")
	require.NoError(t, os.WriteFile(staleTmp, []byte("x"), 0o644))
	oldTime := time.Now().Add(-2 * staleTempFileAge)
	require.NoError(t, os.Chtimes(staleTmp, oldTime, oldTime))

	require.NoError(t, sweepStaleTempFiles(path))

	assert.NoFileExists(t, staleTmp,
		"a stale temp file in a glob-metacharacter directory must still be swept")
}
