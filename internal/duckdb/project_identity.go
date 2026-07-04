package duckdb

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/export"
)

func (s *Store) ListProjectIdentityObservations(
	ctx context.Context,
	labels []string,
) ([]export.ProjectIdentityObservation, error) {
	if labels != nil && len(labels) == 0 {
		return []export.ProjectIdentityObservation{}, nil
	}
	query := `SELECT project, machine, root_path, git_remote, git_remote_name,
		worktree_name, worktree_root_path, observed_at,
		normalized_remote, key_source, key
		FROM project_identity_observations`
	args := make([]any, 0, len(labels))
	if len(labels) > 0 {
		placeholders := make([]string, 0, len(labels))
		for _, label := range labels {
			placeholders = append(placeholders, "?")
			args = append(args, label)
		}
		query += " WHERE project IN (" + strings.Join(placeholders, ",") + ")"
	}
	query += " ORDER BY project, machine, root_path, git_remote"

	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing duckdb project identity observations: %w", err)
	}
	defer rows.Close()

	var out []export.ProjectIdentityObservation
	for rows.Next() {
		var obs export.ProjectIdentityObservation
		var observedAt time.Time
		if err := rows.Scan(
			&obs.Project,
			&obs.Machine,
			&obs.RootPath,
			&obs.GitRemote,
			&obs.GitRemoteName,
			&obs.WorktreeName,
			&obs.WorktreeRootPath,
			&observedAt,
			&obs.NormalizedRemote,
			&obs.KeySource,
			&obs.Key,
		); err != nil {
			return nil, fmt.Errorf("scanning duckdb project identity observation: %w", err)
		}
		obs.ObservedAt = observedAt
		out = append(out, obs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating duckdb project identity observations: %w", err)
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
	result := export.BuildProjectsMap(labels, observations)
	fallbacks, err := s.legacyProjectIdentityCandidates(ctx, labels)
	if err != nil {
		return nil, err
	}
	for _, label := range labels {
		if result[label].Resolution != export.ProjectResolutionUnknown {
			continue
		}
		candidates := fallbacks[label]
		switch len(candidates) {
		case 0:
			result[label] = export.ProjectMapEntry{
				Resolution: export.ProjectResolutionUnknown,
			}
		case 1:
			for _, identity := range candidates {
				i := identity
				result[label] = export.ProjectMapEntry{
					Resolution: export.ProjectResolutionResolved,
					Identity:   &i,
				}
			}
		default:
			result[label] = export.ProjectMapEntry{
				Resolution: export.ProjectResolutionAmbiguous,
			}
		}
	}
	return result, nil
}

func (s *Store) legacyProjectIdentityCandidates(
	ctx context.Context,
	labels []string,
) (map[string]map[string]export.ProjectIdentity, error) {
	out := map[string]map[string]export.ProjectIdentity{}
	if len(labels) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(labels))
	placeholders := make([]string, 0, len(labels))
	for _, label := range labels {
		placeholders = append(placeholders, "?")
		args = append(args, label)
	}
	query := `SELECT project, cwd, COALESCE(file_path, '')
		FROM sessions
		WHERE deleted_at IS NULL
		  AND project IN (` + strings.Join(placeholders, ",") + `)
		  AND (cwd != '' OR COALESCE(file_path, '') != '')
		ORDER BY project, cwd, file_path`
	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing duckdb legacy project identity sessions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var project, cwd, filePath string
		if err := rows.Scan(&project, &cwd, &filePath); err != nil {
			return nil, fmt.Errorf("scanning duckdb legacy project identity session: %w", err)
		}
		for _, root := range duckLegacyProjectIdentityRoots(cwd, filePath) {
			identity := export.BuildStoredProjectIdentity(
				export.ProjectIdentityInput{RootPath: root},
			)
			if identity.Key == "" {
				continue
			}
			if out[project] == nil {
				out[project] = map[string]export.ProjectIdentity{}
			}
			out[project][identity.Key] = identity
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating duckdb legacy project identity sessions: %w", err)
	}
	return out, nil
}

func duckLegacyProjectIdentityRoots(cwd, filePath string) []string {
	var roots []string
	candidates := []string{cwd}
	if filePath = strings.TrimSpace(filePath); filePath != "" {
		candidates = append(candidates, filepath.Dir(filePath))
		slashPath := strings.ReplaceAll(filePath, "\\", "/")
		candidates = append(candidates, path.Dir(slashPath))
	}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || candidate == "." {
			continue
		}
		if _, ok := export.NormalizeStoredRootPath(candidate); !ok {
			continue
		}
		if !slices.Contains(roots, candidate) {
			roots = append(roots, candidate)
		}
	}
	sort.Strings(roots)
	return roots
}
