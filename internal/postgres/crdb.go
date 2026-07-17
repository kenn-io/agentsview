package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// serverIsCockroachDBPG reports whether the connected server is CockroachDB
// speaking the PostgreSQL wire protocol rather than PostgreSQL itself.
// CockroachDB lacks several primitives the shared-storage migrations rely on
// (advisory locks, LOCK TABLE, temporary tables, trigger column lists), so
// migration code paths branch on this instead of failing schema setup.
func serverIsCockroachDBPG(ctx context.Context, pg *sql.DB) (bool, error) {
	var version string
	if err := pg.QueryRowContext(ctx, `SELECT version()`).Scan(&version); err != nil {
		return false, fmt.Errorf("detecting PostgreSQL server flavor: %w", err)
	}
	return strings.Contains(version, "CockroachDB"), nil
}

// isSerializationFailure reports a transaction conflict (SQLSTATE 40001).
// CockroachDB's SERIALIZABLE isolation surfaces the conflicts that
// PostgreSQL's explicit locks would have blocked on as retryable 40001
// errors, so lock-free CockroachDB transactions retry on this instead.
func isSerializationFailure(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "40001")
}

// isFeatureUnimplemented reports SQLSTATE 0A000 (feature_not_supported),
// which CockroachDB returns for parseable but unimplemented statements such
// as CREATE TRIGGER before v24.3.
func isFeatureUnimplemented(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "0A000")
}

// isTriggerTypeUnsupported reports the CREATE FUNCTION ... RETURNS trigger
// failure that CockroachDB versions without trigger support return before
// the CREATE TRIGGER statement is ever reached (SQLSTATE 42704 with "type
// \"trigger\" does not exist", observed on v23.2).
func isTriggerTypeUnsupported(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "42704") && strings.Contains(msg, "trigger")
}

// isDuplicateObject reports SQLSTATE 42710. Two migrations racing to create
// a trigger outside a table lock can both pass DROP TRIGGER IF EXISTS and
// collide on the CREATE TRIGGER that follows.
func isDuplicateObject(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "42710")
}
