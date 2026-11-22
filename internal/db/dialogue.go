package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"time"
)

// The same corpus version invalidates ordinary FTS and the CJK fingerprint.
const (
	searchCorpusVersion         = "1"
	searchCorpusVersionStatsKey = "message_search_corpus_version"
)

// refreshDialogueFTS upgrades the derived index once, preserving every message.
func refreshDialogueFTS(ctx context.Context, w *writerHandle) (bool, error) {
	var version string
	err := w.QueryRowContext(ctx, "SELECT CAST(value AS TEXT) FROM stats WHERE key = ?", searchCorpusVersionStatsKey).Scan(&version)
	if err == nil {
		stored, parseErr := strconv.Atoi(version)
		current, _ := strconv.Atoi(searchCorpusVersion)
		if parseErr == nil && stored > current {
			return false, nil
		}
		if parseErr == nil && stored == current {
			// Upgrade trigger behavior without rebuilding an already-current index.
			tx, err := w.BeginTx(ctx, nil)
			if err != nil {
				return false, err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.ExecContext(ctx, "DROP TRIGGER IF EXISTS messages_au; DROP VIEW IF EXISTS message_search_content;"+schemaFTS); err != nil {
				return false, fmt.Errorf("refreshing message update FTS trigger: %w", err)
			}
			return false, tx.Commit()
		}
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("reading search corpus version: %w", err)
	}
	return true, nil
}

func rebuildMessageFTS(ctx context.Context, w *writerHandle, progress OpenProgressFunc, rotateToken bool) error {
	tx, err := w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	progress.report("Updating full-text search")
	for _, query := range slices.Concat(messageFTSDrops, []string{"DROP VIEW IF EXISTS message_search_content", schemaFTS, "INSERT INTO messages_fts(messages_fts) VALUES('rebuild')"}) {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("refreshing dialogue FTS: %w", err)
		}
	}
	if rotateToken {
		if _, err := tx.ExecContext(ctx, searchTokenSQL); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO stats(key, value) VALUES(?, ?)
  ON CONFLICT(key) DO UPDATE SET value = excluded.value`, searchCorpusVersionStatsKey, searchCorpusVersion); err != nil {
		return fmt.Errorf("storing search corpus version: %w", err)
	}
	return tx.Commit()
}

var messageFTSDrops = []string{
	"DROP TRIGGER IF EXISTS messages_ai", "DROP TRIGGER IF EXISTS messages_ad", "DROP TRIGGER IF EXISTS messages_au",
	"DROP TRIGGER IF EXISTS image_text_ai", "DROP TRIGGER IF EXISTS image_text_ad", "DROP TRIGGER IF EXISTS image_text_au",
	"DROP TABLE IF EXISTS messages_fts",
}

func prepareMessageFTSRepair(ctx context.Context, w *writerHandle) error {
	tx, err := w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, query := range slices.Concat(messageFTSDrops, []string{"DROP VIEW IF EXISTS message_search_content", schemaMessageSearchView}) {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("preparing message FTS repair: %w", err)
		}
	}
	return tx.Commit()
}

// RepairMessageFTS rebuilds a deferred standard message index. Writers wait while it holds db.mu.
func (db *DB) RepairMessageFTS(ctx context.Context) error {
	if !db.messageFTSRepairPending.Load() {
		return nil
	}
	_, err := RetryAcrossHandoff(ctx, db, func() (struct{}, error) {
		if err := db.lockWriterCtx(ctx); err != nil {
			return struct{}{}, err
		}
		defer db.mu.Unlock()
		if db.writerClosed.Load() {
			return struct{}{}, ErrWriterClosed
		}
		w := db.getWriter()
		var exists bool
		if err := w.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='messages_fts')`).Scan(&exists); err != nil {
			return struct{}{}, err
		}
		if !exists {
			start := time.Now()
			log.Print("rebuilding message search index")
			if err := rebuildMessageFTS(ctx, w, nil, true); err != nil {
				return struct{}{}, err
			}
			log.Printf("rebuilt message search index in %s", time.Since(start))
		}
		db.messageFTSRepairPending.Store(false)
		return struct{}{}, nil
	})
	return err
}
