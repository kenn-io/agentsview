package duckdb

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
		tuples := make([]string, 0, end-start)
		for _, key := range snapshotKeys[start:end] {
			args = append(args, key.SessionID, key.Project)
			tuples = append(tuples, "(?, ?)")
		}
		if err := exec(`
			DELETE FROM source_session_project_identity_snapshots
			WHERE source_archive_id = ?
			  AND source_database_generation = ?
			  AND (source_session_id, project) IN (`+
			strings.Join(tuples, ", ")+`)`,
			args...,
		); err != nil {
			return fmt.Errorf("deleting duckdb session identity snapshot delta: %w", err)
		}
	}
	return nil
}

func deleteProjectIdentityArchive(
	exec duckProjectIdentityExec,
	archiveID string,
) error {
	for _, table := range []string{
		"source_project_identity_observations",
		"source_session_project_identity_snapshots",
	} {
		if err := exec(
			"DELETE FROM "+table+" WHERE source_archive_id = ?",
			archiveID,
		); err != nil {
			return fmt.Errorf("clearing duckdb %s archive: %w", table, err)
		}
	}
	return nil
}

func deleteSessionProjectIdentitySnapshotsBySessionID(
	exec duckProjectIdentityExec,
	archiveID string,
	sessionIDs []string,
) error {
	for start := 0; start < len(sessionIDs); start += projectIdentityDeleteBatchSize {
		end := min(start+projectIdentityDeleteBatchSize, len(sessionIDs))
		args := []any{archiveID}
		placeholders := make([]string, 0, end-start)
		for _, sessionID := range sessionIDs[start:end] {
			args = append(args, sessionID)
			placeholders = append(placeholders, "?")
		}
		if err := exec(`
			DELETE FROM source_session_project_identity_snapshots
			WHERE source_archive_id = ?
			  AND source_session_id IN (`+
			strings.Join(placeholders, ", ")+`)`, args...); err != nil {
			return fmt.Errorf(
				"deleting duckdb session identity snapshots by session id: %w",
				err,
			)
		}
	}
	return nil
}

type duckProjectIdentityExec func(string, ...any) error
type duckProjectIdentityQueryRow func(string, ...any) *sql.Row

func upsertSourceArchiveScope(
	ctx context.Context,
	store bun.IDB,
	archiveID, archiveSalt string,
) error {
	return db.UpsertSourceArchiveRow(ctx, store, archiveID, archiveSalt)
}

func upsertSessionProjectIdentitySnapshots(
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

func upsertProjectIdentityObservation(
	ctx context.Context,
	store bun.IDB,
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

	rows, err := db.CanonicalProjectIdentityObservationRows(
		obs.SourceArchiveID, obs.SourceArchiveSalt,
		[]export.ProjectIdentityObservation{obs},
	)
	if err != nil {
		return err
	}
	return db.UpsertProjectIdentityObservationRows(ctx, store, rows)
}
