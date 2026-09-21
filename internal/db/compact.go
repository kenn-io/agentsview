package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	compactManifestVersion = 1
	compactSafetyFloor     = int64(256 << 20)
	compactSafetyNumerator = int64(1)
	compactSafetyDenom     = int64(10)
	compactMaxInt64        = int64(^uint64(0) >> 1)

	// compactPhasePrepared means the candidate and backup are durable and the
	// original archive is still authoritative: recovery may roll forward or
	// back. compactPhaseCommitted means the installed archive passed
	// verification and is authoritative forever: recovery only cleans up and
	// must never restore the backup, because writes may have happened since.
	compactPhasePrepared  = "prepared"
	compactPhaseCommitted = "committed"

	compactOpDirPrefix   = "agentsview-compact-"
	compactCandidateName = "sessions.compacted.db"
	compactBackupName    = "sessions.original.db"
	compactManifestName  = "compact-recovery.json"
	compactStagingName   = "compact-staging"
)

// Test seams: invoked mid-operation so tests can observe the write barrier
// and exercise caller-cancellation after the point of no return.
var (
	compactTestHookDuringBuild  func()
	compactTestHookAfterInstall func()
)

// removeCompactManifest indirects manifest deletion so tests can inject a
// cleanup failure and prove the write barrier outlives a surviving manifest.
var removeCompactManifest = removeIfExists

// compactOpDirPrefixFor namespaces operation directories by archive so that
// archives sharing one staging directory can never sweep each other's
// in-flight candidate or backup.
func compactOpDirPrefixFor(databasePath string) string {
	if abs, err := filepath.Abs(databasePath); err == nil {
		databasePath = abs
	}
	sum := sha256.Sum256([]byte(databasePath))
	return compactOpDirPrefix + hex.EncodeToString(sum[:4]) + "-"
}

// ArchiveFootprint describes the files which make up a local SQLite archive.
// Missing WAL and SHM sidecars are reported as zero bytes.
type ArchiveFootprint struct {
	DatabaseBytes int64 `json:"database_bytes"`
	WALBytes      int64 `json:"wal_bytes"`
	SHMBytes      int64 `json:"shm_bytes"`
	TotalBytes    int64 `json:"total_bytes"`
}

// CompactEstimate reports the amount of reclaimable SQLite storage without
// changing the archive.
type CompactEstimate struct {
	ArchiveFootprint
	PageSize               int64 `json:"page_size"`
	PageCount              int64 `json:"page_count"`
	FreeListCount          int64 `json:"freelist_count"`
	FreeListBytes          int64 `json:"freelist_bytes"`
	EstimatedDatabaseBytes int64 `json:"estimated_database_bytes"`
	EstimatedReclaimBytes  int64 `json:"estimated_reclaim_bytes"`
	// StagingRequiredBytes is the conservative additional space needed when
	// staging and the live archive share one filesystem. It includes the
	// original backup, compacted candidate, final .installing copy, and safety
	// margin.
	StagingRequiredBytes int64 `json:"staging_required_bytes"`
}

// CompactOptions controls where the staged archive is built and whether the
// original database is retained after a successful replacement.
type CompactOptions struct {
	StagingDir string `json:"staging_dir"`
	KeepBackup bool   `json:"keep_backup"`
}

// CompactResult is the observable result of a staged archive replacement.
type CompactResult struct {
	Before         CompactEstimate `json:"before"`
	After          CompactEstimate `json:"after"`
	ReclaimedBytes int64           `json:"reclaimed_bytes"`
	BackupPath     string          `json:"backup_path,omitempty"`
	DurationMillis int64           `json:"duration_millis"`
}

type compactManifest struct {
	Version                int              `json:"version"`
	Phase                  string           `json:"phase"`
	DatabasePath           string           `json:"database_path"`
	OriginalBackupPath     string           `json:"original_backup_path"`
	CompactedPath          string           `json:"compacted_path"`
	InstallingPath         string           `json:"installing_path"`
	OriginalSHA256         string           `json:"original_sha256"`
	OriginalBytes          int64            `json:"original_bytes"`
	CompactedSHA256        string           `json:"compacted_sha256"`
	ExpectedCompactedBytes int64            `json:"expected_compacted_bytes"`
	ExpectedUserVersion    int              `json:"expected_user_version"`
	ExpectedSchemaHash     string           `json:"expected_schema_hash"`
	ExpectedCounts         map[string]int64 `json:"expected_counts"`
	KeepBackup             bool             `json:"keep_backup"`
}

type compactSpaceRequirements struct {
	SharedFilesystemBytes   int64
	StagingFilesystemBytes  int64
	DatabaseFilesystemBytes int64
}

// ErrCompactInProgress reports that another staged compaction already owns
// this archive's staging and manifest paths.
var ErrCompactInProgress = errors.New("archive compaction already in progress")

var compactCoreTables = []string{
	"sessions",
	"messages",
	"tool_calls",
	"tool_result_events",
	"recall_entries",
	"recall_evidence",
}

// EstimateCompact reports the current archive footprint and the amount of
// storage a staged VACUUM is expected to reclaim.
func (db *DB) EstimateCompact(ctx context.Context) (CompactEstimate, error) {
	if err := ctx.Err(); err != nil {
		return CompactEstimate{}, err
	}
	stat, err := os.Stat(db.path)
	if err != nil {
		return CompactEstimate{}, fmt.Errorf("stat database: %w", err)
	}
	if stat.IsDir() || stat.Size() == 0 {
		return CompactEstimate{}, errors.New("database file is missing or empty")
	}

	pageSize, err := pragmaInt64(ctx, db.getReader(), "page_size")
	if err != nil {
		return CompactEstimate{}, fmt.Errorf("read page size: %w", err)
	}
	pageCount, err := pragmaInt64(ctx, db.getReader(), "page_count")
	if err != nil {
		return CompactEstimate{}, fmt.Errorf("read page count: %w", err)
	}
	freeListCount, err := pragmaInt64(ctx, db.getReader(), "freelist_count")
	if err != nil {
		return CompactEstimate{}, fmt.Errorf("read freelist count: %w", err)
	}
	if pageSize <= 0 || pageCount < 0 || freeListCount < 0 || freeListCount > pageCount {
		return CompactEstimate{}, fmt.Errorf(
			"invalid sqlite page statistics: page_size=%d page_count=%d freelist_count=%d",
			pageSize, pageCount, freeListCount,
		)
	}

	walBytes, err := optionalFileSize(db.path + "-wal")
	if err != nil {
		return CompactEstimate{}, fmt.Errorf("stat WAL: %w", err)
	}
	shmBytes, err := optionalFileSize(db.path + "-shm")
	if err != nil {
		return CompactEstimate{}, fmt.Errorf("stat SHM: %w", err)
	}
	databaseBytes := stat.Size()
	freeBytes := freeListCount * pageSize
	estimatedDatabaseBytes := (pageCount - freeListCount) * pageSize
	estimate := CompactEstimate{
		DatabaseBytes:          databaseBytes,
		WALBytes:               walBytes,
		SHMBytes:               shmBytes,
		TotalBytes:             compactByteSum(databaseBytes, walBytes, shmBytes),
		PageSize:               pageSize,
		PageCount:              pageCount,
		FreeListCount:          freeListCount,
		FreeListBytes:          freeBytes,
		EstimatedDatabaseBytes: estimatedDatabaseBytes,
		EstimatedReclaimBytes:  compactByteSum(freeBytes, walBytes),
	}
	estimate.StagingRequiredBytes = compactSpaceRequirementsForSizes(
		compactByteSum(databaseBytes, walBytes), estimatedDatabaseBytes,
	).SharedFilesystemBytes
	return estimate, nil
}

func compactByteSum(values ...int64) int64 {
	var total int64
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if total > compactMaxInt64-value {
			return compactMaxInt64
		}
		total += value
	}
	return total
}

func compactSafetyBytes(totalBytes int64) int64 {
	safety := totalBytes * compactSafetyNumerator / compactSafetyDenom
	if safety < compactSafetyFloor {
		return compactSafetyFloor
	}
	return safety
}

func compactSpaceRequirementsForSizes(
	backupBytes, compactedBytes int64,
) compactSpaceRequirements {
	sharedBase := compactByteSum(backupBytes, compactedBytes, compactedBytes)
	stagingBase := compactByteSum(backupBytes, compactedBytes)
	targetBase := compactedBytes
	return compactSpaceRequirements{
		SharedFilesystemBytes: compactByteSum(
			sharedBase, compactSafetyBytes(sharedBase),
		),
		StagingFilesystemBytes: compactByteSum(
			stagingBase, compactSafetyBytes(stagingBase),
		),
		DatabaseFilesystemBytes: compactByteSum(
			targetBase, compactSafetyBytes(targetBase),
		),
	}
}

func compactRemainingSpaceRequirementsForSizes(
	backupBytes, compactedBytes int64,
) compactSpaceRequirements {
	full := compactSpaceRequirementsForSizes(backupBytes, compactedBytes)
	sharedSafety := compactSafetyBytes(
		compactByteSum(backupBytes, compactedBytes, compactedBytes),
	)
	stagingSafety := compactSafetyBytes(
		compactByteSum(backupBytes, compactedBytes),
	)
	return compactSpaceRequirements{
		SharedFilesystemBytes: compactByteSum(
			backupBytes, compactedBytes, sharedSafety,
		),
		StagingFilesystemBytes: compactByteSum(
			backupBytes, stagingSafety,
		),
		DatabaseFilesystemBytes: full.DatabaseFilesystemBytes,
	}
}

func optionalFileSize(path string) (int64, error) {
	stat, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return stat.Size(), nil
}

func pragmaInt64(ctx context.Context, reader *readerHandle, name string) (int64, error) {
	var value int64
	if err := reader.QueryRowContext(ctx, "PRAGMA "+name).Scan(&value); err != nil {
		return 0, err
	}
	return value, nil
}

// compactOperation carries the paths and expectations of one staged
// replacement between its phases.
type compactOperation struct {
	options        CompactOptions
	stagingDir     string
	opDir          string
	candidatePath  string
	backupPath     string
	installingPath string
	manifestPath   string
	manifest       compactManifest
	verification   archiveVerification
}

// Compact builds a verified compact database in staging and installs it with a
// same-filesystem rename. During the build, writes fail fast with
// ErrWriterClosed (the same barrier a worker maintenance pass uses) while
// reads keep serving; connections pause only for the final swap. The write
// barrier stays up until the committed recovery record is durable, so no write
// can land on an archive that startup recovery might still replace.
func (db *DB) Compact(ctx context.Context, options CompactOptions) (result CompactResult, err error) {
	started := time.Now()
	defer func() { result.DurationMillis = time.Since(started).Milliseconds() }()
	if db.readOnly {
		return result, ErrReadOnly
	}
	// The manifest, .installing path, and staging layout are shared per
	// archive, so concurrent operations would corrupt each other's
	// artifacts. Fail fast instead of queueing a second multi-minute pass.
	if !db.compactMu.TryLock() {
		return result, ErrCompactInProgress
	}
	defer db.compactMu.Unlock()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if db.writerClosed.Load() {
		return result, ErrWriterClosed
	}

	result.Before, err = db.EstimateCompact(ctx)
	if err != nil {
		return result, fmt.Errorf("estimate archive compaction: %w", err)
	}
	op, err := db.prepareCompactStaging(options, result.Before)
	if err != nil {
		return result, err
	}

	// The backfill pass holds long read snapshots that would stall the final
	// swap's drain; it is restarted by restoreCompactServices on every exit.
	db.StopUsageCacheBackfill()
	defer db.restoreCompactServices()
	if err := db.CloseWriter(); err != nil {
		primary := fmt.Errorf("close archive writer for compaction: %w", err)
		_ = os.Remove(op.opDir)
		if rerr := db.ReopenWriter(); rerr != nil {
			return result, errors.Join(primary, fmt.Errorf(
				"reopen archive writer after failed compaction barrier: %w", rerr))
		}
		return result, primary
	}

	if err := db.buildCompactCandidate(ctx, op); err != nil {
		// Remove the staged artifacts and manifest while the barrier still
		// bars every write, then restore write service on the untouched
		// original. The order matters: a write that landed while a prepared
		// manifest survived could be discarded by startup recovery.
		primary := db.abortCompactBeforeInstall(err, op)
		if berr := compactManifestSettled(op.manifestPath); berr != nil {
			return result, errors.Join(primary, berr)
		}
		if rerr := db.ReopenWriter(); rerr != nil {
			return result, errors.Join(primary, fmt.Errorf(
				"reopen archive writer after compaction failure: %w", rerr))
		}
		return result, primary
	}
	if err := db.installCompactCandidate(ctx, op); err != nil {
		return result, err
	}
	if compactTestHookAfterInstall != nil {
		compactTestHookAfterInstall()
	}

	// Point of no return passed: verification and commit must finish even if
	// the caller (an HTTP request, a Ctrl-C'd CLI) has gone away.
	opCtx := context.WithoutCancel(ctx)
	if err := db.verifyReopenedArchive(opCtx, op.verification); err != nil {
		return result, db.rollbackCompactInstall(ctx,
			fmt.Errorf("verify reopened archive: %w", err), op)
	}
	op.manifest.Phase = compactPhaseCommitted
	if err := writeCompactManifest(op.manifestPath, op.manifest); err != nil {
		return result, db.rollbackCompactInstall(ctx,
			fmt.Errorf("record committed archive compaction: %w", err), op)
	}
	// Commit point: the compacted archive is authoritative from here on.
	db.writerClosed.Store(false)

	if after, aerr := db.EstimateCompact(opCtx); aerr != nil {
		log.Printf("compact: measuring compacted archive: %v", aerr)
	} else {
		result.After = after
		result.ReclaimedBytes = result.Before.TotalBytes - after.TotalBytes
	}
	if op.options.KeepBackup {
		result.BackupPath = op.backupPath
	}
	db.cleanupCommittedCompact(op)
	return result, nil
}

// prepareCompactStaging resolves the staging layout, settles any leftover
// state from a previous compaction, and checks free space. It creates the
// per-operation directory only after those gates pass.
func (db *DB) prepareCompactStaging(
	options CompactOptions, estimate CompactEstimate,
) (*compactOperation, error) {
	stagingDir := options.StagingDir
	if stagingDir == "" {
		stagingDir = filepath.Join(filepath.Dir(db.path), compactStagingName)
	}
	stagingDir, err := filepath.Abs(stagingDir)
	if err != nil {
		return nil, fmt.Errorf("resolve staging directory: %w", err)
	}
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return nil, fmt.Errorf("create staging directory: %w", err)
	}
	manifestPath := compactManifestPath(db.path)
	if err := settleLeftoverCompactManifest(db.path, manifestPath); err != nil {
		return nil, err
	}
	sweepCompactOrphans(stagingDir, "", compactOpDirPrefixFor(db.path))
	if err := checkCompactSpace(db.path, stagingDir, estimate); err != nil {
		return nil, err
	}
	opDir, err := os.MkdirTemp(stagingDir, compactOpDirPrefixFor(db.path))
	if err != nil {
		return nil, fmt.Errorf("create compact staging: %w", err)
	}
	op := &compactOperation{
		options:        options,
		stagingDir:     stagingDir,
		opDir:          opDir,
		candidatePath:  filepath.Join(opDir, compactCandidateName),
		backupPath:     filepath.Join(opDir, compactBackupName),
		installingPath: db.path + ".installing",
		manifestPath:   manifestPath,
	}
	op.manifest = compactManifest{
		Version:            compactManifestVersion,
		Phase:              compactPhasePrepared,
		DatabasePath:       db.path,
		OriginalBackupPath: op.backupPath,
		CompactedPath:      op.candidatePath,
		InstallingPath:     op.installingPath,
		KeepBackup:         options.KeepBackup,
	}
	return op, nil
}

// settleLeftoverCompactManifest resolves a manifest left by an earlier
// operation in this process. A committed leftover only needs its cleanup
// finished; anything else requires startup recovery and blocks a new run.
func settleLeftoverCompactManifest(databasePath, manifestPath string) error {
	manifest, err := readCompactManifest(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read compact recovery manifest: %w", err)
	}
	if manifest.Phase == compactPhaseCommitted {
		return finishCompactRecovery(databasePath, manifestPath, manifest)
	}
	return fmt.Errorf(
		"a previous archive compaction did not complete (recovery manifest %s, phase %q); "+
			"restart agentsview so startup recovery can resolve it",
		manifestPath, manifest.Phase,
	)
}

// buildCompactCandidate produces the verified candidate and durable backup
// while readers keep serving and the write barrier holds the source stable,
// then records the prepared manifest and stages the install copy.
func (db *DB) buildCompactCandidate(ctx context.Context, op *compactOperation) error {
	if err := db.vacuumIntoCandidate(ctx, op.candidatePath); err != nil {
		return err
	}
	if compactTestHookDuringBuild != nil {
		compactTestHookDuringBuild()
	}
	if err := syncFile(op.candidatePath); err != nil {
		return fmt.Errorf("sync compacted archive: %w", err)
	}
	if err := syncDirectory(op.opDir); err != nil {
		return fmt.Errorf("sync compact staging directory: %w", err)
	}
	verification, err := db.captureVerification(ctx)
	if err != nil {
		return fmt.Errorf("capture archive verification: %w", err)
	}
	op.verification = verification
	op.manifest.ExpectedUserVersion = verification.UserVersion
	op.manifest.ExpectedSchemaHash = verification.SchemaHash
	op.manifest.ExpectedCounts = maps.Clone(verification.Counts)
	if err := verifyArchiveFile(ctx, op.candidatePath, verification); err != nil {
		return fmt.Errorf("verify compacted archive: %w", err)
	}
	compactedHash, compactedBytes, err := sha256File(op.candidatePath)
	if err != nil {
		return fmt.Errorf("hash compacted archive: %w", err)
	}
	op.manifest.CompactedSHA256 = compactedHash
	op.manifest.ExpectedCompactedBytes = compactedBytes

	// The build checkpoint truncated the WAL and the write barrier keeps the
	// source stable, so the backup below is byte-identical to the live file.
	sourceStat, err := os.Stat(db.path)
	if err != nil {
		return fmt.Errorf("stat checkpointed archive: %w", err)
	}
	if err := checkCompactRemainingSpace(
		db.path, op.stagingDir, sourceStat.Size(), compactedBytes,
	); err != nil {
		return err
	}
	originalHash, originalBytes, err := copyFileSHA256(ctx, db.path, op.backupPath)
	if err != nil {
		return fmt.Errorf("backup original archive: %w", err)
	}
	if originalBytes != sourceStat.Size() {
		return fmt.Errorf(
			"archive size changed while backing it up: expected %d bytes, copied %d",
			sourceStat.Size(), originalBytes,
		)
	}
	if err := syncFile(op.backupPath); err != nil {
		return fmt.Errorf("sync original archive backup: %w", err)
	}
	if err := syncDirectory(op.opDir); err != nil {
		return fmt.Errorf("sync compact staging directory after backup: %w", err)
	}
	op.manifest.OriginalSHA256 = originalHash
	op.manifest.OriginalBytes = originalBytes
	if err := writeCompactManifest(op.manifestPath, op.manifest); err != nil {
		return fmt.Errorf("write compact recovery manifest: %w", err)
	}
	if err := copyFileVerified(
		ctx, op.candidatePath, op.installingPath, compactedHash,
	); err != nil {
		return fmt.Errorf("stage compacted archive for install: %w", err)
	}
	if err := syncDirectory(filepath.Dir(db.path)); err != nil {
		return fmt.Errorf("sync archive directory before install: %w", err)
	}
	return nil
}

// vacuumIntoCandidate checkpoints the WAL and runs VACUUM INTO through a
// dedicated maintenance connection, because the ordinary writer pool is closed
// for the barrier. The candidate is then switched to WAL mode so installing it
// does not rewrite its header on the first writable open.
func (db *DB) vacuumIntoCandidate(ctx context.Context, candidatePath string) error {
	maint, err := sql.Open("sqlite3", makeDSN(db.path, false))
	if err != nil {
		return fmt.Errorf("open maintenance connection: %w", err)
	}
	defer maint.Close()
	maint.SetMaxOpenConns(1)
	if err := checkpointWALTruncateConn(ctx, maint); err != nil {
		return fmt.Errorf("checkpoint WAL before VACUUM INTO: %w", err)
	}
	quotedPath := "'" + strings.ReplaceAll(candidatePath, "'", "''") + "'"
	if _, err := maint.ExecContext(ctx, "VACUUM INTO "+quotedPath); err != nil {
		return fmt.Errorf("VACUUM INTO: %w", err)
	}
	if err := maint.Close(); err != nil {
		return fmt.Errorf("close maintenance connection: %w", err)
	}
	candidate, err := sql.Open("sqlite3", makeDSN(candidatePath, false))
	if err != nil {
		return fmt.Errorf("open compacted archive for WAL conversion: %w", err)
	}
	defer candidate.Close()
	candidate.SetMaxOpenConns(1)
	var mode string
	if err := candidate.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return fmt.Errorf("set compacted archive journal mode: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("compacted archive journal mode is %q, expected wal", mode)
	}
	if err := candidate.Close(); err != nil {
		return fmt.Errorf("close compacted archive after WAL conversion: %w", err)
	}
	// The close checkpoints the conversion WAL; only the bare candidate file
	// is hashed and installed, so a WAL that still holds frames would mean
	// silent data loss. Empty leftover sidecars are just removed.
	walSize, err := optionalFileSize(candidatePath + "-wal")
	if err != nil {
		return fmt.Errorf("stat compacted archive WAL: %w", err)
	}
	if walSize > 0 {
		return fmt.Errorf(
			"compacted archive WAL still holds %d bytes after conversion", walSize)
	}
	if err := removeIfExists(candidatePath + "-wal"); err != nil {
		return err
	}
	return removeIfExists(candidatePath + "-shm")
}

// checkpointWALTruncateConn retries a truncate checkpoint on conn so
// short-lived readers can release their pinned pages.
func checkpointWALTruncateConn(ctx context.Context, conn *sql.DB) error {
	var lastErr error
	for i := range walCheckpointAttempts {
		var busy, logPages, checkpointedPages int
		err := conn.QueryRowContext(
			ctx, "PRAGMA wal_checkpoint(TRUNCATE)",
		).Scan(&busy, &logPages, &checkpointedPages)
		if err != nil {
			return err
		}
		if busy == 0 {
			return nil
		}
		lastErr = ErrWALCheckpointBusy
		if i == walCheckpointAttempts-1 {
			break
		}
		timer := time.NewTimer(walCheckpointRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return lastErr
}

// installCompactCandidate performs the short exclusive swap: close and drain
// every pool, rename the staged copy over the archive, and reopen with the
// write barrier still up. On failure the unchanged (or restored) original is
// serving again before the error returns.
func (db *DB) installCompactCandidate(ctx context.Context, op *compactOperation) error {
	db.mu.Lock()
	if err := db.closeConnectionsLocked(ctx); err != nil {
		return db.reopenUnchangedAfterSwapFailureLocked(
			fmt.Errorf("close archive connections: %w", err), op)
	}
	if err := removeIfExists(db.path + "-wal"); err != nil {
		return db.reopenUnchangedAfterSwapFailureLocked(
			fmt.Errorf("remove archive WAL: %w", err), op)
	}
	if err := removeIfExists(db.path + "-shm"); err != nil {
		return db.reopenUnchangedAfterSwapFailureLocked(
			fmt.Errorf("remove archive SHM: %w", err), op)
	}
	if err := replaceInstalledFile(op.installingPath, db.path); err != nil {
		return db.restoreOriginalAfterSwapFailureLocked(
			fmt.Errorf("install compacted archive: %w", err), op)
	}
	if err := syncDirectory(filepath.Dir(db.path)); err != nil {
		return db.restoreOriginalAfterSwapFailureLocked(
			fmt.Errorf("sync archive directory after install: %w", err), op)
	}
	if err := db.reopenLockedWithBarrier(true); err != nil {
		return db.restoreOriginalAfterSwapFailureLocked(
			fmt.Errorf("reopen compacted archive: %w", err), op)
	}
	db.mu.Unlock()
	return nil
}

// reopenUnchangedAfterSwapFailureLocked restores service on the untouched
// original archive after a swap-phase failure that happened before the
// rename. The barrier stays up until the prepared manifest is removed, so no
// write can land that startup recovery might later discard. The caller holds
// db.mu; it is released here.
func (db *DB) reopenUnchangedAfterSwapFailureLocked(primary error, op *compactOperation) error {
	reopenErr := db.reopenLockedWithBarrier(true)
	db.mu.Unlock()
	if reopenErr != nil {
		return errors.Join(primary, fmt.Errorf(
			"reopen unchanged archive after swap failure: %w", reopenErr))
	}
	return db.settleCompactAbort(primary, op)
}

// settleCompactAbort removes the staged artifacts and lowers the write
// barrier only once no recovery manifest survives. A write accepted while a
// prepared manifest exists is a write startup recovery may discard.
func (db *DB) settleCompactAbort(primary error, op *compactOperation) error {
	err := db.abortCompactBeforeInstall(primary, op)
	if berr := compactManifestSettled(op.manifestPath); berr != nil {
		return errors.Join(err, berr)
	}
	db.writerClosed.Store(false)
	return err
}

// compactManifestSettled confirms no recovery manifest survives, the
// precondition for lowering the compaction write barrier.
func compactManifestSettled(manifestPath string) error {
	_, err := os.Stat(manifestPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("verify compact recovery manifest removal: %w", err)
	}
	return fmt.Errorf(
		"compact recovery manifest %s still present; writes stay barred until "+
			"the next writable start resolves it", manifestPath)
}

// restoreOriginalAfterSwapFailureLocked puts the original archive back after
// a failure at or beyond the rename. The write barrier has been up since
// before the backup was taken and stays up until the prepared manifest is
// removed, so the restore cannot discard a write. The caller holds db.mu; it
// is released here.
func (db *DB) restoreOriginalAfterSwapFailureLocked(primary error, op *compactOperation) error {
	restoreErr := restoreOriginalArchive(db.path, op.manifest)
	if restoreErr == nil {
		restoreErr = db.reopenLockedWithBarrier(true)
	}
	db.mu.Unlock()
	if restoreErr != nil {
		// Leave the prepared manifest and backup in place: startup recovery
		// restores the original conservatively. Keep the barrier so no write
		// lands on an archive recovery may still replace.
		return errors.Join(primary, fmt.Errorf(
			"restore original archive after failed install: %w (recovery manifest %s "+
				"resolves this on the next writable start)", restoreErr, op.manifestPath))
	}
	return db.settleCompactAbort(primary, op)
}

// rollbackCompactInstall undoes an installed but not yet committed
// replacement. Writes have been barred since before the backup was taken, so
// restoring the backup is lossless.
func (db *DB) rollbackCompactInstall(ctx context.Context, primary error, op *compactOperation) error {
	db.mu.Lock()
	if err := db.closeConnectionsLocked(ctx); err != nil {
		// The installed candidate keeps serving reads; the barrier stays up
		// and the prepared manifest lets startup recovery finish the decision.
		reopenErr := db.reopenLockedWithBarrier(true)
		db.mu.Unlock()
		return errors.Join(primary,
			fmt.Errorf("close archive connections for rollback: %w", err), reopenErr)
	}
	return db.restoreOriginalAfterSwapFailureLocked(primary, op)
}

// restoreOriginalArchive reinstates the pre-compaction archive at
// databasePath, preferring cheap paths: the original may still be in place,
// or a Windows install failure may have left it renamed aside as .failed.
func restoreOriginalArchive(databasePath string, manifest compactManifest) error {
	if err := removeIfExists(databasePath + "-wal"); err != nil {
		return fmt.Errorf("remove WAL before restore: %w", err)
	}
	if err := removeIfExists(databasePath + "-shm"); err != nil {
		return fmt.Errorf("remove SHM before restore: %w", err)
	}
	if matches, err := fileMatchesSHA256(databasePath, manifest.OriginalSHA256); err != nil {
		return fmt.Errorf("hash archive during restore: %w", err)
	} else if matches {
		return nil
	}
	restored, err := reinstateSidelinedOriginal(databasePath, manifest.OriginalSHA256)
	if err != nil {
		return fmt.Errorf("reinstate sidelined archive during restore: %w", err)
	}
	if restored {
		return nil
	}
	if err := installRecoveredFile(
		manifest.OriginalBackupPath, databasePath, manifest.OriginalSHA256,
	); err != nil {
		return fmt.Errorf("restore original archive from backup: %w", err)
	}
	return nil
}

// fileMatchesSHA256 reports whether path exists and hashes to expected.
func fileMatchesSHA256(path, expected string) (bool, error) {
	if expected == "" || !compactFileExists(path) {
		return false, nil
	}
	hash, _, err := sha256File(path)
	if err != nil {
		return false, err
	}
	return hash == expected, nil
}

// abortCompactBeforeInstall removes every staged artifact and the recovery
// manifest after a failure that left the original archive authoritative.
func (db *DB) abortCompactBeforeInstall(primary error, op *compactOperation) error {
	errs := []error{primary}
	if err := removeCompactPath(op.opDir, op.installingPath, false); err != nil {
		errs = append(errs, fmt.Errorf("cleanup compact staging: %w", err))
	}
	if err := syncDirectoryIfExists(op.stagingDir); err != nil {
		errs = append(errs, fmt.Errorf("sync compact staging: %w", err))
	}
	if err := removeCompactManifest(op.manifestPath); err != nil {
		errs = append(errs, fmt.Errorf("remove compact recovery manifest: %w", err))
	}
	if err := syncDirectory(filepath.Dir(db.path)); err != nil {
		errs = append(errs, fmt.Errorf("sync archive directory: %w", err))
	}
	return errors.Join(errs...)
}

// cleanupCommittedCompact removes the operation's artifacts and manifest
// after the commit point. Failures are logged, not returned: the compaction
// succeeded, and startup recovery finishes any leftover cleanup.
func (db *DB) cleanupCommittedCompact(op *compactOperation) {
	if err := removeCompactPath(
		op.opDir, op.installingPath, op.options.KeepBackup,
	); err != nil {
		log.Printf("compact: cleanup after committed compaction: %v "+
			"(the next writable start finishes cleanup)", err)
		return
	}
	if err := syncDirectoryIfExists(op.stagingDir); err != nil {
		log.Printf("compact: sync staging after committed compaction: %v", err)
	}
	if err := removeIfExists(op.manifestPath); err != nil {
		log.Printf("compact: remove recovery manifest after committed compaction: %v "+
			"(the next writable start finishes cleanup)", err)
		return
	}
	if err := syncDirectory(filepath.Dir(db.path)); err != nil {
		log.Printf("compact: sync archive directory after committed compaction: %v", err)
	}
}

// restoreCompactServices restarts the checkpoint loop and usage-cache
// backfill after Compact. Each exit path restores the writer itself; when
// one deliberately kept the barrier up (an uncertain on-disk state awaiting
// startup recovery), nothing may write and nothing is restarted.
func (db *DB) restoreCompactServices() {
	if db.writerClosed.Load() || db.rawWriter() == nil {
		return
	}
	db.startWALCheckpointLoop()
	if err := db.restartUsageCacheBackfillIfEnabled(); err != nil {
		log.Printf("compact: restarting usage cache backfill: %v", err)
	}
}

func checkCompactSpace(databasePath, stagingDir string, estimate CompactEstimate) error {
	targetDir := filepath.Dir(databasePath)
	shared, err := sameFilesystem(stagingDir, targetDir)
	if err != nil {
		return fmt.Errorf("compare compact staging and database filesystems: %w", err)
	}
	requirements := compactSpaceRequirementsForSizes(
		compactByteSum(estimate.DatabaseBytes, estimate.WALBytes),
		estimate.EstimatedDatabaseBytes,
	)
	if shared {
		return requireCompactFilesystemSpace(
			"shared staging/database filesystem", targetDir,
			requirements.SharedFilesystemBytes,
		)
	}
	if err := requireCompactFilesystemSpace(
		"staging filesystem", stagingDir, requirements.StagingFilesystemBytes,
	); err != nil {
		return err
	}
	return requireCompactFilesystemSpace(
		"database filesystem", targetDir, requirements.DatabaseFilesystemBytes,
	)
}

// checkCompactRemainingSpace revalidates the exact allocations still needed
// after VACUUM INTO has created the candidate. The candidate's occupied bytes
// are already reflected in the current free-space readings.
func checkCompactRemainingSpace(
	databasePath, stagingDir string, backupBytes, compactedBytes int64,
) error {
	targetDir := filepath.Dir(databasePath)
	shared, err := sameFilesystem(stagingDir, targetDir)
	if err != nil {
		return fmt.Errorf("compare compact staging and database filesystems: %w", err)
	}
	requirements := compactRemainingSpaceRequirementsForSizes(
		backupBytes, compactedBytes,
	)
	if shared {
		return requireCompactFilesystemSpace(
			"shared staging/database filesystem", targetDir,
			requirements.SharedFilesystemBytes,
		)
	}
	if err := requireCompactFilesystemSpace(
		"staging filesystem", stagingDir, requirements.StagingFilesystemBytes,
	); err != nil {
		return err
	}
	return requireCompactFilesystemSpace(
		"database filesystem", targetDir, requirements.DatabaseFilesystemBytes,
	)
}

func requireCompactFilesystemSpace(label, path string, required int64) error {
	free, ok, err := filesystemFreeBytes(path)
	if err != nil {
		return fmt.Errorf("check %s space: %w", label, err)
	}
	if ok && free < uint64(required) {
		available := compactMaxInt64
		if free <= uint64(compactMaxInt64) {
			available = int64(free)
		}
		return fmt.Errorf(
			"insufficient %s space: need %s additional free space, have %s",
			label, formatBytes(required), formatBytes(available),
		)
	}
	return nil
}

type archiveVerification struct {
	UserVersion int
	SchemaHash  string
	Counts      map[string]int64
}

func (db *DB) captureVerification(ctx context.Context) (archiveVerification, error) {
	userVersion, err := pragmaInt64(ctx, db.getReader(), "user_version")
	if err != nil {
		return archiveVerification{}, err
	}
	schemaHash, err := schemaHash(ctx, db.rawReader())
	if err != nil {
		return archiveVerification{}, err
	}
	counts, err := tableCountsDB(ctx, db.rawReader())
	if err != nil {
		return archiveVerification{}, err
	}
	return archiveVerification{UserVersion: int(userVersion), SchemaHash: schemaHash, Counts: counts}, nil
}

func (db *DB) verifyReopenedArchive(ctx context.Context, expected archiveVerification) error {
	if err := verifyQuickCheck(ctx, db.rawReader()); err != nil {
		return err
	}
	actual, err := db.captureVerification(ctx)
	if err != nil {
		return err
	}
	if actual.UserVersion != expected.UserVersion || actual.SchemaHash != expected.SchemaHash || !sameCounts(expected.Counts, actual.Counts) {
		return errors.New("reopened archive differs from the verified compacted archive")
	}
	return nil
}

func verifyQuickCheck(ctx context.Context, conn *sql.DB) error {
	var result string
	if err := conn.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("quick_check: %w", err)
	}
	if !strings.EqualFold(result, "ok") {
		return fmt.Errorf("quick_check returned %q", result)
	}
	return nil
}

func pragmaInt64DB(ctx context.Context, conn *sql.DB, name string) (int64, error) {
	var value int64
	if err := conn.QueryRowContext(ctx, "PRAGMA "+name).Scan(&value); err != nil {
		return 0, err
	}
	return value, nil
}

func schemaHash(ctx context.Context, conn *sql.DB) (string, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT type, name, COALESCE(sql, '')
		FROM sqlite_master
		WHERE name NOT LIKE 'sqlite_%'
		ORDER BY type, name`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	hash := sha256.New()
	for rows.Next() {
		var typ, name, sqlText string
		if err := rows.Scan(&typ, &name, &sqlText); err != nil {
			return "", err
		}
		fmt.Fprintf(hash, "%s\x00%s\x00%s\n", typ, name, sqlText)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func tableCountsDB(ctx context.Context, conn *sql.DB) (map[string]int64, error) {
	counts := make(map[string]int64, len(compactCoreTables))
	for _, table := range compactCoreTables {
		var count int64
		if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM \""+table+"\"").Scan(&count); err != nil {
			return nil, fmt.Errorf("count %s: %w", table, err)
		}
		counts[table] = count
	}
	return counts, nil
}

func sameCounts(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func sha256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), n, nil
}

func copyFileSHA256(ctx context.Context, source, target string) (string, int64, error) {
	in, err := os.Open(source)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", 0, err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, err
	}
	defer out.Close()
	hash := sha256.New()
	writer := io.MultiWriter(out, hash)
	buffer := make([]byte, 1<<20)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return "", total, err
		}
		n, readErr := in.Read(buffer)
		if n > 0 {
			written, writeErr := writer.Write(buffer[:n])
			if writeErr != nil {
				return "", total, writeErr
			}
			if written != n {
				return "", total, io.ErrShortWrite
			}
			total += int64(n)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return "", total, readErr
		}
	}
	if err := out.Sync(); err != nil {
		return "", total, err
	}
	return hex.EncodeToString(hash.Sum(nil)), total, nil
}

func copyFileVerified(ctx context.Context, source, target, expectedHash string) error {
	if err := removeIfExists(target); err != nil {
		return err
	}
	hash, _, err := copyFileSHA256(ctx, source, target)
	if err != nil {
		return err
	}
	if hash != expectedHash {
		return errors.New("copied compacted archive hash mismatch")
	}
	return syncFile(target)
}

// syncFile fsyncs path through a write-access handle: Windows
// FlushFileBuffers rejects handles opened with GENERIC_READ only.
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func removeCompactPath(path, installingPath string, keepBackup bool) error {
	// Verification opens the staged files as databases, which can leave WAL
	// and SHM sidecars beside them; remove those with their owners.
	for _, name := range []string{
		compactCandidateName,
		compactCandidateName + "-wal",
		compactCandidateName + "-shm",
		compactBackupName + "-wal",
		compactBackupName + "-shm",
	} {
		if err := removeIfExists(filepath.Join(path, name)); err != nil {
			return err
		}
	}
	if !keepBackup {
		if err := removeIfExists(filepath.Join(path, compactBackupName)); err != nil {
			return err
		}
	}
	if err := removeIfExists(installingPath); err != nil {
		return err
	}
	failedPath := installingPath + ".failed"
	if base, ok := strings.CutSuffix(installingPath, ".installing"); ok {
		failedPath = base + ".failed"
	}
	if err := removeIfExists(failedPath); err != nil {
		return err
	}
	if keepBackup {
		// Leave the operation directory in place because it contains the
		// intentionally retained original backup.
		return syncDirectoryIfExists(path)
	}
	if err := syncDirectoryIfExists(path); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func compactManifestPath(databasePath string) string {
	return filepath.Join(filepath.Dir(databasePath), compactManifestName)
}

func writeCompactManifest(path string, manifest compactManifest) error {
	data, err := json.Marshal(manifest, jsontext.WithIndent("  "))
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := removeIfExists(tmp); err != nil {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func readCompactManifest(path string) (compactManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return compactManifest{}, err
	}
	var manifest compactManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return compactManifest{}, fmt.Errorf(
			"decode compact recovery manifest %s: %w", path, err)
	}
	return manifest, nil
}

// RecoverCompactManifest completes or rolls back an interrupted staged
// replacement of the archive at databasePath, and sweeps staging directories
// a crash may have abandoned. Call it only for the canonical archive, after
// acquiring exclusive write ownership and before opening the database:
// derived databases (a resync temp, an isolated import) must not interpret
// the archive's manifest.
func RecoverCompactManifest(databasePath string) error {
	manifestPath := compactManifestPath(databasePath)
	manifest, err := readCompactManifest(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			sweepCompactOrphans(
				filepath.Join(filepath.Dir(databasePath), compactStagingName), "",
				compactOpDirPrefixFor(databasePath),
			)
			return nil
		}
		return fmt.Errorf("read compact recovery manifest: %w", err)
	}
	if manifest.Version != compactManifestVersion {
		return fmt.Errorf(
			"unsupported compact recovery manifest version %d in %s",
			manifest.Version, manifestPath)
	}
	// The manifest is directory-scoped, so a base-name match identifies it as
	// this archive's even after the data directory was moved; the staged file
	// paths inside may then be stale, which the hash gates below tolerate.
	if filepath.Base(manifest.DatabasePath) != filepath.Base(databasePath) {
		return fmt.Errorf(
			"compact recovery manifest %s describes database %q, not %q; "+
				"remove the manifest manually if it is stale",
			manifestPath, filepath.Base(manifest.DatabasePath),
			filepath.Base(databasePath))
	}
	opDir := filepath.Dir(manifest.CompactedPath)
	sweepCompactOrphans(filepath.Dir(opDir), opDir, compactOpDirPrefixFor(databasePath))

	switch manifest.Phase {
	case compactPhasePrepared:
		return recoverPreparedCompact(databasePath, manifestPath, manifest)
	case compactPhaseCommitted:
		// The installed archive became authoritative before this record was
		// written, and writes may have landed since. Only clean up.
		return finishCompactRecovery(databasePath, manifestPath, manifest)
	default:
		return fmt.Errorf(
			"unknown compact recovery phase %q in %s", manifest.Phase, manifestPath)
	}
}

// recoverPreparedCompact settles a replacement that stopped before its commit
// record: keep the archive when it is the original or a verified install, and
// otherwise reinstate whichever staged file proves intact. Writes were barred
// throughout the prepared phase, so restoring the backup is lossless.
func recoverPreparedCompact(
	databasePath, manifestPath string, manifest compactManifest,
) error {
	if manifest.CompactedSHA256 == "" || manifest.OriginalSHA256 == "" ||
		manifest.ExpectedCompactedBytes <= 0 || manifest.OriginalBytes <= 0 ||
		manifest.ExpectedSchemaHash == "" || len(manifest.ExpectedCounts) == 0 {
		return fmt.Errorf(
			"compact recovery manifest %s is missing integrity metadata; "+
				"remove it manually after verifying the archive", manifestPath)
	}
	expected := archiveVerification{
		UserVersion: manifest.ExpectedUserVersion,
		SchemaHash:  manifest.ExpectedSchemaHash,
		Counts:      manifest.ExpectedCounts,
	}

	if compactFileExists(databasePath) {
		isCompacted, err := fileMatchesSHA256(databasePath, manifest.CompactedSHA256)
		if err != nil {
			return err
		}
		isOriginal, err := fileMatchesSHA256(databasePath, manifest.OriginalSHA256)
		if err != nil {
			return err
		}
		if isCompacted || isOriginal {
			return finishCompactRecovery(databasePath, manifestPath, manifest)
		}
		// Neither hash matches: a crash after the reopen converted the
		// installed candidate to WAL mode. Content verification decides;
		// row counts could not have moved while the write barrier held.
		if err := verifyArchiveFile(
			context.Background(), databasePath, expected,
		); err == nil {
			return finishCompactRecovery(databasePath, manifestPath, manifest)
		}
	} else {
		restored, err := reinstateSidelinedOriginal(databasePath, manifest.OriginalSHA256)
		if err != nil {
			return err
		}
		if restored {
			return finishCompactRecovery(databasePath, manifestPath, manifest)
		}
		candidateValid, err := compactRecoveryFileState(
			manifest.CompactedPath, manifest.CompactedSHA256,
			manifest.ExpectedCompactedBytes, expected,
		)
		if err != nil {
			return err
		}
		if candidateValid {
			if err := installRecoveredFile(
				manifest.CompactedPath, databasePath, manifest.CompactedSHA256,
			); err != nil {
				return fmt.Errorf("install compacted archive during recovery: %w", err)
			}
			return finishCompactRecovery(databasePath, manifestPath, manifest)
		}
	}

	backupValid, err := compactRecoveryFileState(
		manifest.OriginalBackupPath, manifest.OriginalSHA256,
		manifest.OriginalBytes, expected,
	)
	if err != nil {
		return err
	}
	if backupValid {
		if err := installRecoveredFile(
			manifest.OriginalBackupPath, databasePath, manifest.OriginalSHA256,
		); err != nil {
			return fmt.Errorf("restore original archive during recovery: %w", err)
		}
		return finishCompactRecovery(databasePath, manifestPath, manifest)
	}
	return fmt.Errorf(
		"archive compaction recovery cannot proceed: neither the archive %s, the "+
			"compacted candidate %s, nor the original backup %s verifies against "+
			"manifest %s; resolve manually, then remove the manifest",
		databasePath, manifest.CompactedPath, manifest.OriginalBackupPath,
		manifestPath)
}

// reinstateSidelinedOriginal renames a Windows install failure's .failed
// original back into place when it matches the recorded hash. It must NOT go
// through replaceInstalledFile: on Windows that helper clears target+".failed"
// as scratch space, which here is the source itself. Any invalid file at
// databasePath is removed first; a crash between the remove and the rename is
// safe because startup recovery re-runs this decision from the .failed file.
func reinstateSidelinedOriginal(databasePath, expectedHash string) (bool, error) {
	matches, err := fileMatchesSHA256(databasePath+".failed", expectedHash)
	if err != nil || !matches {
		return false, err
	}
	if err := removeIfExists(databasePath + "-wal"); err != nil {
		return false, err
	}
	if err := removeIfExists(databasePath + "-shm"); err != nil {
		return false, err
	}
	if err := removeIfExists(databasePath); err != nil {
		return false, err
	}
	if err := os.Rename(databasePath+".failed", databasePath); err != nil {
		return false, fmt.Errorf("reinstate sidelined archive: %w", err)
	}
	return true, syncDirectory(filepath.Dir(databasePath))
}

func compactFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func compactRecoveryFileState(
	path, expectedHash string, expectedBytes int64, expected archiveVerification,
) (bool, error) {
	if !compactFileExists(path) {
		return false, nil
	}
	hash, size, err := sha256File(path)
	if err != nil {
		return false, fmt.Errorf("hash recovery candidate %s: %w", path, err)
	}
	if hash != expectedHash || size != expectedBytes {
		return false, nil
	}
	if err := verifyArchiveFile(context.Background(), path, expected); err != nil {
		return false, nil //nolint:nilerr // A candidate that fails integrity verification is ineligible for recovery.
	}
	return true, nil
}

func verifyArchiveFile(ctx context.Context, path string, expected archiveVerification) error {
	conn, err := sql.Open(sqliteUsageDriverName, makeDSN(path, true))
	if err != nil {
		return err
	}
	defer conn.Close()
	return verifyArchiveConn(ctx, conn, expected)
}

func verifyArchiveConn(ctx context.Context, conn *sql.DB, expected archiveVerification) error {
	if err := verifyQuickCheck(ctx, conn); err != nil {
		return err
	}
	userVersion, err := pragmaInt64DB(ctx, conn, "user_version")
	if err != nil {
		return err
	}
	if int(userVersion) != expected.UserVersion {
		return fmt.Errorf("user_version changed from %d to %d", expected.UserVersion, userVersion)
	}
	hash, err := schemaHash(ctx, conn)
	if err != nil {
		return err
	}
	if hash != expected.SchemaHash {
		return errors.New("schema fingerprint changed")
	}
	counts, err := tableCountsDB(ctx, conn)
	if err != nil {
		return err
	}
	if !sameCounts(expected.Counts, counts) {
		return fmt.Errorf("core table row counts changed: before=%v after=%v", expected.Counts, counts)
	}
	return nil
}

func installRecoveredFile(source, target, expectedHash string) error {
	hash, _, err := sha256File(source)
	if err != nil {
		return err
	}
	if expectedHash != "" && hash != expectedHash {
		return errors.New("recovery source hash mismatch")
	}
	if err := removeIfExists(target + "-wal"); err != nil {
		return err
	}
	if err := removeIfExists(target + "-shm"); err != nil {
		return err
	}
	installing := target + ".installing"
	if err := copyFileVerified(context.Background(), source, installing, hash); err != nil {
		return err
	}
	if err := replaceInstalledFile(installing, target); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(target))
}

// finishCompactRecovery removes the operation's artifacts and, last, its
// manifest. It uses both the manifest's recorded paths and paths derived from
// databasePath, so cleanup still works after the data directory was moved.
func finishCompactRecovery(
	databasePath, manifestPath string, manifest compactManifest,
) error {
	opDir := filepath.Dir(manifest.CompactedPath)
	if err := removeCompactPath(opDir, manifest.InstallingPath, manifest.KeepBackup); err != nil {
		return err
	}
	if err := removeIfExists(databasePath + ".installing"); err != nil {
		return err
	}
	if err := syncDirectoryIfExists(opDir); err != nil {
		return err
	}
	if err := syncDirectoryIfExists(filepath.Dir(opDir)); err != nil {
		return err
	}
	if err := removeIfExists(manifestPath); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(manifestPath))
}

// sweepCompactOrphans removes this archive's operation directories that a
// crash abandoned before their recovery manifest became durable. prefix is
// the archive-scoped name prefix, so directories belonging to another archive
// sharing the staging directory are never touched. Only directories still
// holding a compacted candidate (which is never worth keeping) or nothing at
// all are removed; a directory retaining just an original backup was kept
// deliberately by --keep-backup and is left alone.
func sweepCompactOrphans(stagingDir, keepDir, prefix string) {
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		dir := filepath.Join(stagingDir, entry.Name())
		if dir == keepDir {
			continue
		}
		if !compactFileExists(filepath.Join(dir, compactCandidateName)) {
			// Remove the directory only when it is empty; a backup-only
			// directory stays.
			if err := os.Remove(dir); err == nil {
				log.Printf("compact: removed empty abandoned staging directory %s", dir)
			}
			continue
		}
		if err := removeIfExists(filepath.Join(dir, compactCandidateName)); err != nil {
			continue
		}
		for _, name := range []string{
			compactCandidateName + "-wal", compactCandidateName + "-shm",
			compactBackupName, compactBackupName + "-wal", compactBackupName + "-shm",
		} {
			_ = removeIfExists(filepath.Join(dir, name))
		}
		if err := os.Remove(dir); err == nil {
			log.Printf("compact: removed abandoned staging directory %s", dir)
		}
	}
}

func syncDirectoryIfExists(path string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return syncDirectory(path)
}

func formatBytes(value int64) string {
	if value < 1024 {
		return fmt.Sprintf("%d B", value)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	f := float64(value)
	for _, unit := range units {
		f /= 1024
		if f < 1024 || unit == units[len(units)-1] {
			return fmt.Sprintf("%.2f %s", f, unit)
		}
	}
	return fmt.Sprintf("%d B", value)
}
