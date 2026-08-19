package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/ccoveille/go-safecast/v2"

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
			Bands:             pgCatalogPricingBands(p.Bands),
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
		Bands:             pgStoredPricingBands(p.Bands),
	}
}

func pgCatalogPricingBands(bands []pricing.PricingBand) []export.PricingBand {
	out := make([]export.PricingBand, len(bands))
	for i, band := range bands {
		out[i] = export.PricingBand{
			AboveInputTokens:  band.AboveInputTokens,
			InputPerMTok:      band.InputPerMTok,
			OutputPerMTok:     band.OutputPerMTok,
			CacheWritePerMTok: band.CacheCreationPerMTok,
			CacheReadPerMTok:  band.CacheReadPerMTok,
		}
	}
	return out
}

func pgStoredPricingBands(bands []db.PricingBand) []export.PricingBand {
	out := make([]export.PricingBand, len(bands))
	for i, band := range bands {
		var updatedAt *time.Time
		if parsed, err := time.Parse(time.RFC3339Nano, band.UpdatedAt); err == nil {
			t := parsed.UTC()
			updatedAt = &t
		}
		out[i] = export.PricingBand{
			AboveInputTokens:  band.AboveInputTokens,
			InputPerMTok:      band.InputPerMTok,
			OutputPerMTok:     band.OutputPerMTok,
			CacheWritePerMTok: band.CacheCreationPerMTok,
			CacheReadPerMTok:  band.CacheReadPerMTok,
			UpdatedAt:         updatedAt,
		}
	}
	return out
}

func pgModelPricingSource(
	p db.ModelPricing, fallback map[string]export.ModelRates,
) export.PricingRowSource {
	if rates, ok := fallback[p.ModelPattern]; ok &&
		rates.InputPerMTok == p.InputPerMTok &&
		rates.OutputPerMTok == p.OutputPerMTok &&
		rates.CacheWritePerMTok == p.CacheCreationPerMTok &&
		rates.CacheReadPerMTok == p.CacheReadPerMTok &&
		pgPricingBandsEqual(rates.Bands, pgStoredPricingBands(p.Bands)) {
		return export.PricingRowSourceEmbedded
	}
	return export.PricingRowSourceFetched
}

func pgPricingBandsEqual(a, b []export.PricingBand) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].AboveInputTokens != b[i].AboveInputTokens ||
			a[i].InputPerMTok != b[i].InputPerMTok ||
			a[i].OutputPerMTok != b[i].OutputPerMTok ||
			a[i].CacheWritePerMTok != b[i].CacheWritePerMTok ||
			a[i].CacheReadPerMTok != b[i].CacheReadPerMTok {
			return false
		}
	}
	return true
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
	for i, row := range in {
		row.Rates.Bands = append([]export.PricingBand(nil), row.Rates.Bands...)
		out[i] = row
	}
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
		pgModelPricingSelect,
	)
	if err != nil {
		if isUndefinedTable(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("querying pg pricing: %w", err)
	}
	defer rows.Close()

	prices, err := scanPGModelPricingRows(rows)
	if err != nil {
		return 0, err
	}
	fallback := pgFallbackRateMap()
	usableRows := 0
	for _, p := range prices {
		if strings.HasPrefix(p.ModelPattern, "_") {
			continue
		}
		rates := pgModelPricingRates(p)
		rates.Source = pgModelPricingSource(p, fallback)
		out[p.ModelPattern] = rates
		usableRows++
	}
	return usableRows, nil
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
		updated_at = CASE
			WHEN model_pricing.updated_at = '' THEN EXCLUDED.updated_at
			WHEN model_pricing.updated_at::timestamptz >=
				EXCLUDED.updated_at::timestamptz
			THEN to_char(
				(model_pricing.updated_at::timestamptz + INTERVAL '1 microsecond')
					AT TIME ZONE 'UTC',
				'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')
			ELSE EXCLUDED.updated_at
		END
	WHERE model_pricing.input_microdollars_per_mtok IS DISTINCT FROM
			EXCLUDED.input_microdollars_per_mtok
		OR model_pricing.output_microdollars_per_mtok IS DISTINCT FROM
			EXCLUDED.output_microdollars_per_mtok
		OR model_pricing.cache_creation_microdollars_per_mtok IS DISTINCT FROM
			EXCLUDED.cache_creation_microdollars_per_mtok
		OR model_pricing.cache_read_microdollars_per_mtok IS DISTINCT FROM
			EXCLUDED.cache_read_microdollars_per_mtok
	RETURNING model_pattern`)
	return b.String(), args
}

func listPGModelPricing(
	ctx context.Context, pg pgSessionQueryer,
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

func pgPricingTouchStatement(
	prices []db.ModelPricing, defaultUpdatedAt string,
) (string, []any) {
	var b strings.Builder
	b.WriteString(`UPDATE model_pricing AS p
		SET updated_at = CASE
			WHEN p.updated_at = '' THEN v.updated_at
			WHEN p.updated_at::timestamptz >= v.updated_at::timestamptz
			THEN to_char(
				(p.updated_at::timestamptz + INTERVAL '1 microsecond')
					AT TIME ZONE 'UTC',
				'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')
			ELSE v.updated_at
		END
		FROM (VALUES `)
	args := make([]any, 0, len(prices)*2)
	for i, price := range prices {
		if i > 0 {
			b.WriteString(", ")
		}
		base := i*2 + 1
		fmt.Fprintf(&b, "($%d::text, $%d::text)", base, base+1)
		updatedAt := price.UpdatedAt
		if updatedAt == "" {
			updatedAt = defaultUpdatedAt
		}
		args = append(args, sanitizePG(price.ModelPattern), updatedAt)
	}
	b.WriteString(`) AS v(model_pattern, updated_at)
		WHERE p.model_pattern = v.model_pattern`)
	return b.String(), args
}

func pgPricingBandDeleteStatement(
	prices []db.ModelPricing,
) (string, []any) {
	var b strings.Builder
	b.WriteString(`DELETE FROM model_pricing_bands WHERE model_pattern IN (`)
	args := make([]any, len(prices))
	for i, price := range prices {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "$%d", i+1)
		args[i] = sanitizePG(price.ModelPattern)
	}
	b.WriteByte(')')
	return b.String(), args
}

type pgModelPricingBand struct {
	model string
	band  db.PricingBand
}

func pgPricingBandInsertStatement(
	bands []pgModelPricingBand,
	defaultUpdatedAt string,
) (string, []any) {
	var b strings.Builder
	b.WriteString(`INSERT INTO model_pricing_bands
		(model_pattern, above_input_tokens,
		 input_microdollars_per_mtok, output_microdollars_per_mtok,
		 cache_creation_microdollars_per_mtok,
		 cache_read_microdollars_per_mtok, updated_at)
	VALUES `)
	args := make([]any, 0, len(bands)*7)
	for i, item := range bands {
		if i > 0 {
			b.WriteString(", ")
		}
		base := i*7 + 1
		fmt.Fprintf(&b, "($%d, $%d, $%d, $%d, $%d, $%d, $%d)",
			base, base+1, base+2, base+3, base+4, base+5, base+6)
		updatedAt := item.band.UpdatedAt
		if updatedAt == "" {
			updatedAt = defaultUpdatedAt
		}
		args = append(args,
			sanitizePG(item.model),
			item.band.AboveInputTokens,
			item.band.InputPerMTok,
			item.band.OutputPerMTok,
			item.band.CacheCreationPerMTok,
			item.band.CacheReadPerMTok,
			sanitizePG(updatedAt),
		)
	}
	return b.String(), args
}

// pgPricingMetaUpsertStatement writes sentinel metadata rows, whose
// updated_at holds an opaque value rather than a timestamp.
func pgPricingMetaUpsertStatement(
	metaRows []db.ModelPricing,
) (string, []any) {
	var b strings.Builder
	b.WriteString(`INSERT INTO model_pricing
		(model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok,
		 cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok,
		 updated_at)
	VALUES `)
	args := make([]any, 0, len(metaRows)*2)
	for i, row := range metaRows {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "($%d, 0, 0, 0, 0, $%d)", i*2+1, i*2+2)
		args = append(args,
			sanitizePG(row.ModelPattern), sanitizePG(row.UpdatedAt),
		)
	}
	b.WriteString(`
	ON CONFLICT (model_pattern) DO UPDATE SET
		updated_at = EXCLUDED.updated_at`)
	return b.String(), args
}

func pgPricingDeleteStatement(
	table string, patterns []string,
) (string, []any) {
	placeholders := make([]string, len(patterns))
	args := make([]any, len(patterns))
	for i, pattern := range patterns {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = sanitizePG(pattern)
	}
	return `DELETE FROM ` + table + ` WHERE model_pattern IN (` +
		strings.Join(placeholders, ", ") + `)`, args
}

// reconcileModelPricing deletes removePatterns and upserts prices within
// tx. Sentinel metadata rows are written by value; model rows go through
// the change-detecting upsert and band replacement.
func reconcileModelPricing(
	ctx context.Context, tx *sql.Tx,
	prices []db.ModelPricing, removePatterns []string,
) error {
	if err := deletePGModelPricing(ctx, tx, removePatterns); err != nil {
		return err
	}
	metaRows := make([]db.ModelPricing, 0)
	modelRows := make([]db.ModelPricing, 0, len(prices))
	for _, price := range prices {
		if strings.HasPrefix(price.ModelPattern, "_") {
			metaRows = append(metaRows, price)
		} else {
			modelRows = append(modelRows, price)
		}
	}
	for i := 0; i < len(metaRows); i += pricingUpsertBatch {
		end := min(i+pricingUpsertBatch, len(metaRows))
		query, args := pgPricingMetaUpsertStatement(metaRows[i:end])
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf(
				"upserting pg pricing meta at batch %d: %w", i, err)
		}
	}
	return upsertPGModelPricing(ctx, tx, modelRows)
}

func deletePGModelPricing(
	ctx context.Context, tx *sql.Tx, patterns []string,
) error {
	for i := 0; i < len(patterns); i += pricingUpsertBatch {
		end := min(i+pricingUpsertBatch, len(patterns))
		for _, table := range []string{"model_pricing_bands", "model_pricing"} {
			query, args := pgPricingDeleteStatement(table, patterns[i:end])
			if _, err := tx.ExecContext(ctx, query, args...); err != nil {
				return fmt.Errorf(
					"deleting pg %s rows at batch %d: %w", table, i, err)
			}
		}
	}
	return nil
}

func upsertPGModelPricing(
	ctx context.Context, tx *sql.Tx, prices []db.ModelPricing,
) error {
	defaultUpdatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	baseChanged := make(map[string]struct{}, len(prices))
	for i := 0; i < len(prices); i += pricingUpsertBatch {
		end := min(i+pricingUpsertBatch, len(prices))
		query, args := pgPricingUpsertStatement(
			prices[i:end], defaultUpdatedAt,
		)
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf(
				"upserting pg pricing batch starting at %d: %w",
				i, err,
			)
		}
		for rows.Next() {
			var modelPattern string
			if err := rows.Scan(&modelPattern); err != nil {
				rows.Close()
				return fmt.Errorf(
					"scanning changed pg pricing at batch %d: %w", i, err)
			}
			baseChanged[modelPattern] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf(
				"iterating changed pg pricing at batch %d: %w", i, err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf(
				"closing changed pg pricing at batch %d: %w", i, err)
		}
	}
	bandOnlyPrices := make([]db.ModelPricing, 0, len(prices))
	for _, price := range prices {
		if _, changed := baseChanged[price.ModelPattern]; !changed {
			bandOnlyPrices = append(bandOnlyPrices, price)
		}
	}
	for i := 0; i < len(bandOnlyPrices); i += pricingUpsertBatch {
		end := min(i+pricingUpsertBatch, len(bandOnlyPrices))
		batch := bandOnlyPrices[i:end]
		query, args := pgPricingTouchStatement(batch, defaultUpdatedAt)
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf(
				"advancing pg pricing timestamps at batch %d: %w", i, err)
		}
	}
	for i := 0; i < len(prices); i += pricingUpsertBatch {
		end := min(i+pricingUpsertBatch, len(prices))
		batch := prices[i:end]
		query, args := pgPricingBandDeleteStatement(batch)
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf(
				"deleting pg pricing bands at batch %d: %w", i, err)
		}
	}
	var bands []pgModelPricingBand
	for _, price := range prices {
		for _, band := range price.Bands {
			bands = append(bands, pgModelPricingBand{
				model: price.ModelPattern,
				band:  band,
			})
		}
	}
	for i := 0; i < len(bands); i += pricingUpsertBatch {
		end := min(i+pricingUpsertBatch, len(bands))
		query, args := pgPricingBandInsertStatement(
			bands[i:end], defaultUpdatedAt)
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf(
				"inserting pg pricing bands at batch %d: %w", i, err)
		}
	}
	return nil
}

// pricingSyncLockKey names the sync_metadata row a pricing sync locks
// for the life of its transaction.
const pricingSyncLockKey = "model_pricing_sync_lock"

// lockPGModelPricing serializes concurrent pricing syncs by locking a
// dedicated sync_metadata row until tx ends. A row lock is used instead
// of pg_advisory_xact_lock because supported CockroachDB versions do not
// implement advisory locks, and sync_metadata lives in the target
// schema, so the lock is schema-scoped on both engines.
func lockPGModelPricing(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sync_metadata (key, value) VALUES ($1, '')
		 ON CONFLICT (key) DO NOTHING`,
		pricingSyncLockKey,
	); err != nil {
		return fmt.Errorf("creating pg model pricing lock row: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = $1 FOR UPDATE`,
		pricingSyncLockKey,
	); err != nil {
		return fmt.Errorf("locking pg model pricing: %w", err)
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
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning pg pricing sync: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Pushes from several machines read the same ownership sentinel and
	// each write a merged copy, so read, plan, and write are serialized
	// under one lock; otherwise a slower push could overwrite ownership a
	// faster one recorded and leave its rows untracked.
	if err := lockPGModelPricing(ctx, tx); err != nil {
		return err
	}
	existing, err := listPGModelPricing(ctx, tx)
	if err != nil {
		return fmt.Errorf("listing pg model pricing: %w", err)
	}
	changedPrices, removePatterns, err := db.PlanModelPricingSync(
		existing, prices,
	)
	if err != nil {
		return fmt.Errorf("planning model pricing sync: %w", err)
	}
	if len(changedPrices) == 0 && len(removePatterns) == 0 {
		return nil
	}
	if err := reconcileModelPricing(
		ctx, tx, changedPrices, removePatterns,
	); err != nil {
		return fmt.Errorf("syncing model pricing to pg: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing pg pricing sync: %w", err)
	}
	return nil
}
