package db

import (
	"context"
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

// PinCurationEntry is one pinned message's full curation-relevant identity:
// the state a curation fingerprint needs to detect not just a note-only
// edit (PinMessage on an already-pinned message updates the note in place,
// leaving the pinned message id set unchanged) but also an unpin-then-repin
// of the same message (which gets a new pin row ID and CreatedAt even
// though MessageID is unchanged) and a NULL-vs-empty-string note change
// (an explicit empty note is a different state than never having pinned a
// note at all). HasNote distinguishes those last two cases instead of
// collapsing both to an empty string the way a COALESCE-over-note read
// would.
type PinCurationEntry struct {
	ID        int64
	MessageID int64
	CreatedAt string
	Note      string
	HasNote   bool
}

// ListPinnedSessionIDsForScope returns the distinct session IDs that have
// at least one pinned message, restricted to the given project scope and
// sorted for deterministic output. Like ListStarredSessionIDsForScope,
// cost is bounded by the number of pinned rows, not archive size; mirror
// pushes use it to load the pin side of the curation set without listing
// every mirror session.
func (db *DB) ListPinnedSessionIDsForScope(
	ctx context.Context, projects, excludeProjects []string,
) ([]string, error) {
	where, args := curationScopeWhere("s", projects, excludeProjects)
	rows, err := db.getReader().QueryContext(ctx,
		`SELECT DISTINCT pm.session_id FROM pinned_messages pm
		 JOIN sessions s ON s.id = pm.session_id`+where+
			` ORDER BY pm.session_id`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("listing scoped pinned session ids: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning scoped pinned session id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
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
