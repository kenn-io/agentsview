package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const maxArtifactQueuePageSize = 1024

const artifactResetRepublishPendingKey = "artifact_reset_republish_pending"

var (
	// ErrArtifactExportClaimStale tells callers to discard computed export
	// output and retry from a fresh queue claim.
	ErrArtifactExportClaimStale = errors.New("artifact export claim is stale")
	// ErrArtifactRepairClaimStale tells callers that the expected repair
	// identity changed while repair work was in flight.
	ErrArtifactRepairClaimStale = errors.New("artifact repair claim is stale")
)

// ArtifactExportQueueItem identifies one locally-owned session whose artifact
// publication may no longer match the archive.
type ArtifactExportQueueItem struct {
	SessionID  string
	EnqueuedAt string
	Generation int64
}

// ArtifactImportWork identifies one exact immutable artifact whose import must
// be retried after the current bounded transfer pass.
type ArtifactImportWork struct {
	Origin                string
	Kind                  string
	Name                  string
	SHA256                string
	Size                  int64
	Reason                string
	RequiredFormatVersion int
	EnqueuedAt            string
}

func validateArtifactImportWork(work ArtifactImportWork, requireClaim bool) error {
	if strings.TrimSpace(work.Origin) == "" || work.Origin != strings.TrimSpace(work.Origin) {
		return errors.New("artifact import origin is required")
	}
	if work.Kind != "checkpoints" && work.Kind != "meta" {
		return errors.New("artifact import kind must be checkpoints or meta")
	}
	if strings.TrimSpace(work.Name) == "" || strings.ContainsAny(work.Name, `/\\`) {
		return errors.New("artifact import name is required")
	}
	if len(work.SHA256) != 64 {
		return errors.New("complete artifact import identity is required")
	}
	for _, c := range work.SHA256 {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return errors.New("artifact import identity must be lowercase hexadecimal")
		}
	}
	if work.Size < 0 {
		return errors.New("artifact import size must not be negative")
	}
	if strings.TrimSpace(work.Reason) == "" || work.Reason != strings.TrimSpace(work.Reason) {
		return errors.New("artifact import reason is required")
	}
	if work.RequiredFormatVersion < 1 {
		return errors.New("artifact import required format version must be positive")
	}
	if work.Kind == "meta" {
		base := strings.TrimSuffix(work.Name, ".json")
		separator := strings.LastIndexByte(base, '-')
		if base == work.Name || separator < 1 || base[separator+1:] != work.SHA256 {
			return errors.New("artifact import metadata name must match its identity")
		}
	}
	if work.Kind == "checkpoints" {
		if _, err := artifactImportCheckpointSequence(work.Name); err != nil {
			return err
		}
	}
	if requireClaim && strings.TrimSpace(work.EnqueuedAt) == "" {
		return errors.New("artifact import enqueue time is required")
	}
	return nil
}

func artifactImportCheckpointSequence(name string) (int64, error) {
	if len(name) != len("cp-0000000000.json") || !strings.HasPrefix(name, "cp-") ||
		!strings.HasSuffix(name, ".json") {
		return 0, errors.New("canonical artifact checkpoint name is required")
	}
	digits := strings.TrimSuffix(strings.TrimPrefix(name, "cp-"), ".json")
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return 0, errors.New("canonical artifact checkpoint name is required")
		}
	}
	sequence, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || sequence < 1 {
		return 0, errors.New("positive artifact checkpoint sequence is required")
	}
	return sequence, nil
}

// EnqueueArtifactImport retains one exact retry claim. Repeated observations
// of the same immutable identity are idempotent; conflicting identities fail
// closed. A newer checkpoint supersedes older queued checkpoints for its
// origin, while metadata events are retained independently.
func (db *DB) EnqueueArtifactImport(ctx context.Context, work ArtifactImportWork) error {
	if err := validateArtifactImportWork(work, false); err != nil {
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
		SELECT sha256, size FROM artifact_import_queue
		WHERE origin = ? AND kind = ? AND name = ?`,
		work.Origin, work.Kind, work.Name,
	).Scan(&existingSHA, &existingSize)
	if err == nil {
		if existingSHA != work.SHA256 || existingSize != work.Size {
			return errors.New("artifact import reference has a conflicting identity")
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE artifact_import_queue SET
				reason = ?,
				required_format_version = max(required_format_version, ?)
			WHERE origin = ? AND kind = ? AND name = ?`,
			work.Reason, work.RequiredFormatVersion, work.Origin, work.Kind, work.Name,
		); err != nil {
			return fmt.Errorf("refreshing artifact import work: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing artifact import refresh: %w", err)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading artifact import identity: %w", err)
	}

	if work.Kind == "checkpoints" {
		var newer bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM artifact_import_queue
				WHERE origin = ? AND kind = 'checkpoints' AND name > ?
			)`, work.Origin, work.Name).Scan(&newer); err != nil {
			return fmt.Errorf("reading newer artifact checkpoint work: %w", err)
		}
		if newer {
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("committing superseded artifact checkpoint: %w", err)
			}
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM artifact_import_queue
			WHERE origin = ? AND kind = 'checkpoints' AND name < ?`,
			work.Origin, work.Name,
		); err != nil {
			return fmt.Errorf("retiring older artifact checkpoint work: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO artifact_import_queue(
			origin, kind, name, sha256, size, reason, required_format_version
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		work.Origin, work.Kind, work.Name, work.SHA256, work.Size,
		work.Reason, work.RequiredFormatVersion,
	); err != nil {
		return fmt.Errorf("enqueueing artifact import work: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing artifact import enqueue: %w", err)
	}
	return nil
}

// PendingArtifactImports returns one bounded FIFO page whose required format
// is understood by readerFormatVersion.
func (db *DB) PendingArtifactImports(
	ctx context.Context, readerFormatVersion, limit int,
) ([]ArtifactImportWork, error) {
	if readerFormatVersion < 1 {
		return nil, errors.New("artifact import reader format version must be positive")
	}
	if limit < 1 || limit > maxArtifactQueuePageSize {
		return nil, fmt.Errorf("artifact import page size must be between 1 and %d",
			maxArtifactQueuePageSize)
	}
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT origin, kind, name, sha256, size, reason,
		       required_format_version, enqueued_at
		FROM artifact_import_queue
		WHERE required_format_version <= ?
		ORDER BY enqueued_at, origin, kind, name
		LIMIT ?`, readerFormatVersion, limit)
	if err != nil {
		return nil, fmt.Errorf("reading pending artifact imports: %w", err)
	}
	defer rows.Close()
	work := make([]ArtifactImportWork, 0, min(limit, 64))
	for rows.Next() {
		var item ArtifactImportWork
		if err := rows.Scan(
			&item.Origin, &item.Kind, &item.Name, &item.SHA256, &item.Size,
			&item.Reason, &item.RequiredFormatVersion, &item.EnqueuedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning pending artifact import: %w", err)
		}
		work = append(work, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating pending artifact imports: %w", err)
	}
	return work, nil
}

// AcknowledgeArtifactImport compare-and-deletes one exact queue claim.
func (db *DB) AcknowledgeArtifactImport(
	ctx context.Context, work ArtifactImportWork,
) (bool, error) {
	if err := validateArtifactImportWork(work, true); err != nil {
		return false, err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	result, err := db.getWriter().ExecContext(ctx, `
		DELETE FROM artifact_import_queue
		WHERE origin = ? AND kind = ? AND name = ? AND sha256 = ? AND size = ?
		  AND reason = ? AND required_format_version = ? AND enqueued_at = ?`,
		work.Origin, work.Kind, work.Name, work.SHA256, work.Size, work.Reason,
		work.RequiredFormatVersion, work.EnqueuedAt,
	)
	if err != nil {
		return false, fmt.Errorf("acknowledging artifact import: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("reading artifact import acknowledgement: %w", err)
	}
	return rows == 1, nil
}

// ArtifactImportQueueStats reports all durable unfinished work, including
// future-format rows that are not yet eligible to drain.
func (db *DB) ArtifactImportQueueStats(ctx context.Context) (int, string, error) {
	var count int
	var oldest string
	if err := db.getReader().QueryRowContext(ctx, `
		SELECT count(*), coalesce(min(enqueued_at), '')
		FROM artifact_import_queue`).Scan(&count, &oldest); err != nil {
		return 0, "", fmt.Errorf("reading artifact import queue statistics: %w", err)
	}
	return count, oldest, nil
}

// ArtifactResetRepublishPending is the durable authority for reconstructing a
// repository after its previous vault has been moved aside. RootFingerprint
// binds the intent to one canonical repository without persisting its path.
type ArtifactResetRepublishPending struct {
	Version         int    `json:"v"`
	RootFingerprint string `json:"root_fingerprint"`
	Origin          string `json:"origin"`
	Token           string `json:"token"`
	BaselineHLC     string `json:"baseline_hlc"`
}

func validateArtifactResetRepublishPending(state ArtifactResetRepublishPending) error {
	if state.Version != 1 || len(state.RootFingerprint) != 64 ||
		strings.TrimSpace(state.Origin) == "" || len(state.Token) != 64 ||
		strings.TrimSpace(state.BaselineHLC) == "" {
		return errors.New("complete artifact reset republish state is required")
	}
	for _, value := range []string{state.RootFingerprint, state.Token} {
		for _, c := range value {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return errors.New("artifact reset republish identity must be lowercase hexadecimal")
			}
		}
	}
	return nil
}

// SetArtifactResetRepublishPending stores the singleton reset intent.
func (db *DB) SetArtifactResetRepublishPending(
	ctx context.Context, state ArtifactResetRepublishPending,
) error {
	if err := validateArtifactResetRepublishPending(state); err != nil {
		return err
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encoding artifact reset republish state: %w", err)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err = db.getWriter().ExecContext(ctx, `
		INSERT INTO pg_sync_state(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		artifactResetRepublishPendingKey, string(encoded),
	)
	if err != nil {
		return fmt.Errorf("storing artifact reset republish state: %w", err)
	}
	return nil
}

// ArtifactResetRepublishPending returns the durable reset intent, if present.
func (db *DB) ArtifactResetRepublishPending(
	ctx context.Context,
) (ArtifactResetRepublishPending, bool, error) {
	var encoded string
	err := db.getReader().QueryRowContext(ctx,
		`SELECT value FROM pg_sync_state WHERE key = ?`,
		artifactResetRepublishPendingKey,
	).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return ArtifactResetRepublishPending{}, false, nil
	}
	if err != nil {
		return ArtifactResetRepublishPending{}, false,
			fmt.Errorf("reading artifact reset republish state: %w", err)
	}
	var state ArtifactResetRepublishPending
	if err := json.Unmarshal([]byte(encoded), &state); err != nil {
		return ArtifactResetRepublishPending{}, false,
			fmt.Errorf("decoding artifact reset republish state: %w", err)
	}
	if err := validateArtifactResetRepublishPending(state); err != nil {
		return ArtifactResetRepublishPending{}, false, err
	}
	return state, true, nil
}

// ClearArtifactResetRepublishPending compare-and-swap clears one completed
// intent without allowing a stale recovery to erase a newer reset.
func (db *DB) ClearArtifactResetRepublishPending(
	ctx context.Context, state ArtifactResetRepublishPending,
) (bool, error) {
	if err := validateArtifactResetRepublishPending(state); err != nil {
		return false, err
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return false, fmt.Errorf("encoding artifact reset republish state: %w", err)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	result, err := db.getWriter().ExecContext(ctx,
		`DELETE FROM pg_sync_state WHERE key = ? AND value = ?`,
		artifactResetRepublishPendingKey, string(encoded),
	)
	if err != nil {
		return false, fmt.Errorf("clearing artifact reset republish state: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("reading artifact reset republish clear result: %w", err)
	}
	return rows == 1, nil
}

// ArtifactPublication is the last manifest selected for one locally-owned
// session. Rows are the authority used to stream a full checkpoint map.
type ArtifactPublication struct {
	Origin            string
	SessionID         string
	ManifestHash      string
	SourceFingerprint string
}

// ArtifactPublicationChange changes or removes one publication row. Delete is
// used when a queued session is no longer locally owned or no longer exists.
type ArtifactPublicationChange struct {
	SessionID         string
	Generation        int64
	ManifestHash      string
	SourceFingerprint string
	Delete            bool
}

// ArtifactCheckpointHead records the last successfully created checkpoint,
// the exact publication revision it represents, and its catalog identity.
type ArtifactCheckpointHead struct {
	Origin              string
	Sequence            int
	PublicationRevision int64
	SessionMapSHA256    string
	CheckpointSHA256    string
	CheckpointSize      int64
}

// ArtifactCheckpointLanding identifies the exact foreign checkpoint whose
// complete GID-to-manifest map has durable local provenance.
type ArtifactCheckpointLanding struct {
	Origin   string
	Sequence int
}

// ArtifactPeerCheckpointHead records the highest immutable checkpoint
// identity received for one foreign origin, including checkpoints whose
// dependency closure has not landed yet.
type ArtifactPeerCheckpointHead struct {
	Origin           string
	Sequence         int
	CheckpointSHA256 string
	CheckpointSize   int64
}

// ArtifactRepair identifies canonical content whose physical representation
// must be repaired from a trusted peer.
type ArtifactRepair struct {
	Origin     string
	Kind       string
	Name       string
	SHA256     string
	Size       int64
	DetectedAt string
}

// PendingArtifactExports returns the oldest bounded page of dirty local
// sessions. Reading work never acknowledges it.
func (db *DB) PendingArtifactExports(
	ctx context.Context, limit int,
) ([]ArtifactExportQueueItem, error) {
	if err := validateArtifactQueueLimit(limit); err != nil {
		return nil, err
	}
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT session_id, enqueued_at, generation
		FROM artifact_export_queue
		WHERE pending = 1
		ORDER BY enqueued_at, session_id
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("reading artifact export queue: %w", err)
	}
	defer rows.Close()
	items := make([]ArtifactExportQueueItem, 0, min(limit, 64))
	for rows.Next() {
		var item ArtifactExportQueueItem
		if err := rows.Scan(&item.SessionID, &item.EnqueuedAt, &item.Generation); err != nil {
			return nil, fmt.Errorf("scanning artifact export queue: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating artifact export queue: %w", err)
	}
	return items, nil
}

// ArtifactExportClaims returns pending generation claims for the exact bounded
// session set requested by a watcher batch. Missing or already-clean IDs are
// omitted; mutation APIs revalidate every returned generation under a writer
// reservation before changing publication authority.
func (db *DB) ArtifactExportClaims(
	ctx context.Context, sessionIDs []string,
) ([]ArtifactExportQueueItem, error) {
	if len(sessionIDs) == 0 {
		return []ArtifactExportQueueItem{}, nil
	}
	if len(sessionIDs) > maxArtifactQueuePageSize {
		return nil, fmt.Errorf("artifact export claim batch exceeds %d rows", maxArtifactQueuePageSize)
	}
	unique := make([]string, 0, len(sessionIDs))
	seen := make(map[string]struct{}, len(sessionIDs))
	for _, id := range sessionIDs {
		if id == "" {
			return nil, errors.New("artifact export claim session id is required")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(unique)), ",")
	args := make([]any, len(unique))
	for i, id := range unique {
		args[i] = id
	}
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT session_id, enqueued_at, generation
		FROM artifact_export_queue
		WHERE pending = 1 AND session_id IN (`+placeholders+`)
		ORDER BY session_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("reading exact artifact export claims: %w", err)
	}
	defer rows.Close()
	items := make([]ArtifactExportQueueItem, 0, len(unique))
	for rows.Next() {
		var item ArtifactExportQueueItem
		if err := rows.Scan(&item.SessionID, &item.EnqueuedAt, &item.Generation); err != nil {
			return nil, fmt.Errorf("scanning exact artifact export claim: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating exact artifact export claims: %w", err)
	}
	return items, nil
}

// ApplyArtifactPublicationChanges atomically applies a bounded export batch and
// returns the resulting per-origin publication revision. The revision advances
// only when a publication row changes. Queue rows remain pending until the
// resulting checkpoint has been created.
func (db *DB) ApplyArtifactPublicationChanges(
	ctx context.Context, origin string, changes []ArtifactPublicationChange,
) (int64, bool, error) {
	if origin == "" {
		return 0, false, errors.New("artifact publication origin is required")
	}
	if len(changes) > maxArtifactQueuePageSize {
		return 0, false, fmt.Errorf(
			"artifact publication batch exceeds %d rows", maxArtifactQueuePageSize,
		)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("beginning artifact publication changes: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockArtifactPublicationTx(ctx, tx); err != nil {
		return 0, false, err
	}
	claims := make([]ArtifactExportQueueItem, 0, len(changes))
	for _, change := range changes {
		claims = append(claims, ArtifactExportQueueItem{
			SessionID: change.SessionID, Generation: change.Generation,
		})
	}
	if _, err := validateArtifactExportClaimsTx(ctx, tx, claims); err != nil {
		return 0, false, err
	}
	changed := false
	for _, change := range changes {
		if change.SessionID == "" {
			return 0, false, errors.New("artifact publication session id is required")
		}
		var result sql.Result
		if change.Delete {
			result, err = tx.ExecContext(ctx, `
				DELETE FROM artifact_publications
				WHERE origin = ? AND session_id = ?`, origin, change.SessionID)
		} else {
			if change.ManifestHash == "" || change.SourceFingerprint == "" {
				return 0, false, fmt.Errorf(
					"artifact publication %s requires manifest hash and source fingerprint",
					change.SessionID,
				)
			}
			result, err = tx.ExecContext(ctx, `
				INSERT INTO artifact_publications (
					origin, session_id, manifest_hash, source_fingerprint
				) VALUES (?, ?, ?, ?)
				ON CONFLICT(origin, session_id) DO UPDATE SET
					manifest_hash = excluded.manifest_hash,
					source_fingerprint = excluded.source_fingerprint
				WHERE artifact_publications.manifest_hash <> excluded.manifest_hash
				   OR artifact_publications.source_fingerprint <> excluded.source_fingerprint`,
				origin, change.SessionID, change.ManifestHash, change.SourceFingerprint)
		}
		if err != nil {
			return 0, false, fmt.Errorf("applying artifact publication %s: %w", change.SessionID, err)
		}
		if result == nil {
			return 0, false, fmt.Errorf("applying artifact publication %s returned no result", change.SessionID)
		}
		rows, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return 0, false, fmt.Errorf("reading artifact publication result %s: %w", change.SessionID, rowsErr)
		}
		changed = changed || rows > 0
	}
	revision, err := artifactPublicationRevisionTx(ctx, tx, origin, changed)
	if err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("committing artifact publication changes: %w", err)
	}
	return revision, changed, nil
}

func artifactPublicationRevisionTx(
	ctx context.Context, tx *sql.Tx, origin string, increment bool,
) (int64, error) {
	var revision int64
	if increment {
		err := tx.QueryRowContext(ctx, `
			INSERT INTO artifact_publication_revisions(origin, revision) VALUES (?, 1)
			ON CONFLICT(origin) DO UPDATE SET revision = revision + 1
			RETURNING revision`, origin).Scan(&revision)
		if err != nil {
			return 0, fmt.Errorf("advancing artifact publication revision: %w", err)
		}
		return revision, nil
	}
	err := tx.QueryRowContext(ctx, `
		SELECT revision FROM artifact_publication_revisions WHERE origin = ?`, origin,
	).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading artifact publication revision: %w", err)
	}
	return revision, nil
}

// AcknowledgeArtifactExports marks successfully processed work clean while
// retaining its generation authority when no checkpoint-head update is needed.
func (db *DB) AcknowledgeArtifactExports(
	ctx context.Context, items []ArtifactExportQueueItem,
) error {
	if len(items) > maxArtifactQueuePageSize {
		return fmt.Errorf("artifact export acknowledgement exceeds %d rows", maxArtifactQueuePageSize)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning artifact export acknowledgement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockArtifactPublicationTx(ctx, tx); err != nil {
		return err
	}
	if err := acknowledgeArtifactExportsTx(ctx, tx, items); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing artifact export acknowledgement: %w", err)
	}
	return nil
}

// RecordArtifactCheckpointHead atomically records a successfully created head
// and acknowledges exactly the queue rows represented by that export batch.
func (db *DB) RecordArtifactCheckpointHead(
	ctx context.Context, head ArtifactCheckpointHead, acknowledgedItems []ArtifactExportQueueItem,
) error {
	if head.Origin == "" || head.Sequence < 1 || head.PublicationRevision < 0 ||
		head.SessionMapSHA256 == "" || head.CheckpointSHA256 == "" || head.CheckpointSize < 0 {
		return errors.New("complete artifact checkpoint head is required")
	}
	if len(acknowledgedItems) > maxArtifactQueuePageSize {
		return fmt.Errorf("artifact checkpoint acknowledgement exceeds %d rows", maxArtifactQueuePageSize)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning artifact checkpoint head: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockArtifactPublicationTx(ctx, tx); err != nil {
		return err
	}
	currentRevision, err := artifactPublicationRevisionTx(ctx, tx, head.Origin, false)
	if err != nil {
		return err
	}
	if currentRevision != head.PublicationRevision {
		return fmt.Errorf("%w: artifact publication revision %d is now %d",
			ErrArtifactExportClaimStale, head.PublicationRevision, currentRevision)
	}
	uniqueClaims, err := validateArtifactExportClaimsTx(ctx, tx, acknowledgedItems)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO artifact_checkpoint_heads (
			origin, sequence, publication_revision, session_map_sha256,
			checkpoint_sha256, checkpoint_size
		) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(origin) DO UPDATE SET
			sequence = excluded.sequence,
			publication_revision = excluded.publication_revision,
			session_map_sha256 = excluded.session_map_sha256,
			checkpoint_sha256 = excluded.checkpoint_sha256,
			checkpoint_size = excluded.checkpoint_size
		WHERE excluded.sequence > artifact_checkpoint_heads.sequence
		   OR (
			excluded.sequence = artifact_checkpoint_heads.sequence
			AND excluded.session_map_sha256 = artifact_checkpoint_heads.session_map_sha256
			AND excluded.checkpoint_sha256 = artifact_checkpoint_heads.checkpoint_sha256
			AND excluded.checkpoint_size = artifact_checkpoint_heads.checkpoint_size
		   )`,
		head.Origin, head.Sequence, head.PublicationRevision, head.SessionMapSHA256,
		head.CheckpointSHA256, head.CheckpointSize,
	)
	if err != nil {
		return fmt.Errorf("recording artifact checkpoint head: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading artifact checkpoint head result: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf(
			"artifact checkpoint head %s sequence %d conflicts with a newer or different head",
			head.Origin, head.Sequence,
		)
	}
	if err := markArtifactExportClaimsCleanTx(ctx, tx, uniqueClaims); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing artifact checkpoint head: %w", err)
	}
	return nil
}

func acknowledgeArtifactExportsTx(
	ctx context.Context, tx *sql.Tx, items []ArtifactExportQueueItem,
) error {
	unique, err := validateArtifactExportClaimsTx(ctx, tx, items)
	if err != nil {
		return err
	}
	return markArtifactExportClaimsCleanTx(ctx, tx, unique)
}

func validateArtifactExportClaimsTx(
	ctx context.Context, tx *sql.Tx, items []ArtifactExportQueueItem,
) ([]ArtifactExportQueueItem, error) {
	unique := make([]ArtifactExportQueueItem, 0, len(items))
	seen := make(map[string]int64, len(items))
	for _, item := range items {
		if item.SessionID == "" || item.Generation < 1 {
			return nil, errors.New("complete artifact export acknowledgement item is required")
		}
		if generation, ok := seen[item.SessionID]; ok {
			if generation != item.Generation {
				return nil, fmt.Errorf("%w: conflicting generations for session %s",
					ErrArtifactExportClaimStale, item.SessionID)
			}
			continue
		}
		seen[item.SessionID] = item.Generation
		unique = append(unique, item)
	}
	for _, item := range unique {
		var generation int64
		var pending bool
		err := tx.QueryRowContext(ctx, `
			SELECT generation, pending FROM artifact_export_queue WHERE session_id = ?`,
			item.SessionID,
		).Scan(&generation, &pending)
		if errors.Is(err, sql.ErrNoRows) || err == nil && (!pending || generation != item.Generation) {
			return nil, fmt.Errorf("%w: session %s generation %d",
				ErrArtifactExportClaimStale, item.SessionID, item.Generation)
		}
		if err != nil {
			return nil, fmt.Errorf("validating artifact export claim %s: %w", item.SessionID, err)
		}
	}
	return unique, nil
}

func markArtifactExportClaimsCleanTx(
	ctx context.Context, tx *sql.Tx, items []ArtifactExportQueueItem,
) error {
	stmt, err := tx.PrepareContext(ctx, `
		UPDATE artifact_export_queue SET pending = 0
		WHERE session_id = ? AND generation = ? AND pending = 1`)
	if err != nil {
		return fmt.Errorf("preparing artifact export acknowledgement: %w", err)
	}
	defer stmt.Close()
	for _, item := range items {
		result, err := stmt.ExecContext(ctx, item.SessionID, item.Generation)
		if err != nil {
			return fmt.Errorf("acknowledging artifact export %s: %w", item.SessionID, err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("reading artifact export acknowledgement %s: %w", item.SessionID, err)
		}
		if rows != 1 {
			return fmt.Errorf("%w: session %s generation %d",
				ErrArtifactExportClaimStale, item.SessionID, item.Generation)
		}
	}
	return nil
}

// lockArtifactPublicationTx obtains SQLite's writer reservation before claim
// validation, closing the check-to-mutate race with other database handles.
func lockArtifactPublicationTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE artifact_export_queue SET generation = generation WHERE 0`); err != nil {
		return fmt.Errorf("locking artifact publication transaction: %w", err)
	}
	return nil
}

// GetArtifactCheckpointHead returns the current recorded head for origin.
func (db *DB) GetArtifactCheckpointHead(
	ctx context.Context, origin string,
) (ArtifactCheckpointHead, bool, error) {
	var head ArtifactCheckpointHead
	err := db.getReader().QueryRowContext(ctx, `
		SELECT origin, sequence, publication_revision, session_map_sha256,
		       checkpoint_sha256, checkpoint_size
		FROM artifact_checkpoint_heads WHERE origin = ?`, origin).Scan(
		&head.Origin, &head.Sequence, &head.PublicationRevision,
		&head.SessionMapSHA256, &head.CheckpointSHA256, &head.CheckpointSize,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ArtifactCheckpointHead{}, false, nil
	}
	if err != nil {
		return ArtifactCheckpointHead{}, false, fmt.Errorf("reading artifact checkpoint head: %w", err)
	}
	return head, true, nil
}

// RecordArtifactPeerCheckpointHead advances one foreign origin's received
// checkpoint head. Replaying the same immutable identity is idempotent; a
// different identity at the same sequence is rejected.
func (db *DB) RecordArtifactPeerCheckpointHead(
	ctx context.Context, head ArtifactPeerCheckpointHead,
) error {
	if head.Origin == "" || head.Sequence < 1 || head.CheckpointSHA256 == "" || head.CheckpointSize < 0 {
		return errors.New("complete artifact peer checkpoint head is required")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	result, err := db.getWriter().ExecContext(ctx, `
		INSERT INTO artifact_peer_checkpoint_heads(
			origin, sequence, checkpoint_sha256, checkpoint_size
		) VALUES (?, ?, ?, ?)
		ON CONFLICT(origin) DO UPDATE SET
			sequence = excluded.sequence,
			checkpoint_sha256 = excluded.checkpoint_sha256,
			checkpoint_size = excluded.checkpoint_size
		WHERE excluded.sequence > artifact_peer_checkpoint_heads.sequence
		   OR (excluded.sequence = artifact_peer_checkpoint_heads.sequence
		       AND excluded.checkpoint_sha256 = artifact_peer_checkpoint_heads.checkpoint_sha256
		       AND excluded.checkpoint_size = artifact_peer_checkpoint_heads.checkpoint_size)`,
		head.Origin, head.Sequence, head.CheckpointSHA256, head.CheckpointSize,
	)
	if err != nil {
		return fmt.Errorf("recording artifact peer checkpoint head: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading artifact peer checkpoint head result: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("artifact peer checkpoint head %s sequence %d conflicts with a newer or different head",
			head.Origin, head.Sequence)
	}
	return nil
}

// GetArtifactPeerCheckpointHead returns the exact highest received checkpoint
// identity for one foreign origin.
func (db *DB) GetArtifactPeerCheckpointHead(
	ctx context.Context, origin string,
) (ArtifactPeerCheckpointHead, bool, error) {
	var head ArtifactPeerCheckpointHead
	err := db.getReader().QueryRowContext(ctx, `
		SELECT origin, sequence, checkpoint_sha256, checkpoint_size
		FROM artifact_peer_checkpoint_heads WHERE origin = ?`, origin).Scan(
		&head.Origin, &head.Sequence, &head.CheckpointSHA256, &head.CheckpointSize,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ArtifactPeerCheckpointHead{}, false, nil
	}
	if err != nil {
		return ArtifactPeerCheckpointHead{}, false,
			fmt.Errorf("reading artifact peer checkpoint head: %w", err)
	}
	return head, true, nil
}

// GetArtifactCheckpointLandingHead returns one foreign origin's landed checkpoint
// sequence without materializing its session map.
func (db *DB) GetArtifactCheckpointLandingHead(
	ctx context.Context, origin string,
) (ArtifactCheckpointLanding, bool, error) {
	var landing ArtifactCheckpointLanding
	err := db.getReader().QueryRowContext(ctx, `
		SELECT origin, sequence FROM artifact_checkpoint_landings WHERE origin = ?`, origin,
	).Scan(&landing.Origin, &landing.Sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return ArtifactCheckpointLanding{}, false, nil
	}
	if err != nil {
		return ArtifactCheckpointLanding{}, false,
			fmt.Errorf("reading artifact checkpoint landing: %w", err)
	}
	return landing, true, nil
}

// RecordArtifactCheckpointLanding atomically replaces one origin's exact
// landed checkpoint map. Older observations cannot regress durable provenance.
func (db *DB) RecordArtifactCheckpointLanding(
	ctx context.Context,
	landing ArtifactCheckpointLanding,
	manifestByGID map[string]string,
) error {
	if landing.Origin == "" || landing.Sequence < 1 {
		return errors.New("complete artifact checkpoint landing is required")
	}
	for gid, manifestHash := range manifestByGID {
		if gid == "" || manifestHash == "" {
			return errors.New("complete artifact checkpoint landing session is required")
		}
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning artifact checkpoint landing: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var current int
	err = tx.QueryRowContext(ctx, `
		SELECT sequence FROM artifact_checkpoint_landings WHERE origin = ?`,
		landing.Origin,
	).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading artifact checkpoint landing: %w", err)
	}
	if err == nil && landing.Sequence < current {
		return fmt.Errorf("artifact checkpoint landing %s sequence %d is older than %d",
			landing.Origin, landing.Sequence, current)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO artifact_checkpoint_landings(origin, sequence) VALUES (?, ?)
		ON CONFLICT(origin) DO UPDATE SET sequence = excluded.sequence`,
		landing.Origin, landing.Sequence,
	); err != nil {
		return fmt.Errorf("recording artifact checkpoint landing: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM artifact_checkpoint_landing_sessions WHERE origin = ?`,
		landing.Origin,
	); err != nil {
		return fmt.Errorf("replacing artifact checkpoint landing sessions: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO artifact_checkpoint_landing_sessions(origin, gid, manifest_hash)
		VALUES (?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("preparing artifact checkpoint landing sessions: %w", err)
	}
	defer stmt.Close()
	for gid, manifestHash := range manifestByGID {
		if _, err := stmt.ExecContext(ctx, landing.Origin, gid, manifestHash); err != nil {
			return fmt.Errorf("recording artifact checkpoint landing session %s: %w", gid, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing artifact checkpoint landing: %w", err)
	}
	return nil
}

// GetArtifactCheckpointLanding returns one origin's landed sequence and exact
// GID-to-manifest map from a single read snapshot.
func (db *DB) GetArtifactCheckpointLanding(
	ctx context.Context, origin string,
) (ArtifactCheckpointLanding, map[string]string, bool, error) {
	if origin == "" {
		return ArtifactCheckpointLanding{}, nil, false,
			errors.New("artifact checkpoint landing origin is required")
	}
	tx, err := db.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ArtifactCheckpointLanding{}, nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	landing := ArtifactCheckpointLanding{Origin: origin}
	err = tx.QueryRowContext(ctx, `
		SELECT sequence FROM artifact_checkpoint_landings WHERE origin = ?`, origin,
	).Scan(&landing.Sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return ArtifactCheckpointLanding{}, map[string]string{}, false, nil
	}
	if err != nil {
		return ArtifactCheckpointLanding{}, nil, false,
			fmt.Errorf("reading artifact checkpoint landing: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT gid, manifest_hash FROM artifact_checkpoint_landing_sessions
		WHERE origin = ? ORDER BY gid`, origin)
	if err != nil {
		return ArtifactCheckpointLanding{}, nil, false,
			fmt.Errorf("reading artifact checkpoint landing sessions: %w", err)
	}
	defer rows.Close()
	manifestByGID := make(map[string]string)
	for rows.Next() {
		var gid, manifestHash string
		if err := rows.Scan(&gid, &manifestHash); err != nil {
			return ArtifactCheckpointLanding{}, nil, false,
				fmt.Errorf("scanning artifact checkpoint landing session: %w", err)
		}
		manifestByGID[gid] = manifestHash
	}
	if err := rows.Err(); err != nil {
		return ArtifactCheckpointLanding{}, nil, false,
			fmt.Errorf("iterating artifact checkpoint landing sessions: %w", err)
	}
	return landing, manifestByGID, true, nil
}

// StreamArtifactCheckpointLanding visits one origin's exact landing map from
// the same read snapshot as its checkpoint sequence.
func (db *DB) StreamArtifactCheckpointLanding(
	ctx context.Context,
	origin string,
	visit func(gid, manifestHash string) error,
) (ArtifactCheckpointLanding, bool, error) {
	if origin == "" || visit == nil {
		return ArtifactCheckpointLanding{}, false,
			errors.New("artifact checkpoint landing origin and visitor are required")
	}
	tx, err := db.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ArtifactCheckpointLanding{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	landing := ArtifactCheckpointLanding{Origin: origin}
	err = tx.QueryRowContext(ctx, `
		SELECT sequence FROM artifact_checkpoint_landings WHERE origin = ?`, origin,
	).Scan(&landing.Sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return ArtifactCheckpointLanding{}, false, nil
	}
	if err != nil {
		return ArtifactCheckpointLanding{}, false,
			fmt.Errorf("reading artifact checkpoint landing: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT gid, manifest_hash FROM artifact_checkpoint_landing_sessions
		WHERE origin = ? ORDER BY gid`, origin)
	if err != nil {
		return ArtifactCheckpointLanding{}, false,
			fmt.Errorf("streaming artifact checkpoint landing sessions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var gid, manifestHash string
		if err := rows.Scan(&gid, &manifestHash); err != nil {
			return ArtifactCheckpointLanding{}, false,
				fmt.Errorf("scanning artifact checkpoint landing session: %w", err)
		}
		if err := visit(gid, manifestHash); err != nil {
			return ArtifactCheckpointLanding{}, false, err
		}
	}
	if err := rows.Err(); err != nil {
		return ArtifactCheckpointLanding{}, false,
			fmt.Errorf("iterating artifact checkpoint landing sessions: %w", err)
	}
	return landing, true, nil
}

// StreamArtifactPublications visits publication rows in canonical session-id
// order without materializing the full checkpoint map. The returned revision
// and every visited row come from the same SQLite read snapshot.
func (db *DB) StreamArtifactPublications(
	ctx context.Context, origin string, visit func(ArtifactPublication) error,
) (int64, error) {
	if visit == nil {
		return 0, errors.New("artifact publication visitor is required")
	}
	db.connMu.RLock()
	reader := db.reader.Load()
	if reader == nil {
		db.connMu.RUnlock()
		return 0, errors.New("database is closed")
	}
	tx, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	db.connMu.RUnlock()
	if err != nil {
		return 0, fmt.Errorf("beginning artifact publication snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	revision, err := artifactPublicationRevisionTx(ctx, tx, origin, false)
	if err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT origin, session_id, manifest_hash, source_fingerprint
		FROM artifact_publications
		WHERE origin = ?
		ORDER BY session_id`, origin)
	if err != nil {
		return 0, fmt.Errorf("streaming artifact publications: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var publication ArtifactPublication
		if err := rows.Scan(
			&publication.Origin, &publication.SessionID,
			&publication.ManifestHash, &publication.SourceFingerprint,
		); err != nil {
			return 0, fmt.Errorf("scanning artifact publication: %w", err)
		}
		if err := visit(publication); err != nil {
			return 0, err
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterating artifact publications: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("closing artifact publications: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing artifact publication snapshot: %w", err)
	}
	return revision, nil
}

// ReserveArtifactCheckpointSequence commits the next sequence in an immediate
// transaction before returning. observedFloor is a stable vault traversal's
// maximum sequence and can only raise, never lower, the retained authority.
func (db *DB) ReserveArtifactCheckpointSequence(
	ctx context.Context, origin string, observedFloor int,
) (_ int, retErr error) {
	if origin == "" {
		return 0, errors.New("artifact checkpoint origin is required")
	}
	if observedFloor < 0 {
		return 0, errors.New("artifact checkpoint observed floor must not be negative")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	conn, err := db.getWriter().Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("acquiring artifact checkpoint connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return 0, fmt.Errorf("beginning artifact checkpoint reservation: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, rollbackErr := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
			retErr = errors.Join(retErr, rollbackErr)
		}
	}()
	var sequence int
	err = conn.QueryRowContext(ctx, `
		INSERT INTO artifact_checkpoint_floors(origin, sequence)
		VALUES (?, ? + 1)
		ON CONFLICT(origin) DO UPDATE SET
			sequence = max(artifact_checkpoint_floors.sequence, ?)+1
		RETURNING sequence`, origin, observedFloor, observedFloor).Scan(&sequence)
	if err != nil {
		return 0, fmt.Errorf("reserving artifact checkpoint sequence: %w", err)
	}
	if _, err := conn.ExecContext(context.WithoutCancel(ctx), "COMMIT"); err != nil {
		return 0, fmt.Errorf("committing artifact checkpoint reservation: %w", err)
	}
	committed = true
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return sequence, nil
}

// GetArtifactCheckpointFloor reports the durable sequence authority, if this
// origin has already been bootstrapped.
func (db *DB) GetArtifactCheckpointFloor(
	ctx context.Context, origin string,
) (int, bool, error) {
	if origin == "" {
		return 0, false, errors.New("artifact checkpoint origin is required")
	}
	var sequence int
	err := db.getReader().QueryRowContext(ctx, `
		SELECT sequence FROM artifact_checkpoint_floors WHERE origin = ?`, origin,
	).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("reading artifact checkpoint floor: %w", err)
	}
	return sequence, true, nil
}

// EnqueueArtifactRepair records corrupt physical content for trusted-peer
// repair. A repeated detection refreshes the expected identity and timestamp.
func (db *DB) EnqueueArtifactRepair(ctx context.Context, repair ArtifactRepair) error {
	if repair.Origin == "" || repair.Kind == "" || repair.Name == "" || repair.SHA256 == "" || repair.Size < 0 {
		return errors.New("complete artifact repair identity is required")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, err := db.getWriter().ExecContext(ctx, `
		INSERT INTO artifact_repair_queue(
			origin, kind, name, sha256, size, detected_at
		) VALUES (?, ?, ?, ?, ?, COALESCE(
			NULLIF(?, ''), strftime('%Y-%m-%dT%H:%M:%fZ','now')
		))
		ON CONFLICT(origin, kind, name) DO UPDATE SET
			sha256 = excluded.sha256,
			size = excluded.size,
			detected_at = excluded.detected_at`,
		repair.Origin, repair.Kind, repair.Name, repair.SHA256, repair.Size,
		repair.DetectedAt,
	); err != nil {
		return fmt.Errorf("enqueueing artifact repair: %w", err)
	}
	return nil
}

// PendingArtifactRepairs returns the oldest bounded repair page.
func (db *DB) PendingArtifactRepairs(ctx context.Context, limit int) ([]ArtifactRepair, error) {
	if err := validateArtifactQueueLimit(limit); err != nil {
		return nil, err
	}
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT origin, kind, name, sha256, size, detected_at
		FROM artifact_repair_queue
		ORDER BY detected_at, origin, kind, name
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("reading artifact repair queue: %w", err)
	}
	defer rows.Close()
	items := make([]ArtifactRepair, 0, min(limit, 64))
	for rows.Next() {
		var repair ArtifactRepair
		if err := rows.Scan(
			&repair.Origin, &repair.Kind, &repair.Name, &repair.SHA256,
			&repair.Size, &repair.DetectedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning artifact repair queue: %w", err)
		}
		items = append(items, repair)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating artifact repair queue: %w", err)
	}
	return items, nil
}

// ArtifactRepairForRef returns the exact queued repair for one logical
// reference without materializing or scanning the queue.
func (db *DB) ArtifactRepairForRef(
	ctx context.Context, origin, kind, name string,
) (ArtifactRepair, bool, error) {
	if origin == "" || kind == "" || name == "" {
		return ArtifactRepair{}, false, errors.New("complete artifact repair reference is required")
	}
	var repair ArtifactRepair
	err := db.getReader().QueryRowContext(ctx, `
		SELECT origin, kind, name, sha256, size, detected_at
		FROM artifact_repair_queue
		WHERE origin = ? AND kind = ? AND name = ?`,
		origin, kind, name,
	).Scan(
		&repair.Origin, &repair.Kind, &repair.Name, &repair.SHA256,
		&repair.Size, &repair.DetectedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ArtifactRepair{}, false, nil
	}
	if err != nil {
		return ArtifactRepair{}, false, fmt.Errorf("reading artifact repair: %w", err)
	}
	return repair, true, nil
}

// AcknowledgeArtifactRepair removes only the exact expected identity that was
// repaired. Re-detection of an identical identity may refresh DetectedAt and
// remains the same claim; a different hash or size must be retried.
func (db *DB) AcknowledgeArtifactRepair(
	ctx context.Context, repair ArtifactRepair,
) error {
	if repair.Origin == "" || repair.Kind == "" || repair.Name == "" || repair.SHA256 == "" || repair.Size < 0 {
		return errors.New("complete artifact repair identity is required")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	result, err := db.getWriter().ExecContext(ctx, `
		DELETE FROM artifact_repair_queue
		WHERE origin = ? AND kind = ? AND name = ? AND sha256 = ? AND size = ?`,
		repair.Origin, repair.Kind, repair.Name, repair.SHA256, repair.Size)
	if err != nil {
		return fmt.Errorf("acknowledging artifact repair: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading artifact repair acknowledgement: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("%w: %s/%s/%s", ErrArtifactRepairClaimStale,
			repair.Origin, repair.Kind, repair.Name)
	}
	return nil
}

func validateArtifactQueueLimit(limit int) error {
	if limit < 1 || limit > maxArtifactQueuePageSize {
		return fmt.Errorf("artifact queue limit must be between 1 and %d", maxArtifactQueuePageSize)
	}
	return nil
}

// enqueueArtifactExportTx advances one locally-owned session exactly once for
// a production transaction whose child-row mutation does not also change an
// export-relevant sessions column.
func enqueueArtifactExportTx(tx *sql.Tx, sessionID string) error {
	_, err := tx.Exec(`
		INSERT INTO artifact_export_queue(session_id)
		SELECT id FROM sessions WHERE id = ? AND machine = 'local'
		ON CONFLICT(session_id) DO UPDATE SET
			enqueued_at = CASE WHEN pending = 0
				THEN strftime('%Y-%m-%dT%H:%M:%fZ','now') ELSE enqueued_at END,
			generation = generation + 1,
			pending = 1`, sessionID)
	if err != nil {
		return fmt.Errorf("enqueueing artifact export for %s: %w", sessionID, err)
	}
	return nil
}

func artifactExportGenerationTx(
	tx *sql.Tx, sessionID string,
) (int64, bool, error) {
	var generation int64
	err := tx.QueryRow(`
		SELECT generation FROM artifact_export_queue WHERE session_id = ?`, sessionID,
	).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("reading artifact export generation for %s: %w", sessionID, err)
	}
	return generation, true, nil
}
