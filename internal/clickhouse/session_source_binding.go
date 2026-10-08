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
// and refuses while any evidence row comes from another push, or while fewer
// messages are readable than that push stored. A row with no stored count
// reports db.ErrSessionRevisionUnavailable, since no retry can help it.
func (s *Store) SessionSourceBinding(ctx context.Context, id string) (string, error) {
	var published uint64
	var stored sql.NullInt64
	// message_count can exceed the stored rows (metadata-only sessions, usage-only archives), so compare against what the push stored.
	err := s.queryRowContext(ctx,
		"SELECT push_version, stored_message_count FROM sessions PREWHERE id = ? WHERE deleted_at IS NULL", id,
	).Scan(&published, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		// No visible row to read; the route answers not found.
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading clickhouse session publication: %w", err)
	}
	if !stored.Valid {
		// Rows pushed before the count existed say nothing about their evidence until a push republishes them.
		return "", fmt.Errorf("%w until the next clickhouse push republishes this session", db.ErrSessionRevisionUnavailable)
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
		// A different message count means a deletion or an unpublished replacement is partway through.
		if table == "messages" && count != uint64(stored.Int64) {
			return "", fmt.Errorf("%w: messages missing", errEvidenceUnpublished)
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterating clickhouse session evidence versions: %w", err)
	}
	return strconv.FormatUint(published, 10), nil
}
