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
	SessionID       string `json:"session_id"`
	Project         string `json:"project"`
	OriginalProject string `json:"original_project"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

// ClearedSessionProjectAssignment describes the automatic project selected
// after removing a one-session override.
type ClearedSessionProjectAssignment struct {
	SessionID string `json:"session_id"`
	Project   string `json:"project"`
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
		INSERT INTO session_project_assignments (
			session_id, project, original_project
		)
		VALUES (?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			project = excluded.project,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')`,
		sessionID, project, previousProject,
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
		SELECT session_id, project, original_project, created_at, updated_at
		FROM session_project_assignments
		WHERE session_id = ?`, sessionID,
	).Scan(
		&assignment.SessionID, &assignment.Project,
		&assignment.OriginalProject,
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

// ClearSessionProjectAssignment removes a one-session override, restores the
// project that was automatic when the override was first created, and applies
// the current folder mapping to that session in the same transaction.
func (db *DB) ClearSessionProjectAssignment(
	ctx context.Context,
	sessionID string,
) (ClearedSessionProjectAssignment, error) {
	if err := db.requireWritable(); err != nil {
		return ClearedSessionProjectAssignment{}, err
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ClearedSessionProjectAssignment{}, fmt.Errorf("session_id is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return ClearedSessionProjectAssignment{}, fmt.Errorf(
			"beginning session project assignment clear: %w", err,
		)
	}
	defer func() { _ = tx.Rollback() }()

	var machine, assignedProject, originalProject string
	if err := tx.QueryRowContext(ctx, `
		SELECT s.machine, s.project, spa.original_project
		FROM sessions s
		JOIN session_project_assignments spa ON spa.session_id = s.id
		WHERE s.id = ? AND s.deleted_at IS NULL`, sessionID,
	).Scan(&machine, &assignedProject, &originalProject); err != nil {
		return ClearedSessionProjectAssignment{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM session_project_assignments WHERE session_id = ?`, sessionID,
	); err != nil {
		return ClearedSessionProjectAssignment{}, fmt.Errorf(
			"deleting session project assignment: %w", err,
		)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE sessions
		SET project = ?,
			local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id = ?`, originalProject, sessionID,
	); err != nil {
		return ClearedSessionProjectAssignment{}, fmt.Errorf(
			"restoring automatic session project: %w", err,
		)
	}

	mappings, err := loadActiveWorktreeMappingsTx(ctx, tx, machine)
	if err != nil {
		return ClearedSessionProjectAssignment{}, fmt.Errorf(
			"loading active worktree mappings: %w", err,
		)
	}
	evaluation, err := evaluateWorktreeMappingsTx(
		ctx, tx, machine, mappings, nil, sessionID,
	)
	if err != nil {
		return ClearedSessionProjectAssignment{}, fmt.Errorf(
			"evaluating cleared session project assignment: %w", err,
		)
	}
	effectiveProject := originalProject
	if len(evaluation.updates) > 0 {
		update := evaluation.updates[0]
		if _, err := updateSessionProjectTx(ctx, tx, update, true); err != nil {
			return ClearedSessionProjectAssignment{}, err
		}
		effectiveProject = update.nextProject
	}
	if assignedProject != effectiveProject {
		if err := reconcileSessionProjectIdentityAggregatesTx(
			ctx, tx, sessionID, []string{assignedProject, effectiveProject},
		); err != nil {
			return ClearedSessionProjectAssignment{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return ClearedSessionProjectAssignment{}, fmt.Errorf(
			"committing session project assignment clear: %w", err,
		)
	}
	return ClearedSessionProjectAssignment{
		SessionID: sessionID,
		Project:   effectiveProject,
	}, nil
}
