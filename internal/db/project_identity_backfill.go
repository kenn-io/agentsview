package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/export"
)

const ProjectIdentityBackfillName = "session_project_identity_snapshots_v1"

const projectIdentityBackfillVerifiedKey = "session_project_identity_snapshots_v1_verified"

const projectIdentityBackfillBatchSize = 100

// BackgroundMigrationStatus describes durable progress for an archive
// backfill. State is one of not_needed, pending, running, failed, or completed.
type BackgroundMigrationStatus struct {
	Name           string  `json:"name"`
	State          string  `json:"state"`
	TotalItems     int     `json:"total_items"`
	CompletedItems int     `json:"completed_items"`
	LastError      string  `json:"last_error,omitempty"`
	StartedAt      *string `json:"started_at,omitempty"`
	UpdatedAt      *string `json:"updated_at,omitempty"`
	CompletedAt    *string `json:"completed_at,omitempty"`
}

func ensureProjectIdentityBackfillQueuedTx(
	ctx context.Context, tx *sql.Tx,
) error {
	var state string
	err := tx.QueryRowContext(ctx, `
		SELECT state FROM background_migrations WHERE name = ?`,
		ProjectIdentityBackfillName,
	).Scan(&state)
	if err == nil && state == "completed" {
		var verified string
		verifyErr := tx.QueryRowContext(ctx,
			`SELECT value FROM stats WHERE key = ?`,
			projectIdentityBackfillVerifiedKey,
		).Scan(&verified)
		if verifyErr == nil && verified == "1" {
			return nil
		}
		if verifyErr != nil && !errors.Is(verifyErr, sql.ErrNoRows) {
			return fmt.Errorf("checking project identity backfill verification: %w", verifyErr)
		}
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("checking project identity backfill marker: %w", err)
	}
	var missing bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM sessions s
			WHERE NOT EXISTS (
				SELECT 1 FROM session_project_identity_snapshots p
				WHERE p.session_id = s.id
			)
		)`).Scan(&missing); err != nil {
		return fmt.Errorf("checking project identity backfill candidates: %w", err)
	}
	if !missing {
		if state == "completed" {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO stats (key, value) VALUES (?, '1')
				ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
				projectIdentityBackfillVerifiedKey,
			); err != nil {
				return fmt.Errorf("verifying project identity backfill: %w", err)
			}
		}
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM stats WHERE key = ?`,
		projectIdentityBackfillVerifiedKey); err != nil {
		return fmt.Errorf("invalidating project identity backfill verification: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO background_migrations (
			name, state, total_items, completed_items
		)
		VALUES (?, 'pending', 0, 0)
		ON CONFLICT(name) DO UPDATE SET
			state = 'pending',
			last_error = '',
			completed_at = NULL,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')`,
		ProjectIdentityBackfillName,
	)
	if err != nil {
		return fmt.Errorf("queueing project identity backfill: %w", err)
	}
	return nil
}

// EnsureProjectIdentityBackfillQueued records a resumable job when sessions
// predate immutable project-identity snapshots. It performs database-only work;
// filesystem and Git discovery are deferred to the background worker.
func (db *DB) EnsureProjectIdentityBackfillQueued(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := db.requireWritable(); err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning project identity backfill queue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := ensureProjectIdentityBackfillQueuedTx(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing project identity backfill queue: %w", err)
	}
	return nil
}

// ProjectIdentityBackfillStatus reports persisted job progress. When an
// archive has missing snapshots but predates the status row, it reports pending
// without mutating the database so daemonless readers cannot silently degrade.
func (db *DB) ProjectIdentityBackfillStatus(
	ctx context.Context,
) (BackgroundMigrationStatus, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	status := BackgroundMigrationStatus{Name: ProjectIdentityBackfillName}
	err := db.getReader().QueryRowContext(ctx, `
		SELECT name, state, total_items, completed_items, last_error,
			started_at, updated_at, completed_at
		FROM background_migrations WHERE name = ?`,
		ProjectIdentityBackfillName,
	).Scan(
		&status.Name, &status.State, &status.TotalItems,
		&status.CompletedItems, &status.LastError, &status.StartedAt,
		&status.UpdatedAt, &status.CompletedAt,
	)
	if err == nil {
		if status.State == "completed" {
			var verified string
			verifyErr := db.getReader().QueryRowContext(ctx,
				`SELECT value FROM stats WHERE key = ?`,
				projectIdentityBackfillVerifiedKey,
			).Scan(&verified)
			if verifyErr == nil && verified == "1" {
				return status, nil
			}
			if verifyErr != nil && !errors.Is(verifyErr, sql.ErrNoRows) {
				return status, fmt.Errorf(
					"checking project identity backfill verification: %w", verifyErr)
			}
			missing, countErr := db.countMissingProjectIdentitySnapshots(ctx)
			if countErr != nil {
				return status, countErr
			}
			if missing > 0 {
				status.State = "pending"
				status.TotalItems = missing
				status.CompletedItems = 0
			}
			return status, nil
		}
		if status.State == "pending" && status.TotalItems == 0 {
			missing, countErr := db.countMissingProjectIdentitySnapshots(ctx)
			if countErr != nil {
				return status, countErr
			}
			status.TotalItems = missing
		}
		return status, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return status, fmt.Errorf("reading project identity backfill status: %w", err)
	}
	missing, err := db.countMissingProjectIdentitySnapshots(ctx)
	if err != nil {
		return status, err
	}
	status.TotalItems = missing
	if missing == 0 {
		status.State = "not_needed"
	} else {
		status.State = "pending"
	}
	return status, nil
}

func (db *DB) countMissingProjectIdentitySnapshots(ctx context.Context) (int, error) {
	var missing int
	if err := db.getReader().QueryRowContext(ctx, `
		SELECT count(*) FROM sessions s
		WHERE NOT EXISTS (
			SELECT 1 FROM session_project_identity_snapshots p
			WHERE p.session_id = s.id
		)`).Scan(&missing); err != nil {
		return 0, fmt.Errorf("counting missing project identity snapshots: %w", err)
	}
	return missing, nil
}

// ProjectIdentityBackfillCandidates returns a bounded set of sessions that do
// not yet have an immutable identity snapshot.
func (db *DB) ProjectIdentityBackfillCandidates(
	ctx context.Context,
) ([]Session, error) {
	return db.ProjectIdentityBackfillCandidatesAfter(ctx, "")
}

// ProjectIdentityBackfillCandidatesAfter returns the next bounded keyset of
// sessions missing immutable identity evidence. afterID prevents each batch
// from rescanning the completed prefix during one backfill run.
func (db *DB) ProjectIdentityBackfillCandidatesAfter(
	ctx context.Context, afterID string,
) ([]Session, error) {
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT s.id, s.project, s.machine, s.agent, s.cwd, s.git_branch,
			s.started_at
		FROM sessions s
		WHERE s.id > ? AND NOT EXISTS (
			SELECT 1 FROM session_project_identity_snapshots p
			WHERE p.session_id = s.id
		)
		ORDER BY s.id
		LIMIT ?`, afterID, projectIdentityBackfillBatchSize)
	if err != nil {
		return nil, fmt.Errorf("querying project identity backfill candidates: %w", err)
	}
	defer rows.Close()
	var sessions []Session
	for rows.Next() {
		var session Session
		if err := rows.Scan(
			&session.ID, &session.Project, &session.Machine, &session.Agent,
			&session.Cwd, &session.GitBranch, &session.StartedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning project identity backfill candidate: %w", err)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating project identity backfill candidates: %w", err)
	}
	return sessions, nil
}

// ApplyProjectIdentityBackfillBatch persists one candidate batch and its
// durable progress in one writer transaction. A failed observation rolls back
// the whole batch so keyset traversal never skips an unpersisted session.
func (db *DB) ApplyProjectIdentityBackfillBatch(
	ctx context.Context, observations []export.ProjectIdentityObservation,
) error {
	if len(observations) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := db.requireWritable(); err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning project identity backfill batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, observation := range observations {
		if strings.TrimSpace(observation.Project) == "" ||
			strings.TrimSpace(observation.Machine) == "" {
			observation.Project = strings.TrimSpace(observation.Project)
			observation.Machine = strings.TrimSpace(observation.Machine)
			observation.RootPath = strings.TrimSpace(observation.RootPath)
			observation.GitBranch = strings.TrimSpace(observation.GitBranch)
			if observation.WorktreeRelationship == "" {
				observation.WorktreeRelationship = export.WorktreeUnknown
			}
			if observation.CheckoutState == "" {
				observation.CheckoutState = export.CheckoutUnknown
			}
			if observation.RemoteResolution == "" {
				observation.RemoteResolution = export.ProjectResolutionUnknown
			}
			if err := upsertSessionProjectIdentitySnapshotExec(
				ctx, tx,
				func(ctx context.Context, query string, args ...any) rowScanner {
					return tx.QueryRowContext(ctx, query, args...)
				},
				observation, false,
			); err != nil {
				return fmt.Errorf(
					"applying unresolved project identity backfill batch: %w", err)
			}
			continue
		}
		if err := upsertProjectIdentityObservationTx(tx, observation); err != nil {
			return fmt.Errorf("applying project identity backfill batch: %w", err)
		}
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE background_migrations SET
			completed_items = completed_items + ?,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE name = ?`, len(observations), ProjectIdentityBackfillName)
	if err != nil {
		return fmt.Errorf("advancing project identity backfill batch: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking project identity backfill batch progress: %w", err)
	}
	if changed == 0 {
		return errors.New("advancing project identity backfill batch: migration row missing")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing project identity backfill batch: %w", err)
	}
	return nil
}

func (db *DB) StartProjectIdentityBackfill(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	missing, err := db.countMissingProjectIdentitySnapshots(ctx)
	if err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	result, err := db.getWriter().ExecContext(ctx, `
		UPDATE background_migrations SET
			state = 'running', last_error = '',
			total_items = completed_items + ?,
			started_at = COALESCE(started_at,
				strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE name = ?`, missing, ProjectIdentityBackfillName)
	if err != nil {
		return fmt.Errorf("starting project identity backfill: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("starting project identity backfill: %w", err)
	}
	if changed == 0 {
		return errors.New("starting project identity backfill: migration row missing")
	}
	return nil
}

func (db *DB) AdvanceProjectIdentityBackfill(ctx context.Context) error {
	return db.updateProjectIdentityBackfill(ctx, `
		UPDATE background_migrations SET
			completed_items = completed_items + 1,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE name = ?`, "advancing")
}

func (db *DB) FailProjectIdentityBackfill(ctx context.Context, cause error) error {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().ExecContext(ctx, `
		UPDATE background_migrations SET
			state = 'failed', last_error = ?,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE name = ?`, message, ProjectIdentityBackfillName)
	if err != nil {
		return fmt.Errorf("recording project identity backfill failure: %w", err)
	}
	return nil
}

func (db *DB) CompleteProjectIdentityBackfill(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning project identity backfill completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
		UPDATE background_migrations SET
			state = 'completed', completed_items = total_items,
			last_error = '',
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now'),
			completed_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE name = ?`, ProjectIdentityBackfillName)
	if err != nil {
		return fmt.Errorf("completing project identity backfill: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking project identity backfill completion: %w", err)
	}
	if changed == 0 {
		return errors.New("completing project identity backfill: migration row missing")
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO stats (key, value) VALUES (?, '1')
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		projectIdentityBackfillVerifiedKey,
	); err != nil {
		return fmt.Errorf("verifying completed project identity backfill: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing project identity backfill completion: %w", err)
	}
	return nil
}

func (db *DB) updateProjectIdentityBackfill(
	ctx context.Context, query, operation string,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	result, err := db.getWriter().ExecContext(
		ctx, query, ProjectIdentityBackfillName,
	)
	if err != nil {
		return fmt.Errorf("%s project identity backfill: %w", operation, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s project identity backfill: %w", operation, err)
	}
	if changed == 0 {
		return fmt.Errorf("%s project identity backfill: migration row missing",
			operation)
	}
	return nil
}
