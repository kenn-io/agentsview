package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// seedUpgradedUsageCache copies the audited format 23 cache into a private candidate.
func seedUpgradedUsageCache(ctx context.Context, archivePath, candidate, databaseID string) (bool, error) {
	oldPath := usageCacheGenerationPath(archivePath, usageCacheUpgradeFrom, databaseID)
	if _, err := os.Stat(oldPath); os.IsNotExist(err) {
		return false, ctx.Err()
	} else if err != nil {
		return false, err
	}
	lease, err := acquireUsageCacheLease(oldPath)
	if err != nil {
		return false, err
	}
	defer lease.Close()
	probe := probeUsageCache(ctx, oldPath)
	if probe.Err != nil {
		return false, probe.Err
	}
	if !probe.Recognized || probe.FormatVersion != usageCacheUpgradeFrom || probe.SourceDatabaseID != databaseID || probe.RetirementProtocolVersion != usageCacheRetirementProtocolVersion {
		return false, fmt.Errorf("incompatible prior usage cache %s", oldPath)
	}
	source, err := sql.Open("sqlite3", makeDSN(oldPath, true))
	if err != nil {
		return false, err
	}
	_, err = source.ExecContext(ctx, "VACUUM INTO ?", candidate)
	closeErr := source.Close()
	if err != nil {
		return false, fmt.Errorf("copying prior usage cache: %w", err)
	}
	if closeErr != nil {
		return false, closeErr
	}
	if err := os.Chmod(candidate, 0o600); err != nil {
		return false, err
	}
	db, err := openUsageCacheDatabase(ctx, candidate)
	if err != nil {
		return false, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, query := range []string{
		`ALTER TABLE usage_facts ADD COLUMN ranking_output_tokens INTEGER`,
		`ALTER TABLE usage_rollup_exceptions ADD COLUMN ranking_output_tokens INTEGER`,
		usageCacheAccountTotalsSQL,
		`INSERT INTO usage_cache_metadata(key, value) VALUES ('reset_epoch', '0'), ('source_mutation_revision', '0')`,
		`UPDATE usage_cache_metadata SET value = '24' WHERE key = 'format_version'`,
	} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return false, fmt.Errorf("converting usage cache: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if err := db.Close(); err != nil {
		return false, err
	}
	upgraded := probeUsageCache(ctx, candidate)
	if upgraded.Err != nil {
		return false, upgraded.Err
	}
	if !upgraded.Compatible {
		return false, errors.New("upgraded usage cache is incomplete")
	}
	return true, nil
}
