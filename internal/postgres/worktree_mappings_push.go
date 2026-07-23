package postgres

import (
	"context"
	"fmt"
	"strconv"

	"go.kenn.io/agentsview/internal/db"
)

const worktreeMappingPublicationStateKey = "worktree_mapping_publication_revision_v1"

// syncWorktreeMappings publishes worktree mapping metadata to the mirror. It
// follows the identity publication contract used by
// syncProjectIdentityObservations: one transaction, archive-scoped full
// rebuilds, tombstoned deltas, and a cursor that advances only after commit.
func (s *Sync) syncWorktreeMappings(ctx context.Context, force bool) error {
	revision, err := s.local.WorktreeMappingPublicationRevision(ctx)
	if err != nil {
		return err
	}
	databaseGeneration, err := s.local.GetDatabaseID(ctx)
	if err != nil {
		return fmt.Errorf("reading database generation: %w", err)
	}
	state := s.effectiveSyncState()
	stateKey := worktreeMappingPublicationStateKey + ":" + databaseGeneration
	publishedValue, err := state.GetSyncState(stateKey)
	if err != nil {
		return fmt.Errorf("reading mapping publication cursor: %w", err)
	}

	fullPublication := force || publishedValue == ""
	var published int64
	if !fullPublication {
		published, err = strconv.ParseInt(publishedValue, 10, 64)
		if err != nil || published < 0 || published > revision {
			fullPublication = true
		}
	}
	if !fullPublication && published == revision {
		return nil
	}

	var mappings []db.WorktreeProjectMapping
	var deletes []db.WorktreeMappingKey
	if fullPublication {
		mappings, err = s.local.ListAllWorktreeProjectMappings(ctx)
		if err != nil {
			return err
		}
	} else {
		delta, err := s.local.LoadWorktreeMappingPublicationDelta(
			ctx, published, revision)
		if err != nil {
			return err
		}
		mappings, deletes = delta.Mappings, delta.Deletes
	}

	if err := s.commitWorktreeMappingPublication(
		ctx, fullPublication, mappings, deletes,
	); err != nil {
		return err
	}
	if err := state.SetSyncState(
		stateKey, strconv.FormatInt(revision, 10),
	); err != nil {
		return fmt.Errorf("advancing mapping publication cursor: %w", err)
	}
	return nil
}

// commitWorktreeMappingPublication writes one publication window (a full
// archive-scoped rebuild or a tombstoned delta) to the mirror in a single
// transaction.
func (s *Sync) commitWorktreeMappingPublication(
	ctx context.Context,
	fullPublication bool,
	mappings []db.WorktreeProjectMapping,
	deletes []db.WorktreeMappingKey,
) error {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning mapping publication tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if fullPublication {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM source_worktree_project_mappings
			WHERE source_archive_id = $1`, s.archiveID); err != nil {
			return fmt.Errorf("clearing mapping mirror scope: %w", err)
		}
	} else {
		for _, key := range deletes {
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM source_worktree_project_mappings
				WHERE source_archive_id = $1
				  AND machine = $2 AND path_prefix = $3`,
				s.archiveID, key.Machine, key.PathPrefix); err != nil {
				return fmt.Errorf("deleting mapping tombstone: %w", err)
			}
		}
	}
	for _, m := range mappings {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO source_worktree_project_mappings
			(source_archive_id, machine, path_prefix, layout, project,
			 original_project, enabled, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (source_archive_id, machine, path_prefix)
			DO UPDATE SET
				layout = EXCLUDED.layout,
				project = EXCLUDED.project,
				original_project = EXCLUDED.original_project,
				enabled = EXCLUDED.enabled,
				updated_at = EXCLUDED.updated_at`,
			s.archiveID, m.Machine, m.PathPrefix, m.Layout, m.Project,
			m.OriginalProject, m.Enabled, m.UpdatedAt); err != nil {
			return fmt.Errorf("upserting mapping mirror row: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing mapping publication: %w", err)
	}
	return nil
}
