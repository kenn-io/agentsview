package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const maxArtifactImportSessionPageSize = 128

// ArtifactCheckpointSession is one exact checkpoint-map entry.
type ArtifactCheckpointSession struct {
	GID          string
	ManifestHash string
}

// ArtifactCheckpointStageState is the durable bounded-decode cursor for one
// exact checkpoint.
type ArtifactCheckpointStageState struct {
	Complete     bool
	DecodedCount int
	DecodeOffset int64
}

// BeginArtifactCheckpointStage reserves durable staging authority for one
// exact checkpoint identity.
func (db *DB) BeginArtifactCheckpointStage(
	ctx context.Context,
	landing ArtifactCheckpointLanding,
	decoderVersion int,
) error {
	if err := validateArtifactCheckpointLanding(landing); err != nil {
		return err
	}
	if decoderVersion < 1 {
		return errors.New("artifact checkpoint decoder version must be positive")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning artifact checkpoint stage: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := requireArtifactCheckpointStageIdentityTx(ctx, tx, landing, true); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM artifact_checkpoint_stage_sessions
		WHERE origin = ? AND sequence = ?
		  AND EXISTS (
			SELECT 1
			FROM artifact_checkpoint_stages
			WHERE origin = ? AND sequence = ?
			  AND decoder_version <> ?
		  )`,
		landing.Origin, landing.Sequence,
		landing.Origin, landing.Sequence, decoderVersion,
	); err != nil {
		return fmt.Errorf("resetting artifact checkpoint stage sessions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE artifact_checkpoint_stages
		SET decoder_version = ?,
		    complete = 0,
		    session_count = 0,
		    pending_count = 0,
		    decoded_count = 0,
		    decode_offset = 0
		WHERE origin = ? AND sequence = ?
		  AND decoder_version <> ?`,
		decoderVersion, landing.Origin, landing.Sequence, decoderVersion,
	); err != nil {
		return fmt.Errorf("resetting artifact checkpoint stage decoder: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing artifact checkpoint stage: %w", err)
	}
	return nil
}

// ArtifactCheckpointStageComplete reports whether an exact stage has already
// been fully decoded and counted.
func (db *DB) ArtifactCheckpointStageComplete(
	ctx context.Context,
	landing ArtifactCheckpointLanding,
) (bool, error) {
	if err := validateArtifactCheckpointLanding(landing); err != nil {
		return false, err
	}
	var complete int
	err := db.getReader().QueryRowContext(ctx, `
		SELECT complete
		FROM artifact_checkpoint_stages
		WHERE origin = ? AND sequence = ?
		  AND checkpoint_sha256 = ? AND checkpoint_size = ?`,
		landing.Origin, landing.Sequence,
		landing.CheckpointSHA256, landing.CheckpointSize,
	).Scan(&complete)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading artifact checkpoint stage state: %w", err)
	}
	return complete == 1, nil
}

// ArtifactCheckpointStageState returns durable progress for an exact stage.
func (db *DB) ArtifactCheckpointStageProgress(
	ctx context.Context,
	landing ArtifactCheckpointLanding,
) (ArtifactCheckpointStageState, error) {
	if err := validateArtifactCheckpointLanding(landing); err != nil {
		return ArtifactCheckpointStageState{}, err
	}
	var state ArtifactCheckpointStageState
	err := db.getReader().QueryRowContext(ctx, `
		SELECT complete, decoded_count, decode_offset
		FROM artifact_checkpoint_stages
		WHERE origin = ? AND sequence = ?
		  AND checkpoint_sha256 = ? AND checkpoint_size = ?`,
		landing.Origin, landing.Sequence,
		landing.CheckpointSHA256, landing.CheckpointSize,
	).Scan(&state.Complete, &state.DecodedCount, &state.DecodeOffset)
	if errors.Is(err, sql.ErrNoRows) {
		return ArtifactCheckpointStageState{}, fmt.Errorf(
			"%w: artifact checkpoint stage is missing",
			ErrArtifactImportConflict,
		)
	}
	if err != nil {
		return ArtifactCheckpointStageState{},
			fmt.Errorf("reading artifact checkpoint stage progress: %w", err)
	}
	return state, nil
}

// StageArtifactCheckpointSessions appends one bounded, idempotent page to an
// exact checkpoint stage.
func (db *DB) StageArtifactCheckpointSessions(
	ctx context.Context,
	landing ArtifactCheckpointLanding,
	entries []ArtifactCheckpointSession,
) error {
	return db.stageArtifactCheckpointSessions(
		ctx, landing, entries, nil, 0,
	)
}

// StageArtifactCheckpointSessionPage atomically advances the durable decode
// cursor with one bounded page.
func (db *DB) StageArtifactCheckpointSessionPage(
	ctx context.Context,
	landing ArtifactCheckpointLanding,
	entries []ArtifactCheckpointSession,
	expectedOffset int64,
	nextOffset int64,
) error {
	if expectedOffset < 0 || nextOffset <= expectedOffset {
		return errors.New("artifact checkpoint decode offsets must advance")
	}
	return db.stageArtifactCheckpointSessions(
		ctx, landing, entries, &expectedOffset, nextOffset,
	)
}

func (db *DB) stageArtifactCheckpointSessions(
	ctx context.Context,
	landing ArtifactCheckpointLanding,
	entries []ArtifactCheckpointSession,
	expectedOffset *int64,
	nextOffset int64,
) error {
	if err := validateArtifactCheckpointLanding(landing); err != nil {
		return err
	}
	if len(entries) > maxArtifactImportSessionPageSize {
		return fmt.Errorf(
			"artifact checkpoint stage page exceeds %d rows",
			maxArtifactImportSessionPageSize,
		)
	}
	for _, entry := range entries {
		if err := validateArtifactCheckpointSession(landing.Origin, entry); err != nil {
			return err
		}
	}
	if len(entries) == 0 && expectedOffset == nil {
		return nil
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning artifact checkpoint session stage: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireArtifactCheckpointStageIdentityTx(ctx, tx, landing, false); err != nil {
		return err
	}
	var complete, decodedCount int
	var decodeOffset int64
	if err := tx.QueryRowContext(ctx, `
		SELECT complete, decoded_count, decode_offset
		FROM artifact_checkpoint_stages
		WHERE origin = ? AND sequence = ?`,
		landing.Origin, landing.Sequence,
	).Scan(&complete, &decodedCount, &decodeOffset); err != nil {
		return fmt.Errorf("reading artifact checkpoint stage completion: %w", err)
	}
	if expectedOffset != nil && decodeOffset != *expectedOffset {
		return fmt.Errorf(
			"%w: artifact checkpoint decode cursor changed",
			ErrArtifactImportConflict,
		)
	}
	insertedCount := 0
	pendingCount := 0
	for _, entry := range entries {
		var manifestHash string
		err := tx.QueryRowContext(ctx, `
			SELECT manifest_hash
			FROM artifact_checkpoint_stage_sessions
			WHERE origin = ? AND sequence = ? AND gid = ?`,
			landing.Origin, landing.Sequence, entry.GID,
		).Scan(&manifestHash)
		switch {
		case err == nil && manifestHash != entry.ManifestHash:
			return fmt.Errorf(
				"%w: checkpoint stage session %q changed identity",
				ErrArtifactImportConflict, entry.GID,
			)
		case err == nil:
			continue
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("reading artifact checkpoint stage session: %w", err)
		}
		if complete == 1 {
			return fmt.Errorf(
				"%w: completed checkpoint stage cannot gain sessions",
				ErrArtifactImportConflict,
			)
		}
		var satisfied int
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM artifact_imported_sessions
				WHERE origin = ? AND gid = ? AND manifest_hash = ?
			)`,
			landing.Origin, entry.GID, entry.ManifestHash,
		).Scan(&satisfied); err != nil {
			return fmt.Errorf("matching staged artifact session provenance: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO artifact_checkpoint_stage_sessions (
				origin, sequence, gid, manifest_hash, satisfied
			) VALUES (?, ?, ?, ?, ?)`,
			landing.Origin, landing.Sequence,
			entry.GID, entry.ManifestHash, satisfied,
		); err != nil {
			return fmt.Errorf("staging artifact checkpoint session: %w", err)
		}
		insertedCount++
		if satisfied == 0 {
			pendingCount++
		}
	}
	if insertedCount > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE artifact_checkpoint_stages
			SET session_count = session_count + ?,
			    pending_count = pending_count + ?
			WHERE origin = ? AND sequence = ? AND complete = 0`,
			insertedCount, pendingCount, landing.Origin, landing.Sequence,
		); err != nil {
			return fmt.Errorf("advancing artifact checkpoint stage counts: %w", err)
		}
	}
	decodedDelta := insertedCount
	if expectedOffset != nil {
		decodedDelta = len(entries)
		if _, err := tx.ExecContext(ctx, `
			UPDATE artifact_checkpoint_stages
			SET decoded_count = decoded_count + ?, decode_offset = ?
			WHERE origin = ? AND sequence = ? AND complete = 0
			  AND decode_offset = ?`,
			decodedDelta, nextOffset,
			landing.Origin, landing.Sequence, *expectedOffset,
		); err != nil {
			return fmt.Errorf("advancing artifact checkpoint decode cursor: %w", err)
		}
	} else if decodedDelta > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE artifact_checkpoint_stages
			SET decoded_count = decoded_count + ?
			WHERE origin = ? AND sequence = ? AND complete = 0`,
			decodedDelta, landing.Origin, landing.Sequence,
		); err != nil {
			return fmt.Errorf("advancing artifact checkpoint decoded count: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing artifact checkpoint session stage: %w", err)
	}
	return nil
}

// CompleteArtifactCheckpointStage makes a fully decoded stage eligible for
// bounded session processing.
func (db *DB) CompleteArtifactCheckpointStage(
	ctx context.Context,
	landing ArtifactCheckpointLanding,
	sessionCount int,
) error {
	if err := validateArtifactCheckpointLanding(landing); err != nil {
		return err
	}
	if sessionCount < 0 {
		return errors.New("artifact checkpoint session count must not be negative")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning artifact checkpoint stage completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireArtifactCheckpointStageIdentityTx(ctx, tx, landing, false); err != nil {
		return err
	}
	var staged, decoded int
	if err := tx.QueryRowContext(ctx, `
		SELECT session_count, decoded_count
		FROM artifact_checkpoint_stages
		WHERE origin = ? AND sequence = ?`,
		landing.Origin, landing.Sequence,
	).Scan(&staged, &decoded); err != nil {
		return fmt.Errorf("counting staged artifact checkpoint sessions: %w", err)
	}
	if staged != sessionCount || decoded != sessionCount {
		return fmt.Errorf(
			"%w: checkpoint stage has %d unique and %d decoded sessions, expected %d",
			ErrArtifactImportConflict, staged, decoded, sessionCount,
		)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE artifact_checkpoint_stages
		SET complete = 1
		WHERE origin = ? AND sequence = ?`,
		landing.Origin, landing.Sequence,
	); err != nil {
		return fmt.Errorf("completing artifact checkpoint stage: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing artifact checkpoint stage completion: %w", err)
	}
	return nil
}

// PendingArtifactCheckpointSessions returns one bounded page whose manifest
// provenance is not yet satisfied and which has not been attempted in the
// active generation.
func (db *DB) PendingArtifactCheckpointSessions(
	ctx context.Context,
	landing ArtifactCheckpointLanding,
	attemptGeneration int64,
	limit int,
) ([]ArtifactCheckpointSession, error) {
	if err := validateArtifactCheckpointLanding(landing); err != nil {
		return nil, err
	}
	if attemptGeneration < 1 {
		return nil, errors.New("artifact checkpoint attempt generation must be positive")
	}
	if limit < 1 || limit > maxArtifactImportSessionPageSize {
		return nil, fmt.Errorf(
			"artifact checkpoint session limit must be between 1 and %d",
			maxArtifactImportSessionPageSize,
		)
	}
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT sessions.gid, sessions.manifest_hash
		FROM artifact_checkpoint_stage_sessions sessions
		JOIN artifact_checkpoint_stages stage
		  ON stage.origin = sessions.origin
		 AND stage.sequence = sessions.sequence
		WHERE sessions.origin = ?
		  AND sessions.sequence = ?
		  AND stage.checkpoint_sha256 = ?
		  AND stage.checkpoint_size = ?
		  AND stage.complete = 1
		  AND sessions.attempt_generation < ?
		  AND sessions.satisfied = 0
		ORDER BY sessions.attempt_generation, sessions.gid
		LIMIT ?`,
		landing.Origin, landing.Sequence,
		landing.CheckpointSHA256, landing.CheckpointSize,
		attemptGeneration, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("reading pending artifact checkpoint sessions: %w", err)
	}
	defer rows.Close()
	entries := make([]ArtifactCheckpointSession, 0, min(limit, 64))
	for rows.Next() {
		var entry ArtifactCheckpointSession
		if err := rows.Scan(&entry.GID, &entry.ManifestHash); err != nil {
			return nil, fmt.Errorf("scanning pending artifact checkpoint session: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating pending artifact checkpoint sessions: %w", err)
	}
	return entries, nil
}

// ArtifactCheckpointStageHasPending reports whether any exact staged manifest
// still lacks matching durable import provenance.
func (db *DB) ArtifactCheckpointStageHasPending(
	ctx context.Context,
	landing ArtifactCheckpointLanding,
) (bool, error) {
	if err := validateArtifactCheckpointLanding(landing); err != nil {
		return false, err
	}
	var pendingCount int
	err := db.getReader().QueryRowContext(ctx, `
		SELECT pending_count
		FROM artifact_checkpoint_stages
		WHERE origin = ? AND sequence = ?
		  AND checkpoint_sha256 = ? AND checkpoint_size = ?
		  AND complete = 1`,
		landing.Origin, landing.Sequence,
		landing.CheckpointSHA256, landing.CheckpointSize,
	).Scan(&pendingCount)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf(
			"%w: artifact checkpoint stage is incomplete",
			ErrArtifactImportConflict,
		)
	}
	if err != nil {
		return false, fmt.Errorf("checking pending artifact checkpoint stage: %w", err)
	}
	return pendingCount > 0, nil
}

// MarkArtifactCheckpointSessionAttempted defers one exact staged entry for the
// remainder of the active attempt generation.
func (db *DB) MarkArtifactCheckpointSessionAttempted(
	ctx context.Context,
	landing ArtifactCheckpointLanding,
	entry ArtifactCheckpointSession,
	attemptGeneration int64,
) (bool, error) {
	if err := validateArtifactCheckpointLanding(landing); err != nil {
		return false, err
	}
	if err := validateArtifactCheckpointSession(landing.Origin, entry); err != nil {
		return false, err
	}
	if attemptGeneration < 1 {
		return false, errors.New("artifact checkpoint attempt generation must be positive")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	result, err := db.getWriter().ExecContext(ctx, `
		UPDATE artifact_checkpoint_stage_sessions
		SET attempt_generation = max(attempt_generation, ?)
		WHERE origin = ? AND sequence = ? AND gid = ? AND manifest_hash = ?
		  AND EXISTS (
			SELECT 1 FROM artifact_checkpoint_stages
			WHERE origin = ? AND sequence = ?
			  AND checkpoint_sha256 = ? AND checkpoint_size = ?
		  )`,
		attemptGeneration,
		landing.Origin, landing.Sequence, entry.GID, entry.ManifestHash,
		landing.Origin, landing.Sequence,
		landing.CheckpointSHA256, landing.CheckpointSize,
	)
	if err != nil {
		return false, fmt.Errorf("marking artifact checkpoint session attempted: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("reading attempted artifact checkpoint session rows: %w", err)
	}
	return rows == 1, nil
}

// RecordArtifactCheckpointLandingFromStage atomically advances landing
// authority only after every staged manifest has matching durable provenance.
func (db *DB) RecordArtifactCheckpointLandingFromStage(
	ctx context.Context,
	landing ArtifactCheckpointLanding,
) error {
	if err := validateArtifactCheckpointLanding(landing); err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning staged artifact checkpoint landing: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var complete, pendingCount int
	err = tx.QueryRowContext(ctx, `
		SELECT complete, pending_count
		FROM artifact_checkpoint_stages
		WHERE origin = ? AND sequence = ?
		  AND checkpoint_sha256 = ? AND checkpoint_size = ?`,
		landing.Origin, landing.Sequence,
		landing.CheckpointSHA256, landing.CheckpointSize,
	).Scan(&complete, &pendingCount)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: artifact checkpoint stage is missing", ErrArtifactImportConflict)
	}
	if err != nil {
		return fmt.Errorf("reading artifact checkpoint stage landing: %w", err)
	}
	if complete != 1 {
		return fmt.Errorf("%w: artifact checkpoint stage is incomplete", ErrArtifactImportConflict)
	}
	if pendingCount != 0 {
		return fmt.Errorf(
			"%w: artifact checkpoint stage has %d pending sessions",
			ErrArtifactImportConflict, pendingCount,
		)
	}
	if err := recordStagedArtifactCheckpointLandingTx(ctx, tx, landing); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing staged artifact checkpoint landing: %w", err)
	}
	return nil
}

func recordStagedArtifactCheckpointLandingTx(
	ctx context.Context,
	tx *sql.Tx,
	landing ArtifactCheckpointLanding,
) error {
	var head ArtifactPeerCheckpointHead
	err := tx.QueryRowContext(ctx, `
		SELECT origin, sequence, checkpoint_sha256, checkpoint_size
		FROM artifact_peer_checkpoint_heads WHERE origin = ?`, landing.Origin,
	).Scan(
		&head.Origin, &head.Sequence,
		&head.CheckpointSHA256, &head.CheckpointSize,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf(
			"%w: artifact checkpoint landing has no peer head",
			ErrArtifactImportConflict,
		)
	}
	if err != nil {
		return fmt.Errorf("reading artifact peer head for landing: %w", err)
	}
	if head.Sequence != landing.Sequence ||
		head.CheckpointSHA256 != landing.CheckpointSHA256 ||
		head.CheckpointSize != landing.CheckpointSize {
		return fmt.Errorf(
			"%w: artifact checkpoint landing does not match peer head",
			ErrArtifactImportConflict,
		)
	}

	var existing ArtifactCheckpointLanding
	err = tx.QueryRowContext(ctx, `
		SELECT origin, sequence, checkpoint_sha256, checkpoint_size
		FROM artifact_checkpoint_landings WHERE origin = ?`, landing.Origin,
	).Scan(
		&existing.Origin, &existing.Sequence,
		&existing.CheckpointSHA256, &existing.CheckpointSize,
	)
	switch {
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("reading artifact checkpoint landing: %w", err)
	case err == nil && existing.Sequence > landing.Sequence:
		return fmt.Errorf(
			"%w: artifact checkpoint landing would regress",
			ErrArtifactImportConflict,
		)
	case err == nil && existing.Sequence == landing.Sequence &&
		(existing.CheckpointSHA256 != landing.CheckpointSHA256 ||
			existing.CheckpointSize != landing.CheckpointSize):
		return fmt.Errorf(
			"%w: artifact checkpoint landing identity changed",
			ErrArtifactImportConflict,
		)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO artifact_checkpoint_landings (
			origin, sequence, checkpoint_sha256, checkpoint_size
		) VALUES (?, ?, ?, ?)
		ON CONFLICT(origin) DO UPDATE SET
			sequence = excluded.sequence,
			checkpoint_sha256 = excluded.checkpoint_sha256,
			checkpoint_size = excluded.checkpoint_size`,
		landing.Origin, landing.Sequence,
		landing.CheckpointSHA256, landing.CheckpointSize,
	); err != nil {
		return fmt.Errorf("recording staged artifact checkpoint landing: %w", err)
	}
	return nil
}

func requireArtifactCheckpointStageIdentityTx(
	ctx context.Context,
	tx *sql.Tx,
	landing ArtifactCheckpointLanding,
	create bool,
) error {
	var sha string
	var size int64
	err := tx.QueryRowContext(ctx, `
		SELECT checkpoint_sha256, checkpoint_size
		FROM artifact_checkpoint_stages
		WHERE origin = ? AND sequence = ?`,
		landing.Origin, landing.Sequence,
	).Scan(&sha, &size)
	switch {
	case err == nil && (sha != landing.CheckpointSHA256 || size != landing.CheckpointSize):
		return fmt.Errorf(
			"%w: artifact checkpoint stage identity changed",
			ErrArtifactImportConflict,
		)
	case err == nil:
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("reading artifact checkpoint stage identity: %w", err)
	case !create:
		return fmt.Errorf("%w: artifact checkpoint stage is missing", ErrArtifactImportConflict)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO artifact_checkpoint_stages (
			origin, sequence, checkpoint_sha256, checkpoint_size
		) VALUES (?, ?, ?, ?)`,
		landing.Origin, landing.Sequence,
		landing.CheckpointSHA256, landing.CheckpointSize,
	); err != nil {
		return fmt.Errorf("creating artifact checkpoint stage: %w", err)
	}
	return nil
}

func validateArtifactCheckpointSession(
	origin string,
	entry ArtifactCheckpointSession,
) error {
	prefix := origin + "~"
	if !strings.HasPrefix(entry.GID, prefix) ||
		len(entry.GID) == len(prefix) ||
		strings.Contains(entry.GID[len(prefix):], "~") {
		return errors.New("artifact checkpoint stage session GID has wrong origin")
	}
	if len(entry.ManifestHash) != 64 {
		return errors.New("artifact checkpoint stage manifest identity is incomplete")
	}
	return validateLowerHex(entry.ManifestHash)
}

// PruneArtifactCheckpointStages removes at most limit rows from stages
// superseded by each origin's peer head. The exact stage backing the current
// landing and stages at the peer head are never eligible.
func (db *DB) PruneArtifactCheckpointStages(
	ctx context.Context,
	limit int,
) (int, bool, error) {
	if limit < 1 || limit > maxArtifactImportSessionPageSize {
		return 0, false, fmt.Errorf(
			"artifact checkpoint prune limit must be between 1 and %d",
			maxArtifactImportSessionPageSize,
		)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("beginning artifact checkpoint prune: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
		DELETE FROM artifact_checkpoint_stage_sessions
		WHERE rowid IN (
			SELECT sessions.rowid
			FROM artifact_checkpoint_stage_sessions sessions
			JOIN artifact_checkpoint_stages stage
			  ON stage.origin = sessions.origin
			 AND stage.sequence = sessions.sequence
			JOIN artifact_peer_checkpoint_heads head
			  ON head.origin = stage.origin
			WHERE stage.sequence < head.sequence
			  AND NOT EXISTS (
				SELECT 1
				FROM artifact_checkpoint_landings landing
				WHERE landing.origin = stage.origin
				  AND landing.sequence = stage.sequence
				  AND landing.checkpoint_sha256 = stage.checkpoint_sha256
				  AND landing.checkpoint_size = stage.checkpoint_size
			  )
			ORDER BY sessions.origin, sessions.sequence, sessions.gid
			LIMIT ?
		)`, limit)
	if err != nil {
		return 0, false, fmt.Errorf("pruning artifact checkpoint sessions: %w", err)
	}
	removed64, err := result.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("reading artifact checkpoint session prune: %w", err)
	}
	removed := int(removed64)
	if remaining := limit - removed; remaining > 0 {
		result, err = tx.ExecContext(ctx, `
			DELETE FROM artifact_checkpoint_stages
			WHERE rowid IN (
				SELECT stage.rowid
				FROM artifact_checkpoint_stages stage
				JOIN artifact_peer_checkpoint_heads head
				  ON head.origin = stage.origin
				WHERE stage.sequence < head.sequence
				  AND NOT EXISTS (
					SELECT 1
					FROM artifact_checkpoint_landings landing
					WHERE landing.origin = stage.origin
					  AND landing.sequence = stage.sequence
					  AND landing.checkpoint_sha256 = stage.checkpoint_sha256
					  AND landing.checkpoint_size = stage.checkpoint_size
				  )
				  AND NOT EXISTS (
					SELECT 1
					FROM artifact_checkpoint_stage_sessions sessions
					WHERE sessions.origin = stage.origin
					  AND sessions.sequence = stage.sequence
				  )
				ORDER BY stage.origin, stage.sequence
				LIMIT ?
			)`, remaining)
		if err != nil {
			return 0, false, fmt.Errorf("pruning artifact checkpoint stages: %w", err)
		}
		headers64, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return 0, false, fmt.Errorf("reading artifact checkpoint stage prune: %w", rowsErr)
		}
		removed += int(headers64)
	}
	if remaining := limit - removed; remaining > 0 {
		result, err = tx.ExecContext(ctx, `
			DELETE FROM artifact_checkpoint_landing_sessions
			WHERE rowid IN (
				SELECT legacy.rowid
				FROM artifact_checkpoint_landing_sessions legacy
				JOIN artifact_checkpoint_landings landing
				  ON landing.origin = legacy.origin
				JOIN artifact_checkpoint_stages stage
				  ON stage.origin = landing.origin
				 AND stage.sequence = landing.sequence
				 AND stage.checkpoint_sha256 = landing.checkpoint_sha256
				 AND stage.checkpoint_size = landing.checkpoint_size
				 AND stage.complete = 1
				ORDER BY legacy.origin, legacy.gid
				LIMIT ?
			)`, remaining)
		if err != nil {
			return 0, false, fmt.Errorf("pruning legacy artifact landing sessions: %w", err)
		}
		legacy64, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return 0, false, fmt.Errorf("reading legacy artifact landing prune: %w", rowsErr)
		}
		removed += int(legacy64)
	}
	var more int
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM artifact_checkpoint_stages stage
			JOIN artifact_peer_checkpoint_heads head
			  ON head.origin = stage.origin
			WHERE stage.sequence < head.sequence
			  AND NOT EXISTS (
				SELECT 1
				FROM artifact_checkpoint_landings landing
				WHERE landing.origin = stage.origin
				  AND landing.sequence = stage.sequence
				  AND landing.checkpoint_sha256 = stage.checkpoint_sha256
				  AND landing.checkpoint_size = stage.checkpoint_size
			  )
		)
		OR EXISTS (
			SELECT 1
			FROM artifact_checkpoint_landing_sessions legacy
			JOIN artifact_checkpoint_landings landing
			  ON landing.origin = legacy.origin
			JOIN artifact_checkpoint_stages stage
			  ON stage.origin = landing.origin
			 AND stage.sequence = landing.sequence
			 AND stage.checkpoint_sha256 = landing.checkpoint_sha256
			 AND stage.checkpoint_size = landing.checkpoint_size
			 AND stage.complete = 1
		)`,
	).Scan(&more); err != nil {
		return 0, false, fmt.Errorf("checking artifact checkpoint prune remainder: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("committing artifact checkpoint prune: %w", err)
	}
	return removed, more == 1, nil
}
