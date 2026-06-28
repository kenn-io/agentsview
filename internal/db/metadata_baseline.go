package db

import (
	"context"
	"database/sql"
	"fmt"
)

// MetadataBaselineSnapshot captures existing local user curation that predates
// artifact metadata event recording.
type MetadataBaselineSnapshot struct {
	Renames           []MetadataBaselineRename
	StarredSessionIDs []string
	Pins              []MetadataBaselinePin
}

// MetadataBaselineRename is one session display-name override.
type MetadataBaselineRename struct {
	SessionID   string
	DisplayName *string
}

// MetadataBaselinePin is one pinned message represented in metadata-event
// coordinates.
type MetadataBaselinePin struct {
	SessionID  string
	SourceUUID string
	Ordinal    int
	Note       *string
}

// MetadataBaselineSnapshot returns the current curation rows that need baseline
// metadata events during artifact sync initialization.
func (db *DB) MetadataBaselineSnapshot(ctx context.Context) (MetadataBaselineSnapshot, error) {
	var snap MetadataBaselineSnapshot

	renameRows, err := db.getReader().QueryContext(ctx, `
		SELECT id, display_name
		FROM sessions
		WHERE deleted_at IS NULL
			AND display_name IS NOT NULL
		ORDER BY id`)
	if err != nil {
		return snap, fmt.Errorf("listing baseline renames: %w", err)
	}
	defer renameRows.Close()
	for renameRows.Next() {
		var sessionID string
		var displayName string
		if err := renameRows.Scan(&sessionID, &displayName); err != nil {
			return snap, fmt.Errorf("scanning baseline rename: %w", err)
		}
		displayNameCopy := displayName
		snap.Renames = append(snap.Renames, MetadataBaselineRename{
			SessionID:   sessionID,
			DisplayName: &displayNameCopy,
		})
	}
	if err := renameRows.Err(); err != nil {
		return snap, fmt.Errorf("iterating baseline renames: %w", err)
	}

	starRows, err := db.getReader().QueryContext(ctx, `
		SELECT ss.session_id
		FROM starred_sessions ss
		JOIN sessions s
			ON s.id = ss.session_id
			AND s.deleted_at IS NULL
		ORDER BY ss.session_id`)
	if err != nil {
		return snap, fmt.Errorf("listing baseline stars: %w", err)
	}
	defer starRows.Close()
	for starRows.Next() {
		var sessionID string
		if err := starRows.Scan(&sessionID); err != nil {
			return snap, fmt.Errorf("scanning baseline star: %w", err)
		}
		snap.StarredSessionIDs = append(snap.StarredSessionIDs, sessionID)
	}
	if err := starRows.Err(); err != nil {
		return snap, fmt.Errorf("iterating baseline stars: %w", err)
	}

	pinRows, err := db.getReader().QueryContext(ctx, `
		SELECT p.session_id, COALESCE(m.source_uuid, ''),
			m.ordinal, p.note
		FROM pinned_messages p
		JOIN sessions s
			ON s.id = p.session_id
			AND s.deleted_at IS NULL
		JOIN messages m
			ON m.id = p.message_id
			AND m.session_id = p.session_id
		ORDER BY p.session_id, m.ordinal, p.id`)
	if err != nil {
		return snap, fmt.Errorf("listing baseline pins: %w", err)
	}
	defer pinRows.Close()
	for pinRows.Next() {
		var pin MetadataBaselinePin
		var note sql.NullString
		if err := pinRows.Scan(
			&pin.SessionID, &pin.SourceUUID, &pin.Ordinal, &note,
		); err != nil {
			return snap, fmt.Errorf("scanning baseline pin: %w", err)
		}
		if note.Valid {
			noteCopy := note.String
			pin.Note = &noteCopy
		}
		snap.Pins = append(snap.Pins, pin)
	}
	if err := pinRows.Err(); err != nil {
		return snap, fmt.Errorf("iterating baseline pins: %w", err)
	}

	return snap, nil
}
