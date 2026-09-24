package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/mattn/go-sqlite3"

	"go.kenn.io/agentsview/internal/ledger"
)

// ledgerEventsNoDeleteTriggerDDL must stay byte-identical to the trigger in
// schema.sql; RebuildLedgerIndex drops and recreates it.
const ledgerEventsNoDeleteTriggerDDL = `CREATE TRIGGER IF NOT EXISTS trg_ledger_events_no_delete
BEFORE DELETE ON ledger_events
BEGIN
    SELECT RAISE(ABORT, 'ledger is append-only');
END;`

// LedgerImportState is the last `ledger import` / ledger-import job
// result for one (zone, path).
type LedgerImportState struct {
	Zone           string
	Path           string
	LastReportJSON string
	UpdatedAt      string
}

func (db *DB) hasLedgerTables(ctx context.Context) bool {
	var n int
	err := db.getReader().QueryRow(ctx,
		"SELECT 1 FROM sqlite_master WHERE type='table' AND name='ledger_segments'",
	).Scan(&n)
	return err == nil && n == 1
}

// mapLedgerWriteError turns the append-only triggers' RAISE(ABORT) into
// ledger.ErrAppendOnly. Only the ledger tables carry triggers that abort
// UPDATE or DELETE, so a trigger constraint error from a ledger write is
// always the append-only guard.
func mapLedgerWriteError(action string, err error) error {
	if err == nil {
		return nil
	}
	if sqliteErr, ok := errors.AsType[sqlite3.Error](err); ok &&
		sqliteErr.ExtendedCode == sqlite3.ErrConstraintTrigger {
		return fmt.Errorf("%s: %w", action, ledger.ErrAppendOnly)
	}
	return fmt.Errorf("%s: %w", action, err)
}

// AppendLedgerSegment stores a verified segment and projects its events
// in one transaction. An identical copy of an existing identity is a
// no-op (AlreadyIdentical); different content is a *ledger.ConflictError
// and the stored row is untouched. Events whose event_id is already
// projected are skipped (INSERT OR IGNORE, as jilog db.rs:184).
func (db *DB) AppendLedgerSegment(
	ctx context.Context, zone string, seg ledger.Segment, origin string,
) (ledger.PublishOutcome, error) {
	prep, err := ledger.PrepareAppend(zone, seg, origin)
	if err != nil {
		return ledger.Published, err
	}
	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin(ctx)
	if err != nil {
		return ledger.Published, fmt.Errorf("beginning ledger append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO ledger_segments (
			zone, source, source_seq, checksum, created_at,
			event_count, events_json, origin, ingested_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (zone, source, source_seq) DO NOTHING`,
		prep.Zone, prep.Source, prep.Seq, prep.Checksum, prep.CreatedAt,
		len(prep.Events), prep.EventsJSON, prep.Origin,
		ledger.StorageTimestamp(time.Now()),
	)
	if err != nil {
		return ledger.Published, mapLedgerWriteError("inserting ledger segment", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var checksum int64
		var createdAt, eventsJSON string
		if err := tx.QueryRowContext(ctx, `
			SELECT checksum, created_at, events_json FROM ledger_segments
			WHERE zone = ? AND source = ? AND source_seq = ?`,
			prep.Zone, prep.Source, prep.Seq,
		).Scan(&checksum, &createdAt, &eventsJSON); err != nil {
			return ledger.Published, fmt.Errorf("reading existing ledger segment: %w", err)
		}
		if prep.SameContent(checksum, createdAt, eventsJSON) {
			return ledger.AlreadyIdentical, nil
		}
		return ledger.Published, prep.Conflict()
	}
	for _, e := range prep.Events {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ledger_events (
				event_id, zone, source, source_seq, timestamp,
				correlation_id, causation_id, actor_ref, object_ref,
				event_class, payload_tier, payload, subsystem, summary,
				segment_source, segment_seq, event_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (event_id) DO NOTHING`,
			e.EventID, prep.Zone, e.Source, e.SourceSeq,
			ledger.StorageTimestamp(e.Timestamp),
			e.CorrelationID, e.CausationID, e.ActorRef, e.ObjectRef,
			e.EventClass, e.PayloadTier, e.Payload, e.Subsystem, e.Summary,
			prep.Source, prep.Seq, e.EventJSON,
		); err != nil {
			return ledger.Published, mapLedgerWriteError("inserting ledger event", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return ledger.Published, fmt.Errorf("committing ledger append: %w", err)
	}
	return ledger.Published, nil
}

// LatestLedgerSeq returns the highest stored seq for (zone, source), or 0.
func (db *DB) LatestLedgerSeq(ctx context.Context, zone, source string) (uint64, error) {
	if !db.hasLedgerTables(ctx) {
		return 0, nil
	}
	var seq int64
	if err := db.getReader().QueryRow(ctx, `
		SELECT COALESCE(MAX(source_seq), 0) FROM ledger_segments
		WHERE zone = ? AND source = ?`, zone, source,
	).Scan(&seq); err != nil {
		return 0, fmt.Errorf("reading latest ledger seq: %w", err)
	}
	return uint64(seq), nil
}

// ListLedgerSegments returns (zone, source) segments with seq > afterSeq in
// seq order, at most limit of them (limit <= 0 means all).
func (db *DB) ListLedgerSegments(
	ctx context.Context, zone, source string, afterSeq uint64, limit int,
) ([]ledger.Segment, error) {
	if !db.hasLedgerTables(ctx) {
		return []ledger.Segment{}, nil
	}
	if afterSeq > uint64(maxLedgerSeq) {
		return []ledger.Segment{}, nil
	}
	if limit <= 0 {
		limit = -1
	}
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT source, source_seq, checksum, created_at, events_json
		FROM ledger_segments
		WHERE zone = ? AND source = ? AND source_seq > ?
		ORDER BY source_seq
		LIMIT ?`, zone, source, int64(afterSeq), limit)
	if err != nil {
		return nil, fmt.Errorf("listing ledger segments: %w", err)
	}
	defer rows.Close()
	out := []ledger.Segment{}
	for rows.Next() {
		var src, createdAt, eventsJSON string
		var seq, checksum int64
		if err := rows.Scan(&src, &seq, &checksum, &createdAt, &eventsJSON); err != nil {
			return nil, fmt.Errorf("scanning ledger segment: %w", err)
		}
		seg, err := ledgerSegmentFromRow(src, seq, checksum, createdAt, eventsJSON)
		if err != nil {
			return nil, err
		}
		out = append(out, seg)
	}
	return out, rows.Err()
}

const maxLedgerSeq = int64(^uint64(0) >> 1)

func ledgerSegmentFromRow(source string, seq, checksum int64, createdAt, eventsJSON string) (ledger.Segment, error) {
	events, err := ledger.ParseEventsJSON([]byte(eventsJSON))
	if err != nil {
		return ledger.Segment{}, fmt.Errorf("ledger segment %s-%06d: %w", source, seq, err)
	}
	return ledger.Segment{
		Source:    source,
		SourceSeq: uint64(seq),
		Checksum:  uint32(checksum),
		CreatedAt: createdAt,
		Events:    events,
	}, nil
}

// LedgerSegmentSeqs returns every stored seq for (zone, source), ascending.
func (db *DB) LedgerSegmentSeqs(ctx context.Context, zone, source string) ([]uint64, error) {
	if !db.hasLedgerTables(ctx) {
		return nil, nil
	}
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT source_seq FROM ledger_segments
		WHERE zone = ? AND source = ? ORDER BY source_seq`, zone, source)
	if err != nil {
		return nil, fmt.Errorf("listing ledger seqs: %w", err)
	}
	defer rows.Close()
	var out []uint64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return nil, fmt.Errorf("scanning ledger seq: %w", err)
		}
		out = append(out, uint64(seq))
	}
	return out, rows.Err()
}

// LedgerStatus summarizes one zone: counts, latest seq per source, gaps
// (detect_gaps, store.rs:366-389) and the stored verify failures.
func (db *DB) LedgerStatus(ctx context.Context, zone string) (ledger.ZoneStatus, error) {
	st := ledger.ZoneStatus{Zone: zone, Sources: map[string]uint64{}}
	if !db.hasLedgerTables(ctx) {
		return st, nil
	}
	r := db.getReader()
	if err := r.QueryRow(ctx,
		`SELECT COUNT(*) FROM ledger_segments WHERE zone = ?`, zone,
	).Scan(&st.Segments); err != nil {
		return st, fmt.Errorf("counting ledger segments: %w", err)
	}
	if err := r.QueryRow(ctx,
		`SELECT COUNT(*) FROM ledger_events WHERE zone = ?`, zone,
	).Scan(&st.Events); err != nil {
		return st, fmt.Errorf("counting ledger events: %w", err)
	}
	latest, err := ledgerLatestSeqs(ctx, r, zone)
	if err != nil {
		return st, err
	}
	sources := make([]string, 0, len(latest))
	for _, l := range latest {
		st.Sources[l.source] = l.seq
		sources = append(sources, l.source)
	}
	for _, source := range sources {
		seqs, err := db.LedgerSegmentSeqs(ctx, zone, source)
		if err != nil {
			return st, err
		}
		for _, gap := range ledger.DetectGaps(seqs) {
			st.Gaps = append(st.Gaps, [2]string{source, strconv.FormatUint(gap, 10)})
		}
		ckpt, err := db.GetLedgerVerifyState(ctx, zone, source)
		if err != nil {
			return st, err
		}
		if ckpt != nil {
			st.Failures = append(st.Failures, ckpt.Failures...)
		}
	}
	return st, nil
}

type ledgerSourceSeq struct {
	source string
	seq    uint64
}

func ledgerLatestSeqs(ctx context.Context, r *readerHandle, zone string) ([]ledgerSourceSeq, error) {
	rows, err := r.QueryContext(ctx, `
		SELECT source, MAX(source_seq) FROM ledger_segments
		WHERE zone = ? GROUP BY source ORDER BY source`, zone)
	if err != nil {
		return nil, fmt.Errorf("listing ledger sources: %w", err)
	}
	defer rows.Close()
	var out []ledgerSourceSeq
	for rows.Next() {
		var l ledgerSourceSeq
		var seq int64
		if err := rows.Scan(&l.source, &seq); err != nil {
			return nil, fmt.Errorf("scanning ledger source: %w", err)
		}
		l.seq = uint64(seq)
		out = append(out, l)
	}
	return out, rows.Err()
}

// GetLedgerVerifyState returns the stored checkpoint for (zone, source),
// nil when none. A row whose JSON no longer parses degrades to the zero
// checkpoint, so the next verify of that source is a full one
// (store.rs:502-507).
func (db *DB) GetLedgerVerifyState(ctx context.Context, zone, source string) (*ledger.VerifyCheckpoint, error) {
	if !db.hasLedgerTables(ctx) {
		return nil, nil
	}
	var verified int64
	var failuresJSON, missingJSON string
	err := db.getReader().QueryRow(ctx, `
		SELECT verified_seq, failures_json, missing_json FROM ledger_verify_state
		WHERE zone = ? AND source = ?`, zone, source,
	).Scan(&verified, &failuresJSON, &missingJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading ledger verify state: %w", err)
	}
	return ledger.DecodeCheckpoint(verified, failuresJSON, missingJSON), nil
}

// SaveLedgerVerifyState upserts the checkpoint for (zone, source).
func (db *DB) SaveLedgerVerifyState(ctx context.Context, zone, source string, c ledger.VerifyCheckpoint) error {
	failures, missing, err := ledger.EncodeCheckpoint(c)
	if err != nil {
		return err
	}
	if c.VerifiedSeq > uint64(maxLedgerSeq) {
		return fmt.Errorf("ledger verify watermark %d exceeds i64::MAX", c.VerifiedSeq)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, err := db.getWriter().Exec(ctx, `
		INSERT INTO ledger_verify_state (
			zone, source, verified_seq, failures_json, missing_json, updated_at
		) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (zone, source) DO UPDATE SET
			verified_seq = excluded.verified_seq,
			failures_json = excluded.failures_json,
			missing_json = excluded.missing_json,
			updated_at = excluded.updated_at`,
		zone, source, int64(c.VerifiedSeq), failures, missing,
		ledger.StorageTimestamp(time.Now()),
	); err != nil {
		return fmt.Errorf("saving ledger verify state: %w", err)
	}
	return nil
}

// GetLedgerImportState returns the last import report for (zone, path).
func (db *DB) GetLedgerImportState(ctx context.Context, zone, path string) (*LedgerImportState, error) {
	if !db.hasLedgerTables(ctx) {
		return nil, nil
	}
	s := LedgerImportState{Zone: zone, Path: path}
	err := db.getReader().QueryRow(ctx, `
		SELECT last_report_json, updated_at FROM ledger_import_state
		WHERE zone = ? AND path = ?`, zone, path,
	).Scan(&s.LastReportJSON, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading ledger import state: %w", err)
	}
	return &s, nil
}

// SaveLedgerImportState upserts the last import report for (zone, path).
func (db *DB) SaveLedgerImportState(ctx context.Context, zone, path, reportJSON string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, err := db.getWriter().Exec(ctx, `
		INSERT INTO ledger_import_state (zone, path, last_report_json, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (zone, path) DO UPDATE SET
			last_report_json = excluded.last_report_json,
			updated_at = excluded.updated_at`,
		zone, path, reportJSON, ledger.StorageTimestamp(time.Now()),
	); err != nil {
		return fmt.Errorf("saving ledger import state: %w", err)
	}
	return nil
}

// RebuildLedgerIndex re-derives ledger_events for one zone from
// ledger_segments, the authority (jilog rebuild_from_store, db.rs:257-282).
// Unlike jilog it runs in one transaction: the delete guard on
// ledger_events is dropped and restored inside it, so a failure leaves the
// projection and the guard exactly as they were. It returns the number of
// events projected and segments read.
func (db *DB) RebuildLedgerIndex(ctx context.Context, zone string) (events, segments int, err error) {
	if !db.hasLedgerTables(ctx) {
		return 0, 0, nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("beginning ledger rebuild: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DROP TRIGGER IF EXISTS trg_ledger_events_no_delete`); err != nil {
		return 0, 0, fmt.Errorf("lifting ledger_events guard: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ledger_events WHERE zone = ?`, zone); err != nil {
		return 0, 0, fmt.Errorf("clearing ledger_events: %w", err)
	}
	segs, err := readLedgerZoneSegmentsTx(ctx, tx, zone)
	if err != nil {
		return 0, 0, err
	}
	for _, seg := range segs {
		prep, err := ledger.PrepareAppend(zone, seg, ledger.OriginLocal)
		if err != nil {
			return 0, 0, fmt.Errorf("re-projecting %s: %w", seg.Filename(), err)
		}
		for _, e := range prep.Events {
			res, err := tx.ExecContext(ctx, `
				INSERT INTO ledger_events (
					event_id, zone, source, source_seq, timestamp,
					correlation_id, causation_id, actor_ref, object_ref,
					event_class, payload_tier, payload, subsystem, summary,
					segment_source, segment_seq, event_json
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (event_id) DO NOTHING`,
				e.EventID, zone, e.Source, e.SourceSeq,
				ledger.StorageTimestamp(e.Timestamp),
				e.CorrelationID, e.CausationID, e.ActorRef, e.ObjectRef,
				e.EventClass, e.PayloadTier, e.Payload, e.Subsystem, e.Summary,
				prep.Source, prep.Seq, e.EventJSON,
			)
			if err != nil {
				return 0, 0, fmt.Errorf("re-projecting ledger event: %w", err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				events++
			}
		}
	}
	if _, err := tx.ExecContext(ctx, ledgerEventsNoDeleteTriggerDDL); err != nil {
		return 0, 0, fmt.Errorf("restoring ledger_events guard: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("committing ledger rebuild: %w", err)
	}
	return events, len(segs), nil
}

func readLedgerZoneSegmentsTx(ctx context.Context, tx *sql.Tx, zone string) ([]ledger.Segment, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT source, source_seq, checksum, created_at, events_json
		FROM ledger_segments WHERE zone = ? ORDER BY source, source_seq`, zone)
	if err != nil {
		return nil, fmt.Errorf("reading ledger segments: %w", err)
	}
	defer rows.Close()
	var segs []ledger.Segment
	for rows.Next() {
		var src, createdAt, eventsJSON string
		var seq, checksum int64
		if err := rows.Scan(&src, &seq, &checksum, &createdAt, &eventsJSON); err != nil {
			return nil, fmt.Errorf("scanning ledger segment: %w", err)
		}
		seg, err := ledgerSegmentFromRow(src, seq, checksum, createdAt, eventsJSON)
		if err != nil {
			return nil, err
		}
		segs = append(segs, seg)
	}
	return segs, rows.Err()
}

// CopyLedgerFrom copies every ledger table from the database at
// sourcePath (resync). Ledger rows are user data with no other source, so
// the caller aborts the swap when this fails.
func (db *DB) CopyLedgerFrom(sourcePath string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	ctx := context.Background()
	conn, err := db.getWriter().Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "ATTACH DATABASE ? AS old_db", sourcePath); err != nil {
		return fmt.Errorf("attaching source db: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, "DETACH DATABASE old_db") }()

	tables := []struct{ name, cols string }{
		{"ledger_segments", "zone, source, source_seq, checksum, created_at, event_count, events_json, origin, ingested_at"},
		{"ledger_events", "event_id, zone, source, source_seq, timestamp, correlation_id, causation_id, actor_ref, object_ref, event_class, payload_tier, payload, subsystem, summary, segment_source, segment_seq, event_json"},
		{"ledger_verify_state", "zone, source, verified_seq, failures_json, missing_json, updated_at"},
		{"ledger_import_state", "zone, path, last_report_json, updated_at"},
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning ledger copy: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, t := range tables {
		if !oldDBHasTable(ctx, tx, t.name) {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO "+t.name+" ("+t.cols+") SELECT "+t.cols+
				" FROM old_db."+t.name+" ORDER BY rowid",
		); err != nil {
			return fmt.Errorf("copying %s: %w", t.name, err)
		}
	}
	return tx.Commit()
}
