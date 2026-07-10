package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

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
		)`+projectIdentityObservationConflictClause,
		obs.Project, obs.Machine, obs.RootPath, obs.GitRemote,
		obs.GitRemoteName, obs.WorktreeName, obs.WorktreeRootPath,
		obs.ObservedAt, obs.NormalizedRemote, obs.KeySource, obs.Key,
	); err != nil {
		return fmt.Errorf("upserting pg project identity observation: %w", err)
	}
	return nil
}

const projectIdentityObservationConflictClause = `
		ON CONFLICT (project, machine, root_path, git_remote)
		DO UPDATE SET
			git_remote_name = EXCLUDED.git_remote_name,
			worktree_name = EXCLUDED.worktree_name,
			worktree_root_path = EXCLUDED.worktree_root_path,
			observed_at = EXCLUDED.observed_at,
			normalized_remote = EXCLUDED.normalized_remote,
			key_source = EXCLUDED.key_source,
			key = EXCLUDED.key`

// projectIdentityRootKey identifies the root a fallback (empty git_remote)
// observation competes with real-remote observations over.
type projectIdentityRootKey struct {
	project  string
	machine  string
	rootPath string
}

func observationRootKey(
	obs export.ProjectIdentityObservation,
) projectIdentityRootKey {
	return projectIdentityRootKey{
		project:  obs.Project,
		machine:  obs.Machine,
		rootPath: obs.RootPath,
	}
}

type projectIdentityObservationPlan struct {
	// realRemote holds deduped observations with a git remote.
	realRemote []export.ProjectIdentityObservation
	// fallbacks holds deduped empty-remote observations whose root has no
	// real-remote observation in the batch. Whether each survives still
	// depends on the rows already in PG.
	fallbacks []export.ProjectIdentityObservation
	// realRoots lists the roots of realRemote in first-seen order; stale
	// fallback rows for these roots must be deleted.
	realRoots []projectIdentityRootKey
}

// planProjectIdentityObservationSync reduces a batch to the final state of
// applying upsertProjectIdentityObservation to each row in order: the last
// observation per conflict key wins, and an empty-remote observation never
// survives alongside a real-remote observation for the same root — an
// earlier real row makes the fallback's existence check succeed, and a
// later real row deletes the already-inserted fallback.
func planProjectIdentityObservationSync(
	observations []export.ProjectIdentityObservation,
) projectIdentityObservationPlan {
	type conflictKey struct {
		root      projectIdentityRootKey
		gitRemote string
	}
	keyOrder := make([]conflictKey, 0, len(observations))
	latest := make(map[conflictKey]export.ProjectIdentityObservation,
		len(observations))
	realRootSet := make(map[projectIdentityRootKey]bool)

	var plan projectIdentityObservationPlan
	for _, obs := range observations {
		key := conflictKey{
			root: observationRootKey(obs), gitRemote: obs.GitRemote,
		}
		if _, seen := latest[key]; !seen {
			keyOrder = append(keyOrder, key)
		}
		latest[key] = obs
		if obs.GitRemote != "" && !realRootSet[key.root] {
			realRootSet[key.root] = true
			plan.realRoots = append(plan.realRoots, key.root)
		}
	}
	for _, key := range keyOrder {
		obs := latest[key]
		if obs.GitRemote != "" {
			plan.realRemote = append(plan.realRemote, obs)
			continue
		}
		if !realRootSet[key.root] {
			plan.fallbacks = append(plan.fallbacks, obs)
		}
	}
	return plan
}

// syncProjectIdentityObservationsBatch applies a batch of observations with
// set-based statements: one DELETE for stale fallback rows, one existence
// probe for fallback candidates, and multi-row upserts. The final table
// state matches applying upsertProjectIdentityObservation row by row with
// no excluded remote.
func syncProjectIdentityObservationsBatch(
	ctx context.Context,
	tx *sql.Tx,
	observations []export.ProjectIdentityObservation,
) error {
	plan := planProjectIdentityObservationSync(observations)
	if err := deleteProjectIdentityFallbackRows(
		ctx, tx, plan.realRoots,
	); err != nil {
		return err
	}
	fallbacks, err := projectIdentityFallbacksWithoutRealRemote(
		ctx, tx, plan.fallbacks,
	)
	if err != nil {
		return err
	}
	if err := insertProjectIdentityObservations(
		ctx, tx, plan.realRemote,
	); err != nil {
		return err
	}
	return insertProjectIdentityObservations(ctx, tx, fallbacks)
}

// projectIdentityRootKeyBatchSize bounds tuple-IN lists at three bind
// parameters per key.
const projectIdentityRootKeyBatchSize = 300

// projectIdentityInsertBatchSize bounds multi-row upserts at eleven bind
// parameters per row.
const projectIdentityInsertBatchSize = 500

func rootKeyTupleArgs(keys []projectIdentityRootKey) (string, []any) {
	tuples := make([]string, len(keys))
	args := make([]any, 0, len(keys)*3)
	for i, key := range keys {
		base := i * 3
		tuples[i] = fmt.Sprintf("($%d, $%d, $%d)", base+1, base+2, base+3)
		args = append(args, key.project, key.machine, key.rootPath)
	}
	return strings.Join(tuples, ", "), args
}

func deleteProjectIdentityFallbackRows(
	ctx context.Context,
	tx *sql.Tx,
	roots []projectIdentityRootKey,
) error {
	for start := 0; start < len(roots); start += projectIdentityRootKeyBatchSize {
		end := min(start+projectIdentityRootKeyBatchSize, len(roots))
		tuples, args := rootKeyTupleArgs(roots[start:end])
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM project_identity_observations
			WHERE git_remote = ''
			  AND (project, machine, root_path) IN (`+tuples+`)`,
			args...,
		); err != nil {
			return fmt.Errorf(
				"removing stale pg project identity root fallbacks: %w", err,
			)
		}
	}
	return nil
}

// projectIdentityFallbacksWithoutRealRemote drops fallback candidates whose
// root already has a real-remote row in PG, mirroring the per-row
// existence check in upsertProjectIdentityObservation.
func projectIdentityFallbacksWithoutRealRemote(
	ctx context.Context,
	tx *sql.Tx,
	candidates []export.ProjectIdentityObservation,
) ([]export.ProjectIdentityObservation, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	shadowed := make(map[projectIdentityRootKey]bool)
	for start := 0; start < len(candidates); start += projectIdentityRootKeyBatchSize {
		end := min(start+projectIdentityRootKeyBatchSize, len(candidates))
		keys := make([]projectIdentityRootKey, 0, end-start)
		for _, obs := range candidates[start:end] {
			keys = append(keys, observationRootKey(obs))
		}
		tuples, args := rootKeyTupleArgs(keys)
		rows, err := tx.QueryContext(ctx, `
			SELECT DISTINCT project, machine, root_path
			FROM project_identity_observations
			WHERE git_remote != ''
			  AND (project, machine, root_path) IN (`+tuples+`)`,
			args...,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"checking pg project identity remote observations: %w", err,
			)
		}
		if err := scanProjectIdentityRootKeys(rows, shadowed); err != nil {
			return nil, err
		}
	}
	out := make([]export.ProjectIdentityObservation, 0, len(candidates))
	for _, obs := range candidates {
		if !shadowed[observationRootKey(obs)] {
			out = append(out, obs)
		}
	}
	return out, nil
}

func scanProjectIdentityRootKeys(
	rows *sql.Rows, out map[projectIdentityRootKey]bool,
) error {
	defer rows.Close()
	for rows.Next() {
		var key projectIdentityRootKey
		if err := rows.Scan(
			&key.project, &key.machine, &key.rootPath,
		); err != nil {
			return fmt.Errorf(
				"scanning pg project identity remote observation: %w", err,
			)
		}
		out[key] = true
	}
	return rows.Err()
}

func insertProjectIdentityObservations(
	ctx context.Context,
	tx *sql.Tx,
	observations []export.ProjectIdentityObservation,
) error {
	for start := 0; start < len(observations); start += projectIdentityInsertBatchSize {
		end := min(start+projectIdentityInsertBatchSize, len(observations))
		chunk := observations[start:end]
		valueRows := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)*11)
		for i, obs := range chunk {
			base := i * 11
			valueRows[i] = fmt.Sprintf(
				"($%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d)",
				base+1, base+2, base+3, base+4, base+5, base+6,
				base+7, base+8, base+9, base+10, base+11,
			)
			args = append(args,
				obs.Project, obs.Machine, obs.RootPath, obs.GitRemote,
				obs.GitRemoteName, obs.WorktreeName, obs.WorktreeRootPath,
				obs.ObservedAt, obs.NormalizedRemote, obs.KeySource, obs.Key,
			)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO project_identity_observations (
				project, machine, root_path, git_remote, git_remote_name,
				worktree_name, worktree_root_path, observed_at,
				normalized_remote, key_source, key
			) VALUES `+strings.Join(valueRows, ",\n\t\t\t")+
			projectIdentityObservationConflictClause,
			args...,
		); err != nil {
			return fmt.Errorf(
				"upserting pg project identity observations: %w", err,
			)
		}
	}
	return nil
}
