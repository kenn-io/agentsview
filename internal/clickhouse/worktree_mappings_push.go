package clickhouse

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// syncWorktreeMappings publishes worktree mapping metadata to the
// ClickHouse mirror up through the local archive's current revision, using
// storedRev as the cursor, following the identity publication contract of
// syncProjectIdentityObservations: archive-scoped full rebuilds, tombstoned
// deltas, and a mirror-resident cursor persisted by the caller only after
// the push succeeds. Filtered mirrors rebuild a scoped explicit-rule
// snapshot on every push; dynamic rules cannot be attributed to one
// project without exposing out-of-scope path metadata. It returns the
// revision just published so the caller can persist it as mirror
// metadata's mapping revision.
func (s *Sync) syncWorktreeMappings(
	ctx context.Context, storedRev int64, full bool,
) (int64, error) {
	revision, err := s.local.WorktreeMappingPublicationRevision(ctx)
	if err != nil {
		return 0, err
	}
	if s.isFiltered() {
		mappings, err := s.local.ListAllWorktreeProjectMappings(ctx)
		if err != nil {
			return 0, err
		}
		mappings = filterWorktreeMappingsForScope(
			mappings, s.projects, s.excludeProjects,
		)
		if err := s.commitWorktreeMappingPublication(
			ctx, true, mappings, nil,
		); err != nil {
			return 0, err
		}
		return revision, nil
	}

	fullPublication := full || storedRev <= 0 || storedRev > revision
	if !fullPublication && storedRev == revision {
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
			ctx, storedRev, revision)
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

func filterWorktreeMappingsForScope(
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
// archive-scoped rebuild or a tombstoned delta) by inserting the current
// rows then deleting older versions of this archive's mapping keys.
func (s *Sync) commitWorktreeMappingPublication(
	ctx context.Context,
	fullPublication bool,
	mappings []db.WorktreeProjectMapping,
	deletes []db.WorktreeMappingKey,
) error {
	version := newPushVersion()
	if !fullPublication {
		if err := s.deleteWorktreeMappingKeys(ctx, deletes); err != nil {
			return err
		}
	}

	rows := make([][]any, 0, len(mappings))
	for _, m := range mappings {
		rows = append(rows, []any{
			s.archiveID, m.Machine, m.PathPrefix, m.Layout, m.Project,
			m.OriginalProject, m.Enabled, m.UpdatedAt, version,
		})
	}
	if err := insertRows(ctx, s.conn, "source_worktree_project_mappings", rows); err != nil {
		return fmt.Errorf("upserting clickhouse mapping mirror row: %w", err)
	}

	if fullPublication {
		if _, err := s.conn.ExecContext(ctx, `
			DELETE FROM source_worktree_project_mappings
			WHERE source_archive_id = ? AND push_version < ?`,
			s.archiveID, version,
		); err != nil {
			return fmt.Errorf("clearing clickhouse mapping mirror scope: %w", err)
		}
		return nil
	}
	return s.deleteOlderWorktreeMappingKeys(ctx, mappings, version)
}

func (s *Sync) deleteWorktreeMappingKeys(
	ctx context.Context, keys []db.WorktreeMappingKey,
) error {
	for start := 0; start < len(keys); start += projectIdentityDeleteBatchSize {
		end := min(start+projectIdentityDeleteBatchSize, len(keys))
		args := []any{s.archiveID}
		tuples := make([]string, 0, end-start)
		for _, key := range keys[start:end] {
			tuples = append(tuples, "(?, ?)")
			args = append(args, key.Machine, key.PathPrefix)
		}
		if _, err := s.conn.ExecContext(ctx, `
			DELETE FROM source_worktree_project_mappings
			WHERE source_archive_id = ?
			  AND (machine, path_prefix) IN (`+strings.Join(tuples, ", ")+`)`,
			args...,
		); err != nil {
			return fmt.Errorf("deleting clickhouse mapping tombstone: %w", err)
		}
	}
	return nil
}

func (s *Sync) deleteOlderWorktreeMappingKeys(
	ctx context.Context,
	mappings []db.WorktreeProjectMapping,
	version uint64,
) error {
	for start := 0; start < len(mappings); start += projectIdentityDeleteBatchSize {
		end := min(start+projectIdentityDeleteBatchSize, len(mappings))
		args := []any{s.archiveID}
		tuples := make([]string, 0, end-start)
		for _, m := range mappings[start:end] {
			tuples = append(tuples, "(?, ?)")
			args = append(args, m.Machine, m.PathPrefix)
		}
		args = append(args, version)
		if _, err := s.conn.ExecContext(ctx, `
			DELETE FROM source_worktree_project_mappings
			WHERE source_archive_id = ?
			  AND (machine, path_prefix) IN (`+strings.Join(tuples, ", ")+`)
			  AND push_version < ?`,
			args...,
		); err != nil {
			return fmt.Errorf("deleting older clickhouse mapping mirror rows: %w", err)
		}
	}
	return nil
}
