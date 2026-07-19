package duckdb

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// rebuildMirror builds a fresh DuckDB mirror file from scratch in a
// temporary file next to path, then atomically swaps it into place. It is
// the only way a schema v3 mirror is created or repaired: unlike Sync.Push,
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

// createMirrorTempPath reserves a temp file name next to path and removes
// it immediately: DuckDB must create the file itself (os.CreateTemp leaves
// behind an empty file DuckDB's Open would otherwise try to reuse as a
// zero-byte database).
func createMirrorTempPath(path string) (string, error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
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

// staleTempFileAge is how old a path.tmp-* rebuild temp file must be before
// sweepStaleTempFiles removes it. A running rebuild's own temp file is
// always younger than this, so the guard only ever catches leftovers from a
// process that crashed or was killed mid-rebuild (see createMirrorTempPath
// and rebuildMirror's deferred cleanup, which only fires for that process's
// own file and never runs at all if the process is killed outright).
const staleTempFileAge = time.Hour

// sweepStaleTempFiles removes path.tmp-* rebuild temp files older than
// staleTempFileAge. Always safe to call at the start of a push: a fresh
// rebuild creates its own temp file after this runs, so it can never sweep
// up a file it is about to use.
func sweepStaleTempFiles(path string) error {
	matches, err := filepath.Glob(path + ".tmp-*")
	if err != nil {
		return fmt.Errorf("globbing duckdb mirror temp files: %w", err)
	}
	cutoff := time.Now().Add(-staleTempFileAge)
	for _, m := range matches {
		info, err := os.Stat(m)
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
}

// captureRebuildSnapshot reads the state tokens rebuildMirror needs to seed
// post-rebuild mirror metadata. It must be called before the rebuild lists
// sessions to push; see rebuildSnapshot for why.
func captureRebuildSnapshot(ctx context.Context, local *db.DB) (rebuildSnapshot, error) {
	deletionRevision, err := local.SessionDeletionPublicationRevision(ctx)
	if err != nil {
		return rebuildSnapshot{}, err
	}
	return rebuildSnapshot{
		cutoff:           time.Now().UTC().Format(localSyncTimestampLayout),
		deletionRevision: deletionRevision,
	}, nil
}

// buildMirrorInto creates schema v3 on a fresh Sync's DuckDB file, pushes
// every in-scope session plus the mirror's global tables, records mirror
// metadata, and checkpoints so the on-disk file reflects every write.
func buildMirrorInto(
	ctx context.Context, s *Sync, opts SyncOptions, onProgress func(PushProgress),
) (PushResult, error) {
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
	scope := canonicalPushScope(opts.Projects, opts.ExcludeProjects)
	if err := s.writeRebuildMetadata(ctx, scope, snapshot, identityRevision); err != nil {
		return result, err
	}
	if _, err := s.duck.ExecContext(ctx, "CHECKPOINT"); err != nil {
		return result, fmt.Errorf("checkpointing duckdb rebuild: %w", err)
	}
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
	start := time.Now()
	var result PushResult
	if err := s.syncModelPricing(ctx); err != nil {
		return result, err
	}
	if err := s.syncCursorUsageEvents(ctx); err != nil {
		return result, err
	}

	sessions, err := s.local.ListSessionsForMirrorWindow(
		ctx, "", "", s.projects, s.excludeProjects,
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
		if err := s.pushEverythingCuration(ctx, sessions); err != nil {
			return result, err
		}
		fingerprint, err := s.curationFingerprint(ctx)
		if err != nil {
			return result, err
		}
		if err := recordMetadataKey(
			ctx, s.duck, curationFingerprintMetadataKey, fingerprint,
		); err != nil {
			return result, err
		}
		result.Diagnostics.CurationRefreshed = true
	}

	result.Duration = time.Since(start)
	return result, nil
}

func (s *Sync) pushEverythingCuration(ctx context.Context, sessions []db.Session) error {
	ids := sessionIDs(sessions)
	return s.withDuckTx(ctx, "replace curation rows", func(tx *sql.Tx) error {
		if err := s.replaceAllPinnedMessages(ctx, tx, ids); err != nil {
			return err
		}
		return s.replaceStarredSessions(ctx, tx, ids)
	})
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
	ctx context.Context, scope string, snapshot rebuildSnapshot, identityRevision int64,
) error {
	return writeMirrorMetadata(ctx, s.duck, mirrorMetadata{
		SchemaVersion:    SchemaVersion,
		DataVersion:      db.CurrentDataVersion(),
		Scope:            scope,
		LastPushCutoff:   snapshot.cutoff,
		LastPushAt:       time.Now().UTC().Format(time.RFC3339),
		LastPushMachine:  s.machine,
		DeletionRevision: snapshot.deletionRevision,
		IdentityRevision: identityRevision,
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
	conn, err := openReadOnlyMirror(tmpPath)
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

// swapMirrorFile atomically replaces dstPath with tmpPath. POSIX rename
// over an existing file succeeds on the first attempt; the retry loop
// exists for platforms (Windows) where another process briefly holding the
// destination open causes a sharing violation. dstPath is left untouched on
// every failed attempt because rename is atomic: there is no partial state
// where the mirror is half-replaced.
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
