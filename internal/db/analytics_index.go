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
		SELECT 1 FROM pragma_index_info('idx_messages_analytics_metadata')
		WHERE name = 'ordinal'
	)`).Scan(&exists); err != nil {
		return fmt.Errorf("checking analytics metadata index: %w", err)
	}
	if !exists {
		log.Print("building SQLite analytics metadata index; startup waits for the message scan to finish")
		if _, err := w.Exec(ctx, `DROP INDEX IF EXISTS idx_messages_analytics_metadata`); err != nil {
			return fmt.Errorf("dropping stale analytics metadata index: %w", err)
		}
		if _, err := w.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_messages_analytics_metadata
			ON messages(session_id, id, timestamp, model, ordinal)`); err != nil {
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
	// FTS5 deletes merged segments by rowid range; small estimates turn that
	// into a full-table scan. Use about SQLite's unanalyzed row estimate so
	// segment deletes stay indexed while an archive grows from empty.
	if _, err := w.Exec(ctx, `UPDATE sqlite_stat1 SET stat = '1000000'
		WHERE tbl GLOB '*_fts_data' AND idx IS NULL AND CAST(stat AS INTEGER) < 1000000`); err != nil {
		return fmt.Errorf("raising FTS segment row estimates: %w", err)
	}
	if _, err := w.Exec(ctx, `ANALYZE sqlite_schema`); err != nil {
		return fmt.Errorf("reloading archive query statistics: %w", err)
	}
	return nil
}
