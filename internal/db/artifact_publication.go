package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const maxArtifactQueuePageSize = 1024

const artifactLocalMachineStateKey = "artifact_local_machine_name"

// ErrArtifactExportClaimStale tells callers to discard computed export
// output and retry from a fresh queue claim.
var ErrArtifactExportClaimStale = errors.New("artifact export claim is stale")

// ErrArtifactOriginMismatch tells callers that the exporter is not operating
// under the database's currently configured artifact namespace.
var ErrArtifactOriginMismatch = errors.New("artifact origin does not match persisted origin")

// beforeApplyArtifactPublicationLock is a test synchronization seam for
// exercising origin adoption races at the writer-reservation boundary.
var beforeApplyArtifactPublicationLock = func() {}

// ArtifactExportQueueItem identifies one locally-owned session whose artifact
// publication may no longer match the archive.
type ArtifactExportQueueItem struct {
	SessionID  string
	EnqueuedAt string
	Generation int64
}

// ArtifactExportOutcome is the terminal result for one exact queue claim.
// An empty Rejection marks successful work clean; a non-empty Rejection records
// a deterministic failure for diagnostics.
type ArtifactExportOutcome struct {
	Item      ArtifactExportQueueItem
	Rejection string
}

// ArtifactExportRejection describes the most recent deterministically rejected
// generation for one session.
type ArtifactExportRejection struct {
	SessionID  string
	Generation int64
	Error      string
	RejectedAt string
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

// CountPendingArtifactExports snapshots the current dirty queue cardinality.
// Full exports use the count as a finite drain boundary so work arriving after
// the snapshot is left for a later convergence round.
func (db *DB) CountPendingArtifactExports(ctx context.Context) (int, error) {
	var count int
	if err := db.getReader().QueryRowContext(ctx, `
		SELECT count(*) FROM artifact_export_queue
		WHERE pending = 1`).Scan(&count); err != nil {
		return 0, fmt.Errorf("counting artifact export queue: %w", err)
	}
	return count, nil
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
	beforeApplyArtifactPublicationLock()
	if err := lockArtifactPublicationTx(ctx, tx); err != nil {
		return 0, false, err
	}
	if err := validateArtifactOriginTx(ctx, tx, origin); err != nil {
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

func validateArtifactOriginTx(
	ctx context.Context, tx *sql.Tx, origin string,
) error {
	var persisted string
	err := tx.QueryRowContext(ctx, `
		SELECT value FROM pg_sync_state WHERE key = 'artifact_origin_id'`,
	).Scan(&persisted)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && persisted == "") {
		return fmt.Errorf("%w: no persisted artifact origin", ErrArtifactOriginMismatch)
	}
	if err != nil {
		return fmt.Errorf("reading persisted artifact origin: %w", err)
	}
	if persisted != origin {
		return fmt.Errorf(
			"%w: requested %s, persisted %s",
			ErrArtifactOriginMismatch, origin, persisted,
		)
	}
	return nil
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
	return db.FinalizeArtifactExports(ctx, successfulArtifactExportOutcomes(items))
}

// FinalizeArtifactExports records terminal outcomes while retaining generation
// authority when no checkpoint-head update is needed.
func (db *DB) FinalizeArtifactExports(
	ctx context.Context, outcomes []ArtifactExportOutcome,
) error {
	if len(outcomes) > maxArtifactQueuePageSize {
		return fmt.Errorf("artifact export finalization exceeds %d rows", maxArtifactQueuePageSize)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning artifact export finalization: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockArtifactPublicationTx(ctx, tx); err != nil {
		return err
	}
	if err := finalizeArtifactExportOutcomesTx(ctx, tx, outcomes); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing artifact export finalization: %w", err)
	}
	return nil
}

// RecordArtifactCheckpointHead atomically records a successfully created head
// and acknowledges exactly the queue rows represented by that export batch.
func (db *DB) RecordArtifactCheckpointHead(
	ctx context.Context, head ArtifactCheckpointHead, acknowledgedItems []ArtifactExportQueueItem,
) error {
	return db.RecordArtifactCheckpointHeadOutcomes(
		ctx, head, successfulArtifactExportOutcomes(acknowledgedItems),
	)
}

// RecordArtifactCheckpointHeadOutcomes atomically records a successfully
// created head and finalizes exactly the queue claims represented by the
// export batch.
func (db *DB) RecordArtifactCheckpointHeadOutcomes(
	ctx context.Context, head ArtifactCheckpointHead, outcomes []ArtifactExportOutcome,
) error {
	if head.Origin == "" || head.Sequence < 1 || head.PublicationRevision < 0 ||
		head.SessionMapSHA256 == "" || head.CheckpointSHA256 == "" || head.CheckpointSize < 0 {
		return errors.New("complete artifact checkpoint head is required")
	}
	if len(outcomes) > maxArtifactQueuePageSize {
		return fmt.Errorf("artifact checkpoint finalization exceeds %d rows", maxArtifactQueuePageSize)
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
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO artifact_checkpoint_floors(origin, sequence)
		VALUES (?, ?)
		ON CONFLICT(origin) DO UPDATE SET
			sequence = max(artifact_checkpoint_floors.sequence, excluded.sequence)`,
		head.Origin, head.Sequence,
	); err != nil {
		return fmt.Errorf("advancing artifact checkpoint floor from head: %w", err)
	}
	if err := finalizeArtifactExportOutcomesTx(ctx, tx, outcomes); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing artifact checkpoint head: %w", err)
	}
	return nil
}

func successfulArtifactExportOutcomes(
	items []ArtifactExportQueueItem,
) []ArtifactExportOutcome {
	outcomes := make([]ArtifactExportOutcome, len(items))
	for i, item := range items {
		outcomes[i] = ArtifactExportOutcome{Item: item}
	}
	return outcomes
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

func finalizeArtifactExportOutcomesTx(
	ctx context.Context, tx *sql.Tx, outcomes []ArtifactExportOutcome,
) error {
	items := make([]ArtifactExportQueueItem, len(outcomes))
	for i, outcome := range outcomes {
		items[i] = outcome.Item
	}
	unique, err := validateArtifactExportClaimsTx(ctx, tx, items)
	if err != nil {
		return err
	}
	bySession := make(map[string]string, len(outcomes))
	for _, outcome := range outcomes {
		if previous, ok := bySession[outcome.Item.SessionID]; ok &&
			previous != outcome.Rejection {
			return fmt.Errorf(
				"conflicting artifact export outcomes for session %s",
				outcome.Item.SessionID,
			)
		}
		bySession[outcome.Item.SessionID] = outcome.Rejection
	}
	for _, item := range unique {
		rejection := bySession[item.SessionID]
		var rejectedGeneration any
		var rejectedAt any
		if rejection != "" {
			rejectedGeneration = item.Generation
			rejectedAt = time.Now().UTC().Format(time.RFC3339Nano)
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE artifact_export_queue SET
				pending = 0,
				rejected_generation = ?,
				last_error = ?,
				rejected_at = ?
			WHERE session_id = ? AND generation = ? AND pending = 1`,
			rejectedGeneration, rejection, rejectedAt,
			item.SessionID, item.Generation,
		)
		if err != nil {
			return fmt.Errorf("finalizing artifact export %s: %w", item.SessionID, err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("reading artifact export finalization %s: %w", item.SessionID, err)
		}
		if rows != 1 {
			return fmt.Errorf("%w: session %s generation %d",
				ErrArtifactExportClaimStale, item.SessionID, item.Generation)
		}
	}
	return nil
}

// GetArtifactExportRejection returns the deterministic rejection diagnostics
// for a session's current clean generation, if present.
func (db *DB) GetArtifactExportRejection(
	ctx context.Context, sessionID string,
) (ArtifactExportRejection, bool, error) {
	var rejection ArtifactExportRejection
	err := db.getReader().QueryRowContext(ctx, `
		SELECT session_id, rejected_generation, last_error, rejected_at
		FROM artifact_export_queue
		WHERE session_id = ?
		  AND rejected_generation IS NOT NULL
		  AND last_error <> ''
		  AND rejected_at IS NOT NULL`, sessionID,
	).Scan(
		&rejection.SessionID, &rejection.Generation,
		&rejection.Error, &rejection.RejectedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ArtifactExportRejection{}, false, nil
	}
	if err != nil {
		return ArtifactExportRejection{}, false,
			fmt.Errorf("reading artifact export rejection: %w", err)
	}
	return rejection, true, nil
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

// ArtifactPublicationPage returns a bounded canonical page after sessionID.
// The revision and rows come from the same SQLite read snapshot, allowing a
// caller to reject pages that no longer match its checkpoint head.
func (db *DB) ArtifactPublicationPage(
	ctx context.Context,
	origin string,
	afterSessionID string,
	limit int,
) ([]ArtifactPublication, int64, bool, error) {
	if limit <= 0 || limit > 512 {
		return nil, 0, false, errors.New(
			"artifact publication page limit must be between 1 and 512",
		)
	}
	db.connMu.RLock()
	reader := db.reader.Load()
	if reader == nil {
		db.connMu.RUnlock()
		return nil, 0, false, errors.New("database is closed")
	}
	tx, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	db.connMu.RUnlock()
	if err != nil {
		return nil, 0, false, fmt.Errorf(
			"beginning artifact publication page snapshot: %w",
			err,
		)
	}
	defer func() { _ = tx.Rollback() }()
	revision, err := artifactPublicationRevisionTx(ctx, tx, origin, false)
	if err != nil {
		return nil, 0, false, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT origin, session_id, manifest_hash, source_fingerprint
		FROM artifact_publications
		WHERE origin = ? AND session_id > ?
		ORDER BY session_id
		LIMIT ?`, origin, afterSessionID, limit+1)
	if err != nil {
		return nil, 0, false, fmt.Errorf("paging artifact publications: %w", err)
	}
	defer rows.Close()
	publications := make([]ArtifactPublication, 0, limit+1)
	for rows.Next() {
		var publication ArtifactPublication
		if err := rows.Scan(
			&publication.Origin,
			&publication.SessionID,
			&publication.ManifestHash,
			&publication.SourceFingerprint,
		); err != nil {
			return nil, 0, false, fmt.Errorf(
				"scanning artifact publication page: %w",
				err,
			)
		}
		publications = append(publications, publication)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, fmt.Errorf(
			"iterating artifact publication page: %w",
			err,
		)
	}
	if err := rows.Close(); err != nil {
		return nil, 0, false, fmt.Errorf(
			"closing artifact publication page: %w",
			err,
		)
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, false, fmt.Errorf(
			"committing artifact publication page snapshot: %w",
			err,
		)
	}
	more := len(publications) > limit
	if more {
		publications = publications[:limit]
	}
	return publications, revision, more, nil
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

func validateArtifactQueueLimit(limit int) error {
	if limit < 1 || limit > maxArtifactQueuePageSize {
		return fmt.Errorf("artifact queue limit must be between 1 and %d", maxArtifactQueuePageSize)
	}
	return nil
}

// enqueueArtifactExportTx advances one locally-owned session exactly once for
// a production transaction whose child-row mutation does not also change an
// export-relevant sessions column. The write is gated on the presence of an
// artifact origin (pg_sync_state key artifact_origin_id) so that archives
// which have never created or adopted an artifact origin never populate the
// export queue.
func enqueueArtifactExportTx(tx transactionQueries, sessionID string) error {
	_, err := tx.Exec(`
		INSERT INTO artifact_export_queue(session_id)
		SELECT id FROM sessions WHERE id = ? AND (
			machine = 'local' OR machine = (
				SELECT value FROM pg_sync_state
				WHERE key = 'artifact_local_machine_name'
			)
		)
			AND EXISTS (SELECT 1 FROM pg_sync_state
				WHERE key = 'artifact_origin_id')
		ON CONFLICT(session_id) DO UPDATE SET
			enqueued_at = CASE WHEN pending = 0
				THEN strftime('%Y-%m-%dT%H:%M:%fZ','now') ELSE enqueued_at END,
			generation = generation + 1,
			pending = 1,
			rejected_generation = NULL,
			last_error = '',
			rejected_at = NULL`, sessionID)
	if err != nil {
		return fmt.Errorf("enqueueing artifact export for %s: %w", sessionID, err)
	}
	return nil
}

// ArtifactLocalMachineName returns the configured machine identity used to
// distinguish locally ingested sessions from imported peer sessions. "local"
// remains the fallback for archives created before the identity was persisted.
func (db *DB) ArtifactLocalMachineName(ctx context.Context) (string, error) {
	var machine string
	err := db.getReader().QueryRowContext(ctx, `
		SELECT value FROM pg_sync_state WHERE key = ?`,
		artifactLocalMachineStateKey,
	).Scan(&machine)
	if errors.Is(err, sql.ErrNoRows) {
		return "local", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading artifact local machine name: %w", err)
	}
	if strings.TrimSpace(machine) == "" {
		return "local", nil
	}
	return machine, nil
}

// ConfigureArtifactLocalMachine atomically persists the runtime local-machine
// identity and re-dirties active-origin publications when that identity
// changes. This publishes newly owned sessions and removes publications that
// belonged to the previous machine identity.
func (db *DB) ConfigureArtifactLocalMachine(machine string) error {
	if strings.TrimSpace(machine) == "" {
		return errors.New("artifact local machine name is required")
	}
	return db.Update(func(tx *sql.Tx) error {
		if err := lockArtifactPublicationTx(context.Background(), tx); err != nil {
			return err
		}
		var existing string
		err := tx.QueryRow(`
			SELECT value FROM pg_sync_state WHERE key = ?`,
			artifactLocalMachineStateKey,
		).Scan(&existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("reading artifact local machine name: %w", err)
		}
		if err == nil && existing == machine {
			return nil
		}
		if _, err := tx.Exec(`
			INSERT INTO pg_sync_state(key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			artifactLocalMachineStateKey, machine,
		); err != nil {
			return fmt.Errorf("persisting artifact local machine name: %w", err)
		}
		var origin string
		err = tx.QueryRow(`
			SELECT value FROM pg_sync_state WHERE key = 'artifact_origin_id'`,
		).Scan(&origin)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading artifact origin: %w", err)
		}
		if origin == "" {
			return nil
		}
		return populateArtifactOriginQueueTx(tx, origin, true)
	})
}

func artifactExportGenerationTx(
	tx transactionQueries, sessionID string,
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

func enqueueArtifactExportIfGenerationUnchangedTx(
	tx transactionQueries,
	sessionID string,
	generationBefore int64,
	existedBefore bool,
) error {
	generationAfter, existsAfter, err := artifactExportGenerationTx(tx, sessionID)
	if err != nil {
		return err
	}
	if existedBefore == existsAfter && generationBefore == generationAfter {
		return enqueueArtifactExportTx(tx, sessionID)
	}
	return nil
}
