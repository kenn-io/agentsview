package duckdb

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

// syncWorktreeMappings publishes worktree mapping metadata to the DuckDB
// mirror up through the local archive's current revision, using
// priorRevision (normally probe.MappingRevision) as the cursor, following
// the identity publication contract of syncProjectIdentityObservations:
// one transaction, archive-scoped full rebuilds, tombstoned deltas, and a
// mirror-resident cursor persisted by the caller only after the push
// succeeds. Mappings are deliberately published in full regardless of any
// project push filter: rules are per-machine path prefixes, not
// per-project rows, so scope filtering does not apply to them. It returns
// the revision just published so the caller can persist it as mirror
// metadata's MappingRevision.
func (s *Sync) syncWorktreeMappings(
	ctx context.Context, priorRevision int64, force bool,
) (int64, error) {
	if err := s.ensureArchiveID(ctx); err != nil {
		return 0, err
	}
	revision, err := s.local.WorktreeMappingPublicationRevision(ctx)
	if err != nil {
		return 0, err
	}

	fullPublication := force || priorRevision <= 0 || priorRevision > revision
	if !fullPublication && priorRevision == revision {
		return revision, nil
	}

	var mappings []db.WorktreeProjectMapping
	var deletes []db.WorktreeMappingKey
	if fullPublication {
		mappings, err = s.local.ListAllWorktreeProjectMappings(ctx)
		if err != nil {
			return 0, err
		}
	} else {
		delta, err := s.local.LoadWorktreeMappingPublicationDelta(
			ctx, priorRevision, revision)
		if err != nil {
			return 0, err
		}
		mappings, deletes = delta.Mappings, delta.Deletes
	}

	if err := s.commitWorktreeMappingPublication(
		ctx, fullPublication, mappings, deletes,
	); err != nil {
		return 0, err
	}
	return revision, nil
}

// commitWorktreeMappingPublication writes one publication window (a full
// archive-scoped rebuild or a tombstoned delta) to the DuckDB mirror in a
// single transaction, routing mutations through the same execMutation
// executor identity sync uses.
func (s *Sync) commitWorktreeMappingPublication(
	ctx context.Context,
	fullPublication bool,
	mappings []db.WorktreeProjectMapping,
	deletes []db.WorktreeMappingKey,
) error {
	tx, err := s.duck.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning duckdb mapping publication tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	exec := func(stmt string, args ...any) error {
		return s.execMutation(ctx, tx, stmt, args...)
	}

	if fullPublication {
		if err := exec(`
			DELETE FROM source_worktree_project_mappings
			WHERE source_archive_id = ?`, s.archiveID); err != nil {
			return fmt.Errorf("clearing duckdb mapping mirror scope: %w", err)
		}
	} else {
		for _, key := range deletes {
			if err := exec(`
				DELETE FROM source_worktree_project_mappings
				WHERE source_archive_id = ?
				  AND machine = ? AND path_prefix = ?`,
				s.archiveID, key.Machine, key.PathPrefix); err != nil {
				return fmt.Errorf("deleting duckdb mapping tombstone: %w", err)
			}
		}
	}
	for _, m := range mappings {
		if err := exec(`
			INSERT INTO source_worktree_project_mappings
			(source_archive_id, machine, path_prefix, layout, project,
			 original_project, enabled, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (source_archive_id, machine, path_prefix)
			DO UPDATE SET
				layout = excluded.layout,
				project = excluded.project,
				original_project = excluded.original_project,
				enabled = excluded.enabled,
				updated_at = excluded.updated_at`,
			s.archiveID, m.Machine, m.PathPrefix, m.Layout, m.Project,
			m.OriginalProject, m.Enabled, m.UpdatedAt); err != nil {
			return fmt.Errorf("upserting duckdb mapping mirror row: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing duckdb mapping publication: %w", err)
	}
	return nil
}
