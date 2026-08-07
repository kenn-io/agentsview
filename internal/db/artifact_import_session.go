package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ArtifactImportedSessionResult reports whether an import replaced normalized
// state or was durably suppressed to preserve an existing local session.
type ArtifactImportedSessionResult struct {
	Written         bool
	WrittenMessages int
	Suppressed      bool
}

// ApplyArtifactImportedSession atomically checks ownership, writes one
// imported session when safe, and records its exact manifest provenance.
func (db *DB) ApplyArtifactImportedSession(
	ctx context.Context,
	imported ArtifactImportedSession,
	write SessionBatchWrite,
) (ArtifactImportedSessionResult, error) {
	return db.applyArtifactImportedSession(ctx, nil, nil, imported, write)
}

// ApplyStagedArtifactImportedSession applies one staged import outcome and
// satisfies that exact checkpoint entry in the same transaction.
func (db *DB) ApplyStagedArtifactImportedSession(
	ctx context.Context,
	landing ArtifactCheckpointLanding,
	staged ArtifactCheckpointSession,
	imported ArtifactImportedSession,
	write SessionBatchWrite,
) (ArtifactImportedSessionResult, error) {
	if err := validateArtifactCheckpointLanding(landing); err != nil {
		return ArtifactImportedSessionResult{}, err
	}
	if err := validateArtifactCheckpointSession(landing.Origin, staged); err != nil {
		return ArtifactImportedSessionResult{}, err
	}
	if imported.Origin != landing.Origin ||
		imported.GID != staged.GID ||
		imported.ManifestHash != staged.ManifestHash {
		return ArtifactImportedSessionResult{},
			errors.New("staged artifact import does not match provenance")
	}
	return db.applyArtifactImportedSession(
		ctx, &landing, &staged, imported, write,
	)
}

func (db *DB) applyArtifactImportedSession(
	ctx context.Context,
	landing *ArtifactCheckpointLanding,
	staged *ArtifactCheckpointSession,
	imported ArtifactImportedSession,
	write SessionBatchWrite,
) (ArtifactImportedSessionResult, error) {
	var result ArtifactImportedSessionResult
	if err := db.requireWritable(); err != nil {
		return result, err
	}
	if err := validateArtifactImportedSession(imported); err != nil {
		return result, err
	}
	if write.Session.ID != imported.ImportedSessionID ||
		write.Session.ID != imported.GID ||
		write.Session.Machine != imported.Origin {
		return result, errors.New(
			"artifact imported session write does not match provenance",
		)
	}
	identity, err := db.localArchiveIdentity(ctx)
	if err != nil {
		return result, err
	}
	stampSessionArchiveIdentity(&write.Session, identity)
	write = sanitizeSessionBatchWrite(write)

	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("beginning artifact imported session: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var machine string
	err = tx.QueryRowContext(ctx, `
		SELECT machine FROM sessions WHERE id = ?`,
		write.Session.ID,
	).Scan(&machine)
	switch {
	case err == nil && machine != imported.Origin:
		if err := recordArtifactImportedSessionTx(ctx, tx, imported); err != nil {
			return result, err
		}
		if err := satisfyArtifactCheckpointImportTx(
			ctx, tx, landing, staged, imported,
		); err != nil {
			return result, err
		}
		if err := tx.Commit(); err != nil {
			return result, fmt.Errorf(
				"committing suppressed artifact imported session: %w", err,
			)
		}
		result.Suppressed = true
		return result, nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return result, fmt.Errorf("checking artifact imported session ownership: %w", err)
	}

	var pendingRecallRevocations recallEvidenceRevocationEvents
	messagesWritten, err := writeOneSessionBatchTx(
		tx, write, &pendingRecallRevocations,
	)
	switch {
	case err == nil:
		result.Written = true
		result.WrittenMessages = messagesWritten
	case errors.Is(err, ErrSessionExcluded):
		result.Suppressed = true
	case errors.Is(err, ErrSessionTrashed):
		return ArtifactImportedSessionResult{}, ErrSessionTrashed
	default:
		return result, err
	}
	if err := recordArtifactImportedSessionTx(ctx, tx, imported); err != nil {
		return ArtifactImportedSessionResult{}, err
	}
	if err := satisfyArtifactCheckpointImportTx(
		ctx, tx, landing, staged, imported,
	); err != nil {
		return ArtifactImportedSessionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ArtifactImportedSessionResult{},
			fmt.Errorf("committing artifact imported session: %w", err)
	}
	pendingRecallRevocations.flush()
	return result, nil
}

func satisfyArtifactCheckpointImportTx(
	ctx context.Context,
	tx *sql.Tx,
	landing *ArtifactCheckpointLanding,
	staged *ArtifactCheckpointSession,
	imported ArtifactImportedSession,
) error {
	if landing == nil || staged == nil {
		return satisfyAllArtifactCheckpointStagesTx(ctx, tx, imported)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE artifact_checkpoint_stage_sessions
		SET satisfied = 1
		WHERE origin = ? AND sequence = ? AND gid = ? AND manifest_hash = ?
		  AND satisfied = 0
		  AND EXISTS (
			SELECT 1 FROM artifact_checkpoint_stages
			WHERE origin = ? AND sequence = ?
			  AND checkpoint_sha256 = ? AND checkpoint_size = ?
			  AND complete = 1
		  )`,
		landing.Origin, landing.Sequence, staged.GID, staged.ManifestHash,
		landing.Origin, landing.Sequence,
		landing.CheckpointSHA256, landing.CheckpointSize,
	)
	if err != nil {
		return fmt.Errorf("satisfying staged artifact imported session: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading staged artifact satisfaction result: %w", err)
	}
	if affected == 0 {
		return nil
	}
	result, err = tx.ExecContext(ctx, `
		UPDATE artifact_checkpoint_stages
		SET pending_count = pending_count - 1
		WHERE origin = ? AND sequence = ?
		  AND checkpoint_sha256 = ? AND checkpoint_size = ?
		  AND complete = 1 AND pending_count > 0`,
		landing.Origin, landing.Sequence,
		landing.CheckpointSHA256, landing.CheckpointSize,
	)
	if err != nil {
		return fmt.Errorf("advancing staged artifact pending count: %w", err)
	}
	affected, err = result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading staged artifact pending result: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf(
			"%w: staged artifact pending count changed",
			ErrArtifactImportConflict,
		)
	}
	return nil
}

func satisfyAllArtifactCheckpointStagesTx(
	ctx context.Context,
	tx *sql.Tx,
	imported ArtifactImportedSession,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT sessions.sequence
		FROM artifact_checkpoint_stage_sessions sessions
		JOIN artifact_checkpoint_stages stage
		  ON stage.origin = sessions.origin
		 AND stage.sequence = sessions.sequence
		WHERE sessions.origin = ? AND sessions.gid = ?
		  AND sessions.manifest_hash = ?
		  AND sessions.satisfied = 0`,
		imported.Origin, imported.GID, imported.ManifestHash,
	)
	if err != nil {
		return fmt.Errorf("reading satisfiable artifact checkpoint stages: %w", err)
	}
	var sequences []int
	for rows.Next() {
		var sequence int
		if err := rows.Scan(&sequence); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scanning satisfiable artifact checkpoint stage: %w", err)
		}
		sequences = append(sequences, sequence)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterating satisfiable artifact checkpoint stages: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("closing satisfiable artifact checkpoint stages: %w", err)
	}
	for _, sequence := range sequences {
		if _, err := tx.ExecContext(ctx, `
			UPDATE artifact_checkpoint_stage_sessions
			SET satisfied = 1
			WHERE origin = ? AND sequence = ? AND gid = ?
			  AND manifest_hash = ? AND satisfied = 0`,
			imported.Origin, sequence, imported.GID, imported.ManifestHash,
		); err != nil {
			return fmt.Errorf("satisfying artifact checkpoint stage: %w", err)
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE artifact_checkpoint_stages
			SET pending_count = pending_count - 1
			WHERE origin = ? AND sequence = ?
			  AND pending_count > 0`,
			imported.Origin, sequence,
		)
		if err != nil {
			return fmt.Errorf("advancing artifact checkpoint pending count: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("reading artifact checkpoint pending result: %w", err)
		}
		if affected != 1 {
			return fmt.Errorf(
				"%w: artifact checkpoint pending count changed",
				ErrArtifactImportConflict,
			)
		}
	}
	return nil
}
