package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
)

func (s *Store) DiagnosticSignalsForDate(ctx context.Context, date string, loc *time.Location) ([]friction.Signal, error) {
	from, to, err := db.FrictionDayBounds(date, loc)
	if err != nil {
		return nil, err
	}
	rows, err := s.pg.QueryContext(ctx, `SELECT e.id,e.source,e.message,e.occurred_at FROM diagnostic_events e LEFT JOIN friction_digest_sessions d ON d.subject_id='diagnostic:'||e.id WHERE e.occurred_at >= $1 AND e.occurred_at < $2 AND (d.subject_id IS NULL OR d.date=$3) ORDER BY e.occurred_at,e.accepted_at,e.id`, from.UTC(), to.UTC(), date)
	if err != nil {
		if isUndefinedTable(err) && !pgHasTable(ctx, s.pg, "diagnostic_events") {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []friction.Signal
	for rows.Next() {
		var id, source, message string
		var ts time.Time
		if err = rows.Scan(&id, &source, &message, &ts); err != nil {
			return nil, err
		}
		out = append(out, friction.Signal{Kind: friction.KindDiagnostic, SubjectID: "diagnostic:" + id, SubjectKind: friction.SubjectDiagnostic, Label: source, Text: message, OccurredAt: ts})
	}
	return out, rows.Err()
}

func (s *Store) FrictionPendingDiagnosticDates(ctx context.Context, last string, loc *time.Location) ([]string, error) {
	_, to, err := db.FrictionDayBounds(last, loc)
	if err != nil {
		return nil, err
	}
	rows, err := s.pg.QueryContext(ctx, `SELECT e.occurred_at FROM diagnostic_events e LEFT JOIN friction_digest_sessions d ON d.subject_id='diagnostic:'||e.id WHERE d.subject_id IS NULL AND e.occurred_at<$1`, to.UTC())
	if err != nil {
		if isUndefinedTable(err) && !pgHasTable(ctx, s.pg, "diagnostic_events") {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	dates := map[string]bool{}
	for rows.Next() {
		var ts time.Time
		if err = rows.Scan(&ts); err != nil {
			return nil, err
		}
		dates[ts.In(loc).Format("2006-01-02")] = true
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(dates))
	for date := range dates {
		out = append(out, date)
	}
	return out, nil
}

// syncDiagnosticEvents publishes a bounded page before advancing its target watermark.
func (s *Sync) syncDiagnosticEvents(ctx context.Context, full bool, markerID string) (retErr error) {
	var conflicts error
	defer func() { retErr = errors.Join(conflicts, retErr) }()
	key := "diagnostic_push:" + s.targetFingerprint + ":" + markerID + ":" + s.databaseGeneration
	marker := "diagnostic_marker:" + markerID + ":" + s.databaseGeneration
	state := handoffSyncState{s.local}
	var exists bool
	if err := s.pg.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_metadata WHERE key=$1)`, marker).Scan(&exists); err != nil {
		return err
	}
	watermark := int64(0)
	if !full && exists {
		raw, err := state.GetSyncState(ctx, key)
		if err != nil {
			return err
		}
		if raw != "" {
			watermark, err = strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return err
			}
		}
	} else if err := state.SetSyncState(ctx, key, "0"); err != nil {
		return err
	}
	for {
		events, err := s.local.DiagnosticEventsAfter(ctx, watermark, 500)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		tx, err := s.pg.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		var pageConflicts error
		for _, event := range events {
			if err = insertDiagnosticEvent(ctx, tx, event); err != nil {
				if errors.Is(err, db.ErrDiagnosticConflict) {
					pageConflicts = errors.Join(pageConflicts, fmt.Errorf("%s: %w", event.ID, err))
					continue
				}
				_ = tx.Rollback()
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO sync_metadata(key,value) VALUES($1,'1') ON CONFLICT(key) DO UPDATE SET value=excluded.value`, marker); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		conflicts = errors.Join(conflicts, pageConflicts)
		watermark = events[len(events)-1].RowID
		if err = state.SetSyncState(ctx, key, strconv.FormatInt(watermark, 10)); err != nil {
			return err
		}
		if len(events) < 500 {
			return nil
		}
	}
}

// insertDiagnosticEvent accepts a retried payload and rejects a different one under the same ID.
func insertDiagnosticEvent(ctx context.Context, tx *sql.Tx, event db.DiagnosticEvent) error {
	result, err := tx.ExecContext(ctx, `INSERT INTO diagnostic_events(id,source,message,occurred_at,occurred_at_exact,accepted_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(id) DO UPDATE SET id=excluded.id WHERE diagnostic_events.source=excluded.source AND diagnostic_events.message=excluded.message AND diagnostic_events.occurred_at_exact=excluded.occurred_at_exact`, event.ID, event.Source, event.Message, event.OccurredAt, event.OccurredAt.UTC().Format(time.RFC3339Nano), event.AcceptedAt)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		err = db.ErrDiagnosticConflict
	}
	return err
}
