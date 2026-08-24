// Package rawcheckpoint owns the laptop's small local SQLite transport-state
// database for hosted raw sync. It records device identity and per-source
// acknowledged head receipts and generations. It is not a chat archive;
// deleting it forces full source reconciliation and deletes no server data.
package rawcheckpoint

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

const schemaVersion = 1

// SourceHead is the last server-acknowledged head of one logical source.
type SourceHead struct {
	ManifestID string
	Receipt    string
	Generation int64
	UpdatedAt  time.Time
}

// Store is the durable checkpoint database.
type Store struct {
	db *sql.DB
}

func (s *Store) withImmediateWrite(
	ctx context.Context,
	operation string,
	fn func(*sql.Conn) error,
) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("rawcheckpoint: %s: %w", operation, err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("rawcheckpoint: %s: %w", operation, err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if err := fn(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("rawcheckpoint: %s: %w", operation, err)
	}
	committed = true
	return nil
}

var ErrMissingReceipt = errors.New(
	"rawcheckpoint: head advancement requires a durable receipt")

var ErrHeadConflict = errors.New(
	"rawcheckpoint: source head already advanced past the expected parent receipt")

var ErrDeviceNotConfigured = errors.New(
	"rawcheckpoint: head advancement requires a configured device")

var ErrDeviceMismatch = errors.New(
	"rawcheckpoint: head advancement is fenced to the configured device")

// Open opens (creating if needed) the checkpoint database at path.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open(checkpointDriverName, checkpointDSN(path, false))
	if err != nil {
		return nil, fmt.Errorf("rawcheckpoint: open: %w", err)
	}
	store := &Store{db: db}
	if err := store.init(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) init(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("rawcheckpoint: read schema version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("rawcheckpoint: database schema %d is newer than supported %d",
			version, schemaVersion)
	}
	if version == schemaVersion {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rawcheckpoint: begin schema: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS device_config (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			device_id TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS raw_sources (
			provider TEXT NOT NULL,
			configured_root_id TEXT NOT NULL,
			source_key TEXT NOT NULL,
			head_manifest_id TEXT NOT NULL DEFAULT '',
			head_receipt TEXT NOT NULL DEFAULT '',
			head_generation INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (provider, configured_root_id, source_key)
		)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("rawcheckpoint: create schema: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("rawcheckpoint: set schema version: %w", err)
	}
	return tx.Commit()
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// SetDevice records this laptop's device identity, replacing any previously
// recorded value so re-provisioning overwrites instead of failing. Changing
// the device identity clears all per-source heads in the same transaction,
// because the server chains heads per device and a re-provisioned device
// starts from an empty chain; recording the same device again is a no-op.
func (s *Store) SetDevice(ctx context.Context, deviceID string) error {
	if deviceID == "" {
		return fmt.Errorf("rawcheckpoint: device ID is required")
	}
	return s.withImmediateWrite(ctx, "set device", func(conn *sql.Conn) error {
		var current string
		err := conn.QueryRowContext(ctx,
			`SELECT device_id FROM device_config WHERE id = 1`).Scan(&current)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := conn.ExecContext(ctx,
				`INSERT INTO device_config (id, device_id, created_at) VALUES (1, ?, ?)`,
				deviceID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return fmt.Errorf("rawcheckpoint: set device: %w", err)
			}
		case err != nil:
			return fmt.Errorf("rawcheckpoint: set device: %w", err)
		case current == deviceID:
			return nil
		default:
			if _, err := conn.ExecContext(ctx,
				`UPDATE device_config SET device_id = ?, created_at = ? WHERE id = 1`,
				deviceID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return fmt.Errorf("rawcheckpoint: set device: %w", err)
			}
			if _, err := conn.ExecContext(ctx, `DELETE FROM raw_sources`); err != nil {
				return fmt.Errorf("rawcheckpoint: set device: %w", err)
			}
		}
		return nil
	})
}

// Device returns the recorded device identity and whether one has been set.
func (s *Store) Device(ctx context.Context) (string, bool, error) {
	var deviceID string
	err := s.db.QueryRowContext(ctx,
		`SELECT device_id FROM device_config WHERE id = 1`).Scan(&deviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("rawcheckpoint: read device: %w", err)
	}
	return deviceID, true, nil
}

// SourceHead returns the acknowledged head for one logical source.
func (s *Store) SourceHead(
	ctx context.Context,
	provider parser.AgentType,
	configuredRootID, sourceKey string,
) (SourceHead, bool, error) {
	var head SourceHead
	var updated string
	err := s.db.QueryRowContext(ctx,
		`SELECT head_manifest_id, head_receipt, head_generation, updated_at
		FROM raw_sources
		WHERE provider = ? AND configured_root_id = ? AND source_key = ?`,
		string(provider), configuredRootID, sourceKey,
	).Scan(&head.ManifestID, &head.Receipt, &head.Generation, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return SourceHead{}, false, nil
	}
	if err != nil {
		return SourceHead{}, false, fmt.Errorf("rawcheckpoint: read head: %w", err)
	}
	head.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return head, true, nil
}

// AdvanceHead records a newly acknowledged head using the server's own
// commit semantics: expectedParentReceipt must equal the stored head receipt
// — empty only while no head exists yet, and then required to be empty, so a
// delayed older commit result can never overwrite a newer acknowledged head
// nor manufacture a head out of thin air. Replaying the acknowledged head's
// own commit is idempotent. Advancement is fenced to the configured device
// and validated atomically: the caller's deviceID must match the recorded
// device_config identity in the same transaction that performs the
// compare-and-swap, so a stale in-flight ack from a re-provisioned device
// can never repopulate heads that SetDevice cleared. Callers must pass the
// server's durable CommitResult; an empty receipt is refused so no unsafe
// checkpoint can advance.
// A new source must begin at generation one, and each non-replay advancement
// must be exactly the next generation; acknowledgements cannot skip state.
func (s *Store) AdvanceHead(
	ctx context.Context,
	deviceID string,
	provider parser.AgentType,
	configuredRootID, sourceKey string,
	expectedParentReceipt string,
	commit rawsync.CommitResult,
) error {
	if commit.Receipt == "" || commit.ManifestID == "" {
		return ErrMissingReceipt
	}
	if err := rawsync.ValidateCommitResult(commit); err != nil {
		return fmt.Errorf("rawcheckpoint: invalid commit result: %w", err)
	}
	return s.withImmediateWrite(ctx, "advance head", func(conn *sql.Conn) error {
		var configured string
		err := conn.QueryRowContext(ctx,
			`SELECT device_id FROM device_config WHERE id = 1`).Scan(&configured)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrDeviceNotConfigured
		case err != nil:
			return fmt.Errorf("rawcheckpoint: advance head: read device: %w", err)
		case configured != deviceID:
			return ErrDeviceMismatch
		}

		var current SourceHead
		err = conn.QueryRowContext(ctx,
			`SELECT head_manifest_id, head_receipt, head_generation
			 FROM raw_sources
			 WHERE provider = ? AND configured_root_id = ? AND source_key = ?`,
			string(provider), configuredRootID, sourceKey,
		).Scan(&current.ManifestID, &current.Receipt, &current.Generation)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if expectedParentReceipt != "" || commit.Generation != 1 {
				return ErrHeadConflict
			}
			_, err = conn.ExecContext(ctx, `INSERT INTO raw_sources
				(provider, configured_root_id, source_key,
				 head_manifest_id, head_receipt, head_generation, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				string(provider), configuredRootID, sourceKey,
				commit.ManifestID, commit.Receipt, commit.Generation,
				time.Now().UTC().Format(time.RFC3339Nano))
			if err != nil {
				return fmt.Errorf("rawcheckpoint: advance head: %w", err)
			}
			return nil
		case err != nil:
			return fmt.Errorf("rawcheckpoint: advance head: read head: %w", err)
		}

		if current.ManifestID == commit.ManifestID &&
			current.Receipt == commit.Receipt &&
			current.Generation == commit.Generation {
			return nil
		}
		if commit.Generation-current.Generation != 1 ||
			current.Receipt != expectedParentReceipt {
			return ErrHeadConflict
		}
		result, err := conn.ExecContext(ctx, `UPDATE raw_sources SET
			head_manifest_id = ?, head_receipt = ?, head_generation = ?, updated_at = ?
			WHERE provider = ? AND configured_root_id = ? AND source_key = ?`,
			commit.ManifestID, commit.Receipt, commit.Generation,
			time.Now().UTC().Format(time.RFC3339Nano),
			string(provider), configuredRootID, sourceKey)
		if err != nil {
			return fmt.Errorf("rawcheckpoint: advance head: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("rawcheckpoint: advance head: %w", err)
		}
		if rows != 1 {
			return ErrHeadConflict
		}
		return nil
	})
}
