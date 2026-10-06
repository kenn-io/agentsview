package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
)

// ErrSessionExternalParentInvalid identifies invalid external parent input.
var ErrSessionExternalParentInvalid = errors.New("invalid session parent link")

// ExternalRelationshipType is the relationship every launcher link records.
// A worker started by an orchestrator is delegated work, which the session
// tree, sidebar, and usage rollups already model as a subagent. The SQL below
// spells it as a literal.
const ExternalRelationshipType = "subagent"

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
// it when the session has no transcript parent or spawn edge.
func (db *DB) SetSessionExternalParent(
	ctx context.Context, sessionID, parentID string,
) (SessionExternalParent, error) {
	if err := db.requireWritable(); err != nil {
		return SessionExternalParent{}, err
	}
	sessionID = strings.TrimSpace(sessionID)
	parentID = strings.TrimSpace(parentID)
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
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO session_external_parents (session_id, parent_session_id)
		VALUES (?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			parent_session_id = excluded.parent_session_id,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')`,
		sessionID, parentID,
	); err != nil {
		return SessionExternalParent{}, fmt.Errorf("saving session parent: %w", err)
	}
	moved, err := applySessionExternalParentsFor(ctx, tx, []string{sessionID})
	if err != nil {
		return SessionExternalParent{}, err
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
	// Dropping the link can release another link whose chain ran through it.
	moved, err := applySessionExternalParentsFor(ctx, tx, []string{sessionID})
	if err != nil {
		return SessionExternalParent{}, err
	}
	if err := tx.Commit(); err != nil {
		return SessionExternalParent{}, fmt.Errorf("committing session parent clear: %w", err)
	}
	link.Changed = link.Applied || moved > 0
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
		SELECT ep.session_id, ep.parent_session_id,
			ep.created_at, ep.updated_at,
			s.id IS NOT NULL, s.parent_session_id, s.relationship_type,
			s.parser_parent_session_id,
			s.id IS NOT NULL AND `+spawnEdgeExistsSQL("s")+`
		FROM session_external_parents ep
		LEFT JOIN sessions s ON s.id = ep.session_id
		WHERE ep.session_id = ?`, sessionID,
	).Scan(
		&link.SessionID, &link.ParentSessionID,
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
	link.RelationshipType = ExternalRelationshipType
	link.Applied = link.SessionFound &&
		parserParent.String == "" && !spawned &&
		currentParent.String == link.ParentSessionID &&
		currentType.String == link.RelationshipType
	return link, nil
}

// externalParentStepSQL is the parent a chain walk follows from the session
// row p and its launcher link pe: the launcher link when no transcript parent
// or spawn edge outranks it, and the stored parent otherwise, so the answer
// never depends on which link was applied first.
var externalParentStepSQL = `CASE
		WHEN pe.session_id IS NOT NULL
			AND COALESCE(p.parser_parent_session_id, '') = ''
			AND NOT ` + spawnEdgeExistsSQL("p") + `
		THEN pe.parent_session_id
		ELSE NULLIF(p.parent_session_id, '')
	END`

// externalParentChainSQL reports whether the parent chain from the start
// expression reaches the target expression, following externalParentStepSQL.
func externalParentChainSQL(start, target string) string {
	return `EXISTS (
		WITH RECURSIVE chain(id) AS (
			SELECT ` + start + `
			UNION
			SELECT ` + externalParentStepSQL + `
			FROM chain
			LEFT JOIN sessions p ON p.id = chain.id
			LEFT JOIN session_external_parents pe ON pe.session_id = chain.id
			WHERE ` + externalParentStepSQL + ` IS NOT NULL
		)
		SELECT 1 FROM chain WHERE id = ` + target + `
	)`
}

// applySessionExternalParentsSQL recomputes the launcher links that
// linksSQL selects (with columns session_id and parent_session_id) from
// current evidence. A link is the effective parent
// when the session has no transcript parent, no spawn edge, and the link's
// chain does not lead back to the session; otherwise a session it once
// applied to returns to no parent. The session triggers run it, so it has
// no statement-level WITH, which trigger bodies cannot carry, and no alias on
// the updated table, which ALTER TABLE's trigger check rejects.
func applySessionExternalParentsSQL(linksSQL string) string {
	// LIMIT -1 keeps the planner from flattening l into the update, which
	// would walk each chain once per reference to cyclic, and the unary plus
	// keeps it from indexing l and scanning sessions instead of seeking them.
	return `
	UPDATE sessions
	SET parent_session_id = IIF(l.cyclic, NULL, l.parent_session_id),
		relationship_type = IIF(l.cyclic, '', 'subagent'),
		local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
	FROM (
		SELECT ep.session_id, ep.parent_session_id,
			` + externalParentChainSQL("ep.parent_session_id", "ep.session_id") + ` AS cyclic
		FROM (` + linksSQL + `) AS ep
		LIMIT -1
	) AS l
	WHERE sessions.id = +l.session_id
	AND COALESCE(sessions.parser_parent_session_id, '') = ''
	AND NOT ` + spawnEdgeExistsSQL("sessions") + `
	AND (
		COALESCE(sessions.parent_session_id, '') <> IIF(l.cyclic, '', l.parent_session_id)
		OR COALESCE(sessions.relationship_type, '') <> IIF(l.cyclic, '', 'subagent')
	)`
}

var applyAllSessionExternalParentsSQL = applySessionExternalParentsSQL(
	"SELECT * FROM session_external_parents")

// scopedSessionExternalParentsSQL limits the recompute to links that a write
// of the sessions selected by start can move. A changed session x only alters
// the links whose chain runs through it, which all sit in its subtree
// (walked over stored parents and launcher links alike). Within that subtree
// a link can start closing a loop only if its session is also above x, and
// can stop closing one only if it is not applied now; the rest keep their
// answer, so their chains are never walked.
func scopedSessionExternalParentsSQL(start string) string {
	return applySessionExternalParentsSQL(`
		WITH RECURSIVE
		down(id) AS (
			` + start + `
			UNION
			SELECT c.id FROM down JOIN sessions c ON c.parent_session_id = down.id
			UNION
			SELECT c.session_id FROM down
			JOIN session_external_parents c ON c.parent_session_id = down.id
		),
		up(id) AS (
			` + start + `
			UNION
			SELECT ` + externalParentStepSQL + `
			FROM up
			LEFT JOIN sessions p ON p.id = up.id
			LEFT JOIN session_external_parents pe ON pe.session_id = up.id
			WHERE ` + externalParentStepSQL + ` IS NOT NULL
		)
		SELECT ep.session_id, ep.parent_session_id
		FROM down
		CROSS JOIN session_external_parents ep ON ep.session_id = down.id
		CROSS JOIN sessions cur ON cur.id = ep.session_id
		WHERE ep.session_id IN (SELECT id FROM up)
		OR cur.parent_session_id IS NOT ep.parent_session_id
		OR cur.relationship_type IS NOT 'subagent'`)
}

// applyScopedSessionExternalParentsSQL binds the JSON id list twice.
var applyScopedSessionExternalParentsSQL = scopedSessionExternalParentsSQL(
	"SELECT value FROM json_each(?)")

// sessionExternalParentTriggerDropsSQL and
// sessionExternalParentTriggerCreatesSQL apply launcher links on every
// session write that inserts a row or changes its parent columns, so an
// upsert that rewrites the same parents skips the walk. The triggers run the
// scoped recompute from the written row inside the writer's statement, so
// no writer can skip it and a rolled-back write takes its recompute with it.
// Like the artifact queue triggers they reference migrated columns, so they
// are dropped before column migrations and created after.
const sessionExternalParentTriggerDropsSQL = `
DROP TRIGGER IF EXISTS trg_sessions_external_parent_insert;
DROP TRIGGER IF EXISTS trg_sessions_external_parent_update;
`

var sessionExternalParentTriggerCreatesSQL = `
CREATE TRIGGER IF NOT EXISTS trg_sessions_external_parent_insert
AFTER INSERT ON sessions
WHEN EXISTS (SELECT 1 FROM session_external_parents)
BEGIN` + scopedSessionExternalParentsSQL("SELECT NEW.id") + `;
END;

CREATE TRIGGER IF NOT EXISTS trg_sessions_external_parent_update
AFTER UPDATE OF parent_session_id, parser_parent_session_id ON sessions
WHEN (OLD.parent_session_id IS NOT NEW.parent_session_id
	OR OLD.parser_parent_session_id IS NOT NEW.parser_parent_session_id)
AND EXISTS (SELECT 1 FROM session_external_parents)
BEGIN` + scopedSessionExternalParentsSQL("SELECT NEW.id") + `;
END;
`

// applySessionExternalParents recomputes every launcher link and returns the
// number of sessions whose effective parent changed. Full linking passes use
// it to catch removed spawn edges, which never write the worker's row.
func applySessionExternalParents(ctx context.Context, tx *sql.Tx) (int, error) {
	return execSessionExternalParents(ctx, tx.ExecContext, applyAllSessionExternalParentsSQL)
}

// applySessionExternalParentsFor recomputes the launcher links that a change
// to the given sessions can move, so its cost tracks the batch rather than
// the number of links.
func applySessionExternalParentsFor(
	ctx context.Context, tx *sql.Tx, ids []string,
) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var linked bool
	if err := tx.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM session_external_parents)",
	).Scan(&linked); err != nil {
		return 0, fmt.Errorf("checking session parents: %w", err)
	}
	if !linked {
		return 0, nil
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return 0, fmt.Errorf("encoding session ids: %w", err)
	}
	return execSessionExternalParents(ctx, tx.ExecContext,
		applyScopedSessionExternalParentsSQL, string(encoded), string(encoded))
}

func execSessionExternalParents(
	ctx context.Context,
	exec func(context.Context, string, ...any) (sql.Result, error),
	query string, args ...any,
) (int, error) {
	res, err := exec(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("applying session parents: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("counting applied session parents: %w", err)
	}
	return int(n), nil
}

// rejectExternalParentCycle rejects a link whose parent chain already leads
// back to the session.
func rejectExternalParentCycle(
	ctx context.Context, q recallQueryRower, sessionID, parentID string,
) error {
	var cyclic bool
	if err := q.QueryRowContext(ctx,
		"SELECT "+externalParentChainSQL("?", "?"), parentID, sessionID,
	).Scan(&cyclic); err != nil {
		return fmt.Errorf("checking session parent ancestry: %w", err)
	}
	if cyclic {
		return fmt.Errorf(
			"%w: %s is already a descendant of %s",
			ErrSessionExternalParentInvalid, parentID, sessionID)
	}
	return nil
}
