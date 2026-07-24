package duckdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// mirrorWorkDirSuffix is appended to the mirror path to form the mirror's
// private work directory, which holds every generated artifact: rebuild
// temp files (createMirrorTempPath) and reopen hardlink aliases
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
// a rebuild creating its temp file, a serve process hardlinking a reopen
// alias — never from probe/status/sweep paths, which must stay create-free.
//
// The directory must be private: rebuild temp files and reopen aliases
// live here, and the temp-file flow unlinks a reserved name before DuckDB
// reopens it (see createMirrorTempPath), so anyone who can write in this
// directory could swap in a symlink mid-rebuild or replace the temp file
// before the final rename. A fresh directory is created 0700, and an
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

// rebuildMirror builds a fresh DuckDB mirror file from scratch in a
// temporary file inside the mirror's work directory, then atomically swaps
// it over path. It is
// the only way a schema v4 mirror is created or repaired: unlike Sync.Push,
// it never touches an existing mirror file in place, so a rebuild that
// fails at any point leaves the previous mirror (if any) fully intact.
func rebuildMirror(
	ctx context.Context, path string, local *db.DB, machine string,
	opts SyncOptions, onProgress func(PushProgress),
) (PushResult, error) {
	tmpPath, err := createMirrorTempPath(path)
	if err != nil {
		return PushResult{}, err
	}
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmpPath)
		}
	}()

	s, err := New(tmpPath, local, machine, opts)
	if err != nil {
		return PushResult{}, err
	}
	result, buildErr := buildMirrorInto(ctx, s, opts, onProgress)
	if closeErr := s.Close(); closeErr != nil && buildErr == nil {
		buildErr = fmt.Errorf("closing duckdb rebuild file: %w", closeErr)
	}
	if buildErr != nil {
		return result, buildErr
	}

	if err := validateBuiltMirror(ctx, tmpPath, result.SessionsPushed); err != nil {
		return result, err
	}
	if err := swapMirrorFile(tmpPath, path); err != nil {
		return result, err
	}
	success = true
	result.Diagnostics.Full = true
	return result, nil
}

// createMirrorTempPath reserves a temp file name inside the mirror's work
// directory (created here, lazily, because a rebuild is now actually
// starting — probe/status paths never reach this) and removes the file
// immediately: DuckDB must create the file itself (os.CreateTemp leaves
// behind an empty file DuckDB's Open would otherwise try to reuse as a
// zero-byte database). The unlink-to-reopen window is safe because
// ensureMirrorWorkDir guarantees the directory is writable only by the
// current user — nothing else can swap a symlink onto the reserved name.
func createMirrorTempPath(path string) (string, error) {
	workDir, err := ensureMirrorWorkDir(path)
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(workDir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("creating duckdb rebuild temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("closing duckdb rebuild temp file: %w", err)
	}
	if err := os.Remove(tmpPath); err != nil {
		return "", fmt.Errorf("clearing duckdb rebuild temp file: %w", err)
	}
	return tmpPath, nil
}

// staleTempFileAge is how old a work-directory .tmp-<digits> rebuild temp
// file must be before
// sweepStaleTempFiles removes it. A running rebuild's own temp file is
// always younger than this, so the guard only ever catches leftovers from a
// process that crashed or was killed mid-rebuild (see createMirrorTempPath
// and rebuildMirror's deferred cleanup, which only fires for that process's
// own file and never runs at all if the process is killed outright).
//
// The age is a heuristic, not proof of liveness: pushes are not serialized
// across processes, so a second push's sweep could in principle run while a
// first push's rebuild has genuinely been in progress for longer than this
// threshold (a very large archive, a slow disk) and race-remove its still-
// live temp file. 24 hours makes that window large enough that hitting it
// in practice would already be a surprising rebuild duration on its own.
// The worst case if it does happen is bounded and self-healing: the
// in-progress rebuild's own rename fails with an actionable "temp file
// missing" error, that one push attempt fails, and the caller retries — the
// existing mirror file is never touched (rebuildMirror never writes to it
// in place), so there is no risk of corruption, only a failed push.
const staleTempFileAge = 24 * time.Hour

// sweepStaleTempFiles removes <base>.tmp-<digits> rebuild temp files older
// than staleTempFileAge from the mirror's work directory. Always safe to
// call at the start of a push, even before ProbeMirror has recognized the
// destination as ours: the work directory's name is itself the ownership
// marker (see mirrorWorkDirSuffix), so nothing outside it — in particular
// no sibling of the mirror, whatever its name — is ever deleted. A missing
// work directory means nothing was ever generated here; the sweep returns
// nil without creating it. A fresh rebuild creates its own temp file after
// this runs, so the sweep can never remove a file it is about to use.
//
// Names are matched by literal prefix over os.ReadDir instead of
// filepath.Glob: path is interpolated into a glob pattern, and glob
// metacharacters ([, ?, *) in a project or archive directory name would
// otherwise be interpreted as glob syntax instead of literal characters,
// silently breaking or over-matching the sweep. The suffix after the
// prefix must be entirely ASCII digits — the exact shape
// createMirrorTempPath's os.CreateTemp "*" expansion generates — so any
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
	for i := 0; i < len(suffix); i++ {
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
}

// captureRebuildSnapshot reads the state tokens rebuildMirror needs to seed
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
	return rebuildSnapshot{
		cutoff:           time.Now().UTC().Format(localSyncTimestampLayout),
		deletionRevision: deletionRevision,
		sourceDatabaseID: sourceDatabaseID,
	}, nil
}

// buildMirrorInto creates schema v4 on a fresh Sync's DuckDB file, pushes
// every in-scope session plus the mirror's global tables, records mirror
// metadata, and checkpoints so the on-disk file reflects every write.
//
// It owns start-to-finish timing for PushResult.Duration rather than
// letting pushEverything set it: identity publication and the metadata
// write both happen after pushEverything returns, so a Duration captured
// inside pushEverything alone would underreport a --full push's real wall
// time by everything after the session push loop.
func buildMirrorInto(
	ctx context.Context, s *Sync, opts SyncOptions, onProgress func(PushProgress),
) (PushResult, error) {
	start := time.Now()
	if err := createSchema(ctx, s.duck); err != nil {
		return PushResult{}, err
	}
	snapshot, err := captureRebuildSnapshot(ctx, s.local)
	if err != nil {
		return PushResult{}, err
	}
	result, err := s.pushEverything(ctx, onProgress)
	if err != nil {
		return result, err
	}
	if result.Errors > 0 {
		return result, fmt.Errorf(
			"rebuild failed with %d session push errors", result.Errors,
		)
	}
	identityRevision, err := s.syncProjectIdentityObservations(ctx, 0, true)
	if err != nil {
		return result, err
	}
	mappingRevision, err := s.syncWorktreeMappings(ctx, 0, true)
	if err != nil {
		return result, err
	}
	scope := canonicalPushScope(opts.Projects, opts.ExcludeProjects)
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

// pushEverything performs a full-only push of every session in scope plus
// the mirror's global tables (pricing, cursor usage, curation rows). Unlike
// Sync.Push it never computes incremental fingerprint diffs or reads/writes
// push watermarks: rebuildMirror is the only caller, and it always starts
// from an empty freshly created file. Project identity publication is not
// done here: buildMirrorInto runs it separately, after pushEverything
// succeeds, so it can capture the revision syncProjectIdentityObservations
// returns without changing this function's signature.
func (s *Sync) pushEverything(
	ctx context.Context, onProgress func(PushProgress),
) (PushResult, error) {
	var result PushResult
	if err := s.ensureArchiveID(ctx); err != nil {
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
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ID < sessions[j].ID
	})
	result.Diagnostics.LocalSessionCount = len(sessions)
	result.Diagnostics.CandidateSessions = countPushSessions(sessions)

	fingerprints, err := s.sessionFingerprints(ctx, sessions)
	if err != nil {
		return result, err
	}

	pushed := make([]db.Session, 0, len(sessions))
	for batchStart := 0; batchStart < len(sessions); batchStart += duckSessionPushBatchSize {
		end := min(batchStart+duckSessionPushBatchSize, len(sessions))
		if err := s.pushSessionBatchForMode(
			ctx, sessions[batchStart:end], batchStart, len(sessions),
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
		Scope:            scope,
		LastPushCutoff:   snapshot.cutoff,
		LastPushAt:       time.Now().UTC().Format(time.RFC3339),
		LastPushMachine:  s.machine,
		DeletionRevision: snapshot.deletionRevision,
		IdentityRevision: identityRevision,
		MappingRevision:  mappingRevision,
	})
}

// validateBuiltMirror re-probes the freshly built (and now closed) temp
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
	conn, err := OpenReadOnly(tmpPath)
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

// swapMirrorFile atomically replaces dstPath with tmpPath. tmpPath lives in
// the mirror's work directory, a subdirectory of dstPath's own parent, so
// the rename never crosses a volume boundary: it stays a same-filesystem
// rename and remains atomic on POSIX and Windows exactly as a
// sibling-to-sibling rename would. POSIX rename over an existing file
// succeeds on the first attempt; the retry loop exists for platforms
// (Windows) where another process briefly holding the destination open
// causes a sharing violation. dstPath is left untouched on every failed
// attempt because rename is atomic: there is no partial state where the
// mirror is half-replaced.
func swapMirrorFile(tmpPath, dstPath string) error {
	var err error
	for attempt := range 5 {
		if err = os.Rename(tmpPath, dstPath); err == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
	}
	return fmt.Errorf("replacing duckdb mirror %s: %w; if 'agentsview duckdb serve' "+
		"or 'agentsview duckdb quack serve' is running against this file, stop it "+
		"and re-run the push", dstPath, err)
}
