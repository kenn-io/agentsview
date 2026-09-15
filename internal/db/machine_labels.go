package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/uptrace/bun"
)

// MachineLabelKeyPrefix identifies display labels in archive and mirror metadata.
const MachineLabelKeyPrefix = "machine_label:"

// MachineAliasKeyPrefix identifies proven former archive owners.
const MachineAliasKeyPrefix = "machine_alias:"

// GetMachineLabels returns explicitly recorded labels, keyed by machine identity.
func (s *BunStore) GetMachineLabels(ctx context.Context) (map[string]string, error) {
	return s.getMachineMetadata(ctx, MachineLabelKeyPrefix)
}

// GetMachineAliases returns former machine keys and their canonical identities.
func (s *BunStore) GetMachineAliases(ctx context.Context) (map[string]string, error) {
	return s.getMachineMetadata(ctx, MachineAliasKeyPrefix)
}

func (s *BunStore) getMachineMetadata(ctx context.Context, prefix string) (map[string]string, error) {
	var rows []struct {
		Key   string `bun:"key"`
		Value string `bun:"value"`
	}
	err := s.view(ctx, func(store bun.IDB) error {
		return store.NewSelect().Table(s.backend.Capabilities().MachineMetadataTable).
			Column("key", "value").Where("key LIKE ? ESCAPE '\\'", strings.ReplaceAll(prefix, "_", "\\_")+"%").Scan(ctx, &rows)
	})
	if err != nil {
		return nil, fmt.Errorf("reading machine metadata: %w", err)
	}
	metadata := make(map[string]string, len(rows))
	for _, row := range rows {
		metadata[strings.TrimPrefix(row.Key, prefix)] = row.Value
	}
	return metadata, nil
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
