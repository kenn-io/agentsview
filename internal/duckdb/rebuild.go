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

// buildMirrorInto creates schema v3 on a fresh Sync's DuckDB file, pushes
// every in-scope session plus the mirror's global tables, records mirror
// metadata, and checkpoints so the on-disk file reflects every write.
func buildMirrorInto(
	ctx context.Context, s *Sync, opts SyncOptions, onProgress func(PushProgress),
) (PushResult, error) {
	if err := createSchema(ctx, s.duck); err != nil {
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
	scope := canonicalPushScope(opts.Projects, opts.ExcludeProjects)
	if err := s.writeRebuildMetadata(ctx, scope); err != nil {
		return result, err
	}
	if _, err := s.duck.ExecContext(ctx, "CHECKPOINT"); err != nil {
		return result, fmt.Errorf("checkpointing duckdb rebuild: %w", err)
	}
	return result, nil
}

// pushEverything performs a full-only push of every session in scope plus
// the mirror's global tables (pricing, cursor usage, curation rows, project
// identity). Unlike Sync.Push it never computes incremental fingerprint
// diffs or reads/writes push watermarks: rebuildMirror is the only caller,
// and it always starts from an empty freshly created file.
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
		if _, err := s.syncProjectIdentityObservations(ctx, 0, true); err != nil {
			return result, err
		}
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
// happen after this rebuild.
func (s *Sync) writeRebuildMetadata(ctx context.Context, scope string) error {
	identityRevision, err := s.local.ProjectIdentityPublicationRevision(ctx)
	if err != nil {
		return err
	}
	deletionRevision, err := s.local.SessionDeletionPublicationRevision(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	return writeMirrorMetadata(ctx, s.duck, mirrorMetadata{
		SchemaVersion:    SchemaVersion,
		DataVersion:      db.CurrentDataVersion(),
		Scope:            scope,
		LastPushCutoff:   now.Format(localSyncTimestampLayout),
		LastPushAt:       now.Format(time.RFC3339),
		LastPushMachine:  s.machine,
		DeletionRevision: deletionRevision,
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
