package postgres

import (
	"context"
	"fmt"
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
		for i, label := range labels {
			placeholders = append(placeholders, fmt.Sprintf("$%d", i+1))
			args = append(args, label)
		}
		query += " WHERE project IN (" + strings.Join(placeholders, ",") + ")"
	}
	query += " ORDER BY project, machine, root_path, git_remote"

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
	for i, label := range labels {
		placeholders = append(placeholders, fmt.Sprintf("$%d", i+1))
		args = append(args, label)
	}
	// PostgreSQL does not mirror sessions.file_path; fallback candidates are
	// therefore limited to cwd. DuckDB mirrors file_path and includes that
	// parent directory for parity with SQLite where the data exists.
	query := `SELECT project, cwd
		FROM sessions
		WHERE deleted_at IS NULL
		  AND project IN (` + strings.Join(placeholders, ",") + `)
		  AND cwd != ''
		ORDER BY project, cwd`
	rows, err := s.pg.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing pg legacy project identity sessions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var project, cwd string
		if err := rows.Scan(&project, &cwd); err != nil {
			return nil, fmt.Errorf("scanning pg legacy project identity session: %w", err)
		}
		identity := export.BuildStoredProjectIdentity(
			export.ProjectIdentityInput{RootPath: cwd},
		)
		if identity.Key == "" {
			continue
		}
		if out[project] == nil {
			out[project] = map[string]export.ProjectIdentity{}
		}
		out[project][identity.Key] = identity
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating pg legacy project identity sessions: %w", err)
	}
	return out, nil
}
