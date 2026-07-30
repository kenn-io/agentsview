package db

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/agentsview/internal/money"
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
		`SELECT p.model_pattern, p.input_microdollars_per_mtok,
			p.output_microdollars_per_mtok,
			p.cache_creation_microdollars_per_mtok,
			p.cache_read_microdollars_per_mtok, p.updated_at,
			b.above_input_tokens, b.input_microdollars_per_mtok,
			b.output_microdollars_per_mtok,
			b.cache_creation_microdollars_per_mtok,
			b.cache_read_microdollars_per_mtok, b.updated_at
		 FROM model_pricing p
		 LEFT JOIN model_pricing_bands b ON b.model_pattern = p.model_pattern
		 ORDER BY p.model_pattern, b.above_input_tokens`,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"listing model pricing: %w", err,
		)
	}
	defer rows.Close()

	var out []ModelPricing
	byPattern := make(map[string]int)
	for rows.Next() {
		var p ModelPricing
		var threshold, input, output, cacheCreation, cacheRead sql.NullInt64
		var bandUpdatedAt sql.NullString
		if err := rows.Scan(
			&p.ModelPattern,
			&p.InputPerMTok,
			&p.OutputPerMTok,
			&p.CacheCreationPerMTok,
			&p.CacheReadPerMTok,
			&p.UpdatedAt,
			&threshold,
			&input,
			&output,
			&cacheCreation,
			&cacheRead,
			&bandUpdatedAt,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning model pricing: %w", err,
			)
		}
		i, exists := byPattern[p.ModelPattern]
		if !exists {
			i = len(out)
			byPattern[p.ModelPattern] = i
			out = append(out, p)
		}
		if threshold.Valid {
			out[i].Bands = append(out[i].Bands, PricingBand{
				AboveInputTokens:     int(threshold.Int64),
				InputPerMTok:         money.Money{Microdollars: input.Int64},
				OutputPerMTok:        money.Money{Microdollars: output.Int64},
				CacheCreationPerMTok: money.Money{Microdollars: cacheCreation.Int64},
				CacheReadPerMTok:     money.Money{Microdollars: cacheRead.Int64},
				UpdatedAt:            bandUpdatedAt.String,
			})
		}
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
