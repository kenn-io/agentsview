package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	corerecall "go.kenn.io/agentsview/internal/recall"
)

const (
	RecallLexicalScorePolicyVersion = corerecall.LexicalScorePolicyVersion
	RecallVectorScorePolicyVersion  = "recall-vector-cosine-v1"
	RecallHybridScorePolicyVersion  = "recall-hybrid-rrf-v1"
	RecallQuerySurfaceQuery         = "query"
	RecallQuerySurfaceBrief         = "brief"
	RecallQuerySurfaceCalibration   = "calibration"
	recallExposureInsertBatchSize   = 100
)

// RecallQueryEvent is an append-only snapshot of one completed recall request.
// FiltersJSON is the caller's stable serialized filter contract; exposures
// retain ranked entry IDs and scores even if those entries are later deleted.
type RecallQueryEvent struct {
	QueryID            string                `json:"query_id"`
	Query              string                `json:"query"`
	Surface            string                `json:"surface"`
	FiltersJSON        string                `json:"filters_json"`
	TrustedOnly        bool                  `json:"trusted_only"`
	ScorePolicyVersion string                `json:"score_policy_version"`
	ResultCount        int                   `json:"result_count"`
	PackedCount        int                   `json:"packed_count"`
	TopScore           float64               `json:"top_score"`
	MissReason         string                `json:"miss_reason,omitempty"`
	CreatedAt          string                `json:"created_at"`
	Exposures          []RecallQueryExposure `json:"exposures,omitempty"`
}

// RecallQueryExposure records one ranked result as it appeared when the query
// completed. It intentionally has no foreign key to recall_entries.
type RecallQueryExposure struct {
	QueryID string  `json:"query_id"`
	Rank    int     `json:"rank"`
	EntryID string  `json:"entry_id"`
	Score   float64 `json:"score"`
	Packed  bool    `json:"packed"`
}

// RecordRecallQueryEvent inserts an event and all exposures atomically. An
// empty query ID receives a cryptographically random UUID.
func (db *DB) RecordRecallQueryEvent(
	ctx context.Context,
	event RecallQueryEvent,
) (string, error) {
	if err := db.requireWritable(); err != nil {
		return "", err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	event.QueryID = strings.TrimSpace(event.QueryID)
	event.Surface = strings.TrimSpace(event.Surface)
	event.ScorePolicyVersion = strings.TrimSpace(event.ScorePolicyVersion)
	if event.Surface == "" {
		return "", fmt.Errorf("recall query surface is required")
	}
	if event.FiltersJSON == "" {
		event.FiltersJSON = "{}"
	}
	if event.ScorePolicyVersion == "" {
		event.ScorePolicyVersion = RecallLexicalScorePolicyVersion
	}
	if event.QueryID == "" {
		id, err := newUUIDv4()
		if err != nil {
			return "", fmt.Errorf("generating recall query id: %w", err)
		}
		event.QueryID = id
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("beginning recall query event: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO recall_query_events (
			id, query_text, surface, filters_json, trusted_only,
			score_policy_version, result_count, packed_count,
			top_score, miss_reason
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.QueryID,
		event.Query,
		event.Surface,
		event.FiltersJSON,
		event.TrustedOnly,
		event.ScorePolicyVersion,
		event.ResultCount,
		event.PackedCount,
		event.TopScore,
		event.MissReason,
	); err != nil {
		return "", fmt.Errorf("inserting recall query event: %w", err)
	}
	for start := 0; start < len(event.Exposures); start += recallExposureInsertBatchSize {
		end := min(start+recallExposureInsertBatchSize, len(event.Exposures))
		batch := event.Exposures[start:end]
		if err := insertRecallQueryExposureBatch(
			ctx, tx, event.QueryID, batch,
		); err != nil {
			return "", fmt.Errorf(
				"inserting recall query exposure ranks %d through %d: %w",
				batch[0].Rank,
				batch[len(batch)-1].Rank,
				err,
			)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("committing recall query event: %w", err)
	}
	return event.QueryID, nil
}

func insertRecallQueryExposureBatch(
	ctx context.Context,
	tx *sql.Tx,
	queryID string,
	exposures []RecallQueryExposure,
) error {
	if len(exposures) == 0 {
		return nil
	}
	var query strings.Builder
	query.WriteString(`
		INSERT INTO recall_query_exposures (
			query_id, rank, entry_id, score, packed
		) VALUES `)
	args := make([]any, 0, len(exposures)*5)
	for i, exposure := range exposures {
		if i > 0 {
			query.WriteString(", ")
		}
		query.WriteString("(?, ?, ?, ?, ?)")
		args = append(
			args,
			queryID,
			exposure.Rank,
			exposure.EntryID,
			exposure.Score,
			exposure.Packed,
		)
	}
	_, err := tx.ExecContext(ctx, query.String(), args...)
	return err
}

// GetRecallQueryEvent returns one event with exposures ordered by rank. It is
// intentionally a concrete SQLite API rather than a server Store capability;
// calibration and proposal lookup can expose a narrower contract later.
func (db *DB) GetRecallQueryEvent(
	ctx context.Context,
	queryID string,
) (*RecallQueryEvent, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var event RecallQueryEvent
	err := db.getReader().QueryRowContext(ctx, `
		SELECT id, query_text, surface, filters_json, trusted_only,
		       score_policy_version, result_count, packed_count,
		       top_score, miss_reason, created_at
		FROM recall_query_events
		WHERE id = ?`,
		strings.TrimSpace(queryID),
	).Scan(
		&event.QueryID,
		&event.Query,
		&event.Surface,
		&event.FiltersJSON,
		&event.TrustedOnly,
		&event.ScorePolicyVersion,
		&event.ResultCount,
		&event.PackedCount,
		&event.TopScore,
		&event.MissReason,
		&event.CreatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("getting recall query event %s: %w", queryID, err)
	}
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT query_id, rank, entry_id, score, packed
		FROM recall_query_exposures
		WHERE query_id = ?
		ORDER BY rank ASC`,
		event.QueryID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying recall query exposures: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var exposure RecallQueryExposure
		if err := rows.Scan(
			&exposure.QueryID,
			&exposure.Rank,
			&exposure.EntryID,
			&exposure.Score,
			&exposure.Packed,
		); err != nil {
			return nil, fmt.Errorf("scanning recall query exposure: %w", err)
		}
		event.Exposures = append(event.Exposures, exposure)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading recall query exposures: %w", err)
	}
	return &event, nil
}

func copyRecallQueryEventsFromAttachedTx(
	ctx context.Context,
	tx *sql.Tx,
) error {
	eventsExist, err := attachedRecallTableExistsTx(
		ctx, tx, "recall_query_events",
	)
	if err != nil {
		return err
	}
	if !eventsExist {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO recall_query_events (
			id, query_text, surface, filters_json, trusted_only,
			score_policy_version, result_count, packed_count,
			top_score, miss_reason, created_at
		)
		SELECT
			id, query_text, surface, filters_json, trusted_only,
			score_policy_version, result_count, packed_count,
			top_score, miss_reason, created_at
		FROM old_db.recall_query_events`); err != nil {
		return fmt.Errorf("copying recall query events: %w", err)
	}
	exposuresExist, err := attachedRecallTableExistsTx(
		ctx, tx, "recall_query_exposures",
	)
	if err != nil {
		return err
	}
	if !exposuresExist {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO recall_query_exposures (
			query_id, rank, entry_id, score, packed
		)
		SELECT query_id, rank, entry_id, score, packed
		FROM old_db.recall_query_exposures
		WHERE query_id IN (SELECT id FROM main.recall_query_events)`); err != nil {
		return fmt.Errorf("copying recall query exposures: %w", err)
	}
	return nil
}

func attachedRecallTableExistsTx(
	ctx context.Context,
	tx *sql.Tx,
	table string,
) (bool, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM old_db.sqlite_master
			WHERE type = 'table' AND name = ?
		)`, table).Scan(&exists); err != nil {
		return false, fmt.Errorf(
			"checking source recall table %s: %w",
			table,
			err,
		)
	}
	return exists, nil
}
