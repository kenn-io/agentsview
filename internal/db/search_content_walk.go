package db

import (
	"context"
	"encoding/json/v2"
	"fmt"
)

// substringWalkFirstBatch is how many messages the sessions of a substring
// walk's first batch hold. Each later batch holds twice as many, so a search
// whose matches sit in recent sessions reads little, and one that must read
// every session runs only a few more statements than a single scan.
const substringWalkFirstBatch = 16384

// walkContentSessions calls visit with batches of the sessions in f's
// lexical scope, newest first in substringOrder, until visit reports that it
// has every row it needs. Each batch is a "session_id IN (...)" condition and
// its argument, in the form sessionScopeSubquery returns.
//
// substringOrder sorts by session recency, then session ID, then position in
// the session, so the rows of one session are contiguous and every row of an
// earlier batch sorts before every row of a later one. Reading the batches in
// order therefore yields the rows in the same order as one statement over the
// whole scope. The tool sources have no substring index, and a sort over the
// whole scope reads every tool input and output in it; stopping after the
// batch that fills the page skips the older sessions.
func walkContentSessions(
	ctx context.Context, q messageRowsQuerier, f ContentSearchFilter,
	visit func(scope string, args []any) (bool, error),
) error {
	sessions, err := listContentSessions(ctx, q, f)
	if err != nil {
		return err
	}
	budget := substringWalkFirstBatch
	for len(sessions) > 0 {
		var ids []string
		held := 0
		for len(sessions) > 0 && (len(ids) == 0 || held < budget) {
			ids = append(ids, sessions[0].id)
			held += max(sessions[0].messages, 1)
			sessions = sessions[1:]
		}
		list, err := json.Marshal(ids)
		if err != nil {
			return err
		}
		done, err := visit("session_id IN (SELECT value FROM json_each(?))", []any{string(list)})
		if err != nil || done {
			return err
		}
		budget *= 2
	}
	return nil
}

// contentSession is a session a substring walk reads and the number of
// messages it holds.
type contentSession struct {
	id       string
	messages int
}

// listContentSessions lists the sessions in f's lexical scope in the order
// walkContentSessions reads them.
func listContentSessions(
	ctx context.Context, q messageRowsQuerier, f ContentSearchFilter,
) ([]contentSession, error) {
	where, args := contentScopeWhere(f)
	rows, err := q.QueryContext(ctx, `SELECT id, message_count FROM sessions WHERE `+where+`
		ORDER BY julianday(COALESCE(ended_at, started_at, '')) DESC, id ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("listing search sessions: %w", err)
	}
	defer rows.Close()
	var sessions []contentSession
	for rows.Next() {
		var s contentSession
		if err := rows.Scan(&s.id, &s.messages); err != nil {
			return nil, fmt.Errorf("scanning search session: %w", err)
		}
		sessions = append(sessions, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing search sessions: %w", err)
	}
	return sessions, nil
}
