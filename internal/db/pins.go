package db

import (
	"context"
	"database/sql"
	"fmt"
)

// PinnedMessage represents a row in the pinned_messages table.
type PinnedMessage struct {
	ID        int64   `json:"id"`
	SessionID string  `json:"session_id"`
	MessageID int64   `json:"message_id"`
	Ordinal   int     `json:"ordinal"`
	Note      *string `json:"note,omitempty"`
	Content   *string `json:"content,omitempty"`
	Role      *string `json:"role,omitempty"`
	CreatedAt string  `json:"created_at"`

	// Session metadata — populated only for the "all pins" query.
	SessionProject      *string `json:"session_project,omitempty"`
	SessionAgent        *string `json:"session_agent,omitempty"`
	SessionDisplayName  *string `json:"session_display_name,omitempty"`
	SessionFirstMessage *string `json:"session_first_message,omitempty"`
}

const pinnedBaseCols = `id, session_id, message_id, ordinal, note, created_at`

func scanPinnedRow(rs rowScanner) (PinnedMessage, error) {
	var p PinnedMessage
	err := rs.Scan(
		&p.ID, &p.SessionID, &p.MessageID,
		&p.Ordinal, &p.Note, &p.CreatedAt,
	)
	return p, err
}

func scanPinnedRowWithContent(rs rowScanner) (PinnedMessage, error) {
	var p PinnedMessage
	err := rs.Scan(
		&p.ID, &p.SessionID, &p.MessageID,
		&p.Ordinal, &p.Note, &p.CreatedAt,
		&p.Content, &p.Role,
		&p.SessionProject, &p.SessionAgent, &p.SessionDisplayName,
		&p.SessionFirstMessage,
	)
	return p, err
}

// PinMessage creates a pin for a message. If the message is
// already pinned, the note is updated. The message must belong to
// the specified session (enforced via INSERT ... SELECT).
func (db *DB) PinMessage(
	sessionID string, messageID int64, note *string,
) (int64, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	// Use INSERT ... SELECT to enforce session-message ownership
	// and read ordinal from the messages table (not the client).
	// RowsAffected is not checked because SQLite may report 0 on
	// an idempotent upsert (same note value). Instead we rely on
	// the subsequent SELECT to detect a missing pin.
	if _, err := db.getWriter().Exec(
		`INSERT INTO pinned_messages (session_id, message_id, ordinal, note)
		 SELECT ?, m.id, m.ordinal, ?
		 FROM messages m
		 WHERE m.id = ? AND m.session_id = ?
		 ON CONFLICT(session_id, message_id) DO UPDATE SET note = excluded.note`,
		sessionID, note, messageID, sessionID,
	); err != nil {
		return 0, fmt.Errorf("pinning message: %w", err)
	}

	// Retrieve the actual row ID (LastInsertId is unreliable on
	// upsert in SQLite). If no row exists the message did not
	// belong to the session (the INSERT ... SELECT matched nothing).
	var id int64
	err := db.getWriter().QueryRow(
		"SELECT id FROM pinned_messages WHERE session_id = ? AND message_id = ?",
		sessionID, messageID,
	).Scan(&id)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, fmt.Errorf("retrieving pin id: %w", err)
	}
	return id, nil
}

// UnpinMessage removes a pin.
func (db *DB) UnpinMessage(sessionID string, messageID int64) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().Exec(
		"DELETE FROM pinned_messages WHERE session_id = ? AND message_id = ?",
		sessionID, messageID,
	)
	return err
}

// ListPinnedMessages returns all pins, optionally filtered by session.
// Pass empty sessionID for all pins across all sessions.
// When listing all pins, message content and role are included.
func (db *DB) ListPinnedMessages(
	ctx context.Context, sessionID string,
) ([]PinnedMessage, error) {
	var query string
	var args []any
	if sessionID != "" {
		query = "SELECT " + pinnedBaseCols +
			" FROM pinned_messages WHERE session_id = ?" +
			" ORDER BY created_at DESC"
		args = []any{sessionID}
	} else {
		// Join sessions to exclude trashed sessions and include
		// session metadata (project, agent, display_name) so the
		// frontend doesn't need a separate lookup.
		query = `SELECT p.id, p.session_id, p.message_id, p.ordinal,
				p.note, p.created_at, m.content, m.role,
				s.project, s.agent, s.display_name, s.first_message
			FROM pinned_messages p
			JOIN sessions s ON p.session_id = s.id AND s.deleted_at IS NULL
			LEFT JOIN messages m ON p.message_id = m.id
			ORDER BY p.created_at DESC LIMIT 500`
	}

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing pinned messages: %w", err)
	}
	defer rows.Close()

	var pins []PinnedMessage
	withContent := sessionID == ""
	for rows.Next() {
		var p PinnedMessage
		var scanErr error
		if withContent {
			p, scanErr = scanPinnedRowWithContent(rows)
		} else {
			p, scanErr = scanPinnedRow(rows)
		}
		if scanErr != nil {
			return nil, fmt.Errorf("scanning pinned message: %w", scanErr)
		}
		pins = append(pins, p)
	}
	return pins, rows.Err()
}

// GetPinnedMessageIDs returns message IDs that are pinned for a session.
func (db *DB) GetPinnedMessageIDs(
	ctx context.Context, sessionID string,
) (map[int64]bool, error) {
	rows, err := db.getReader().QueryContext(ctx,
		"SELECT message_id FROM pinned_messages WHERE session_id = ?",
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}
