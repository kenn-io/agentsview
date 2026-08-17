package db

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/money"
)

// ModelPricing holds per-model token pricing (per million tokens).
type ModelPricing struct {
	ModelPattern         string        `json:"model_pattern"`
	InputPerMTok         money.Money   `json:"input_per_mtok"`
	OutputPerMTok        money.Money   `json:"output_per_mtok"`
	CacheCreationPerMTok money.Money   `json:"cache_creation_per_mtok"`
	CacheReadPerMTok     money.Money   `json:"cache_read_per_mtok"`
	UpdatedAt            string        `json:"updated_at"`
	Bands                []PricingBand `json:"bands"`
}

type PricingBand struct {
	AboveInputTokens     int         `json:"above_input_tokens"`
	InputPerMTok         money.Money `json:"input_per_mtok"`
	OutputPerMTok        money.Money `json:"output_per_mtok"`
	CacheCreationPerMTok money.Money `json:"cache_creation_per_mtok"`
	CacheReadPerMTok     money.Money `json:"cache_read_per_mtok"`
	UpdatedAt            string      `json:"updated_at"`
}

// PricingChangeSummary describes how desired pricing rows compare
// with rows already stored in a backend.
type PricingChangeSummary struct {
	Total     int
	Missing   int
	Changed   int
	Unchanged int
}

const pricingWriteBatch = 100

// FilterChangedModelPricing returns the subset of desired rows that
// would actually insert or update pricing fields. UpdatedAt-only
// differences are intentionally ignored to match the upsert WHERE
// clause used by both SQLite and PostgreSQL.
func FilterChangedModelPricing(
	existing, desired []ModelPricing,
) (PricingChangeSummary, []ModelPricing) {
	byPattern := make(map[string]ModelPricing, len(existing))
	for _, p := range existing {
		byPattern[p.ModelPattern] = p
	}

	summary := PricingChangeSummary{Total: len(desired)}
	changed := make([]ModelPricing, 0, len(desired))
	for _, p := range desired {
		current, ok := byPattern[p.ModelPattern]
		if !ok {
			summary.Missing++
			changed = append(changed, p)
			continue
		}
		if pricingFieldsEqual(current, p) {
			summary.Unchanged++
			continue
		}
		summary.Changed++
		changed = append(changed, p)
	}
	return summary, changed
}

func pricingFieldsEqual(a, b ModelPricing) bool {
	return a.InputPerMTok == b.InputPerMTok &&
		a.OutputPerMTok == b.OutputPerMTok &&
		a.CacheCreationPerMTok == b.CacheCreationPerMTok &&
		a.CacheReadPerMTok == b.CacheReadPerMTok &&
		pricingBandsEqual(a.Bands, b.Bands)
}

func pricingBandsEqual(a, b []PricingBand) bool {
	if len(a) != len(b) {
		return false
	}
	a = slices.Clone(a)
	b = slices.Clone(b)
	slices.SortFunc(a, comparePricingBands)
	slices.SortFunc(b, comparePricingBands)
	for i := range a {
		if comparePricingBands(a[i], b[i]) != 0 {
			return false
		}
	}
	return true
}

func comparePricingBands(a, b PricingBand) int {
	for _, comparison := range []int{
		cmp.Compare(a.AboveInputTokens, b.AboveInputTokens),
		cmp.Compare(a.InputPerMTok.Microdollars, b.InputPerMTok.Microdollars),
		cmp.Compare(a.OutputPerMTok.Microdollars, b.OutputPerMTok.Microdollars),
		cmp.Compare(a.CacheCreationPerMTok.Microdollars, b.CacheCreationPerMTok.Microdollars),
		cmp.Compare(a.CacheReadPerMTok.Microdollars, b.CacheReadPerMTok.Microdollars),
	} {
		if comparison != 0 {
			return comparison
		}
	}
	return 0
}

func sqlitePricingValues(
	b *strings.Builder, args *[]any, prices []ModelPricing,
) {
	for i, p := range prices {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(
			"(?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))",
		)
		*args = append(*args,
			p.ModelPattern,
			p.InputPerMTok.Microdollars,
			p.OutputPerMTok.Microdollars,
			p.CacheCreationPerMTok.Microdollars,
			p.CacheReadPerMTok.Microdollars,
		)
	}
}

func sqlitePricingUpsertStatement(prices []ModelPricing) (string, []any) {
	var b strings.Builder
	b.WriteString(`INSERT INTO model_pricing
		(model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok,
		 cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok,
		 updated_at)
	VALUES `)
	args := make([]any, 0, len(prices)*5)
	sqlitePricingValues(&b, &args, prices)
	b.WriteString(`
	ON CONFLICT(model_pattern) DO UPDATE SET
		input_microdollars_per_mtok          = excluded.input_microdollars_per_mtok,
		output_microdollars_per_mtok         = excluded.output_microdollars_per_mtok,
		cache_creation_microdollars_per_mtok = excluded.cache_creation_microdollars_per_mtok,
		cache_read_microdollars_per_mtok     = excluded.cache_read_microdollars_per_mtok,
		updated_at              = excluded.updated_at
	WHERE model_pricing.input_microdollars_per_mtok IS NOT excluded.input_microdollars_per_mtok
		OR model_pricing.output_microdollars_per_mtok IS NOT excluded.output_microdollars_per_mtok
		OR model_pricing.cache_creation_microdollars_per_mtok IS NOT
			excluded.cache_creation_microdollars_per_mtok
		OR model_pricing.cache_read_microdollars_per_mtok IS NOT
			excluded.cache_read_microdollars_per_mtok`)
	return b.String(), args
}

func sqlitePricingInsertMissingStatement(
	prices []ModelPricing,
) (string, []any) {
	var b strings.Builder
	b.WriteString(`INSERT INTO model_pricing
		(model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok,
		 cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok,
		 updated_at)
	VALUES `)
	args := make([]any, 0, len(prices)*5)
	sqlitePricingValues(&b, &args, prices)
	b.WriteString(`
	ON CONFLICT(model_pattern) DO NOTHING`)
	return b.String(), args
}

// UpsertModelPricing inserts or replaces pricing rows in a
// single transaction.
func (db *DB) UpsertModelPricing(
	prices []ModelPricing,
) error {
	if err := db.requireWritable(); err != nil {
		return err
	}
	if len(prices) == 0 {
		return nil
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	existing, err := db.listModelPricing(context.Background())
	if err != nil {
		return fmt.Errorf(
			"listing current pricing before upsert: %w", err,
		)
	}
	_, prices = FilterChangedModelPricing(existing, prices)
	if len(prices) == 0 {
		return nil
	}

	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("beginning pricing upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for i := 0; i < len(prices); i += pricingWriteBatch {
		end := min(i+pricingWriteBatch, len(prices))
		query, args := sqlitePricingUpsertStatement(prices[i:end])
		if _, err := tx.Exec(query, args...); err != nil {
			return fmt.Errorf(
				"upserting pricing batch starting at %d: %w",
				i, err,
			)
		}
	}
	for _, price := range prices {
		if _, err := tx.Exec(`
			UPDATE model_pricing
			SET updated_at = CASE
				WHEN updated_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now')
				THEN strftime(
					'%Y-%m-%dT%H:%M:%fZ', updated_at, '+0.001 seconds')
				ELSE strftime('%Y-%m-%dT%H:%M:%fZ','now')
			END
			WHERE model_pattern = ?`, price.ModelPattern); err != nil {
			return fmt.Errorf(
				"advancing pricing timestamp for %q: %w",
				price.ModelPattern,
				err,
			)
		}
	}
	if err := replaceModelPricingBands(tx, prices); err != nil {
		return err
	}
	return tx.Commit()
}

func replaceModelPricingBands(tx *sql.Tx, prices []ModelPricing) error {
	for _, price := range prices {
		if _, err := tx.Exec(
			`DELETE FROM model_pricing_bands WHERE model_pattern = ?`,
			price.ModelPattern,
		); err != nil {
			return fmt.Errorf(
				"deleting pricing bands for %q: %w",
				price.ModelPattern,
				err,
			)
		}
		for _, band := range price.Bands {
			if _, err := tx.Exec(`
				INSERT INTO model_pricing_bands
					(model_pattern, above_input_tokens,
					 input_microdollars_per_mtok, output_microdollars_per_mtok,
					 cache_creation_microdollars_per_mtok,
					 cache_read_microdollars_per_mtok, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))`,
				price.ModelPattern,
				band.AboveInputTokens,
				band.InputPerMTok.Microdollars,
				band.OutputPerMTok.Microdollars,
				band.CacheCreationPerMTok.Microdollars,
				band.CacheReadPerMTok.Microdollars,
			); err != nil {
				return fmt.Errorf(
					"inserting pricing band %d for %q: %w",
					band.AboveInputTokens,
					price.ModelPattern,
					err,
				)
			}
		}
	}
	return nil
}

// DeleteModelPricing removes pricing rows by exact model pattern.
// Used by the version-gated fallback seed to drop stale curated alias
// rows that a previous binary seeded but the current fallback set no
// longer carries (an exact-match row would otherwise shadow the
// date-based pricing path for those names).
func (db *DB) DeleteModelPricing(patterns []string) error {
	if err := db.requireWritable(); err != nil {
		return err
	}
	if len(patterns) == 0 {
		return nil
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	placeholders := make([]string, len(patterns))
	args := make([]any, len(patterns))
	for i, p := range patterns {
		placeholders[i] = "?"
		args[i] = p
	}
	_, err := db.getWriter().Exec(
		`DELETE FROM model_pricing
		 WHERE model_pattern IN (`+strings.Join(placeholders, ",")+`)`,
		args...,
	)
	if err != nil {
		return fmt.Errorf("deleting pricing rows: %w", err)
	}
	return nil
}

// GetPricingMeta reads SQLite-local pricing refresh state. Returns "" if not
// found.
func (db *DB) GetPricingMeta(key string) (string, error) {
	var val string
	err := db.getReader().QueryRow(
		`SELECT value FROM pricing_metadata WHERE key = ?`, key,
	).Scan(&val)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf(
			"reading pricing meta %q: %w", key, err,
		)
	}
	return val, nil
}

// SetPricingMeta stores SQLite-local pricing refresh state separately from
// model rates.
func (db *DB) SetPricingMeta(key, value string) error {
	_, err := db.getWriter().Exec(
		`INSERT INTO pricing_metadata (key, value)
		 VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')`,
		key, value,
	)
	if err != nil {
		return fmt.Errorf(
			"setting pricing meta %q: %w", key, err,
		)
	}
	return nil
}

// CopyModelPricingFrom copies model pricing, bands, and SQLite-local pricing
// refresh metadata from the database file at sourcePath.
// Called during a resync so the rebuilt DB keeps pricing across the
// swap; without it every usage cost reads as zero until the next
// daemon startup re-seeds the table.
func (db *DB) CopyModelPricingFrom(sourcePath string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	// Pin a single connection: ATTACH is connection-scoped and
	// database/sql's pool doesn't guarantee the same underlying
	// connection across separate Exec calls.
	ctx := context.Background()
	conn, err := db.getWriter().Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(
		ctx, "ATTACH DATABASE ? AS old_db", sourcePath,
	); err != nil {
		return fmt.Errorf("attaching source db: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(ctx, "DETACH DATABASE old_db")
	}()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning model pricing copy: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO model_pricing
			(model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok,
			 cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok,
			 updated_at)
		SELECT model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok,
			updated_at
		FROM old_db.model_pricing`,
	); err != nil {
		return fmt.Errorf("copying model pricing: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO model_pricing_bands
			(model_pattern, above_input_tokens,
			 input_microdollars_per_mtok, output_microdollars_per_mtok,
			 cache_creation_microdollars_per_mtok,
			 cache_read_microdollars_per_mtok, updated_at)
		SELECT model_pattern, above_input_tokens,
			input_microdollars_per_mtok, output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok,
			cache_read_microdollars_per_mtok, updated_at
		FROM old_db.model_pricing_bands`,
	); err != nil {
		return fmt.Errorf("copying model pricing bands: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO pricing_metadata (key, value, updated_at)
		SELECT key, value, updated_at FROM old_db.pricing_metadata`,
	); err != nil {
		return fmt.Errorf("copying pricing metadata: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing model pricing copy: %w", err)
	}
	return nil
}

// InsertMissingModelPricing inserts pricing rows only for model
// patterns not already present, leaving existing rows untouched.
// Used by the direct CLI usage path to guarantee fallback rates
// exist without clobbering richer LiteLLM rows a running server may
// have written. Unlike UpsertModelPricing (ON CONFLICT DO UPDATE),
// this is non-destructive (ON CONFLICT DO NOTHING).
func (db *DB) InsertMissingModelPricing(
	prices []ModelPricing,
) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	existing, err := db.listModelPricing(context.Background())
	if err != nil {
		return fmt.Errorf("listing current pricing before insert: %w", err)
	}
	existingPatterns := make(map[string]struct{}, len(existing))
	for _, price := range existing {
		existingPatterns[price.ModelPattern] = struct{}{}
	}
	missing := make([]ModelPricing, 0, len(prices))
	for _, price := range prices {
		if _, exists := existingPatterns[price.ModelPattern]; !exists {
			missing = append(missing, price)
		}
	}
	prices = missing
	if len(prices) == 0 {
		return nil
	}

	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("beginning pricing insert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for i := 0; i < len(prices); i += pricingWriteBatch {
		end := min(i+pricingWriteBatch, len(prices))
		query, args := sqlitePricingInsertMissingStatement(prices[i:end])
		if _, err := tx.Exec(query, args...); err != nil {
			return fmt.Errorf(
				"inserting pricing batch starting at %d: %w",
				i, err,
			)
		}
	}
	if err := replaceModelPricingBands(tx, prices); err != nil {
		return err
	}
	return tx.Commit()
}

// GetModelPricing returns pricing for an exact model match.
// Returns nil, nil if not found.
// HasModelPricingRows reports whether any pricing rows are stored.
func (db *DB) HasModelPricingRows(ctx context.Context) (bool, error) {
	var exists bool
	err := db.getReader().QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM model_pricing)`,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("checking pricing rows: %w", err)
	}
	return exists, nil
}

func (db *DB) GetModelPricing(
	model string,
) (*ModelPricing, error) {
	prices, err := db.listModelPricing(context.Background())
	if err != nil {
		return nil, fmt.Errorf(
			"getting pricing %q: %w", model, err,
		)
	}
	for i := range prices {
		if prices[i].ModelPattern == model {
			return &prices[i], nil
		}
	}
	return nil, nil
}
