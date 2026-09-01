package db

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
	"go.kenn.io/agentsview/internal/export"
)

// Identity publications historically use 500-row batches. Their widest model
// binds fewer than 25 columns, keeping one statement well below PostgreSQL's
// parameter limit while avoiding fivefold statement growth on full mirrors.
const canonicalIdentityWriteBatchSize = 500

var (
	canonicalProjectIdentityObservationColumns = bunmodel.ModelColumns(
		(*bunmodel.SourceProjectIdentityObservation)(nil),
	)
	canonicalProjectIdentityObservationConflictClause = canonicalConflictUpdateClauseForKey(
		"source_archive_id, project, machine, root_path, git_remote",
		canonicalReplacementColumns(
			(*bunmodel.SourceProjectIdentityObservation)(nil),
			"source_archive_id", "project", "machine", "root_path", "git_remote",
		),
	)
	canonicalSessionProjectIdentitySnapshotColumns = bunmodel.ModelColumns(
		(*bunmodel.SourceSessionProjectIdentitySnapshot)(nil),
	)
	canonicalSessionProjectIdentitySnapshotConflictClause = canonicalConflictUpdateClauseForKey(
		"source_archive_id, source_database_generation, source_session_id",
		canonicalReplacementColumns(
			(*bunmodel.SourceSessionProjectIdentitySnapshot)(nil),
			"source_archive_id", "source_database_generation", "source_session_id",
		),
	)
)

// CanonicalProjectIdentityObservationRows converts source observations into
// their complete portable representation, including credential-safe remotes.
func CanonicalProjectIdentityObservationRows(
	archiveID, archiveSalt string,
	observations []export.ProjectIdentityObservation,
) ([]bunmodel.SourceProjectIdentityObservation, error) {
	if archiveID == "" {
		return nil, fmt.Errorf("converting project identity observations: empty archive id")
	}
	rows := make([]bunmodel.SourceProjectIdentityObservation, 0, len(observations))
	for _, observation := range observations {
		observation.SourceArchiveID = archiveID
		observation.SourceArchiveSalt = archiveSalt
		observation = export.SanitizeStoredProjectIdentityObservation(observation)
		observedAt := bunmodel.NewTimestamp(observation.ObservedAt)
		truncateCanonicalTimestamp(&observedAt)
		rows = append(rows, bunmodel.SourceProjectIdentityObservation{
			SourceArchiveID: archiveID, SourceArchiveSalt: archiveSalt,
			Project:              SanitizeUTF8(observation.Project),
			Machine:              SanitizeUTF8(observation.Machine),
			RootPath:             SanitizeUTF8(observation.RootPath),
			GitRemote:            SanitizeUTF8(observation.GitRemote),
			GitRemoteName:        SanitizeUTF8(observation.GitRemoteName),
			RepositoryPath:       SanitizeUTF8(observation.RepositoryPath),
			WorktreeName:         SanitizeUTF8(observation.WorktreeName),
			WorktreeRootPath:     SanitizeUTF8(observation.WorktreeRootPath),
			WorktreeRelationship: SanitizeUTF8(string(observation.WorktreeRelationship)),
			CheckoutState:        SanitizeUTF8(string(observation.CheckoutState)),
			GitBranch:            SanitizeUTF8(observation.GitBranch),
			RemoteResolution:     SanitizeUTF8(string(observation.RemoteResolution)),
			RemoteCandidateCount: observation.RemoteCandidateCount,
			ObservedAt:           observedAt,
			NormalizedRemote:     SanitizeUTF8(observation.NormalizedRemote),
			KeySource:            SanitizeUTF8(observation.KeySource),
			Key:                  SanitizeUTF8(observation.Key),
		})
	}
	return rows, nil
}

// CanonicalSessionProjectIdentitySnapshotRows converts durable per-session
// source snapshots into their complete portable representation. The stable
// archive/generation/session identity remains fixed while reclassification or
// source refresh may replace the snapshot payload.
func CanonicalSessionProjectIdentitySnapshotRows(
	archiveID, databaseGeneration string,
	snapshots []export.ProjectIdentityObservation,
) ([]bunmodel.SourceSessionProjectIdentitySnapshot, error) {
	if archiveID == "" {
		return nil, fmt.Errorf("converting session identity snapshots: empty archive id")
	}
	if databaseGeneration == "" {
		return nil, fmt.Errorf(
			"converting session identity snapshots: empty database generation",
		)
	}
	rows := make([]bunmodel.SourceSessionProjectIdentitySnapshot, 0, len(snapshots))
	for _, snapshot := range snapshots {
		snapshot = export.SanitizeStoredProjectIdentityObservation(snapshot)
		observedAt := bunmodel.NewTimestamp(snapshot.ObservedAt)
		truncateCanonicalTimestamp(&observedAt)
		rows = append(rows, bunmodel.SourceSessionProjectIdentitySnapshot{
			SourceArchiveID:          archiveID,
			SourceDatabaseGeneration: databaseGeneration,
			SourceSessionID:          SanitizeUTF8(snapshot.SessionID),
			Project:                  SanitizeUTF8(snapshot.Project),
			Machine:                  SanitizeUTF8(snapshot.Machine),
			RootPath:                 SanitizeUTF8(snapshot.RootPath),
			GitRemote:                SanitizeUTF8(snapshot.GitRemote),
			GitRemoteName:            SanitizeUTF8(snapshot.GitRemoteName),
			RepositoryPath:           SanitizeUTF8(snapshot.RepositoryPath),
			WorktreeName:             SanitizeUTF8(snapshot.WorktreeName),
			WorktreeRootPath:         SanitizeUTF8(snapshot.WorktreeRootPath),
			WorktreeRelationship:     SanitizeUTF8(string(snapshot.WorktreeRelationship)),
			CheckoutState:            SanitizeUTF8(string(snapshot.CheckoutState)),
			GitBranch:                SanitizeUTF8(snapshot.GitBranch),
			RemoteResolution:         SanitizeUTF8(string(snapshot.RemoteResolution)),
			RemoteCandidateCount:     snapshot.RemoteCandidateCount,
			ObservedAt:               observedAt,
			NormalizedRemote:         SanitizeUTF8(snapshot.NormalizedRemote),
			KeySource:                SanitizeUTF8(snapshot.KeySource),
			Key:                      SanitizeUTF8(snapshot.Key),
		})
	}
	return rows, nil
}

// UpsertSourceArchiveRow establishes one immutable archive identity scope.
func UpsertSourceArchiveRow(
	ctx context.Context, store bun.IDB, archiveID, archiveSalt string,
) error {
	if archiveID == "" {
		return fmt.Errorf("upserting source archive: empty archive id")
	}
	row := bunmodel.SourceArchive{
		SourceArchiveID: archiveID, SourceArchiveSalt: archiveSalt,
	}
	if _, err := store.NewInsert().Model(&row).
		On("CONFLICT (source_archive_id) DO NOTHING").Returning("").Exec(ctx); err != nil {
		return fmt.Errorf("upserting source archive %q: %w", archiveID, err)
	}
	var existingSalt string
	if err := store.NewSelect().Model((*bunmodel.SourceArchive)(nil)).
		Column("source_archive_salt").
		Where("source_archive_id = ?", archiveID).Scan(ctx, &existingSalt); err != nil {
		return fmt.Errorf("verifying source archive %q: %w", archiveID, err)
	}
	if existingSalt != archiveSalt {
		return fmt.Errorf("archive salt mismatch for %q", archiveID)
	}
	return nil
}

// UpsertProjectIdentityObservationRows writes complete canonical observation
// rows; adapters own only fallback selection and publication-scope policy.
func UpsertProjectIdentityObservationRows(
	ctx context.Context,
	store bun.IDB,
	rows []bunmodel.SourceProjectIdentityObservation,
) error {
	for start := 0; start < len(rows); start += canonicalIdentityWriteBatchSize {
		end := min(start+canonicalIdentityWriteBatchSize, len(rows))
		chunk := rows[start:end]
		query := store.NewInsert().Model(&chunk).
			Column(canonicalProjectIdentityObservationColumns...).
			On(canonicalProjectIdentityObservationConflictClause)
		if _, err := query.Returning("").Exec(ctx); err != nil {
			return fmt.Errorf("upserting canonical project identity observations: %w", err)
		}
	}
	return nil
}

// UpsertSessionProjectIdentitySnapshotRows writes complete canonical source
// snapshots using the portable archive/generation/session identity.
func UpsertSessionProjectIdentitySnapshotRows(
	ctx context.Context,
	store bun.IDB,
	rows []bunmodel.SourceSessionProjectIdentitySnapshot,
) error {
	for start := 0; start < len(rows); start += canonicalIdentityWriteBatchSize {
		end := min(start+canonicalIdentityWriteBatchSize, len(rows))
		chunk := rows[start:end]
		query := store.NewInsert().Model(&chunk).
			Column(canonicalSessionProjectIdentitySnapshotColumns...).
			On(canonicalSessionProjectIdentitySnapshotConflictClause)
		if _, err := query.Returning("").Exec(ctx); err != nil {
			return fmt.Errorf("upserting canonical session identity snapshots: %w", err)
		}
	}
	return nil
}

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

// InsertWorktreeProjectMappingRows creates complete canonical mappings and
// preserves archive duplicate rejection.
func InsertWorktreeProjectMappingRows(
	ctx context.Context,
	store bun.IDB,
	rows []bunmodel.SourceWorktreeProjectMapping,
) error {
	for i := range rows {
		row := rows[i]
		if _, err := store.NewInsert().Model(&row).
			Column(bunmodel.ModelColumns(
				(*bunmodel.SourceWorktreeProjectMapping)(nil),
			)...).
			Value("enabled", "?", row.Enabled).Returning("").Exec(ctx); err != nil {
			return fmt.Errorf("inserting canonical worktree mappings: %w", err)
		}
	}
	return nil
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
			Column(bunmodel.ModelColumns(
				(*bunmodel.SourceWorktreeProjectMapping)(nil),
			)...).
			Value("enabled", "?", row.Enabled).
			On("CONFLICT (source_archive_id, machine, path_prefix) DO UPDATE")
		for _, column := range canonicalReplacementColumns(
			(*bunmodel.SourceWorktreeProjectMapping)(nil),
			"source_archive_id", "machine", "path_prefix", "original_project",
		) {
			query = query.Set("? = EXCLUDED.?", bun.Ident(column), bun.Ident(column))
		}
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

// UpdateWorktreeProjectMappingRow replaces one archive mapping selected by
// its stable source ID, allowing its natural path key to change atomically.
func UpdateWorktreeProjectMappingRow(
	ctx context.Context,
	store bun.IDB,
	archiveID string,
	id int64,
	machine string,
	row bunmodel.SourceWorktreeProjectMapping,
) (bool, error) {
	result, err := store.NewUpdate().Model(&row).
		Column(canonicalReplacementColumns(
			(*bunmodel.SourceWorktreeProjectMapping)(nil),
			"id", "source_archive_id", "machine", "path_prefix",
		)...).
		Set("path_prefix = ?", row.PathPrefix).
		Value("enabled", "?", row.Enabled).
		Where("source_archive_id = ?", archiveID).
		Where("id = ?", id).
		Where("machine = ?", machine).Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("updating canonical worktree mapping: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("counting canonical worktree mapping update: %w", err)
	}
	return changed > 0, nil
}

// DeleteWorktreeProjectMappingRows applies archive-scoped mapping tombstones.
func DeleteWorktreeProjectMappingRows(
	ctx context.Context,
	store bun.IDB,
	archiveID string,
	keys []WorktreeMappingKey,
) error {
	return writeCanonicalBatches(keys, func(batch []WorktreeMappingKey) error {
		tuples := make([][]any, 0, len(batch))
		for _, key := range batch {
			tuples = append(tuples, []any{key.Machine, key.PathPrefix})
		}
		if _, err := store.NewDelete().
			Model((*bunmodel.SourceWorktreeProjectMapping)(nil)).
			Where("source_archive_id = ?", archiveID).
			Where("(machine, path_prefix) IN ?", bun.Tuple(tuples)).Exec(ctx); err != nil {
			return fmt.Errorf("deleting canonical worktree mappings: %w", err)
		}
		return nil
	})
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
