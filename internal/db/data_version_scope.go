package db

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

// dataVersionScopes maps a parser data version to the providers whose stored
// output it changes. A declared version reparses those providers' sources in
// place; an undeclared version requires a full rebuild.
//
// Declare a version only when the reparse preserves source and session
// identities, and prove that with a regression test. List every provider
// whose output changes, including each provider that shares the changed
// parser. A provider left out keeps stale output marked as current.
// Validate the map with validateDataVersionScopes.
var dataVersionScopes = map[int][]parser.AgentType{
	132: {parser.AgentClaude, parser.AgentCowork, parser.AgentQoder, parser.AgentIcodemate, parser.AgentGrok},
}

// firstScopedDataVersion is the oldest version that may be declared. Released
// binaries treated every earlier version as a full rebuild, and their retry
// rows cannot be told apart from complete rows at the archive version.
const firstScopedDataVersion = 131

// scopedUpgradePendingKey is the stats key that keeps startup sync and cold
// reads waiting until a full sync pass has reparsed affected sources.
const scopedUpgradePendingKey = "scoped_upgrade_pending"

// ScopedDataVersionProviders returns the providers to reparse when an
// archive moves from one parser version to another. It returns nil when any
// version in the range requires a full rebuild.
func ScopedDataVersionProviders(from, to int) []parser.AgentType {
	if from <= 0 || from >= to {
		return nil
	}
	var affected []parser.AgentType
	for version := from + 1; version <= to; version++ {
		providers := dataVersionScopes[version]
		if len(providers) == 0 {
			return nil
		}
		affected = append(affected, providers...)
	}
	slices.Sort(affected)
	return slices.Compact(affected)
}

// AgentDataVersion returns the newest parser version that changed the stored
// output of a session agent. Cache keys use it so a scoped upgrade does not
// invalidate unaffected agents.
func AgentDataVersion(agent parser.AgentType) int {
	for version := dataVersion; version > 0; version-- {
		providers := dataVersionScopes[version]
		if len(providers) == 0 {
			return version
		}
		for _, provider := range providers {
			if slices.Contains(parser.ProviderStoredAgents(provider), agent) {
				return version
			}
		}
	}
	return 0
}

// fullRebuildDataVersion returns the newest parser version that requires a
// full rebuild.
func fullRebuildDataVersion() int {
	for version := dataVersion; version > 0; version-- {
		if len(dataVersionScopes[version]) == 0 {
			return version
		}
	}
	return 0
}

// validateDataVersionScopes rejects declarations that a scoped upgrade cannot
// apply safely.
func validateDataVersionScopes(scopes map[int][]parser.AgentType) error {
	for version, providers := range scopes {
		if len(providers) == 0 {
			return fmt.Errorf("data version %d: declaration lists no providers", version)
		}
		for _, provider := range providers {
			if _, ok := parser.AgentByType(provider); !ok {
				return fmt.Errorf(
					"data version %d: %q is not a registered provider",
					version, provider,
				)
			}
		}
		if version < firstScopedDataVersion || version > dataVersion {
			return fmt.Errorf(
				"data version %d: declare only versions %d through %d",
				version, firstScopedDataVersion, dataVersion,
			)
		}
	}
	return nil
}

// CompleteScopedUpgrade records that a full sync pass reparsed the sources of
// every scoped upgrade up to the running parser version.
func (db *DB) CompleteScopedUpgrade(ctx context.Context) error {
	return db.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"DELETE FROM stats WHERE key = ? AND value <= ?",
			scopedUpgradePendingKey, dataVersion,
		)
		return err
	})
}

func advanceDataVersion(ctx context.Context, tx *sql.Tx, target int) error {
	current, _, err := readUserVersion(ctx, tx)
	if err != nil {
		return fmt.Errorf("reading data version: %w", err)
	}
	providers := ScopedDataVersionProviders(current, target)
	if len(providers) > 0 {
		var agents []parser.AgentType
		for _, provider := range providers {
			agents = append(agents, parser.ProviderStoredAgents(provider)...)
		}
		if err := invalidateAgentParseCaches(ctx, tx, agents); err != nil {
			return err
		}
		if err := promoteUnaffectedSessions(ctx, tx, current, target, agents); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO stats(key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = max(value, excluded.value)`,
			scopedUpgradePendingKey, target,
		); err != nil {
			return fmt.Errorf("recording pending scoped upgrade: %w", err)
		}
	}
	return writeUserVersion(ctx, tx, max(current, target))
}

// invalidateAgentParseCaches deletes skip and failure entries that belong to
// the affected agents. Entries without an agent qualifier could belong to any
// provider, so they are deleted too.
func invalidateAgentParseCaches(
	ctx context.Context, tx *sql.Tx, agents []parser.AgentType,
) error {
	caches := []struct{ table, key string }{
		{"skipped_files", "file_path"},
		{"source_failures", "cache_key"},
	}
	for _, cache := range caches {
		where := []string{"instr(" + cache.key + ", ?) = 0"}
		args := []any{ProviderAgentSkipMarker}
		for _, agent := range agents {
			qualifier := ProviderAgentSkipMarker + string(agent)
			where = append(where,
				"instr("+cache.key+" || '?', ?) > 0",
				"instr("+cache.key+", ?) > 0",
			)
			args = append(args, qualifier+"?", qualifier+"&")
		}
		query := "DELETE FROM " + cache.table + " WHERE " + strings.Join(where, " OR ")
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("invalidating %s parser cache: %w", cache.table, err)
		}
	}
	return nil
}

// promoteUnaffectedSessions stamps complete rows of unaffected agents with the
// target version. Rows below the archive version are retry markers and stay
// stale.
func promoteUnaffectedSessions(
	ctx context.Context, tx *sql.Tx, current, target int,
	agents []parser.AgentType,
) error {
	args := []any{target, current, target}
	for _, agent := range agents {
		args = append(args, string(agent))
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(agents)), ",")
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET data_version = ?
		 WHERE data_version >= ? AND data_version < ?
		   AND agent NOT IN (`+placeholders+`)`,
		args...,
	); err != nil {
		return fmt.Errorf("advancing unaffected parser versions: %w", err)
	}
	return nil
}
