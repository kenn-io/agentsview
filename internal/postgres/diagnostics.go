package postgres

import (
	"context"
	"strconv"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
)

func (s *Store) DiagnosticSignalsForDate(ctx context.Context, date string, loc *time.Location) ([]friction.Signal, error) {
	if !pgHasTable(ctx, s.pg, "diagnostic_events") {
		return nil, nil
	}
	from, to, err := db.FrictionDayBounds(date, loc)
	if err != nil {
		return nil, err
	}
	rows, err := s.pg.QueryContext(ctx, `SELECT e.id,e.source,e.message,e.machine,e.occurred_at FROM diagnostic_events e LEFT JOIN friction_digest_sessions d ON d.subject_id='diagnostic:'||e.id WHERE e.occurred_at >= $1 AND e.occurred_at < $2 AND (d.subject_id IS NULL OR d.date=$3) ORDER BY e.occurred_at,e.accepted_at,e.id`, from.UTC(), to.UTC(), date)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []friction.Signal
	for rows.Next() {
		var id, source, message, machine string
		var ts time.Time
		if err = rows.Scan(&id, &source, &message, &machine, &ts); err != nil {
			return nil, err
		}
		out = append(out, friction.Signal{Kind: friction.KindDiagnostic, SubjectID: "diagnostic:" + id, SubjectKind: friction.SubjectDiagnostic, Label: source, Text: message, Dims: friction.Dims{Machine: machine}, OccurredAt: ts})
	}
	return out, rows.Err()
}

func (s *Store) FrictionPendingDiagnosticDates(ctx context.Context, last string, loc *time.Location) ([]string, error) {
	if !pgHasTable(ctx, s.pg, "diagnostic_events") {
		return nil, nil
	}
	_, to, err := db.FrictionDayBounds(last, loc)
	if err != nil {
		return nil, err
	}
	rows, err := s.pg.QueryContext(ctx, `SELECT e.occurred_at FROM diagnostic_events e LEFT JOIN friction_digest_sessions d ON d.subject_id='diagnostic:'||e.id WHERE d.subject_id IS NULL AND e.occurred_at<$1`, to.UTC())
	if err != nil {
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
func (s *Sync) syncDiagnosticEvents(ctx context.Context, full bool, markerID string) error {
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
		tx, err := s.pg.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, event := range events {
			machine := event.Machine
			if machine == "" {
				machine = s.machine
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO diagnostic_events(id,source,message,machine,occurred_at,accepted_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(id) DO NOTHING`, event.ID, event.Source, event.Message, machine, event.OccurredAt, event.AcceptedAt); err != nil {
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
		if len(events) == 0 {
			return nil
		}
		watermark = events[len(events)-1].RowID
		if err = state.SetSyncState(ctx, key, strconv.FormatInt(watermark, 10)); err != nil {
			return err
		}
		if len(events) < 500 {
			return nil
		}
	}
}
