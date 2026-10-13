package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"strings"
)

// SessionPlanUsage is one session's row in session_plan_usage: the latest
// plan usage report and the newest outcome written after it, and the
// session's live token capture coverage. HasPlanUsage is false for a
// session with coverage only; HasCoverage is false for a session no live
// capture ever saw. Times are RFC 3339 UTC strings, empty when absent.
// Credits stay decimal strings as OpenAI sent them.
type SessionPlanUsage struct {
	HasPlanUsage    bool
	DataAsOf        string
	DataStatus      string
	UsageSource     string
	WeeklyPercent   *float64
	FiveHourPercent *float64
	Credits         *string
	Groups          []SessionPlanUsageGroup
	Final           bool
	OutcomeKind     string
	OutcomeAt       string
	// CountedUnder is the usage root's session ID when this session's
	// usage counts toward the root.
	CountedUnder    string
	HasCoverage     bool
	LastCaptureAt   string
	UncapturedTurns int
}

// SessionPlanUsageGroup is a plan usage report's figures for one model,
// reasoning effort, and speed. The JSON names are the stored format of
// session_plan_usage.groups_json.
type SessionPlanUsageGroup struct {
	Model             string   `json:"model"`
	ReasoningEffort   string   `json:"reasoning_effort"`
	Speed             string   `json:"speed"`
	ProductExperience string   `json:"product_experience"`
	WeeklyPercent     *float64 `json:"weekly_percent"`
	FiveHourPercent   *float64 `json:"five_hour_percent"`
	Credits           *string  `json:"credits"`
}

const sessionPlanUsageCols = `has_plan_usage, data_as_of, data_status,
	usage_source, weekly_percent, five_hour_percent, credits, groups_json,
	is_final, outcome_kind, outcome_at, counted_under, has_coverage,
	last_capture_at, uncaptured_turns`

// ReplaceSessionPlanUsage replaces one session's plan usage row. A nil p
// removes the row.
func (db *DB) ReplaceSessionPlanUsage(
	ctx context.Context, sessionID string, p *SessionPlanUsage,
) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning plan usage tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := replaceSessionPlanUsageTx(tx, sessionID, p); err != nil {
		return err
	}
	return tx.Commit()
}

// replaceSessionPlanUsageTx writes or removes the row and, when either
// changed the table, bumps local_modified_at so push targets re-select the
// session, as a usage event replace does.
func replaceSessionPlanUsageTx(
	tx transactionQueries, sessionID string, p *SessionPlanUsage,
) error {
	if p == nil {
		res, err := tx.Exec(
			`DELETE FROM session_plan_usage WHERE session_id = ?`, sessionID,
		)
		if err != nil {
			return fmt.Errorf("deleting plan usage for %s: %w", sessionID, err)
		}
		removed, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("counting removed plan usage for %s: %w", sessionID, err)
		}
		if removed == 0 {
			return nil
		}
		return bumpLocalModifiedTx(tx, sessionID, "plan usage replace")
	}
	groups := p.Groups
	if groups == nil {
		groups = []SessionPlanUsageGroup{}
	}
	groupsJSON, err := json.Marshal(groups)
	if err != nil {
		return fmt.Errorf("encoding plan usage groups for %s: %w", sessionID, err)
	}
	if _, err := tx.Exec(`
		INSERT OR REPLACE INTO session_plan_usage (session_id, `+sessionPlanUsageCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, p.HasPlanUsage, nullIfEmpty(p.DataAsOf), p.DataStatus,
		p.UsageSource, p.WeeklyPercent, p.FiveHourPercent, p.Credits,
		string(groupsJSON), p.Final, p.OutcomeKind, nullIfEmpty(p.OutcomeAt),
		p.CountedUnder, p.HasCoverage, nullIfEmpty(p.LastCaptureAt),
		p.UncapturedTurns,
	); err != nil {
		return fmt.Errorf("writing plan usage for %s: %w", sessionID, err)
	}
	return bumpLocalModifiedTx(tx, sessionID, "plan usage replace")
}

func bumpLocalModifiedTx(tx transactionQueries, sessionID, after string) error {
	if _, err := tx.Exec(
		`UPDATE sessions
		    SET local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		  WHERE id = ?`,
		sessionID,
	); err != nil {
		return fmt.Errorf("bumping local_modified_at for %s after %s: %w",
			sessionID, after, err)
	}
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// GetSessionPlanUsage returns one session's plan usage row, or nil when
// the session has none.
func (db *DB) GetSessionPlanUsage(
	ctx context.Context, sessionID string,
) (*SessionPlanUsage, error) {
	return sessionPlanUsageWithQuerier(ctx, db.getReader(), sessionID)
}

func sessionPlanUsageWithQuerier(
	ctx context.Context, q messageRowsQuerier, sessionID string,
) (*SessionPlanUsage, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+sessionPlanUsageCols+` FROM session_plan_usage WHERE session_id = ?`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying plan usage for %s: %w", sessionID, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var p SessionPlanUsage
	var dataAsOf, outcomeAt, lastCaptureAt, credits sql.NullString
	var weekly, fiveHour sql.NullFloat64
	var groupsJSON string
	if err := rows.Scan(
		&p.HasPlanUsage, &dataAsOf, &p.DataStatus, &p.UsageSource,
		&weekly, &fiveHour, &credits, &groupsJSON, &p.Final,
		&p.OutcomeKind, &outcomeAt, &p.CountedUnder, &p.HasCoverage,
		&lastCaptureAt, &p.UncapturedTurns,
	); err != nil {
		return nil, fmt.Errorf("scanning plan usage for %s: %w", sessionID, err)
	}
	if err := json.Unmarshal([]byte(groupsJSON), &p.Groups); err != nil {
		return nil, fmt.Errorf("decoding plan usage groups for %s: %w", sessionID, err)
	}
	if len(p.Groups) == 0 {
		p.Groups = nil
	}
	p.DataAsOf, p.OutcomeAt, p.LastCaptureAt = dataAsOf.String, outcomeAt.String, lastCaptureAt.String
	if weekly.Valid {
		p.WeeklyPercent = &weekly.Float64
	}
	if fiveHour.Valid {
		p.FiveHourPercent = &fiveHour.Float64
	}
	if credits.Valid {
		p.Credits = &credits.String
	}
	return &p, rows.Err()
}

// PlanUsageFingerprints returns each session's plan usage row as one
// exact string, "" for a session with no row. Push targets compare it to
// detect plan usage changes, as they do usage event fingerprints.
func (db *DB) PlanUsageFingerprints(
	ctx context.Context, sessionIDs []string,
) (map[string]string, error) {
	out := make(map[string]string, len(sessionIDs))
	for _, id := range sessionIDs {
		out[id] = ""
	}
	const batchSize = 900
	for start := 0; start < len(sessionIDs); start += batchSize {
		batch := sessionIDs[start:min(start+batchSize, len(sessionIDs))]
		if err := appendPlanUsageFingerprints(ctx, db.getReader(), out, batch); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func appendPlanUsageFingerprints(
	ctx context.Context, q messageRowsQuerier, out map[string]string, sessionIDs []string,
) error {
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		args[i] = id
	}
	rows, err := q.QueryContext(ctx,
		`SELECT session_id, `+sessionPlanUsageCols+` FROM session_plan_usage
		WHERE session_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+`)`,
		args...,
	)
	if err != nil {
		return fmt.Errorf("querying plan usage fingerprints: %w", err)
	}
	defer rows.Close()
	text := func(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
	for rows.Next() {
		var sessionID, dataStatus, usageSource, groupsJSON string
		var outcomeKind, countedUnder string
		var hasPlanUsage, final, hasCoverage bool
		var uncaptured int
		var dataAsOf, outcomeAt, lastCaptureAt, credits sql.NullString
		var weekly, fiveHour sql.NullFloat64
		if err := rows.Scan(
			&sessionID, &hasPlanUsage, &dataAsOf, &dataStatus, &usageSource,
			&weekly, &fiveHour, &credits, &groupsJSON, &final,
			&outcomeKind, &outcomeAt, &countedUnder, &hasCoverage,
			&lastCaptureAt, &uncaptured,
		); err != nil {
			return fmt.Errorf("scanning plan usage fingerprint: %w", err)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%t|%t|%t|%d|", hasPlanUsage, final, hasCoverage, uncaptured)
		for _, s := range []sql.NullString{
			dataAsOf, text(dataStatus), text(usageSource), credits,
			text(groupsJSON), text(outcomeKind), outcomeAt,
			text(countedUnder), lastCaptureAt,
		} {
			fmt.Fprintf(&b, "%t:%d:%s|", s.Valid, len(s.String), s.String)
		}
		for _, f := range []sql.NullFloat64{weekly, fiveHour} {
			fmt.Fprintf(&b, "%t:%s|", f.Valid, strconv.FormatFloat(f.Float64, 'g', -1, 64))
		}
		out[sessionID] = b.String()
	}
	return rows.Err()
}
