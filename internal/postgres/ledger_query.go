package postgres

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ledger"
)

// QueryLedger mirrors (*db.DB).QueryLedger.
func (s *Store) QueryLedger(ctx context.Context, q ledger.Query) ([]ledger.ZoneEvents, error) {
	zones := []string{q.Zone}
	if q.Zone == "" {
		var err error
		if zones, err = s.LedgerZones(ctx); err != nil {
			return nil, err
		}
	}
	limit := q.Limit
	if limit <= 0 {
		limit = db.DefaultLedgerQueryLimit
	}
	out := []ledger.ZoneEvents{}
	for _, zone := range zones {
		events, err := s.queryLedgerZone(ctx, zone, q, limit)
		if err != nil {
			return nil, err
		}
		if len(events) > 0 {
			out = append(out, ledger.ZoneEvents{Zone: zone, Events: events})
		}
	}
	return out, nil
}

func (s *Store) queryLedgerZone(ctx context.Context, zone string, q ledger.Query, limit int) ([]ledger.Event, error) {
	var where strings.Builder
	args := []any{zone}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	where.WriteString("zone = $1")
	if !q.Since.IsZero() {
		where.WriteString(" AND timestamp >= " + arg(q.Since.UTC()))
	}
	if !q.Until.IsZero() {
		where.WriteString(" AND timestamp < " + arg(q.Until.UTC()))
	}
	if q.Class != nil {
		where.WriteString(" AND event_class = " + arg(string(*q.Class)))
	}
	if len(q.Subsystems) > 0 {
		where.WriteString(" AND subsystem <> '' AND (")
		for i, p := range q.Subsystems {
			if i > 0 {
				where.WriteString(" OR ")
			}
			if prefix, ok := strings.CutSuffix(p, "*"); ok {
				n := arg(prefix)
				where.WriteString("left(subsystem, char_length(" + n + ")) = " + n)
			} else {
				where.WriteString("subsystem = " + arg(p))
			}
		}
		where.WriteString(")")
	}
	query := "SELECT event_json FROM ledger_events WHERE " + where.String() +
		" ORDER BY timestamp DESC, event_id DESC LIMIT " + arg(limit)
	rows, err := s.pg.QueryContext(ctx, query, args...)
	if isUndefinedTable(err) {
		return nil, nil
	}
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

// LedgerZones mirrors (*db.DB).LedgerZones.
func (s *Store) LedgerZones(ctx context.Context) ([]string, error) {
	rows, err := s.pg.QueryContext(ctx, `SELECT DISTINCT zone FROM ledger_segments ORDER BY zone`)
	if isUndefinedTable(err) {
		return []string{}, nil
	}
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
