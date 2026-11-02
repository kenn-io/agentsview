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

// FrictionIssueLinkWriter records and removes pattern-to-issue links.
// Pattern reads return the link with each pattern.
type FrictionIssueLinkWriter interface {
	SaveFrictionIssueLink(ctx context.Context, l FrictionIssueLink) error
	// DeleteFrictionIssueLink reports whether a link existed.
	DeleteFrictionIssueLink(ctx context.Context, fingerprint string) (bool, error)
}

// SaveFrictionIssueLink records l, replacing any link for its pattern.
func (db *DB) SaveFrictionIssueLink(ctx context.Context, l FrictionIssueLink) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().ExecContext(ctx, `
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
	return nil
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

// frictionIssueColumns are the link columns a pattern read selects through
// its LEFT JOIN; an unlinked pattern reads them all as NULL.
const frictionIssueColumns = `l.kata_instance_uid, l.kata_project_uid,
	l.issue_uid, l.qualified_id, l.web_url, l.source, l.linked_at`

type frictionIssueScan struct {
	instance, project, issue, qualified, url, source, linkedAt sql.NullString
}

func (s *frictionIssueScan) targets() []any {
	return []any{&s.instance, &s.project, &s.issue, &s.qualified, &s.url, &s.source, &s.linkedAt}
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
