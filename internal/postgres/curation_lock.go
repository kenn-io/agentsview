package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/uptrace/bun"
)

func lockPinnedMessagesSession(
	ctx context.Context, store bun.IDB, sessionID string,
) error {
	var lockedSessionID string
	err := store.NewRaw(`
		SELECT id
		FROM sessions
		WHERE id = ?
		FOR UPDATE`, sessionID).Scan(ctx, &lockedSessionID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf(
			"locking pg pins for session %s: %w", sessionID, err,
		)
	}
	return nil
}

func (*postgresBunBackend) LockCurationSession(
	ctx context.Context, store bun.IDB, sessionID string,
) error {
	return lockPinnedMessagesSession(ctx, store, sessionID)
}
