package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// StarSession marks a session as starred in the shared PG dashboard
// metadata. Returns false when the session does not exist.
func (s *Store) StarSession(sessionID string) (bool, error) {
	res, err := s.pg.Exec(`
		INSERT INTO starred_sessions (session_id)
		SELECT $1 WHERE EXISTS (
			SELECT 1 FROM sessions WHERE id = $1
		)
		ON CONFLICT (session_id) DO NOTHING`,
		sessionID,
	)
	if err != nil {
		return false, fmt.Errorf("starring session %s: %w", sessionID, err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		return true, nil
	}

	var exists int
	err = s.pg.QueryRow(
		`SELECT 1 FROM sessions WHERE id = $1`,
		sessionID,
	).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking session %s: %w", sessionID, err)
	}
	return true, nil
}

// UnstarSession removes a session star from the shared PG dashboard
// metadata.
func (s *Store) UnstarSession(sessionID string) error {
	if _, err := s.pg.Exec(
		`DELETE FROM starred_sessions WHERE session_id = $1`,
		sessionID,
	); err != nil {
		return fmt.Errorf("unstarring session %s: %w", sessionID, err)
	}
	return nil
}

// ListStarredSessionIDs returns shared PG-starred session IDs.
func (s *Store) ListStarredSessionIDs(
	ctx context.Context,
) ([]string, error) {
	rows, err := s.pg.QueryContext(ctx, `
		SELECT session_id
		FROM starred_sessions
		ORDER BY created_at DESC, session_id DESC`)
	if err != nil {
		return nil, fmt.Errorf("listing starred sessions: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning starred session: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// BulkStarSessions stars multiple existing sessions in one transaction.
// Unknown session IDs are skipped.
func (s *Store) BulkStarSessions(sessionIDs []string) error {
	if len(sessionIDs) == 0 {
		return nil
	}

	tx, err := s.pg.Begin()
	if err != nil {
		return fmt.Errorf("beginning star transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`
		INSERT INTO starred_sessions (session_id)
		SELECT $1 WHERE EXISTS (
			SELECT 1 FROM sessions WHERE id = $1
		)
		ON CONFLICT (session_id) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("preparing star statement: %w", err)
	}
	defer stmt.Close()

	for _, id := range sessionIDs {
		if _, err := stmt.Exec(id); err != nil {
			return fmt.Errorf("starring session %s: %w", id, err)
		}
	}

	return tx.Commit()
}

// PinMessage creates or updates a shared PG pin for a message. PG
// exposes message IDs as the message ordinal, matching scanPGMessages.
// On conflict, ordinal and source_uuid are refreshed from the current
// message so the pin tracks whatever is at message_id today;
// created_at is preserved by being absent from the SET clause.
func (s *Store) PinMessage(
	sessionID string, messageID int64, note *string,
) (int64, error) {
	var id int64
	err := s.pg.QueryRow(`
		WITH upsert AS (
			INSERT INTO pinned_messages (
				session_id, message_id, ordinal, source_uuid, note
			)
			SELECT $1, m.ordinal, m.ordinal,
				COALESCE(m.source_uuid, ''), $3
			FROM messages m
			WHERE m.session_id = $1 AND m.ordinal = $2
			ON CONFLICT (session_id, message_id)
			DO UPDATE SET
				note = EXCLUDED.note,
				ordinal = EXCLUDED.ordinal,
				source_uuid = EXCLUDED.source_uuid
			RETURNING id
		)
		SELECT id FROM upsert`,
		sessionID, messageID, note,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("pinning message: %w", err)
	}
	return id, nil
}

// UnpinMessage removes a shared PG pin.
func (s *Store) UnpinMessage(sessionID string, messageID int64) error {
	if _, err := s.pg.Exec(
		`DELETE FROM pinned_messages
		 WHERE session_id = $1 AND message_id = $2`,
		sessionID, messageID,
	); err != nil {
		return fmt.Errorf("unpinning message: %w", err)
	}
	return nil
}

// ListPinnedMessages returns shared PG pins, optionally filtered by
// session or project. All-pins results include message and session
// metadata for the pinned page.
func (s *Store) ListPinnedMessages(
	ctx context.Context, sessionID string, project string,
) ([]db.PinnedMessage, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if sessionID != "" {
		rows, err = s.pg.QueryContext(ctx, `
			SELECT id, session_id, message_id, ordinal, note, created_at
			FROM pinned_messages
			WHERE session_id = $1
			ORDER BY created_at DESC, id DESC`,
			sessionID,
		)
	} else {
		query := `
			SELECT p.id, p.session_id, p.message_id, p.ordinal,
				p.note, p.created_at, m.content, m.role,
				s.project, s.agent, COALESCE(s.display_name, s.session_name), s.first_message
			FROM pinned_messages p
			JOIN sessions s
				ON p.session_id = s.id
				AND s.deleted_at IS NULL
			LEFT JOIN messages m
				ON m.session_id = p.session_id
				AND m.ordinal = p.message_id`
		args := []any{}
		if project != "" {
			query += " WHERE s.project = $1"
			args = append(args, project)
		}
		query += " ORDER BY p.created_at DESC, p.id DESC LIMIT 500"
		rows, err = s.pg.QueryContext(ctx, query, args...)
	}
	if err != nil {
		return nil, fmt.Errorf("listing pinned messages: %w", err)
	}
	defer rows.Close()

	var pins []db.PinnedMessage
	withContent := sessionID == ""
	for rows.Next() {
		p, scanErr := scanPGPinnedMessage(rows, withContent)
		if scanErr != nil {
			return nil, scanErr
		}
		pins = append(pins, p)
	}
	return pins, rows.Err()
}

func scanPGPinnedMessage(
	rs interface{ Scan(dest ...any) error },
	withContent bool,
) (db.PinnedMessage, error) {
	var p db.PinnedMessage
	var createdAt time.Time
	var err error
	if withContent {
		err = rs.Scan(
			&p.ID, &p.SessionID, &p.MessageID,
			&p.Ordinal, &p.Note, &createdAt,
			&p.Content, &p.Role,
			&p.SessionProject, &p.SessionAgent,
			&p.SessionDisplayName, &p.SessionFirstMessage,
		)
	} else {
		err = rs.Scan(
			&p.ID, &p.SessionID, &p.MessageID,
			&p.Ordinal, &p.Note, &createdAt,
		)
	}
	if err != nil {
		return p, fmt.Errorf("scanning pinned message: %w", err)
	}
	p.CreatedAt = FormatISO8601(createdAt)
	return p, nil
}
