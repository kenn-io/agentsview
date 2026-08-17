package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ccoveille/go-safecast/v2"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/pricing"
)

func fallbackPricingRows() []db.ModelPricing {
	src := pricing.FallbackPricing()
	out := make([]db.ModelPricing, len(src))
	for i, p := range src {
		bands := make([]db.PricingBand, len(p.Bands))
		for j, band := range p.Bands {
			bands[j] = db.PricingBand{
				AboveInputTokens:     band.AboveInputTokens,
				InputPerMTok:         band.InputPerMTok,
				OutputPerMTok:        band.OutputPerMTok,
				CacheCreationPerMTok: band.CacheCreationPerMTok,
				CacheReadPerMTok:     band.CacheReadPerMTok,
			}
		}
		out[i] = db.ModelPricing{
			ModelPattern:         p.ModelPattern,
			InputPerMTok:         p.InputPerMTok,
			OutputPerMTok:        p.OutputPerMTok,
			CacheCreationPerMTok: p.CacheCreationPerMTok,
			CacheReadPerMTok:     p.CacheReadPerMTok,
			Bands:                bands,
		}
	}
	return out
}

const pgModelPricingSelect = `SELECT
	p.model_pattern,
	p.input_microdollars_per_mtok,
	p.output_microdollars_per_mtok,
	p.cache_creation_microdollars_per_mtok,
	p.cache_read_microdollars_per_mtok,
	p.updated_at,
	b.above_input_tokens,
	b.input_microdollars_per_mtok,
	b.output_microdollars_per_mtok,
	b.cache_creation_microdollars_per_mtok,
	b.cache_read_microdollars_per_mtok,
	b.updated_at
FROM model_pricing p
LEFT JOIN model_pricing_bands b ON b.model_pattern = p.model_pattern
ORDER BY p.model_pattern, b.above_input_tokens`

func listPGModelPricing(
	ctx context.Context, pg *sql.DB,
) ([]db.ModelPricing, error) {
	rows, err := pg.QueryContext(ctx,
		pgModelPricingSelect,
	)
	if err != nil {
		return nil, fmt.Errorf("listing pg pricing: %w", err)
	}
	defer rows.Close()

	return scanPGModelPricingRows(rows)
}

func scanPGModelPricingRows(rows *sql.Rows) ([]db.ModelPricing, error) {
	out := make([]db.ModelPricing, 0)
	byPattern := make(map[string]int)
	for rows.Next() {
		var p db.ModelPricing
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
			return nil, fmt.Errorf("scanning pg pricing: %w", err)
		}
		i, exists := byPattern[p.ModelPattern]
		if !exists {
			i = len(out)
			byPattern[p.ModelPattern] = i
			out = append(out, p)
		}
		if threshold.Valid {
			aboveInputTokens, err := safecast.Convert[int](threshold.Int64)
			if err != nil {
				return nil, fmt.Errorf(
					"converting pg pricing threshold for %q: %w",
					p.ModelPattern, err,
				)
			}
			out[i].Bands = append(out[i].Bands, db.PricingBand{
				AboveInputTokens: aboveInputTokens,
				InputPerMTok: money.Money{
					Microdollars: input.Int64,
				},
				OutputPerMTok: money.Money{
					Microdollars: output.Int64,
				},
				CacheCreationPerMTok: money.Money{
					Microdollars: cacheCreation.Int64,
				},
				CacheReadPerMTok: money.Money{
					Microdollars: cacheRead.Int64,
				},
				UpdatedAt: bandUpdatedAt.String,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating pg pricing: %w", err)
	}
	return out, nil
}

func (s *Sync) syncModelPricing(ctx context.Context) error {
	prices, err := s.local.ListModelPricing(ctx)
	if err != nil {
		return fmt.Errorf("listing local model pricing: %w", err)
	}
	if len(prices) == 0 {
		prices = fallbackPricingRows()
	}
	existing, err := listPGModelPricing(ctx, s.pg)
	if err != nil {
		return fmt.Errorf("listing pg model pricing: %w", err)
	}
	_, changedPrices := db.FilterChangedModelPricing(
		existing, prices,
	)
	if len(changedPrices) == 0 {
		return nil
	}
	defaultUpdatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	for i := range changedPrices {
		if changedPrices[i].UpdatedAt == "" {
			changedPrices[i].UpdatedAt = defaultUpdatedAt
		}
	}
	rows, bands, err := db.CanonicalModelPricingRows(changedPrices)
	if err != nil {
		return fmt.Errorf("converting pg model pricing: %w", err)
	}
	if err := db.UpsertModelPricingRows(ctx, s.bunDB(), rows, bands); err != nil {
		return fmt.Errorf("syncing model pricing to pg: %w", err)
	}
	return nil
}
