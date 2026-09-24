package db

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/ledger"
)

// DefaultLedgerQueryLimit is jilog query's --limit default (query.rs:42).
const DefaultLedgerQueryLimit = 100

// QueryLedger returns matching events per zone, newest first
// (timestamp DESC, event_id DESC), at most q.Limit per zone. Unlike jilog
// (query.rs:210, "newest 5 x limit, then filter") every filter runs in SQL,
// so older matches are never cut off (spec D20). q.Zone "" queries every
// stored zone, in name order; zones without matches are omitted.
func (db *DB) QueryLedger(ctx context.Context, q ledger.Query) ([]ledger.ZoneEvents, error) {
	if !db.hasLedgerTables(ctx) {
		return []ledger.ZoneEvents{}, nil
	}
	zones := []string{q.Zone}
	if q.Zone == "" {
		var err error
		if zones, err = db.LedgerZones(ctx); err != nil {
			return nil, err
		}
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLedgerQueryLimit
	}
	out := []ledger.ZoneEvents{}
	for _, zone := range zones {
		events, err := db.queryLedgerZone(ctx, zone, q, limit)
		if err != nil {
			return nil, err
		}
		if len(events) > 0 {
			out = append(out, ledger.ZoneEvents{Zone: zone, Events: events})
		}
	}
	return out, nil
}

func (db *DB) queryLedgerZone(ctx context.Context, zone string, q ledger.Query, limit int) ([]ledger.Event, error) {
	var where strings.Builder
	args := []any{zone}
	where.WriteString("zone = ?")
	if !q.Since.IsZero() {
		where.WriteString(" AND timestamp >= ?")
		args = append(args, ledger.StorageTimestamp(q.Since))
	}
	if !q.Until.IsZero() {
		where.WriteString(" AND timestamp < ?")
		args = append(args, ledger.StorageTimestamp(q.Until))
	}
	if q.Class != nil {
		where.WriteString(" AND event_class = ?")
		args = append(args, string(*q.Class))
	}
	if len(q.Subsystems) > 0 {
		// An event without a subsystem never matches a pattern
		// (query.rs:231-234); a trailing '*' is a prefix match.
		where.WriteString(" AND subsystem <> '' AND (")
		for i, p := range q.Subsystems {
			if i > 0 {
				where.WriteString(" OR ")
			}
			if prefix, ok := strings.CutSuffix(p, "*"); ok {
				where.WriteString("substr(subsystem, 1, length(?)) = ?")
				args = append(args, prefix, prefix)
			} else {
				where.WriteString("subsystem = ?")
				args = append(args, p)
			}
		}
		where.WriteString(")")
	}
	args = append(args, limit)
	rows, err := db.getReader().QueryContext(ctx,
		"SELECT event_json FROM ledger_events WHERE "+where.String()+
			" ORDER BY timestamp DESC, event_id DESC LIMIT ?", args...)
	if err != nil {
		return nil, fmt.Errorf("querying ledger events: %w", err)
	}
	defer rows.Close()
	var events []ledger.Event
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scanning ledger event: %w", err)
		}
		e, err := ledger.ParseEventJSON([]byte(raw))
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// LedgerZones lists every zone that holds a segment, in name order.
func (db *DB) LedgerZones(ctx context.Context) ([]string, error) {
	if !db.hasLedgerTables(ctx) {
		return []string{}, nil
	}
	rows, err := db.getReader().QueryContext(ctx,
		`SELECT DISTINCT zone FROM ledger_segments ORDER BY zone`)
	if err != nil {
		return nil, fmt.Errorf("listing ledger zones: %w", err)
	}
	defer rows.Close()
	zones := []string{}
	for rows.Next() {
		var z string
		if err := rows.Scan(&z); err != nil {
			return nil, fmt.Errorf("scanning ledger zone: %w", err)
		}
		zones = append(zones, z)
	}
	return zones, rows.Err()
}
