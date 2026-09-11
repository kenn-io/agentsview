package duckdb

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

func (s *Sync) syncMachineMetadata(ctx context.Context) error {
	labels, err := s.local.GetMachineLabels(ctx)
	if err != nil {
		return err
	}
	aliases, err := s.local.GetMachineAliases(ctx)
	if err != nil {
		return err
	}
	tx, err := s.duck.BeginTx(ctx, nil)
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
				INSERT INTO sync_metadata (key, value) VALUES (?, ?)
				ON CONFLICT(key) DO UPDATE SET value = excluded.value`, prefix+machine, value); err != nil {
				return fmt.Errorf("syncing machine metadata: %w", err)
			}
		}
	}
	return tx.Commit()
}
