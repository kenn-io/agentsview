package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// refreshDialogueFTS upgrades the derived index once, preserving every message.
func refreshDialogueFTS(ctx context.Context, w *writerHandle) error {
	var ddl string
	err := w.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE name = 'messages_fts'").Scan(&ddl)
	if errors.Is(err, sql.ErrNoRows) || err == nil && strings.Contains(ddl, "message_search_content") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading message FTS schema: %w", err)
	}
	tx, err := w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, query := range []string{
		"DROP TRIGGER IF EXISTS messages_ai", "DROP TRIGGER IF EXISTS messages_ad", "DROP TRIGGER IF EXISTS messages_au",
		"DROP TABLE messages_fts", schemaFTS, "INSERT INTO messages_fts(messages_fts) VALUES('rebuild')",
	} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("refreshing dialogue FTS: %w", err)
		}
	}
	return tx.Commit()
}
