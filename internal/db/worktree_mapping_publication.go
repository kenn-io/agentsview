package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const archiveMetadataWorktreeMappingRevisionKey = "worktree_mapping_publication_revision"

// WorktreeMappingPublicationRevision is an O(1) change token for the complete
// worktree mapping set. It is bumped by triggers on every mapping insert,
// update, or delete, and read by push to decide whether mapping metadata
// needs republication.
func (db *DB) WorktreeMappingPublicationRevision(
	ctx context.Context,
) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var raw string
	err := db.getReader().QueryRowContext(ctx,
		`SELECT value FROM archive_metadata WHERE key = ?`,
		archiveMetadataWorktreeMappingRevisionKey,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf(
			"reading worktree mapping publication revision: %w", err)
	}
	revision, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || revision < 0 {
		return 0, fmt.Errorf(
			"invalid worktree mapping publication revision %q", raw)
	}
	return revision, nil
}

// WorktreeMappingKey identifies a mapping by its natural key, used for
// deletion tombstones since the numeric ID is not stable across replicas.
type WorktreeMappingKey struct {
	Machine    string
	PathPrefix string
}

// WorktreeMappingPublicationDelta contains current mapping rows and
// deletion tombstones whose latest journaled change falls inside one
// publication revision window.
type WorktreeMappingPublicationDelta struct {
	Mappings []WorktreeProjectMapping
	Deletes  []WorktreeMappingKey
}

// LoadWorktreeMappingPublicationDelta returns mappings whose latest journaled
// change falls inside (afterRevision, throughRevision], split into current
// rows and tombstoned keys, mirroring LoadProjectIdentityPublicationDelta.
func (db *DB) LoadWorktreeMappingPublicationDelta(
	ctx context.Context, afterRevision, throughRevision int64,
) (WorktreeMappingPublicationDelta, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if afterRevision < 0 || throughRevision < afterRevision {
		return WorktreeMappingPublicationDelta{}, fmt.Errorf(
			"invalid worktree mapping publication window (%d, %d]",
			afterRevision, throughRevision)
	}
	if afterRevision == throughRevision {
		return WorktreeMappingPublicationDelta{}, nil
	}

	rows, err := db.getReader().QueryContext(ctx, `
		SELECT m.id, m.machine, m.path_prefix, m.layout, m.project,
		       m.original_project, m.enabled, m.created_at, m.updated_at
		FROM worktree_project_mapping_changes c
		JOIN worktree_project_mappings m
		  ON m.machine = c.machine AND m.path_prefix = c.path_prefix
		WHERE c.deleted = 0 AND c.revision > ? AND c.revision <= ?
		ORDER BY m.machine, m.path_prefix`,
		afterRevision, throughRevision)
	if err != nil {
		return WorktreeMappingPublicationDelta{}, fmt.Errorf(
			"loading changed worktree mappings: %w", err)
	}
	mappings, err := scanWorktreeMappings(rows)
	if err != nil {
		return WorktreeMappingPublicationDelta{}, err
	}

	deleteRows, err := db.getReader().QueryContext(ctx, `
		SELECT machine, path_prefix
		FROM worktree_project_mapping_changes
		WHERE deleted = 1 AND revision > ? AND revision <= ?
		ORDER BY machine, path_prefix`,
		afterRevision, throughRevision)
	if err != nil {
		return WorktreeMappingPublicationDelta{}, fmt.Errorf(
			"loading worktree mapping tombstones: %w", err)
	}
	defer func() { _ = deleteRows.Close() }()
	var deletes []WorktreeMappingKey
	for deleteRows.Next() {
		var key WorktreeMappingKey
		if err := deleteRows.Scan(&key.Machine, &key.PathPrefix); err != nil {
			return WorktreeMappingPublicationDelta{}, fmt.Errorf(
				"scanning worktree mapping tombstone: %w", err)
		}
		deletes = append(deletes, key)
	}
	if err := deleteRows.Err(); err != nil {
		return WorktreeMappingPublicationDelta{}, fmt.Errorf(
			"iterating worktree mapping tombstones: %w", err)
	}
	return WorktreeMappingPublicationDelta{
		Mappings: mappings, Deletes: deletes,
	}, nil
}

// ListAllWorktreeProjectMappings returns every mapping for every machine,
// used by full publication.
func (db *DB) ListAllWorktreeProjectMappings(
	ctx context.Context,
) ([]WorktreeProjectMapping, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT id, machine, path_prefix, layout, project,
		       original_project, enabled, created_at, updated_at
		FROM worktree_project_mappings
		ORDER BY machine, path_prefix`)
	if err != nil {
		return nil, fmt.Errorf("listing all worktree mappings: %w", err)
	}
	return scanWorktreeMappings(rows)
}
