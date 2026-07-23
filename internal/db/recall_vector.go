package db

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

const (
	recallEmbeddingWatermarkStatus = "__watermark__"
	recallCorpusRevisionPrefix     = "counter-v1:"
)

// RecallCorpusRevision returns a stable source revision for freshness checks.
// Current archives maintain a monotonic counter through recall_entries
// triggers. Read-only archives created before that schema addition fall back
// to their indexed timestamp watermarks so explicit builds remain usable.
func (db *DB) RecallCorpusRevision(ctx context.Context) (string, error) {
	hasState, err := db.recallTableExists(ctx, "recall_corpus_state")
	if err != nil {
		return "", err
	}
	if hasState {
		var revision int64
		if err := db.getReader().QueryRowContext(ctx, `
			SELECT revision FROM recall_corpus_state WHERE singleton = 1
		`).Scan(&revision); err != nil {
			return "", fmt.Errorf("reading recall corpus revision: %w", err)
		}
		return recallCorpusRevisionPrefix + strconv.FormatInt(revision, 10), nil
	}

	var maxUpdated string
	if err := db.getReader().QueryRowContext(ctx, `
		SELECT COALESCE((
			SELECT updated_at
			FROM recall_entries INDEXED BY idx_recall_entries_updated
			ORDER BY updated_at DESC, id ASC
			LIMIT 1
		), '')
	`).Scan(&maxUpdated); err != nil {
		return "", fmt.Errorf("reading legacy recall corpus revision: %w", err)
	}
	hasDeletions, err := db.recallTableExists(ctx, "recall_embedding_deletions")
	if err != nil {
		return "", err
	}
	if hasDeletions {
		var maxDeleted string
		if err := db.getReader().QueryRowContext(ctx, `
			SELECT COALESCE((
				SELECT deleted_at
				FROM recall_embedding_deletions
					INDEXED BY idx_recall_embedding_deletions_updated
				ORDER BY deleted_at DESC, entry_id ASC
				LIMIT 1
			), '')
		`).Scan(&maxDeleted); err != nil {
			return "", fmt.Errorf("reading legacy recall deletion revision: %w", err)
		}
		if endedAfter(maxDeleted, maxUpdated) {
			maxUpdated = maxDeleted
		}
	}
	return "watermark-v1:" + maxUpdated, nil
}

func (db *DB) recallTableExists(ctx context.Context, name string) (bool, error) {
	var exists bool
	if err := db.getReader().QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?
		)`, name).Scan(&exists); err != nil {
		return false, fmt.Errorf("checking recall table %s: %w", name, err)
	}
	return exists, nil
}

// ScanRecallEmbeddingUnits streams the complete served accepted-entry corpus
// as one embedding document per entry. SessionID and SourceUUID both carry the
// entry ID so the generic vector mirror can retain its stable document identity
// and return the entry ID from semantic hits.
func (db *DB) ScanRecallEmbeddingUnits(
	ctx context.Context,
	since string,
	fn func(EmbeddableUnit) error,
) (maxUpdated string, err error) {
	hasChanges, err := db.recallTableExists(ctx, "recall_embedding_changes")
	if err != nil {
		return "", err
	}
	if hasChanges {
		if since == "" {
			return db.scanRecallEmbeddingRevision(ctx, 0, true, fn)
		}
		if revision, ok := parseRecallCorpusRevision(since); ok {
			return db.scanRecallEmbeddingRevision(ctx, revision, false, fn)
		}
		// A vectors.db created before revision watermarks may still carry an
		// RFC3339 cursor. Reconcile every surviving identity once, including
		// tombstones, then advance it into the monotonic revision domain.
		return db.scanRecallEmbeddingCompatibility(ctx, fn)
	}
	if strings.HasPrefix(since, recallCorpusRevisionPrefix) {
		// Transitional read-only archives can have the corpus counter from an
		// earlier binary but not the compact change table. A complete delta is
		// the only safe way to avoid blessing skipped backdated mutations.
		return db.scanRecallEmbeddingCompatibility(ctx, fn)
	}

	includeDeletions, err := db.recallTableExists(ctx, "recall_embedding_deletions")
	if err != nil {
		return "", err
	}
	query, args := recallEmbeddingScanSQL(since, includeDeletions)

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return "", fmt.Errorf("scanning recall embedding units: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, title, body, trigger, status, updatedAt string
		if err := rows.Scan(
			&id, &title, &body, &trigger, &status, &updatedAt,
		); err != nil {
			return "", fmt.Errorf("scanning recall embedding unit: %w", err)
		}
		if endedAfter(updatedAt, maxUpdated) {
			maxUpdated = updatedAt
		}
		if status == recallEmbeddingWatermarkStatus {
			continue
		}
		content := title + "\n\n" + body
		if trigger != "" {
			content += "\n\n" + trigger
		}
		if err := fn(EmbeddableUnit{
			SessionID:  id,
			Kind:       "user",
			SourceUUID: id,
			Deleted:    status != "accepted",
			Content:    content,
		}); err != nil {
			return "", err
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterating recall embedding units: %w", err)
	}
	return maxUpdated, nil
}

func parseRecallCorpusRevision(value string) (int64, bool) {
	if !strings.HasPrefix(value, recallCorpusRevisionPrefix) {
		return 0, false
	}
	revision, err := strconv.ParseInt(
		strings.TrimPrefix(value, recallCorpusRevisionPrefix), 10, 64,
	)
	return revision, err == nil && revision >= 0
}

func (db *DB) scanRecallEmbeddingRevision(
	ctx context.Context, since int64, full bool,
	fn func(EmbeddableUnit) error,
) (string, error) {
	query, args := recallEmbeddingRevisionScanSQL(since, full)
	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return "", fmt.Errorf("scanning recall embedding revisions: %w", err)
	}
	defer rows.Close()
	var maxRevision int64
	for rows.Next() {
		var id, title, body, trigger, status string
		var revision int64
		if err := rows.Scan(
			&id, &title, &body, &trigger, &status, &revision,
		); err != nil {
			return "", fmt.Errorf("scanning recall embedding revision: %w", err)
		}
		if revision > maxRevision {
			maxRevision = revision
		}
		if status == recallEmbeddingWatermarkStatus {
			continue
		}
		if err := emitRecallEmbeddingUnit(fn, id, title, body, trigger, status); err != nil {
			return "", err
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterating recall embedding revisions: %w", err)
	}
	return recallCorpusRevisionPrefix + strconv.FormatInt(maxRevision, 10), nil
}

func recallEmbeddingRevisionScanSQL(since int64, full bool) (string, []any) {
	if full {
		return `
			WITH current_revision AS (
				SELECT revision FROM recall_corpus_state WHERE singleton = 1
			)
			SELECT id, title, body, trigger, status, revision
			FROM recall_entries CROSS JOIN current_revision
			WHERE status = 'accepted'
			UNION ALL
			SELECT '', '', '', '', '` + recallEmbeddingWatermarkStatus + `', revision
			FROM current_revision
			ORDER BY id ASC`, nil
	}
	return `
		WITH current_revision AS (
			SELECT revision FROM recall_corpus_state WHERE singleton = 1
		), changed AS (
			SELECT changes.entry_id, changes.revision
			FROM recall_embedding_changes changes CROSS JOIN current_revision
			WHERE changes.revision > ?
			  AND changes.revision <= current_revision.revision
		)
		SELECT changed.entry_id,
			COALESCE(entry.title, ''), COALESCE(entry.body, ''),
			COALESCE(entry.trigger, ''), COALESCE(entry.status, 'deleted'),
			changed.revision
		FROM changed
		LEFT JOIN recall_entries entry ON entry.id = changed.entry_id
		UNION ALL
		SELECT '', '', '', '', '` + recallEmbeddingWatermarkStatus + `', revision
		FROM current_revision
		ORDER BY revision ASC, entry_id ASC`, []any{since}
}

func (db *DB) scanRecallEmbeddingCompatibility(
	ctx context.Context, fn func(EmbeddableUnit) error,
) (string, error) {
	hasState, err := db.recallTableExists(ctx, "recall_corpus_state")
	if err != nil {
		return "", err
	}
	if !hasState {
		return "", fmt.Errorf("recall corpus revision state is unavailable")
	}
	hasDeletions, err := db.recallTableExists(ctx, "recall_embedding_deletions")
	if err != nil {
		return "", err
	}
	query := `
		WITH current_revision AS (
			SELECT revision FROM recall_corpus_state WHERE singleton = 1
		)
		SELECT id, title, body, trigger, status, revision
		FROM recall_entries CROSS JOIN current_revision`
	if hasDeletions {
		query += `
		UNION ALL
		SELECT entry_id, '', '', '', 'deleted', revision
		FROM recall_embedding_deletions CROSS JOIN current_revision`
	}
	query += `
		UNION ALL
		SELECT '', '', '', '', '` + recallEmbeddingWatermarkStatus + `', revision
		FROM current_revision
		ORDER BY id ASC`
	rows, err := db.getReader().QueryContext(ctx, query)
	if err != nil {
		return "", fmt.Errorf("scanning recall embedding compatibility delta: %w", err)
	}
	defer rows.Close()
	var revision int64
	for rows.Next() {
		var id, title, body, trigger, status string
		if err := rows.Scan(&id, &title, &body, &trigger, &status, &revision); err != nil {
			return "", fmt.Errorf("scanning recall embedding compatibility row: %w", err)
		}
		if status == recallEmbeddingWatermarkStatus {
			continue
		}
		if err := emitRecallEmbeddingUnit(fn, id, title, body, trigger, status); err != nil {
			return "", err
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterating recall embedding compatibility delta: %w", err)
	}
	return recallCorpusRevisionPrefix + strconv.FormatInt(revision, 10), nil
}

func emitRecallEmbeddingUnit(
	fn func(EmbeddableUnit) error,
	id, title, body, trigger, status string,
) error {
	content := title + "\n\n" + body
	if trigger != "" {
		content += "\n\n" + trigger
	}
	return fn(EmbeddableUnit{
		SessionID:  id,
		Kind:       "user",
		SourceUUID: id,
		Deleted:    status != "accepted",
		Content:    content,
	})
}

func recallEmbeddingScanSQL(since string, includeDeletions bool) (string, []any) {
	query := `
		SELECT id, title, body, trigger, status, updated_at
		FROM recall_entries`
	var args []any
	if since != "" {
		query += " INDEXED BY idx_recall_entries_updated WHERE updated_at >= ?"
		args = append(args, since)
		if includeDeletions {
			query += `
			UNION ALL
			SELECT entry_id, '', '', '', 'deleted', deleted_at
			FROM recall_embedding_deletions
				INDEXED BY idx_recall_embedding_deletions_updated
			WHERE deleted_at >= ?`
			args = append(args, since)
		}
		return `SELECT id, title, body, trigger, status, updated_at FROM (` + query +
			") ORDER BY updated_at DESC, id ASC", args
	} else {
		watermarkSQL := `COALESCE((
			SELECT updated_at
			FROM recall_entries INDEXED BY idx_recall_entries_updated
			ORDER BY updated_at DESC, id ASC
			LIMIT 1
		), '')`
		if includeDeletions {
			watermarkSQL = `MAX(` + watermarkSQL + `,
				COALESCE((
					SELECT deleted_at
					FROM recall_embedding_deletions
						INDEXED BY idx_recall_embedding_deletions_updated
					ORDER BY deleted_at DESC, entry_id ASC
					LIMIT 1
				), '')
			)`
		}
		return `
			SELECT id, title, body, trigger, status, updated_at
			FROM recall_entries
			WHERE status = 'accepted'
			UNION ALL
			SELECT '', '', '', '', '` + recallEmbeddingWatermarkStatus + `',
				` + watermarkSQL + `
			ORDER BY id ASC`, nil
	}
}
