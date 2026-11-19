package clickhouse

import (
	"context"
	"database/sql"
	"fmt"
)

// dialogueDedupedKey records that dedupeStoredDialogue scheduled its
// mutation, so EnsureSchemaOn schedules it once per mirror.
const dialogueDedupedKey = "agentsview_dialogue_deduplicated"

// dedupeStoredDialogue rewrites rows pushed before pushes applied
// db.StoredDialogue: a dialogue_text that repeats content on a message without
// thinking text becomes NULL, which search already reads as content.
// ClickHouse runs the mutation in the background, and searches return the
// same results before and after it.
//
// The SET expression must produce ” under the NULL. A Nullable column keeps
// its nested value under the null mask, and UPDATE ... = NULL keeps the old
// string there, so the column would not shrink. The else branch never applies
// to a row the WHERE selects; it only supplies that empty nested value.
func dedupeStoredDialogue(ctx context.Context, conn *sql.DB) error {
	meta, err := readMetadata(ctx, conn, dialogueDedupedKey)
	if err != nil {
		return err
	}
	if meta[dialogueDedupedKey] != "" {
		return nil
	}
	if _, err := conn.ExecContext(ctx, `ALTER TABLE messages
		UPDATE dialogue_text = if(dialogue_text = content, NULL, '')
		WHERE dialogue_text = content AND thinking_text = ''`); err != nil {
		return fmt.Errorf("scheduling clickhouse dialogue deduplication: %w", err)
	}
	return writeMetadata(ctx, conn, map[string]string{dialogueDedupedKey: "1"})
}
