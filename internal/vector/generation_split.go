package vector

import (
	"context"
	"fmt"
	"strconv"

	kitvec "go.kenn.io/kit/vector"
)

const generationSplitKey = "generation_max_input_chars:"

// forGeneration keeps the Open recipe immutable while write operations use
// their own chunk boundaries. Index fields are immutable handles and options;
// the database and store provide synchronization for their shared state.
func (ix *Index) forGeneration(gen kitvec.Generation) *Index {
	operation := *ix
	if maxChars, err := strconv.Atoi(gen.Params["max_input_chars"]); err == nil && maxChars > 0 {
		operation.split = kitvec.SplitOptions{MaxRunes: maxChars, Overlap: ChunkOverlap(maxChars)}
	}
	return &operation
}

// recordGenerationSplit precedes activation. A fingerprint's recipe never
// changes, so prepared searches can safely retain it across later activations.
func (ix *Index) recordGenerationSplit(ctx context.Context, fingerprint string) error {
	value := strconv.Itoa(max(0, ix.split.MaxRunes))
	if _, err := ix.db.ExecContext(ctx, `INSERT INTO `+ix.spec.MetaTable+` (key, value) VALUES (?, ?) ON CONFLICT(key) DO NOTHING`, generationSplitKey+fingerprint, value); err != nil {
		return fmt.Errorf("record generation chunk recipe: %w", err)
	}
	stored, _, err := ix.metaGet(ctx, generationSplitKey+fingerprint)
	if err != nil {
		return err
	}
	if stored != value {
		return fmt.Errorf("conflicting chunk recipe for generation %q", fingerprint)
	}
	return nil
}

func (ix *Index) generationSplit(ctx context.Context, fingerprint string) (kitvec.SplitOptions, error) {
	raw, found, err := ix.metaGet(ctx, generationSplitKey+fingerprint)
	if err != nil {
		return kitvec.SplitOptions{}, err
	}
	if !found {
		return ix.split, nil
	} // Legacy generations use the immutable Open recipe.
	return splitFromRecipe(raw, fingerprint)
}

func splitFromRecipe(raw, fingerprint string) (kitvec.SplitOptions, error) {
	maxChars, err := strconv.Atoi(raw)
	if err != nil {
		return kitvec.SplitOptions{}, fmt.Errorf("invalid chunk recipe for generation %q", fingerprint)
	}
	maxChars = max(0, maxChars)
	return kitvec.SplitOptions{MaxRunes: maxChars, Overlap: ChunkOverlap(maxChars)}, nil
}
