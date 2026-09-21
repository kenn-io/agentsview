package clickhouse

import (
	"context"
	"database/sql"
	"fmt"
	"log"
)

// Session publication follows dependent inserts and removal of older versions.
// Recompute the maximum there so corrections and empty replacements can lower it.
//
// A materialized view runs inside the insert and reads the joined table in
// full. Inside the view, the inner sessions reference is the inserted block, so
// the filter keeps each publish to its own sessions' events rather than every
// event in the mirror.
const terminalEventSnapshotSelect = `SELECT s.id AS session_id, s.push_version AS push_version,
 max(tre.timestamp) AS last_terminal_at, %s AS revision
 FROM sessions s LEFT JOIN (
  SELECT session_id, push_version, timestamp FROM tool_result_events
  WHERE session_id IN (SELECT id FROM sessions)
  AND source = 'tool_execution' AND status IN ('completed', 'errored')
 ) tre ON tre.session_id = s.id AND tre.push_version = s.push_version
 GROUP BY s.id, s.push_version`

func ensureTerminalEventSnapshots(ctx context.Context, conn *sql.DB) error {
	const marker = "terminal_event_snapshots_backfill"
	metadata, err := readMetadata(ctx, conn, marker)
	if err != nil {
		return err
	}
	if metadata[marker] == "1" {
		return nil
	}
	queries := []string{
		`CREATE TABLE IF NOT EXISTS terminal_event_snapshots (
   session_id String, push_version UInt64,
   last_terminal_at Nullable(DateTime64(6, 'UTC')), revision UInt128)
   ENGINE = ReplacingMergeTree(revision) ORDER BY session_id
   SETTINGS index_granularity = 64`,
		"CREATE MATERIALIZED VIEW IF NOT EXISTS terminal_event_snapshots_mv TO terminal_event_snapshots AS " +
			fmt.Sprintf(terminalEventSnapshotSelect, "toUInt128(s.push_version) * 2 + 1"),
	}
	for _, query := range queries {
		if _, err := conn.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("creating terminal event snapshots: %w", err)
		}
	}
	// Live inserts win over an older backfill of the same published version.
	log.Print("ClickHouse: filling terminal event snapshots before serving; source events remain unchanged")
	if _, err := conn.ExecContext(ctx, "INSERT INTO terminal_event_snapshots "+
		fmt.Sprintf(terminalEventSnapshotSelect, "toUInt128(s.push_version) * 2")); err != nil {
		return fmt.Errorf("backfilling terminal event snapshots (startup can retry): %w", err)
	}
	return writeMetadata(ctx, conn, map[string]string{marker: "1"})
}
