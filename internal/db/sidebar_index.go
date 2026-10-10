package db

import (
	"context"
	"fmt"
)

// ensureSidebarIndexLocked runs after column migrations, or on a fresh schema.
// The recursive sidebar query needs both predicates in its child lookup so
// SQLite does not scan all human sessions for each node in a large tree.
func ensureSidebarIndexLocked(ctx context.Context, w *writerHandle) error {
	_, err := w.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_sessions_parent_automated
		ON sessions(parent_session_id, is_automated)
		WHERE parent_session_id IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("creating sidebar parent and automation index: %w", err)
	}
	return nil
}
