package db

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
)

const (
	GenAIPricingSourceEmbedded = "embedded"
	GenAIPricingSourceFetched  = "fetched"
)

// GenAIPricingDocument is the intact upstream GenAI Prices v2 JSON plus the
// provenance needed to identify embedded and refreshed copies.
type GenAIPricingDocument struct {
	Version   string
	SourceRef string
	Source    string
	Data      []byte
	UpdatedAt string
}

// GenAIPricingDocumentsEqual reports whether two documents have the same
// pricing content and provenance. UpdatedAt is observation metadata and does
// not make otherwise identical pricing documents different.
func GenAIPricingDocumentsEqual(a, b *GenAIPricingDocument) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Version == b.Version &&
		a.SourceRef == b.SourceRef &&
		a.Source == b.Source &&
		bytes.Equal(a.Data, b.Data)
}

type genAIPricingQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// GetGenAIPricing returns the stored GenAI Prices document, or nil when the
// archive has not been seeded yet.
func (db *DB) GetGenAIPricing(
	ctx context.Context,
) (*GenAIPricingDocument, error) {
	return getGenAIPricingFrom(ctx, db.getReader())
}

func getGenAIPricingFrom(
	ctx context.Context, q genAIPricingQuerier,
) (*GenAIPricingDocument, error) {
	var document GenAIPricingDocument
	err := q.QueryRowContext(ctx, `
		SELECT version, source_ref, source, data_json, updated_at
		FROM genai_pricing WHERE singleton = 1`).Scan(
		&document.Version,
		&document.SourceRef,
		&document.Source,
		&document.Data,
		&document.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading GenAI pricing document: %w", err)
	}
	return &document, nil
}

// UpsertGenAIPricing replaces the singleton upstream document after a
// successful refresh. The JSON is stored without translation.
func (db *DB) UpsertGenAIPricing(
	ctx context.Context, document GenAIPricingDocument,
) error {
	return db.writeGenAIPricing(ctx, document, false)
}

// InsertMissingGenAIPricing installs the embedded document, replacing an older
// embedded copy without replacing a document fetched by an earlier process.
func (db *DB) InsertMissingGenAIPricing(
	ctx context.Context, document GenAIPricingDocument,
) error {
	return db.writeGenAIPricing(ctx, document, true)
}

func (db *DB) writeGenAIPricing(
	ctx context.Context, document GenAIPricingDocument, missingOnly bool,
) error {
	if err := db.requireWritable(); err != nil {
		return err
	}
	if document.Version == "" || len(document.Data) == 0 {
		return fmt.Errorf("writing GenAI pricing document: missing version or data")
	}
	if document.Source != GenAIPricingSourceEmbedded &&
		document.Source != GenAIPricingSourceFetched {
		return fmt.Errorf(
			"writing GenAI pricing document: invalid source %q", document.Source,
		)
	}
	conflict := `DO UPDATE SET
		version = excluded.version,
		source_ref = excluded.source_ref,
		source = excluded.source,
		data_json = excluded.data_json,
		updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
	WHERE genai_pricing.version IS NOT excluded.version
		OR genai_pricing.source_ref IS NOT excluded.source_ref
		OR genai_pricing.source IS NOT excluded.source
		OR genai_pricing.data_json IS NOT excluded.data_json`
	if missingOnly {
		conflict = `DO UPDATE SET
			version = excluded.version,
			source_ref = excluded.source_ref,
			source = excluded.source,
			data_json = excluded.data_json,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE genai_pricing.source = 'embedded'
			AND excluded.source = 'embedded'
			AND (genai_pricing.version IS NOT excluded.version
				OR genai_pricing.source_ref IS NOT excluded.source_ref
				OR genai_pricing.data_json IS NOT excluded.data_json)`
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().ExecContext(ctx, `
		INSERT INTO genai_pricing
			(singleton, version, source_ref, source, data_json, updated_at)
		VALUES (1, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		ON CONFLICT(singleton) `+conflict,
		document.Version,
		document.SourceRef,
		document.Source,
		document.Data,
	)
	if err != nil {
		return fmt.Errorf("writing GenAI pricing document: %w", err)
	}
	return nil
}
