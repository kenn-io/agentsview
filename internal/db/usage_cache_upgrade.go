package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/mattn/go-sqlite3"
)

func unusableUsageCache(err error, conversion bool) bool {
	sqliteErr, ok := errors.AsType[sqlite3.Error](err)
	return ok && (sqliteErr.Code == sqlite3.ErrCorrupt || sqliteErr.Code == sqlite3.ErrNotADB || conversion && sqliteErr.Code == sqlite3.ErrError)
}

// seedUpgradedUsageCache copies an audited older cache into a private candidate.
func seedUpgradedUsageCache(ctx context.Context, archivePath, candidate, databaseID string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var oldPath string
	var from int
	for version := usageCacheUpgradeTo - 1; version >= usageCacheUpgradeFrom; version-- {
		path := usageCacheGenerationPath(archivePath, version, databaseID)
		if _, err := os.Stat(path); err == nil {
			oldPath, from = path, version
			break
		} else if !os.IsNotExist(err) {
			return false, err
		}
	}
	if oldPath == "" {
		return false, ctx.Err()
	}
	lease, err := acquireUsageCacheLease(oldPath)
	if err != nil {
		return false, err
	}
	defer lease.Close()
	probe := probeUsageCache(ctx, oldPath)
	if probe.Err != nil {
		if unusableUsageCache(probe.Err, false) {
			log.Printf("warning: rebuilding unusable usage cache: %v", probe.Err)
			return false, nil
		}
		return false, probe.Err
	}
	if !probe.Recognized || probe.FormatVersion != from || probe.SourceDatabaseID != databaseID || probe.RetirementProtocolVersion != usageCacheRetirementProtocolVersion {
		log.Printf("warning: rebuilding incompatible usage cache %s", oldPath)
		return false, nil
	}
	source, err := sql.Open("sqlite3", makeDSN(oldPath, true))
	if err != nil {
		return false, err
	}
	_, err = source.ExecContext(ctx, "VACUUM INTO ?", candidate)
	closeErr := source.Close()
	if closeErr != nil {
		return false, closeErr
	}
	if err != nil {
		if unusableUsageCache(err, false) {
			log.Printf("warning: rebuilding unusable usage cache: %v", err)
			return false, nil
		}
		return false, fmt.Errorf("copying prior usage cache: %w", err)
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
	var queries []string
	if from <= 22 {
		queries = append(queries,
			`ALTER TABLE usage_facts ADD COLUMN cost_status TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE usage_rollup_exceptions ADD COLUMN cost_status TEXT NOT NULL DEFAULT ''`,
			`DELETE FROM usage_rollup_timezones`,
			`UPDATE usage_cache_metadata SET value = '' WHERE key = 'backfill_completed_at'`,
		)
	}
	if from == 21 {
		queries = append(queries, `DROP INDEX usage_facts_claude_identity`, usageCacheClaudeIdentityIndexSQL)
	}
	queries = append(queries,
		`ALTER TABLE usage_facts ADD COLUMN ranking_output_tokens INTEGER`,
		`ALTER TABLE usage_rollup_exceptions ADD COLUMN ranking_output_tokens INTEGER`,
		usageCacheAccountTotalsSQL,
		`INSERT INTO usage_cache_metadata(key, value) VALUES ('reset_epoch', '0'), ('source_mutation_revision', '0')`,
	)
	for _, query := range queries {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			if unusableUsageCache(err, true) {
				log.Printf("warning: rebuilding unusable usage cache: %v", err)
				return false, nil
			}
			return false, fmt.Errorf("converting usage cache: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE usage_cache_metadata SET value = ? WHERE key = 'format_version'`, usageCacheUpgradeTo); err != nil {
		if unusableUsageCache(err, true) {
			log.Printf("warning: rebuilding unusable usage cache: %v", err)
			return false, nil
		}
		return false, fmt.Errorf("updating usage cache format: %w", err)
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
		log.Printf("warning: rebuilding incomplete usage cache %s", oldPath)
		return false, nil
	}
	return true, nil
}
