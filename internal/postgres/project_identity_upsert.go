package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

const projectIdentityDeleteBatchSize = 300

func deleteProjectIdentityDelta(
	ctx context.Context,
	q bun.IDB,
	archiveID, databaseGeneration string,
	observationKeys []db.ProjectIdentityObservationKey,
	snapshotKeys []db.SessionProjectIdentitySnapshotKey,
) error {
	for start := 0; start < len(observationKeys); start += projectIdentityDeleteBatchSize {
		end := min(start+projectIdentityDeleteBatchSize, len(observationKeys))
		args := []any{archiveID}
		tuples := make([]string, 0, end-start)
		for _, key := range observationKeys[start:end] {
			base := len(args)
			tuples = append(tuples, fmt.Sprintf(
				"(?%d, ?%d, ?%d, ?%d)", base, base+1, base+2, base+3,
			))
			args = append(args, key.Project, key.Machine, key.RootPath, key.GitRemote)
		}
		if _, err := q.ExecContext(ctx, `
			DELETE FROM source_project_identity_observations
			WHERE source_archive_id = ?0
			  AND (project, machine, root_path, git_remote) IN (`+
			strings.Join(tuples, ", ")+`)`, args...); err != nil {
			return fmt.Errorf("deleting pg project identity observation delta: %w", err)
		}
	}
	for start := 0; start < len(snapshotKeys); start += projectIdentityDeleteBatchSize {
		end := min(start+projectIdentityDeleteBatchSize, len(snapshotKeys))
		args := []any{archiveID, databaseGeneration}
		tuples := make([]string, 0, end-start)
		for _, key := range snapshotKeys[start:end] {
			base := len(args)
			tuples = append(tuples, fmt.Sprintf("(?%d, ?%d)", base, base+1))
			args = append(args, key.SessionID, key.Project)
		}
		if _, err := q.ExecContext(ctx, `
			DELETE FROM source_session_project_identity_snapshots
			WHERE source_archive_id = ?0
			  AND source_database_generation = ?1
			  AND (source_session_id, project) IN (`+
			strings.Join(tuples, ", ")+`)`,
			args...,
		); err != nil {
			return fmt.Errorf("deleting pg session identity snapshot delta: %w", err)
		}
	}
	return nil
}

func deleteProjectIdentityArchive(
	ctx context.Context,
	q bun.IDB,
	archiveID string,
) error {
	for _, table := range []string{
		"source_project_identity_observations",
		"source_session_project_identity_snapshots",
	} {
		if _, err := q.ExecContext(ctx,
			"DELETE FROM "+table+" WHERE source_archive_id = ?0",
			archiveID,
		); err != nil {
			return fmt.Errorf("clearing pg %s archive: %w", table, err)
		}
	}
	return nil
}

func prepareFilteredProjectIdentityPublication(
	ctx context.Context,
	q bun.IDB,
	archiveID, databaseGeneration, publicationScope string,
	full, adoptLegacyScope bool,
	projects, excludeProjects []string,
	observationKeys []db.ProjectIdentityObservationKey,
	snapshotKeys []db.SessionProjectIdentitySnapshotKey,
	refreshSessionIDs []string,
) error {
	if full {
		if adoptLegacyScope {
			if err := adoptLegacyFilteredProjectIdentityScope(
				ctx, q, archiveID, publicationScope,
				projects, excludeProjects,
			); err != nil {
				return err
			}
		}
		if err := releaseFilteredProjectIdentityFullOwnership(
			ctx, q, archiveID, publicationScope,
		); err != nil {
			return err
		}
	} else {
		if err := deleteFilteredProjectIdentityDeltaOwnership(
			ctx, q, archiveID, databaseGeneration, publicationScope,
			observationKeys, snapshotKeys,
		); err != nil {
			return err
		}
	}
	if err := deleteFilteredSnapshotOwnershipBySessionID(
		ctx, q, archiveID, publicationScope, refreshSessionIDs,
	); err != nil {
		return err
	}
	return nil
}

// adoptLegacyFilteredProjectIdentityScope assigns ownerless rows written by
// the v2 publisher to the filter that previously managed them. The subsequent
// full reconciliation removes stale rows before publishing the current scope;
// a successful v3 cursor write prevents this bounded adoption from recurring.
func adoptLegacyFilteredProjectIdentityScope(
	ctx context.Context,
	q bun.IDB,
	archiveID, publicationScope string,
	projects, excludeProjects []string,
) error {
	args := []any{archiveID, publicationScope}
	values := projects
	operator := "IN"
	if len(values) == 0 {
		values = excludeProjects
		operator = "NOT IN"
	}
	placeholders := make([]string, 0, len(values))
	for _, project := range values {
		args = append(args, project)
		placeholders = append(placeholders, fmt.Sprintf("?%d", len(args)-1))
	}
	projectPredicate := operator + " (" + strings.Join(placeholders, ", ") + ")"
	if _, err := q.ExecContext(ctx, `
		INSERT INTO source_project_identity_observation_scopes (
			source_archive_id, project, machine, root_path, git_remote,
			publication_scope
		)
		SELECT observation.source_archive_id, observation.project,
			observation.machine, observation.root_path, observation.git_remote, ?1
		FROM source_project_identity_observations observation
		WHERE observation.source_archive_id = ?0
		  AND observation.project `+projectPredicate+`
		  AND NOT EXISTS (
			SELECT 1
			FROM source_project_identity_observation_scopes owner
			WHERE owner.source_archive_id = observation.source_archive_id
			  AND owner.project = observation.project
			  AND owner.machine = observation.machine
			  AND owner.root_path = observation.root_path
			  AND owner.git_remote = observation.git_remote
		  )
		ON CONFLICT DO NOTHING`, args...); err != nil {
		return fmt.Errorf("adopting legacy pg identity observations: %w", err)
	}

	if _, err := q.ExecContext(ctx, `
		INSERT INTO source_session_project_identity_snapshot_scopes (
			source_archive_id, source_database_generation,
			source_session_id, publication_scope
		)
		SELECT snapshot.source_archive_id, snapshot.source_database_generation,
			snapshot.source_session_id, ?1
		FROM source_session_project_identity_snapshots snapshot
		WHERE snapshot.source_archive_id = ?0
		  AND snapshot.project `+projectPredicate+`
		  AND NOT EXISTS (
			SELECT 1
			FROM source_session_project_identity_snapshot_scopes owner
			WHERE owner.source_archive_id = snapshot.source_archive_id
			  AND owner.source_database_generation =
			      snapshot.source_database_generation
			  AND owner.source_session_id = snapshot.source_session_id
		  )
		ON CONFLICT DO NOTHING`, args...); err != nil {
		return fmt.Errorf("adopting legacy pg identity snapshots: %w", err)
	}
	return nil
}

func releaseFilteredProjectIdentityFullOwnership(
	ctx context.Context,
	q bun.IDB,
	archiveID, publicationScope string,
) error {
	if _, err := q.ExecContext(ctx, `
		DELETE FROM source_project_identity_observations observation
		WHERE observation.source_archive_id = ?0
		  AND EXISTS (
			SELECT 1
			FROM source_project_identity_observation_scopes owner
			WHERE owner.source_archive_id = observation.source_archive_id
			  AND owner.project = observation.project
			  AND owner.machine = observation.machine
			  AND owner.root_path = observation.root_path
			  AND owner.git_remote = observation.git_remote
			  AND owner.publication_scope = ?1
		  )
		  AND NOT EXISTS (
			SELECT 1
			FROM source_project_identity_observation_scopes owner
			WHERE owner.source_archive_id = observation.source_archive_id
			  AND owner.project = observation.project
			  AND owner.machine = observation.machine
			  AND owner.root_path = observation.root_path
			  AND owner.git_remote = observation.git_remote
			  AND owner.publication_scope <> ?1
		  )`, archiveID, publicationScope); err != nil {
		return fmt.Errorf(
			"clearing exclusively owned filtered pg identity observations: %w",
			err,
		)
	}
	if _, err := q.ExecContext(ctx, `
		DELETE FROM source_session_project_identity_snapshots snapshot
		WHERE snapshot.source_archive_id = ?0
		  AND EXISTS (
			SELECT 1
			FROM source_session_project_identity_snapshot_scopes owner
			WHERE owner.source_archive_id = snapshot.source_archive_id
			  AND owner.source_database_generation =
			      snapshot.source_database_generation
			  AND owner.source_session_id = snapshot.source_session_id
			  AND owner.publication_scope = ?1
		  )
		  AND NOT EXISTS (
			SELECT 1
			FROM source_session_project_identity_snapshot_scopes owner
			WHERE owner.source_archive_id = snapshot.source_archive_id
			  AND owner.source_database_generation =
			      snapshot.source_database_generation
			  AND owner.source_session_id = snapshot.source_session_id
			  AND owner.publication_scope <> ?1
		  )`, archiveID, publicationScope); err != nil {
		return fmt.Errorf(
			"clearing exclusively owned filtered pg identity snapshots: %w", err,
		)
	}
	for _, table := range []string{
		"source_project_identity_observation_scopes",
		"source_session_project_identity_snapshot_scopes",
	} {
		if _, err := q.ExecContext(ctx,
			"DELETE FROM "+table+
				" WHERE source_archive_id = ?0 AND publication_scope = ?1",
			archiveID, publicationScope,
		); err != nil {
			return fmt.Errorf("clearing filtered pg %s scope: %w", table, err)
		}
	}
	return nil
}

func deleteFilteredProjectIdentityDeltaOwnership(
	ctx context.Context,
	q bun.IDB,
	archiveID, databaseGeneration, publicationScope string,
	observationKeys []db.ProjectIdentityObservationKey,
	snapshotKeys []db.SessionProjectIdentitySnapshotKey,
) error {
	for start := 0; start < len(observationKeys); start += projectIdentityDeleteBatchSize {
		end := min(start+projectIdentityDeleteBatchSize, len(observationKeys))
		args := []any{archiveID, publicationScope}
		tuples := make([]string, 0, end-start)
		for _, key := range observationKeys[start:end] {
			base := len(args)
			tuples = append(tuples, fmt.Sprintf(
				"(?%d, ?%d, ?%d, ?%d)", base, base+1, base+2, base+3,
			))
			args = append(args, key.Project, key.Machine, key.RootPath, key.GitRemote)
		}
		if _, err := q.ExecContext(ctx, `
			DELETE FROM source_project_identity_observations observation
			WHERE observation.source_archive_id = ?0
			  AND (observation.project, observation.machine,
			       observation.root_path, observation.git_remote) IN (`+
			strings.Join(tuples, ", ")+`)
			  AND EXISTS (
				SELECT 1
				FROM source_project_identity_observation_scopes owner
				WHERE owner.source_archive_id = observation.source_archive_id
				  AND owner.project = observation.project
				  AND owner.machine = observation.machine
				  AND owner.root_path = observation.root_path
				  AND owner.git_remote = observation.git_remote
				  AND owner.publication_scope = ?1
			  )
			  AND NOT EXISTS (
				SELECT 1
				FROM source_project_identity_observation_scopes owner
				WHERE owner.source_archive_id = observation.source_archive_id
				  AND owner.project = observation.project
				  AND owner.machine = observation.machine
				  AND owner.root_path = observation.root_path
				  AND owner.git_remote = observation.git_remote
				  AND owner.publication_scope <> ?1
			  )`, args...); err != nil {
			return fmt.Errorf(
				"deleting exclusively owned filtered pg identity delta: %w", err,
			)
		}
		if _, err := q.ExecContext(ctx, `
			DELETE FROM source_project_identity_observation_scopes
			WHERE source_archive_id = ?0 AND publication_scope = ?1
			  AND (project, machine, root_path, git_remote) IN (`+
			strings.Join(tuples, ", ")+`)`, args...); err != nil {
			return fmt.Errorf(
				"deleting filtered pg project identity ownership delta: %w", err)
		}
	}
	for start := 0; start < len(snapshotKeys); start += projectIdentityDeleteBatchSize {
		end := min(start+projectIdentityDeleteBatchSize, len(snapshotKeys))
		args := []any{archiveID, databaseGeneration, publicationScope}
		placeholders := make([]string, 0, end-start)
		for _, key := range snapshotKeys[start:end] {
			args = append(args, key.SessionID)
			placeholders = append(
				placeholders, fmt.Sprintf("?%d", len(args)-1))
		}
		if _, err := q.ExecContext(ctx, `
			DELETE FROM source_session_project_identity_snapshots snapshot
			WHERE snapshot.source_archive_id = ?0
			  AND snapshot.source_database_generation = ?1
			  AND snapshot.source_session_id IN (`+
			strings.Join(placeholders, ", ")+`)
			  AND EXISTS (
				SELECT 1
				FROM source_session_project_identity_snapshot_scopes owner
				WHERE owner.source_archive_id = snapshot.source_archive_id
				  AND owner.source_database_generation =
				      snapshot.source_database_generation
				  AND owner.source_session_id = snapshot.source_session_id
				  AND owner.publication_scope = ?2
			  )
			  AND NOT EXISTS (
				SELECT 1
				FROM source_session_project_identity_snapshot_scopes owner
				WHERE owner.source_archive_id = snapshot.source_archive_id
				  AND owner.source_database_generation =
				      snapshot.source_database_generation
				  AND owner.source_session_id = snapshot.source_session_id
				  AND owner.publication_scope <> ?2
			  )`, args...); err != nil {
			return fmt.Errorf(
				"deleting exclusively owned filtered pg snapshot delta: %w", err,
			)
		}
		if _, err := q.ExecContext(ctx, `
			DELETE FROM source_session_project_identity_snapshot_scopes
			WHERE source_archive_id = ?0
			  AND source_database_generation = ?1
			  AND publication_scope = ?2
			  AND source_session_id IN (`+
			strings.Join(placeholders, ", ")+`)`, args...); err != nil {
			return fmt.Errorf(
				"deleting filtered pg session identity ownership delta: %w", err)
		}
	}
	return nil
}

func deleteFilteredSnapshotOwnershipBySessionID(
	ctx context.Context,
	q bun.IDB,
	archiveID, publicationScope string,
	sessionIDs []string,
) error {
	for start := 0; start < len(sessionIDs); start += projectIdentityDeleteBatchSize {
		end := min(start+projectIdentityDeleteBatchSize, len(sessionIDs))
		args := []any{archiveID, publicationScope}
		placeholders := make([]string, 0, end-start)
		for _, sessionID := range sessionIDs[start:end] {
			args = append(args, sessionID)
			placeholders = append(
				placeholders, fmt.Sprintf("?%d", len(args)-1))
		}
		if _, err := q.ExecContext(ctx, `
			DELETE FROM source_session_project_identity_snapshots snapshot
			WHERE snapshot.source_archive_id = ?0
			  AND snapshot.source_session_id IN (`+
			strings.Join(placeholders, ", ")+`)
			  AND EXISTS (
				SELECT 1
				FROM source_session_project_identity_snapshot_scopes owner
				WHERE owner.source_archive_id = snapshot.source_archive_id
				  AND owner.source_database_generation =
				      snapshot.source_database_generation
				  AND owner.source_session_id = snapshot.source_session_id
				  AND owner.publication_scope = ?1
			  )
			  AND NOT EXISTS (
				SELECT 1
				FROM source_session_project_identity_snapshot_scopes owner
				WHERE owner.source_archive_id = snapshot.source_archive_id
				  AND owner.source_database_generation =
				      snapshot.source_database_generation
				  AND owner.source_session_id = snapshot.source_session_id
				  AND owner.publication_scope <> ?1
			  )`, args...); err != nil {
			return fmt.Errorf(
				"deleting exclusively owned filtered pg refreshed snapshots: %w",
				err,
			)
		}
		if _, err := q.ExecContext(ctx, `
			DELETE FROM source_session_project_identity_snapshot_scopes
			WHERE source_archive_id = ?0 AND publication_scope = ?1
			  AND source_session_id IN (`+
			strings.Join(placeholders, ", ")+`)`, args...); err != nil {
			return fmt.Errorf(
				"deleting filtered pg session identity refresh ownership: %w",
				err,
			)
		}
	}
	return nil
}

func ownProjectIdentityObservations(
	ctx context.Context,
	tx bun.IDB,
	archiveID, publicationScope string,
	observations []export.ProjectIdentityObservation,
) error {
	for start := 0; start < len(observations); start += projectIdentityDeleteBatchSize {
		end := min(start+projectIdentityDeleteBatchSize, len(observations))
		args := []any{archiveID, publicationScope}
		tuples := make([]string, 0, end-start)
		for _, observation := range observations[start:end] {
			base := len(args)
			tuples = append(tuples, fmt.Sprintf(
				"(?%d, ?%d, ?%d, ?%d)", base, base+1, base+2, base+3,
			))
			args = append(args,
				observation.Project, observation.Machine,
				observation.RootPath, observation.GitRemote,
			)
		}
		if _, err := tx.ExecContext(ctx, `
			WITH keys(project, machine, root_path, git_remote) AS (
				VALUES `+strings.Join(tuples, ", ")+`
			)
			INSERT INTO source_project_identity_observation_scopes (
				source_archive_id, project, machine, root_path, git_remote,
				publication_scope
			)
			SELECT ?0, keys.project, keys.machine, keys.root_path,
				keys.git_remote, ?1
			FROM keys
			JOIN source_project_identity_observations observation
			  ON observation.source_archive_id = ?0
			 AND observation.project = keys.project
			 AND observation.machine = keys.machine
			 AND observation.root_path = keys.root_path
			 AND observation.git_remote = keys.git_remote
			ON CONFLICT DO NOTHING`, args...); err != nil {
			return fmt.Errorf("owning pg project identity observations: %w", err)
		}
	}
	return nil
}

func ownSessionProjectIdentitySnapshots(
	ctx context.Context,
	tx bun.IDB,
	archiveID, databaseGeneration, publicationScope string,
	snapshots []export.ProjectIdentityObservation,
) error {
	for start := 0; start < len(snapshots); start += projectIdentityDeleteBatchSize {
		end := min(start+projectIdentityDeleteBatchSize, len(snapshots))
		args := []any{archiveID, databaseGeneration, publicationScope}
		placeholders := make([]string, 0, end-start)
		for _, snapshot := range snapshots[start:end] {
			args = append(args, snapshot.SessionID)
			placeholders = append(
				placeholders, fmt.Sprintf("(?%d)", len(args)-1))
		}
		if _, err := tx.ExecContext(ctx, `
			WITH keys(source_session_id) AS (
				VALUES `+strings.Join(placeholders, ", ")+`
			)
			INSERT INTO source_session_project_identity_snapshot_scopes (
				source_archive_id, source_database_generation,
				source_session_id, publication_scope
			)
			SELECT ?0, ?1, keys.source_session_id, ?2
			FROM keys
			JOIN source_session_project_identity_snapshots snapshot
			  ON snapshot.source_archive_id = ?0
			 AND snapshot.source_database_generation = ?1
			 AND snapshot.source_session_id = keys.source_session_id
			ON CONFLICT DO NOTHING`, args...); err != nil {
			return fmt.Errorf("owning pg session identity snapshots: %w", err)
		}
	}
	return nil
}

func deleteSessionProjectIdentitySnapshotsBySessionID(
	ctx context.Context,
	q bun.IDB,
	archiveID string,
	sessionIDs []string,
) error {
	for start := 0; start < len(sessionIDs); start += projectIdentityDeleteBatchSize {
		end := min(start+projectIdentityDeleteBatchSize, len(sessionIDs))
		args := []any{archiveID}
		placeholders := make([]string, 0, end-start)
		for _, sessionID := range sessionIDs[start:end] {
			args = append(args, sessionID)
			placeholders = append(placeholders, fmt.Sprintf("?%d", len(args)-1))
		}
		if _, err := q.ExecContext(ctx, `
			DELETE FROM source_session_project_identity_snapshots
			WHERE source_archive_id = ?0
			  AND source_session_id IN (`+
			strings.Join(placeholders, ", ")+`)`, args...); err != nil {
			return fmt.Errorf(
				"deleting pg session identity snapshots by session id: %w", err,
			)
		}
	}
	return nil
}

func upsertProjectIdentityObservation(
	ctx context.Context,
	q bun.IDB,
	obs export.ProjectIdentityObservation,
	excludeRemote string,
) error {
	if obs.GitRemote == "" && obs.RemoteResolution != export.ProjectResolutionAmbiguous {
		var exists bool
		if err := q.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM source_project_identity_observations
				WHERE source_archive_id = ?0 AND project = ?1
				  AND machine = ?2 AND root_path = ?3
				  AND (git_remote != '' OR remote_resolution = ?4)
				  AND (?5 = '' OR git_remote != ?5)
			)`,
			obs.SourceArchiveID, obs.Project, obs.Machine, obs.RootPath,
			export.ProjectResolutionAmbiguous, excludeRemote,
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
		WHERE source_archive_id = ?0 AND project = ?1
		  AND machine = ?2 AND root_path = ?3
		  AND git_remote = '' AND remote_resolution != ?4`,
		obs.SourceArchiveID, obs.Project, obs.Machine, obs.RootPath,
		export.ProjectResolutionAmbiguous,
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
			?0, ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11,
			?12, ?13, ?14, ?15, ?16, ?17, ?18
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
	// ambiguous holds deduped empty-remote observations that must coexist
	// with real-remote evidence for the same root.
	ambiguous []export.ProjectIdentityObservation
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
// observation per conflict key wins. Ordinary empty-remote fallbacks never
// survive alongside real-remote evidence for the same root, while ambiguous
// observations always survive because they are conflicting evidence rather
// than root-derived fallbacks.
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
		previous, seen := latest[key]
		if !seen {
			keyOrder = append(keyOrder, key)
		} else if key.gitRemote == "" &&
			previous.RemoteResolution == export.ProjectResolutionAmbiguous &&
			obs.RemoteResolution != export.ProjectResolutionAmbiguous {
			continue
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
		if obs.RemoteResolution == export.ProjectResolutionAmbiguous {
			plan.ambiguous = append(plan.ambiguous, obs)
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
	tx bun.IDB,
	store bun.IDB,
	archiveID, archiveSalt string,
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
	unconditional := make([]export.ProjectIdentityObservation, 0,
		len(plan.realRemote)+len(plan.ambiguous))
	unconditional = append(unconditional, plan.realRemote...)
	unconditional = append(unconditional, plan.ambiguous...)
	if err := insertProjectIdentityObservations(
		ctx, store, archiveID, archiveSalt, unconditional,
	); err != nil {
		return err
	}
	return insertProjectIdentityObservations(
		ctx, store, archiveID, archiveSalt, fallbacks,
	)
}

// projectIdentityRootKeyBatchSize bounds tuple-IN lists at four bind
// parameters per key.
const projectIdentityRootKeyBatchSize = 300

func rootKeyTupleArgs(keys []projectIdentityRootKey) (string, []any) {
	tuples := make([]string, len(keys))
	args := make([]any, 0, len(keys)*4)
	for i, key := range keys {
		base := i * 4
		tuples[i] = fmt.Sprintf("(?%d, ?%d, ?%d, ?%d)",
			base, base+1, base+2, base+3)
		args = append(args, key.archiveID, key.project, key.machine, key.rootPath)
	}
	return strings.Join(tuples, ", "), args
}

func deleteProjectIdentityFallbackRows(
	ctx context.Context,
	tx bun.IDB,
	roots []projectIdentityRootKey,
) error {
	for start := 0; start < len(roots); start += projectIdentityRootKeyBatchSize {
		end := min(start+projectIdentityRootKeyBatchSize, len(roots))
		tuples, args := rootKeyTupleArgs(roots[start:end])
		ambiguousParam := len(args)
		args = append(args, export.ProjectResolutionAmbiguous)
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM source_project_identity_observations
			WHERE git_remote = ''
			  AND remote_resolution != ?`+fmt.Sprint(ambiguousParam)+`
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
	tx bun.IDB,
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
		ambiguousParam := len(args)
		args = append(args, export.ProjectResolutionAmbiguous)
		rows, err := tx.QueryContext(ctx, `
			SELECT DISTINCT source_archive_id, project, machine, root_path
			FROM source_project_identity_observations
			WHERE (git_remote != '' OR remote_resolution = ?`+
			fmt.Sprint(ambiguousParam)+`)
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
	store bun.IDB,
	archiveID, archiveSalt string,
	observations []export.ProjectIdentityObservation,
) error {
	rows, err := db.CanonicalProjectIdentityObservationRows(
		archiveID, archiveSalt, observations,
	)
	if err != nil {
		return err
	}
	return db.UpsertProjectIdentityObservationRows(ctx, store, rows)
}

func insertSessionProjectIdentitySnapshots(
	ctx context.Context,
	store bun.IDB,
	archiveID, databaseGeneration string,
	snapshots []export.ProjectIdentityObservation,
) error {
	rows, err := db.CanonicalSessionProjectIdentitySnapshotRows(
		archiveID, databaseGeneration, snapshots,
	)
	if err != nil {
		return err
	}
	return db.UpsertSessionProjectIdentitySnapshotRows(ctx, store, rows)
}
