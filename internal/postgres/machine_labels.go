package postgres

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
	rows, err := s.pg.QueryContext(ctx, `
		SELECT key, value FROM sync_metadata
		WHERE key LIKE $1 ESCAPE '\'`, strings.ReplaceAll(prefix, "_", "\\_")+"%")
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

func (s *Sync) syncMachineMetadata(ctx context.Context) error {
	labels, err := s.local.GetMachineLabels(ctx)
	if err != nil {
		return err
	}
	aliases, err := s.local.GetMachineAliases(ctx)
	if err != nil {
		return err
	}
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting machine metadata sync: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for prefix, values := range map[string]map[string]string{
		db.MachineLabelKeyPrefix: labels,
		db.MachineAliasKeyPrefix: aliases,
	} {
		for machine, value := range values {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO sync_metadata (key, value) VALUES ($1, $2)
				ON CONFLICT(key) DO UPDATE SET value = excluded.value
				WHERE sync_metadata.value IS DISTINCT FROM excluded.value`,
				prefix+machine, value); err != nil {
				return fmt.Errorf("syncing machine metadata: %w", err)
			}
		}
	}
	return tx.Commit()
}
