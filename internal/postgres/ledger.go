package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5/pgconn"

	"go.kenn.io/agentsview/internal/ledger"
)

// ledgerAppendOnlyMessage is what ledger_reject_mutation() raises.
const ledgerAppendOnlyMessage = "ledger is append-only"

// isLedgerAppendOnlyError matches the ledger_reject_mutation trigger.
func isLedgerAppendOnlyError(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == "55000" && pgErr.Message == ledgerAppendOnlyMessage
}

func mapLedgerPGError(action string, err error) error {
	if isLedgerAppendOnlyError(err) {
		return fmt.Errorf("%s: %w", action, ledger.ErrAppendOnly)
	}
	return mapPGWriteError(action, err)
}

// AppendLedgerSegment is the PostgreSQL twin of (*db.DB).AppendLedgerSegment:
// same validation (ledger.PrepareAppend), same identical/conflict rules.
func (s *Store) AppendLedgerSegment(
	ctx context.Context, zone string, seg ledger.Segment, origin string,
) (ledger.PublishOutcome, error) {
	prep, err := ledger.PrepareAppend(zone, seg, origin)
	if err != nil {
		return ledger.Published, err
	}
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return ledger.Published, fmt.Errorf("beginning ledger append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	outcome, err := appendPreparedLedgerSegmentTx(ctx, tx, prep)
	if err != nil || outcome == ledger.AlreadyIdentical {
		return outcome, err
	}
	if err := tx.Commit(); err != nil {
		return ledger.Published, mapLedgerPGError("committing ledger append", err)
	}
	return ledger.Published, nil
}

// appendPreparedLedgerSegmentTx inserts one prepared segment inside tx.
// PR 14's push reuses it for replication.
func appendPreparedLedgerSegmentTx(
	ctx context.Context, tx *sql.Tx, prep ledger.PreparedSegment,
) (ledger.PublishOutcome, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO ledger_segments (
			zone, source, source_seq, checksum, created_at,
			event_count, events_json, origin
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (zone, source, source_seq) DO NOTHING`,
		prep.Zone, prep.Source, prep.Seq, prep.Checksum, prep.CreatedAt,
		len(prep.Events), prep.EventsJSON, prep.Origin,
	)
	if err != nil {
		return ledger.Published, mapLedgerPGError("inserting ledger segment", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var checksum int64
		var createdAt, eventsJSON string
		if err := tx.QueryRowContext(ctx, `
			SELECT checksum, created_at, events_json FROM ledger_segments
			WHERE zone = $1 AND source = $2 AND source_seq = $3`,
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
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
			ON CONFLICT (event_id) DO NOTHING`,
			e.EventID, prep.Zone, e.Source, e.SourceSeq, e.Timestamp,
			e.CorrelationID, e.CausationID, e.ActorRef, e.ObjectRef,
			e.EventClass, e.PayloadTier, e.Payload, e.Subsystem, e.Summary,
			prep.Source, prep.Seq, e.EventJSON,
		); err != nil {
			return ledger.Published, mapLedgerPGError("inserting ledger event", err)
		}
	}
	return ledger.Published, nil
}

// LatestLedgerSeq returns the highest stored seq for (zone, source), or 0.
func (s *Store) LatestLedgerSeq(ctx context.Context, zone, source string) (uint64, error) {
	var seq int64
	err := s.pg.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(source_seq), 0) FROM ledger_segments
		WHERE zone = $1 AND source = $2`, zone, source,
	).Scan(&seq)
	if isUndefinedTable(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading latest ledger seq: %w", err)
	}
	return uint64(seq), nil
}

// ListLedgerSegments returns (zone, source) segments with seq > afterSeq,
// in seq order, at most limit (limit <= 0 means all).
func (s *Store) ListLedgerSegments(
	ctx context.Context, zone, source string, afterSeq uint64, limit int,
) ([]ledger.Segment, error) {
	if afterSeq > uint64(1<<63-1) {
		return []ledger.Segment{}, nil
	}
	var lim any
	if limit > 0 {
		lim = limit
	}
	rows, err := s.pg.QueryContext(ctx, `
		SELECT source, source_seq, checksum, created_at, events_json
		FROM ledger_segments
		WHERE zone = $1 AND source = $2 AND source_seq > $3
		ORDER BY source_seq
		LIMIT $4`, zone, source, int64(afterSeq), lim)
	if isUndefinedTable(err) {
		return []ledger.Segment{}, nil
	}
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
		events, err := ledger.ParseEventsJSON([]byte(eventsJSON))
		if err != nil {
			return nil, fmt.Errorf("ledger segment %s-%06d: %w", src, seq, err)
		}
		out = append(out, ledger.Segment{
			Source: src, SourceSeq: uint64(seq), Checksum: uint32(checksum),
			CreatedAt: createdAt, Events: events,
		})
	}
	return out, rows.Err()
}

// LedgerSegmentSeqs returns every stored seq for (zone, source), ascending.
func (s *Store) LedgerSegmentSeqs(ctx context.Context, zone, source string) ([]uint64, error) {
	rows, err := s.pg.QueryContext(ctx, `
		SELECT source_seq FROM ledger_segments
		WHERE zone = $1 AND source = $2 ORDER BY source_seq`, zone, source)
	if isUndefinedTable(err) {
		return nil, nil
	}
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

// LedgerStatus mirrors (*db.DB).LedgerStatus.
func (s *Store) LedgerStatus(ctx context.Context, zone string) (ledger.ZoneStatus, error) {
	st := ledger.ZoneStatus{Zone: zone, Sources: map[string]uint64{}}
	err := s.pg.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ledger_segments WHERE zone = $1`, zone,
	).Scan(&st.Segments)
	if isUndefinedTable(err) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("counting ledger segments: %w", err)
	}
	if err := s.pg.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ledger_events WHERE zone = $1`, zone,
	).Scan(&st.Events); err != nil {
		return st, fmt.Errorf("counting ledger events: %w", err)
	}
	latest, err := s.ledgerLatestSeqs(ctx, zone)
	if err != nil {
		return st, err
	}
	sources := make([]string, 0, len(latest))
	for _, source := range slices.Sorted(maps.Keys(latest)) {
		st.Sources[source] = latest[source]
		sources = append(sources, source)
	}
	for _, source := range sources {
		seqs, err := s.LedgerSegmentSeqs(ctx, zone, source)
		if err != nil {
			return st, err
		}
		for _, gap := range ledger.DetectGaps(seqs) {
			st.Gaps = append(st.Gaps, [2]string{source, strconv.FormatUint(gap, 10)})
		}
		ckpt, err := s.GetLedgerVerifyState(ctx, zone, source)
		if err != nil {
			return st, err
		}
		if ckpt != nil {
			st.Failures = append(st.Failures, ckpt.Failures...)
		}
	}
	return st, nil
}

func (s *Store) ledgerLatestSeqs(ctx context.Context, zone string) (map[string]uint64, error) {
	rows, err := s.pg.QueryContext(ctx, `
		SELECT source, MAX(source_seq) FROM ledger_segments
		WHERE zone = $1 GROUP BY source`, zone)
	if err != nil {
		return nil, fmt.Errorf("listing ledger sources: %w", err)
	}
	defer rows.Close()
	out := map[string]uint64{}
	for rows.Next() {
		var source string
		var seq int64
		if err := rows.Scan(&source, &seq); err != nil {
			return nil, fmt.Errorf("scanning ledger source: %w", err)
		}
		out[source] = uint64(seq)
	}
	return out, rows.Err()
}

// GetLedgerVerifyState mirrors (*db.DB).GetLedgerVerifyState.
func (s *Store) GetLedgerVerifyState(ctx context.Context, zone, source string) (*ledger.VerifyCheckpoint, error) {
	var verified int64
	var failuresJSON, missingJSON string
	err := s.pg.QueryRowContext(ctx, `
		SELECT verified_seq, failures_json, missing_json FROM ledger_verify_state
		WHERE zone = $1 AND source = $2`, zone, source,
	).Scan(&verified, &failuresJSON, &missingJSON)
	if errors.Is(err, sql.ErrNoRows) || isUndefinedTable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading ledger verify state: %w", err)
	}
	return ledger.DecodeCheckpoint(verified, failuresJSON, missingJSON), nil
}

// SaveLedgerVerifyState mirrors (*db.DB).SaveLedgerVerifyState.
func (s *Store) SaveLedgerVerifyState(ctx context.Context, zone, source string, c ledger.VerifyCheckpoint) error {
	f, m, err := ledger.EncodeCheckpoint(c)
	if err != nil {
		return err
	}
	if c.VerifiedSeq > uint64(1<<63-1) {
		return fmt.Errorf("ledger verify watermark %d exceeds i64::MAX", c.VerifiedSeq)
	}
	if _, err := s.pg.ExecContext(ctx, `
		INSERT INTO ledger_verify_state (
			zone, source, verified_seq, failures_json, missing_json, updated_at
		) VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (zone, source) DO UPDATE SET
			verified_seq = excluded.verified_seq,
			failures_json = excluded.failures_json,
			missing_json = excluded.missing_json,
			updated_at = excluded.updated_at`,
		zone, source, int64(c.VerifiedSeq), f, m,
	); err != nil {
		return mapPGWriteError("saving ledger verify state", err)
	}
	return nil
}

var (
	_ ledger.WriterStore = (*Store)(nil)
	_ ledger.VerifyStore = (*Store)(nil)
)
