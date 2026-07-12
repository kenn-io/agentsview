package duckdb

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

const projectIdentityDeleteBatchSize = 300

func deleteProjectIdentityDelta(
	exec duckProjectIdentityExec,
	archiveID, databaseGeneration string,
	observationKeys []db.ProjectIdentityObservationKey,
	snapshotKeys []db.SessionProjectIdentitySnapshotKey,
) error {
	for start := 0; start < len(observationKeys); start += projectIdentityDeleteBatchSize {
		end := min(start+projectIdentityDeleteBatchSize, len(observationKeys))
		args := []any{archiveID}
		tuples := make([]string, 0, end-start)
		for _, key := range observationKeys[start:end] {
			tuples = append(tuples, "(?, ?, ?, ?)")
			args = append(args, key.Project, key.Machine, key.RootPath, key.GitRemote)
		}
		if err := exec(`
			DELETE FROM source_project_identity_observations
			WHERE source_archive_id = ?
			  AND (project, machine, root_path, git_remote) IN (`+
			strings.Join(tuples, ", ")+`)`, args...); err != nil {
			return fmt.Errorf("deleting duckdb project identity observation delta: %w", err)
		}
	}
	for start := 0; start < len(snapshotKeys); start += projectIdentityDeleteBatchSize {
		end := min(start+projectIdentityDeleteBatchSize, len(snapshotKeys))
		args := []any{archiveID, databaseGeneration}
		placeholders := make([]string, 0, end-start)
		for _, key := range snapshotKeys[start:end] {
			args = append(args, key.SessionID)
			placeholders = append(placeholders, "?")
		}
		if err := exec(`
			DELETE FROM source_session_project_identity_snapshots
			WHERE source_archive_id = ?
			  AND source_database_generation = ?
			  AND source_session_id IN (`+strings.Join(placeholders, ", ")+`)`,
			args...,
		); err != nil {
			return fmt.Errorf("deleting duckdb session identity snapshot delta: %w", err)
		}
	}
	return nil
}

func deleteProjectIdentityScope(
	exec duckProjectIdentityExec,
	archiveID string,
	projects, excludeProjects []string,
) error {
	args := []any{archiveID}
	predicates := []string{"source_archive_id = ?"}
	appendSet := func(column string, values []string, negate bool) {
		if len(values) == 0 {
			return
		}
		placeholders := make([]string, len(values))
		for i, value := range values {
			placeholders[i] = "?"
			args = append(args, value)
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
		if err := exec("DELETE FROM "+table+" WHERE "+where, args...); err != nil {
			return fmt.Errorf("clearing duckdb %s publication scope: %w", table, err)
		}
	}
	return nil
}

type duckProjectIdentityExec func(string, ...any) error
type duckProjectIdentityQueryRow func(string, ...any) *sql.Row

const projectIdentitySnapshotInsertBatchSize = 500

func upsertSourceArchiveScope(
	exec duckProjectIdentityExec,
	queryRow duckProjectIdentityQueryRow,
	archiveID, archiveSalt string,
) error {
	var existingSalt string
	err := queryRow(`
		SELECT source_archive_salt
		FROM source_archives
		WHERE source_archive_id = ?`, archiveID).Scan(&existingSalt)
	if err == nil {
		if existingSalt != archiveSalt {
			return fmt.Errorf("archive salt mismatch for %q", archiveID)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading duckdb source archive scope: %w", err)
	}
	if err := exec(`
		INSERT INTO source_archives (source_archive_id, source_archive_salt)
		VALUES (?, ?)
		ON CONFLICT(source_archive_id) DO NOTHING`,
		archiveID, archiveSalt,
	); err != nil {
		return fmt.Errorf("upserting duckdb source archive scope: %w", err)
	}
	if err := queryRow(`
		SELECT source_archive_salt
		FROM source_archives
		WHERE source_archive_id = ?`, archiveID).Scan(&existingSalt); err != nil {
		return fmt.Errorf("verifying duckdb source archive scope: %w", err)
	}
	if existingSalt != archiveSalt {
		return fmt.Errorf("archive salt mismatch for %q", archiveID)
	}
	return nil
}

func upsertSessionProjectIdentitySnapshots(
	exec duckProjectIdentityExec,
	archiveID, databaseGeneration string,
	snapshots []export.ProjectIdentityObservation,
) error {
	for start := 0; start < len(snapshots); start += projectIdentitySnapshotInsertBatchSize {
		end := min(start+projectIdentitySnapshotInsertBatchSize, len(snapshots))
		chunk := snapshots[start:end]
		valueRows := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)*20)
		for i, obs := range chunk {
			valueRows[i] = "(" + strings.TrimSuffix(strings.Repeat("?, ", 20), ", ") + ")"
			args = append(args,
				archiveID, databaseGeneration, obs.SessionID,
				obs.Project, obs.Machine, obs.RootPath, obs.GitRemote,
				obs.GitRemoteName, obs.RepositoryPath, obs.WorktreeName,
				obs.WorktreeRootPath, obs.WorktreeRelationship, obs.CheckoutState,
				obs.GitBranch, obs.RemoteResolution, obs.RemoteCandidateCount,
				obs.ObservedAt, obs.NormalizedRemote, obs.KeySource, obs.Key,
			)
		}
		if err := exec(`
			INSERT INTO source_session_project_identity_snapshots (
				source_archive_id, source_database_generation, source_session_id,
				project, machine, root_path, git_remote, git_remote_name,
				repository_path, worktree_name, worktree_root_path,
				worktree_relationship, checkout_state, git_branch,
				remote_resolution, remote_candidate_count, observed_at,
				normalized_remote, key_source, key
			) VALUES `+strings.Join(valueRows, ",\n\t\t\t")+`
			ON CONFLICT(
				source_archive_id, source_database_generation, source_session_id
			) DO UPDATE SET
				project = excluded.project,
				machine = excluded.machine,
				root_path = excluded.root_path,
				git_remote = excluded.git_remote,
				git_remote_name = excluded.git_remote_name,
				repository_path = excluded.repository_path,
				worktree_name = excluded.worktree_name,
				worktree_root_path = excluded.worktree_root_path,
				worktree_relationship = excluded.worktree_relationship,
				checkout_state = excluded.checkout_state,
				git_branch = excluded.git_branch,
				remote_resolution = excluded.remote_resolution,
				remote_candidate_count = excluded.remote_candidate_count,
				observed_at = excluded.observed_at,
				normalized_remote = excluded.normalized_remote,
				key_source = excluded.key_source,
				key = excluded.key`, args...); err != nil {
			return fmt.Errorf("upserting duckdb session project identity snapshots: %w", err)
		}
	}
	return nil
}

func upsertProjectIdentityObservation(
	exec duckProjectIdentityExec,
	queryRow duckProjectIdentityQueryRow,
	obs export.ProjectIdentityObservation,
	excludeRemote string,
) error {
	if obs.GitRemote == "" && obs.RemoteResolution != export.ProjectResolutionAmbiguous {
		var exists int
		if err := queryRow(`
			SELECT COUNT(*) FROM source_project_identity_observations
			WHERE source_archive_id = ? AND project = ?
			  AND machine = ? AND root_path = ?
			  AND (git_remote != '' OR remote_resolution = ?)
			  AND (? = '' OR git_remote != ?)`,
			obs.SourceArchiveID, obs.Project, obs.Machine, obs.RootPath,
			export.ProjectResolutionAmbiguous,
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
		DELETE FROM source_project_identity_observations
		WHERE source_archive_id = ? AND project = ?
		  AND machine = ? AND root_path = ?
		  AND git_remote = '' AND remote_resolution != ?`,
		obs.SourceArchiveID, obs.Project, obs.Machine, obs.RootPath,
		export.ProjectResolutionAmbiguous,
	); err != nil {
		return fmt.Errorf(
			"removing stale duckdb project identity root fallback: %w", err,
		)
	}

	if err := exec(`
		INSERT INTO source_project_identity_observations (
			source_archive_id, source_archive_salt,
			project, machine, root_path, git_remote, git_remote_name,
			repository_path, worktree_name, worktree_root_path,
			worktree_relationship, checkout_state, git_branch,
			remote_resolution, remote_candidate_count, observed_at,
			normalized_remote, key_source, key
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_archive_id, project, machine, root_path, git_remote) DO UPDATE SET
			source_archive_id = excluded.source_archive_id,
			source_archive_salt = excluded.source_archive_salt,
			git_remote_name = excluded.git_remote_name,
			repository_path = excluded.repository_path,
			worktree_name = excluded.worktree_name,
			worktree_root_path = excluded.worktree_root_path,
			worktree_relationship = excluded.worktree_relationship,
			checkout_state = excluded.checkout_state,
			git_branch = excluded.git_branch,
			remote_resolution = excluded.remote_resolution,
			remote_candidate_count = excluded.remote_candidate_count,
			observed_at = excluded.observed_at,
			normalized_remote = excluded.normalized_remote,
			key_source = excluded.key_source,
			key = excluded.key`,
		obs.SourceArchiveID, obs.SourceArchiveSalt,
		obs.Project, obs.Machine, obs.RootPath, obs.GitRemote,
		obs.GitRemoteName, obs.RepositoryPath, obs.WorktreeName,
		obs.WorktreeRootPath, obs.WorktreeRelationship, obs.CheckoutState,
		obs.GitBranch, obs.RemoteResolution, obs.RemoteCandidateCount,
		obs.ObservedAt, obs.NormalizedRemote, obs.KeySource, obs.Key,
	); err != nil {
		return fmt.Errorf("upserting duckdb project identity observation: %w", err)
	}
	return nil
}
