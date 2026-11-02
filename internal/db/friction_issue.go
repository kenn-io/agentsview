package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// FrictionIssueLink ties a Friction Log pattern to the Kata issue filed or
// linked for it. Kata owns the issue's state, title and status; the archive
// keeps only enough to find and show the issue.
type FrictionIssueLink struct {
	Fingerprint     string    `json:"fingerprint"`
	KataInstanceUID string    `json:"kata_instance_uid"`
	KataProjectUID  string    `json:"kata_project_uid"`
	IssueUID        string    `json:"issue_uid"`
	QualifiedID     string    `json:"qualified_id"`
	WebURL          string    `json:"web_url"`
	Source          string    `json:"source"`
	LinkedAt        time.Time `json:"linked_at"`
}

// How a link was made: AgentsView created the issue, found one Kata already
// held for the pattern, or a person linked it by hand.
const (
	FrictionIssueSourceCreated = "created"
	FrictionIssueSourceFound   = "found"
	FrictionIssueSourceManual  = "manual"
)

// FrictionFilingState records why automatic filing has not linked a
// pattern yet. A linked pattern has none.
type FrictionFilingState struct {
	Fingerprint string `json:"fingerprint"`
	// State is FrictionFilingRetry, FrictionFilingNeedsPerson or
	// FrictionFilingPaused.
	State         string     `json:"state"`
	Attempts      int        `json:"attempts"`
	FirstFailedAt *time.Time `json:"first_failed_at,omitempty"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// Automatic filing states: retry after NextAttemptAt; stop until a person
// files or links the pattern; or stop because a person removed its link.
const (
	FrictionFilingRetry       = "retry"
	FrictionFilingNeedsPerson = "needs_person"
	FrictionFilingPaused      = "paused"
)

// FrictionIssueLinkWriter records and removes pattern-to-issue links and the
// automatic filing state. Pattern reads return both with each pattern.
type FrictionIssueLinkWriter interface {
	// SaveFrictionIssueLink also clears the pattern's filing state.
	SaveFrictionIssueLink(ctx context.Context, l FrictionIssueLink) error
	// DeleteFrictionIssueLink reports whether a link existed.
	DeleteFrictionIssueLink(ctx context.Context, fingerprint string) (bool, error)
	SaveFrictionFilingState(ctx context.Context, st FrictionFilingState) error
}

// SaveFrictionIssueLink records l, replacing any link for its pattern, and
// clears the pattern's filing state.
func (db *DB) SaveFrictionIssueLink(ctx context.Context, l FrictionIssueLink) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("saving friction issue link: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM friction_filing_state WHERE fingerprint = ?`, l.Fingerprint); err != nil {
		return fmt.Errorf("clearing friction filing state: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO friction_issue_links (fingerprint, kata_instance_uid,
			kata_project_uid, issue_uid, qualified_id, web_url, source, linked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(fingerprint) DO UPDATE SET
			kata_instance_uid = excluded.kata_instance_uid,
			kata_project_uid = excluded.kata_project_uid,
			issue_uid = excluded.issue_uid,
			qualified_id = excluded.qualified_id,
			web_url = excluded.web_url,
			source = excluded.source,
			linked_at = excluded.linked_at`,
		l.Fingerprint, l.KataInstanceUID, l.KataProjectUID, l.IssueUID,
		l.QualifiedID, l.WebURL, l.Source, l.LinkedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("saving friction issue link: %w", err)
	}
	return tx.Commit()
}

// SaveFrictionFilingState records st, replacing any state for its pattern.
func (db *DB) SaveFrictionFilingState(ctx context.Context, st FrictionFilingState) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().ExecContext(ctx, `
		INSERT INTO friction_filing_state (fingerprint, state, attempts,
			first_failed_at, next_attempt_at, last_error, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(fingerprint) DO UPDATE SET
			state = excluded.state, attempts = excluded.attempts,
			first_failed_at = excluded.first_failed_at,
			next_attempt_at = excluded.next_attempt_at,
			last_error = excluded.last_error, updated_at = excluded.updated_at`,
		st.Fingerprint, st.State, st.Attempts, timeText(st.FirstFailedAt),
		timeText(st.NextAttemptAt), st.LastError, st.UpdatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("saving friction filing state: %w", err)
	}
	return nil
}

func timeText(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// DeleteFrictionIssueLink removes the local link only; the Kata issue stays.
func (db *DB) DeleteFrictionIssueLink(ctx context.Context, fingerprint string) (bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	res, err := db.getWriter().ExecContext(ctx,
		`DELETE FROM friction_issue_links WHERE fingerprint = ?`, fingerprint)
	if err != nil {
		return false, fmt.Errorf("deleting friction issue link: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("deleting friction issue link: %w", err)
	}
	return n > 0, nil
}

// frictionIssueColumns are the link and filing-state columns a pattern read
// selects through its LEFT JOINs; absent rows read as NULL.
const frictionIssueColumns = `l.kata_instance_uid, l.kata_project_uid,
	l.issue_uid, l.qualified_id, l.web_url, l.source, l.linked_at,
	f.state, f.attempts, f.first_failed_at, f.next_attempt_at, f.last_error,
	f.updated_at`

// frictionIssueJoins attach links and filing state to friction_patterns p.
const frictionIssueJoins = `LEFT JOIN friction_issue_links l ON l.fingerprint = p.fingerprint
	LEFT JOIN friction_filing_state f ON f.fingerprint = p.fingerprint`

type frictionIssueScan struct {
	instance, project, issue, qualified, url, source, linkedAt sql.NullString

	state, firstFailed, nextAttempt, lastError, updated sql.NullString
	attempts                                            sql.NullInt64
}

func (s *frictionIssueScan) targets() []any {
	return []any{
		&s.instance, &s.project, &s.issue, &s.qualified, &s.url, &s.source, &s.linkedAt,
		&s.state, &s.attempts, &s.firstFailed, &s.nextAttempt, &s.lastError, &s.updated,
	}
}

func (s *frictionIssueScan) filing(fingerprint string) (*FrictionFilingState, error) {
	if !s.state.Valid {
		return nil, nil
	}
	st := &FrictionFilingState{
		Fingerprint: fingerprint, State: s.state.String,
		Attempts: int(s.attempts.Int64), LastError: s.lastError.String,
	}
	var err error
	if st.FirstFailedAt, err = parseTimeText(s.firstFailed); err != nil {
		return nil, err
	}
	if st.NextAttemptAt, err = parseTimeText(s.nextAttempt); err != nil {
		return nil, err
	}
	updated, err := parseTimeText(s.updated)
	if err != nil {
		return nil, err
	}
	if updated != nil {
		st.UpdatedAt = *updated
	}
	return st, nil
}

func parseTimeText(s sql.NullString) (*time.Time, error) {
	if !s.Valid || s.String == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s.String)
	if err != nil {
		return nil, fmt.Errorf("parsing friction filing time: %w", err)
	}
	return &t, nil
}

func (s *frictionIssueScan) link(fingerprint string) (*FrictionIssueLink, error) {
	if !s.issue.Valid {
		return nil, nil
	}
	linkedAt, err := time.Parse(time.RFC3339Nano, s.linkedAt.String)
	if err != nil {
		return nil, fmt.Errorf("parsing friction issue link time: %w", err)
	}
	return &FrictionIssueLink{
		Fingerprint: fingerprint, KataInstanceUID: s.instance.String,
		KataProjectUID: s.project.String, IssueUID: s.issue.String,
		QualifiedID: s.qualified.String, WebURL: s.url.String,
		Source: s.source.String, LinkedAt: linkedAt,
	}, nil
}

var _ FrictionIssueLinkWriter = (*DB)(nil)
