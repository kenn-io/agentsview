package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
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
		`SELECT model_pattern, input_per_mtok,
			output_per_mtok, cache_creation_per_mtok,
			cache_read_per_mtok, updated_at
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
			InputPerMTok:      cp.Input,
			OutputPerMTok:     cp.Output,
			CacheWritePerMTok: cp.CacheCreation,
			CacheReadPerMTok:  cp.CacheRead,
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
		(model_pattern, input_per_mtok, output_per_mtok,
		 cache_creation_per_mtok, cache_read_per_mtok,
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
		input_per_mtok = EXCLUDED.input_per_mtok,
		output_per_mtok = EXCLUDED.output_per_mtok,
		cache_creation_per_mtok = EXCLUDED.cache_creation_per_mtok,
		cache_read_per_mtok = EXCLUDED.cache_read_per_mtok,
		updated_at = EXCLUDED.updated_at
	WHERE model_pricing.input_per_mtok IS DISTINCT FROM
			EXCLUDED.input_per_mtok
		OR model_pricing.output_per_mtok IS DISTINCT FROM
			EXCLUDED.output_per_mtok
		OR model_pricing.cache_creation_per_mtok IS DISTINCT FROM
			EXCLUDED.cache_creation_per_mtok
		OR model_pricing.cache_read_per_mtok IS DISTINCT FROM
			EXCLUDED.cache_read_per_mtok
		OR (model_pricing.model_pattern LIKE '\_%' ESCAPE '\'
			AND model_pricing.updated_at IS DISTINCT FROM
				EXCLUDED.updated_at)`)
	return b.String(), args
}

func listPGModelPricing(
	ctx context.Context, pg *sql.DB,
) ([]db.ModelPricing, error) {
	rows, err := pg.QueryContext(ctx,
		`SELECT model_pattern, input_per_mtok,
			output_per_mtok, cache_creation_per_mtok,
			cache_read_per_mtok, updated_at
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

func reconcileModelPricing(
	ctx context.Context, pg *sql.DB, prices []db.ModelPricing,
	removePatterns []string,
) error {
	if len(prices) == 0 && len(removePatterns) == 0 {
		return nil
	}

	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning pg pricing upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for i := 0; i < len(removePatterns); i += pricingUpsertBatch {
		end := min(i+pricingUpsertBatch, len(removePatterns))
		placeholders := make([]string, end-i)
		args := make([]any, end-i)
		for j, pattern := range removePatterns[i:end] {
			placeholders[j] = fmt.Sprintf("$%d", j+1)
			args[j] = pattern
		}
		query := `DELETE FROM model_pricing WHERE model_pattern IN (` +
			strings.Join(placeholders, ", ") + `)`
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf(
				"removing obsolete pricing batch starting at %d: %w",
				i, err,
			)
		}
	}

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

// pricingSyncChanges plans one push of the local pricing table onto
// PostgreSQL: the rows to upsert and the patterns to delete.
//
// Deletions are driven by provenance sentinels rather than by set
// difference, because a pattern PostgreSQL holds and the local archive
// lacks is indistinguishable from one another machine pushed:
//
//   - _openrouter_aliases lists the bare aliases the local refresh
//     currently emits. Aliases in PostgreSQL's copy of that list but not
//     in the local one were retired, so they are removed.
//   - _openrouter_shadowed lists the patterns the local refresh
//     suppressed because a higher-priority source already covers them.
//     That list is absolute, not a diff: every pattern on it must not
//     exist, so it is removed without consulting PostgreSQL's copy.
//
// Without the second sentinel a qualified OpenRouter row pushed before
// LiteLLM listed the model (minimax/minimax-m3 against a later
// minimax/MiniMax-M3) would survive in PostgreSQL after the local
// reconciliation dropped it, and pg serve would keep resolving the bare
// model name against two tied keys, treat it as ambiguous, and price it
// at zero.
func pricingSyncChanges(
	existing, desired []db.ModelPricing,
) ([]db.ModelPricing, []string, error) {
	removeSet := make(map[string]struct{})

	shadowedMeta, shadowedFound := pricingMetaRow(
		desired, pricing.OpenRouterShadowedMetaKey,
	)
	if shadowedFound {
		shadowed, err := decodePricingAliases(shadowedMeta.UpdatedAt)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"decoding local OpenRouter shadowed patterns: %w", err,
			)
		}
		for _, pattern := range shadowed {
			removeSet[pattern] = struct{}{}
		}
	}

	aliasMeta, aliasFound := pricingMetaRow(
		desired, pricing.OpenRouterAliasesMetaKey,
	)
	existingAliasMeta, existingAliasFound := pricingMetaRow(
		existing, pricing.OpenRouterAliasesMetaKey,
	)
	if aliasFound && existingAliasFound {
		currentAliases, err := decodePricingAliases(aliasMeta.UpdatedAt)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"decoding local OpenRouter aliases: %w", err,
			)
		}
		previousAliases, err := decodePricingAliases(
			existingAliasMeta.UpdatedAt,
		)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"decoding PostgreSQL OpenRouter aliases: %w", err,
			)
		}
		current := make(map[string]struct{}, len(currentAliases))
		for _, alias := range currentAliases {
			current[alias] = struct{}{}
		}
		for _, alias := range previousAliases {
			if _, ok := current[alias]; !ok {
				removeSet[alias] = struct{}{}
			}
		}
	}

	remove := make([]string, 0, len(removeSet))
	for pattern := range removeSet {
		remove = append(remove, pattern)
	}
	sort.Strings(remove)

	// Removed patterns are dropped from the comparison baseline so a
	// pattern a surviving source still publishes is re-upserted after the
	// delete instead of being left missing.
	kept := make([]db.ModelPricing, 0, len(existing))
	for _, row := range existing {
		if _, removing := removeSet[row.ModelPattern]; !removing {
			kept = append(kept, row)
		}
	}
	_, changed := db.FilterChangedModelPricing(kept, desired)
	if aliasFound {
		changed = appendChangedPricingMeta(changed, existing, aliasMeta)
	}
	if shadowedFound {
		changed = appendChangedPricingMeta(changed, existing, shadowedMeta)
	}
	return changed, remove, nil
}

// appendChangedPricingMeta re-adds a provenance sentinel whose payload
// changed. FilterChangedModelPricing compares only rate fields and
// sentinels carry all zeros, so a new payload alone never marks the row
// changed. A sentinel PostgreSQL does not hold yet is already reported as
// missing, so only the both-present case needs the extra push.
func appendChangedPricingMeta(
	changed, existing []db.ModelPricing, meta db.ModelPricing,
) []db.ModelPricing {
	previous, found := pricingMetaRow(existing, meta.ModelPattern)
	if !found || previous.UpdatedAt == meta.UpdatedAt {
		return changed
	}
	if containsPricingPattern(changed, meta.ModelPattern) {
		return changed
	}
	return append(changed, meta)
}

func pricingMetaRow(
	rows []db.ModelPricing, key string,
) (db.ModelPricing, bool) {
	for _, row := range rows {
		if row.ModelPattern == key {
			return row, true
		}
	}
	return db.ModelPricing{}, false
}

func decodePricingAliases(value string) ([]string, error) {
	var aliases []string
	if err := json.Unmarshal([]byte(value), &aliases); err != nil {
		return nil, err
	}
	return aliases, nil
}

func containsPricingPattern(
	rows []db.ModelPricing, pattern string,
) bool {
	for _, row := range rows {
		if row.ModelPattern == pattern {
			return true
		}
	}
	return false
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
	changedPrices, removePatterns, err := pricingSyncChanges(
		existing, prices,
	)
	if err != nil {
		return fmt.Errorf("planning model pricing sync: %w", err)
	}
	if len(changedPrices) == 0 && len(removePatterns) == 0 {
		return nil
	}
	if err := reconcileModelPricing(
		ctx, s.pg, changedPrices, removePatterns,
	); err != nil {
		return fmt.Errorf("syncing model pricing to pg: %w", err)
	}
	return nil
}
