package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/export"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
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

// GenAIPricingState describes a stored GenAI pricing document without its
// data. Mirrors read it to decide whether the archive's document changed
// without transferring the document, which is close to a megabyte.
type GenAIPricingState struct {
	Version   string
	SourceRef string
	Source    string
	// DataSHA256 is the lowercase hexadecimal SHA-256 of the stored data,
	// as GenAIPricingDataSHA256 computes it.
	DataSHA256 string
	UpdatedAt  string
}

// GenAIPricingDataSHA256 returns the lowercase hexadecimal SHA-256 of a
// GenAI pricing document's data.
func GenAIPricingDataSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Matches reports whether the stored document has the same pricing content
// and provenance as document, comparing the data by digest. UpdatedAt is
// observation metadata and does not make otherwise identical documents
// different. A nil state is a missing document.
func (s *GenAIPricingState) Matches(document *GenAIPricingDocument) bool {
	if s == nil || document == nil {
		return s == nil && document == nil
	}
	return s.Version == document.Version &&
		s.SourceRef == document.SourceRef &&
		s.Source == document.Source &&
		s.DataSHA256 == GenAIPricingDataSHA256(document.Data)
}

// GenAIPricingDigest is a stored document's provenance and the SHA-256 of
// its JSON. A replica or mirror reads it in place of the JSON, which is
// several hundred KB, to learn whether this process already parsed the
// document.
type GenAIPricingDigest struct {
	Version   string
	SourceRef string
	Source    string
	UpdatedAt string
	Sum       [sha256.Size]byte
}

// ParseGenAIPricingSum reads the SHA-256 a database computed over a stored
// GenAI Prices document. Databases return it as 32 raw bytes (PostgreSQL) or
// as 64 hex digits in either case (ClickHouse, DuckDB, CockroachDB).
func ParseGenAIPricingSum(dbSum []byte) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	switch len(dbSum) {
	case len(sum):
		copy(sum[:], dbSum)
		return sum, nil
	case hex.EncodedLen(len(sum)):
		if _, err := hex.Decode(sum[:], dbSum); err != nil {
			return sum, fmt.Errorf("decoding GenAI pricing digest: %w", err)
		}
		return sum, nil
	}
	return sum, fmt.Errorf("GenAI pricing digest has %d bytes, not a SHA-256", len(dbSum))
}

// GenAIEffectivePricingRow returns the pricing row of a stored GenAI Prices
// document, or of the embedded document when document is nil. A document
// this process parsed before, with the same bytes, is not parsed again.
func GenAIEffectivePricingRow(
	document *GenAIPricingDocument,
) (export.EffectivePricingRow, error) {
	if document == nil {
		embedded := pricingpkg.EmbeddedGenAIDocument()
		return export.EffectivePricingRow{
			GenAI: embedded.Prices, GenAIVersion: embedded.Version,
			GenAISource: export.PricingRowSourceEmbedded,
		}, nil
	}
	parsed, err := pricingpkg.ParseStoredGenAIDocument(
		document.Data, document.Version, document.SourceRef,
	)
	if err != nil {
		return export.EffectivePricingRow{}, fmt.Errorf(
			"parsing stored GenAI pricing document: %w", err,
		)
	}
	return genAIPricingRow(parsed, document.Source, document.UpdatedAt), nil
}

// CachedGenAIEffectivePricingRow returns the pricing row of the stored
// document that digest describes, when this process already parsed a
// document with the same bytes. Callers read the JSON only when it reports
// false.
func CachedGenAIEffectivePricingRow(
	digest GenAIPricingDigest,
) (export.EffectivePricingRow, bool) {
	parsed, ok := pricingpkg.CachedGenAIDocument(
		digest.Sum, digest.Version, digest.SourceRef,
	)
	if !ok {
		return export.EffectivePricingRow{}, false
	}
	return genAIPricingRow(parsed, digest.Source, digest.UpdatedAt), true
}

func genAIPricingRow(
	parsed pricingpkg.GenAIDocument, source, updatedAt string,
) export.EffectivePricingRow {
	var updated *time.Time
	if t, err := time.Parse(time.RFC3339Nano, updatedAt); err == nil {
		utc := t.UTC()
		updated = &utc
	}
	rowSource := export.PricingRowSourceFetched
	if source == GenAIPricingSourceEmbedded {
		rowSource = export.PricingRowSourceEmbedded
	}
	return export.EffectivePricingRow{
		GenAI: parsed.Prices, GenAIVersion: parsed.Version,
		GenAISource: rowSource, GenAIUpdatedAt: updated,
	}
}

// GetGenAIPricing returns the stored GenAI Prices document, or nil when the
// archive has not been seeded yet.
func (db *DB) GetGenAIPricing(
	ctx context.Context,
) (*GenAIPricingDocument, error) {
	return getGenAIPricingFrom(ctx, db.getReader())
}

func getGenAIPricingFrom(
	ctx context.Context, q recallQueryRower,
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
		return errors.New("writing GenAI pricing document: missing version or data")
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
	if err := db.lockWriterCtx(ctx); err != nil {
		return err
	}
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

// EmbeddedGenAIPricingDocument returns the GenAI pricing snapshot built into
// the binary.
func EmbeddedGenAIPricingDocument() GenAIPricingDocument {
	embedded := pricingpkg.EmbeddedGenAIDocument()
	return GenAIPricingDocument{
		Version: embedded.Version, SourceRef: embedded.SourceRef,
		Source: GenAIPricingSourceEmbedded, Data: embedded.RawJSON(),
	}
}
