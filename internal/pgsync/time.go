package pgsync

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// syncTimestampLayout uses microsecond precision to match PostgreSQL's
// timestamp resolution. localSyncTimestampLayout uses millisecond
// precision to match SQLite's datetime resolution. The corresponding
// previousSyncTimestamp/previousLocalSyncTimestamp functions subtract
// 1 microsecond and 1 millisecond respectively.
const syncTimestampLayout = "2006-01-02T15:04:05.000000Z"
const localSyncTimestampLayout = "2006-01-02T15:04:05.000Z"

func formatSyncTimestamp(t time.Time) string {
	return t.UTC().Format(syncTimestampLayout)
}

func normalizeSyncTimestamp(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	ts, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", err
	}
	return formatSyncTimestamp(ts), nil
}

func normalizeLocalSyncTimestamp(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	ts, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", err
	}
	return ts.UTC().Format(localSyncTimestampLayout), nil
}

func previousLocalSyncTimestamp(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	ts, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", err
	}
	return ts.Add(-time.Millisecond).UTC().Format(localSyncTimestampLayout), nil
}

// trustedSQL is a string type for SQL expressions that are known to be
// safe (literals, column references). Using a distinct type prevents
// accidental injection of user input into pgTimestampSQL.
type trustedSQL string

// pgTimestampSQL returns a SQL fragment that formats expr as an ISO
// timestamp. The trustedSQL type ensures only compile-time-known
// expressions are passed.
func pgTimestampSQL(expr trustedSQL) string {
	return "TO_CHAR(" + string(expr) + ", 'YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')"
}

func normalizePGUpdatedAt(ctx context.Context, pg *sql.DB) error {
	const versionKey = "updated_at_format_version"
	const versionValue = "2"
	var version string
	err := pg.QueryRowContext(ctx,
		"SELECT value FROM agentsview.sync_metadata WHERE key = $1",
		versionKey,
	).Scan(&version)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("reading pg timestamp format version: %w", err)
	}
	if version == versionValue {
		return nil
	}

	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin pg timestamp normalization tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		"SELECT pg_advisory_xact_lock(hashtext('agentsview_updated_at_normalize'))",
	); err != nil {
		return fmt.Errorf("acquiring updated_at normalization lock: %w", err)
	}

	// Re-check version after acquiring the lock — another process may
	// have completed the migration while we waited.
	err = tx.QueryRowContext(ctx,
		"SELECT value FROM agentsview.sync_metadata WHERE key = $1",
		versionKey,
	).Scan(&version)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("re-reading pg timestamp format version: %w", err)
	}
	if version == versionValue {
		return nil
	}

	if _, err := tx.ExecContext(ctx, `
		DO $$
		DECLARE
			session_row RECORD;
			normalized_updated_at TEXT;
		BEGIN
			FOR session_row IN
				SELECT id, updated_at
				FROM agentsview.sessions
			LOOP
				BEGIN
					normalized_updated_at := `+pgTimestampSQL("(session_row.updated_at::timestamptz AT TIME ZONE 'UTC')")+`;
				EXCEPTION WHEN OTHERS THEN
					RAISE WARNING 'pgsync: could not normalize updated_at=% for session id=%', session_row.updated_at, session_row.id;
					normalized_updated_at := `+pgTimestampSQL("NOW() AT TIME ZONE 'UTC'")+`;
				END;

				UPDATE agentsview.sessions
				SET updated_at = normalized_updated_at
				WHERE id = session_row.id
					AND updated_at IS DISTINCT FROM normalized_updated_at;
			END LOOP;
		END $$;
	`); err != nil {
		return fmt.Errorf("normalizing pg updated_at values: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		ALTER TABLE agentsview.sessions
		ALTER COLUMN updated_at SET DEFAULT `+pgTimestampSQL("NOW() AT TIME ZONE 'UTC'")); err != nil {
		return fmt.Errorf("updating pg updated_at default: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO agentsview.sync_metadata (key, value)
		VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value
	`, versionKey, versionValue); err != nil {
		return fmt.Errorf("recording pg timestamp format version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit pg timestamp normalization tx: %w", err)
	}
	return nil
}

func (p *PGSync) normalizeSyncTimestamps(ctx context.Context) error {
	p.schemaMu.Lock()
	defer p.schemaMu.Unlock()
	if !p.schemaDone {
		if err := ensureSchema(ctx, p.pg); err != nil {
			return err
		}
		p.schemaDone = true
	}

	return normalizeLocalSyncStateTimestamps(p.local)
}

func normalizeLocalSyncStateTimestamps(local interface {
	GetSyncState(key string) (string, error)
	SetSyncState(key, value string) error
}) error {
	// Only normalize last_push_at for push-only sync.
	value, err := local.GetSyncState("last_push_at")
	if err != nil {
		return fmt.Errorf("reading last_push_at: %w", err)
	}
	if value == "" {
		return nil
	}
	normalized, err := normalizeLocalSyncTimestamp(value)
	if err != nil {
		return fmt.Errorf("normalizing last_push_at: %w", err)
	}
	if normalized == value {
		return nil
	}
	if err := local.SetSyncState("last_push_at", normalized); err != nil {
		return fmt.Errorf("writing last_push_at: %w", err)
	}
	return nil
}
