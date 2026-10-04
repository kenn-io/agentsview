package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"go.kenn.io/agentsview/internal/db"
)

var _ db.SessionSourceBinder = (*Store)(nil)

// errEvidenceUnpublished means a push has written a session's messages but not yet its session row, so the row's revision doesn't describe them.
var errEvidenceUnpublished = errors.New("session evidence is newer than its published session row")

// SessionSourceChanged reports a read that caught a push between its dependent rows and its session row.
func (s *Store) SessionSourceChanged(err error) bool {
	return errors.Is(err, errEvidenceUnpublished)
}

// SessionSourceBinding names the push that published the session row, and
// refuses while messages, tool calls or results from a later push are readable.
func (s *Store) SessionSourceBinding(ctx context.Context, id string) (string, error) {
	var published, evidence uint64
	err := s.queryRowContext(ctx, `
		SELECT
			(SELECT max(push_version) FROM sessions WHERE id = ?),
			greatest(
				(SELECT max(push_version) FROM messages WHERE session_id = ?),
				(SELECT max(push_version) FROM tool_calls WHERE session_id = ?),
				(SELECT max(push_version) FROM tool_result_events WHERE session_id = ?))`,
		id, id, id, id).Scan(&published, &evidence)
	if err != nil {
		return "", fmt.Errorf("reading clickhouse session publication: %w", err)
	}
	// A session that never published has no row to read; the route answers not found.
	if published == 0 {
		return "", nil
	}
	if evidence > published {
		return "", errEvidenceUnpublished
	}
	return strconv.FormatUint(published, 10), nil
}
