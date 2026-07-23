package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const maxArtifactQueuePageSize = 1024

// ErrArtifactExportClaimStale tells callers to discard computed export
// output and retry from a fresh queue claim.
var ErrArtifactExportClaimStale = errors.New("artifact export claim is stale")

// ArtifactExportQueueItem identifies one locally-owned session whose artifact
// publication may no longer match the archive.
type ArtifactExportQueueItem struct {
	SessionID  string
	EnqueuedAt string
	Generation int64
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
func enqueueArtifactExportTx(tx *sql.Tx, sessionID string) error {
	_, err := tx.Exec(`
		INSERT INTO artifact_export_queue(session_id)
		SELECT id FROM sessions WHERE id = ? AND machine = 'local'
			AND EXISTS (SELECT 1 FROM pg_sync_state
				WHERE key = 'artifact_origin_id')
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
