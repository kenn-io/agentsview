package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"go.kenn.io/agentsview/internal/friction"
)

var ErrDiagnosticConflict = errors.New("diagnostic ID already has a different payload")

func diagnosticTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

// DiagnosticEvent is an immutable event recorded outside an agent session.
type DiagnosticEvent struct {
	ID         string    `json:"id"`
	Source     string    `json:"source"`
	Message    string    `json:"message"`
	Machine    string    `json:"machine,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
	AcceptedAt time.Time `json:"accepted_at"`
	RowID      int64     `json:"-"`
}

func ValidateDiagnostic(e DiagnosticEvent, now time.Time) error {
	if _, err := uuid.Parse(e.ID); err != nil {
		return errors.New("id must be a UUID")
	}
	if strings.TrimSpace(e.Source) == "" || utf8.RuneCountInString(e.Source) > 64 || !utf8.ValidString(e.Source) {
		return errors.New("source must contain 1–64 characters")
	}
	if strings.TrimSpace(e.Message) == "" || utf8.RuneCountInString(e.Message) > 500 || !utf8.ValidString(e.Message) {
		return errors.New("message must contain 1–500 characters")
	}
	if e.OccurredAt.After(now.Add(5*time.Minute)) {
		return errors.New("occurred_at must not be in the future")
	}
	return nil
}

// AppendDiagnostic stores one event; an identical retry returns the stored row.
func (db *DB) AppendDiagnostic(ctx context.Context, e DiagnosticEvent) (DiagnosticEvent, error) {
	if err := db.requireDerivedTextStorage("diagnostics"); err != nil {
		return DiagnosticEvent{}, err
	}
	now := time.Now().UTC()
	suppliedTime := !e.OccurredAt.IsZero()
	if e.OccurredAt.IsZero() {
		e.OccurredAt = now
	}
	if err := ValidateDiagnostic(e, now); err != nil {
		return DiagnosticEvent{}, err
	}
	e.OccurredAt = e.OccurredAt.UTC()
	e.AcceptedAt = now
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().Exec(ctx, `INSERT INTO diagnostic_events(id,source,message,machine,occurred_at,accepted_at) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, e.ID, e.Source, e.Message, e.Machine, diagnosticTime(e.OccurredAt), diagnosticTime(e.AcceptedAt))
	if err != nil {
		return DiagnosticEvent{}, fmt.Errorf("append diagnostic: %w", err)
	}
	var got DiagnosticEvent
	var occurred, accepted string
	err = db.getWriter().QueryRow(ctx, `SELECT id,source,message,machine,occurred_at,accepted_at FROM diagnostic_events WHERE id=?`, e.ID).Scan(&got.ID, &got.Source, &got.Message, &got.Machine, &occurred, &accepted)
	if err != nil {
		return DiagnosticEvent{}, err
	}
	got.OccurredAt, err = parseTimestamp(occurred)
	if err != nil {
		return DiagnosticEvent{}, err
	}
	got.AcceptedAt, err = parseTimestamp(accepted)
	if err != nil {
		return DiagnosticEvent{}, err
	}
	if got.Source != e.Source || got.Message != e.Message || (suppliedTime && !got.OccurredAt.Equal(e.OccurredAt)) {
		return DiagnosticEvent{}, ErrDiagnosticConflict
	}
	return got, nil
}

func (db *DB) diagnosticTableExists(ctx context.Context) (bool, error) {
	var n int
	err := db.getReader().QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='diagnostic_events'`).Scan(&n)
	return n != 0, err
}

// DiagnosticEventsAfter lists rows in append order for a replica push.
func (db *DB) DiagnosticEventsAfter(ctx context.Context, after int64, limit int) ([]DiagnosticEvent, error) {
	ok, err := db.diagnosticTableExists(ctx)
	if err != nil || !ok {
		return nil, err
	}
	rows, err := db.getReader().QueryContext(ctx, `SELECT rowid,id,source,message,machine,occurred_at,accepted_at FROM diagnostic_events WHERE rowid>? ORDER BY rowid LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []DiagnosticEvent
	for rows.Next() {
		var e DiagnosticEvent
		var occurred, accepted string
		if err = rows.Scan(&e.RowID, &e.ID, &e.Source, &e.Message, &e.Machine, &occurred, &accepted); err != nil {
			return nil, err
		}
		e.OccurredAt, err = parseTimestamp(occurred)
		if err != nil {
			return nil, err
		}
		e.AcceptedAt, err = parseTimestamp(accepted)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (db *DB) DiagnosticSignalsForDate(ctx context.Context, date string, loc *time.Location) ([]friction.Signal, error) {
	ok, err := db.diagnosticTableExists(ctx)
	if err != nil || !ok {
		return nil, err
	}
	from, to, err := FrictionDayBounds(date, loc)
	if err != nil {
		return nil, err
	}
	rows, err := db.getReader().QueryContext(ctx, `SELECT e.id,e.source,e.message,e.machine,e.occurred_at FROM diagnostic_events e LEFT JOIN friction_digest_sessions d ON d.subject_id='diagnostic:'||e.id WHERE e.occurred_at>=? AND e.occurred_at<? AND (d.subject_id IS NULL OR d.date=?) ORDER BY e.occurred_at,e.accepted_at,e.rowid`, diagnosticTime(from), diagnosticTime(to), date)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []friction.Signal
	for rows.Next() {
		var id, source, message, machine, raw string
		if err = rows.Scan(&id, &source, &message, &machine, &raw); err != nil {
			return nil, err
		}
		ts, err := parseTimestamp(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, friction.Signal{Kind: friction.KindDiagnostic, SubjectID: "diagnostic:" + id, SubjectKind: friction.SubjectDiagnostic, Label: source, Text: message, Dims: friction.Dims{Machine: machine}, OccurredAt: ts})
	}
	return out, rows.Err()
}

func (db *DB) FrictionPendingDiagnosticDates(ctx context.Context, last string, loc *time.Location) ([]string, error) {
	ok, err := db.diagnosticTableExists(ctx)
	if err != nil || !ok {
		return nil, err
	}
	_, to, err := FrictionDayBounds(last, loc)
	if err != nil {
		return nil, err
	}
	rows, err := db.getReader().QueryContext(ctx, `SELECT e.occurred_at FROM diagnostic_events e LEFT JOIN friction_digest_sessions d ON d.subject_id='diagnostic:'||e.id WHERE d.subject_id IS NULL AND e.occurred_at<?`, diagnosticTime(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	dates := map[string]bool{}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		ts, err := parseTimestamp(raw)
		if err != nil {
			return nil, err
		}
		dates[ts.In(loc).Format("2006-01-02")] = true
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(dates))
	for d := range dates {
		out = append(out, d)
	}
	return out, nil
}
