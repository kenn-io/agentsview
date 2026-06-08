package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ModelPricing holds per-model token pricing (per million tokens).
type ModelPricing struct {
	ModelPattern         string  `json:"model_pattern"`
	InputPerMTok         float64 `json:"input_per_mtok"`
	OutputPerMTok        float64 `json:"output_per_mtok"`
	CacheCreationPerMTok float64 `json:"cache_creation_per_mtok"`
	CacheReadPerMTok     float64 `json:"cache_read_per_mtok"`
	UpdatedAt            string  `json:"updated_at"`
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
		a.CacheReadPerMTok == b.CacheReadPerMTok
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
			p.InputPerMTok,
			p.OutputPerMTok,
			p.CacheCreationPerMTok,
			p.CacheReadPerMTok,
		)
	}
}

func sqlitePricingUpsertStatement(prices []ModelPricing) (string, []any) {
	var b strings.Builder
	b.WriteString(`INSERT INTO model_pricing
		(model_pattern, input_per_mtok, output_per_mtok,
		 cache_creation_per_mtok, cache_read_per_mtok,
		 updated_at)
	VALUES `)
	args := make([]any, 0, len(prices)*5)
	sqlitePricingValues(&b, &args, prices)
	b.WriteString(`
	ON CONFLICT(model_pattern) DO UPDATE SET
		input_per_mtok          = excluded.input_per_mtok,
		output_per_mtok         = excluded.output_per_mtok,
		cache_creation_per_mtok = excluded.cache_creation_per_mtok,
		cache_read_per_mtok     = excluded.cache_read_per_mtok,
		updated_at              = excluded.updated_at
	WHERE model_pricing.input_per_mtok IS NOT excluded.input_per_mtok
		OR model_pricing.output_per_mtok IS NOT excluded.output_per_mtok
		OR model_pricing.cache_creation_per_mtok IS NOT
			excluded.cache_creation_per_mtok
		OR model_pricing.cache_read_per_mtok IS NOT
			excluded.cache_read_per_mtok`)
	return b.String(), args
}

func sqlitePricingInsertMissingStatement(
	prices []ModelPricing,
) (string, []any) {
	var b strings.Builder
	b.WriteString(`INSERT INTO model_pricing
		(model_pattern, input_per_mtok, output_per_mtok,
		 cache_creation_per_mtok, cache_read_per_mtok,
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
	return tx.Commit()
}

// GetPricingMeta reads a metadata value stored as a sentinel
// row in model_pricing. Returns "" if not found.
func (db *DB) GetPricingMeta(key string) (string, error) {
	var val string
	err := db.getReader().QueryRow(
		`SELECT updated_at FROM model_pricing
		 WHERE model_pattern = ?`, key,
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

// SetPricingMeta stores a metadata value as a sentinel row
// in model_pricing with zero pricing fields.
func (db *DB) SetPricingMeta(key, value string) error {
	_, err := db.getWriter().Exec(
		`INSERT INTO model_pricing
			(model_pattern, input_per_mtok, output_per_mtok,
			 cache_creation_per_mtok, cache_read_per_mtok,
			 updated_at)
		 VALUES (?, 0, 0, 0, 0, ?)
		 ON CONFLICT(model_pattern) DO UPDATE SET
			updated_at = excluded.updated_at`,
		key, value,
	)
	if err != nil {
		return fmt.Errorf(
			"setting pricing meta %q: %w", key, err,
		)
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
	return tx.Commit()
}

// GetModelPricing returns pricing for an exact model match.
// Returns nil, nil if not found.
func (db *DB) GetModelPricing(
	model string,
) (*ModelPricing, error) {
	var p ModelPricing
	err := db.getReader().QueryRow(
		`SELECT model_pattern, input_per_mtok,
			output_per_mtok, cache_creation_per_mtok,
			cache_read_per_mtok, updated_at
		 FROM model_pricing
		 WHERE model_pattern = ?`,
		model,
	).Scan(
		&p.ModelPattern,
		&p.InputPerMTok,
		&p.OutputPerMTok,
		&p.CacheCreationPerMTok,
		&p.CacheReadPerMTok,
		&p.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf(
			"getting pricing %q: %w", model, err,
		)
	}
	return &p, nil
}
