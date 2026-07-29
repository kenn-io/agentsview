package db

import (
	"context"
	"database/sql"
	"fmt"
)

type pricingQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

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
	return listModelPricingFrom(ctx, db.getReader())
}

func listModelPricingFrom(
	ctx context.Context, q pricingQuerier,
) ([]ModelPricing, error) {
	rows, err := q.QueryContext(
		ctx,
		`SELECT model_pattern, input_microdollars_per_mtok,
			output_microdollars_per_mtok, cache_creation_microdollars_per_mtok,
			cache_read_microdollars_per_mtok, updated_at
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
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("closing model pricing rows: %w", err)
	}

	bands, err := loadAllPricingBands(ctx, q)
	if err != nil {
		return nil, err
	}
	byPattern := make(map[string]int, len(out))
	for i := range out {
		byPattern[out[i].ModelPattern] = i
	}
	for _, band := range bands {
		if i, ok := byPattern[band.modelPattern]; ok {
			out[i].Bands = append(out[i].Bands, band.PricingBand)
		}
	}
	if out == nil {
		out = []ModelPricing{}
	}
	return out, nil
}

type storedPricingBand struct {
	modelPattern string
	PricingBand
}

func loadAllPricingBands(
	ctx context.Context, q pricingQuerier,
) ([]storedPricingBand, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT model_pattern, above_input_tokens,
			input_microdollars_per_mtok, output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok,
			cache_read_microdollars_per_mtok, updated_at
		FROM model_pricing_bands
		ORDER BY model_pattern, above_input_tokens`)
	if err != nil {
		return nil, fmt.Errorf("listing model pricing bands: %w", err)
	}
	defer rows.Close()

	var out []storedPricingBand
	for rows.Next() {
		var band storedPricingBand
		if err := rows.Scan(
			&band.modelPattern,
			&band.AboveInputTokens,
			&band.InputPerMTok,
			&band.OutputPerMTok,
			&band.CacheCreationPerMTok,
			&band.CacheReadPerMTok,
			&band.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning model pricing band: %w", err)
		}
		out = append(out, band)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating model pricing bands: %w", err)
	}
	return out, nil
}

func loadPricingBandsForModel(
	ctx context.Context, q pricingQuerier, model string,
) ([]PricingBand, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT above_input_tokens,
			input_microdollars_per_mtok, output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok,
			cache_read_microdollars_per_mtok, updated_at
		FROM model_pricing_bands
		WHERE model_pattern = ?
		ORDER BY above_input_tokens`, model)
	if err != nil {
		return nil, fmt.Errorf("listing pricing bands for %q: %w", model, err)
	}
	defer rows.Close()

	var out []PricingBand
	for rows.Next() {
		var band PricingBand
		if err := rows.Scan(
			&band.AboveInputTokens,
			&band.InputPerMTok,
			&band.OutputPerMTok,
			&band.CacheCreationPerMTok,
			&band.CacheReadPerMTok,
			&band.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning pricing band for %q: %w", model, err)
		}
		out = append(out, band)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating pricing bands for %q: %w", model, err)
	}
	return out, nil
}
