package clickhouse

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

func (s *Store) GetMachineLabels(ctx context.Context) (map[string]string, error) {
	return s.getMachineMetadata(ctx, db.MachineLabelKeyPrefix)
}

func (s *Store) GetMachineAliases(ctx context.Context) (map[string]string, error) {
	return s.getMachineMetadata(ctx, db.MachineAliasKeyPrefix)
}

func (s *Store) getMachineMetadata(ctx context.Context, prefix string) (map[string]string, error) {
	rows, err := s.queryContext(ctx, `
		SELECT key, value FROM sync_metadata
		WHERE startsWith(key, ?)`, prefix)
	if err != nil {
		return nil, fmt.Errorf("reading clickhouse machine metadata: %w", err)
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
