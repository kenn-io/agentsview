package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ccoveille/go-safecast/v2"
	"github.com/uptrace/bun"

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
				AboveInputTokens:       band.AboveInputTokens,
				InputPerMTok:           band.InputPerMTok,
				OutputPerMTok:          band.OutputPerMTok,
				CacheCreationPerMTok:   band.CacheCreationPerMTok,
				CacheCreation1hPerMTok: band.CacheCreation1hPerMTok,
				CacheReadPerMTok:       band.CacheReadPerMTok,
			}
		}
		out[i] = db.ModelPricing{
			ModelPattern:           p.ModelPattern,
			InputPerMTok:           p.InputPerMTok,
			OutputPerMTok:          p.OutputPerMTok,
			CacheCreationPerMTok:   p.CacheCreationPerMTok,
			CacheCreation1hPerMTok: p.CacheCreation1hPerMTok,
			CacheReadPerMTok:       p.CacheReadPerMTok,
			Bands:                  bands,
		}
	}
	return out
}

const pgModelPricingSelect = `SELECT
	p.model_pattern,
	p.input_microdollars_per_mtok,
	p.output_microdollars_per_mtok,
	p.cache_creation_microdollars_per_mtok,
	p.cache_creation_1h_microdollars_per_mtok,
	p.cache_read_microdollars_per_mtok,
	p.updated_at,
	b.above_input_tokens,
	b.input_microdollars_per_mtok,
	b.output_microdollars_per_mtok,
	b.cache_creation_microdollars_per_mtok,
	b.cache_creation_1h_microdollars_per_mtok,
	b.cache_read_microdollars_per_mtok,
	b.updated_at
FROM model_pricing p
LEFT JOIN model_pricing_bands b ON b.model_pattern = p.model_pattern
ORDER BY p.model_pattern, b.above_input_tokens`

func listPGModelPricing(
	ctx context.Context, pg interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	},
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
		var threshold, input, output, cacheCreation, cacheCreation1h,
			cacheRead sql.NullInt64
		var bandUpdatedAt sql.NullString
		if err := rows.Scan(
			&p.ModelPattern,
			&p.InputPerMTok,
			&p.OutputPerMTok,
			&p.CacheCreationPerMTok,
			&p.CacheCreation1hPerMTok,
			&p.CacheReadPerMTok,
			&p.UpdatedAt,
			&threshold,
			&input,
			&output,
			&cacheCreation,
			&cacheCreation1h,
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
				CacheCreation1hPerMTok: money.Money{
					Microdollars: cacheCreation1h.Int64,
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
	localOwnership, err := s.local.GetPricingMeta(pricing.OpenRouterModelsMetaKey)
	if err != nil {
		return fmt.Errorf("reading local pricing ownership: %w", err)
	}
	if err := s.bunDB().RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := lockPGModelPricing(ctx, tx); err != nil {
			return err
		}
		existing, err := listPGModelPricing(ctx, tx)
		if err != nil {
			return fmt.Errorf("listing pg model pricing: %w", err)
		}
		targetOwnership, err := db.ReadSyncMetadata(
			ctx, tx, pricing.OpenRouterModelsMetaKey,
		)
		if err != nil {
			return err
		}
		changedPrices, removePatterns, ownership, err := db.PlanModelPricingSync(
			existing, prices, targetOwnership, localOwnership,
		)
		if err != nil {
			return fmt.Errorf("planning pg model pricing sync: %w", err)
		}
		if len(changedPrices) == 0 && len(removePatterns) == 0 && ownership.Key == "" {
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
		if err := db.ApplyModelPricingRows(
			ctx, tx, rows, bands, removePatterns,
		); err != nil {
			return fmt.Errorf("syncing model pricing to pg: %w", err)
		}
		return db.WriteSyncMetadata(ctx, tx, ownership)
	}); err != nil {
		return err
	}
	return nil
}

const pricingSyncLockKey = "model_pricing_sync_lock"

func lockPGModelPricing(ctx context.Context, tx bun.IDB) error {
	if _, err := tx.NewRaw(`
		INSERT INTO sync_metadata (key, value) VALUES (?, '')
		ON CONFLICT (key) DO NOTHING`, pricingSyncLockKey).Exec(ctx); err != nil {
		return fmt.Errorf("creating pg model pricing lock row: %w", err)
	}
	if _, err := tx.NewRaw(`
		SELECT value FROM sync_metadata WHERE key = ? FOR UPDATE`,
		pricingSyncLockKey,
	).Exec(ctx); err != nil {
		return fmt.Errorf("locking pg model pricing: %w", err)
	}
	return nil
}
