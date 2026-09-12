package postgres

import (
	"context"
	"fmt"
	"strconv"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db"
)

const worktreeMappingPublicationStateKey = "worktree_mapping_publication_revision_v2"

// syncWorktreeMappings publishes worktree mapping metadata to the mirror. It
// follows the identity publication contract used by
// syncProjectIdentityObservations: one transaction, archive-scoped full
// rebuilds, tombstoned deltas, and a cursor that advances only after commit.
func (s *Sync) syncWorktreeMappings(ctx context.Context, force bool) error {
	revision, err := s.local.WorktreeMappingPublicationRevision(ctx)
	if err != nil {
		return err
	}
	if s.isFiltered() {
		// A filtered destination has no safe representation for dynamic rules
		// whose project is derived from arbitrary paths. Rebuild the small
		// explicit-rule set every push instead of advancing a cursor: a rule
		// whose target moves out of scope must remove its previously published
		// row even though the filtered delta contains no safe replacement.
		mappings, err := s.local.ListAllWorktreeProjectMappings(ctx)
		if err != nil {
			return err
		}
		mappings = filterWorktreeMappingsForPGScope(
			mappings, s.projects, s.excludeProjects,
		)
		return s.commitWorktreeMappingPublication(
			ctx, true, mappings, nil,
		)
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

func filterWorktreeMappingsForPGScope(
	mappings []db.WorktreeProjectMapping,
	projects, excludeProjects []string,
) []db.WorktreeProjectMapping {
	out := make([]db.WorktreeProjectMapping, 0, len(mappings))
	for _, mapping := range mappings {
		if mapping.Project == "" ||
			!projectInPGSyncScope(mapping.Project, projects, excludeProjects) {
			continue
		}
		if mapping.OriginalProject == "" ||
			!projectInPGSyncScope(
				mapping.OriginalProject, projects, excludeProjects,
			) {
			mapping.OriginalProject = ""
		}
		out = append(out, mapping)
	}
	return out
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
	rows, err := db.CanonicalWorktreeProjectMappingRows(s.archiveID, mappings)
	if err != nil {
		return fmt.Errorf("converting pg worktree mappings: %w", err)
	}
	tx, err := s.bunDB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning mapping publication tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	publicationScope := unfilteredPublicationScope
	if s.isFiltered() {
		publicationScope = pushSyncStateScope(
			"", s.projects, s.excludeProjects,
		)
	}
	if fullPublication {
		if s.isFiltered() {
			if err := releaseFilteredWorktreeMappingFullOwnership(
				ctx, tx, s.archiveID, publicationScope,
			); err != nil {
				return err
			}
		} else if err := db.ClearWorktreeProjectMappingRows(
			ctx, tx, s.archiveID,
		); err != nil {
			return fmt.Errorf("clearing mapping mirror scope: %w", err)
		}
	} else if err := db.DeleteWorktreeProjectMappingRows(
		ctx, tx, s.archiveID, deletes,
	); err != nil {
		return fmt.Errorf("deleting mapping tombstones: %w", err)
	}
	var policy db.WorktreeMappingConflictPolicy
	if s.isFiltered() {
		policy = func(query *bun.InsertQuery) *bun.InsertQuery {
			return query.Set(`original_project = CASE
				WHEN EXCLUDED.original_project = ''
				 AND EXISTS (
					SELECT 1
					FROM source_worktree_project_mapping_scopes owner
					WHERE owner.source_archive_id = EXCLUDED.source_archive_id
					  AND owner.machine = EXCLUDED.machine
					  AND owner.path_prefix = EXCLUDED.path_prefix
					  AND owner.publication_scope <> ?
				 )
				THEN source_worktree_project_mapping.original_project
				ELSE EXCLUDED.original_project
			END`, publicationScope)
		}
	}
	if err := db.UpsertWorktreeProjectMappingRows(
		ctx, tx, rows, policy,
	); err != nil {
		return fmt.Errorf("upserting mapping mirror rows: %w", err)
	}
	for _, row := range rows {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO source_worktree_project_mapping_scopes (
				source_archive_id, machine, path_prefix, publication_scope
			) VALUES (?0, ?1, ?2, ?3)
			ON CONFLICT DO NOTHING`,
			s.archiveID, row.Machine, row.PathPrefix, publicationScope,
		); err != nil {
			return fmt.Errorf("owning mapping mirror row: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing mapping publication: %w", err)
	}
	return nil
}

func releaseFilteredWorktreeMappingFullOwnership(
	ctx context.Context,
	q bun.IDB,
	archiveID, publicationScope string,
) error {
	if _, err := q.ExecContext(ctx, `
		DELETE FROM source_worktree_project_mappings mapping
		WHERE mapping.source_archive_id = ?0
		  AND EXISTS (
			SELECT 1
			FROM source_worktree_project_mapping_scopes owner
			WHERE owner.source_archive_id = mapping.source_archive_id
			  AND owner.machine = mapping.machine
			  AND owner.path_prefix = mapping.path_prefix
			  AND owner.publication_scope = ?1
		  )
		  AND NOT EXISTS (
			SELECT 1
			FROM source_worktree_project_mapping_scopes owner
			WHERE owner.source_archive_id = mapping.source_archive_id
			  AND owner.machine = mapping.machine
			  AND owner.path_prefix = mapping.path_prefix
			  AND owner.publication_scope <> ?1
		  )`, archiveID, publicationScope); err != nil {
		return fmt.Errorf(
			"clearing exclusively owned filtered mappings: %w", err,
		)
	}
	if _, err := q.ExecContext(ctx, `
		DELETE FROM source_worktree_project_mapping_scopes
		WHERE source_archive_id = ?0 AND publication_scope = ?1`,
		archiveID, publicationScope,
	); err != nil {
		return fmt.Errorf(
			"clearing filtered mapping publication ownership: %w", err,
		)
	}
	return nil
}
