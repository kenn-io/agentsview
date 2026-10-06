package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrSessionExternalParentInvalid identifies invalid external parent input.
var ErrSessionExternalParentInvalid = errors.New("invalid session parent link")

// DefaultExternalRelationshipType is the relationship recorded when a
// launcher supplies a parent without naming one. A worker started by an
// orchestrator is delegated work, which the session tree, sidebar, and
// usage rollups already model as a subagent.
const DefaultExternalRelationshipType = "subagent"

// ExternalRelationshipTypes lists the relationship types a launcher may
// record. They are the parser's own child relationships, so the session
// tree and every mirror treat external links like parsed ones.
var ExternalRelationshipTypes = []string{"subagent", "fork", "continuation"}

// maxExternalParentDepth bounds the ancestor walk used to reject cycles.
const maxExternalParentDepth = 256

// SessionExternalParent is a parent link supplied by whatever launched a
// session. It is stored apart from parsed transcript data and applies only
// while the parser found no parent of its own.
type SessionExternalParent struct {
	SessionID        string `json:"session_id"`
	ParentSessionID  string `json:"parent_session_id"`
	RelationshipType string `json:"relationship_type"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
	// SessionFound reports whether the session is already archived. A link
	// for a session that has not synced yet applies when it does.
	SessionFound bool `json:"session_found"`
	// Applied reports whether the link is the session's effective parent.
	// It is false while the session is not archived or when a parser-derived
	// parent takes precedence.
	Applied bool `json:"applied"`
	// Changed reports whether a write moved the session's effective parent.
	Changed bool `json:"-"`
}

// GetSessionExternalParent returns the stored external parent link, or
// sql.ErrNoRows when none is recorded.
func (db *DB) GetSessionExternalParent(
	ctx context.Context, sessionID string,
) (SessionExternalParent, error) {
	sessionID = strings.TrimSpace(sessionID)
	return loadSessionExternalParent(ctx, db.getReader(), sessionID)
}

// SetSessionExternalParent records a launcher-supplied parent and applies
// it when the session has no parser-derived parent.
func (db *DB) SetSessionExternalParent(
	ctx context.Context, sessionID, parentID, relationshipType string,
) (SessionExternalParent, error) {
	if err := db.requireWritable(); err != nil {
		return SessionExternalParent{}, err
	}
	sessionID = strings.TrimSpace(sessionID)
	parentID = strings.TrimSpace(parentID)
	relationshipType = strings.TrimSpace(relationshipType)
	if relationshipType == "" {
		relationshipType = DefaultExternalRelationshipType
	}
	switch {
	case sessionID == "":
		return SessionExternalParent{}, fmt.Errorf(
			"%w: session_id is required", ErrSessionExternalParentInvalid)
	case parentID == "":
		return SessionExternalParent{}, fmt.Errorf(
			"%w: parent_session_id is required", ErrSessionExternalParentInvalid)
	case parentID == sessionID:
		return SessionExternalParent{}, fmt.Errorf(
			"%w: a session cannot be its own parent", ErrSessionExternalParentInvalid)
	case !slices.Contains(ExternalRelationshipTypes, relationshipType):
		return SessionExternalParent{}, fmt.Errorf(
			"%w: relationship_type must be one of %s",
			ErrSessionExternalParentInvalid,
			strings.Join(ExternalRelationshipTypes, ", "))
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return SessionExternalParent{}, fmt.Errorf("beginning session parent write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := rejectExternalParentCycle(ctx, tx, sessionID, parentID); err != nil {
		return SessionExternalParent{}, err
	}
	previous, err := loadSessionExternalParent(ctx, tx, sessionID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return SessionExternalParent{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO session_external_parents (
			session_id, parent_session_id, relationship_type
		) VALUES (?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			parent_session_id = excluded.parent_session_id,
			relationship_type = excluded.relationship_type,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')`,
		sessionID, parentID, relationshipType,
	); err != nil {
		return SessionExternalParent{}, fmt.Errorf("saving session parent: %w", err)
	}
	// Replace the effective parent only when it is empty or is the link
	// this call replaces; a parser-derived parent or a tool-call spawn edge
	// always wins.
	applied, err := tx.ExecContext(ctx, `
		UPDATE sessions
		SET parent_session_id = ?,
			relationship_type = ?,
			local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id = ?
		AND COALESCE(parser_parent_session_id, '') = ''
		AND NOT `+spawnEdgeExistsSQL("sessions")+`
		AND (
			COALESCE(parent_session_id, '') = ''
			OR (parent_session_id = ? AND relationship_type = ?)
		)
		AND (parent_session_id IS NOT ? OR relationship_type IS NOT ?)`,
		parentID, relationshipType, sessionID,
		previous.ParentSessionID, previous.RelationshipType,
		parentID, relationshipType,
	)
	if err != nil {
		return SessionExternalParent{}, fmt.Errorf("applying session parent: %w", err)
	}
	moved, err := applied.RowsAffected()
	if err != nil {
		return SessionExternalParent{}, fmt.Errorf("applying session parent: %w", err)
	}
	link, err := loadSessionExternalParent(ctx, tx, sessionID)
	if err != nil {
		return SessionExternalParent{}, err
	}
	link.Changed = moved > 0
	if err := tx.Commit(); err != nil {
		return SessionExternalParent{}, fmt.Errorf("committing session parent: %w", err)
	}
	return link, nil
}

// ClearSessionExternalParent removes a launcher-supplied parent. When the
// link was the session's effective parent, the session returns to having
// none. It returns sql.ErrNoRows when no link is recorded.
func (db *DB) ClearSessionExternalParent(
	ctx context.Context, sessionID string,
) (SessionExternalParent, error) {
	if err := db.requireWritable(); err != nil {
		return SessionExternalParent{}, err
	}
	sessionID = strings.TrimSpace(sessionID)

	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return SessionExternalParent{}, fmt.Errorf("beginning session parent clear: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	link, err := loadSessionExternalParent(ctx, tx, sessionID)
	if err != nil {
		return SessionExternalParent{}, err
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM session_external_parents WHERE session_id = ?", sessionID,
	); err != nil {
		return SessionExternalParent{}, fmt.Errorf("deleting session parent: %w", err)
	}
	if link.Applied {
		if _, err := tx.ExecContext(ctx, `
			UPDATE sessions
			SET parent_session_id = NULL,
				relationship_type = '',
				local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
			WHERE id = ?`, sessionID,
		); err != nil {
			return SessionExternalParent{}, fmt.Errorf("removing session parent: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return SessionExternalParent{}, fmt.Errorf("committing session parent clear: %w", err)
	}
	link.Changed = link.Applied
	link.Applied = false
	return link, nil
}

// spawnEdgeExistsSQL reports whether a tool call records the session row
// named by alias as a spawned subagent. Linking derives that session's
// parent from the edge, so a launcher-supplied link never owns it.
func spawnEdgeExistsSQL(alias string) string {
	return `EXISTS (
			SELECT 1 FROM tool_calls tc
			WHERE tc.subagent_session_id = ` + alias + `.id
			AND tc.session_id IS NOT tc.subagent_session_id
		)`
}

func loadSessionExternalParent(
	ctx context.Context, q recallQueryRower, sessionID string,
) (SessionExternalParent, error) {
	var (
		link          SessionExternalParent
		currentParent sql.NullString
		currentType   sql.NullString
		parserParent  sql.NullString
		spawned       bool
	)
	err := q.QueryRowContext(ctx, `
		SELECT ep.session_id, ep.parent_session_id, ep.relationship_type,
			ep.created_at, ep.updated_at,
			s.id IS NOT NULL, s.parent_session_id, s.relationship_type,
			s.parser_parent_session_id,
			s.id IS NOT NULL AND `+spawnEdgeExistsSQL("s")+`
		FROM session_external_parents ep
		LEFT JOIN sessions s ON s.id = ep.session_id
		WHERE ep.session_id = ?`, sessionID,
	).Scan(
		&link.SessionID, &link.ParentSessionID, &link.RelationshipType,
		&link.CreatedAt, &link.UpdatedAt,
		&link.SessionFound, &currentParent, &currentType, &parserParent,
		&spawned,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SessionExternalParent{}, err
		}
		return SessionExternalParent{}, fmt.Errorf("loading session parent: %w", err)
	}
	link.Applied = link.SessionFound &&
		parserParent.String == "" && !spawned &&
		currentParent.String == link.ParentSessionID &&
		currentType.String == link.RelationshipType
	return link, nil
}

// rejectExternalParentCycle walks the proposed parent's ancestors, through
// both effective and pending external links, and rejects a link that would
// make the session its own ancestor.
func rejectExternalParentCycle(
	ctx context.Context, q recallQueryRower, sessionID, parentID string,
) error {
	current := parentID
	for range maxExternalParentDepth {
		var next sql.NullString
		err := q.QueryRowContext(ctx, `
			SELECT COALESCE(
				NULLIF((SELECT parent_session_id FROM sessions WHERE id = ?), ''),
				(SELECT parent_session_id FROM session_external_parents
					WHERE session_id = ?)
			)`, current, current,
		).Scan(&next)
		if err != nil {
			return fmt.Errorf("checking session parent ancestry: %w", err)
		}
		if !next.Valid || next.String == "" {
			return nil
		}
		if next.String == sessionID {
			return fmt.Errorf(
				"%w: %s is already a descendant of %s",
				ErrSessionExternalParentInvalid, parentID, sessionID)
		}
		current = next.String
	}
	return fmt.Errorf(
		"%w: parent chain deeper than %d sessions",
		ErrSessionExternalParentInvalid, maxExternalParentDepth)
}
