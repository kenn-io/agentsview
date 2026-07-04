package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/agentsview/internal/export"
)

type pgProjectIdentityExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func upsertProjectIdentityObservation(
	ctx context.Context,
	q pgProjectIdentityExecer,
	obs export.ProjectIdentityObservation,
	excludeRemote string,
) error {
	if obs.GitRemote == "" {
		var exists bool
		if err := q.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM project_identity_observations
				WHERE project = $1 AND machine = $2 AND root_path = $3
				  AND git_remote != ''
				  AND ($4 = '' OR git_remote != $4)
			)`,
			obs.Project, obs.Machine, obs.RootPath, excludeRemote,
		).Scan(&exists); err != nil {
			return fmt.Errorf(
				"checking pg project identity remote observation: %w", err,
			)
		}
		if exists {
			return nil
		}
	} else if _, err := q.ExecContext(ctx, `
		DELETE FROM project_identity_observations
		WHERE project = $1 AND machine = $2 AND root_path = $3
		  AND git_remote = ''`,
		obs.Project, obs.Machine, obs.RootPath,
	); err != nil {
		return fmt.Errorf(
			"removing stale pg project identity root fallback: %w", err,
		)
	}

	if _, err := q.ExecContext(ctx, `
		INSERT INTO project_identity_observations (
			project, machine, root_path, git_remote, git_remote_name,
			worktree_name, worktree_root_path, observed_at,
			normalized_remote, key_source, key
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
		)
		ON CONFLICT (project, machine, root_path, git_remote)
		DO UPDATE SET
			git_remote_name = EXCLUDED.git_remote_name,
			worktree_name = EXCLUDED.worktree_name,
			worktree_root_path = EXCLUDED.worktree_root_path,
			observed_at = EXCLUDED.observed_at,
			normalized_remote = EXCLUDED.normalized_remote,
			key_source = EXCLUDED.key_source,
			key = EXCLUDED.key`,
		obs.Project, obs.Machine, obs.RootPath, obs.GitRemote,
		obs.GitRemoteName, obs.WorktreeName, obs.WorktreeRootPath,
		obs.ObservedAt, obs.NormalizedRemote, obs.KeySource, obs.Key,
	); err != nil {
		return fmt.Errorf("upserting pg project identity observation: %w", err)
	}
	return nil
}
