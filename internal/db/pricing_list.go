package db

import (
	"context"
	"fmt"
)

// ListModelPricing returns every pricing row, including sentinel
// metadata rows (for example `_fallback_version`).
func (db *DB) ListModelPricing(
	ctx context.Context,
) ([]ModelPricing, error) {
	return db.listModelPricing(ctx)
}

func (db *DB) listModelPricing(
	ctx context.Context,
) ([]ModelPricing, error) {
	rows, err := db.getReader().QueryContext(
		ctx,
		`SELECT model_pattern, input_per_mtok,
			output_per_mtok, cache_creation_per_mtok,
			cache_read_per_mtok, updated_at
		 FROM model_pricing
		 ORDER BY model_pattern`,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"listing model pricing: %w", err,
		)
	}
	defer rows.Close()

	var out []ModelPricing
	for rows.Next() {
		var p ModelPricing
		if err := rows.Scan(
			&p.ModelPattern,
			&p.InputPerMTok,
			&p.OutputPerMTok,
			&p.CacheCreationPerMTok,
			&p.CacheReadPerMTok,
			&p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning model pricing: %w", err,
			)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"iterating model pricing: %w", err,
		)
	}
	if out == nil {
		out = []ModelPricing{}
	}
	return out, nil
}
