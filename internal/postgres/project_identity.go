package postgres

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/export"
)

// ListProjectIdentityObservations returns the mirrored identity
// observations for the given raw project labels, or every stored
// observation when labels is nil. Rows are ordered by (source_archive_id,
// project, machine, root_path, git_remote). Label lists of any size are
// supported: labels are sorted, deduplicated, and split into
// maxPGVars-sized chunks so the IN list stays far below PostgreSQL's
// 65,535 bind-parameter protocol limit. source_archive_id leads the ORDER
// BY, so per-chunk (label-range) results do not concatenate into the
// global order; when more than one chunk runs, the combined rows are
// re-sorted in Go on the same key columns for deterministic API ordering.
func (s *Store) ListProjectIdentityObservations(
	ctx context.Context,
	labels []string,
) ([]export.ProjectIdentityObservation, error) {
	if labels == nil {
		return s.listProjectIdentityObservationsChunk(ctx, nil)
	}
	if len(labels) == 0 {
		return []export.ProjectIdentityObservation{}, nil
	}
	sorted := slices.Clone(labels)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	if len(sorted) <= maxPGVars {
		return s.listProjectIdentityObservationsChunk(ctx, sorted)
	}
	var out []export.ProjectIdentityObservation
	err := pgQueryChunked(sorted, func(chunk []string) error {
		part, err := s.listProjectIdentityObservationsChunk(ctx, chunk)
		if err != nil {
			return err
		}
		out = append(out, part...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortProjectIdentityObservations(out)
	return out, nil
}

// sortProjectIdentityObservations restores the documented
// (source_archive_id, project, machine, root_path, git_remote) ordering
// after chunked queries are concatenated.
func sortProjectIdentityObservations(obs []export.ProjectIdentityObservation) {
	sort.SliceStable(obs, func(i, j int) bool {
		a, b := obs[i], obs[j]
		if a.SourceArchiveID != b.SourceArchiveID {
			return a.SourceArchiveID < b.SourceArchiveID
		}
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		if a.Machine != b.Machine {
			return a.Machine < b.Machine
		}
		if a.RootPath != b.RootPath {
			return a.RootPath < b.RootPath
		}
		return a.GitRemote < b.GitRemote
	})
}

// listProjectIdentityObservationsChunk runs one observation query for a
// single label chunk (nil means "all rows"); the chunk must already be
// within the bind-parameter budget.
func (s *Store) listProjectIdentityObservationsChunk(
	ctx context.Context,
	labels []string,
) ([]export.ProjectIdentityObservation, error) {
	query := `SELECT source_archive_id, source_archive_salt,
		project, machine, root_path, git_remote, git_remote_name,
		repository_path, worktree_name, worktree_root_path,
		worktree_relationship, checkout_state, git_branch,
		remote_resolution, remote_candidate_count, observed_at,
		normalized_remote, key_source, key
		FROM source_project_identity_observations`
	args := make([]any, 0, len(labels))
	var predicates []string
	if len(labels) > 0 {
		placeholders := make([]string, 0, len(labels))
		for _, label := range labels {
			args = append(args, label)
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
		}
		predicates = append(predicates,
			"project IN ("+strings.Join(placeholders, ",")+")")
	}
	if len(predicates) > 0 {
		query += " WHERE " + strings.Join(predicates, " AND ")
	}
	query += " ORDER BY source_archive_id, project, machine, root_path, git_remote"

	rows, err := s.pg.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing pg project identity observations: %w", err)
	}
	defer rows.Close()

	var out []export.ProjectIdentityObservation
	for rows.Next() {
		var obs export.ProjectIdentityObservation
		var observedAt time.Time
		if err := rows.Scan(
			&obs.SourceArchiveID,
			&obs.SourceArchiveSalt,
			&obs.Project,
			&obs.Machine,
			&obs.RootPath,
			&obs.GitRemote,
			&obs.GitRemoteName,
			&obs.RepositoryPath,
			&obs.WorktreeName,
			&obs.WorktreeRootPath,
			&obs.WorktreeRelationship,
			&obs.CheckoutState,
			&obs.GitBranch,
			&obs.RemoteResolution,
			&obs.RemoteCandidateCount,
			&observedAt,
			&obs.NormalizedRemote,
			&obs.KeySource,
			&obs.Key,
		); err != nil {
			return nil, fmt.Errorf("scanning pg project identity observation: %w", err)
		}
		obs.ObservedAt = observedAt
		out = append(out, obs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating pg project identity observations: %w", err)
	}
	return out, nil
}

func (s *Store) BuildProjectIdentityMap(
	ctx context.Context,
	labels []string,
) (map[string]export.ProjectMapEntry, error) {
	if labels != nil && len(labels) == 0 {
		return map[string]export.ProjectMapEntry{}, nil
	}
	observations, err := s.ListProjectIdentityObservations(ctx, labels)
	if err != nil {
		return nil, err
	}
	scope, err := s.sourceArchiveIdentityScope(ctx, observations)
	if err != nil {
		return nil, err
	}
	return export.BuildProjectsMapWithScope(labels, observations, scope), nil
}

func (s *Store) sourceArchiveIdentityScope(
	ctx context.Context,
	observations []export.ProjectIdentityObservation,
) (export.IdentityScope, error) {
	query := `
		SELECT source_archive_id, source_archive_salt
		FROM source_archives
	`
	query += " ORDER BY source_archive_id"
	rows, err := s.pg.QueryContext(ctx, query)
	if err != nil {
		return export.IdentityScope{}, fmt.Errorf(
			"listing pg source archives: %w", err,
		)
	}
	defer rows.Close()

	var scopes []export.IdentityScope
	for rows.Next() {
		var scope export.IdentityScope
		if err := rows.Scan(&scope.ArchiveID, &scope.ArchiveSalt); err != nil {
			return export.IdentityScope{}, fmt.Errorf(
				"scanning pg source archive: %w", err,
			)
		}
		scopes = append(scopes, scope)
	}
	if err := rows.Err(); err != nil {
		return export.IdentityScope{}, fmt.Errorf(
			"iterating pg source archives: %w", err,
		)
	}
	if len(scopes) == 1 {
		return scopes[0], nil
	}
	if len(scopes) == 0 {
		return observationIdentityScope(observations), nil
	}
	return export.AggregateIdentityScope(scopes), nil
}

func observationIdentityScope(
	observations []export.ProjectIdentityObservation,
) export.IdentityScope {
	unique := make(map[string]export.IdentityScope)
	for _, obs := range observations {
		scope := export.IdentityScope{
			ArchiveID:   strings.TrimSpace(obs.SourceArchiveID),
			ArchiveSalt: strings.TrimSpace(obs.SourceArchiveSalt),
		}
		if scope.ArchiveID == "" || scope.ArchiveSalt == "" {
			continue
		}
		unique[scope.ArchiveID+"\x00"+scope.ArchiveSalt] = scope
	}
	if len(unique) == 0 {
		return export.LegacySharedStoreIdentityScope()
	}
	scopes := make([]export.IdentityScope, 0, len(unique))
	for _, scope := range unique {
		scopes = append(scopes, scope)
	}
	if len(scopes) == 1 {
		return scopes[0]
	}
	return export.AggregateIdentityScope(scopes)
}
