package db

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

// WorktreeMappingConflictPolicy supplies the narrow target-ownership rule for
// original_project. All other canonical columns use complete replacement.
type WorktreeMappingConflictPolicy func(*bun.InsertQuery) *bun.InsertQuery

// CanonicalWorktreeProjectMappingRows converts archive mappings into the
// complete portable source-scoped representation used by every mirror.
func CanonicalWorktreeProjectMappingRows(
	archiveID string, mappings []WorktreeProjectMapping,
) ([]bunmodel.SourceWorktreeProjectMapping, error) {
	if archiveID == "" {
		return nil, fmt.Errorf("converting worktree mappings: empty archive id")
	}
	rows := make([]bunmodel.SourceWorktreeProjectMapping, 0, len(mappings))
	for _, mapping := range mappings {
		createdAt, err := requiredTimestampToBunRow(mapping.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf(
				"worktree mapping %q created_at: %w", mapping.PathPrefix, err,
			)
		}
		updatedAt, err := requiredTimestampToBunRow(mapping.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf(
				"worktree mapping %q updated_at: %w", mapping.PathPrefix, err,
			)
		}
		truncateCanonicalTimestamp(&createdAt)
		truncateCanonicalTimestamp(&updatedAt)
		rows = append(rows, bunmodel.SourceWorktreeProjectMapping{
			ID: mapping.ID, SourceArchiveID: archiveID,
			Machine:         SanitizeUTF8(mapping.Machine),
			PathPrefix:      SanitizeUTF8(mapping.PathPrefix),
			Layout:          SanitizeUTF8(mapping.Layout),
			Project:         SanitizeUTF8(mapping.Project),
			OriginalProject: SanitizeUTF8(mapping.OriginalProject),
			Enabled:         mapping.Enabled, CreatedAt: createdAt, UpdatedAt: updatedAt,
		})
	}
	return rows, nil
}

// UpsertWorktreeProjectMappingRows writes complete canonical mappings. A
// target may customize only original_project conflict ownership.
func UpsertWorktreeProjectMappingRows(
	ctx context.Context,
	store bun.IDB,
	rows []bunmodel.SourceWorktreeProjectMapping,
	policy WorktreeMappingConflictPolicy,
) error {
	for i := range rows {
		row := rows[i]
		query := store.NewInsert().Model(&row).
			Column(
				"id", "source_archive_id", "machine", "path_prefix", "layout",
				"project", "original_project", "enabled", "created_at", "updated_at",
			).
			Value("enabled", "?", row.Enabled).
			On("CONFLICT (source_archive_id, machine, path_prefix) DO UPDATE").
			Set("id = EXCLUDED.id").
			Set("layout = EXCLUDED.layout").
			Set("project = EXCLUDED.project").
			Set("enabled = EXCLUDED.enabled").
			Set("created_at = EXCLUDED.created_at").
			Set("updated_at = EXCLUDED.updated_at")
		if policy == nil {
			query = query.Set("original_project = EXCLUDED.original_project")
		} else {
			query = policy(query)
		}
		if _, err := query.Returning("").Exec(ctx); err != nil {
			return fmt.Errorf("upserting canonical worktree mappings: %w", err)
		}
	}
	return nil
}

// DeleteWorktreeProjectMappingRows applies archive-scoped mapping tombstones.
func DeleteWorktreeProjectMappingRows(
	ctx context.Context,
	store bun.IDB,
	archiveID string,
	keys []WorktreeMappingKey,
) error {
	for start := 0; start < len(keys); start += canonicalWriteBatchSize {
		end := min(start+canonicalWriteBatchSize, len(keys))
		tuples := make([][]any, 0, end-start)
		for _, key := range keys[start:end] {
			tuples = append(tuples, []any{key.Machine, key.PathPrefix})
		}
		if _, err := store.NewDelete().
			Model((*bunmodel.SourceWorktreeProjectMapping)(nil)).
			Where("source_archive_id = ?", archiveID).
			Where("(machine, path_prefix) IN ?", bun.Tuple(tuples)).Exec(ctx); err != nil {
			return fmt.Errorf("deleting canonical worktree mappings: %w", err)
		}
	}
	return nil
}

// ClearWorktreeProjectMappingRows clears one archive's mapping publication.
func ClearWorktreeProjectMappingRows(
	ctx context.Context, store bun.IDB, archiveID string,
) error {
	if _, err := store.NewDelete().
		Model((*bunmodel.SourceWorktreeProjectMapping)(nil)).
		Where("source_archive_id = ?", archiveID).Exec(ctx); err != nil {
		return fmt.Errorf("clearing canonical worktree mappings: %w", err)
	}
	return nil
}
