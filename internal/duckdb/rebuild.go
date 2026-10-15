package duckdb

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/kit/atomicfile"
)

// mirrorWorkDirSuffix is appended to the mirror path to form the mirror's
// private work directory, which holds every generated artifact: the rebuild
// file (rebuildFilePath), legacy rebuild temp files, and reopen hardlink aliases
// (openMirrorAlias). The directory name itself is the ownership marker —
// only agentsview creates and writes into <mirror>.agentsview-work — so the
// stale-artifact sweeps (sweepStaleTempFiles,
// SweepStaleMirrorReopenAliases) can safely run before the destination is
// even probed or recognized as ours: they only ever delete inside this
// directory and never touch SIBLINGS of the mirror, whose names a user's
// own files could coincidentally match.
const mirrorWorkDirSuffix = ".agentsview-work"

// mirrorWorkDirPath returns the path of the private work directory for the
// mirror at path. It never creates the directory: probes and sweeps must
// not create anything (see ensureMirrorWorkDir).
func mirrorWorkDirPath(path string) string {
	return path + mirrorWorkDirSuffix
}

// ensureMirrorWorkDir creates the mirror's work directory if needed and
// returns its path. Callers may only invoke it once real work is starting —
// a rebuild creating its rebuild file, a serve process hardlinking a reopen
// alias — never from probe/status/sweep paths, which must stay create-free.
//
// The directory must be private: the rebuild file and reopen aliases live
// here, so anyone who can write in this directory could swap in a symlink
// mid-rebuild or replace the rebuild file before the final rename. A fresh directory is created 0700, and an
// existing one is verified — a real directory (not a symlink), owned by
// the current user, not writable by group or other (Unix; Windows access
// control is ACL-based and skips the ownership check) — failing closed
// otherwise. This only matters when the mirror sits in a directory other
// local users can write to (a pre-created hostile work dir); in default
// user-owned locations nobody else can create the directory first.
func ensureMirrorWorkDir(path string) (string, error) {
	workDir := mirrorWorkDirPath(path)
	if err := os.Mkdir(workDir, 0o700); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf(
			"creating duckdb mirror work directory %s: %w", workDir, err,
		)
	}
	info, err := os.Lstat(workDir)
	if err != nil {
		return "", fmt.Errorf(
			"statting duckdb mirror work directory %s: %w", workDir, err,
		)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf(
			"duckdb mirror work directory %s is a symlink; remove it and retry",
			workDir,
		)
	}
	if !info.IsDir() {
		return "", fmt.Errorf(
			"duckdb mirror work directory %s is not a directory; remove it and retry",
			workDir,
		)
	}
	if err := checkWorkDirPrivate(workDir, info); err != nil {
		return "", err
	}
	return workDir, nil
}

// rebuildFileSuffix names the file a rebuild writes inside the mirror's
// work directory before swapping it over the mirror. The name is fixed so
// the next push finds a file left by a rebuild that was killed partway and
// continues it instead of starting over.
const rebuildFileSuffix = ".rebuild"

// rebuildLockSuffix names the lock file in the work directory that a push
// holds from reading the rebuild file until the rebuilt file replaces the
// mirror, so two pushes never build, validate, or swap the same file.
const rebuildLockSuffix = ".rebuild.lock"

// errRebuildLocked reports that another push holds the mirror's rebuild lock.
var errRebuildLocked = errors.New("another push is rebuilding the mirror")

// rebuildRunMetadataKey holds, inside a rebuild file, the rebuildRun the
// file is being built for. The record stays in the file when it replaces the
// mirror, so a rebuild whose final swap fails resumes too. Only rebuild files
// are checked for it; in a mirror it just records the rebuild that built it.
const rebuildRunMetadataKey = "agentsview_rebuild_run"

// rebuildFilePath returns the rebuild file path for the mirror at path.
func rebuildFilePath(path string) string {
	return filepath.Join(mirrorWorkDirPath(path), filepath.Base(path)+rebuildFileSuffix)
}

// rebuildRun describes what a rebuild file is being built for. A file built
// for a different archive generation, scope, machine, or format may hold
// rows the current push would not write, so only a matching file resumes.
type rebuildRun struct {
	// Reason is why the rebuild started. A resumed rebuild keeps it.
	Reason           string `json:"reason"`
	SchemaVersion    int    `json:"schema_version"`
	DataVersion      int    `json:"data_version"`
	Scope            string `json:"scope"`
	Machine          string `json:"machine"`
	SourceDatabaseID string `json:"source_database_id"`
	SourceArchiveID  string `json:"source_archive_id"`
	// Errored reports that the rebuild reached the end with session errors.
	// Such a rebuild resumes only while something else still requires a
	// rebuild, including readers blocking the incremental push; otherwise
	// the incremental push handles the failed sessions.
	Errored bool `json:"errored,omitempty"`
}

// newRebuildRun describes a rebuild this process would run now.
func newRebuildRun(
	ctx context.Context, local *db.DB, machine string,
	opts storage.MirrorPushOptions, reason string,
) (rebuildRun, error) {
	databaseID, err := local.GetDatabaseID(ctx)
	if err != nil {
		return rebuildRun{}, fmt.Errorf("reading local archive database id: %w", err)
	}
	archiveID, err := local.GetArchiveID(ctx)
	if err != nil {
		return rebuildRun{}, fmt.Errorf("reading local archive id: %w", err)
	}
	return rebuildRun{
		Reason:           reason,
		SchemaVersion:    SchemaVersion,
		DataVersion:      db.CurrentDataVersion(),
		Scope:            db.CanonicalPushScope(opts.Projects, opts.ExcludeProjects),
		Machine:          machine,
		SourceDatabaseID: databaseID,
		SourceArchiveID:  archiveID,
	}, nil
}

// resumableAs reports whether a file built for r can continue as a rebuild
// for current.
func (r rebuildRun) resumableAs(current rebuildRun) bool {
	r.Reason, r.Errored = current.Reason, current.Errored
	return r == current
}

// claimRebuild takes path's rebuild lock and returns the run of an unfinished
// rebuild to continue, or nil to start fresh. The caller holds the lock until
// the rebuild swaps in or fails, then unlocks it.
//
// fresh discards an unfinished rebuild: --full asks for every session to be
// rewritten, and the file's fingerprints cannot see in-place edits. The
// caller decides whether a rebuild that ended with session errors resumes.
func claimRebuild(
	ctx context.Context, path string, current rebuildRun, fresh bool,
) (*flock.Flock, *rebuildRun, error) {
	workDir, err := ensureMirrorWorkDir(path)
	if err != nil {
		return nil, nil, err
	}
	lock := flock.New(filepath.Join(workDir, filepath.Base(path)+rebuildLockSuffix))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, nil, fmt.Errorf("locking duckdb mirror %s for rebuild: %w", path, err)
	}
	if !locked {
		return nil, nil, errRebuildLocked
	}
	pending, err := pendingRebuild(ctx, path, current)
	if err == nil && pending != nil {
		switch {
		case fresh:
			log.Printf("duckdbsync: --full requested; discarding the unfinished rebuild (%s)", pending.Reason)
			pending, err = nil, removeRebuildFile(rebuildFilePath(path))
		}
	}
	if err != nil {
		return nil, nil, errors.Join(err, unlockRebuild(lock))
	}
	return lock, pending, nil
}

func unlockRebuild(lock *flock.Flock) error {
	if err := lock.Unlock(); err != nil {
		return fmt.Errorf("unlocking duckdb mirror rebuild: %w", err)
	}
	return nil
}

// rebuildFileExists reports whether an unfinished rebuild left its file.
func rebuildFileExists(path string) (bool, error) {
	file := rebuildFilePath(path)
	if _, err := os.Lstat(file); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("checking duckdb rebuild file %s: %w", file, err)
	}
	return true, nil
}

// pendingRebuild returns the run of a rebuild file left by an unfinished
// rebuild that current can continue, or nil when there is none. It removes a
// file that cannot continue: the mirror itself was never touched, so nothing
// else depends on it. A file another process holds open means a rebuild from
// an older build is running there, and pendingRebuild fails instead of
// competing with it. Cancellation keeps the file for the next push.
func pendingRebuild(ctx context.Context, path string, current rebuildRun) (*rebuildRun, error) {
	file := rebuildFilePath(path)
	exists, err := rebuildFileExists(path)
	if err != nil || !exists {
		return nil, err
	}
	// The work directory exists because the file does; this only verifies
	// it is still private before opening anything inside it.
	if _, err := ensureMirrorWorkDir(path); err != nil {
		return nil, err
	}
	conn, err := Open(ctx, file)
	if err != nil {
		if isMirrorHeldError(err) {
			return nil, rebuildInProgressError(path, err)
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("opening duckdb rebuild file %s: %w", file, err)
		}
		log.Printf("duckdbsync: discarding unreadable unfinished rebuild %s: %v", file, err)
		return nil, removeRebuildFile(file)
	}
	value, readErr := readMetadataKey(ctx, conn, rebuildRunMetadataKey)
	if err := conn.Close(); err != nil {
		return nil, fmt.Errorf("closing duckdb rebuild file %s: %w", file, err)
	}
	if readErr != nil && ctx.Err() != nil {
		return nil, fmt.Errorf("reading duckdb rebuild file %s: %w", file, readErr)
	}
	var run rebuildRun
	if readErr == nil && value != "" {
		readErr = json.Unmarshal([]byte(value), &run)
	}
	switch {
	case readErr != nil || value == "":
		log.Printf("duckdbsync: discarding unfinished rebuild %s with no readable run record: %v", file, readErr)
		return nil, removeRebuildFile(file)
	case !run.resumableAs(current):
		log.Printf("duckdbsync: discarding unfinished rebuild (%s) because the archive, scope, machine, or format changed since it began",
			run.Reason)
		return nil, removeRebuildFile(file)
	}
	return &run, nil
}

func rebuildInProgressError(path string, err error) error {
	return fmt.Errorf(
		"another push is already rebuilding the duckdb mirror %s (%w); "+
			"wait for it to finish, then retry", path, err,
	)
}

// removeRebuildFile removes a rebuild file and its write-ahead log.
func removeRebuildFile(file string) error {
	for _, name := range []string{file, file + ".wal"} {
		if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing duckdb rebuild file %s: %w", name, err)
		}
	}
	return nil
}

// buildMirror builds a complete DuckDB mirror in the rebuild file inside
// the mirror's work directory, then atomically swaps it over path. It is
// the only way a schema v9 mirror is created or repaired: unlike Sync.Push,
// it never touches an existing mirror file in place, so a rebuild that
// fails at any point leaves the previous mirror (if any) fully intact. The
// caller holds the rebuild lock from claimRebuild.
//
// A rebuild that fails or is killed keeps its file. The next rebuild for
// the same archive, scope, machine, and formats reopens it and pushes only
// the sessions it is missing or holds stale, so repeated interruptions
// still converge. run is recorded in the file, so a later push can tell
// whether the file can continue and report why the rebuild started.
func buildMirror(
	ctx context.Context, path string, local *db.DB, machine string,
	opts storage.MirrorPushOptions, run rebuildRun,
	onProgress func(storage.MirrorPushProgress),
) (storage.MirrorPushResult, error) {
	file := rebuildFilePath(path)

	s, err := New(ctx, file, local, machine, opts)
	if err != nil {
		if isMirrorHeldError(err) {
			return storage.MirrorPushResult{}, rebuildInProgressError(path, err)
		}
		return storage.MirrorPushResult{}, err
	}
	result, buildErr := buildMirrorInto(ctx, s, opts, run, onProgress)
	if closeErr := s.Close(); closeErr != nil && buildErr == nil {
		buildErr = fmt.Errorf("closing duckdb rebuild file: %w", closeErr)
	}
	if buildErr != nil {
		return result, buildErr
	}

	wantSessions := result.SessionsPushed + result.Diagnostics.SkippedUnchangedSessions.Total
	if err := validateBuiltMirror(ctx, file, wantSessions); err != nil {
		if ctx.Err() != nil {
			return result, err
		}
		return result, errors.Join(err, removeRebuildFile(file))
	}
	if err := swapMirrorFile(file, path); err != nil {
		return result, err
	}
	result.Diagnostics.Full = true
	return result, nil
}

// staleTempFileAge is how old a work-directory .tmp-<digits> rebuild temp
// file must be before sweepStaleTempFiles removes it. Rebuilds now write the
// fixed-name rebuild file instead (see rebuildFilePath), so these files only
// come from older agentsview builds that crashed or were killed mid-rebuild.
//
// The age is a heuristic, not proof of liveness: pushes are not serialized
// across processes, so this sweep could in principle run while an older
// build's rebuild has genuinely been in progress for longer than this
// threshold (a very large archive, a slow disk) and race-remove its still-
// live temp file. 24 hours makes that window large enough that hitting it
// in practice would already be a surprising rebuild duration on its own.
// The worst case if it does happen is bounded and self-healing: the
// in-progress rebuild's own rename fails with an actionable "temp file
// missing" error, that one push attempt fails, and the caller retries — the
// existing mirror file is never touched (buildMirror never writes to it
// in place), so there is no risk of corruption, only a failed push.
const staleTempFileAge = 24 * time.Hour

// sweepStaleTempFiles removes <base>.tmp-<digits> rebuild temp files older
// than staleTempFileAge from the mirror's work directory. It never matches
// the rebuild file, which the next rebuild resumes or discards itself.
// Always safe to call at the start of a push, even before ProbeMirror has recognized the
// destination as ours: the work directory's name is itself the ownership
// marker (see mirrorWorkDirSuffix), so nothing outside it — in particular
// no sibling of the mirror, whatever its name — is ever deleted. A missing
// work directory means nothing was ever generated here; the sweep returns
// nil without creating it.
//
// Names are matched by literal prefix over os.ReadDir instead of
// filepath.Glob: path is interpolated into a glob pattern, and glob
// metacharacters ([, ?, *) in a project or archive directory name would
// otherwise be interpreted as glob syntax instead of literal characters,
// silently breaking or over-matching the sweep. The suffix after the
// prefix must be entirely ASCII digits — the exact shape older builds'
// os.CreateTemp "*" expansion generated — so any
// other file inside the work directory survives, and the directory itself
// is never removed.
func sweepStaleTempFiles(path string) error {
	workDir := mirrorWorkDirPath(path)
	prefix := filepath.Base(path) + ".tmp-"
	entries, err := os.ReadDir(workDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading duckdb mirror work directory %s: %w", workDir, err)
	}
	cutoff := time.Now().Add(-staleTempFileAge)
	for _, entry := range entries {
		if !entry.Type().IsRegular() ||
			!isGeneratedSweepName(entry.Name(), prefix) {
			continue
		}
		m := filepath.Join(workDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("statting duckdb mirror temp file %s: %w", m, err)
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(m); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing stale duckdb mirror temp file %s: %w", m, err)
		}
	}
	return nil
}

// isGeneratedSweepName reports whether name is prefix followed by a
// non-empty run of ASCII digits — the only shape the stale-file sweeps
// (sweepStaleTempFiles and SweepStaleMirrorReopenAliases) ever generate:
// os.CreateTemp expands its "*" to decimal digits and reopen aliases append
// time.Now().UnixNano(). The sweeps already confine themselves to the
// mirror's work directory; the shape check is a second guard so that even
// inside that directory only generated artifacts are ever removed.
func isGeneratedSweepName(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	suffix := name[len(prefix):]
	if suffix == "" {
		return false
	}
	for i := range len(suffix) {
		if suffix[i] < '0' || suffix[i] > '9' {
			return false
		}
	}
	return true
}

// rebuildSnapshot captures the mirror metadata state tokens that must be
// read BEFORE pushEverything enumerates sessions. A session mutated or
// hard-deleted while the rebuild's session push loop is still running must
// produce a sync_marker (or deletion journal revision) strictly greater
// than these captured values, or the very next incremental push would never
// select it: the mirror would silently keep stale or deleted data until the
// next --full rebuild.
//
// The project identity revision does not need pre-capture here:
// syncProjectIdentityObservations reads ProjectIdentityPublicationRevision
// as the first thing it does and returns that same revision, so its return
// value already reflects the state as of that read, before its own
// publication writes happen. Callers just need to use that return value
// instead of re-reading the revision after the fact.
type rebuildSnapshot struct {
	cutoff           string
	deletionRevision int64
	// sourceDatabaseID identifies the archive generation this rebuild reads
	// from (see mirrorMetadata.SourceDatabaseID). It cannot change while the
	// rebuild's *db.DB handle is open, so capturing it here is a convenience
	// rather than a race guard.
	sourceDatabaseID string
	// sourceArchiveID is the stable provenance identity stamped onto every
	// mirrored row. A repair can change it without changing database_id.
	sourceArchiveID string
}

// captureRebuildSnapshot reads the state tokens buildMirror needs to seed
// post-rebuild mirror metadata. It must be called before the rebuild lists
// sessions to push; see rebuildSnapshot for why.
func captureRebuildSnapshot(ctx context.Context, local *db.DB) (rebuildSnapshot, error) {
	deletionRevision, err := local.SessionDeletionPublicationRevision(ctx)
	if err != nil {
		return rebuildSnapshot{}, err
	}
	sourceDatabaseID, err := local.GetDatabaseID(ctx)
	if err != nil {
		return rebuildSnapshot{}, fmt.Errorf(
			"reading local archive database id: %w", err,
		)
	}
	sourceArchiveID, err := local.GetArchiveID(ctx)
	if err != nil {
		return rebuildSnapshot{}, fmt.Errorf(
			"reading local archive id: %w", err,
		)
	}
	return rebuildSnapshot{
		cutoff:           time.Now().UTC().Format(localSyncTimestampLayout),
		deletionRevision: deletionRevision,
		sourceDatabaseID: sourceDatabaseID,
		sourceArchiveID:  sourceArchiveID,
	}, nil
}

// buildMirrorInto creates schema v9 on a fresh Sync's DuckDB file, pushes
// every in-scope session plus the mirror's global tables, records mirror
// metadata, and checkpoints so the on-disk file reflects every write.
//
// It owns start-to-finish timing for storage.MirrorPushResult.Duration rather than
// letting pushEverything set it: identity publication and the metadata
// write both happen after pushEverything returns, so a Duration captured
// inside pushEverything alone would underreport a --full push's real wall
// time by everything after the session push loop.
func buildMirrorInto(
	ctx context.Context, s *Sync, opts storage.MirrorPushOptions, run rebuildRun,
	onProgress func(storage.MirrorPushProgress),
) (storage.MirrorPushResult, error) {
	start := time.Now()
	if err := createSchema(ctx, s.duck); err != nil {
		return storage.MirrorPushResult{}, err
	}
	encodedRun, err := json.Marshal(run)
	if err != nil {
		return storage.MirrorPushResult{}, fmt.Errorf("encoding duckdb rebuild run: %w", err)
	}
	if err := recordMetadataKey(ctx, s.duck, rebuildRunMetadataKey, string(encodedRun)); err != nil {
		return storage.MirrorPushResult{}, err
	}
	snapshot, err := captureRebuildSnapshot(ctx, s.local)
	if err != nil {
		return storage.MirrorPushResult{}, err
	}
	result, err := s.pushEverything(ctx, onProgress)
	if err != nil {
		return result, err
	}
	if result.Errors > 0 {
		run.Errored = true
		encodedRun, err := json.Marshal(run)
		if err != nil {
			return result, fmt.Errorf("encoding duckdb rebuild run: %w", err)
		}
		if err := recordMetadataKey(ctx, s.duck, rebuildRunMetadataKey, string(encodedRun)); err != nil {
			return result, err
		}
		return result, fmt.Errorf(
			"rebuild failed with %d session push errors", result.Errors,
		)
	}
	identityRevision, err := s.syncProjectIdentityObservations(ctx, 0, true, nil)
	if err != nil {
		return result, err
	}
	mappingRevision, err := s.syncWorktreeMappings(ctx, 0, true)
	if err != nil {
		return result, err
	}
	scope := db.CanonicalPushScope(opts.Projects, opts.ExcludeProjects)
	if err := s.writeRebuildMetadata(
		ctx, scope, snapshot, identityRevision, mappingRevision,
	); err != nil {
		return result, err
	}
	if _, err := s.duck.ExecContext(ctx, "CHECKPOINT"); err != nil {
		return result, fmt.Errorf("checkpointing duckdb rebuild: %w", err)
	}
	result.Duration = time.Since(start)
	return result, nil
}

// pushEverything brings a rebuild file to every session in scope plus the
// mirror's global tables (pricing, cursor usage, curation rows). It reads no
// push watermarks: buildMirror is the only caller. The file is empty on a
// fresh rebuild. A resumed rebuild's file already holds the sessions the
// interrupted one committed: those whose fingerprints still match are kept,
// and those no longer in the listing are removed. Project identity
// publication is not done here: buildMirrorInto runs it separately, after pushEverything
// succeeds, so it can capture the revision syncProjectIdentityObservations
// returns without changing this function's signature.
func (s *Sync) pushEverything(
	ctx context.Context, onProgress func(storage.MirrorPushProgress),
) (storage.MirrorPushResult, error) {
	var result storage.MirrorPushResult
	if err := s.ensureArchiveID(ctx); err != nil {
		return result, err
	}
	if err := s.syncMachineMetadata(ctx); err != nil {
		return result, err
	}
	if err := s.syncModelPricing(ctx); err != nil {
		return result, err
	}
	if err := s.syncCursorUsageEvents(ctx); err != nil {
		return result, err
	}

	sessions, err := s.local.ListSessionsForMirrorWindow(
		ctx, "", s.projects, s.excludeProjects,
	)
	if err != nil {
		return result, fmt.Errorf("listing sessions for duckdb rebuild: %w", err)
	}
	db.SortSessionsNewestFirst(sessions)
	result.Diagnostics.LocalSessionCount = len(sessions)
	result.Diagnostics.CandidateSessions = countPushSessions(sessions)
	if err := s.deleteUnlistedMirrorSessions(ctx, sessions, &result); err != nil {
		return result, err
	}

	changed, unchanged, fingerprints, err := s.selectChangedSessions(ctx, sessions)
	if err != nil {
		return result, err
	}
	result.Diagnostics.SkippedUnchangedSessions = countPushSessions(unchanged)

	pushed := make([]db.Session, 0, len(changed))
	for batchStart := 0; batchStart < len(changed); batchStart += duckSessionPushBatchSize {
		end := min(batchStart+duckSessionPushBatchSize, len(changed))
		if err := s.pushSessionBatchForMode(
			ctx, changed[batchStart:end], batchStart, len(changed),
			&result, &pushed, onProgress, fingerprints,
		); err != nil {
			return result, err
		}
	}
	result.Diagnostics.PushedSessions = countPushSessions(pushed)

	if result.Errors == 0 {
		// One snapshot feeds both the fingerprint and the copy (see
		// curationSnapshot), and the recorded fingerprint covers what
		// replaceCuration actually WROTE, so it always describes exactly
		// the mirror's curation state: a star/pin change landing during
		// the rebuild either made it into the snapshot (mirrored and
		// fingerprinted) or lands after it (mismatches on the next push,
		// refreshing again). No interleaving can strand the mirror behind
		// a matching fingerprint.
		//
		// replaceCuration validates curation session IDs against the mirror's
		// own sessions table, which at this point holds exactly the batches
		// the loop above committed, so rebuilds and incremental pushes share
		// one curation write path.
		snap, err := s.loadCurationSnapshot(ctx)
		if err != nil {
			return result, err
		}
		written, err := s.replaceCuration(ctx, snap)
		if err != nil {
			return result, err
		}
		if err := recordMetadataKey(
			ctx, s.duck, curationFingerprintMetadataKey, written,
		); err != nil {
			return result, err
		}
		result.Diagnostics.CurationRefreshed = true
	}

	return result, nil
}

// deleteUnlistedMirrorSessions removes the rebuild file's sessions that are
// not in listed: sessions an interrupted rebuild committed that were deleted
// or left the push scope before this one resumed it.
func (s *Sync) deleteUnlistedMirrorSessions(
	ctx context.Context, listed []db.Session, result *storage.MirrorPushResult,
) error {
	ids, err := s.mirrorSessionIDs(ctx)
	if err != nil {
		return err
	}
	keep := make(map[string]bool, len(listed))
	for _, sess := range listed {
		keep[sess.ID] = true
	}
	var stale []string
	for _, id := range ids {
		if !keep[id] {
			stale = append(stale, id)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	if err := s.withDuckTx(ctx, "delete sessions missing from the archive", func(tx *sql.Tx) error {
		for _, id := range stale {
			if err := s.deleteMirrorSession(ctx, tx, id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	result.Diagnostics.DeletedStaleSessions += len(stale)
	return nil
}

// mirrorSessionIDs returns the IDs of every session in the mirror file.
func (s *Sync) mirrorSessionIDs(ctx context.Context) ([]string, error) {
	rows, err := s.duck.QueryContext(ctx, `SELECT id FROM sessions`)
	if err != nil {
		return nil, fmt.Errorf("reading duckdb rebuild sessions: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning duckdb rebuild session: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading duckdb rebuild sessions: %w", err)
	}
	return ids, nil
}

// writeRebuildMetadata records the mirrorMetadata a probe reads back:
// schema/data version, push scope, cutoff/machine bookkeeping, and the
// source revisions needed to detect deletions and identity changes that
// happen after this rebuild. cutoff and deletionRevision come from
// snapshot, captured before pushEverything enumerated sessions (see
// rebuildSnapshot); identityRevision comes from
// syncProjectIdentityObservations's return value, which is already
// as-of-its-own-read and needs no pre-capture. Re-reading either token
// here, after the push loop has run, would let a session mutated or
// hard-deleted during the rebuild fall permanently outside the next
// incremental push's window.
func (s *Sync) writeRebuildMetadata(
	ctx context.Context, scope string, snapshot rebuildSnapshot,
	identityRevision, mappingRevision int64,
) error {
	return writeMirrorMetadata(ctx, s.duck, mirrorMetadata{
		SchemaVersion:    SchemaVersion,
		DataVersion:      db.CurrentDataVersion(),
		SourceDatabaseID: snapshot.sourceDatabaseID,
		SourceArchiveID:  snapshot.sourceArchiveID,
		Scope:            scope,
		LastPushCutoff:   snapshot.cutoff,
		LastPushAt:       time.Now().UTC().Format(time.RFC3339),
		LastPushMachine:  s.machine,
		DeletionRevision: snapshot.deletionRevision,
		IdentityRevision: identityRevision,
		MappingRevision:  mappingRevision,
	})
}

// validateBuiltMirror re-probes the freshly built (and now closed) rebuild
// file read-only before it is swapped into place, so a mirror that failed
// to write its own metadata or lost rows never replaces a working one.
func validateBuiltMirror(ctx context.Context, tmpPath string, wantSessions int) error {
	probe, err := ProbeMirror(ctx, tmpPath)
	if err != nil {
		return fmt.Errorf("validating rebuilt duckdb mirror: %w", err)
	}
	if !probe.ShapeOK {
		return fmt.Errorf(
			"rebuilt duckdb mirror failed validation: %s", probe.ShapeIssue,
		)
	}
	if probe.SchemaVersion != SchemaVersion {
		return fmt.Errorf(
			"rebuilt duckdb mirror has schema version %d, want %d",
			probe.SchemaVersion, SchemaVersion,
		)
	}
	conn, err := OpenReadOnly(ctx, tmpPath)
	if err != nil {
		return fmt.Errorf("validating rebuilt duckdb mirror: %w", err)
	}
	defer func() { _ = conn.Close() }()
	var count int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil {
		return fmt.Errorf("counting rebuilt duckdb mirror sessions: %w", err)
	}
	if count != wantSessions {
		return fmt.Errorf(
			"rebuilt duckdb mirror has %d sessions, want %d", count, wantSessions,
		)
	}
	return nil
}

// swapMirrorFile atomically replaces dstPath with tmpPath. tmpPath, the
// rebuild file, lives in
// the mirror's work directory, a subdirectory of dstPath's own parent, so
// the rename never crosses a volume boundary. On Windows atomicfile.Replace
// renames with POSIX semantics, so the swap succeeds while 'agentsview duckdb
// serve' has the mirror open: DuckDB opens files with FILE_SHARE_DELETE, and
// the server keeps reading the old file until WatchMirrorReplacement reopens
// it, as on Unix. A handle opened without delete sharing still blocks the
// rename; the retry loop covers one that is held briefly. dstPath is left
// untouched on every failed attempt because rename is atomic: there is no
// partial state where the mirror is half-replaced.
func swapMirrorFile(tmpPath, dstPath string) error {
	var err error
	for attempt := range 5 {
		if err = atomicfile.Replace(tmpPath, dstPath); err == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
	}
	return fmt.Errorf("replacing duckdb mirror %s: %w; if another program has "+
		"this file open, close it and re-run the push", dstPath, err)
}
