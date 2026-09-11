package db

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/config"
)

// ApplyMachineAliases resolves configured source ownership using the archive's
// recorded machine adoption decisions.
func (db *DB) ApplyMachineAliases(ctx context.Context, cfg *config.Config) error {
	aliases, err := db.GetMachineAliases(ctx)
	if err != nil {
		return fmt.Errorf("reading source machine aliases: %w", err)
	}
	cfg.ApplyMachineAliases(aliases)
	return nil
}
