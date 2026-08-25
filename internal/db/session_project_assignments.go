package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

// SessionProjectAssignment is a user-selected project override for one
// session. It takes precedence over parser discovery and folder mapping rules.
type SessionProjectAssignment struct {
	SessionID string `json:"session_id"`
	Project   string `json:"project"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// AssignSessionProject moves one session without creating reusable folder
// mapping evidence.
func (db *DB) AssignSessionProject(
	ctx context.Context,
	sessionID string,
	project string,
) (SessionProjectAssignment, error) {
	if err := db.requireWritable(); err != nil {
		return SessionProjectAssignment{}, err
	}
	sessionID = strings.TrimSpace(sessionID)
	project = parser.NormalizeName(strings.TrimSpace(project))
	if sessionID == "" {
		return SessionProjectAssignment{}, fmt.Errorf("session_id is required")
	}
	if project == "" {
		return SessionProjectAssignment{}, fmt.Errorf("project is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return SessionProjectAssignment{}, fmt.Errorf(
			"beginning session project assignment: %w", err,
		)
	}
	defer func() { _ = tx.Rollback() }()

	var previousProject string
	if err := tx.QueryRowContext(ctx,
		`SELECT project FROM sessions WHERE id = ? AND deleted_at IS NULL`,
		sessionID,
	).Scan(&previousProject); err != nil {
		if err == sql.ErrNoRows {
			return SessionProjectAssignment{}, err
		}
		return SessionProjectAssignment{}, fmt.Errorf(
			"loading session for project assignment: %w", err,
		)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO session_project_assignments (session_id, project)
		VALUES (?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			project = excluded.project,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')`,
		sessionID, project,
	); err != nil {
		return SessionProjectAssignment{}, fmt.Errorf(
			"saving session project assignment: %w", err,
		)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE sessions
		SET project = ?,
			local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id = ?`, project, sessionID,
	); err != nil {
		return SessionProjectAssignment{}, fmt.Errorf(
			"applying session project assignment: %w", err,
		)
	}
	if previousProject != project {
		if err := reconcileSessionProjectIdentityAggregatesTx(
			ctx, tx, sessionID, []string{previousProject, project},
		); err != nil {
			return SessionProjectAssignment{}, err
		}
	}

	var assignment SessionProjectAssignment
	if err := tx.QueryRowContext(ctx, `
		SELECT session_id, project, created_at, updated_at
		FROM session_project_assignments
		WHERE session_id = ?`, sessionID,
	).Scan(
		&assignment.SessionID, &assignment.Project,
		&assignment.CreatedAt, &assignment.UpdatedAt,
	); err != nil {
		return SessionProjectAssignment{}, fmt.Errorf(
			"loading saved session project assignment: %w", err,
		)
	}
	if err := tx.Commit(); err != nil {
		return SessionProjectAssignment{}, fmt.Errorf(
			"committing session project assignment: %w", err,
		)
	}
	return assignment, nil
}
