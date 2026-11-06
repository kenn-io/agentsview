package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// Every link and filing-state write runs in frictionLinkTx, which first
// locks the pattern's row. Writers for one pattern therefore run one at a
// time in the same order, and a write that checks for a person's link or
// pause sees the last committed change.

// SaveFrictionIssueLink records l, replacing any link for its pattern, and
// clears the pattern's filing state.
func (s *Store) SaveFrictionIssueLink(ctx context.Context, l db.FrictionIssueLink) error {
	return s.frictionLinkTx(ctx, l.Fingerprint, func(tx storeExecutor) error {
		_, err := tx.ExecContext(ctx, `
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
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM friction_filing_state WHERE fingerprint = $1`, l.Fingerprint); err != nil {
			return fmt.Errorf("clearing friction filing state: %w", err)
		}
		return nil
	})
}

// AddFrictionIssueLink records l unless its pattern is already linked,
// clearing the filing state when it does, and returns the stored link.
func (s *Store) AddFrictionIssueLink(ctx context.Context, l db.FrictionIssueLink) (db.FrictionIssueLink, error) {
	link, _, err := s.addFrictionIssueLink(ctx, l, false)
	return link, err
}

// AddFrictionIssueLinkUnlessPaused adds l like AddFrictionIssueLink unless
// a person paused the pattern, and reports whether the pattern is linked.
func (s *Store) AddFrictionIssueLinkUnlessPaused(ctx context.Context, l db.FrictionIssueLink) (db.FrictionIssueLink, bool, error) {
	return s.addFrictionIssueLink(ctx, l, true)
}

func (s *Store) addFrictionIssueLink(ctx context.Context, l db.FrictionIssueLink, respectPause bool) (db.FrictionIssueLink, bool, error) {
	stored := db.FrictionIssueLink{Fingerprint: l.Fingerprint}
	linked := false
	err := s.frictionLinkTx(ctx, l.Fingerprint, func(tx storeExecutor) error {
		if respectPause {
			var paused bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM friction_filing_state
				WHERE fingerprint = $1 AND state = $2)`, l.Fingerprint, db.FrictionFilingPaused).Scan(&paused); err != nil {
				return fmt.Errorf("reading friction filing state: %w", err)
			}
			if paused {
				return nil
			}
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO friction_issue_links (fingerprint, kata_instance_uid,
				kata_project_uid, issue_uid, qualified_id, web_url, source, linked_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (fingerprint) DO NOTHING`,
			l.Fingerprint, l.KataInstanceUID, l.KataProjectUID, l.IssueUID,
			l.QualifiedID, l.WebURL, l.Source, l.LinkedAt.UTC())
		if err != nil {
			return fmt.Errorf("adding friction issue link: %w", err)
		}
		added, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("adding friction issue link: %w", err)
		}
		if added > 0 {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM friction_filing_state WHERE fingerprint = $1`, l.Fingerprint); err != nil {
				return fmt.Errorf("clearing friction filing state: %w", err)
			}
		}
		if err := tx.QueryRowContext(ctx, `
			SELECT kata_instance_uid, kata_project_uid, issue_uid, qualified_id,
				web_url, source, linked_at
			FROM friction_issue_links WHERE fingerprint = $1`, l.Fingerprint).Scan(
			&stored.KataInstanceUID, &stored.KataProjectUID, &stored.IssueUID,
			&stored.QualifiedID, &stored.WebURL, &stored.Source, &stored.LinkedAt); err != nil {
			return fmt.Errorf("reading friction issue link: %w", err)
		}
		linked = true
		return nil
	})
	if err != nil || !linked {
		return db.FrictionIssueLink{}, false, err
	}
	stored.LinkedAt = stored.LinkedAt.UTC()
	return stored, true, nil
}

// SaveFrictionFilingState records st unless the pattern has a link or a
// person paused it.
func (s *Store) SaveFrictionFilingState(ctx context.Context, st db.FrictionFilingState) error {
	return s.frictionLinkTx(ctx, st.Fingerprint, func(tx storeExecutor) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO friction_filing_state (fingerprint, state, attempts,
				first_failed_at, next_attempt_at, last_error, updated_at)
			SELECT $1, $2, $3, $4, $5, $6, $7
			WHERE NOT EXISTS (SELECT 1 FROM friction_issue_links WHERE fingerprint = $1)
			ON CONFLICT (fingerprint) DO UPDATE SET
				state = EXCLUDED.state, attempts = EXCLUDED.attempts,
				first_failed_at = EXCLUDED.first_failed_at,
				next_attempt_at = EXCLUDED.next_attempt_at,
				last_error = EXCLUDED.last_error, updated_at = EXCLUDED.updated_at
			WHERE friction_filing_state.state <> $8`,
			st.Fingerprint, st.State, st.Attempts, utcPtr(st.FirstFailedAt),
			utcPtr(st.NextAttemptAt), st.LastError, st.UpdatedAt.UTC(), db.FrictionFilingPaused)
		if err != nil {
			return fmt.Errorf("saving friction filing state: %w", err)
		}
		return nil
	})
}

func utcPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

// DeleteFrictionIssueLink removes the local link and pauses automatic
// filing for the pattern, which would otherwise link the same issue again.
// Both happen in one transaction. The Kata issue stays.
func (s *Store) DeleteFrictionIssueLink(ctx context.Context, fingerprint string) (bool, error) {
	removed := false
	err := s.frictionLinkTx(ctx, fingerprint, func(tx storeExecutor) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM friction_issue_links WHERE fingerprint = $1`, fingerprint)
		if err != nil {
			return fmt.Errorf("deleting friction issue link: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("deleting friction issue link: %w", err)
		}
		if n == 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO friction_filing_state (fingerprint, state, attempts, last_error, updated_at)
			VALUES ($1, $2, 0, '', $3)
			ON CONFLICT (fingerprint) DO UPDATE SET
				state = EXCLUDED.state, attempts = 0, first_failed_at = NULL,
				next_attempt_at = NULL, last_error = '', updated_at = EXCLUDED.updated_at`,
			fingerprint, db.FrictionFilingPaused, time.Now().UTC()); err != nil {
			return fmt.Errorf("pausing friction filing: %w", err)
		}
		removed = true
		return nil
	})
	return removed, err
}

// frictionLinkTx runs fn in one transaction after locking the pattern's
// row: the store's own transaction when it is a transaction view, as the
// write probe's is, or a new one.
func (s *Store) frictionLinkTx(ctx context.Context, fingerprint string, fn func(tx storeExecutor) error) error {
	run := func(tx storeExecutor) error {
		if _, err := tx.ExecContext(ctx,
			`SELECT 1 FROM friction_patterns WHERE fingerprint = $1 FOR UPDATE`, fingerprint); err != nil {
			return fmt.Errorf("locking friction pattern: %w", err)
		}
		return fn(tx)
	}
	if s.readTx != nil {
		return run(s.readTx)
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("saving friction issue link: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := run(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// frictionLinkColumns and frictionFilingColumns are what a pattern read
// selects through its LEFT JOINs; absent rows read as NULL.
const (
	frictionLinkColumns = `l.kata_instance_uid, l.kata_project_uid,
	l.issue_uid, l.qualified_id, l.web_url, l.source, l.linked_at`
	frictionFilingColumns = `f.state, f.attempts, f.first_failed_at,
	f.next_attempt_at, f.last_error, f.updated_at`
)

// frictionIssueSelect returns the link and filing-state columns and joins a
// pattern read adds. A read-only role reading a schema that predates either
// table cannot create it, so its pattern reads show that table's fields as
// empty instead of failing. Each table is looked up until a read finds it,
// so a later migration takes effect without a restart.
func (s *Store) frictionIssueSelect(ctx context.Context) (columns, join string) {
	rt := s.runtimeState()
	link, linkJoin := frictionLinkColumns, `LEFT JOIN friction_issue_links l ON l.fingerprint = p.fingerprint`
	if !s.frictionTablePresent(ctx, &rt.frictionIssueLinksPresent, "friction_issue_links") {
		link, linkJoin = `NULL, NULL, NULL, NULL, NULL, NULL, NULL`, ""
	}
	filing, filingJoin := frictionFilingColumns, `LEFT JOIN friction_filing_state f ON f.fingerprint = p.fingerprint`
	if !s.frictionTablePresent(ctx, &rt.frictionFilingStatePresent, "friction_filing_state") {
		filing, filingJoin = `NULL, NULL, NULL, NULL, NULL, NULL`, ""
	}
	return link + ", " + filing, linkJoin + " " + filingJoin
}

// frictionTablePresent reports whether table exists, remembering a yes in
// present.
func (s *Store) frictionTablePresent(ctx context.Context, present *atomic.Bool, table string) bool {
	if present.Load() {
		return true
	}
	if !pgHasTable(ctx, s.pg, table) {
		return false
	}
	present.Store(true)
	return true
}

type frictionIssueScan struct {
	instance, project, issue, qualified, url, source sql.NullString
	linkedAt                                         sql.NullTime

	state, lastError                  sql.NullString
	attempts                          sql.NullInt64
	firstFailed, nextAttempt, updated sql.NullTime
}

func (s *frictionIssueScan) targets() []any {
	return []any{
		&s.instance, &s.project, &s.issue, &s.qualified, &s.url, &s.source, &s.linkedAt,
		&s.state, &s.attempts, &s.firstFailed, &s.nextAttempt, &s.lastError, &s.updated,
	}
}

func (s *frictionIssueScan) filing(fingerprint string) *db.FrictionFilingState {
	if !s.state.Valid {
		return nil
	}
	return &db.FrictionFilingState{
		Fingerprint: fingerprint, State: s.state.String,
		Attempts: int(s.attempts.Int64), LastError: s.lastError.String,
		FirstFailedAt: nullTimePtr(s.firstFailed), NextAttemptAt: nullTimePtr(s.nextAttempt),
		UpdatedAt: s.updated.Time.UTC(),
	}
}

func nullTimePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	return new(t.Time.UTC())
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
// write Kata issue links and automatic filing state. The filing routes refuse before contacting Kata
// when it cannot, so no issue is created without its link.
func (s *Store) FrictionIssueLinksWritable() bool {
	return s.runtimeState().frictionIssueLinksWritable.Load()
}

// probeFrictionIssueLinkWrite runs every link and filing-state write, the
// replacing save, the insert-only save, the filing-state save and the
// delete, inside a rolled-back transaction. A missing table or a role
// lacking INSERT, UPDATE or DELETE reports false.
func (s *Store) probeFrictionIssueLinkWrite(ctx context.Context) bool {
	if !pgHasTable(ctx, s.pg, "friction_issue_links") || !pgHasTable(ctx, s.pg, "friction_filing_state") {
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
		err = probe.SaveFrictionFilingState(ctx, db.FrictionFilingState{
			Fingerprint: link.Fingerprint, State: db.FrictionFilingPaused, UpdatedAt: link.LinkedAt,
		})
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
