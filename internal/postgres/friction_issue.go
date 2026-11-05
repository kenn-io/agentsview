package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// SaveFrictionIssueLink records l, replacing any link for its pattern.
func (s *Store) SaveFrictionIssueLink(ctx context.Context, l db.FrictionIssueLink) error {
	_, err := s.pg.ExecContext(ctx, `
		INSERT INTO friction_issue_links (fingerprint, kata_instance_uid,
			kata_project_uid, issue_uid, qualified_id, web_url, source, linked_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (fingerprint) DO UPDATE SET
			kata_instance_uid = EXCLUDED.kata_instance_uid,
			kata_project_uid = EXCLUDED.kata_project_uid,
			issue_uid = EXCLUDED.issue_uid,
			qualified_id = EXCLUDED.qualified_id,
			web_url = EXCLUDED.web_url,
			source = EXCLUDED.source,
			linked_at = EXCLUDED.linked_at`,
		l.Fingerprint, l.KataInstanceUID, l.KataProjectUID, l.IssueUID,
		l.QualifiedID, l.WebURL, l.Source, l.LinkedAt.UTC())
	if err != nil {
		return fmt.Errorf("saving friction issue link: %w", err)
	}
	return nil
}

// AddFrictionIssueLink records l unless its pattern is already linked and
// returns the stored link, in one statement so a concurrent link wins
// cleanly.
func (s *Store) AddFrictionIssueLink(ctx context.Context, l db.FrictionIssueLink) (db.FrictionIssueLink, error) {
	stored := db.FrictionIssueLink{Fingerprint: l.Fingerprint}
	err := s.pg.QueryRowContext(ctx, `
		WITH added AS (
			INSERT INTO friction_issue_links (fingerprint, kata_instance_uid,
				kata_project_uid, issue_uid, qualified_id, web_url, source, linked_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (fingerprint) DO NOTHING
			RETURNING kata_instance_uid, kata_project_uid, issue_uid,
				qualified_id, web_url, source, linked_at
		)
		SELECT * FROM added
		UNION ALL
		SELECT kata_instance_uid, kata_project_uid, issue_uid, qualified_id,
			web_url, source, linked_at
		FROM friction_issue_links
		WHERE fingerprint = $1 AND NOT EXISTS (SELECT 1 FROM added)`,
		l.Fingerprint, l.KataInstanceUID, l.KataProjectUID, l.IssueUID,
		l.QualifiedID, l.WebURL, l.Source, l.LinkedAt.UTC()).Scan(
		&stored.KataInstanceUID, &stored.KataProjectUID, &stored.IssueUID,
		&stored.QualifiedID, &stored.WebURL, &stored.Source, &stored.LinkedAt)
	if err != nil {
		return db.FrictionIssueLink{}, fmt.Errorf("adding friction issue link: %w", err)
	}
	stored.LinkedAt = stored.LinkedAt.UTC()
	return stored, nil
}

// DeleteFrictionIssueLink removes the local link only; the Kata issue stays.
func (s *Store) DeleteFrictionIssueLink(ctx context.Context, fingerprint string) (bool, error) {
	res, err := s.pg.ExecContext(ctx,
		`DELETE FROM friction_issue_links WHERE fingerprint = $1`, fingerprint)
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

// frictionIssueSelect returns the link columns and join a pattern read
// adds. A read-only role reading a schema that predates the links table
// cannot create it, so its pattern reads show every pattern unlinked
// instead of failing. The table is looked up until a read finds it, so a
// later migration takes effect without a restart.
func (s *Store) frictionIssueSelect(ctx context.Context) (columns, join string) {
	if !s.runtimeState().frictionIssueLinksPresent.Load() {
		if !pgHasTable(ctx, s.pg, "friction_issue_links") {
			return `NULL, NULL, NULL, NULL, NULL, NULL, NULL`, ""
		}
		s.runtimeState().frictionIssueLinksPresent.Store(true)
	}
	return frictionIssueColumns, `LEFT JOIN friction_issue_links l ON l.fingerprint = p.fingerprint`
}

type frictionIssueScan struct {
	instance, project, issue, qualified, url, source sql.NullString
	linkedAt                                         sql.NullTime
}

func (s *frictionIssueScan) targets() []any {
	return []any{&s.instance, &s.project, &s.issue, &s.qualified, &s.url, &s.source, &s.linkedAt}
}

func (s *frictionIssueScan) link(fingerprint string) *db.FrictionIssueLink {
	if !s.issue.Valid {
		return nil
	}
	return &db.FrictionIssueLink{
		Fingerprint: fingerprint, KataInstanceUID: s.instance.String,
		KataProjectUID: s.project.String, IssueUID: s.issue.String,
		QualifiedID: s.qualified.String, WebURL: s.url.String,
		Source: s.source.String, LinkedAt: s.linkedAt.Time.UTC(),
	}
}

// FrictionIssueLinksWritable reports whether startup proved this role can
// write Kata issue links. The filing routes refuse before contacting Kata
// when it cannot, so no issue is created without its link.
func (s *Store) FrictionIssueLinksWritable() bool {
	return s.runtimeState().frictionIssueLinksWritable.Load()
}

// probeFrictionIssueLinkWrite runs every link write, the replacing save,
// the insert-only save and the delete, inside a rolled-back transaction. A
// missing table or a role lacking INSERT, UPDATE or DELETE reports false.
func (s *Store) probeFrictionIssueLinkWrite(ctx context.Context) bool {
	if !pgHasTable(ctx, s.pg, "friction_issue_links") {
		return false
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return false
	}
	defer func() { _ = tx.Rollback() }()
	probe := s.readView(tx)
	link := db.FrictionIssueLink{
		Fingerprint: "friction-capability-probe", Source: db.FrictionIssueSourceManual, LinkedAt: time.Now(),
	}
	err = probe.SaveFrictionIssueLink(ctx, link)
	if err == nil {
		err = probe.SaveFrictionIssueLink(ctx, link)
	}
	if err == nil {
		_, err = probe.AddFrictionIssueLink(ctx, link)
	}
	if err == nil {
		_, err = probe.DeleteFrictionIssueLink(ctx, link.Fingerprint)
	}
	if err != nil && !IsReadOnlyError(err) {
		log.Printf("friction: issue link capability probe: %v", err)
	}
	return err == nil
}

var _ db.FrictionIssueLinkWriter = (*Store)(nil)
