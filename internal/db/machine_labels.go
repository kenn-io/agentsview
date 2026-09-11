package db

import (
	"context"
	"fmt"
	"strings"
)

// MachineLabelKeyPrefix identifies display labels in archive and mirror metadata.
const MachineLabelKeyPrefix = "machine_label:"

// MachineAliasKeyPrefix identifies proven former archive owners.
const MachineAliasKeyPrefix = "machine_alias:"

// GetMachineLabels returns explicitly recorded labels, keyed by machine identity.
func (db *DB) GetMachineLabels(ctx context.Context) (map[string]string, error) {
	return db.getMachineMetadata(ctx, MachineLabelKeyPrefix)
}

// GetMachineAliases returns former machine keys and their canonical identities.
func (db *DB) GetMachineAliases(ctx context.Context) (map[string]string, error) {
	return db.getMachineMetadata(ctx, MachineAliasKeyPrefix)
}

func (db *DB) getMachineMetadata(ctx context.Context, prefix string) (map[string]string, error) {
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT key, value FROM pg_sync_state
		WHERE key LIKE ? ESCAPE '\'`, strings.ReplaceAll(prefix, "_", "\\_")+"%")
	if err != nil {
		return nil, fmt.Errorf("reading machine metadata: %w", err)
	}
	defer rows.Close()
	metadata := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		metadata[strings.TrimPrefix(key, prefix)] = value
	}
	return metadata, rows.Err()
}

// CanonicalMachineFilter resolves recorded aliases in a comma-separated filter.
func CanonicalMachineFilter(machine string, aliases map[string]string) string {
	machines := strings.Split(machine, ",")
	for i, key := range machines {
		if canonical, ok := aliases[strings.TrimSpace(key)]; ok {
			machines[i] = canonical
		}
	}
	return strings.Join(machines, ",")
}

// ResolveMachineFilter reads aliases for direct archive and mirror queries.
func ResolveMachineFilter(ctx context.Context, store Store, machine string) (string, error) {
	if machine == "" {
		return "", nil
	}
	aliases, err := store.GetMachineAliases(ctx)
	if err != nil {
		return "", err
	}
	if local, ok := store.(*DB); ok {
		var identity string
		err := local.getReader().QueryRowContext(ctx, `SELECT COALESCE(
			(SELECT value FROM pg_sync_state WHERE key = ?), '')`, artifactLocalInstallationStateKey).Scan(&identity)
		if err != nil {
			return "", fmt.Errorf("reading local installation identity: %w", err)
		}
		if identity != "" {
			aliases["local"] = identity
		}
	}
	return CanonicalMachineFilter(machine, aliases), nil
}
