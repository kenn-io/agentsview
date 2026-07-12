package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

const projectIdentityDeleteBatchSize = 300

func deleteProjectIdentityDelta(
	ctx context.Context,
	q pgProjectIdentityExecer,
	archiveID, databaseGeneration string,
	observationKeys []db.ProjectIdentityObservationKey,
	snapshotKeys []db.SessionProjectIdentitySnapshotKey,
) error {
	for start := 0; start < len(observationKeys); start += projectIdentityDeleteBatchSize {
		end := min(start+projectIdentityDeleteBatchSize, len(observationKeys))
		args := []any{archiveID}
		tuples := make([]string, 0, end-start)
		for _, key := range observationKeys[start:end] {
			base := len(args) + 1
			tuples = append(tuples, fmt.Sprintf(
				"($%d, $%d, $%d, $%d)", base, base+1, base+2, base+3,
			))
			args = append(args, key.Project, key.Machine, key.RootPath, key.GitRemote)
		}
		if _, err := q.ExecContext(ctx, `
			DELETE FROM source_project_identity_observations
			WHERE source_archive_id = $1
			  AND (project, machine, root_path, git_remote) IN (`+
			strings.Join(tuples, ", ")+`)`, args...); err != nil {
			return fmt.Errorf("deleting pg project identity observation delta: %w", err)
		}
	}
	for start := 0; start < len(snapshotKeys); start += projectIdentityDeleteBatchSize {
		end := min(start+projectIdentityDeleteBatchSize, len(snapshotKeys))
		args := []any{archiveID, databaseGeneration}
		placeholders := make([]string, 0, end-start)
		for _, key := range snapshotKeys[start:end] {
			args = append(args, key.SessionID)
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
		}
		if _, err := q.ExecContext(ctx, `
			DELETE FROM source_session_project_identity_snapshots
			WHERE source_archive_id = $1
			  AND source_database_generation = $2
			  AND source_session_id IN (`+strings.Join(placeholders, ", ")+`)`,
			args...,
		); err != nil {
			return fmt.Errorf("deleting pg session identity snapshot delta: %w", err)
		}
	}
	return nil
}

func deleteProjectIdentityScope(
	ctx context.Context,
	q pgProjectIdentityExecer,
	archiveID string,
	projects, excludeProjects []string,
) error {
	args := []any{archiveID}
	predicates := []string{"source_archive_id = $1"}
	appendSet := func(column string, values []string, negate bool) {
		if len(values) == 0 {
			return
		}
		placeholders := make([]string, 0, len(values))
		for _, value := range values {
			args = append(args, value)
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
		}
		op := "IN"
		if negate {
			op = "NOT IN"
		}
		predicates = append(predicates,
			column+" "+op+" ("+strings.Join(placeholders, ",")+")")
	}
	appendSet("project", projects, false)
	appendSet("project", excludeProjects, true)
	where := strings.Join(predicates, " AND ")
	for _, table := range []string{
		"source_project_identity_observations",
		"source_session_project_identity_snapshots",
	} {
		if _, err := q.ExecContext(ctx, "DELETE FROM "+table+" WHERE "+where, args...); err != nil {
			return fmt.Errorf("clearing pg %s publication scope: %w", table, err)
		}
	}
	return nil
}

type pgProjectIdentityExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func upsertSourceArchiveScope(
	ctx context.Context,
	q pgProjectIdentityExecer,
	archiveID, archiveSalt string,
) error {
	result, err := q.ExecContext(ctx, `
		INSERT INTO source_archives (source_archive_id, source_archive_salt)
		VALUES ($1, $2)
		ON CONFLICT (source_archive_id) DO UPDATE SET
			source_archive_salt = source_archives.source_archive_salt
		WHERE source_archives.source_archive_salt = EXCLUDED.source_archive_salt`,
		archiveID, archiveSalt,
	)
	if err != nil {
		return fmt.Errorf("upserting pg source archive scope: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking pg source archive scope: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("archive salt mismatch for %q", archiveID)
	}
	return nil
}

func upsertProjectIdentityObservation(
	ctx context.Context,
	q pgProjectIdentityExecer,
	obs export.ProjectIdentityObservation,
	excludeRemote string,
) error {
	if obs.GitRemote == "" && obs.RemoteResolution != export.ProjectResolutionAmbiguous {
		var exists bool
		if err := q.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM source_project_identity_observations
				WHERE source_archive_id = $1 AND project = $2
				  AND machine = $3 AND root_path = $4
				  AND git_remote != ''
				  AND ($5 = '' OR git_remote != $5)
			)`,
			obs.SourceArchiveID, obs.Project, obs.Machine, obs.RootPath, excludeRemote,
		).Scan(&exists); err != nil {
			return fmt.Errorf(
				"checking pg project identity remote observation: %w", err,
			)
		}
		if exists {
			return nil
		}
	} else if _, err := q.ExecContext(ctx, `
		DELETE FROM source_project_identity_observations
		WHERE source_archive_id = $1 AND project = $2
		  AND machine = $3 AND root_path = $4
		  AND git_remote = ''`,
		obs.SourceArchiveID, obs.Project, obs.Machine, obs.RootPath,
	); err != nil {
		return fmt.Errorf(
			"removing stale pg project identity root fallback: %w", err,
		)
	}

	if _, err := q.ExecContext(ctx, `
		INSERT INTO source_project_identity_observations (
			source_archive_id, source_archive_salt,
			project, machine, root_path, git_remote, git_remote_name,
			repository_path, worktree_name, worktree_root_path,
			worktree_relationship, checkout_state, git_branch,
			remote_resolution, remote_candidate_count, observed_at,
			normalized_remote, key_source, key
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
			$13, $14, $15, $16, $17, $18, $19
		)`+projectIdentityObservationConflictClause,
		obs.SourceArchiveID, obs.SourceArchiveSalt,
		obs.Project, obs.Machine, obs.RootPath, obs.GitRemote,
		obs.GitRemoteName, obs.RepositoryPath, obs.WorktreeName,
		obs.WorktreeRootPath, obs.WorktreeRelationship, obs.CheckoutState,
		obs.GitBranch, obs.RemoteResolution, obs.RemoteCandidateCount,
		obs.ObservedAt, obs.NormalizedRemote, obs.KeySource, obs.Key,
	); err != nil {
		return fmt.Errorf("upserting pg project identity observation: %w", err)
	}
	return nil
}

const projectIdentityObservationConflictClause = `
		ON CONFLICT (source_archive_id, project, machine, root_path, git_remote)
		DO UPDATE SET
			source_archive_salt = EXCLUDED.source_archive_salt,
			git_remote_name = EXCLUDED.git_remote_name,
			repository_path = EXCLUDED.repository_path,
			worktree_name = EXCLUDED.worktree_name,
			worktree_root_path = EXCLUDED.worktree_root_path,
			worktree_relationship = EXCLUDED.worktree_relationship,
			checkout_state = EXCLUDED.checkout_state,
			git_branch = EXCLUDED.git_branch,
			remote_resolution = EXCLUDED.remote_resolution,
			remote_candidate_count = EXCLUDED.remote_candidate_count,
			observed_at = EXCLUDED.observed_at,
			normalized_remote = EXCLUDED.normalized_remote,
			key_source = EXCLUDED.key_source,
			key = EXCLUDED.key`

// projectIdentityRootKey identifies the root a fallback (empty git_remote)
// observation competes with real-remote observations over.
type projectIdentityRootKey struct {
	archiveID string
	project   string
	machine   string
	rootPath  string
}

func observationRootKey(
	obs export.ProjectIdentityObservation,
) projectIdentityRootKey {
	return projectIdentityRootKey{
		archiveID: obs.SourceArchiveID,
		project:   obs.Project,
		machine:   obs.Machine,
		rootPath:  obs.RootPath,
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

// projectIdentityRootKeyBatchSize bounds tuple-IN lists at four bind
// parameters per key.
const projectIdentityRootKeyBatchSize = 300

// projectIdentityInsertBatchSize bounds multi-row upserts at nineteen bind
// parameters per row.
const projectIdentityInsertBatchSize = 500

const projectIdentitySnapshotInsertBatchSize = 500

func rootKeyTupleArgs(keys []projectIdentityRootKey) (string, []any) {
	tuples := make([]string, len(keys))
	args := make([]any, 0, len(keys)*4)
	for i, key := range keys {
		base := i * 4
		tuples[i] = fmt.Sprintf("($%d, $%d, $%d, $%d)",
			base+1, base+2, base+3, base+4)
		args = append(args, key.archiveID, key.project, key.machine, key.rootPath)
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
			DELETE FROM source_project_identity_observations
			WHERE git_remote = ''
			  AND (source_archive_id, project, machine, root_path) IN (`+tuples+`)`,
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
			SELECT DISTINCT source_archive_id, project, machine, root_path
			FROM source_project_identity_observations
			WHERE git_remote != ''
			  AND (source_archive_id, project, machine, root_path) IN (`+tuples+`)`,
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
			&key.archiveID, &key.project, &key.machine, &key.rootPath,
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
		args := make([]any, 0, len(chunk)*19)
		for i, obs := range chunk {
			base := i * 19
			valueRows[i] = fmt.Sprintf(
				"($%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d)",
				base+1, base+2, base+3, base+4, base+5, base+6,
				base+7, base+8, base+9, base+10, base+11, base+12,
				base+13, base+14, base+15, base+16, base+17, base+18,
				base+19,
			)
			args = append(args,
				obs.SourceArchiveID, obs.SourceArchiveSalt,
				obs.Project, obs.Machine, obs.RootPath, obs.GitRemote,
				obs.GitRemoteName, obs.RepositoryPath, obs.WorktreeName,
				obs.WorktreeRootPath, obs.WorktreeRelationship, obs.CheckoutState,
				obs.GitBranch, obs.RemoteResolution, obs.RemoteCandidateCount,
				obs.ObservedAt, obs.NormalizedRemote, obs.KeySource, obs.Key,
			)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO source_project_identity_observations (
				source_archive_id, source_archive_salt,
				project, machine, root_path, git_remote, git_remote_name,
				repository_path, worktree_name, worktree_root_path,
				worktree_relationship, checkout_state, git_branch,
				remote_resolution, remote_candidate_count, observed_at,
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

func insertSessionProjectIdentitySnapshots(
	ctx context.Context,
	tx *sql.Tx,
	archiveID, databaseGeneration string,
	snapshots []export.ProjectIdentityObservation,
) error {
	for start := 0; start < len(snapshots); start += projectIdentitySnapshotInsertBatchSize {
		end := min(start+projectIdentitySnapshotInsertBatchSize, len(snapshots))
		chunk := snapshots[start:end]
		valueRows := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)*20)
		for i, obs := range chunk {
			base := i * 20
			placeholders := make([]string, 20)
			for j := range placeholders {
				placeholders[j] = fmt.Sprintf("$%d", base+j+1)
			}
			valueRows[i] = "(" + strings.Join(placeholders, ", ") + ")"
			args = append(args,
				archiveID, databaseGeneration, obs.SessionID,
				obs.Project, obs.Machine, obs.RootPath, obs.GitRemote,
				obs.GitRemoteName, obs.RepositoryPath, obs.WorktreeName,
				obs.WorktreeRootPath, obs.WorktreeRelationship, obs.CheckoutState,
				obs.GitBranch, obs.RemoteResolution, obs.RemoteCandidateCount,
				obs.ObservedAt, obs.NormalizedRemote, obs.KeySource, obs.Key,
			)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO source_session_project_identity_snapshots (
				source_archive_id, source_database_generation, source_session_id,
				project, machine, root_path, git_remote, git_remote_name,
				repository_path, worktree_name, worktree_root_path,
				worktree_relationship, checkout_state, git_branch,
				remote_resolution, remote_candidate_count, observed_at,
				normalized_remote, key_source, key
			) VALUES `+strings.Join(valueRows, ",\n\t\t\t")+`
			ON CONFLICT (
				source_archive_id, source_database_generation, source_session_id
			) DO UPDATE SET
				project = EXCLUDED.project,
				machine = EXCLUDED.machine,
				root_path = EXCLUDED.root_path,
				git_remote = EXCLUDED.git_remote,
				git_remote_name = EXCLUDED.git_remote_name,
				repository_path = EXCLUDED.repository_path,
				worktree_name = EXCLUDED.worktree_name,
				worktree_root_path = EXCLUDED.worktree_root_path,
				worktree_relationship = EXCLUDED.worktree_relationship,
				checkout_state = EXCLUDED.checkout_state,
				git_branch = EXCLUDED.git_branch,
				remote_resolution = EXCLUDED.remote_resolution,
				remote_candidate_count = EXCLUDED.remote_candidate_count,
				observed_at = EXCLUDED.observed_at,
				normalized_remote = EXCLUDED.normalized_remote,
				key_source = EXCLUDED.key_source,
				key = EXCLUDED.key`, args...); err != nil {
			return fmt.Errorf("upserting pg session project identity snapshots: %w", err)
		}
	}
	return nil
}
