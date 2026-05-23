package db

import (
	"database/sql"
	"fmt"
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

// UpsertModelPricing inserts or replaces pricing rows in a
// single transaction.
func (db *DB) UpsertModelPricing(
	prices []ModelPricing,
) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("beginning pricing upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`
		INSERT INTO model_pricing
			(model_pattern, input_per_mtok, output_per_mtok,
			 cache_creation_per_mtok, cache_read_per_mtok,
			 updated_at)
		VALUES (?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		ON CONFLICT(model_pattern) DO UPDATE SET
			input_per_mtok          = excluded.input_per_mtok,
			output_per_mtok         = excluded.output_per_mtok,
			cache_creation_per_mtok = excluded.cache_creation_per_mtok,
			cache_read_per_mtok     = excluded.cache_read_per_mtok,
			updated_at              = excluded.updated_at`)
	if err != nil {
		return fmt.Errorf("preparing pricing upsert: %w", err)
	}
	defer stmt.Close()

	for _, p := range prices {
		if _, err := stmt.Exec(
			p.ModelPattern,
			p.InputPerMTok,
			p.OutputPerMTok,
			p.CacheCreationPerMTok,
			p.CacheReadPerMTok,
		); err != nil {
			return fmt.Errorf(
				"upserting pricing %q: %w",
				p.ModelPattern, err,
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

	stmt, err := tx.Prepare(`
		INSERT INTO model_pricing
			(model_pattern, input_per_mtok, output_per_mtok,
			 cache_creation_per_mtok, cache_read_per_mtok,
			 updated_at)
		VALUES (?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		ON CONFLICT(model_pattern) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("preparing pricing insert: %w", err)
	}
	defer stmt.Close()

	for _, p := range prices {
		if _, err := stmt.Exec(
			p.ModelPattern,
			p.InputPerMTok,
			p.OutputPerMTok,
			p.CacheCreationPerMTok,
			p.CacheReadPerMTok,
		); err != nil {
			return fmt.Errorf(
				"inserting pricing %q: %w", p.ModelPattern, err,
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
