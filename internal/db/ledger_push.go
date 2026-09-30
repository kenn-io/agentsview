package db

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/ledger"
)

// LedgerPushCursor is a position in replication's stable segment order.
type LedgerPushCursor struct {
	IngestedAt string
	Zone       string
	Source     string
	Seq        int64
}

// LedgerPushSegment is one stored segment with its zone and ingest time.
type LedgerPushSegment struct {
	Zone       string
	IngestedAt string
	Segment    ledger.Segment
}

// LedgerPushIdentity identifies one segment to retry outside the regular
// ingest-time window.
type LedgerPushIdentity struct {
	Zone   string
	Source string
	Seq    int64
}

// Cursor returns this segment's position in replication order.
func (p LedgerPushSegment) Cursor() LedgerPushCursor {
	return LedgerPushCursor{
		IngestedAt: p.IngestedAt, Zone: p.Zone,
		Source: p.Segment.Source, Seq: int64(p.Segment.SourceSeq),
	}
}

// ListLedgerSegmentsForPush pages by ingest time and segment identity.
func (db *DB) ListLedgerSegmentsForPush(
	ctx context.Context, since string, after *LedgerPushCursor, limit int,
) ([]LedgerPushSegment, error) {
	if !db.hasLedgerTables(ctx) {
		return nil, nil
	}
	if limit <= 0 {
		limit = -1
	}
	query := `
		SELECT zone, ingested_at, source, source_seq, checksum, created_at, events_json
		FROM ledger_segments
		WHERE ingested_at >= ?`
	args := []any{since}
	if after != nil {
		query += ` AND (ingested_at, zone, source, source_seq) > (?, ?, ?, ?)`
		args = append(args, after.IngestedAt, after.Zone, after.Source, after.Seq)
	}
	query += ` ORDER BY ingested_at, zone, source, source_seq LIMIT ?`
	args = append(args, limit)
	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing ledger segments for push: %w", err)
	}
	defer rows.Close()
	var out []LedgerPushSegment
	for rows.Next() {
		var p LedgerPushSegment
		var src, createdAt, eventsJSON string
		var seq, checksum int64
		if err := rows.Scan(&p.Zone, &p.IngestedAt, &src, &seq, &checksum, &createdAt, &eventsJSON); err != nil {
			return nil, fmt.Errorf("scanning ledger segment: %w", err)
		}
		if p.Segment, err = ledgerSegmentFromRow(src, seq, checksum, createdAt, eventsJSON); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListLedgerSegmentsForPushIdentities loads an explicit set of retry
// identities in one query. Callers should pass bounded batches so the query
// stays below SQLite's bind-parameter limit.
func (db *DB) ListLedgerSegmentsForPushIdentities(
	ctx context.Context, identities []LedgerPushIdentity,
) ([]LedgerPushSegment, error) {
	if len(identities) == 0 || !db.hasLedgerTables(ctx) {
		return nil, nil
	}
	var query strings.Builder
	query.WriteString(`
		SELECT zone, ingested_at, source, source_seq, checksum, created_at, events_json
		FROM ledger_segments
		WHERE (zone, source, source_seq) IN (`)
	args := make([]any, 0, len(identities)*3)
	for i, identity := range identities {
		if i > 0 {
			query.WriteString(", ")
		}
		query.WriteString("(?, ?, ?)")
		args = append(args, identity.Zone, identity.Source, identity.Seq)
	}
	query.WriteString(`) ORDER BY ingested_at, zone, source, source_seq`)
	rows, err := db.getReader().QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("listing ledger retry segments: %w", err)
	}
	defer rows.Close()
	var out []LedgerPushSegment
	for rows.Next() {
		var p LedgerPushSegment
		var src, createdAt, eventsJSON string
		var seq, checksum int64
		if err := rows.Scan(&p.Zone, &p.IngestedAt, &src, &seq, &checksum, &createdAt, &eventsJSON); err != nil {
			return nil, fmt.Errorf("scanning ledger retry segment: %w", err)
		}
		if p.Segment, err = ledgerSegmentFromRow(src, seq, checksum, createdAt, eventsJSON); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListSyncStateByPrefix returns sync state with prefix removed from each key.
func (db *DB) ListSyncStateByPrefix(ctx context.Context, prefix string) (map[string]string, error) {
	escaped := strings.NewReplacer(
		"\\", "\\\\", "%", "\\%", "_", "\\_",
	).Replace(prefix)
	rows, err := db.getReader().QueryContext(ctx,
		"SELECT key, value FROM pg_sync_state WHERE key LIKE ? ESCAPE '\\'",
		escaped+"%",
	)
	if err != nil {
		return nil, fmt.Errorf("listing sync state: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("scanning sync state: %w", err)
		}
		out[strings.TrimPrefix(key, prefix)] = value
	}
	return out, rows.Err()
}
