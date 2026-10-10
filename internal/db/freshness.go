package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/parser"
)

// GetProviderStatHash returns the per-component stat digest stored against
// (agent, file_path), if any. Returns (0, false, nil) when no row exists,
// which lets the engine treat the absence as "not yet populated" and fall
// through to a real fingerprint+parse on the first sync after a provider
// opts in. The integer reads signed (SQLite INTEGER is int64) but the
// hash is always non-negative in practice; callers do not depend on the
// sign.
func (db *DB) GetProviderStatHash(
	ctx context.Context,
	agent parser.AgentType,
	filePath string,
) (uint64, bool, error) {
	var h int64
	err := db.getReader().QueryRowContext(ctx,
		"SELECT stat_hash FROM provider_freshness"+
			" WHERE agent = ? AND file_path = ?",
		string(agent), filePath,
	).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf(
			"reading provider_freshness for %s/%s: %w", agent, filePath, err)
	}
	return uint64(h), true, nil
}

// UpsertProviderStatHash records the per-component stat digest against
// (agent, file_path). Replaces any existing row. updated_at is refreshed
// to the current time so future cache-eviction sweeps can drop stale
// entries for archived files.
func (db *DB) UpsertProviderStatHash(
	ctx context.Context,
	agent parser.AgentType,
	filePath string,
	hash uint64,
) error {
	_, err := db.getWriter().ExecContext(ctx,
		"INSERT INTO provider_freshness (agent, file_path, stat_hash)"+
			" VALUES (?, ?, ?)"+
			" ON CONFLICT (agent, file_path) DO UPDATE SET"+
			"   stat_hash = excluded.stat_hash,"+
			"   updated_at = excluded.updated_at",
		string(agent), filePath, int64(hash),
	)
	if err != nil {
		return fmt.Errorf(
			"upserting provider_freshness for %s/%s: %w", agent, filePath, err)
	}
	return nil
}

// DeleteProviderStatHash drops the freshness row for (agent, file_path).
// Callers use this on tombstoning paths so a future discovery of the same
// path starts with a clean absence and forces a full parse.
func (db *DB) DeleteProviderStatHash(
	ctx context.Context,
	agent parser.AgentType,
	filePath string,
) error {
	_, err := db.getWriter().ExecContext(ctx,
		"DELETE FROM provider_freshness WHERE agent = ? AND file_path = ?",
		string(agent), filePath,
	)
	if err != nil {
		return fmt.Errorf(
			"deleting provider_freshness for %s/%s: %w", agent, filePath, err)
	}
	return nil
}
