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
// succeeds. Filtered mirrors rebuild a scoped explicit-rule snapshot on
// every push; dynamic rules cannot be attributed to one project without
// exposing out-of-scope path metadata. It returns the revision just
// published so the caller can persist it as mirror metadata's
// MappingRevision.
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
	if s.isFiltered() {
		mappings, err := s.local.ListAllWorktreeProjectMappings(ctx)
		if err != nil {
			return 0, err
		}
		mappings = filterWorktreeMappingsForDuckScope(
			mappings, s.projects, s.excludeProjects,
		)
		if err := s.commitWorktreeMappingPublication(
			ctx, true, mappings, nil,
		); err != nil {
			return 0, err
		}
		return revision, nil
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

func filterWorktreeMappingsForDuckScope(
	mappings []db.WorktreeProjectMapping,
	projects, excludeProjects []string,
) []db.WorktreeProjectMapping {
	out := make([]db.WorktreeProjectMapping, 0, len(mappings))
	for _, mapping := range mappings {
		if mapping.Project == "" ||
			!projectMatchesPushScope(
				mapping.Project, projects, excludeProjects,
			) {
			continue
		}
		if mapping.OriginalProject == "" ||
			!projectMatchesPushScope(
				mapping.OriginalProject, projects, excludeProjects,
			) {
			mapping.OriginalProject = ""
		}
		out = append(out, mapping)
	}
	return out
}

// commitWorktreeMappingPublication writes one publication window (a full
// archive-scoped rebuild or a tombstoned delta) to the DuckDB mirror in a
// single Bun transaction.
func (s *Sync) commitWorktreeMappingPublication(
	ctx context.Context,
	fullPublication bool,
	mappings []db.WorktreeProjectMapping,
	deletes []db.WorktreeMappingKey,
) error {
	rows, err := db.CanonicalWorktreeProjectMappingRows(s.archiveID, mappings)
	if err != nil {
		return fmt.Errorf("converting duckdb worktree mappings: %w", err)
	}
	tx, err := s.bun.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning duckdb mapping publication tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if fullPublication {
		if err := db.ClearWorktreeProjectMappingRows(ctx, tx, s.archiveID); err != nil {
			return fmt.Errorf("clearing duckdb mapping mirror scope: %w", err)
		}
	} else if err := db.DeleteWorktreeProjectMappingRows(
		ctx, tx, s.archiveID, deletes,
	); err != nil {
		return fmt.Errorf("deleting duckdb mapping tombstones: %w", err)
	}
	if err := db.UpsertWorktreeProjectMappingRows(ctx, tx, rows, nil); err != nil {
		return fmt.Errorf("upserting duckdb mapping mirror rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing duckdb mapping publication: %w", err)
	}
	return nil
}
