package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/vector"
)

// clearUsageOnlyVectors applies the archive policy to both local vector stores.
// It runs before serving or pushing, even when vector search is disabled.
func clearUsageOnlyVectors(ctx context.Context, cfg config.Config) error {
	if !cfg.ArchiveContent.UsageOnly() {
		return nil
	}
	path := cfg.Vector.ResolvedDBPath(cfg.DataDir)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	lock, ok, err := acquireVectorsWriteLockWithRetry(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("cannot apply usage-only policy while the vector index is being written")
	}
	defer lock.Close()
	for _, spec := range []vector.IndexSpec{vector.MessageIndexSpec(), vector.RecallIndexSpec()} {
		exists, err := vector.StoreExists(ctx, path, spec)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		ix, err := vector.OpenSpec(ctx, path, spec, false, cfg.Vector.Embeddings.MaxInputChars)
		if err != nil {
			return err
		}
		err = ix.Clear(ctx)
		if err := errors.Join(err, ix.Close()); err != nil {
			return fmt.Errorf("clearing usage-only vector index: %w", err)
		}
	}
	return nil
}
