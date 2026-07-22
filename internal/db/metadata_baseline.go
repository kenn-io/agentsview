package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const metadataBaselinePageSize = 128

// MetadataBaselineSnapshot captures existing local user curation that predates
// artifact metadata event recording.
type MetadataBaselineSnapshot struct {
	Renames           []MetadataBaselineRename
	StarredSessionIDs []string
	SoftDeletedIDs    []string
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

// VisitMetadataBaselinePages visits current local curation in fixed-size
// keyset pages. Every query cursor is fully read and closed before visit runs,
// so the callback may create artifacts and record replay state on the same DB.
func (db *DB) VisitMetadataBaselinePages(
	ctx context.Context,
	visit func(MetadataBaselineSnapshot) error,
) error {
	if visit == nil {
		return errors.New("metadata baseline page visitor is required")
	}
	for _, visitKind := range []func(context.Context, func(MetadataBaselineSnapshot) error) error{
		db.visitMetadataBaselineRenamePages,
		db.visitMetadataBaselineStarPages,
		db.visitMetadataBaselineSoftDeletePages,
		db.visitMetadataBaselinePinPages,
	} {
		if err := visitKind(ctx, visit); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) visitMetadataBaselineRenamePages(
	ctx context.Context, visit func(MetadataBaselineSnapshot) error,
) error {
	afterSessionID := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := db.getReader().QueryContext(ctx, `
			SELECT id, display_name
			FROM sessions
			WHERE display_name IS NOT NULL AND id > ?
			ORDER BY id
			LIMIT ?`, afterSessionID, metadataBaselinePageSize)
		if err != nil {
			return fmt.Errorf("listing baseline rename page: %w", err)
		}
		page := MetadataBaselineSnapshot{
			Renames: make([]MetadataBaselineRename, 0, metadataBaselinePageSize),
		}
		for rows.Next() {
			var sessionID, displayName string
			if err := rows.Scan(&sessionID, &displayName); err != nil {
				rows.Close()
				return fmt.Errorf("scanning baseline rename page: %w", err)
			}
			displayNameCopy := displayName
			page.Renames = append(page.Renames, MetadataBaselineRename{
				SessionID: sessionID, DisplayName: &displayNameCopy,
			})
		}
		if err := closeMetadataBaselineRows(rows, "rename"); err != nil {
			return err
		}
		if len(page.Renames) == 0 {
			return nil
		}
		if err := visit(page); err != nil {
			return err
		}
		afterSessionID = page.Renames[len(page.Renames)-1].SessionID
		if len(page.Renames) < metadataBaselinePageSize {
			return nil
		}
	}
}

func (db *DB) visitMetadataBaselineStarPages(
	ctx context.Context, visit func(MetadataBaselineSnapshot) error,
) error {
	afterSessionID := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := db.getReader().QueryContext(ctx, `
			SELECT ss.session_id
			FROM starred_sessions ss
			JOIN sessions s ON s.id = ss.session_id
			WHERE ss.session_id > ?
			ORDER BY ss.session_id
			LIMIT ?`, afterSessionID, metadataBaselinePageSize)
		if err != nil {
			return fmt.Errorf("listing baseline star page: %w", err)
		}
		page := MetadataBaselineSnapshot{
			StarredSessionIDs: make([]string, 0, metadataBaselinePageSize),
		}
		for rows.Next() {
			var sessionID string
			if err := rows.Scan(&sessionID); err != nil {
				rows.Close()
				return fmt.Errorf("scanning baseline star page: %w", err)
			}
			page.StarredSessionIDs = append(page.StarredSessionIDs, sessionID)
		}
		if err := closeMetadataBaselineRows(rows, "star"); err != nil {
			return err
		}
		if len(page.StarredSessionIDs) == 0 {
			return nil
		}
		if err := visit(page); err != nil {
			return err
		}
		afterSessionID = page.StarredSessionIDs[len(page.StarredSessionIDs)-1]
		if len(page.StarredSessionIDs) < metadataBaselinePageSize {
			return nil
		}
	}
}

func (db *DB) visitMetadataBaselineSoftDeletePages(
	ctx context.Context, visit func(MetadataBaselineSnapshot) error,
) error {
	afterSessionID := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := db.getReader().QueryContext(ctx, `
			SELECT id
			FROM sessions
			WHERE deleted_at IS NOT NULL AND id > ?
			ORDER BY id
			LIMIT ?`, afterSessionID, metadataBaselinePageSize)
		if err != nil {
			return fmt.Errorf("listing baseline soft-delete page: %w", err)
		}
		page := MetadataBaselineSnapshot{
			SoftDeletedIDs: make([]string, 0, metadataBaselinePageSize),
		}
		for rows.Next() {
			var sessionID string
			if err := rows.Scan(&sessionID); err != nil {
				rows.Close()
				return fmt.Errorf("scanning baseline soft-delete page: %w", err)
			}
			page.SoftDeletedIDs = append(page.SoftDeletedIDs, sessionID)
		}
		if err := closeMetadataBaselineRows(rows, "soft-delete"); err != nil {
			return err
		}
		if len(page.SoftDeletedIDs) == 0 {
			return nil
		}
		if err := visit(page); err != nil {
			return err
		}
		afterSessionID = page.SoftDeletedIDs[len(page.SoftDeletedIDs)-1]
		if len(page.SoftDeletedIDs) < metadataBaselinePageSize {
			return nil
		}
	}
}

type metadataBaselinePinPageRow struct {
	pin   MetadataBaselinePin
	pinID int64
}

func (db *DB) visitMetadataBaselinePinPages(
	ctx context.Context, visit func(MetadataBaselineSnapshot) error,
) error {
	afterSessionID := ""
	afterOrdinal := 0
	var afterPinID int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := db.getReader().QueryContext(ctx, `
			SELECT p.session_id, COALESCE(m.source_uuid, ''),
				p.ordinal, p.note, p.id
			FROM pinned_messages p
			JOIN sessions s ON s.id = p.session_id
			JOIN messages m ON m.id = p.message_id AND m.session_id = p.session_id
			WHERE (p.session_id, p.ordinal, p.id) > (?, ?, ?)
			ORDER BY p.session_id, p.ordinal, p.id
			LIMIT ?`,
			afterSessionID, afterOrdinal, afterPinID,
			metadataBaselinePageSize,
		)
		if err != nil {
			return fmt.Errorf("listing baseline pin page: %w", err)
		}
		pageRows := make([]metadataBaselinePinPageRow, 0, metadataBaselinePageSize)
		for rows.Next() {
			var row metadataBaselinePinPageRow
			var note sql.NullString
			if err := rows.Scan(
				&row.pin.SessionID, &row.pin.SourceUUID, &row.pin.Ordinal, &note, &row.pinID,
			); err != nil {
				rows.Close()
				return fmt.Errorf("scanning baseline pin page: %w", err)
			}
			if note.Valid {
				noteCopy := note.String
				row.pin.Note = &noteCopy
			}
			pageRows = append(pageRows, row)
		}
		if err := closeMetadataBaselineRows(rows, "pin"); err != nil {
			return err
		}
		if len(pageRows) == 0 {
			return nil
		}
		page := MetadataBaselineSnapshot{
			Pins: make([]MetadataBaselinePin, len(pageRows)),
		}
		for index := range pageRows {
			page.Pins[index] = pageRows[index].pin
		}
		if err := visit(page); err != nil {
			return err
		}
		last := pageRows[len(pageRows)-1]
		afterSessionID = last.pin.SessionID
		afterOrdinal = last.pin.Ordinal
		afterPinID = last.pinID
		if len(pageRows) < metadataBaselinePageSize {
			return nil
		}
	}
}

func closeMetadataBaselineRows(rows *sql.Rows, kind string) error {
	rowsErr := rows.Err()
	closeErr := rows.Close()
	if err := errors.Join(rowsErr, closeErr); err != nil {
		return fmt.Errorf("iterating baseline %s page: %w", kind, err)
	}
	return nil
}

// MetadataBaselineSnapshot returns the current curation rows that need baseline
// metadata events during artifact sync initialization.
func (db *DB) MetadataBaselineSnapshot(ctx context.Context) (MetadataBaselineSnapshot, error) {
	var snap MetadataBaselineSnapshot

	// Curation queries do not filter on deleted_at: a session sitting in
	// trash at opt-in still baselines its name, star, and pins, so a later
	// restore reaches peers with them instead of only the soft delete.
	renameRows, err := db.getReader().QueryContext(ctx, `
		SELECT id, display_name
		FROM sessions
		WHERE display_name IS NOT NULL
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

	deletedRows, err := db.getReader().QueryContext(ctx, `
		SELECT id
		FROM sessions
		WHERE deleted_at IS NOT NULL
		ORDER BY id`)
	if err != nil {
		return snap, fmt.Errorf("listing baseline soft deletes: %w", err)
	}
	defer deletedRows.Close()
	for deletedRows.Next() {
		var sessionID string
		if err := deletedRows.Scan(&sessionID); err != nil {
			return snap, fmt.Errorf("scanning baseline soft delete: %w", err)
		}
		snap.SoftDeletedIDs = append(snap.SoftDeletedIDs, sessionID)
	}
	if err := deletedRows.Err(); err != nil {
		return snap, fmt.Errorf("iterating baseline soft deletes: %w", err)
	}

	pinRows, err := db.getReader().QueryContext(ctx, `
		SELECT p.session_id, COALESCE(m.source_uuid, ''),
			m.ordinal, p.note
		FROM pinned_messages p
		JOIN sessions s
			ON s.id = p.session_id
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
