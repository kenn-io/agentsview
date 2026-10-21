package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// The same corpus version invalidates ordinary FTS and the CJK fingerprint.
const (
	searchCorpusVersion         = "1"
	searchCorpusVersionStatsKey = "message_search_corpus_version"
)

// refreshDialogueFTS upgrades the derived index once, preserving every message.
func refreshDialogueFTS(ctx context.Context, w *writerHandle, progress OpenProgressFunc) error {
	var version string
	err := w.QueryRowContext(ctx, "SELECT CAST(value AS TEXT) FROM stats WHERE key = ?", searchCorpusVersionStatsKey).Scan(&version)
	if err == nil {
		stored, parseErr := strconv.Atoi(version)
		current, _ := strconv.Atoi(searchCorpusVersion)
		if parseErr == nil && stored >= current {
			return nil
		}
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading search corpus version: %w", err)
	}
	return rebuildMessageFTS(ctx, w, progress)
}

func rebuildMessageFTS(ctx context.Context, w *writerHandle, progress OpenProgressFunc) error {
	tx, err := w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	progress.report("Updating full-text search")
	for _, query := range []string{
		"DROP TRIGGER IF EXISTS messages_ai", "DROP TRIGGER IF EXISTS messages_ad", "DROP TRIGGER IF EXISTS messages_au",
		"DROP TABLE IF EXISTS messages_fts", "DROP VIEW IF EXISTS message_search_content", schemaFTS,
		"INSERT INTO messages_fts(messages_fts) VALUES('rebuild')",
	} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("refreshing dialogue FTS: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO stats(key, value) VALUES(?, ?)
  ON CONFLICT(key) DO UPDATE SET value = excluded.value`, searchCorpusVersionStatsKey, searchCorpusVersion); err != nil {
		return fmt.Errorf("storing search corpus version: %w", err)
	}
	return tx.Commit()
}
