package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/pricing"
)

type pricingLoad struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	prices  []export.EffectivePricingRow
	err     error
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

func pricingRowsToMap(prices []db.ModelPricing) map[string]export.ModelRates {
	fallback := pgFallbackRateMap()
	out := make(map[string]export.ModelRates, len(prices))
	for _, p := range prices {
		if strings.HasPrefix(p.ModelPattern, "_") {
			continue
		}
		rates := pgModelPricingRates(p)
		rates.Source = pgModelPricingSource(p, fallback)
		out[p.ModelPattern] = rates
	}
	return out
}

func pgFallbackRateMap() map[string]export.ModelRates {
	src := pricing.FallbackPricing()
	out := make(map[string]export.ModelRates, len(src))
	for _, p := range src {
		out[p.ModelPattern] = export.ModelRates{
			InputPerMTok:      p.InputPerMTok,
			OutputPerMTok:     p.OutputPerMTok,
			CacheWritePerMTok: p.CacheCreationPerMTok,
			CacheReadPerMTok:  p.CacheReadPerMTok,
			Source:            export.PricingRowSourceEmbedded,
		}
	}
	return out
}

func pgModelPricingRates(p db.ModelPricing) export.ModelRates {
	var updatedAt *time.Time
	if p.UpdatedAt != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, p.UpdatedAt); err == nil {
			t := parsed.UTC()
			updatedAt = &t
		}
	}
	return export.ModelRates{
		InputPerMTok:      p.InputPerMTok,
		OutputPerMTok:     p.OutputPerMTok,
		CacheWritePerMTok: p.CacheCreationPerMTok,
		CacheReadPerMTok:  p.CacheReadPerMTok,
		UpdatedAt:         updatedAt,
	}
}

func pgModelPricingSource(
	p db.ModelPricing, fallback map[string]export.ModelRates,
) export.PricingRowSource {
	if rates, ok := fallback[p.ModelPattern]; ok &&
		rates.InputPerMTok == p.InputPerMTok &&
		rates.OutputPerMTok == p.OutputPerMTok &&
		rates.CacheWritePerMTok == p.CacheCreationPerMTok &&
		rates.CacheReadPerMTok == p.CacheReadPerMTok {
		return export.PricingRowSourceEmbedded
	}
	return export.PricingRowSourceFetched
}

func fallbackPricingMap() map[string]export.ModelRates {
	return pricingRowsToMap(fallbackPricingRows())
}

func pricingMapRows(
	in map[string]export.ModelRates,
) []export.EffectivePricingRow {
	out := make([]export.EffectivePricingRow, 0, len(in))
	for pattern, rates := range in {
		out = append(out, export.EffectivePricingRow{
			ModelPattern: pattern,
			Rates:        rates,
		})
	}
	return out
}

func clonePricingRows(
	in []export.EffectivePricingRow,
) []export.EffectivePricingRow {
	out := make([]export.EffectivePricingRow, len(in))
	copy(out, in)
	return out
}

func (s *Store) loadPricingMap(
	ctx context.Context,
) ([]export.EffectivePricingRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	load := s.startPricingLoad()
	defer s.leavePricingLoad(load)

	select {
	case <-load.done:
		if load.err != nil {
			return nil, load.err
		}
		return clonePricingRows(load.prices), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Store) startPricingLoad() *pricingLoad {
	s.pricingLoadMu.Lock()
	defer s.pricingLoadMu.Unlock()
	if s.pricingLoad != nil {
		s.pricingLoad.waiters++
		return s.pricingLoad
	}

	ctx, cancel := context.WithCancel(context.Background())
	load := &pricingLoad{
		done:    make(chan struct{}),
		cancel:  cancel,
		waiters: 1,
	}
	s.pricingLoad = load
	go s.runPricingLoad(ctx, load)
	return load
}

func (s *Store) runPricingLoad(ctx context.Context, load *pricingLoad) {
	out := map[string]export.ModelRates{}
	dbRows, err := s.mergeDBPricing(ctx, out)
	if err == nil && dbRows == 0 {
		out = fallbackPricingMap()
	}
	load.cancel()

	var prices []export.EffectivePricingRow
	if err == nil {
		s.pricingMu.Lock()
		s.applyCustomPricing(out)
		s.pricingMu.Unlock()
		prices = pricingMapRows(out)
	}

	s.pricingLoadMu.Lock()
	defer s.pricingLoadMu.Unlock()
	load.err = err
	load.prices = prices
	if s.pricingLoad == load {
		s.pricingLoad = nil
	}
	close(load.done)
}

func (s *Store) leavePricingLoad(load *pricingLoad) {
	var cancel context.CancelFunc
	s.pricingLoadMu.Lock()
	load.waiters--
	if load.waiters == 0 && s.pricingLoad == load {
		s.pricingLoad = nil
		cancel = load.cancel
	}
	s.pricingLoadMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Store) forgetPricingLoad() {
	s.pricingLoadMu.Lock()
	defer s.pricingLoadMu.Unlock()
	s.pricingLoad = nil
}

// mergeDBPricing layers rows from the PG model_pricing table onto
// out. A missing table is treated as "no DB overrides" so that
// custom_model_pricing still applies on fresh PG installs where
// `agentsview pg push` has not run yet.
func (s *Store) mergeDBPricing(
	ctx context.Context, out map[string]export.ModelRates,
) (int, error) {
	rows, err := s.pg.QueryContext(
		ctx,
		`SELECT model_pattern, input_microdollars_per_mtok,
			output_microdollars_per_mtok, cache_creation_microdollars_per_mtok,
			cache_read_microdollars_per_mtok, updated_at
		 FROM model_pricing`,
	)
	if err != nil {
		if isUndefinedTable(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("querying pg pricing: %w", err)
	}
	defer rows.Close()

	count := 0
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
			return 0, fmt.Errorf("scanning pg pricing: %w", err)
		}
		if strings.HasPrefix(p.ModelPattern, "_") {
			continue
		}
		rates := pgModelPricingRates(p)
		rates.Source = pgModelPricingSource(p, pgFallbackRateMap())
		out[p.ModelPattern] = rates
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterating pg pricing: %w", err)
	}
	return count, nil
}

// applyCustomPricing overlays user-configured rates onto out, letting
// custom entries win over both DB and fallback pricing for the same
// model. Kept separate from loadPricingMap so unit tests can exercise
// the override step without a live PostgreSQL connection.
func (s *Store) applyCustomPricing(out map[string]export.ModelRates) {
	for model, cp := range s.customPricing {
		rates := export.ModelRates{
			InputPerMTok: money.Money{
				Microdollars: cp.InputMicrodollarsPerMTok,
			},
			OutputPerMTok: money.Money{
				Microdollars: cp.OutputMicrodollarsPerMTok,
			},
			CacheWritePerMTok: money.Money{
				Microdollars: cp.CacheCreationMicrodollarsPerMTok,
			},
			CacheReadPerMTok: money.Money{
				Microdollars: cp.CacheReadMicrodollarsPerMTok,
			},
		}
		rates.Source = pgCustomPricingSource()
		out[model] = rates
	}
}

func pgCustomPricingSource() export.PricingRowSource {
	return export.PricingRowSourceCustom
}

const pricingUpsertBatch = 100

func pgPricingUpsertStatement(
	prices []db.ModelPricing, defaultUpdatedAt string,
) (string, []any) {
	var b strings.Builder
	b.WriteString(`INSERT INTO model_pricing
		(model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok,
		 cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok,
		 updated_at)
	VALUES `)
	args := make([]any, 0, len(prices)*6)
	for i, p := range prices {
		if i > 0 {
			b.WriteString(", ")
		}
		base := i*6 + 1
		fmt.Fprintf(
			&b,
			"($%d, $%d, $%d, $%d, $%d, $%d)",
			base, base+1, base+2, base+3, base+4, base+5,
		)
		updatedAt := p.UpdatedAt
		if updatedAt == "" {
			updatedAt = defaultUpdatedAt
		}
		args = append(args,
			sanitizePG(p.ModelPattern),
			p.InputPerMTok,
			p.OutputPerMTok,
			p.CacheCreationPerMTok,
			p.CacheReadPerMTok,
			sanitizePG(updatedAt),
		)
	}
	b.WriteString(`
	ON CONFLICT (model_pattern) DO UPDATE SET
		input_microdollars_per_mtok = EXCLUDED.input_microdollars_per_mtok,
		output_microdollars_per_mtok = EXCLUDED.output_microdollars_per_mtok,
		cache_creation_microdollars_per_mtok = EXCLUDED.cache_creation_microdollars_per_mtok,
		cache_read_microdollars_per_mtok = EXCLUDED.cache_read_microdollars_per_mtok,
		updated_at = EXCLUDED.updated_at
	WHERE model_pricing.input_microdollars_per_mtok IS DISTINCT FROM
			EXCLUDED.input_microdollars_per_mtok
		OR model_pricing.output_microdollars_per_mtok IS DISTINCT FROM
			EXCLUDED.output_microdollars_per_mtok
		OR model_pricing.cache_creation_microdollars_per_mtok IS DISTINCT FROM
			EXCLUDED.cache_creation_microdollars_per_mtok
		OR model_pricing.cache_read_microdollars_per_mtok IS DISTINCT FROM
			EXCLUDED.cache_read_microdollars_per_mtok`)
	return b.String(), args
}

func listPGModelPricing(
	ctx context.Context, pg *sql.DB,
) ([]db.ModelPricing, error) {
	rows, err := pg.QueryContext(ctx,
		`SELECT model_pattern, input_microdollars_per_mtok,
			output_microdollars_per_mtok, cache_creation_microdollars_per_mtok,
			cache_read_microdollars_per_mtok, updated_at
		 FROM model_pricing`,
	)
	if err != nil {
		return nil, fmt.Errorf("listing pg pricing: %w", err)
	}
	defer rows.Close()

	var out []db.ModelPricing
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
			return nil, fmt.Errorf("scanning pg pricing: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating pg pricing: %w", err)
	}
	return out, nil
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

	defaultUpdatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	for i := 0; i < len(prices); i += pricingUpsertBatch {
		end := min(i+pricingUpsertBatch, len(prices))
		query, args := pgPricingUpsertStatement(
			prices[i:end], defaultUpdatedAt,
		)
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf(
				"upserting pg pricing batch starting at %d: %w",
				i, err,
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
	if err := upsertModelPricing(ctx, s.pg, changedPrices); err != nil {
		return fmt.Errorf("syncing model pricing to pg: %w", err)
	}
	return nil
}
