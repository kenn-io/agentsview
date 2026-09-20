package clickhouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/ccoveille/go-safecast/v2"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

const pricingUpsertBatch = 100

func (s *Sync) syncModelPricing(ctx context.Context) error {
	prices, err := s.local.ListModelPricing(ctx)
	if err != nil {
		return err
	}
	if len(prices) == 0 {
		prices = fallbackPricingRows()
	}
	if len(prices) == 0 {
		return s.syncGenAIPricing(ctx)
	}

	existing, err := readModelPricing(ctx, s.conn)
	if err != nil {
		return err
	}
	prices, removePatterns, err := db.PlanModelPricingSync(existing, prices)
	if err != nil {
		return fmt.Errorf("planning clickhouse pricing sync: %w", err)
	}
	if len(prices) == 0 && len(removePatterns) == 0 {
		return s.syncGenAIPricing(ctx)
	}
	if err := s.removeModelPricing(ctx, removePatterns); err != nil {
		return err
	}
	if err := s.replaceModelPricing(ctx, prices); err != nil {
		return err
	}
	return s.syncGenAIPricing(ctx)
}

func fallbackPricingRows() []db.ModelPricing {
	src := pricingpkg.FallbackPricing()
	out := make([]db.ModelPricing, len(src))
	now := time.Now().UTC().Format(time.RFC3339Nano)
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
				UpdatedAt:              now,
			}
		}
		out[i] = db.ModelPricing{
			ModelPattern:           p.ModelPattern,
			InputPerMTok:           p.InputPerMTok,
			OutputPerMTok:          p.OutputPerMTok,
			CacheCreationPerMTok:   p.CacheCreationPerMTok,
			CacheCreation1hPerMTok: p.CacheCreation1hPerMTok,
			CacheReadPerMTok:       p.CacheReadPerMTok,
			UpdatedAt:              now,
			Bands:                  bands,
		}
	}
	return out
}

func (s *Sync) removeModelPricing(ctx context.Context, patterns []string) error {
	if len(patterns) == 0 {
		return nil
	}
	for _, table := range []string{"model_pricing_bands", "model_pricing"} {
		if err := deleteByPatterns(ctx, s.conn, table, patterns, nil); err != nil {
			return err
		}
	}
	return nil
}

func (s *Sync) replaceModelPricing(ctx context.Context, prices []db.ModelPricing) error {
	if len(prices) == 0 {
		return nil
	}
	version := newPushVersion()
	priceRows := make([][]any, 0, len(prices))
	bandRows := make([][]any, 0)
	patterns := make([]string, 0, len(prices))
	for _, p := range prices {
		patterns = append(patterns, p.ModelPattern)
		priceRows = append(priceRows, []any{
			p.ModelPattern,
			p.InputPerMTok.Microdollars,
			p.OutputPerMTok.Microdollars,
			p.CacheCreationPerMTok.Microdollars,
			p.CacheCreation1hPerMTok.Microdollars,
			p.CacheReadPerMTok.Microdollars,
			p.UpdatedAt,
			version,
		})
		for _, band := range p.Bands {
			updatedAt := band.UpdatedAt
			if updatedAt == "" {
				updatedAt = p.UpdatedAt
			}
			bandRows = append(bandRows, []any{
				p.ModelPattern,
				int64(band.AboveInputTokens),
				band.InputPerMTok.Microdollars,
				band.OutputPerMTok.Microdollars,
				band.CacheCreationPerMTok.Microdollars,
				band.CacheCreation1hPerMTok.Microdollars,
				band.CacheReadPerMTok.Microdollars,
				updatedAt,
				version,
			})
		}
	}
	if err := insertRows(ctx, s.conn, "model_pricing", priceRows); err != nil {
		return err
	}
	if err := insertRows(ctx, s.conn, "model_pricing_bands", bandRows); err != nil {
		return err
	}
	if err := deleteByPatterns(ctx, s.conn, "model_pricing", patterns, &version); err != nil {
		return err
	}
	return deleteByPatterns(ctx, s.conn, "model_pricing_bands", patterns, &version)
}

func deleteByPatterns(
	ctx context.Context, conn *sql.DB, table string, patterns []string, version *uint64,
) error {
	for start := 0; start < len(patterns); start += pricingUpsertBatch {
		end := min(start+pricingUpsertBatch, len(patterns))
		batch := patterns[start:end]
		placeholders, args := inArgs(batch)
		query := "DELETE FROM " + table + " WHERE model_pattern IN (" + placeholders + ")"
		if version != nil {
			query += " AND push_version < ?"
			args = append(args, *version)
		}
		if _, err := conn.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("deleting clickhouse %s rows starting at %d: %w", table, start, err)
		}
	}
	return nil
}

type genAIPricingQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type genAIPricingRow interface {
	Scan(...any) error
}

func embeddedGenAIPricingDocument() db.GenAIPricingDocument {
	embedded := pricingpkg.EmbeddedGenAIDocument()
	return db.GenAIPricingDocument{
		Version: embedded.Version, SourceRef: embedded.SourceRef,
		Source: db.GenAIPricingSourceEmbedded, Data: embedded.RawJSON(),
	}
}

func loadGenAIPricing(
	ctx context.Context, q genAIPricingQuerier,
) (*db.GenAIPricingDocument, error) {
	return scanGenAIPricing(q.QueryRowContext(ctx, `
		SELECT version, source_ref, source, data_json, updated_at
		FROM genai_pricing WHERE singleton = 1`))
}

func scanGenAIPricing(row genAIPricingRow) (*db.GenAIPricingDocument, error) {
	var document db.GenAIPricingDocument
	var data string
	err := row.Scan(
		&document.Version, &document.SourceRef, &document.Source,
		&data, &document.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading clickhouse GenAI pricing document: %w", err)
	}
	document.Data = []byte(data)
	return &document, nil
}

func genAIEffectivePricingRow(
	document *db.GenAIPricingDocument,
) (export.EffectivePricingRow, error) {
	if document == nil {
		embedded := pricingpkg.EmbeddedGenAIDocument()
		return export.EffectivePricingRow{
			GenAI: embedded.Prices, GenAIVersion: embedded.Version,
			GenAISource: export.PricingRowSourceEmbedded,
		}, nil
	}
	parsed, err := pricingpkg.ParseGenAIDocument(
		document.Data, document.Version, document.SourceRef,
	)
	if err != nil {
		return export.EffectivePricingRow{}, fmt.Errorf(
			"parsing clickhouse GenAI pricing document: %w", err,
		)
	}
	var updatedAt *time.Time
	if parsedTime, parseErr := time.Parse(
		time.RFC3339Nano, document.UpdatedAt,
	); parseErr == nil {
		utc := parsedTime.UTC()
		updatedAt = &utc
	}
	source := export.PricingRowSourceFetched
	if document.Source == db.GenAIPricingSourceEmbedded {
		source = export.PricingRowSourceEmbedded
	}
	return export.EffectivePricingRow{
		GenAI: parsed.Prices, GenAIVersion: parsed.Version,
		GenAISource: source, GenAIUpdatedAt: updatedAt,
	}, nil
}

func (s *Sync) syncGenAIPricing(ctx context.Context) error {
	document, err := s.local.GetGenAIPricing(ctx)
	if err != nil {
		return fmt.Errorf("reading local GenAI pricing document: %w", err)
	}
	if document == nil {
		embedded := embeddedGenAIPricingDocument()
		document = &embedded
	}
	existing, err := loadGenAIPricing(ctx, s.conn)
	if err != nil {
		return err
	}
	if db.GenAIPricingDocumentsEqual(existing, document) {
		return nil
	}
	version := newPushVersion()
	row := [][]any{{
		int64(1),
		document.Version,
		document.SourceRef,
		document.Source,
		string(document.Data),
		document.UpdatedAt,
		version,
	}}
	if err := insertRows(ctx, s.conn, "genai_pricing", row); err != nil {
		return err
	}
	if _, err := s.conn.ExecContext(ctx,
		"DELETE FROM genai_pricing WHERE singleton = 1 AND push_version < ?",
		version,
	); err != nil {
		return fmt.Errorf("deleting older clickhouse genai_pricing rows: %w", err)
	}
	return nil
}

func readModelPricing(ctx context.Context, conn *sql.DB) ([]db.ModelPricing, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT model_pattern, input_microdollars_per_mtok,
			output_microdollars_per_mtok, cache_creation_microdollars_per_mtok,
			cache_creation_1h_microdollars_per_mtok,
			cache_read_microdollars_per_mtok, updated_at
		FROM model_pricing
		ORDER BY model_pattern`)
	if err != nil {
		return nil, fmt.Errorf("listing clickhouse pricing: %w", err)
	}
	defer rows.Close()

	out := make([]db.ModelPricing, 0)
	byPattern := make(map[string]int)
	for rows.Next() {
		var p db.ModelPricing
		if err := rows.Scan(
			&p.ModelPattern,
			&p.InputPerMTok,
			&p.OutputPerMTok,
			&p.CacheCreationPerMTok,
			&p.CacheCreation1hPerMTok,
			&p.CacheReadPerMTok,
			&p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning clickhouse pricing: %w", err)
		}
		byPattern[p.ModelPattern] = len(out)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating clickhouse pricing: %w", err)
	}

	bandRows, err := conn.QueryContext(ctx, `
		SELECT model_pattern, above_input_tokens, input_microdollars_per_mtok,
			output_microdollars_per_mtok, cache_creation_microdollars_per_mtok,
			cache_creation_1h_microdollars_per_mtok,
			cache_read_microdollars_per_mtok, updated_at
		FROM model_pricing_bands
		ORDER BY model_pattern, above_input_tokens`)
	if err != nil {
		return nil, fmt.Errorf("listing clickhouse pricing bands: %w", err)
	}
	defer bandRows.Close()
	for bandRows.Next() {
		var pattern, updatedAt string
		var threshold int64
		var input, output, cacheCreation, cacheCreation1h, cacheRead money.Money
		if err := bandRows.Scan(
			&pattern, &threshold, &input, &output,
			&cacheCreation, &cacheCreation1h, &cacheRead, &updatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning clickhouse pricing band: %w", err)
		}
		i, ok := byPattern[pattern]
		if !ok {
			continue
		}
		aboveInputTokens, err := safecast.Convert[int](threshold)
		if err != nil {
			return nil, fmt.Errorf(
				"converting clickhouse pricing threshold for %q: %w",
				pattern, err,
			)
		}
		out[i].Bands = append(out[i].Bands, db.PricingBand{
			AboveInputTokens:       aboveInputTokens,
			InputPerMTok:           input,
			OutputPerMTok:          output,
			CacheCreationPerMTok:   cacheCreation,
			CacheCreation1hPerMTok: cacheCreation1h,
			CacheReadPerMTok:       cacheRead,
			UpdatedAt:              updatedAt,
		})
	}
	if err := bandRows.Err(); err != nil {
		return nil, fmt.Errorf("iterating clickhouse pricing bands: %w", err)
	}
	return out, nil
}

// syncCursorUsageEvents appends the cursor admin usage rows the mirror has
// not consumed yet, tracked by a high-water id in mirror sync_metadata.
// Project-filtered pushes skip these rows: they are global and unattributed.
func (s *Sync) syncCursorUsageEvents(ctx context.Context) error {
	if s.isFiltered() {
		return nil
	}

	key := s.archiveKey(cursorUsageMaxIDKeyBase)
	stored, err := readMetadata(ctx, s.conn, key)
	if err != nil {
		return err
	}
	var sinceID int64
	if v := stored[key]; v != "" {
		sinceID, err = strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("parsing cursor usage high-water id %q: %w", v, err)
		}
	}

	events, err := s.local.GetCursorUsageEvents(ctx, sinceID)
	if err != nil {
		return fmt.Errorf("loading local cursor usage events: %w", err)
	}
	if len(events) == 0 {
		return nil
	}

	version := newPushVersion()
	rows := make([][]any, 0, len(events))
	maxID := sinceID
	for _, ev := range events {
		occurredAt, ok := parseTimestamp(ev.OccurredAt)
		if !ok {
			return fmt.Errorf("parsing cursor usage occurred_at %q", ev.OccurredAt)
		}
		ts := occurredAt
		rows = append(rows, []any{
			ev.ID,
			&ts,
			db.SanitizeUTF8(ev.Model),
			db.SanitizeUTF8(ev.Kind),
			int64(ev.InputTokens),
			int64(ev.OutputTokens),
			int64(ev.CacheWriteTokens),
			int64(ev.CacheReadTokens),
			ev.Charged.Microdollars,
			ev.CursorTokenFee.Microdollars,
			db.SanitizeUTF8(ev.UserID),
			db.SanitizeUTF8(ev.UserEmail),
			ev.IsHeadless,
			db.SanitizeUTF8(ev.DedupKey),
			version,
		})
		if ev.ID > maxID {
			maxID = ev.ID
		}
	}
	if err := insertRows(ctx, s.conn, "cursor_usage_events", rows); err != nil {
		return err
	}
	return writeMetadata(ctx, s.conn, map[string]string{
		key: strconv.FormatInt(maxID, 10),
	})
}
