package clickhouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

var _ db.ToolSequenceReadSource = (*Store)(nil)

type toolSequencePublication struct {
	version     uint64
	revision    string
	termination sql.NullString
	messages    uint64
}

func (s *Store) toolSequencePublication(ctx context.Context, id string) (toolSequencePublication, bool, error) {
	var p toolSequencePublication
	err := s.conn.QueryRowContext(ctx, `SELECT push_version, transcript_revision, termination_status, message_count FROM sessions WHERE id = ? AND deleted_at IS NULL`, id).Scan(&p.version, &p.revision, &p.termination, &p.messages)
	if errors.Is(err, sql.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}

func (s *Store) ToolSequenceReadSource(ctx context.Context, id string, validatePublication bool) (string, bool, error) {
	before, exists, err := s.toolSequencePublication(ctx, id)
	if err != nil || !exists {
		return "", false, err
	}
	binding := fmt.Sprintf("%d:%s:%t:%s:%d", before.version, before.revision, before.termination.Valid, before.termination.String, before.messages)
	if !validatePublication {
		return binding, false, nil
	}
	var messages, mismatches uint64
	err = s.conn.QueryRowContext(ctx, `SELECT count(), countIf(push_version != ?) FROM messages WHERE session_id = ?`, before.version, id).Scan(&messages, &mismatches)
	if err != nil {
		return "", false, err
	}
	pending := messages != before.messages || mismatches != 0
	for _, table := range []string{"tool_calls", "tool_result_events"} {
		var mismatches uint64
		err := s.conn.QueryRowContext(ctx, `SELECT countIf(push_version != ?) FROM `+table+` WHERE session_id = ?`, before.version, id).Scan(&mismatches)
		if err != nil {
			return "", false, err
		}
		pending = pending || mismatches != 0
	}
	after, exists, err := s.toolSequencePublication(ctx, id)
	if err != nil {
		return "", false, err
	}
	pending = pending || !exists || before != after
	return binding, pending, nil
}
