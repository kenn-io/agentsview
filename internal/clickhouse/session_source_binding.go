package clickhouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"go.kenn.io/agentsview/internal/db"
)

var _ db.SessionSourceBinder = (*Store)(nil)

// errEvidenceUnpublished means the readable messages, tool calls or results don't all belong to the push that published the session row, so its revision doesn't describe them.
var errEvidenceUnpublished = errors.New("session evidence does not match its published session row")

// SessionSourceChanged reports a read that caught a push or deletion partway through.
func (s *Store) SessionSourceChanged(err error) bool {
	return errors.Is(err, errEvidenceUnpublished)
}

// SessionSourceBinding names the push that published the visible session row
// and refuses while any evidence row comes from another push, or while the
// row counts messages that are no longer readable.
func (s *Store) SessionSourceBinding(ctx context.Context, id string) (string, error) {
	var published uint64
	var messageCount int
	err := s.queryRowContext(ctx,
		"SELECT push_version, message_count FROM sessions PREWHERE id = ? WHERE deleted_at IS NULL", id,
	).Scan(&published, &messageCount)
	if errors.Is(err, sql.ErrNoRows) {
		// No visible row to read; the route answers not found.
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading clickhouse session publication: %w", err)
	}
	rows, err := s.queryContext(ctx, `
		SELECT 'messages', count(), min(push_version), max(push_version) FROM messages WHERE session_id = ?
		UNION ALL
		SELECT 'tool_calls', count(), min(push_version), max(push_version) FROM tool_calls WHERE session_id = ?
		UNION ALL
		SELECT 'tool_result_events', count(), min(push_version), max(push_version) FROM tool_result_events WHERE session_id = ?`,
		id, id, id)
	if err != nil {
		return "", fmt.Errorf("reading clickhouse session evidence versions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table string
		var count, low, high uint64
		if err := rows.Scan(&table, &count, &low, &high); err != nil {
			return "", fmt.Errorf("scanning clickhouse session evidence versions: %w", err)
		}
		if count > 0 && (low != published || high != published) {
			return "", fmt.Errorf("%w: %s", errEvidenceUnpublished, table)
		}
		// Messages deleted ahead of their session row, or replaced by an empty push that never published.
		if table == "messages" && count == 0 && messageCount > 0 {
			return "", fmt.Errorf("%w: messages missing", errEvidenceUnpublished)
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterating clickhouse session evidence versions: %w", err)
	}
	return strconv.FormatUint(published, 10), nil
}
