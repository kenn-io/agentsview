package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/config"
)

// MachineOriginKeyPrefix maps a remote-sync host to the installation identity
// its archive reports for itself. Session rows keep the host name; only
// reporting exports and the change stream read this record.
const MachineOriginKeyPrefix = "machine_origin:"

// RecordRemoteMachineOrigin records the installation identity a remote-sync
// host reported. Without it, the host's sessions are attributed to the host
// name, while the host's own archive attributes the same sessions to its
// installation identity, and a consumer that keeps one copy per originating
// machine counts them twice. A missing or malformed identity, as from an older
// remote, keeps the host name. So does this archive's own identity, which a
// host cannot claim.
func (db *DB) RecordRemoteMachineOrigin(ctx context.Context, host, identity string) error {
	if host == "" || host == "local" || config.ValidateInstallationID(identity) != nil {
		return nil
	}
	return db.Update(ctx, func(tx *sql.Tx) error {
		var local, existing string
		err := tx.QueryRowContext(ctx, `SELECT
			COALESCE((SELECT value FROM pg_sync_state WHERE key = ?), ''),
			COALESCE((SELECT value FROM pg_sync_state WHERE key = ?), '')`,
			artifactLocalInstallationStateKey, MachineOriginKeyPrefix+host).Scan(&local, &existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("reading machine origin: %w", err)
		}
		if identity == local || identity == existing {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO pg_sync_state(key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, MachineOriginKeyPrefix+host, identity); err != nil {
			return fmt.Errorf("recording machine origin: %w", err)
		}
		// The change stream names each session's origin, so publish the host's
		// sessions again under the new one.
		if _, err := tx.ExecContext(ctx, `INSERT INTO archive_metadata(key, value)
			VALUES ('conversation_publication_revision', '1')
			ON CONFLICT(key) DO UPDATE SET value = CAST(value AS INTEGER) + 1`); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO conversation_session_changes(session_id, revision, deleted)
			SELECT s.id, CAST(m.value AS INTEGER), s.deleted_at IS NOT NULL
			FROM sessions s, archive_metadata m
			WHERE s.machine = ? AND m.key = 'conversation_publication_revision'
			ON CONFLICT(session_id) DO UPDATE SET revision = excluded.revision, deleted = excluded.deleted`, host)
		if err != nil {
			return fmt.Errorf("republishing sessions from %s: %w", host, err)
		}
		return nil
	})
}
