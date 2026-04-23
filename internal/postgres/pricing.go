package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/pricing"
)

type modelRates struct {
	input         float64
	output        float64
	cacheCreation float64
	cacheRead     float64
}

func fallbackPricingRows() []db.ModelPricing {
	src := pricing.FallbackPricing()
	out := make([]db.ModelPricing, len(src))
	for i, p := range src {
		out[i] = db.ModelPricing{
			ModelPattern:         p.ModelPattern,
			InputPerMTok:         p.InputPerMTok,
			OutputPerMTok:        p.OutputPerMTok,
			CacheCreationPerMTok: p.CacheCreationPerMTok,
			CacheReadPerMTok:     p.CacheReadPerMTok,
		}
	}
	return out
}

func pricingRowsToMap(prices []db.ModelPricing) map[string]modelRates {
	out := make(map[string]modelRates, len(prices))
	for _, p := range prices {
		if strings.HasPrefix(p.ModelPattern, "_") {
			continue
		}
		out[p.ModelPattern] = modelRates{
			input:         p.InputPerMTok,
			output:        p.OutputPerMTok,
			cacheCreation: p.CacheCreationPerMTok,
			cacheRead:     p.CacheReadPerMTok,
		}
	}
	return out
}

func fallbackPricingMap() map[string]modelRates {
	return pricingRowsToMap(fallbackPricingRows())
}

func (s *Store) loadPricingMap(
	ctx context.Context,
) (map[string]modelRates, error) {
	out := fallbackPricingMap()
	if err := s.mergeDBPricing(ctx, out); err != nil {
		return nil, err
	}
	s.applyCustomPricing(out)
	return out, nil
}

// mergeDBPricing layers rows from the PG model_pricing table onto
// out. A missing table is treated as "no DB overrides" so that
// custom_model_pricing still applies on fresh PG installs where
// `agentsview pg push` has not run yet.
func (s *Store) mergeDBPricing(
	ctx context.Context, out map[string]modelRates,
) error {
	rows, err := s.pg.QueryContext(
		ctx,
		`SELECT model_pattern, input_per_mtok,
			output_per_mtok, cache_creation_per_mtok,
			cache_read_per_mtok, updated_at
		 FROM model_pricing`,
	)
	if err != nil {
		if isUndefinedTable(err) {
			return nil
		}
		return fmt.Errorf("querying pg pricing: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var p db.ModelPricing
		if err := rows.Scan(
			&p.ModelPattern,
			&p.InputPerMTok,
			&p.OutputPerMTok,
			&p.CacheCreationPerMTok,
			&p.CacheReadPerMTok,
			&p.UpdatedAt,
		); err != nil {
			return fmt.Errorf("scanning pg pricing: %w", err)
		}
		if strings.HasPrefix(p.ModelPattern, "_") {
			continue
		}
		out[p.ModelPattern] = modelRates{
			input:         p.InputPerMTok,
			output:        p.OutputPerMTok,
			cacheCreation: p.CacheCreationPerMTok,
			cacheRead:     p.CacheReadPerMTok,
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating pg pricing: %w", err)
	}
	return nil
}

// applyCustomPricing overlays user-configured rates onto out, letting
// custom entries win over both DB and fallback pricing for the same
// model. Kept separate from loadPricingMap so unit tests can exercise
// the override step without a live PostgreSQL connection.
func (s *Store) applyCustomPricing(out map[string]modelRates) {
	for model, cp := range s.customPricing {
		out[model] = modelRates{
			input:         cp.Input,
			output:        cp.Output,
			cacheCreation: cp.CacheCreation,
			cacheRead:     cp.CacheRead,
		}
	}
}

func upsertModelPricing(
	ctx context.Context, pg *sql.DB, prices []db.ModelPricing,
) error {
	if len(prices) == 0 {
		return nil
	}

	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning pg pricing upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO model_pricing
			(model_pattern, input_per_mtok, output_per_mtok,
			 cache_creation_per_mtok, cache_read_per_mtok,
			 updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (model_pattern) DO UPDATE SET
			input_per_mtok = EXCLUDED.input_per_mtok,
			output_per_mtok = EXCLUDED.output_per_mtok,
			cache_creation_per_mtok = EXCLUDED.cache_creation_per_mtok,
			cache_read_per_mtok = EXCLUDED.cache_read_per_mtok,
			updated_at = EXCLUDED.updated_at`)
	if err != nil {
		return fmt.Errorf("preparing pg pricing upsert: %w", err)
	}
	defer stmt.Close()

	for _, p := range prices {
		updatedAt := p.UpdatedAt
		if updatedAt == "" {
			updatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		}
		if _, err := stmt.ExecContext(
			ctx,
			sanitizePG(p.ModelPattern),
			p.InputPerMTok,
			p.OutputPerMTok,
			p.CacheCreationPerMTok,
			p.CacheReadPerMTok,
			sanitizePG(updatedAt),
		); err != nil {
			return fmt.Errorf(
				"upserting pg pricing %q: %w",
				p.ModelPattern, err,
			)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing pg pricing upsert: %w", err)
	}
	return nil
}

func (s *Sync) syncModelPricing(ctx context.Context) error {
	prices, err := s.local.ListModelPricing(ctx)
	if err != nil {
		return fmt.Errorf("listing local model pricing: %w", err)
	}
	if len(prices) == 0 {
		prices = fallbackPricingRows()
	}
	if err := upsertModelPricing(ctx, s.pg, prices); err != nil {
		return fmt.Errorf("syncing model pricing to pg: %w", err)
	}
	return nil
}
