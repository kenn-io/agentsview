package rawcheckpoint

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

var ErrAcknowledgeConflict = errors.New("rawcheckpoint: generation acknowledgement conflict")

var ErrGenerationFailureConflict = errors.New(
	"rawcheckpoint: generation failure state conflict")

// GenerationFailureClass records the transport outcome needed by the later
// uploader without embedding provider responses or raw content in the outbox.
type GenerationFailureClass string

const (
	GenerationFailureTransient             GenerationFailureClass = "transient"
	GenerationFailureParentReceiptConflict GenerationFailureClass = "parent_receipt_conflict"
	GenerationFailurePermanent             GenerationFailureClass = "permanent"
)

// AcknowledgeResult reports whether an acknowledgement was replayed and what
// physical capacity its normal-operation garbage collection reclaimed.
type AcknowledgeResult struct {
	Replayed bool
	Garbage  GarbageCollectionReport
}

// BindFinalizedCommit durably records the full server result for a finalized
// generation before the local acknowledgement advances its source head. A
// later drain can then finish acknowledgement without committing the manifest
// again.
func (s *Store) BindFinalizedCommit(
	ctx context.Context,
	deviceID, captureID string,
	commit rawsync.CommitResult,
) error {
	if captureID == "" {
		return ErrAcknowledgeConflict
	}
	if err := rawsync.ValidateCommitResult(commit); err != nil {
		return fmt.Errorf("rawcheckpoint: invalid finalized commit: %w", err)
	}
	return s.withImmediateWrite(ctx, "bind finalized commit", func(conn *sql.Conn) error {
		if err := requireConfiguredDeviceConn(ctx, conn, deviceID); err != nil {
			return err
		}
		var state, manifestID, receipt string
		var generation int64
		err := conn.QueryRowContext(ctx, `SELECT state, manifest_id, ack_receipt, ack_generation
			FROM outbox_generations WHERE capture_id = ?`, captureID,
		).Scan(&state, &manifestID, &receipt, &generation)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAcknowledgeConflict
		}
		if err != nil {
			return fmt.Errorf("rawcheckpoint: bind finalized commit: %w", err)
		}
		if state != "finalized" {
			return ErrAcknowledgeConflict
		}
		if manifestID == commit.ManifestID && receipt == commit.Receipt &&
			generation == commit.Generation {
			return nil
		}
		if manifestID != "" || receipt != "" || generation != 0 {
			return ErrAcknowledgeConflict
		}
		result, err := conn.ExecContext(ctx, `UPDATE outbox_generations
			SET manifest_id = ?, ack_receipt = ?, ack_generation = ?, updated_at = ?
			WHERE capture_id = ? AND state = 'finalized'
				AND manifest_id = '' AND ack_receipt = '' AND ack_generation = 0`,
			commit.ManifestID, commit.Receipt, commit.Generation,
			s.now().UTC().Format(time.RFC3339Nano), captureID,
		)
		if err != nil {
			return fmt.Errorf("rawcheckpoint: bind finalized commit: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return ErrAcknowledgeConflict
		}
		return nil
	})
}

// FinalizedCommit returns a server result durably recorded for a finalized
// generation. The bool is false when the generation still needs committing.
func (s *Store) FinalizedCommit(
	ctx context.Context,
	deviceID, captureID string,
) (rawsync.CommitResult, bool, error) {
	if captureID == "" {
		return rawsync.CommitResult{}, false, ErrAcknowledgeConflict
	}
	var commit rawsync.CommitResult
	found := false
	err := s.withImmediateWrite(ctx, "read finalized commit", func(conn *sql.Conn) error {
		if err := requireConfiguredDeviceConn(ctx, conn, deviceID); err != nil {
			return err
		}
		var state string
		err := conn.QueryRowContext(ctx, `SELECT state, manifest_id, ack_receipt, ack_generation
			FROM outbox_generations WHERE capture_id = ?`, captureID,
		).Scan(&state, &commit.ManifestID, &commit.Receipt, &commit.Generation)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAcknowledgeConflict
		}
		if err != nil {
			return fmt.Errorf("rawcheckpoint: read finalized commit: %w", err)
		}
		if state != "finalized" {
			return ErrAcknowledgeConflict
		}
		if commit.ManifestID == "" && commit.Receipt == "" && commit.Generation == 0 {
			return nil
		}
		if err := rawsync.ValidateCommitResult(commit); err != nil {
			return fmt.Errorf("rawcheckpoint: invalid stored finalized commit: %w", err)
		}
		found = true
		return nil
	})
	if err != nil {
		return rawsync.CommitResult{}, false, err
	}
	return commit, found, nil
}

// FinalizeNextManifest returns the oldest uploadable generation and durably
// freezes the acknowledged parent receipt used by all retries.
func (s *Store) FinalizeNextManifest(
	ctx context.Context,
	deviceID string,
) (rawsync.Manifest, bool, error) {
	var manifest rawsync.Manifest
	found := false
	err := s.withImmediateWrite(ctx, "finalize next manifest", func(conn *sql.Conn) error {
		if err := requireConfiguredDeviceConn(ctx, conn, deviceID); err != nil {
			return err
		}
		var provider, capturedAt, kind, state, expectedParent string
		err := conn.QueryRowContext(ctx, `SELECT generation.capture_id,
			generation.provider, generation.configured_root_id,
			generation.source_key, generation.captured_at, generation.kind,
			generation.state, generation.expected_parent_receipt
			FROM outbox_generations AS generation
			LEFT JOIN outbox_generations AS predecessor
				ON predecessor.capture_id = generation.predecessor_capture_id
			WHERE generation.state IN ('queued', 'finalized')
			AND generation.blocked = 0
			AND (generation.retry_at = '' OR generation.retry_at <= ?)
			AND (generation.predecessor_capture_id IS NULL
				OR predecessor.state = 'acknowledged')
			ORDER BY generation.captured_at, generation.capture_id LIMIT 1`,
			checkpointTimestamp(s.now()),
		).Scan(&manifest.CaptureID, &provider, &manifest.ConfiguredRootID,
			&manifest.SourceKey, &capturedAt, &kind, &state, &expectedParent)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("rawcheckpoint: finalize manifest: select generation: %w", err)
		}
		manifest.SchemaVersion = rawsync.ManifestSchemaVersion
		manifest.Provider = parser.AgentType(provider)
		manifest.CapturedAt, _ = time.Parse(time.RFC3339Nano, capturedAt)
		manifest.Kind = rawsync.ManifestKind(kind)
		if state == "queued" {
			if err := conn.QueryRowContext(ctx, `SELECT head_receipt FROM raw_sources
				WHERE provider = ? AND configured_root_id = ? AND source_key = ?`,
				provider, manifest.ConfiguredRootID, manifest.SourceKey,
			).Scan(&expectedParent); err != nil {
				return fmt.Errorf("rawcheckpoint: finalize manifest: read source head: %w", err)
			}
			now := checkpointTimestamp(s.now())
			if _, err := conn.ExecContext(ctx, `UPDATE outbox_generations SET
				state = 'finalized', expected_parent_receipt = ?, updated_at = ?
				WHERE capture_id = ? AND state = 'queued'`,
				expectedParent, now, manifest.CaptureID); err != nil {
				return fmt.Errorf("rawcheckpoint: finalize manifest: persist parent: %w", err)
			}
		}
		manifest.ExpectedParentReceipt = expectedParent
		entries, err := loadManifestEntriesConn(ctx, conn, manifest.CaptureID)
		if err != nil {
			return err
		}
		manifest.Entries = entries
		found = true
		return nil
	})
	return manifest, found && err == nil, err
}

// RecordGenerationFailure durably delays a transient retry or blocks one
// source chain after a parent-receipt conflict. Other source chains remain
// independently uploadable.
func (s *Store) RecordGenerationFailure(
	ctx context.Context,
	deviceID, captureID string,
	class GenerationFailureClass,
	retryAt time.Time,
) error {
	blocked := 0
	attemptIncrement := 0
	retryStamp := ""
	switch class {
	case GenerationFailureTransient:
		if retryAt.IsZero() {
			return ErrGenerationFailureConflict
		}
		retryStamp = checkpointTimestamp(retryAt)
		attemptIncrement = 1
	case GenerationFailureParentReceiptConflict:
		if !retryAt.IsZero() {
			return ErrGenerationFailureConflict
		}
		blocked = 1
	case GenerationFailurePermanent:
		if !retryAt.IsZero() {
			return ErrGenerationFailureConflict
		}
		blocked = 1
	default:
		return ErrGenerationFailureConflict
	}
	return s.withImmediateWrite(ctx, "record generation failure", func(conn *sql.Conn) error {
		if err := requireConfiguredDeviceConn(ctx, conn, deviceID); err != nil {
			return err
		}
		result, err := conn.ExecContext(ctx, `UPDATE outbox_generations SET
			retry_at = ?, error_class = ?, blocked = ?,
			attempt_count = attempt_count + ?, updated_at = ?
			WHERE capture_id = ? AND state = 'finalized'
				AND (blocked = 0 OR ? = 1)`, retryStamp, string(class),
			blocked, attemptIncrement, checkpointTimestamp(s.now()), captureID, blocked)
		if err != nil {
			return fmt.Errorf("rawcheckpoint: record generation failure: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return ErrGenerationFailureConflict
		}
		return nil
	})
}

// GenerationAttempts returns the durable transient failure count for one
// finalized generation, fenced to the configured device.
func (s *Store) GenerationAttempts(
	ctx context.Context,
	deviceID, captureID string,
) (int, error) {
	var attempts int
	err := s.withImmediateWrite(ctx, "read generation attempts", func(conn *sql.Conn) error {
		if err := requireConfiguredDeviceConn(ctx, conn, deviceID); err != nil {
			return err
		}
		if err := conn.QueryRowContext(ctx, `SELECT attempt_count
			FROM outbox_generations WHERE capture_id = ? AND state = 'finalized'`,
			captureID,
		).Scan(&attempts); errors.Is(err, sql.ErrNoRows) {
			return ErrGenerationFailureConflict
		} else if err != nil {
			return fmt.Errorf("rawcheckpoint: read generation attempts: %w", err)
		}
		return nil
	})
	return attempts, err
}

// DiscardRejectedGeneration removes a permanently rejected generation and
// every unacknowledged descendant, then resets the source to its last durable
// server head so a later audit can create a fresh replacement generation.
func (s *Store) DiscardRejectedGeneration(
	ctx context.Context,
	deviceID, captureID string,
) (GarbageCollectionReport, error) {
	if captureID == "" {
		return GarbageCollectionReport{}, ErrGenerationFailureConflict
	}
	err := s.withImmediateWrite(ctx, "discard rejected generation", func(conn *sql.Conn) error {
		if err := requireConfiguredDeviceConn(ctx, conn, deviceID); err != nil {
			return err
		}
		return discardRejectedGenerationConn(ctx, conn, captureID, s)
	})
	if err != nil {
		return GarbageCollectionReport{}, err
	}
	return s.CollectGarbage(ctx)
}

func discardRejectedGenerationConn(
	ctx context.Context,
	conn *sql.Conn,
	captureID string,
	store *Store,
) error {
	rows, err := conn.QueryContext(ctx, `WITH RECURSIVE suffix(capture_id) AS (
			SELECT capture_id FROM outbox_generations
			WHERE capture_id = ? AND state = 'finalized'
			UNION ALL
			SELECT generation.capture_id FROM outbox_generations AS generation
			JOIN suffix ON generation.predecessor_capture_id = suffix.capture_id
			WHERE generation.state != 'acknowledged'
		)
		SELECT capture_id FROM suffix`, captureID)
	if err != nil {
		return fmt.Errorf("rawcheckpoint: discard rejected generation: %w", err)
	}
	var discarded []string
	for rows.Next() {
		var current string
		if err := rows.Scan(&current); err != nil {
			rows.Close()
			return fmt.Errorf("rawcheckpoint: discard rejected generation: %w", err)
		}
		discarded = append(discarded, current)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("rawcheckpoint: discard rejected generation: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("rawcheckpoint: discard rejected generation: %w", err)
	}
	if len(discarded) == 0 {
		return ErrGenerationFailureConflict
	}
	for _, current := range discarded {
		if err := releaseGenerationObjectsConn(ctx, conn, current); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx,
			`DELETE FROM outbox_entries WHERE capture_id = ?`, current); err != nil {
			return fmt.Errorf("rawcheckpoint: discard rejected entries: %w", err)
		}
	}
	if err := resetInvalidSourcesConn(
		ctx, conn, discarded, store, "server_rejected",
	); err != nil {
		return err
	}
	for _, current := range discarded {
		if _, err := conn.ExecContext(ctx,
			`DELETE FROM outbox_generations WHERE capture_id = ?`, current); err != nil {
			return fmt.Errorf("rawcheckpoint: discard rejected generation: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, `UPDATE outbox_objects
			SET state = 'garbage_pending'
			WHERE ref_count = 0 AND state != 'remote'`); err != nil {
		return fmt.Errorf("rawcheckpoint: discard rejected objects: %w", err)
	}
	return nil
}

// ResumeGeneration atomically reconciles a parent-receipt conflict to the
// reported server head and requeues the local generation to freeze that new
// parent. It accepts an untouched finalized generation, a transiently failed
// generation without a durable commit result, or an older blocked conflict.
func (s *Store) ResumeGeneration(
	ctx context.Context,
	deviceID, captureID string,
	reconciled SourceHead,
) error {
	if err := validateReconciledSourceHead(reconciled); err != nil {
		return fmt.Errorf("rawcheckpoint: invalid reconciled head: %w", err)
	}
	return s.withImmediateWrite(ctx, "resume generation", func(conn *sql.Conn) error {
		if err := requireConfiguredDeviceConn(ctx, conn, deviceID); err != nil {
			return err
		}
		var source SourceIdentity
		var state, failureClass, manifestID, receipt string
		var generation int64
		var blocked int
		err := conn.QueryRowContext(ctx, `SELECT provider, configured_root_id,
			source_key, state, error_class, blocked, manifest_id, ack_receipt,
			ack_generation FROM outbox_generations
			WHERE capture_id = ?`, captureID,
		).Scan(&source.Provider, &source.ConfiguredRootID, &source.SourceKey,
			&state, &failureClass, &blocked, &manifestID, &receipt, &generation)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrGenerationFailureConflict
		}
		if err != nil {
			return fmt.Errorf("rawcheckpoint: resume generation: read state: %w", err)
		}
		cleanFinalized := failureClass == "" && blocked == 0
		transientFailure := failureClass == string(GenerationFailureTransient) &&
			blocked == 0 && manifestID == "" && receipt == "" && generation == 0
		blockedConflict := failureClass == string(GenerationFailureParentReceiptConflict) &&
			blocked == 1
		if state != "finalized" || (!cleanFinalized && !transientFailure && !blockedConflict) {
			return ErrGenerationFailureConflict
		}
		now := s.now().UTC().Format(time.RFC3339Nano)
		if _, err := conn.ExecContext(ctx, `UPDATE raw_sources SET
			head_capture_id = '', head_manifest_id = ?, head_receipt = ?,
			head_generation = ?, updated_at = ?
			WHERE provider = ? AND configured_root_id = ? AND source_key = ?`,
			reconciled.ManifestID, reconciled.Receipt, reconciled.Generation, now,
			string(source.Provider), source.ConfiguredRootID, source.SourceKey); err != nil {
			return fmt.Errorf("rawcheckpoint: resume generation: reconcile source head: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM raw_source_base_entries
			WHERE provider = ? AND configured_root_id = ? AND source_key = ?`,
			string(source.Provider), source.ConfiguredRootID, source.SourceKey); err != nil {
			return fmt.Errorf("rawcheckpoint: resume generation: clear stale append base: %w", err)
		}
		result, err := conn.ExecContext(ctx, `UPDATE outbox_generations SET
			state = 'queued', expected_parent_receipt = '', manifest_id = '',
			ack_receipt = '', ack_generation = 0,
			retry_at = '', error_class = '', blocked = 0, attempt_count = 0,
			updated_at = ?
			WHERE capture_id = ? AND state = 'finalized'
			AND ((error_class = '' AND blocked = 0)
				OR (error_class = ? AND blocked = 0
					AND manifest_id = '' AND ack_receipt = '' AND ack_generation = 0)
				OR (error_class = ? AND blocked = 1))`, now, captureID,
			string(GenerationFailureTransient),
			string(GenerationFailureParentReceiptConflict))
		if err != nil {
			return fmt.Errorf("rawcheckpoint: resume generation: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return ErrGenerationFailureConflict
		}
		return nil
	})
}

func validateReconciledSourceHead(head SourceHead) error {
	if head.ManifestID == "" && head.Receipt == "" && head.Generation == 0 {
		return nil
	}
	return rawsync.ValidateCommitResult(rawsync.CommitResult{
		ManifestID: head.ManifestID,
		Receipt:    head.Receipt,
		Generation: head.Generation,
	})
}

// AcknowledgeGeneration atomically fences a durable server result to the
// finalized local generation, advances its source head, and releases local
// object references while retaining the acknowledged append base metadata.
func (s *Store) AcknowledgeGeneration(
	ctx context.Context,
	deviceID, captureID string,
	commit rawsync.CommitResult,
) (AcknowledgeResult, error) {
	if captureID == "" {
		return AcknowledgeResult{}, ErrAcknowledgeConflict
	}
	if err := rawsync.ValidateCommitResult(commit); err != nil {
		return AcknowledgeResult{}, fmt.Errorf("rawcheckpoint: invalid commit result: %w", err)
	}
	var result AcknowledgeResult
	err := s.withImmediateWrite(ctx, "acknowledge generation", func(conn *sql.Conn) error {
		if err := requireConfiguredDeviceConn(ctx, conn, deviceID); err != nil {
			return err
		}
		var source SourceIdentity
		var state, expectedParent, storedManifestID, storedReceipt string
		var storedGeneration int64
		var current SourceHead
		err := conn.QueryRowContext(ctx, `SELECT generation.provider,
			generation.configured_root_id, generation.source_key, generation.state,
			generation.expected_parent_receipt, generation.manifest_id,
			generation.ack_receipt, generation.ack_generation,
			source.head_manifest_id, source.head_receipt, source.head_generation
			FROM outbox_generations AS generation
			JOIN raw_sources AS source
				ON source.provider = generation.provider
				AND source.configured_root_id = generation.configured_root_id
				AND source.source_key = generation.source_key
			WHERE generation.capture_id = ?`, captureID,
		).Scan(&source.Provider, &source.ConfiguredRootID, &source.SourceKey,
			&state, &expectedParent, &storedManifestID, &storedReceipt, &storedGeneration,
			&current.ManifestID, &current.Receipt, &current.Generation)
		if errors.Is(err, sql.ErrNoRows) {
			var replay SourceHead
			err = conn.QueryRowContext(ctx, `SELECT head_manifest_id, head_receipt,
				head_generation FROM raw_sources WHERE head_capture_id = ?`, captureID,
			).Scan(&replay.ManifestID, &replay.Receipt, &replay.Generation)
			if err == nil && replay.ManifestID == commit.ManifestID &&
				replay.Receipt == commit.Receipt && replay.Generation == commit.Generation {
				result.Replayed = true
				return nil
			}
			return ErrAcknowledgeConflict
		}
		if err != nil {
			return fmt.Errorf("rawcheckpoint: acknowledge generation: read state: %w", err)
		}
		if state == "acknowledged" {
			if storedManifestID == commit.ManifestID && storedReceipt == commit.Receipt &&
				storedGeneration == commit.Generation {
				result.Replayed = true
				return nil
			}
			return ErrAcknowledgeConflict
		}
		if state != "finalized" || storedManifestID == "" ||
			storedManifestID != commit.ManifestID ||
			((storedReceipt != "" || storedGeneration != 0) &&
				(storedReceipt != commit.Receipt || storedGeneration != commit.Generation)) ||
			expectedParent != current.Receipt ||
			commit.Generation != current.Generation+1 {
			return ErrAcknowledgeConflict
		}
		entries, err := loadCapturedEntriesConn(ctx, conn, captureID)
		if err != nil {
			return err
		}
		if err := replaceAcknowledgedBaseConn(ctx, conn, source, entries); err != nil {
			return err
		}
		if err := releaseGenerationObjectsConn(ctx, conn, captureID); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM outbox_objects
			WHERE state = 'remote' AND ref_count = 0 AND NOT EXISTS (
				SELECT 1 FROM raw_source_base_objects AS base
				WHERE base.sha256 = outbox_objects.sha256
				AND base.length = outbox_objects.length
			)`); err != nil {
			return fmt.Errorf("rawcheckpoint: acknowledge generation: prune remote objects: %w", err)
		}
		now := s.now().UTC().Format(time.RFC3339Nano)
		if _, err := conn.ExecContext(ctx, `DELETE FROM outbox_entries WHERE capture_id = ?`,
			captureID); err != nil {
			return fmt.Errorf("rawcheckpoint: acknowledge generation: release entries: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `UPDATE raw_sources SET
			head_capture_id = ?, head_manifest_id = ?, head_receipt = ?,
			head_generation = ?, updated_at = ?
			WHERE provider = ? AND configured_root_id = ? AND source_key = ?`,
			captureID, commit.ManifestID, commit.Receipt, commit.Generation, now,
			string(source.Provider), source.ConfiguredRootID, source.SourceKey); err != nil {
			return fmt.Errorf("rawcheckpoint: acknowledge generation: advance head: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `UPDATE outbox_generations
			SET predecessor_capture_id = NULL, updated_at = ?
			WHERE predecessor_capture_id = ?`, now, captureID); err != nil {
			return fmt.Errorf("rawcheckpoint: acknowledge generation: detach successor: %w", err)
		}
		if _, err := conn.ExecContext(ctx,
			`DELETE FROM outbox_generations WHERE capture_id = ?`, captureID); err != nil {
			return fmt.Errorf("rawcheckpoint: acknowledge generation: compact generation: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `UPDATE outbox_objects
			SET state = 'garbage_pending'
			WHERE ref_count = 0 AND state != 'remote'`); err != nil {
			return fmt.Errorf("rawcheckpoint: acknowledge generation: mark garbage: %w", err)
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	result.Garbage, err = s.CollectGarbage(ctx)
	return result, err
}

func requireConfiguredDeviceConn(ctx context.Context, conn *sql.Conn, deviceID string) error {
	var configured string
	err := conn.QueryRowContext(ctx,
		`SELECT device_id FROM device_config WHERE id = 1`).Scan(&configured)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrDeviceNotConfigured
	case err != nil:
		return fmt.Errorf("rawcheckpoint: read configured device: %w", err)
	case configured != deviceID:
		return ErrDeviceMismatch
	default:
		return nil
	}
}

func loadManifestEntriesConn(
	ctx context.Context,
	conn *sql.Conn,
	captureID string,
) ([]rawsync.Entry, error) {
	captured, err := loadCapturedEntriesConn(ctx, conn, captureID)
	if err != nil {
		return nil, err
	}
	entries := make([]rawsync.Entry, 0, len(captured))
	for _, entry := range captured {
		entries = append(entries, rawsync.Entry{
			Path: entry.Path, Type: "file", Length: entry.Length,
			Objects: append([]rawsync.ObjectRef(nil), entry.Objects...),
		})
	}
	return entries, nil
}

func loadCapturedEntriesConn(
	ctx context.Context,
	conn *sql.Conn,
	captureID string,
) ([]CapturedEntry, error) {
	rows, err := conn.QueryContext(ctx, `SELECT entry_ordinal, path, length,
		mod_time_ns, file_identity, prefix_sha256, appendable
		FROM outbox_entries WHERE capture_id = ? ORDER BY entry_ordinal`, captureID)
	if err != nil {
		return nil, fmt.Errorf("rawcheckpoint: load captured entries: %w", err)
	}
	defer rows.Close()
	entries := make([]CapturedEntry, 0)
	ordinals := make([]int, 0)
	for rows.Next() {
		var entry CapturedEntry
		var ordinal int
		if err := rows.Scan(&ordinal, &entry.Path, &entry.Length, &entry.ModTimeNS,
			&entry.FileIdentity, &entry.PrefixSHA256, &entry.Appendable); err != nil {
			return nil, fmt.Errorf("rawcheckpoint: load captured entries: %w", err)
		}
		entries = append(entries, entry)
		ordinals = append(ordinals, ordinal)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rawcheckpoint: load captured entries: %w", err)
	}
	rows.Close()
	for i, ordinal := range ordinals {
		objectRows, err := conn.QueryContext(ctx, `SELECT sha256, length
			FROM outbox_entry_objects WHERE capture_id = ? AND entry_ordinal = ?
			ORDER BY object_ordinal`, captureID, ordinal)
		if err != nil {
			return nil, fmt.Errorf("rawcheckpoint: load captured objects: %w", err)
		}
		for objectRows.Next() {
			var ref rawsync.ObjectRef
			if err := objectRows.Scan(&ref.SHA256, &ref.Length); err != nil {
				objectRows.Close()
				return nil, fmt.Errorf("rawcheckpoint: load captured objects: %w", err)
			}
			entries[i].Objects = append(entries[i].Objects, ref)
		}
		if err := objectRows.Err(); err != nil {
			objectRows.Close()
			return nil, fmt.Errorf("rawcheckpoint: load captured objects: %w", err)
		}
		if err := objectRows.Close(); err != nil {
			return nil, fmt.Errorf("rawcheckpoint: load captured objects: %w", err)
		}
	}
	return entries, nil
}

func replaceAcknowledgedBaseConn(
	ctx context.Context,
	conn *sql.Conn,
	source SourceIdentity,
	entries []CapturedEntry,
) error {
	if _, err := conn.ExecContext(ctx, `DELETE FROM raw_source_base_entries
		WHERE provider = ? AND configured_root_id = ? AND source_key = ?`,
		string(source.Provider), source.ConfiguredRootID, source.SourceKey); err != nil {
		return fmt.Errorf("rawcheckpoint: replace acknowledged base: %w", err)
	}
	for entryOrdinal, entry := range entries {
		if _, err := conn.ExecContext(ctx, `INSERT INTO raw_source_base_entries
			(provider, configured_root_id, source_key, entry_ordinal, path, length,
			 mod_time_ns, file_identity, prefix_sha256, appendable)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, string(source.Provider),
			source.ConfiguredRootID, source.SourceKey, entryOrdinal, entry.Path,
			entry.Length, entry.ModTimeNS, entry.FileIdentity, entry.PrefixSHA256,
			entry.Appendable); err != nil {
			return fmt.Errorf("rawcheckpoint: replace acknowledged base entry: %w", err)
		}
		for objectOrdinal, ref := range entry.Objects {
			if _, err := conn.ExecContext(ctx, `INSERT INTO raw_source_base_objects
				(provider, configured_root_id, source_key, entry_ordinal,
				 object_ordinal, sha256, length) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				string(source.Provider), source.ConfiguredRootID, source.SourceKey,
				entryOrdinal, objectOrdinal, ref.SHA256, ref.Length); err != nil {
				return fmt.Errorf("rawcheckpoint: replace acknowledged base object: %w", err)
			}
		}
	}
	return nil
}

func releaseGenerationObjectsConn(ctx context.Context, conn *sql.Conn, captureID string) error {
	rows, err := conn.QueryContext(ctx, `SELECT sha256, length, count(*)
		FROM outbox_entry_objects WHERE capture_id = ? GROUP BY sha256, length`, captureID)
	if err != nil {
		return fmt.Errorf("rawcheckpoint: release generation objects: %w", err)
	}
	type referenced struct {
		ref   rawsync.ObjectRef
		count int64
	}
	var refs []referenced
	for rows.Next() {
		var item referenced
		if err := rows.Scan(&item.ref.SHA256, &item.ref.Length, &item.count); err != nil {
			rows.Close()
			return fmt.Errorf("rawcheckpoint: release generation objects: %w", err)
		}
		refs = append(refs, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("rawcheckpoint: release generation objects: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("rawcheckpoint: release generation objects: %w", err)
	}
	for _, item := range refs {
		result, err := conn.ExecContext(ctx, `UPDATE outbox_objects
			SET ref_count = ref_count - ?
			WHERE sha256 = ? AND length = ? AND ref_count >= ?`,
			item.count, item.ref.SHA256, item.ref.Length, item.count)
		if err != nil {
			return fmt.Errorf("rawcheckpoint: release generation object: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return fmt.Errorf("rawcheckpoint: release generation object reference mismatch")
		}
	}
	return nil
}
