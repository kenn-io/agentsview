package db

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// ErrArtifactImportConflict reports two identities claiming the same durable
// artifact-import authority.
var ErrArtifactImportConflict = errors.New("artifact import conflict")

// ArtifactImportVersions are the independently understood wire versions used
// to select eligible import work.
type ArtifactImportVersions struct {
	Checkpoint int
	Manifest   int
	Segment    int
}

// ArtifactImportWork binds one pending checkpoint reference to its exact
// catalog identity and the minimum decoder versions needed to consume it.
type ArtifactImportWork struct {
	Origin                    string
	Kind                      string
	Name                      string
	SHA256                    string
	Size                      int64
	RequiredCheckpointVersion int
	RequiredManifestVersion   int
	RequiredSegmentVersion    int
	AttemptGeneration         int64
	EnqueuedAt                string
}

// ArtifactPeerCheckpointHead is the highest exact checkpoint identity observed
// for one foreign origin.
type ArtifactPeerCheckpointHead struct {
	Origin           string
	Sequence         int
	CheckpointSHA256 string
	CheckpointSize   int64
}

// ArtifactCheckpointLanding records the exact foreign checkpoint whose full
// current-version session map has been durably satisfied.
type ArtifactCheckpointLanding struct {
	Origin           string
	Sequence         int
	CheckpointSHA256 string
	CheckpointSize   int64
}

// ArtifactImportedSession records the manifest last durably applied or
// intentionally suppressed for one foreign session.
type ArtifactImportedSession struct {
	Origin            string
	GID               string
	ManifestHash      string
	ImportedSessionID string
}

// EnqueueArtifactImport records exact checkpoint work without moving its FIFO
// position on replay. A higher checkpoint supersedes lower queued sequences.
func (db *DB) EnqueueArtifactImport(
	ctx context.Context, work ArtifactImportWork,
) error {
	if err := validateArtifactImportWork(work, false); err != nil {
		return err
	}
	sequence, err := artifactImportCheckpointSequence(work.Name)
	if err != nil {
		return err
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning artifact import enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existingSHA string
	var existingSize int64
	err = tx.QueryRowContext(ctx, `
		SELECT sha256, size
		FROM artifact_import_queue
		WHERE origin = ? AND kind = ? AND name = ?`,
		work.Origin, work.Kind, work.Name,
	).Scan(&existingSHA, &existingSize)
	switch {
	case err == nil && (existingSHA != work.SHA256 || existingSize != work.Size):
		return fmt.Errorf(
			"%w: checkpoint %s/%s has another identity",
			ErrArtifactImportConflict, work.Origin, work.Name,
		)
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("reading artifact import identity: %w", err)
	}

	var highestName string
	err = tx.QueryRowContext(ctx, `
		SELECT name
		FROM artifact_import_queue
		WHERE origin = ? AND kind = 'checkpoints'
		ORDER BY name DESC
		LIMIT 1`, work.Origin,
	).Scan(&highestName)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading highest artifact import checkpoint: %w", err)
	}
	if err == nil {
		highest, parseErr := artifactImportCheckpointSequence(highestName)
		if parseErr != nil {
			return fmt.Errorf("parsing queued artifact checkpoint: %w", parseErr)
		}
		if highest > sequence {
			return nil
		}
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO artifact_import_queue (
			origin, kind, name, sha256, size,
			required_checkpoint_version,
			required_manifest_version,
			required_segment_version,
			attempt_generation
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(origin, kind, name) DO UPDATE SET
			required_checkpoint_version = max(
				artifact_import_queue.required_checkpoint_version,
				excluded.required_checkpoint_version
			),
			required_manifest_version = max(
				artifact_import_queue.required_manifest_version,
				excluded.required_manifest_version
			),
			required_segment_version = max(
				artifact_import_queue.required_segment_version,
				excluded.required_segment_version
			),
			attempt_generation = max(
				artifact_import_queue.attempt_generation,
				excluded.attempt_generation
			)`,
		work.Origin, work.Kind, work.Name, work.SHA256, work.Size,
		work.RequiredCheckpointVersion, work.RequiredManifestVersion,
		work.RequiredSegmentVersion, work.AttemptGeneration,
	)
	if err != nil {
		return fmt.Errorf("enqueueing artifact import: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		DELETE FROM artifact_import_queue
		WHERE origin = ? AND kind = 'checkpoints' AND name < ?`,
		work.Origin, work.Name,
	)
	if err != nil {
		return fmt.Errorf("superseding older artifact imports: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing artifact import enqueue: %w", err)
	}
	return nil
}

// PendingArtifactImports returns one bounded FIFO page understood by the
// caller and not yet attempted in the active generation.
func (db *DB) PendingArtifactImports(
	ctx context.Context,
	versions ArtifactImportVersions,
	attemptGeneration int64,
	limit int,
) ([]ArtifactImportWork, error) {
	if err := validateArtifactImportVersions(versions); err != nil {
		return nil, err
	}
	if attemptGeneration < 1 {
		return nil, errors.New("artifact import attempt generation must be positive")
	}
	if err := validateArtifactQueueLimit(limit); err != nil {
		return nil, err
	}
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT
			origin, kind, name, sha256, size,
			required_checkpoint_version,
			required_manifest_version,
			required_segment_version,
			attempt_generation, enqueued_at
		FROM artifact_import_queue
		WHERE required_checkpoint_version <= ?
		  AND required_manifest_version <= ?
		  AND required_segment_version <= ?
		  AND attempt_generation < ?
		ORDER BY enqueued_at, origin, kind, name
		LIMIT ?`,
		versions.Checkpoint, versions.Manifest, versions.Segment,
		attemptGeneration, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("reading artifact import queue: %w", err)
	}
	defer rows.Close()
	work := make([]ArtifactImportWork, 0, min(limit, 64))
	for rows.Next() {
		var item ArtifactImportWork
		if err := rows.Scan(
			&item.Origin, &item.Kind, &item.Name, &item.SHA256, &item.Size,
			&item.RequiredCheckpointVersion,
			&item.RequiredManifestVersion,
			&item.RequiredSegmentVersion,
			&item.AttemptGeneration, &item.EnqueuedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning artifact import queue: %w", err)
		}
		work = append(work, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating artifact import queue: %w", err)
	}
	return work, nil
}

// ReserveArtifactImportAttemptGeneration returns a durable generation greater
// than every previously reserved retry pass.
func (db *DB) ReserveArtifactImportAttemptGeneration(
	ctx context.Context,
) (int64, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("beginning artifact import generation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var generation int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO artifact_import_attempt_generations(singleton, generation)
		VALUES (1, 1)
		ON CONFLICT(singleton) DO UPDATE SET
			generation = artifact_import_attempt_generations.generation + 1
		RETURNING generation`).Scan(&generation)
	if err != nil {
		return 0, fmt.Errorf("reserving artifact import generation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing artifact import generation: %w", err)
	}
	return generation, nil
}

// MarkArtifactImportAttempted moves only the exact claim into the supplied
// generation. A stale claim leaves newer work untouched.
func (db *DB) MarkArtifactImportAttempted(
	ctx context.Context,
	work ArtifactImportWork,
	attemptGeneration int64,
) (bool, error) {
	if err := validateArtifactImportWork(work, true); err != nil {
		return false, err
	}
	if attemptGeneration <= work.AttemptGeneration {
		return false, errors.New("artifact import attempt generation must advance")
	}
	result, err := db.getWriter().ExecContext(ctx, `
		UPDATE artifact_import_queue
		SET attempt_generation = ?
		WHERE origin = ? AND kind = ? AND name = ?
		  AND sha256 = ? AND size = ?
		  AND required_checkpoint_version = ?
		  AND required_manifest_version = ?
		  AND required_segment_version = ?
		  AND attempt_generation = ? AND enqueued_at = ?`,
		attemptGeneration,
		work.Origin, work.Kind, work.Name, work.SHA256, work.Size,
		work.RequiredCheckpointVersion, work.RequiredManifestVersion,
		work.RequiredSegmentVersion, work.AttemptGeneration, work.EnqueuedAt,
	)
	if err != nil {
		return false, fmt.Errorf("marking artifact import attempted: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("reading artifact import attempt result: %w", err)
	}
	return affected == 1, nil
}

// AcknowledgeArtifactImport removes only the exact durable claim.
func (db *DB) AcknowledgeArtifactImport(
	ctx context.Context, work ArtifactImportWork,
) (bool, error) {
	if err := validateArtifactImportWork(work, true); err != nil {
		return false, err
	}
	result, err := db.getWriter().ExecContext(ctx, `
		DELETE FROM artifact_import_queue
		WHERE origin = ? AND kind = ? AND name = ?
		  AND sha256 = ? AND size = ?
		  AND required_checkpoint_version = ?
		  AND required_manifest_version = ?
		  AND required_segment_version = ?
		  AND attempt_generation = ? AND enqueued_at = ?`,
		work.Origin, work.Kind, work.Name, work.SHA256, work.Size,
		work.RequiredCheckpointVersion, work.RequiredManifestVersion,
		work.RequiredSegmentVersion, work.AttemptGeneration, work.EnqueuedAt,
	)
	if err != nil {
		return false, fmt.Errorf("acknowledging artifact import: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("reading artifact import acknowledgement: %w", err)
	}
	return affected == 1, nil
}

// ArtifactImportQueueStats reports all retained work, including rows deferred
// behind future-version gates.
func (db *DB) ArtifactImportQueueStats(
	ctx context.Context,
) (int, string, error) {
	var count int
	var oldest sql.NullString
	err := db.getReader().QueryRowContext(ctx, `
		SELECT count(*), min(enqueued_at) FROM artifact_import_queue`,
	).Scan(&count, &oldest)
	if err != nil {
		return 0, "", fmt.Errorf("reading artifact import queue stats: %w", err)
	}
	return count, oldest.String, nil
}

// RecordArtifactPeerCheckpointHead advances one origin monotonically and fails
// closed when the same sequence is claimed by another identity.
func (db *DB) RecordArtifactPeerCheckpointHead(
	ctx context.Context, head ArtifactPeerCheckpointHead,
) (bool, error) {
	if err := validateArtifactPeerCheckpointHead(head); err != nil {
		return false, err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("beginning artifact peer head update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existing ArtifactPeerCheckpointHead
	err = tx.QueryRowContext(ctx, `
		SELECT origin, sequence, checkpoint_sha256, checkpoint_size
		FROM artifact_peer_checkpoint_heads WHERE origin = ?`, head.Origin,
	).Scan(
		&existing.Origin, &existing.Sequence,
		&existing.CheckpointSHA256, &existing.CheckpointSize,
	)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO artifact_peer_checkpoint_heads (
				origin, sequence, checkpoint_sha256, checkpoint_size
			) VALUES (?, ?, ?, ?)`,
			head.Origin, head.Sequence, head.CheckpointSHA256, head.CheckpointSize,
		)
		if err != nil {
			return false, fmt.Errorf("inserting artifact peer head: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("committing artifact peer head: %w", err)
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading artifact peer head: %w", err)
	}
	if head.Sequence < existing.Sequence {
		return false, nil
	}
	if head.Sequence == existing.Sequence {
		if head.CheckpointSHA256 != existing.CheckpointSHA256 ||
			head.CheckpointSize != existing.CheckpointSize {
			return false, fmt.Errorf(
				"%w: checkpoint %s sequence %d has another identity",
				ErrArtifactImportConflict, head.Origin, head.Sequence,
			)
		}
		return false, nil
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE artifact_peer_checkpoint_heads
		SET sequence = ?, checkpoint_sha256 = ?, checkpoint_size = ?
		WHERE origin = ?`,
		head.Sequence, head.CheckpointSHA256, head.CheckpointSize, head.Origin,
	)
	if err != nil {
		return false, fmt.Errorf("advancing artifact peer head: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("committing artifact peer head: %w", err)
	}
	return true, nil
}

// GetArtifactPeerCheckpointHead returns the retained highest identity.
func (db *DB) GetArtifactPeerCheckpointHead(
	ctx context.Context, origin string,
) (ArtifactPeerCheckpointHead, bool, error) {
	if strings.TrimSpace(origin) == "" || origin != strings.TrimSpace(origin) {
		return ArtifactPeerCheckpointHead{}, false,
			errors.New("artifact peer origin is required")
	}
	var head ArtifactPeerCheckpointHead
	err := db.getReader().QueryRowContext(ctx, `
		SELECT origin, sequence, checkpoint_sha256, checkpoint_size
		FROM artifact_peer_checkpoint_heads WHERE origin = ?`, origin,
	).Scan(
		&head.Origin, &head.Sequence,
		&head.CheckpointSHA256, &head.CheckpointSize,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ArtifactPeerCheckpointHead{}, false, nil
	}
	if err != nil {
		return ArtifactPeerCheckpointHead{}, false,
			fmt.Errorf("reading artifact peer head: %w", err)
	}
	return head, true, nil
}

// RecordArtifactCheckpointLanding atomically binds the complete session map to
// the exact currently recorded peer head.
func (db *DB) RecordArtifactCheckpointLanding(
	ctx context.Context,
	landing ArtifactCheckpointLanding,
	sessionMap map[string]string,
) error {
	if err := validateArtifactCheckpointLanding(landing); err != nil {
		return err
	}
	gids := make([]string, 0, len(sessionMap))
	for gid, manifestHash := range sessionMap {
		if !strings.HasPrefix(gid, landing.Origin+"~") ||
			len(gid) == len(landing.Origin)+1 {
			return fmt.Errorf("artifact checkpoint GID %q has wrong origin", gid)
		}
		if len(manifestHash) != 64 {
			return fmt.Errorf("artifact checkpoint manifest %q is incomplete", gid)
		}
		if err := validateLowerHex(manifestHash); err != nil {
			return fmt.Errorf("validating artifact checkpoint manifest %q: %w", gid, err)
		}
		gids = append(gids, gid)
	}
	slices.Sort(gids)

	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning artifact checkpoint landing: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var head ArtifactPeerCheckpointHead
	err = tx.QueryRowContext(ctx, `
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
	case err == nil && existing.Sequence == landing.Sequence:
		if existing.CheckpointSHA256 != landing.CheckpointSHA256 ||
			existing.CheckpointSize != landing.CheckpointSize {
			return fmt.Errorf(
				"%w: artifact checkpoint landing identity changed",
				ErrArtifactImportConflict,
			)
		}
		equal, compareErr := artifactLandingMapEqualTx(
			ctx, tx, landing.Origin, sessionMap,
		)
		if compareErr != nil {
			return compareErr
		}
		if !equal {
			return fmt.Errorf(
				"%w: artifact checkpoint landing map changed",
				ErrArtifactImportConflict,
			)
		}
		return nil
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO artifact_checkpoint_landings (
			origin, sequence, checkpoint_sha256, checkpoint_size
		) VALUES (?, ?, ?, ?)
		ON CONFLICT(origin) DO UPDATE SET
			sequence = excluded.sequence,
			checkpoint_sha256 = excluded.checkpoint_sha256,
			checkpoint_size = excluded.checkpoint_size`,
		landing.Origin, landing.Sequence,
		landing.CheckpointSHA256, landing.CheckpointSize,
	)
	if err != nil {
		return fmt.Errorf("recording artifact checkpoint landing: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM artifact_checkpoint_landing_sessions
		WHERE origin = ?`, landing.Origin,
	); err != nil {
		return fmt.Errorf("clearing artifact checkpoint landing map: %w", err)
	}
	for _, gid := range gids {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO artifact_checkpoint_landing_sessions (
				origin, gid, manifest_hash
			) VALUES (?, ?, ?)`,
			landing.Origin, gid, sessionMap[gid],
		); err != nil {
			return fmt.Errorf(
				"recording artifact checkpoint landing session %q: %w", gid, err,
			)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing artifact checkpoint landing: %w", err)
	}
	return nil
}

// GetArtifactCheckpointLanding returns an identity and a fresh copy of its
// complete landed session map.
func (db *DB) GetArtifactCheckpointLanding(
	ctx context.Context, origin string,
) (ArtifactCheckpointLanding, map[string]string, bool, error) {
	if strings.TrimSpace(origin) == "" || origin != strings.TrimSpace(origin) {
		return ArtifactCheckpointLanding{}, nil, false,
			errors.New("artifact checkpoint landing origin is required")
	}
	var landing ArtifactCheckpointLanding
	err := db.getReader().QueryRowContext(ctx, `
		SELECT origin, sequence, checkpoint_sha256, checkpoint_size
		FROM artifact_checkpoint_landings WHERE origin = ?`, origin,
	).Scan(
		&landing.Origin, &landing.Sequence,
		&landing.CheckpointSHA256, &landing.CheckpointSize,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ArtifactCheckpointLanding{}, nil, false, nil
	}
	if err != nil {
		return ArtifactCheckpointLanding{}, nil, false,
			fmt.Errorf("reading artifact checkpoint landing: %w", err)
	}
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT gid, manifest_hash
		FROM artifact_checkpoint_landing_sessions
		WHERE origin = ? ORDER BY gid`, origin,
	)
	if err != nil {
		return ArtifactCheckpointLanding{}, nil, false,
			fmt.Errorf("reading artifact checkpoint landing map: %w", err)
	}
	defer rows.Close()
	sessionMap := make(map[string]string)
	for rows.Next() {
		var gid, manifestHash string
		if err := rows.Scan(&gid, &manifestHash); err != nil {
			return ArtifactCheckpointLanding{}, nil, false,
				fmt.Errorf("scanning artifact checkpoint landing map: %w", err)
		}
		sessionMap[gid] = manifestHash
	}
	if err := rows.Err(); err != nil {
		return ArtifactCheckpointLanding{}, nil, false,
			fmt.Errorf("iterating artifact checkpoint landing map: %w", err)
	}
	return landing, sessionMap, true, nil
}

// ArtifactImportedManifestHashes returns provenance for a bounded exact GID
// set without scanning an origin's complete imported history.
func (db *DB) ArtifactImportedManifestHashes(
	ctx context.Context, origin string, gids []string,
) (map[string]string, error) {
	if strings.TrimSpace(origin) == "" || origin != strings.TrimSpace(origin) {
		return nil, errors.New("artifact imported-session origin is required")
	}
	if len(gids) > maxArtifactQueuePageSize {
		return nil, fmt.Errorf(
			"artifact imported-session query exceeds %d rows",
			maxArtifactQueuePageSize,
		)
	}
	unique := make([]string, 0, len(gids))
	seen := make(map[string]struct{}, len(gids))
	for _, gid := range gids {
		if !strings.HasPrefix(gid, origin+"~") || len(gid) == len(origin)+1 {
			return nil, fmt.Errorf("artifact imported-session GID %q has wrong origin", gid)
		}
		if _, ok := seen[gid]; ok {
			continue
		}
		seen[gid] = struct{}{}
		unique = append(unique, gid)
	}
	result := make(map[string]string, len(unique))
	if len(unique) == 0 {
		return result, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(unique)), ",")
	args := make([]any, 0, len(unique)+1)
	args = append(args, origin)
	for _, gid := range unique {
		args = append(args, gid)
	}
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT gid, manifest_hash
		FROM artifact_imported_sessions
		WHERE origin = ? AND gid IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("reading artifact imported-session provenance: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var gid, manifestHash string
		if err := rows.Scan(&gid, &manifestHash); err != nil {
			return nil, fmt.Errorf(
				"scanning artifact imported-session provenance: %w", err,
			)
		}
		result[gid] = manifestHash
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"iterating artifact imported-session provenance: %w", err,
		)
	}
	return result, nil
}

// RecordArtifactImportedSession advances durable per-session provenance.
func (db *DB) RecordArtifactImportedSession(
	ctx context.Context, imported ArtifactImportedSession,
) error {
	if err := validateArtifactImportedSession(imported); err != nil {
		return err
	}
	_, err := db.getWriter().ExecContext(ctx, `
		INSERT INTO artifact_imported_sessions (
			origin, gid, manifest_hash, imported_session_id
		) VALUES (?, ?, ?, ?)
		ON CONFLICT(origin, gid) DO UPDATE SET
			manifest_hash = excluded.manifest_hash,
			imported_session_id = excluded.imported_session_id,
			imported_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE artifact_imported_sessions.manifest_hash <> excluded.manifest_hash
		   OR artifact_imported_sessions.imported_session_id <>
		      excluded.imported_session_id`,
		imported.Origin, imported.GID,
		imported.ManifestHash, imported.ImportedSessionID,
	)
	if err != nil {
		return fmt.Errorf("recording artifact imported-session provenance: %w", err)
	}
	return nil
}

func artifactLandingMapEqualTx(
	ctx context.Context,
	tx *sql.Tx,
	origin string,
	want map[string]string,
) (bool, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT gid, manifest_hash
		FROM artifact_checkpoint_landing_sessions
		WHERE origin = ?`, origin,
	)
	if err != nil {
		return false, fmt.Errorf("reading existing artifact landing map: %w", err)
	}
	defer rows.Close()
	got := make(map[string]string)
	for rows.Next() {
		var gid, manifestHash string
		if err := rows.Scan(&gid, &manifestHash); err != nil {
			return false, fmt.Errorf("scanning existing artifact landing map: %w", err)
		}
		got[gid] = manifestHash
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterating existing artifact landing map: %w", err)
	}
	if len(got) != len(want) {
		return false, nil
	}
	for gid, manifestHash := range want {
		if got[gid] != manifestHash {
			return false, nil
		}
	}
	return true, nil
}

func validateArtifactImportWork(
	work ArtifactImportWork, requireClaim bool,
) error {
	if strings.TrimSpace(work.Origin) == "" ||
		work.Origin != strings.TrimSpace(work.Origin) {
		return errors.New("artifact import origin is required")
	}
	if work.Kind != "checkpoints" {
		return errors.New("artifact import kind must be checkpoints")
	}
	if _, err := artifactImportCheckpointSequence(work.Name); err != nil {
		return err
	}
	if len(work.SHA256) != 64 || work.Size < 0 {
		return errors.New("complete artifact import identity is required")
	}
	if err := validateLowerHex(work.SHA256); err != nil {
		return err
	}
	if err := validateArtifactImportVersions(ArtifactImportVersions{
		Checkpoint: work.RequiredCheckpointVersion,
		Manifest:   work.RequiredManifestVersion,
		Segment:    work.RequiredSegmentVersion,
	}); err != nil {
		return err
	}
	if work.AttemptGeneration < 0 {
		return errors.New("artifact import attempt generation must not be negative")
	}
	if requireClaim && strings.TrimSpace(work.EnqueuedAt) == "" {
		return errors.New("artifact import enqueue time is required")
	}
	return nil
}

func validateArtifactImportVersions(versions ArtifactImportVersions) error {
	if versions.Checkpoint < 1 || versions.Manifest < 1 || versions.Segment < 1 {
		return errors.New("artifact import versions must be positive")
	}
	return nil
}

func validateArtifactPeerCheckpointHead(head ArtifactPeerCheckpointHead) error {
	if strings.TrimSpace(head.Origin) == "" ||
		head.Origin != strings.TrimSpace(head.Origin) {
		return errors.New("artifact peer origin is required")
	}
	if head.Sequence < 1 {
		return errors.New("artifact peer checkpoint sequence must be positive")
	}
	if len(head.CheckpointSHA256) != 64 || head.CheckpointSize < 0 {
		return errors.New("complete artifact peer checkpoint identity is required")
	}
	return validateLowerHex(head.CheckpointSHA256)
}

func validateArtifactCheckpointLanding(
	landing ArtifactCheckpointLanding,
) error {
	return validateArtifactPeerCheckpointHead(ArtifactPeerCheckpointHead(landing))
}

func validateArtifactImportedSession(imported ArtifactImportedSession) error {
	if strings.TrimSpace(imported.Origin) == "" ||
		imported.Origin != strings.TrimSpace(imported.Origin) {
		return errors.New("artifact imported-session origin is required")
	}
	if !strings.HasPrefix(imported.GID, imported.Origin+"~") ||
		len(imported.GID) == len(imported.Origin)+1 {
		return errors.New("artifact imported-session GID has wrong origin")
	}
	if len(imported.ManifestHash) != 64 {
		return errors.New("artifact imported-session manifest identity is incomplete")
	}
	if err := validateLowerHex(imported.ManifestHash); err != nil {
		return err
	}
	if strings.TrimSpace(imported.ImportedSessionID) == "" {
		return errors.New("artifact imported session ID is required")
	}
	return nil
}

func validateLowerHex(value string) error {
	if value != strings.ToLower(value) {
		return errors.New("artifact identity must be lowercase hexadecimal")
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("artifact identity must be hexadecimal: %w", err)
	}
	return nil
}

func artifactImportCheckpointSequence(name string) (int, error) {
	if len(name) != len("cp-0000000001.json") ||
		!strings.HasPrefix(name, "cp-") ||
		!strings.HasSuffix(name, ".json") {
		return 0, errors.New("artifact import checkpoint name is invalid")
	}
	sequence64, err := strconv.ParseInt(name[3:13], 10, 32)
	if err != nil || sequence64 < 1 ||
		fmt.Sprintf("cp-%010d.json", sequence64) != name {
		return 0, errors.New("artifact import checkpoint name is invalid")
	}
	return int(sequence64), nil
}
