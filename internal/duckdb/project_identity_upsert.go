package duckdb

import (
	"database/sql"
	"fmt"

	"go.kenn.io/agentsview/internal/export"
)

type duckProjectIdentityExec func(string, ...any) error
type duckProjectIdentityQueryRow func(string, ...any) *sql.Row

func upsertProjectIdentityObservation(
	exec duckProjectIdentityExec,
	queryRow duckProjectIdentityQueryRow,
	obs export.ProjectIdentityObservation,
	excludeRemote string,
) error {
	if obs.GitRemote == "" {
		var exists int
		if err := queryRow(`
			SELECT COUNT(*) FROM project_identity_observations
			WHERE project = ? AND machine = ? AND root_path = ?
			  AND git_remote != ''
			  AND (? = '' OR git_remote != ?)`,
			obs.Project, obs.Machine, obs.RootPath,
			excludeRemote, excludeRemote,
		).Scan(&exists); err != nil {
			return fmt.Errorf(
				"checking duckdb project identity remote observation: %w", err,
			)
		}
		if exists > 0 {
			return nil
		}
	} else if err := exec(`
		DELETE FROM project_identity_observations
		WHERE project = ? AND machine = ? AND root_path = ?
		  AND git_remote = ''`,
		obs.Project, obs.Machine, obs.RootPath,
	); err != nil {
		return fmt.Errorf(
			"removing stale duckdb project identity root fallback: %w", err,
		)
	}

	if err := exec(`
		INSERT INTO project_identity_observations (
			project, machine, root_path, git_remote, git_remote_name,
			worktree_name, worktree_root_path, observed_at,
			normalized_remote, key_source, key
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project, machine, root_path, git_remote) DO UPDATE SET
			git_remote_name = excluded.git_remote_name,
			worktree_name = excluded.worktree_name,
			worktree_root_path = excluded.worktree_root_path,
			observed_at = excluded.observed_at,
			normalized_remote = excluded.normalized_remote,
			key_source = excluded.key_source,
			key = excluded.key`,
		obs.Project, obs.Machine, obs.RootPath, obs.GitRemote,
		obs.GitRemoteName, obs.WorktreeName, obs.WorktreeRootPath,
		obs.ObservedAt, obs.NormalizedRemote, obs.KeySource, obs.Key,
	); err != nil {
		return fmt.Errorf("upserting duckdb project identity observation: %w", err)
	}
	return nil
}
