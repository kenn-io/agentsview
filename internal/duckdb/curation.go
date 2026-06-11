package duckdb

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

func (s *Store) StarSession(sessionID string) (bool, error) {
	return false, db.ErrReadOnly
}

func (s *Store) UnstarSession(sessionID string) error {
	return db.ErrReadOnly
}

func (s *Store) ListStarredSessionIDs(ctx context.Context) ([]string, error) {
	rows, err := s.duck.QueryContext(ctx, `
		SELECT session_id FROM starred_sessions
		ORDER BY created_at DESC, session_id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) BulkStarSessions(sessionIDs []string) error {
	return db.ErrReadOnly
}

func (s *Store) PinMessage(sessionID string, messageID int64, note *string) (int64, error) {
	return 0, db.ErrReadOnly
}

func (s *Store) UnpinMessage(sessionID string, messageID int64) error {
	return db.ErrReadOnly
}

func (s *Store) ListPinnedMessages(
	ctx context.Context, sessionID string, project string,
) ([]db.PinnedMessage, error) {
	var rows *sql.Rows
	var err error
	if sessionID != "" {
		rows, err = s.duck.QueryContext(ctx, `
			SELECT id, session_id, message_id, ordinal, note, created_at
			FROM pinned_messages
			WHERE session_id = ?
			ORDER BY created_at DESC, id DESC`,
			sessionID,
		)
	} else {
		query := `
			SELECT p.id, p.session_id, p.message_id, p.ordinal,
				p.note, p.created_at, m.content, m.role,
				s.project, s.agent, COALESCE(s.display_name, s.session_name), s.first_message
			FROM pinned_messages p
			JOIN sessions s ON p.session_id = s.id AND s.deleted_at IS NULL
			LEFT JOIN messages m ON p.session_id = m.session_id
				AND p.message_id = m.id`
		args := []any{}
		if project != "" {
			query += " WHERE s.project = ?"
			args = append(args, project)
		}
		query += " ORDER BY p.created_at DESC, p.id DESC LIMIT 500"
		rows, err = s.duck.QueryContext(ctx, query, args...)
	}
	if err != nil {
		return nil, fmt.Errorf("listing duckdb pinned messages: %w", err)
	}
	defer rows.Close()
	var pins []db.PinnedMessage
	withContent := sessionID == ""
	for rows.Next() {
		var p db.PinnedMessage
		var created any
		if withContent {
			err = rows.Scan(&p.ID, &p.SessionID, &p.MessageID, &p.Ordinal,
				&p.Note, &created, &p.Content, &p.Role,
				&p.SessionProject, &p.SessionAgent,
				&p.SessionDisplayName, &p.SessionFirstMessage)
		} else {
			err = rows.Scan(&p.ID, &p.SessionID, &p.MessageID,
				&p.Ordinal, &p.Note, &created)
		}
		if err != nil {
			return nil, err
		}
		p.CreatedAt = formatDBTime(created)
		pins = append(pins, p)
	}
	return pins, rows.Err()
}
