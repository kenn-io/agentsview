package db

import (
	"context"
	"fmt"
	"log"
)

// ensureAnalyticsIndexLocked keeps model and tool-time lookups off message
// body pages. Call after opening the writer and after a deferred bulk build.
func ensureAnalyticsIndexLocked(ctx context.Context, w *writerHandle) error {
	var exists bool
	if err := w.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM sqlite_master WHERE type = 'index'
		AND name = 'idx_messages_analytics_metadata'
	)`).Scan(&exists); err != nil {
		return fmt.Errorf("checking analytics metadata index: %w", err)
	}
	if !exists {
		log.Print("building SQLite analytics metadata index; startup waits for the message scan to finish")
		if _, err := w.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_messages_analytics_metadata
			ON messages(session_id, id, timestamp, model)`); err != nil {
			return fmt.Errorf("creating analytics metadata index: %w", err)
		}
	}
	// Include tables not yet queried on this connection (0x10000), run
	// ANALYZE where needed (0x02), and limit its samples (0x10). Analyzing
	// only the new index can favor an un-analyzed, non-covering competitor.
	// SQLite skips tables whose statistics are already sufficiently current.
	if _, err := w.Exec(ctx, `PRAGMA optimize = 0x10012`); err != nil {
		return fmt.Errorf("optimizing archive query statistics: %w", err)
	}
	return nil
}
