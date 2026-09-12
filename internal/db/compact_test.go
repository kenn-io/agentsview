package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestCompactStagedReplacementReclaimsFreePages(t *testing.T) {
	database := testDB(t)
	const payload = "0123456789abcdef0123456789abcdef"
	const rows = 256
	const repeats = 4096
	require.NoError(t, database.Update(func(tx bun.Tx) error {
		if _, err := tx.Exec(`CREATE TABLE compact_test_payload (value TEXT NOT NULL)`); err != nil {
			return err
		}
		stmt, err := tx.Prepare(`INSERT INTO compact_test_payload(value) VALUES (?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		value := strings.Repeat(payload, repeats)
		for range rows {
			if _, err := stmt.Exec(value); err != nil {
				return err
			}
		}
		return nil
	}))
	require.NoError(t, database.Update(func(tx bun.Tx) error {
		_, err := tx.Exec(`DELETE FROM compact_test_payload`)
		return err
	}))

	before, err := database.EstimateCompact(context.Background())
	require.NoError(t, err)
	require.Positive(t, before.FreeListBytes)

	result, err := database.Compact(context.Background(), CompactOptions{
		StagingDir: t.TempDir(),
	})
	require.NoError(t, err)
	require.Positive(t, result.ReclaimedBytes)
	require.Less(t, result.After.DatabaseBytes, result.Before.DatabaseBytes)
	require.Zero(t, result.After.FreeListCount)
	require.Empty(t, result.BackupPath)

	var count int
	require.NoError(t, database.Reader().QueryRow(
		`SELECT count(*) FROM compact_test_payload`,
	).Scan(&count))
	require.Zero(t, count)
	_, err = os.Stat(compactManifestPath(database.Path()))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestCompactEstimateIncludesWALAndStagingRequirement(t *testing.T) {
	database := testDB(t)
	estimate, err := database.EstimateCompact(context.Background())
	require.NoError(t, err)
	require.Positive(t, estimate.DatabaseBytes)
	require.GreaterOrEqual(t, estimate.TotalBytes, estimate.DatabaseBytes)
	require.GreaterOrEqual(t, estimate.StagingRequiredBytes,
		estimate.DatabaseBytes+estimate.WALBytes+
			2*estimate.EstimatedDatabaseBytes)
}

func TestCompactSpaceRequirementsIncludeInstallingCopy(t *testing.T) {
	const (
		backupBytes    = int64(4 << 30)
		compactedBytes = int64(2 << 30)
	)
	requirements := compactSpaceRequirementsForSizes(
		backupBytes, compactedBytes,
	)
	sharedBase := compactByteSum(backupBytes, compactedBytes, compactedBytes)
	stagingBase := compactByteSum(backupBytes, compactedBytes)
	require.Equal(t,
		compactByteSum(sharedBase, compactSafetyBytes(sharedBase)),
		requirements.SharedFilesystemBytes,
	)
	require.Equal(t,
		compactByteSum(stagingBase, compactSafetyBytes(stagingBase)),
		requirements.StagingFilesystemBytes,
	)
	require.Equal(t,
		compactByteSum(compactedBytes, compactSafetyBytes(compactedBytes)),
		requirements.DatabaseFilesystemBytes,
	)
}

func TestCompactCloseFailureReopensUnchangedArchive(t *testing.T) {
	database := testDB(t)
	require.NoError(t, database.Update(func(tx bun.Tx) error {
		_, err := tx.Exec(`
			CREATE TABLE compact_close_failure (value INTEGER NOT NULL);
			INSERT INTO compact_close_failure(value) VALUES (1), (2), (3);
		`)
		return err
	}))
	require.NoError(t, database.CheckpointWALTruncateWithRetry(
		context.Background(),
	))

	rows, err := database.rawReader().Query(
		`SELECT value FROM compact_close_failure ORDER BY value`,
	)
	require.NoError(t, err)
	defer rows.Close()
	require.True(t, rows.Next())

	restoreTimeout := SetCloseDrainTimeoutForTest(20 * time.Millisecond)
	defer restoreTimeout()
	staging := t.TempDir()
	_, err = database.Compact(context.Background(), CompactOptions{
		StagingDir: staging,
	})
	require.ErrorContains(t, err, "database file is not safe to replace")

	var count int
	require.NoError(t, database.Reader().QueryRow(
		`SELECT count(*) FROM compact_close_failure`,
	).Scan(&count))
	require.Equal(t, 3, count)
	// Service is fully restored: the write barrier is down again.
	require.NoError(t, database.Update(func(tx bun.Tx) error {
		_, err := tx.Exec(`INSERT INTO compact_close_failure(value) VALUES (4)`)
		return err
	}))
	require.NoFileExists(t, database.Path()+".installing")
	require.NoFileExists(t, compactManifestPath(database.Path()))
	entries, readErr := os.ReadDir(staging)
	require.NoError(t, readErr)
	require.Empty(t, entries)
}

func TestCompactKeepsBarrierWhenAbortLeavesManifest(t *testing.T) {
	database := testDB(t)
	require.NoError(t, database.Update(func(tx bun.Tx) error {
		_, err := tx.Exec(`
			CREATE TABLE compact_manifest_probe (value INTEGER NOT NULL);
			INSERT INTO compact_manifest_probe(value) VALUES (1);
		`)
		return err
	}))
	// Truncate the WAL first so the build's checkpoint succeeds despite the
	// held reader below; the reader then makes the swap's connection drain
	// time out after the prepared manifest is already durable, routing into
	// the abort path.
	require.NoError(t, database.CheckpointWALTruncateWithRetry(
		context.Background(),
	))
	rows, err := database.rawReader().Query(
		`SELECT value FROM compact_manifest_probe`,
	)
	require.NoError(t, err)
	defer rows.Close()
	require.True(t, rows.Next())
	restoreTimeout := SetCloseDrainTimeoutForTest(20 * time.Millisecond)
	defer restoreTimeout()

	injected := errors.New("injected manifest removal failure")
	prevRemove := removeCompactManifest
	removeCompactManifest = func(string) error { return injected }
	defer func() { removeCompactManifest = prevRemove }()

	_, err = database.Compact(context.Background(), CompactOptions{
		StagingDir: t.TempDir(),
	})
	require.ErrorIs(t, err, injected)
	require.ErrorContains(t, err, "writes stay barred")
	require.FileExists(t, compactManifestPath(database.Path()))

	writeErr := database.Update(func(tx bun.Tx) error {
		_, err := tx.Exec(`INSERT INTO compact_manifest_probe(value) VALUES (2)`)
		return err
	})
	require.ErrorIs(t, writeErr, ErrWriterClosed,
		"a surviving prepared manifest must keep writes barred: startup "+
			"recovery is allowed to discard anything written past it")

	// Reads keep serving the unchanged archive while the barrier holds.
	var count int
	require.NoError(t, database.Reader().QueryRow(
		`SELECT count(*) FROM compact_manifest_probe`,
	).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, removeIfExists(compactManifestPath(database.Path())))
}

func TestCompactKeepBackupRetainsOnlyOriginal(t *testing.T) {
	database := testDB(t)
	staging := t.TempDir()
	result, err := database.Compact(context.Background(), CompactOptions{
		StagingDir: staging,
		KeepBackup: true,
	})
	require.NoError(t, err)
	require.FileExists(t, result.BackupPath)
	require.NotContains(t, result.BackupPath, compactCandidateName)
	require.NoFileExists(t, filepath.Join(filepath.Dir(result.BackupPath), compactCandidateName))
	require.NoFileExists(t, database.Path()+".installing")
	require.NoFileExists(t, compactManifestPath(database.Path()))
}

func TestCompactBarsWritesDuringBuildAndRestoresThem(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	var duringErr, rawErr error
	compactTestHookDuringBuild = func() {
		duringErr = database.Update(func(tx bun.Tx) error {
			_, err := tx.Exec(`CREATE TABLE compact_barrier_probe (x INTEGER)`)
			return err
		})
		// A raw writer path that never takes db.mu must hit the same barrier.
		rawErr = database.UpsertProviderStatHash(ctx, parser.AgentCodex, "probe.jsonl", 1)
	}
	defer func() { compactTestHookDuringBuild = nil }()

	_, err := database.Compact(ctx, CompactOptions{
		StagingDir: t.TempDir(),
	})
	require.NoError(t, err)
	require.ErrorIs(t, duringErr, ErrWriterClosed,
		"a write during the staged build must fail fast, not park or succeed")
	require.ErrorIs(t, rawErr, ErrWriterClosed,
		"raw writer paths without db.mu must honor the compact barrier")
	require.NoError(t, database.Update(func(tx bun.Tx) error {
		_, err := tx.Exec(`CREATE TABLE compact_barrier_probe (x INTEGER)`)
		return err
	}), "writes must flow again after the compaction commits")
	require.NoError(t,
		database.UpsertProviderStatHash(ctx, parser.AgentCodex, "probe.jsonl", 1),
		"raw writer paths must flow again after the compaction commits")
}

func TestCompactBarsRawWritesBetweenInstallAndCommit(t *testing.T) {
	database := testDB(t)
	// After the swap the pools are reopened on the installed candidate but
	// the compaction has not committed; a write landing here would be lost
	// by a rollback, so the barrier must hold even with a live writer pool.
	var rawErr error
	compactTestHookAfterInstall = func() {
		rawErr = database.UpsertProviderStatHash(
			context.Background(), parser.AgentCodex, "probe.jsonl", 1)
	}
	defer func() { compactTestHookAfterInstall = nil }()

	_, err := database.Compact(context.Background(), CompactOptions{
		StagingDir: t.TempDir(),
	})
	require.NoError(t, err)
	require.ErrorIs(t, rawErr, ErrWriterClosed,
		"raw writes must stay barred between install and commit")
}

func TestCompactKeepsGitCacheReadOnlyBetweenInstallAndCommit(t *testing.T) {
	skipIfNoGit(t)
	database := testDB(t)
	repo := statsOutcomeRepo(t)
	insertSessionFixture(t, database, sessionFixture{
		id: "compact-git-cache", agent: "claude", userMsgs: 5,
		startedAt: hoursAgo(5), cwd: repo,
	})

	var stats *SessionStats
	var statsErr error
	compactTestHookAfterInstall = func() {
		require.True(t, database.WriterClosed(),
			"the compaction barrier must remain active before commit")
		require.NotNil(t, database.rawWriter(),
			"the installed archive must already have a reopened writer pool")
		stats, statsErr = database.GetSessionStats(context.Background(), StatsFilter{
			Since: "28d", IncludeGitOutcomes: true,
		})
	}
	defer func() { compactTestHookAfterInstall = nil }()

	_, err := database.Compact(context.Background(), CompactOptions{
		StagingDir: t.TempDir(),
	})
	require.NoError(t, err)
	require.NoError(t, statsErr, "compute outcome stats before compact commit")
	require.NotNil(t, stats, "session stats")
	require.NotNil(t, stats.OutcomeStats, "outcome stats")
	assert.Equal(t, 3, stats.OutcomeStats.Commits, "Commits")

	var cacheRows int
	require.NoError(t, database.Reader().QueryRow(
		`SELECT count(*) FROM git_cache`,
	).Scan(&cacheRows))
	assert.Zero(t, cacheRows,
		"git cache misses must not write until the compact commit point")
}

func TestCompactConcurrentAttemptReturnsBusy(t *testing.T) {
	database := testDB(t)
	secondStaging := t.TempDir()
	var secondErr error
	compactTestHookDuringBuild = func() {
		_, secondErr = database.Compact(context.Background(), CompactOptions{
			StagingDir: secondStaging,
		})
	}
	defer func() { compactTestHookDuringBuild = nil }()

	_, err := database.Compact(context.Background(), CompactOptions{
		StagingDir: t.TempDir(),
	})
	require.NoError(t, err, "the first compaction must complete")
	require.ErrorIs(t, secondErr, ErrCompactInProgress,
		"a concurrent compaction must fail fast, not share staging paths")
}

func TestCompactSurvivesCallerCancelAfterInstall(t *testing.T) {
	database := testDB(t)
	require.NoError(t, database.Update(func(tx bun.Tx) error {
		_, err := tx.Exec(`
			CREATE TABLE compact_cancel_probe (value INTEGER NOT NULL);
			INSERT INTO compact_cancel_probe(value) VALUES (7);
		`)
		return err
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	compactTestHookAfterInstall = func() { cancel() }
	defer func() { compactTestHookAfterInstall = nil }()

	result, err := database.Compact(ctx, CompactOptions{StagingDir: t.TempDir()})
	require.NoError(t, err,
		"cancellation after the swap must not roll back a good compaction")
	require.Positive(t, result.After.DatabaseBytes)

	var value int
	require.NoError(t, database.Reader().QueryRow(
		`SELECT value FROM compact_cancel_probe`,
	).Scan(&value))
	require.Equal(t, 7, value)
	require.NoFileExists(t, compactManifestPath(database.Path()))
}

func TestCompactRestartsUsageCacheBackfill(t *testing.T) {
	database := testDB(t)
	started := make(chan struct{}, 4)
	database.SetUsageCacheBackfillStarted(func() { started <- struct{}{} })
	require.NoError(t, database.StartUsageCacheBackfill(context.Background()))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("initial backfill pass did not start")
	}
	require.NoError(t, database.WaitUsageCacheBackfill(context.Background()))

	_, err := database.Compact(context.Background(), CompactOptions{
		StagingDir: t.TempDir(),
	})
	require.NoError(t, err)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("compaction did not restart the usage cache backfill")
	}
}

func TestCompactRefusedWhileWriterBarrierHeld(t *testing.T) {
	database := testDB(t)
	require.NoError(t, database.CloseWriter())
	defer func() { require.NoError(t, database.ReopenWriter()) }()

	_, err := database.Compact(context.Background(), CompactOptions{
		StagingDir: t.TempDir(),
	})
	require.ErrorIs(t, err, ErrWriterClosed)
	require.True(t, database.WriterClosed(),
		"a refused compaction must leave the outer barrier owner intact")
}

func TestCompactPreservesRecallFTSSearchable(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	if d.recallFTSKind(ctx) != "fts5" {
		t.Skip("requires fts5 runtime support")
	}
	insertSession(t, d, "s1", "agentsview", func(s *Session) {
		s.Agent = "codex"
	})
	// Free low rowids before compacting, the scenario in which VACUUM INTO
	// would renumber the survivor's rowid if it renumbered at all (see
	// TestVacuumPreservesRecallEntriesFTSSearchable for the in-place twin).
	bodies := []string{
		"alpha aardvark", "beta barnacle", "gamma heliotrope overflow",
	}
	for i, body := range bodies {
		_, err := d.InsertRecallEntry(RecallEntry{
			ID: fmt.Sprintf("m%d", i+1), Type: "fact", Scope: "project",
			Status: "accepted", Title: "t", Body: body,
			Project: "agentsview", Agent: "codex", SourceSessionID: "s1",
		})
		require.NoError(t, err, "insert recall")
	}
	_, err := d.getWriter().Exec(
		`DELETE FROM recall_entries WHERE id IN ('m1', 'm2')`,
	)
	require.NoError(t, err, "delete earlier entries")

	q := RecallQuery{Text: "heliotrope"}
	terms := recallQueryTerms(q.Text)
	pre, err := d.listRecallFTS5Candidates(ctx, q, terms)
	require.NoError(t, err, "fts5 search before compact")
	require.Len(t, pre, 1, "fts join finds survivor before compact")

	_, err = d.Compact(ctx, CompactOptions{StagingDir: t.TempDir()})
	require.NoError(t, err, "compact")

	post, err := d.listRecallFTS5Candidates(ctx, q, terms)
	require.NoError(t, err, "fts5 search after compact")
	require.Len(t, post, 1, "fts join still finds survivor after compact")
	assert.Equal(t, "m3", post[0].ID)
}

func TestRecoverCompactCommittedManifestPreservesLaterWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	d := testDBAtPath(t, path, "archive")
	ctx := context.Background()
	require.NoError(t, d.CheckpointWALTruncateWithRetry(ctx))

	opDir := filepath.Join(dir, compactStagingName, "agentsview-compact-crashed")
	require.NoError(t, os.MkdirAll(opDir, 0o755))
	backupPath := filepath.Join(opDir, compactBackupName)
	backupHash, backupBytes, err := copyFileSHA256(ctx, path, backupPath)
	require.NoError(t, err, "snapshot pre-compaction backup")

	// A write that landed after the commit record but before cleanup
	// finished. Recovery must never take it away.
	require.NoError(t, d.Update(func(tx bun.Tx) error {
		_, err := tx.Exec(`
			CREATE TABLE compact_committed_probe (value INTEGER NOT NULL);
			INSERT INTO compact_committed_probe(value) VALUES (42);
		`)
		return err
	}))
	require.NoError(t, d.Close())

	// The stale DatabasePath simulates a data directory that was moved after
	// the crash; the base name still identifies the manifest as this
	// archive's.
	require.NoError(t, writeCompactManifest(compactManifestPath(path), compactManifest{
		Version:                compactManifestVersion,
		Phase:                  compactPhaseCommitted,
		DatabasePath:           "/moved/away/test.db",
		OriginalBackupPath:     backupPath,
		CompactedPath:          filepath.Join(opDir, compactCandidateName),
		InstallingPath:         "/moved/away/test.db.installing",
		OriginalSHA256:         backupHash,
		OriginalBytes:          backupBytes,
		CompactedSHA256:        backupHash,
		ExpectedCompactedBytes: backupBytes,
	}))

	require.NoError(t, RecoverCompactManifest(path))
	require.NoFileExists(t, compactManifestPath(path))
	require.NoDirExists(t, opDir, "committed recovery removes the operation directory")

	reopened, err := OpenReadOnly(path)
	require.NoError(t, err, "reopen recovered archive")
	defer reopened.Close()
	var value int
	require.NoError(t, reopened.Reader().QueryRow(
		`SELECT value FROM compact_committed_probe`,
	).Scan(&value))
	require.Equal(t, 42, value,
		"a row written after the commit point must survive recovery")
}

func TestRecoverCompactPreparedManifestKeepsVerifiedArchive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	d := testDBAtPath(t, path, "archive")
	ctx := context.Background()
	require.NoError(t, d.CheckpointWALTruncateWithRetry(ctx))

	opDir := filepath.Join(dir, compactStagingName, "agentsview-compact-crashed")
	require.NoError(t, os.MkdirAll(opDir, 0o755))
	backupPath := filepath.Join(opDir, compactBackupName)
	backupHash, backupBytes, err := copyFileSHA256(ctx, path, backupPath)
	require.NoError(t, err, "snapshot older backup")

	// Advance the archive past the backup so a wrong restore is detectable,
	// then record its verification as the manifest's expectation.
	insertSession(t, d, "s1", "agentsview")
	require.NoError(t, d.CheckpointWALTruncateWithRetry(ctx))
	verification, err := d.captureVerification(ctx)
	require.NoError(t, err)
	require.NoError(t, d.Close())
	archiveHashBefore, archiveBytes, err := sha256File(path)
	require.NoError(t, err)

	// The recorded candidate hash matches nothing on disk: the crash happened
	// after the reopen already rewrote the installed file's header. Content
	// verification, not the hash, must decide.
	staleHash := sha256.Sum256([]byte("candidate before header rewrite"))
	require.NoError(t, writeCompactManifest(compactManifestPath(path), compactManifest{
		Version:                compactManifestVersion,
		Phase:                  compactPhasePrepared,
		DatabasePath:           path,
		OriginalBackupPath:     backupPath,
		CompactedPath:          filepath.Join(opDir, compactCandidateName),
		InstallingPath:         path + ".installing",
		OriginalSHA256:         backupHash,
		OriginalBytes:          backupBytes,
		CompactedSHA256:        hex.EncodeToString(staleHash[:]),
		ExpectedCompactedBytes: archiveBytes,
		ExpectedUserVersion:    verification.UserVersion,
		ExpectedSchemaHash:     verification.SchemaHash,
		ExpectedCounts:         verification.Counts,
	}))

	require.NoError(t, RecoverCompactManifest(path))
	archiveHashAfter, _, err := sha256File(path)
	require.NoError(t, err)
	require.Equal(t, archiveHashBefore, archiveHashAfter,
		"a content-verified archive must not be replaced by the older backup")
	require.NoFileExists(t, compactManifestPath(path))
	require.NoDirExists(t, opDir)
}

func TestRestoreOriginalArchiveReinstatesSidelinedOriginal(t *testing.T) {
	// A Windows install failure leaves the original renamed aside as .failed.
	// The restore must move that file back intact — on Windows the generic
	// replace helper clears target+".failed" as scratch, so routing the
	// sidelined source through it would delete the only original.
	for name, targetPresent := range map[string]bool{
		"invalid file at target": true,
		"target missing":         false,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "test.db")
			original := []byte("original archive bytes")
			require.NoError(t, os.WriteFile(path+".failed", original, 0o600))
			if targetPresent {
				require.NoError(t, os.WriteFile(
					path, []byte("half-installed candidate"), 0o600))
			}
			sum := sha256.Sum256(original)
			manifest := compactManifest{
				OriginalSHA256: hex.EncodeToString(sum[:]),
				// No backup on disk: success proves the sidelined file was used.
				OriginalBackupPath: filepath.Join(dir, "missing-backup"),
			}

			require.NoError(t, restoreOriginalArchive(path, manifest))
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, original, got,
				"the sidelined original must be reinstated intact")
			require.NoFileExists(t, path+".failed")
		})
	}
}

func TestRecoverCompactManifestRejectsForeignDatabaseName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	manifestPath := compactManifestPath(path)
	require.NoError(t, writeCompactManifest(manifestPath, compactManifest{
		Version:      compactManifestVersion,
		Phase:        compactPhasePrepared,
		DatabasePath: filepath.Join(dir, "other.db"),
	}))

	err := RecoverCompactManifest(path)
	require.ErrorContains(t, err, "other.db")
	require.ErrorContains(t, err, manifestPath,
		"the operator needs the manifest path to resolve the conflict")
	require.FileExists(t, manifestPath, "a foreign manifest is never deleted")
}

func TestRecoverCompactManifestSweepsAbandonedStaging(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, compactStagingName)
	path := filepath.Join(dir, "test.db")
	prefix := compactOpDirPrefixFor(path)

	orphan := filepath.Join(staging, prefix+"orphan")
	require.NoError(t, os.MkdirAll(orphan, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(orphan, compactCandidateName), []byte("partial candidate"), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(orphan, compactBackupName), []byte("partial backup"), 0o600))

	empty := filepath.Join(staging, prefix+"empty")
	require.NoError(t, os.MkdirAll(empty, 0o755))

	kept := filepath.Join(staging, prefix+"kept")
	require.NoError(t, os.MkdirAll(kept, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(kept, compactBackupName), []byte("retained backup"), 0o600))

	// An in-flight operation of a different archive sharing this staging
	// directory must never be swept, even mid-build with a candidate present.
	foreign := filepath.Join(staging,
		compactOpDirPrefixFor(filepath.Join(dir, "other.db"))+"active")
	require.NoError(t, os.MkdirAll(foreign, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(foreign, compactCandidateName), []byte("other archive candidate"), 0o600))

	require.NoError(t, RecoverCompactManifest(path))
	require.NoDirExists(t, orphan, "candidate-holding orphan is removed")
	require.NoDirExists(t, empty, "empty orphan is removed")
	require.FileExists(t, filepath.Join(kept, compactBackupName),
		"a --keep-backup directory is never swept")
	require.FileExists(t, filepath.Join(foreign, compactCandidateName),
		"another archive's staging is never swept")
}

func TestCompactStagingSweepIgnoresOtherArchives(t *testing.T) {
	database := testDB(t)
	staging := t.TempDir()
	foreign := filepath.Join(staging,
		compactOpDirPrefixFor(filepath.Join(staging, "other.db"))+"active")
	require.NoError(t, os.MkdirAll(foreign, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(foreign, compactCandidateName), []byte("other archive candidate"), 0o600))

	_, err := database.Compact(context.Background(), CompactOptions{
		StagingDir: staging,
	})
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(foreign, compactCandidateName),
		"compacting one archive must not sweep another archive's staging")
}

func TestOpenIgnoresCompactManifestForDerivedDatabase(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, compactManifestName)
	require.NoError(t, writeCompactManifest(manifestPath, compactManifest{
		Version:      compactManifestVersion,
		Phase:        compactPhasePrepared,
		DatabasePath: filepath.Join(dir, "sessions.db"),
	}))

	// A resync builds its temp database beside the archive; the archive's
	// pending manifest must not block or confuse that open.
	derived, err := Open(filepath.Join(dir, "sessions.db"+"-resync"))
	require.NoError(t, err)
	require.NoError(t, derived.Close())
	require.FileExists(t, manifestPath,
		"a derived database open must leave the archive's manifest alone")
}
