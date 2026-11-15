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
	for version := usageCacheUpgradeTo - 1; version >= usageCacheUpgradeFrom; version-- {
		path := usageCacheGenerationPath(archivePath, version, databaseID)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return false, err
		}
		seeded, err := seedUsageCacheFrom(ctx, archivePath, candidate, databaseID, path, version)
		if err != nil || seeded {
			return seeded, err
		}
		if err := removeUsageCacheFiles(candidate); err != nil {
			return false, err
		}
		file, err := os.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return false, err
		}
		if err := file.Close(); err != nil {
			return false, err
		}
	}
	return false, ctx.Err()
}

func seedUsageCacheFrom(ctx context.Context, archivePath, candidate, databaseID, oldPath string, from int) (bool, error) {
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
	conn, err := db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if from <= 22 {
		if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS usage_archive`, makeDSN(archivePath, true)); err != nil {
			return false, fmt.Errorf("opening archive for cache upgrade: %w", err)
		}
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if from <= 22 {
		// Earlier formats did not store event cost statuses. Refill those owners
		// from the archive using the normal extraction path.
		if _, err := tx.ExecContext(ctx, `DELETE FROM usage_cached_sessions WHERE session_id IN (
			SELECT session_id FROM usage_archive.usage_events WHERE cost_status != ''
		)`); err != nil {
			return false, fmt.Errorf("refreshing recorded cost owners: %w", err)
		}
	}
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
		fmt.Sprintf(`UPDATE usage_cache_metadata SET value = '%d' WHERE key = 'format_version'`, usageCacheUpgradeTo),
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
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if err := conn.Close(); err != nil {
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
