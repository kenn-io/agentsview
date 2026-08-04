package db

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

// SessionReplicationSnapshot is one consistent archive view of a session and
// every dependent row copied by PostgreSQL and DuckDB replication.
type SessionReplicationSnapshot struct {
	Session        Session
	Messages       []Message
	UsageEvents    []UsageEvent
	SecretFindings []SecretFinding
	PinnedMessages []PinnedMessage
}

// ReadSessionReplicationSnapshot loads one replication unit inside a single
// SQLite read transaction so a target cannot commit rows from mixed source
// revisions.
func (db *DB) ReadSessionReplicationSnapshot(
	ctx context.Context, sessionID string,
) (SessionReplicationSnapshot, error) {
	var snapshot SessionReplicationSnapshot
	err := db.consistentView(ctx, func(store bun.IDB) error {
		session, err := db.getSessionFrom(ctx, store, sessionID, true)
		if err != nil {
			return fmt.Errorf("reading replication session: %w", err)
		}
		if session == nil {
			return fmt.Errorf("reading replication session %s: not found", sessionID)
		}
		snapshot.Session = *session

		messageRows, err := scanBunMessages(ctx, store.NewSelect().
			Model((*bunmodel.Message)(nil)).Where("session_id = ?", sessionID).
			OrderExpr("ordinal ASC"))
		if err != nil {
			return fmt.Errorf("reading replication messages: %w", err)
		}
		if err := attachBunToolData(ctx, store, messageRows); err != nil {
			return err
		}
		snapshot.Messages = messageRows

		snapshot.UsageEvents, err = usageEventsWithQuerier(ctx, store, sessionID, 0)
		if err != nil {
			return err
		}
		snapshot.SecretFindings, err = sessionSecretFindingsWithQuerier(
			ctx, store, sessionID,
		)
		if err != nil {
			return err
		}
		snapshot.PinnedMessages, err = listPinnedMessagesWithStore(
			ctx, store, sessionID, "",
		)
		return err
	})
	if err != nil {
		return SessionReplicationSnapshot{}, err
	}
	return snapshot, nil
}
