package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// migrateRecallReviewStateConstraintLocked removes the legacy SQL enum from
// recall_entries. Review-state policy belongs to the shared Go write boundary;
// keeping it in the table would require a table migration for every new state.
// The caller must hold db.mu and invoke this before schema initialization so
// schema.sql can recreate the dropped indexes and triggers canonically.
func migrateRecallReviewStateConstraintLocked(
	w *writerHandle,
) (retErr error) {
	var tableSQL string
	err := w.QueryRow(`
		SELECT sql FROM sqlite_master
		WHERE type = 'table' AND name = 'recall_entries'
	`).Scan(&tableSQL)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf(
			"probing recall_entries review constraint: %w", err,
		)
	}
	if !strings.Contains(tableSQL, "CHECK (review_state IN") {
		return nil
	}

	ctx := context.Background()
	conn, err := w.Conn(ctx)
	if err != nil {
		return fmt.Errorf(
			"acquiring recall review migration connection: %w", err,
		)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()

	var foreignKeys int
	if err := conn.QueryRowContext(
		ctx, `PRAGMA foreign_keys`,
	).Scan(&foreignKeys); err != nil {
		return fmt.Errorf("reading foreign-key mode: %w", err)
	}
	if _, err := conn.ExecContext(
		ctx, `PRAGMA foreign_keys = OFF`,
	); err != nil {
		return fmt.Errorf("disabling foreign keys: %w", err)
	}
	defer func() {
		if foreignKeys == 0 {
			return
		}
		if _, err := conn.ExecContext(
			ctx, `PRAGMA foreign_keys = ON`,
		); err != nil {
			retErr = errors.Join(retErr,
				fmt.Errorf("restoring foreign keys: %w", err))
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning recall review migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(
		ctx, recallReviewStateMigrationPrepareSQL,
	); err != nil {
		return fmt.Errorf("preparing recall review migration: %w", err)
	}
	var sourceCount, replacementCount int64
	if err := tx.QueryRowContext(ctx, `
		SELECT
			(SELECT count(*) FROM recall_entries),
			(SELECT count(*) FROM recall_entries_review_state_v2)
	`).Scan(&sourceCount, &replacementCount); err != nil {
		return fmt.Errorf("counting migrated recall entries: %w", err)
	}
	if sourceCount != replacementCount {
		return fmt.Errorf(
			"migrating recall review state copied %d of %d entries",
			replacementCount, sourceCount,
		)
	}
	if _, err := tx.ExecContext(
		ctx, recallReviewStateMigrationSwapSQL,
	); err != nil {
		return fmt.Errorf("swapping migrated recall entries: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("checking migrated recall foreign keys: %w", err)
	}
	broken := rows.Next()
	if err := rows.Close(); err != nil {
		return fmt.Errorf("closing recall foreign-key check: %w", err)
	}
	if broken {
		return errors.New("migrated recall entries failed foreign-key check")
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing recall review migration: %w", err)
	}
	return nil
}

const recallReviewStateMigrationPrepareSQL = `
CREATE TABLE recall_entries_review_state_v2 (
    id                TEXT PRIMARY KEY,
    type              TEXT NOT NULL,
    scope             TEXT NOT NULL,
    status            TEXT NOT NULL DEFAULT 'accepted',
    review_state      TEXT NOT NULL DEFAULT 'unreviewed_auto',
    title             TEXT NOT NULL,
    body              TEXT NOT NULL,
    trigger           TEXT NOT NULL DEFAULT '',
    confidence        REAL,
    uncertainty       TEXT NOT NULL DEFAULT '',
    project           TEXT NOT NULL DEFAULT '',
    cwd               TEXT NOT NULL DEFAULT '',
    git_branch        TEXT NOT NULL DEFAULT '',
    agent             TEXT NOT NULL DEFAULT '',
    source_session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    source_episode_id TEXT NOT NULL DEFAULT '',
    source_run_id     TEXT NOT NULL DEFAULT '',
    extractor_method  TEXT NOT NULL DEFAULT '',
    model             TEXT NOT NULL DEFAULT '',
    transferable      INTEGER NOT NULL DEFAULT 0,
    provenance_ok     INTEGER NOT NULL DEFAULT 0,
    supersedes_entry_id TEXT NOT NULL DEFAULT '',
    superseded_by_entry_id TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL
        DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at        TEXT NOT NULL
        DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
INSERT INTO recall_entries_review_state_v2 (
    rowid, id, type, scope, status, review_state, title, body, trigger,
    confidence, uncertainty, project, cwd, git_branch, agent,
    source_session_id, source_episode_id, source_run_id, extractor_method,
    model, transferable, provenance_ok, supersedes_entry_id,
    superseded_by_entry_id, created_at, updated_at
)
SELECT
    rowid, id, type, scope, status, review_state, title, body, trigger,
    confidence, uncertainty, project, cwd, git_branch, agent,
    source_session_id, source_episode_id, source_run_id, extractor_method,
    model, transferable, provenance_ok, supersedes_entry_id,
    superseded_by_entry_id, created_at, updated_at
FROM recall_entries;
`

const recallReviewStateMigrationSwapSQL = `
DROP TABLE recall_entries;
ALTER TABLE recall_entries_review_state_v2 RENAME TO recall_entries;
`
