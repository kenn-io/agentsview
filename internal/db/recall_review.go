package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	corerecall "go.kenn.io/agentsview/internal/recall"
)

type RecallReviewAction string

const (
	RecallReviewApprove RecallReviewAction = "approve"
	RecallReviewArchive RecallReviewAction = "archive"
)

var (
	ErrInvalidRecallReviewAction = errors.New("invalid recall review action")
	ErrRecallEntryNotFound       = errors.New("recall entry not found")
	ErrRecallReviewConflict      = errors.New("recall review conflict")
	ErrRecallReviewProvenance    = errors.New("recall provenance is revoked")
)

func (a RecallReviewAction) Validate() error {
	switch a {
	case RecallReviewApprove, RecallReviewArchive:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidRecallReviewAction, a)
	}
}

// ReviewRecallEntry records a terminal human decision for one automatic
// entry. The transition and returned representation share one transaction so
// callers never receive a failure after a decision was durably committed.
func (db *DB) ReviewRecallEntry(
	ctx context.Context,
	id string,
	action RecallReviewAction,
) (RecallEntry, error) {
	if err := db.requireWritable(); err != nil {
		return RecallEntry{}, err
	}
	id = strings.TrimSpace(id)
	action = RecallReviewAction(strings.TrimSpace(string(action)))
	if err := action.Validate(); err != nil {
		return RecallEntry{}, err
	}
	if id == "" {
		return RecallEntry{}, ErrRecallEntryNotFound
	}
	if ctx == nil {
		ctx = context.Background()
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return RecallEntry{}, fmt.Errorf("begin recall review: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var status, reviewState string
	var provenanceOK bool
	err = tx.QueryRowContext(ctx, `
		SELECT status, review_state, provenance_ok
		FROM recall_entries WHERE id = ?`, id,
	).Scan(&status, &reviewState, &provenanceOK)
	if errors.Is(err, sql.ErrNoRows) {
		return RecallEntry{}, fmt.Errorf("%w: %s", ErrRecallEntryNotFound, id)
	}
	if err != nil {
		return RecallEntry{}, fmt.Errorf("read recall review state: %w", err)
	}
	if status != corerecall.StatusAccepted ||
		reviewState != corerecall.ReviewStateUnreviewedAuto {
		return RecallEntry{}, fmt.Errorf(
			"%w: entry %s is %s/%s",
			ErrRecallReviewConflict, id, status, reviewState,
		)
	}
	if action == RecallReviewApprove && !provenanceOK {
		return RecallEntry{}, fmt.Errorf("%w: entry %s", ErrRecallReviewProvenance, id)
	}

	nextStatus := corerecall.StatusAccepted
	nextReview := corerecall.ReviewStateHumanReviewed
	guard := ""
	if action == RecallReviewArchive {
		nextStatus = corerecall.StatusArchived
		nextReview = corerecall.ReviewStateHumanRejected
	} else {
		guard = " AND provenance_ok != 0"
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE recall_entries
		SET status = ?, review_state = ?,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id = ? AND status = ? AND review_state = ?`+guard,
		nextStatus, nextReview, id, corerecall.StatusAccepted,
		corerecall.ReviewStateUnreviewedAuto,
	)
	if err != nil {
		return RecallEntry{}, fmt.Errorf("update recall review: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return RecallEntry{}, fmt.Errorf("count recall review update: %w", err)
	}
	if affected != 1 {
		return RecallEntry{}, fmt.Errorf(
			"%w: entry %s changed during review", ErrRecallReviewConflict, id,
		)
	}

	entry, err := scanRecallEntryRow(tx.QueryRowContext(ctx,
		"SELECT "+recallBaseCols+" FROM recall_entries WHERE id = ?", id))
	if err != nil {
		return RecallEntry{}, fmt.Errorf("read reviewed recall entry: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, entry_id, session_id, message_start_ordinal,
			message_end_ordinal, message_start_source_uuid,
			message_end_source_uuid, content_digest, tool_use_id, snippet
		FROM recall_evidence
		WHERE entry_id = ? ORDER BY id ASC`, id)
	if err != nil {
		return RecallEntry{}, fmt.Errorf("read reviewed recall evidence: %w", err)
	}
	for rows.Next() {
		evidence, scanErr := scanRecallEvidenceRow(rows)
		if scanErr != nil {
			_ = rows.Close()
			return RecallEntry{}, fmt.Errorf(
				"scan reviewed recall evidence: %w", scanErr)
		}
		entry.Evidence = append(entry.Evidence, evidence)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return RecallEntry{}, fmt.Errorf("read reviewed recall evidence: %w", err)
	}
	if err := rows.Close(); err != nil {
		return RecallEntry{}, fmt.Errorf("close reviewed recall evidence: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return RecallEntry{}, fmt.Errorf("commit recall review: %w", err)
	}
	return entry, nil
}
