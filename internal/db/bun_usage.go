package db

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ccoveille/go-safecast/v2"
	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db/bunmodel"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

const bunPricingWriteBatchSize = 500

const bunCursorUsageWriteBatchSize = 50

// CanonicalModelPricingRows converts public pricing records and their bands
// into the common persistence models used by every adapter.
func CanonicalModelPricingRows(
	prices []ModelPricing,
) ([]bunmodel.ModelPricing, []bunmodel.ModelPricingBand, error) {
	rows := make([]bunmodel.ModelPricing, 0, len(prices))
	bands := make([]bunmodel.ModelPricingBand, 0)
	for _, price := range prices {
		pattern := SanitizeUTF8(price.ModelPattern)
		updatedAtText := SanitizeUTF8(price.UpdatedAt)
		var updatedAt bunmodel.Timestamp
		if updatedAtText != "" {
			var err error
			updatedAt, err = requiredTimestampToBunRow(updatedAtText)
			if err != nil {
				return nil, nil, fmt.Errorf(
					"converting model pricing timestamp for %q: %w", pattern, err,
				)
			}
		}
		rows = append(rows, bunmodel.ModelPricing{
			ModelPattern:                     pattern,
			InputMicrodollarsPerMTok:         price.InputPerMTok.Microdollars,
			OutputMicrodollarsPerMTok:        price.OutputPerMTok.Microdollars,
			CacheCreationMicrodollarsPerMTok: price.CacheCreationPerMTok.Microdollars,
			CacheReadMicrodollarsPerMTok:     price.CacheReadPerMTok.Microdollars,
			UpdatedAt:                        updatedAt,
		})
		for _, band := range price.Bands {
			threshold, err := safecast.Convert[int64](band.AboveInputTokens)
			if err != nil {
				return nil, nil, fmt.Errorf(
					"converting pricing band threshold for %q: %w", pattern, err,
				)
			}
			bandUpdatedAtText := SanitizeUTF8(band.UpdatedAt)
			if bandUpdatedAtText == "" {
				bandUpdatedAtText = updatedAtText
			}
			var bandUpdatedAt bunmodel.Timestamp
			if bandUpdatedAtText != "" {
				var err error
				bandUpdatedAt, err = requiredTimestampToBunRow(bandUpdatedAtText)
				if err != nil {
					return nil, nil, fmt.Errorf(
						"converting model pricing band timestamp for %q: %w",
						pattern, err,
					)
				}
			}
			bands = append(bands, bunmodel.ModelPricingBand{
				ModelPattern: pattern, AboveInputTokens: threshold,
				InputMicrodollarsPerMTok:         band.InputPerMTok.Microdollars,
				OutputMicrodollarsPerMTok:        band.OutputPerMTok.Microdollars,
				CacheCreationMicrodollarsPerMTok: band.CacheCreationPerMTok.Microdollars,
				CacheReadMicrodollarsPerMTok:     band.CacheReadPerMTok.Microdollars,
				UpdatedAt:                        bandUpdatedAt,
			})
		}
	}
	return rows, bands, nil
}

// UpsertModelPricing writes public pricing records through the canonical Bun
// models and the backend's guarded archive-write handle.
func (s *BunStore) UpsertModelPricing(prices []ModelPricing) error {
	rows, bands, err := CanonicalModelPricingRows(prices)
	if err != nil {
		return err
	}
	ctx := context.Background()
	return s.update(ctx, WriteArchive, func(store bun.IDB) error {
		return UpsertModelPricingRows(ctx, store, rows, bands)
	})
}

// CanonicalCursorUsageEventRows converts append-only Cursor usage into
// portable rows. Source IDs are deliberately omitted because every target
// assigns storage IDs independently and deduplicates by the stable event key.
func CanonicalCursorUsageEventRows(
	events []CursorUsageEvent,
) ([]bunmodel.CursorUsageEvent, error) {
	rows := make([]bunmodel.CursorUsageEvent, 0, len(events))
	for _, event := range events {
		occurredAt, err := requiredTimestampToBunRow(event.OccurredAt)
		if err != nil {
			return nil, fmt.Errorf("cursor usage event occurred_at: %w", err)
		}
		truncateCanonicalTimestamp(&occurredAt)
		event.Model = SanitizeUTF8(event.Model)
		event.Kind = SanitizeUTF8(event.Kind)
		event.UserID = SanitizeUTF8(event.UserID)
		event.UserEmail = SanitizeUTF8(event.UserEmail)
		dedupKey := SanitizeUTF8(event.DedupKey)
		if dedupKey == "" {
			event.DedupKey = ""
			dedupKey = CursorUsageEventDedupKey(event)
		}
		if dedupKey == "" {
			return nil, fmt.Errorf("cursor usage event dedup key is required")
		}
		rows = append(rows, bunmodel.CursorUsageEvent{
			OccurredAt: occurredAt, Model: event.Model,
			Kind: event.Kind, InputTokens: event.InputTokens,
			OutputTokens: event.OutputTokens, CacheWriteTokens: event.CacheWriteTokens,
			CacheReadTokens:            event.CacheReadTokens,
			ChargedMicrodollars:        event.Charged.Microdollars,
			CursorTokenFeeMicrodollars: event.CursorTokenFee.Microdollars,
			UserID:                     event.UserID, UserEmail: event.UserEmail,
			IsHeadless: event.IsHeadless, DedupKey: SanitizeUTF8(dedupKey),
		})
	}
	return rows, nil
}

// AppendCursorUsageEventRows appends canonical Cursor usage and ignores rows
// whose stable deduplication key is already present.
func AppendCursorUsageEventRows(
	ctx context.Context, store bun.IDB, rows []bunmodel.CursorUsageEvent,
) error {
	for start := 0; start < len(rows); start += bunCursorUsageWriteBatchSize {
		end := min(start+bunCursorUsageWriteBatchSize, len(rows))
		batch := rows[start:end]
		if _, err := store.NewInsert().Model(&batch).ExcludeColumn("id").
			On("CONFLICT DO NOTHING").Returning("").Exec(ctx); err != nil {
			return fmt.Errorf("appending cursor usage rows: %w", err)
		}
	}
	return nil
}

// UpsertModelPricingRows writes canonical base prices and replaces the bands
// for exactly those model patterns in one transaction.
func UpsertModelPricingRows(
	ctx context.Context,
	store bun.IDB,
	prices []bunmodel.ModelPricing,
	bands []bunmodel.ModelPricingBand,
) error {
	patterns := make([]string, 0, len(prices))
	allowed := make(map[string]struct{}, len(prices))
	for _, price := range prices {
		if price.ModelPattern == "" {
			return fmt.Errorf("upserting model pricing rows: empty model pattern")
		}
		if _, exists := allowed[price.ModelPattern]; exists {
			return fmt.Errorf(
				"upserting model pricing rows: duplicate model pattern %q",
				price.ModelPattern,
			)
		}
		allowed[price.ModelPattern] = struct{}{}
		patterns = append(patterns, price.ModelPattern)
	}
	for _, band := range bands {
		if _, ok := allowed[band.ModelPattern]; !ok {
			return fmt.Errorf(
				"upserting model pricing rows: band model %q has no base row",
				band.ModelPattern,
			)
		}
	}
	if len(prices) == 0 {
		return nil
	}

	return store.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		prices = append([]bunmodel.ModelPricing(nil), prices...)
		bands = append([]bunmodel.ModelPricingBand(nil), bands...)
		var existingPrices []bunmodel.ModelPricing
		if err := tx.NewSelect().Model(&existingPrices).
			Where("model_pattern IN (?)", bun.List(patterns)).Scan(ctx); err != nil {
			return fmt.Errorf("reading model pricing revisions: %w", err)
		}
		existingPriceByPattern := make(
			map[string]bunmodel.ModelPricing, len(existingPrices),
		)
		for _, existing := range existingPrices {
			existingPriceByPattern[existing.ModelPattern] = existing
		}
		defaultRevision := bunmodel.NewTimestamp(time.Now())
		var existingBands []bunmodel.ModelPricingBand
		if len(patterns) > 0 {
			if err := tx.NewSelect().Model(&existingBands).
				Where("model_pattern IN (?)", bun.List(patterns)).Scan(ctx); err != nil {
				return fmt.Errorf("reading model pricing band revisions: %w", err)
			}
		}
		type bandKey struct {
			pattern   string
			threshold int64
		}
		existingBandByKey := make(
			map[bandKey]bunmodel.ModelPricingBand, len(existingBands),
		)
		incomingBandKeys := make(map[bandKey]struct{}, len(bands))
		bandContentChanged := make(map[string]bool, len(patterns))
		for _, existing := range existingBands {
			existingBandByKey[bandKey{
				existing.ModelPattern, existing.AboveInputTokens,
			}] = existing
		}
		for i := range bands {
			key := bandKey{bands[i].ModelPattern, bands[i].AboveInputTokens}
			incomingBandKeys[key] = struct{}{}
			existing, ok := existingBandByKey[key]
			if ok && modelPricingBandValuesEqual(existing, bands[i]) {
				bands[i].UpdatedAt = existing.UpdatedAt
				continue
			}
			bandContentChanged[bands[i].ModelPattern] = true
			bands[i].UpdatedAt = nextPricingRevision(
				existing.UpdatedAt, bands[i].UpdatedAt, defaultRevision,
			)
		}
		for key := range existingBandByKey {
			if _, ok := incomingBandKeys[key]; !ok {
				bandContentChanged[key.pattern] = true
			}
		}
		for i := range prices {
			existing, ok := existingPriceByPattern[prices[i].ModelPattern]
			if ok && modelPricingValuesEqual(existing, prices[i]) &&
				!bandContentChanged[prices[i].ModelPattern] {
				prices[i].UpdatedAt = existing.UpdatedAt
				continue
			}
			prices[i].UpdatedAt = nextPricingRevision(
				existing.UpdatedAt,
				prices[i].UpdatedAt, defaultRevision,
			)
		}
		for start := 0; start < len(prices); start += bunPricingWriteBatchSize {
			end := min(start+bunPricingWriteBatchSize, len(prices))
			if err := upsertModelPricingRowBatch(ctx, tx, prices[start:end]); err != nil {
				return fmt.Errorf("upserting model pricing rows: %w", err)
			}
		}
		return ReplaceModelPricingBandRows(ctx, tx, patterns, bands)
	})
}

func upsertModelPricingRowBatch(
	ctx context.Context, store bun.IDB, rows []bunmodel.ModelPricing,
) error {
	var query strings.Builder
	query.WriteString(`INSERT INTO model_pricing (
		model_pattern, input_microdollars_per_mtok,
		output_microdollars_per_mtok,
		cache_creation_microdollars_per_mtok,
		cache_read_microdollars_per_mtok, updated_at
	) VALUES `)
	args := make([]any, 0, len(rows)*6)
	for i, row := range rows {
		if i > 0 {
			query.WriteString(", ")
		}
		query.WriteString("(?, ?, ?, ?, ?, ?)")
		args = append(args,
			row.ModelPattern,
			row.InputMicrodollarsPerMTok,
			row.OutputMicrodollarsPerMTok,
			row.CacheCreationMicrodollarsPerMTok,
			row.CacheReadMicrodollarsPerMTok,
			row.UpdatedAt,
		)
	}
	query.WriteString(` ON CONFLICT (model_pattern) DO UPDATE SET
		input_microdollars_per_mtok = EXCLUDED.input_microdollars_per_mtok,
		output_microdollars_per_mtok = EXCLUDED.output_microdollars_per_mtok,
		cache_creation_microdollars_per_mtok = EXCLUDED.cache_creation_microdollars_per_mtok,
		cache_read_microdollars_per_mtok = EXCLUDED.cache_read_microdollars_per_mtok,
		updated_at = EXCLUDED.updated_at`)
	_, err := store.NewRaw(query.String(), args...).Exec(ctx)
	return err
}

func modelPricingValuesEqual(
	left, right bunmodel.ModelPricing,
) bool {
	return left.InputMicrodollarsPerMTok == right.InputMicrodollarsPerMTok &&
		left.OutputMicrodollarsPerMTok == right.OutputMicrodollarsPerMTok &&
		left.CacheCreationMicrodollarsPerMTok ==
			right.CacheCreationMicrodollarsPerMTok &&
		left.CacheReadMicrodollarsPerMTok == right.CacheReadMicrodollarsPerMTok
}

func modelPricingBandValuesEqual(
	left, right bunmodel.ModelPricingBand,
) bool {
	return left.InputMicrodollarsPerMTok == right.InputMicrodollarsPerMTok &&
		left.OutputMicrodollarsPerMTok == right.OutputMicrodollarsPerMTok &&
		left.CacheCreationMicrodollarsPerMTok ==
			right.CacheCreationMicrodollarsPerMTok &&
		left.CacheReadMicrodollarsPerMTok == right.CacheReadMicrodollarsPerMTok
}

func nextPricingRevision(
	existing, proposed, fallback bunmodel.Timestamp,
) bunmodel.Timestamp {
	if proposed.IsZero() {
		proposed = fallback
	}
	if existing.IsZero() {
		return proposed
	}
	if proposed.After(existing.Time) {
		return proposed
	}
	return bunmodel.NewTimestamp(existing.Add(time.Microsecond))
}

// ReplaceModelPricingBandRows replaces bands for modelPatterns on the supplied
// transaction-scoped store. Callers that also write base prices must keep both
// helpers in the same transaction; UpsertModelPricingRows enforces that path.
func ReplaceModelPricingBandRows(
	ctx context.Context,
	store bun.IDB,
	modelPatterns []string,
	bands []bunmodel.ModelPricingBand,
) error {
	if len(modelPatterns) == 0 {
		if len(bands) > 0 {
			return fmt.Errorf("replacing model pricing bands: no model patterns")
		}
		return nil
	}
	allowed := make(map[string]struct{}, len(modelPatterns))
	for _, pattern := range modelPatterns {
		allowed[pattern] = struct{}{}
	}
	for _, band := range bands {
		if _, ok := allowed[band.ModelPattern]; !ok {
			return fmt.Errorf(
				"replacing model pricing bands: band model %q is outside replacement set",
				band.ModelPattern,
			)
		}
	}
	if _, err := store.NewDelete().Model((*bunmodel.ModelPricingBand)(nil)).
		Where("model_pattern IN (?)", bun.List(modelPatterns)).Exec(ctx); err != nil {
		return fmt.Errorf("deleting model pricing bands: %w", err)
	}
	for start := 0; start < len(bands); start += bunPricingWriteBatchSize {
		end := min(start+bunPricingWriteBatchSize, len(bands))
		if err := insertModelPricingBandRowBatch(ctx, store, bands[start:end]); err != nil {
			return fmt.Errorf("inserting model pricing bands: %w", err)
		}
	}
	return nil
}

func insertModelPricingBandRowBatch(
	ctx context.Context, store bun.IDB, rows []bunmodel.ModelPricingBand,
) error {
	var query strings.Builder
	query.WriteString(`INSERT INTO model_pricing_bands (
		model_pattern, above_input_tokens,
		input_microdollars_per_mtok, output_microdollars_per_mtok,
		cache_creation_microdollars_per_mtok,
		cache_read_microdollars_per_mtok, updated_at
	) VALUES `)
	args := make([]any, 0, len(rows)*7)
	for i, row := range rows {
		if i > 0 {
			query.WriteString(", ")
		}
		query.WriteString("(?, ?, ?, ?, ?, ?, ?)")
		args = append(args,
			row.ModelPattern,
			row.AboveInputTokens,
			row.InputMicrodollarsPerMTok,
			row.OutputMicrodollarsPerMTok,
			row.CacheCreationMicrodollarsPerMTok,
			row.CacheReadMicrodollarsPerMTok,
			row.UpdatedAt,
		)
	}
	_, err := store.NewRaw(query.String(), args...).Exec(ctx)
	return err
}

// LoadPricingMap returns the effective stored and in-memory pricing catalogue.
// It is exported for adapter-owned consumers that have not yet moved into the
// common store; usage methods call the unexported alias below.
func (s *BunStore) LoadPricingMap(
	ctx context.Context,
) ([]export.EffectivePricingRow, error) {
	var rows []export.EffectivePricingRow
	err := s.consistentView(ctx, func(store bun.IDB) error {
		var err error
		rows, err = s.loadPricingMapFrom(ctx, store)
		return err
	})
	return rows, err
}

func (s *BunStore) loadPricingMapFrom(
	ctx context.Context, store bun.IDB,
) ([]export.EffectivePricingRow, error) {
	pricingTable, err := s.bunTableExists(ctx, store, "model_pricing")
	if err != nil {
		return nil, err
	}
	var prices []ModelPricing
	if pricingTable {
		bandsTable, probeErr := s.bunTableExists(ctx, store, "model_pricing_bands")
		if probeErr != nil {
			return nil, probeErr
		}
		prices, err = listBunModelPricing(ctx, store, bandsTable)
		if err != nil {
			return nil, err
		}
	}

	s.pricingMu.RLock()
	custom := maps.Clone(s.pricing.custom)
	effective := cloneModelRates(s.pricing.effective)
	emptyCatalog := cloneModelRates(s.pricing.emptyCatalog)
	s.pricingMu.RUnlock()

	fallback := fallbackRateMap()
	out := make(map[string]export.ModelRates)
	for _, price := range prices {
		rates := modelPricingRates(price)
		rates.Source = modelPricingSource(price, fallback)
		out[price.ModelPattern] = rates
	}
	if len(out) == 0 {
		maps.Copy(out, emptyCatalog)
	}
	for model, rate := range custom {
		out[model] = export.ModelRates{
			InputPerMTok: money.Money{
				Microdollars: rate.InputMicrodollarsPerMTok,
			},
			OutputPerMTok: money.Money{
				Microdollars: rate.OutputMicrodollarsPerMTok,
			},
			CacheWritePerMTok: money.Money{
				Microdollars: rate.CacheCreationMicrodollarsPerMTok,
			},
			CacheReadPerMTok: money.Money{
				Microdollars: rate.CacheReadMicrodollarsPerMTok,
			},
			Source: customPricingSource(),
		}
	}
	maps.Copy(out, effective)
	return pricingMapRows(out), nil
}

func (s *BunStore) bunTableExists(
	ctx context.Context, store bun.IDB, table string,
) (bool, error) {
	probe, ok := s.backend.(BunTablePresenceProbe)
	if !ok {
		return true, nil
	}
	exists, err := probe.BunTableExists(ctx, store, table)
	if err != nil {
		return false, fmt.Errorf("probing optional table %s: %w", table, err)
	}
	return exists, nil
}

func listBunModelPricing(
	ctx context.Context, store bun.IDB, includeBands bool,
) ([]ModelPricing, error) {
	var rows []bunmodel.ModelPricing
	if err := store.NewSelect().Model(&rows).
		OrderExpr("model_pattern ASC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing model pricing: %w", err)
	}
	var bandRows []bunmodel.ModelPricingBand
	if includeBands {
		if err := store.NewSelect().Model(&bandRows).
			OrderExpr("model_pattern ASC, above_input_tokens ASC").Scan(ctx); err != nil {
			return nil, fmt.Errorf("listing model pricing bands: %w", err)
		}
	}
	bands := make(map[string][]PricingBand)
	for _, row := range bandRows {
		threshold, err := safecast.Convert[int](row.AboveInputTokens)
		if err != nil {
			return nil, fmt.Errorf(
				"converting model pricing threshold for %q: %w",
				row.ModelPattern, err,
			)
		}
		bands[row.ModelPattern] = append(bands[row.ModelPattern], PricingBand{
			AboveInputTokens: threshold,
			InputPerMTok: money.Money{
				Microdollars: row.InputMicrodollarsPerMTok,
			},
			OutputPerMTok: money.Money{
				Microdollars: row.OutputMicrodollarsPerMTok,
			},
			CacheCreationPerMTok: money.Money{
				Microdollars: row.CacheCreationMicrodollarsPerMTok,
			},
			CacheReadPerMTok: money.Money{
				Microdollars: row.CacheReadMicrodollarsPerMTok,
			},
			UpdatedAt: formatRequiredUsageTimestamp(row.UpdatedAt),
		})
	}
	out := make([]ModelPricing, len(rows))
	for index, row := range rows {
		out[index] = ModelPricing{
			ModelPattern: row.ModelPattern,
			InputPerMTok: money.Money{
				Microdollars: row.InputMicrodollarsPerMTok,
			},
			OutputPerMTok: money.Money{
				Microdollars: row.OutputMicrodollarsPerMTok,
			},
			CacheCreationPerMTok: money.Money{
				Microdollars: row.CacheCreationMicrodollarsPerMTok,
			},
			CacheReadPerMTok: money.Money{
				Microdollars: row.CacheReadMicrodollarsPerMTok,
			},
			UpdatedAt: formatRequiredUsageTimestamp(row.UpdatedAt),
			Bands:     bands[row.ModelPattern],
		}
	}
	return out, nil
}

type bunUsageProjection struct {
	ID                       int64               `bun:"id"`
	SessionID                string              `bun:"session_id"`
	MessageOrdinal           *int                `bun:"message_ordinal"`
	UsageTimestamp           *bunmodel.Timestamp `bun:"usage_timestamp"`
	Model                    string              `bun:"model"`
	TokenJSON                string              `bun:"token_json"`
	InputTokens              int                 `bun:"input_tokens"`
	OutputTokens             int                 `bun:"output_tokens"`
	CacheCreationInputTokens int                 `bun:"cache_creation_input_tokens"`
	CacheReadInputTokens     int                 `bun:"cache_read_input_tokens"`
	ReasoningTokens          int                 `bun:"reasoning_tokens"`
	CostMicrodollars         *int64              `bun:"cost_microdollars"`
	CostStatus               string              `bun:"cost_status"`
	CostSource               string              `bun:"cost_source"`
	UsageSource              string              `bun:"usage_source"`
	DedupKey                 string              `bun:"dedup_key"`
	ClaudeMessageID          string              `bun:"claude_message_id"`
	ClaudeRequestID          string              `bun:"claude_request_id"`
	SourceUUID               string              `bun:"source_uuid"`
	UsageDedupKey            string              `bun:"usage_dedup_key"`
	Project                  string              `bun:"project"`
	Agent                    string              `bun:"agent"`
	Machine                  string              `bun:"machine"`
	GitBranch                string              `bun:"git_branch"`
	UserMessageCount         int                 `bun:"user_message_count"`
	IsAutomated              bool                `bun:"is_automated"`
	SessionStartedAt         *bunmodel.Timestamp `bun:"session_started_at"`
	SessionEndedAt           *bunmodel.Timestamp `bun:"session_ended_at"`
	SessionCreatedAt         bunmodel.Timestamp  `bun:"session_created_at"`
	TerminationStatus        *string             `bun:"termination_status"`
	DisplayName              *string             `bun:"display_name"`
	SessionName              *string             `bun:"session_name"`
	FirstMessage             *string             `bun:"first_message"`
}

type bunDailyUsageProjection struct {
	ID                       int64               `bun:"id"`
	SessionID                string              `bun:"session_id"`
	MessageOrdinal           sql.NullInt64       `bun:"message_ordinal"`
	UsageTimestamp           bunmodel.Timestamp  `bun:"usage_timestamp"`
	Model                    string              `bun:"model"`
	TokenJSON                string              `bun:"token_json"`
	InputTokens              int                 `bun:"input_tokens"`
	OutputTokens             int                 `bun:"output_tokens"`
	CacheCreationInputTokens int                 `bun:"cache_creation_input_tokens"`
	CacheReadInputTokens     int                 `bun:"cache_read_input_tokens"`
	ReasoningTokens          int                 `bun:"reasoning_tokens"`
	CostMicrodollars         sql.NullInt64       `bun:"cost_microdollars"`
	CostSource               string              `bun:"cost_source"`
	UsageSource              string              `bun:"usage_source"`
	DedupKey                 string              `bun:"dedup_key"`
	UsageDedupKey            string              `bun:"-"`
	ClaudeMessageID          string              `bun:"claude_message_id"`
	ClaudeRequestID          string              `bun:"claude_request_id"`
	SourceUUID               string              `bun:"source_uuid"`
	Project                  string              `bun:"project"`
	Agent                    string              `bun:"agent"`
	Machine                  string              `bun:"machine"`
	GitBranch                string              `bun:"git_branch"`
	UserMessageCount         int                 `bun:"user_message_count"`
	IsAutomated              bool                `bun:"is_automated"`
	SessionStartedAt         bunmodel.Timestamp  `bun:"session_started_at"`
	SessionEndedAt           *bunmodel.Timestamp `bun:"session_ended_at"`
	SessionCreatedAt         bunmodel.Timestamp  `bun:"session_created_at"`
	TerminationStatus        *string             `bun:"termination_status"`
}

type bunCursorUsageProjection struct {
	OccurredAt          bunmodel.Timestamp `bun:"occurred_at"`
	Model               string             `bun:"model"`
	InputTokens         int                `bun:"input_tokens"`
	OutputTokens        int                `bun:"output_tokens"`
	CacheWriteTokens    int                `bun:"cache_write_tokens"`
	CacheReadTokens     int                `bun:"cache_read_tokens"`
	ChargedMicrodollars int64              `bun:"charged_microdollars"`
	IsHeadless          bool               `bun:"is_headless"`
	DedupKey            string             `bun:"dedup_key"`
}

const bunUsageSessionColumns = `
	s.project AS project,
	s.agent AS agent,
	s.machine AS machine,
	s.git_branch AS git_branch,
	s.user_message_count AS user_message_count,
	s.is_automated AS is_automated,
	s.started_at AS session_started_at,
	s.ended_at AS session_ended_at,
	s.created_at AS session_created_at,
	s.termination_status AS termination_status,
	s.display_name AS display_name,
	s.session_name AS session_name,
	s.first_message AS first_message`

const bunDailyUsageSessionColumns = `
	s.project AS project,
	s.agent AS agent,
	s.machine AS machine,
	s.git_branch AS git_branch,
	s.user_message_count AS user_message_count,
	s.is_automated AS is_automated,
	s.started_at AS session_started_at,
	s.ended_at AS session_ended_at,
	s.created_at AS session_created_at,
	s.termination_status AS termination_status`

func bunMessageUsageColumns(timestampOrder func(string) string) string {
	return `
	m.session_id AS session_id,
	m.ordinal AS message_ordinal,
	` + bunUsageTimestampColumn(timestampOrder, "m.timestamp") + ` AS usage_timestamp,
	m.model AS model,
	m.token_usage AS token_json,
	m.claude_message_id AS claude_message_id,
	m.claude_request_id AS claude_request_id,
	m.source_uuid AS source_uuid`
}

func bunEventUsageColumns(timestampOrder func(string) string) string {
	return `
	ue.id AS id,
	ue.session_id AS session_id,
	ue.message_ordinal AS message_ordinal,
	` + bunUsageTimestampColumn(timestampOrder, "ue.occurred_at") + ` AS usage_timestamp,
	ue.model AS model,
	ue.input_tokens AS input_tokens,
	ue.output_tokens AS output_tokens,
	ue.cache_creation_input_tokens AS cache_creation_input_tokens,
	ue.cache_read_input_tokens AS cache_read_input_tokens,
	ue.reasoning_tokens AS reasoning_tokens,
	ue.cost_microdollars AS cost_microdollars,
	ue.cost_status AS cost_status,
	ue.cost_source AS cost_source,
	ue.source AS usage_source,
	ue.dedup_key AS dedup_key`
}

func bunUsageTimestampColumn(
	timestampOrder func(string) string, column string,
) string {
	return "CASE WHEN " + timestampOrder(bunNullableTimestamp(column)) +
		" IS NULL THEN NULL ELSE " + column + " END"
}

func bunDailyMessageUsageColumns(timestampOrder func(string) string) string {
	return `
	m.session_id AS session_id,
	m.ordinal AS message_ordinal,
	` + bunUsageTimestampColumn(timestampOrder, "m.timestamp") + ` AS usage_timestamp,
	m.model AS model,
	m.token_usage AS token_json,
	m.claude_message_id AS claude_message_id,
	m.claude_request_id AS claude_request_id,
	m.source_uuid AS source_uuid`
}

func bunDailyEventUsageColumns(timestampOrder func(string) string) string {
	return `
	ue.id AS id,
	ue.session_id AS session_id,
	ue.message_ordinal AS message_ordinal,
	` + bunUsageTimestampColumn(timestampOrder, "ue.occurred_at") + ` AS usage_timestamp,
	ue.model AS model,
	ue.input_tokens AS input_tokens,
	ue.output_tokens AS output_tokens,
	ue.cache_creation_input_tokens AS cache_creation_input_tokens,
	ue.cache_read_input_tokens AS cache_read_input_tokens,
	ue.reasoning_tokens AS reasoning_tokens,
	ue.cost_microdollars AS cost_microdollars,
	ue.cost_source AS cost_source,
	ue.source AS usage_source,
	ue.dedup_key AS dedup_key`
}

func (s *BunStore) loadDailyUsageRows(
	ctx context.Context, filter UsageFilter, includeCursor, matching bool,
) ([]dailyUsageScanRow, error) {
	var staged []dailyUsageScanRow
	err := s.consistentView(ctx, func(store bun.IDB) error {
		var err error
		staged, err = s.loadDailyUsageRowsFrom(
			ctx, store, filter, includeCursor, matching,
		)
		return err
	})
	return staged, err
}

func (s *BunStore) loadDailyUsageRowsFrom(
	ctx context.Context, store bun.IDB, filter UsageFilter,
	includeCursor, matching bool,
) ([]dailyUsageScanRow, error) {
	rows, err := s.loadBunSessionUsageRows(ctx, store, filter, matching)
	if err != nil {
		return nil, err
	}
	if includeCursor {
		cursor, err := s.loadBunCursorUsageRows(ctx, store, filter)
		if err != nil {
			return nil, err
		}
		rows = append(rows, cursor...)
	}
	sortDailyUsageRows(rows)
	return rows, nil
}

func (s *BunStore) loadSessionUsageRowsFrom(
	ctx context.Context, store bun.IDB, filter UsageFilter, sessionID string,
) ([]usageScanRow, error) {
	rows, err := s.loadBunUsageProjections(
		ctx, store, filter, false, []string{sessionID},
	)
	if err != nil {
		return nil, err
	}
	out := make([]usageScanRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, usageProjectionToFullRow(row))
	}
	sortUsageRows(out)
	return out, nil
}

func (s *BunStore) loadBunSessionUsageRows(
	ctx context.Context, store bun.IDB, filter UsageFilter, matching bool,
) ([]dailyUsageScanRow, error) {
	queryFilter := filter
	if !matching {
		// Claude streams can emit the same request through a parent and a
		// delegated session. Load the complete bounded candidate set before
		// applying model or session filters so the fullest snapshot wins and
		// is then attributed to its earliest session.
		queryFilter = usageSnapshotInputFilter(filter)
	}
	projections, err := s.loadBunDailyUsageProjections(
		ctx, store, queryFilter, matching,
	)
	if err != nil {
		return nil, err
	}
	if !matching {
		return normalizeBunDailyUsageProjections(projections, filter), nil
	}
	rows := make([]dailyUsageScanRow, 0, len(projections))
	for _, row := range projections {
		rows = append(rows, dailyUsageProjectionToRow(row))
	}
	return rows, nil
}

func (s *BunStore) loadBunDailyUsageProjections(
	ctx context.Context, store bun.IDB, filter UsageFilter, matching bool,
) ([]bunDailyUsageProjection, error) {
	messageQuery, eventQuery := s.bunDailyUsageQueries(store, filter, matching)
	var messages []bunDailyUsageProjection
	if err := messageQuery.Scan(ctx, &messages); err != nil {
		return nil, fmt.Errorf("querying daily usage messages: %w", err)
	}

	var events []bunDailyUsageProjection
	if err := eventQuery.Scan(ctx, &events); err != nil {
		return nil, fmt.Errorf("querying daily usage events: %w", err)
	}

	rows := make([]bunDailyUsageProjection, 0, len(messages)+len(events))
	for _, row := range messages {
		row.UsageSource = "message"
		rows = append(rows, row)
	}
	for _, row := range events {
		row.UsageDedupKey = dailyUsageEventProjectionDedupKey(row)
		rows = append(rows, row)
	}
	return rows, nil
}

func normalizeBunDailyUsageProjections(
	projections []bunDailyUsageProjection, filter UsageFilter,
) []dailyUsageScanRow {
	loc := filter.location()
	bounded := usageBoundsForFilter(filter).bounded()
	eligible := make([]bunDailyUsageProjection, 0, len(projections))
	for _, row := range projections {
		daily := dailyUsageProjectionToRow(row)
		if bounded {
			date := dailyUsageLocalDate(daily, loc)
			if date == "" || filter.From != "" && date < filter.From ||
				filter.To != "" && date > filter.To {
				continue
			}
		}
		eligible = append(eligible, row)
	}

	snapshotRows := make([]activity.UsageRow, len(eligible))
	metadata := make(map[string]bunDailyUsageProjection, len(eligible))
	for i, row := range eligible {
		daily := dailyUsageProjectionToRow(row)
		metadata[row.SessionID] = row
		_, outputTokens, _, _, _ := dailyUsageRowTokens(daily)
		snapshotRows[i] = activity.UsageRow{
			SessionID: row.SessionID, Timestamp: daily.ts,
			MessageOrdinal: usageRowMessageOrdinal(daily.messageOrdinal),
			OutputTokens:   outputTokens,
			WebSearchRequests: usageRowWebSearchRequests(
				daily.usageSource, daily.tokenJSON),
			ClaudeMessageID: row.ClaudeMessageID,
			ClaudeRequestID: row.ClaudeRequestID,
		}
	}
	mask, attribution, webSearchRequests :=
		activity.ClaudeSnapshotSurvivorSelection(snapshotRows)
	referenceTime := time.Now().UTC()
	rows := make([]dailyUsageScanRow, 0, len(eligible))
	for i, row := range eligible {
		if !mask[i] {
			continue
		}
		if attributed, ok := metadata[attribution[i]]; ok {
			row = bunDailyUsageProjectionWithSessionMetadata(row, attributed)
		}
		if !usageSourceMatches(row.Model, filter) ||
			!bunDailyUsageSessionMatches(row, filter, referenceTime) {
			continue
		}
		daily := dailyUsageProjectionToRow(row)
		daily.webSearchRequests = sql.NullInt64{
			Int64: int64(webSearchRequests[i]), Valid: true,
		}
		rows = append(rows, daily)
	}
	sortDailyUsageRows(rows)
	return rows
}

func bunDailyUsageProjectionWithSessionMetadata(
	row, attributed bunDailyUsageProjection,
) bunDailyUsageProjection {
	row.SessionID = attributed.SessionID
	row.Project = attributed.Project
	row.Agent = attributed.Agent
	row.Machine = attributed.Machine
	row.GitBranch = attributed.GitBranch
	row.UserMessageCount = attributed.UserMessageCount
	row.IsAutomated = attributed.IsAutomated
	row.SessionStartedAt = attributed.SessionStartedAt
	row.SessionEndedAt = attributed.SessionEndedAt
	row.SessionCreatedAt = attributed.SessionCreatedAt
	row.TerminationStatus = attributed.TerminationStatus
	return row
}

func bunDailyUsageSessionMatches(
	row bunDailyUsageProjection, filter UsageFilter, referenceTime time.Time,
) bool {
	startedAt := row.SessionStartedAt
	return usageSessionMatches(bunUsageProjection{
		Project: row.Project, Agent: row.Agent, Machine: row.Machine,
		GitBranch: row.GitBranch, UserMessageCount: row.UserMessageCount,
		IsAutomated: row.IsAutomated, SessionStartedAt: &startedAt,
		SessionEndedAt:    row.SessionEndedAt,
		SessionCreatedAt:  row.SessionCreatedAt,
		TerminationStatus: row.TerminationStatus,
	}, filter, referenceTime)
}

func (s *BunStore) bunDailyUsageQueries(
	store bun.IDB, filter UsageFilter, matching bool,
) (*bun.SelectQuery, *bun.SelectQuery) {
	referenceTime := time.Now().UTC()
	timestampOrder := s.backend.TimestampOrderExpr
	messageQuery := store.NewSelect().TableExpr("messages AS m").
		ColumnExpr(bunDailyMessageUsageColumns(timestampOrder) + "," +
			bunDailyUsageSessionColumns).
		Join("JOIN sessions AS s ON s.id = m.session_id").
		Where("s.deleted_at IS NULL")
	if matching {
		messageQuery = messageQuery.Where("m.role = ?", "assistant").
			Where("m.model != ?", "<synthetic>")
	} else {
		messageQuery = messageQuery.Where("m.token_usage != ?", "").
			Where("m.model != ?", "").Where("m.model != ?", "<synthetic>")
	}
	messageQuery = appendBunUsageFilters(
		messageQuery, filter, "m.model", s.backend.TimestampOrderExpr, referenceTime,
	)
	messageQuery = appendBunUsageBounds(
		messageQuery, filter, "m.timestamp", true, s.backend.TimestampOrderExpr,
	)
	messageTimestamp := "COALESCE(" +
		timestampOrder(bunNullableTimestamp("m.timestamp")) + ", " +
		timestampOrder(bunNullableTimestamp("s.started_at")) + ", " +
		timestampOrder("s.created_at") + ")"
	messageQuery = messageQuery.
		OrderExpr(messageTimestamp + " ASC").
		OrderExpr("m.session_id ASC").
		OrderExpr("m.ordinal ASC")

	eventQuery := store.NewSelect().TableExpr("usage_events AS ue").
		ColumnExpr(bunDailyEventUsageColumns(timestampOrder)+","+
			bunDailyUsageSessionColumns).
		Join("JOIN sessions AS s ON s.id = ue.session_id").
		Where("s.deleted_at IS NULL").Where("ue.model != ?", "")
	eventQuery = appendBunUsageFilters(
		eventQuery, filter, "ue.model", s.backend.TimestampOrderExpr, referenceTime,
	)
	eventQuery = appendBunUsageBounds(
		eventQuery, filter, "ue.occurred_at", true, s.backend.TimestampOrderExpr,
	)
	eventTimestamp := "COALESCE(" +
		timestampOrder(bunNullableTimestamp("ue.occurred_at")) + ", " +
		timestampOrder(bunNullableTimestamp("s.started_at")) + ", " +
		timestampOrder("s.created_at") + ")"
	eventQuery = eventQuery.
		OrderExpr(eventTimestamp + " ASC").
		OrderExpr("ue.session_id ASC").
		OrderExpr("COALESCE(ue.message_ordinal, -1) ASC")
	return messageQuery, eventQuery
}

func (s *BunStore) streamDailyUsageRowsFrom(
	ctx context.Context, store bun.IDB, filter UsageFilter, includeCursor, matching bool,
	consume func(dailyUsageScanRow) error,
) error {
	rows, err := s.loadDailyUsageRowsFrom(
		ctx, store, filter, includeCursor, matching,
	)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := consume(row); err != nil {
			return err
		}
	}
	return nil
}

func (s *BunStore) loadBunUsageProjections(
	ctx context.Context, store bun.IDB, filter UsageFilter,
	matching bool, sessionIDs []string,
) ([]bunUsageProjection, error) {
	referenceTime := time.Now().UTC()
	timestampOrder := s.backend.TimestampOrderExpr
	var messages []bunUsageProjection
	messageQuery := store.NewSelect().TableExpr("messages AS m").
		ColumnExpr(bunMessageUsageColumns(timestampOrder) + "," + bunUsageSessionColumns).
		Join("JOIN sessions AS s ON s.id = m.session_id").
		Where("s.deleted_at IS NULL")
	if matching {
		messageQuery = messageQuery.Where("m.role = ?", "assistant").
			Where("m.model != ?", "<synthetic>")
	} else {
		messageQuery = messageQuery.Where("m.token_usage != ?", "").
			Where("m.model != ?", "").Where("m.model != ?", "<synthetic>")
	}
	if len(sessionIDs) > 0 {
		messageQuery = messageQuery.Where(
			"m.session_id IN (?)", bun.List(sessionIDs),
		)
	}
	messageQuery = appendBunUsageFilters(
		messageQuery, filter, "m.model", s.backend.TimestampOrderExpr, referenceTime,
	)
	messageQuery = appendBunUsageBounds(
		messageQuery, filter, "m.timestamp", true,
		s.backend.TimestampOrderExpr,
	)
	if err := messageQuery.Scan(ctx, &messages); err != nil {
		return nil, fmt.Errorf("querying usage messages: %w", err)
	}
	for index := range messages {
		messages[index].CostStatus = ""
		messages[index].CostSource = ""
	}

	var events []bunUsageProjection
	eventQuery := store.NewSelect().TableExpr("usage_events AS ue").
		ColumnExpr(bunEventUsageColumns(timestampOrder)+","+bunUsageSessionColumns).
		Join("JOIN sessions AS s ON s.id = ue.session_id").
		Where("s.deleted_at IS NULL").Where("ue.model != ?", "")
	if len(sessionIDs) > 0 {
		eventQuery = eventQuery.Where(
			"ue.session_id IN (?)", bun.List(sessionIDs),
		)
	}
	eventQuery = appendBunUsageFilters(
		eventQuery, filter, "ue.model", s.backend.TimestampOrderExpr, referenceTime,
	)
	eventQuery = appendBunUsageBounds(
		eventQuery, filter, "ue.occurred_at", true,
		s.backend.TimestampOrderExpr,
	)
	if err := eventQuery.Scan(ctx, &events); err != nil {
		return nil, fmt.Errorf("querying usage events: %w", err)
	}

	rows := make([]bunUsageProjection, 0, len(messages)+len(events))
	for _, row := range messages {
		if !usageSourceMatches(row.Model, filter) ||
			!usageSessionMatches(row, filter, referenceTime) {
			continue
		}
		row.UsageDedupKey = ""
		rows = append(rows, row)
	}
	for _, row := range events {
		if !usageSourceMatches(row.Model, filter) ||
			!usageSessionMatches(row, filter, referenceTime) {
			continue
		}
		row.UsageDedupKey = usageEventProjectionDedupKey(row)
		rows = append(rows, row)
	}
	return rows, nil
}

func appendBunUsageBounds(
	query *bun.SelectQuery, filter UsageFilter, timestampColumn string,
	withSessionFallback bool, timestampOrder func(string) string,
) *bun.SelectQuery {
	bounds := usageBoundsForFilter(filter)
	expr := timestampOrder(bunNullableTimestamp(timestampColumn))
	if withSessionFallback {
		expr = "COALESCE(" + timestampOrder(bunNullableTimestamp(timestampColumn)) +
			", " + timestampOrder(bunNullableTimestamp("s.started_at")) +
			", " + timestampOrder("s.created_at") + ")"
	}
	parameter := timestampOrder("?")
	if bounds.from != "" {
		query = query.Where(expr+" >= "+parameter, bounds.from)
	}
	if bounds.to != "" {
		query = query.Where(expr+" <= "+parameter, bounds.to)
	}
	return query
}

func appendBunUsageFilters(
	query *bun.SelectQuery, filter UsageFilter, modelColumn string,
	timestampOrder func(string) string, referenceTime time.Time,
) *bun.SelectQuery {
	query = appendBunUsageValues(query, modelColumn, csvUsageValues(filter.Model), true)
	query = appendBunUsageValues(
		query, modelColumn, csvUsageValues(filter.ExcludeModel), false,
	)
	query = appendBunUsageValues(query, "s.agent", csvUsageValues(filter.Agent), true)
	query = appendBunUsageValues(query, "s.project", filter.ProjectFilterLabels(), true)
	query = appendBunUsageValues(query, "s.machine", csvUsageValues(filter.Machine), true)
	query = appendBunUsageValues(
		query, "s.project", filter.ExcludedProjectFilterLabels(), false,
	)
	query = appendBunUsageValues(
		query, "s.agent", csvUsageValues(filter.ExcludeAgent), false,
	)
	if filter.GitBranch != "" {
		clause, args := BranchPairClauseArgs(
			"s.project", "s.git_branch", filter.GitBranch, nil,
		)
		query = query.Where(clause, args...)
	}
	if filter.MinUserMessages > 0 {
		query = query.Where("s.user_message_count >= ?", filter.MinUserMessages)
	}
	scope := normalizeAutomatedScope(filter.AutomatedScope, filter.ExcludeAutomated)
	if filter.ExcludeOneShot {
		if scope == "human" {
			query = query.Where("s.user_message_count > 1")
		} else {
			query = query.Where("(s.user_message_count > 1 OR s.is_automated = ?)", true)
		}
	}
	switch scope {
	case "human":
		query = query.Where("s.is_automated = ?", false)
	case "automated":
		query = query.Where("s.is_automated = ?", true)
	}
	if filter.ActiveSince != "" {
		expr, parameter := bunUsageSessionActivityComparison(timestampOrder)
		query = query.Where(expr+" >= "+parameter, filter.ActiveSince)
	}
	return appendBunUsageTerminationFilter(
		query, filter.Termination, timestampOrder, referenceTime,
	)
}

func csvUsageValues(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, ",")
}

func appendBunUsageValues(
	query *bun.SelectQuery, column string, values []string, include bool,
) *bun.SelectQuery {
	if len(values) == 0 {
		return query
	}
	operator := " IN (?)"
	if !include {
		operator = " NOT IN (?)"
	}
	return query.Where(column+operator, bun.List(values))
}

func bunUsageSessionActivityComparison(
	timestampOrder func(string) string,
) (string, string) {
	expr := "COALESCE(" + bunNullableTimestamp("s.ended_at") + ", " +
		bunNullableTimestamp("s.started_at") + ", s.created_at)"
	return timestampOrder(expr), timestampOrder("?")
}

func appendBunUsageTerminationFilter(
	query *bun.SelectQuery, filter string, timestampOrder func(string) string,
	referenceTime time.Time,
) *bun.SelectQuery {
	return appendBunTerminationFilter(
		query, filter, "s", timestampOrder, referenceTime,
	)
}

func appendBunTerminationFilter(
	query *bun.SelectQuery, filter, alias string,
	timestampOrder func(string) string,
	referenceTime time.Time,
) *bun.SelectQuery {
	if !usageHasTerminationFilter(filter) {
		return query
	}
	activityExpr := "COALESCE(" + bunNullableTimestamp(alias+".ended_at") + ", " +
		bunNullableTimestamp(alias+".started_at") + ", " + alias + ".created_at)"
	activityExpr = timestampOrder(activityExpr)
	parameter := timestampOrder("?")
	activeCutoff := referenceTime.UTC().Add(-activeWindow).Format(time.RFC3339Nano)
	staleCutoff := referenceTime.UTC().Add(-staleWindow).Format(time.RFC3339Nano)
	flagged := alias + ".termination_status IN ('tool_call_pending', 'truncated')"
	var predicates []string
	var args []any
	for part := range strings.SplitSeq(filter, ",") {
		switch strings.TrimSpace(part) {
		case "active":
			predicates = append(predicates, activityExpr+" > "+parameter)
			args = append(args, activeCutoff)
		case "stale":
			predicates = append(predicates, "("+activityExpr+" > "+parameter+
				" AND "+activityExpr+" <= "+parameter+" AND "+flagged+")")
			args = append(args, staleCutoff, activeCutoff)
		case "unclean":
			predicates = append(predicates, "("+activityExpr+" <= "+parameter+
				" AND "+flagged+")")
			args = append(args, staleCutoff)
		case "clean":
			predicates = append(predicates, alias+".termination_status = 'clean'")
		case "awaiting_user":
			predicates = append(predicates, alias+".termination_status = 'awaiting_user'")
		}
	}
	if len(predicates) == 0 {
		return query
	}
	return query.Where("("+strings.Join(predicates, " OR ")+")", args...)
}

func usageProjectionToDailyRow(row bunUsageProjection) dailyUsageScanRow {
	source := "message"
	if row.UsageSource != "" {
		source = row.UsageSource
	}
	return dailyUsageScanRow{
		sessionID: row.SessionID, messageOrdinal: nullableUsageOrdinal(row.MessageOrdinal),
		usageSource: source, ts: usageProjectionTimestamp(row), model: row.Model,
		tokenJSON: row.TokenJSON, inputTokens: row.InputTokens,
		outputTokens:             row.OutputTokens,
		cacheCreationInputTokens: row.CacheCreationInputTokens,
		cacheReadInputTokens:     row.CacheReadInputTokens,
		reasoningTokens:          row.ReasoningTokens,
		cost:                     nullableUsageCost(row.CostMicrodollars), costSource: row.CostSource,
		claudeMessageID: row.ClaudeMessageID, claudeRequestID: row.ClaudeRequestID,
		sourceUUID: row.SourceUUID, usageDedupKey: row.UsageDedupKey,
		project: row.Project, agent: row.Agent, machine: row.Machine,
	}
}

func dailyUsageProjectionToRow(row bunDailyUsageProjection) dailyUsageScanRow {
	return dailyUsageProjectionToRowMode(row, true)
}

func dailyUsageProjectionToRowMode(
	row bunDailyUsageProjection, formatTimestamp bool,
) dailyUsageScanRow {
	usageTime := dailyUsageProjectionTime(row)
	var timestamp string
	if formatTimestamp {
		timestamp = formatRequiredUsageTime(usageTime)
	}
	return dailyUsageScanRow{
		sessionID: row.SessionID, messageOrdinal: row.MessageOrdinal,
		usageSource: row.UsageSource,
		ts:          timestamp, usageTime: usageTime,
		model:     row.Model,
		tokenJSON: row.TokenJSON, inputTokens: row.InputTokens,
		outputTokens:             row.OutputTokens,
		cacheCreationInputTokens: row.CacheCreationInputTokens,
		cacheReadInputTokens:     row.CacheReadInputTokens,
		reasoningTokens:          row.ReasoningTokens,
		cost:                     row.CostMicrodollars,
		costSource:               row.CostSource,
		claudeMessageID:          row.ClaudeMessageID,
		claudeRequestID:          row.ClaudeRequestID,
		sourceUUID:               row.SourceUUID,
		usageDedupKey:            row.UsageDedupKey,
		project:                  row.Project,
		agent:                    row.Agent,
		machine:                  row.Machine,
	}
}

func dailyUsageProjectionTime(row bunDailyUsageProjection) time.Time {
	if !row.UsageTimestamp.IsZero() {
		return row.UsageTimestamp.Time
	}
	if !row.SessionStartedAt.IsZero() {
		return row.SessionStartedAt.Time
	}
	if !row.SessionCreatedAt.IsZero() {
		return row.SessionCreatedAt.Time
	}
	return time.Time{}
}

func formatRequiredUsageTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func usageProjectionToFullRow(row bunUsageProjection) usageScanRow {
	daily := usageProjectionToDailyRow(row)
	return usageScanRow{
		sessionID: daily.sessionID, messageOrdinal: daily.messageOrdinal,
		usageSource: daily.usageSource, ts: daily.ts, model: daily.model,
		tokenJSON: daily.tokenJSON, inputTokens: daily.inputTokens,
		outputTokens:             daily.outputTokens,
		cacheCreationInputTokens: daily.cacheCreationInputTokens,
		cacheReadInputTokens:     daily.cacheReadInputTokens,
		reasoningTokens:          daily.reasoningTokens, cost: daily.cost,
		costStatus: row.CostStatus, costSource: daily.costSource,
		claudeMessageID: daily.claudeMessageID,
		claudeRequestID: daily.claudeRequestID, sourceUUID: daily.sourceUUID,
		usageDedupKey: daily.usageDedupKey, project: daily.project,
		agent: daily.agent, machine: daily.machine,
		userMessageCount: row.UserMessageCount, isAutomated: boolInt(row.IsAutomated),
		sessionActivityAt: usageSessionActivity(row),
		terminationStatus: stringValue(row.TerminationStatus),
		displayName:       usageSessionDisplayName(row),
		startedAt:         formatUsageTimestamp(row.SessionStartedAt),
	}
}

func usageEventProjectionDedupKey(row bunUsageProjection) string {
	if row.DedupKey != "" {
		return row.SessionID + ":" + row.UsageSource + ":" + row.DedupKey
	}
	return fmt.Sprintf("%s:%s:id:%d", row.SessionID, row.UsageSource, row.ID)
}

func dailyUsageEventProjectionDedupKey(row bunDailyUsageProjection) string {
	if row.DedupKey != "" {
		return row.SessionID + ":" + row.UsageSource + ":" + row.DedupKey
	}
	return fmt.Sprintf("%s:%s:id:%d", row.SessionID, row.UsageSource, row.ID)
}

func (s *BunStore) loadBunCursorUsageRows(
	ctx context.Context, store bun.IDB, filter UsageFilter,
) ([]dailyUsageScanRow, error) {
	if !cursorUsageMatchesFilter(filter) {
		return nil, nil
	}
	exists, err := s.bunTableExists(ctx, store, "cursor_usage_events")
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	var rows []bunCursorUsageProjection
	query := store.NewSelect().TableExpr("cursor_usage_events AS cu").
		Column("occurred_at", "model", "input_tokens", "output_tokens",
			"cache_write_tokens", "cache_read_tokens", "charged_microdollars",
			"is_headless", "dedup_key").
		Where("model != ?", "")
	query = appendBunUsageValues(query, "cu.model", csvUsageValues(filter.Model), true)
	query = appendBunUsageValues(
		query, "cu.model", csvUsageValues(filter.ExcludeModel), false,
	)
	switch normalizeAutomatedScope(filter.AutomatedScope, filter.ExcludeAutomated) {
	case "human":
		query = query.Where("cu.is_headless = ?", false)
	case "automated":
		query = query.Where("cu.is_headless = ?", true)
	}
	query = appendBunUsageBounds(
		query, filter, "cu.occurred_at", false, s.backend.TimestampOrderExpr,
	)
	if err := query.Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("querying cursor usage events: %w", err)
	}
	out := make([]dailyUsageScanRow, 0, len(rows))
	for _, row := range rows {
		if !usageSourceMatches(row.Model, filter) || !cursorAutomationMatches(row, filter) {
			continue
		}
		out = append(out, dailyUsageScanRow{
			usageSource: "cursor", ts: formatRequiredUsageTimestamp(row.OccurredAt),
			model: row.Model, inputTokens: row.InputTokens, outputTokens: row.OutputTokens,
			cacheCreationInputTokens: row.CacheWriteTokens,
			cacheReadInputTokens:     row.CacheReadTokens,
			cost:                     sql.NullInt64{Int64: row.ChargedMicrodollars, Valid: true},
			costSource:               "cursor-reported", usageDedupKey: row.DedupKey,
			agent: "cursor",
		})
	}
	return out, nil
}

func usageSourceMatches(model string, filter UsageFilter) bool {
	return usageCSVMatches(model, filter.Model, true) &&
		usageCSVMatches(model, filter.ExcludeModel, false)
}

func usageSessionMatches(
	row bunUsageProjection, filter UsageFilter, referenceTime time.Time,
) bool {
	if !usageCSVMatches(row.Agent, filter.Agent, true) ||
		!usageValuesMatch(row.Project, filter.ProjectFilterLabels(), true) ||
		!usageCSVMatches(row.Machine, filter.Machine, true) ||
		!usageValuesMatch(row.Project, filter.ExcludedProjectFilterLabels(), false) ||
		!usageCSVMatches(row.Agent, filter.ExcludeAgent, false) {
		return false
	}
	if filter.GitBranch != "" {
		matched := false
		for _, pair := range SplitBranchFilterTokens(filter.GitBranch) {
			if row.Project == pair.Project && row.GitBranch == pair.Branch {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if filter.MinUserMessages > 0 && row.UserMessageCount < filter.MinUserMessages {
		return false
	}
	scope := normalizeAutomatedScope(filter.AutomatedScope, filter.ExcludeAutomated)
	if filter.ExcludeOneShot && row.UserMessageCount <= 1 &&
		(scope == "human" || !row.IsAutomated) {
		return false
	}
	if scope == "human" && row.IsAutomated || scope == "automated" && !row.IsAutomated {
		return false
	}
	activity := usageSessionActivityTime(row)
	if filter.ActiveSince != "" {
		cutoff, err := time.Parse(time.RFC3339Nano, filter.ActiveSince)
		if err == nil && activity.Before(cutoff) {
			return false
		}
	}
	return usageTerminationMatches(
		stringValue(row.TerminationStatus), activity, filter.Termination, referenceTime,
	)
}

func cursorUsageMatchesFilter(filter UsageFilter) bool {
	if len(filter.ProjectFilterLabels()) > 0 ||
		len(filter.ExcludedProjectFilterLabels()) > 0 ||
		filter.Machine != "" || filter.GitBranch != "" ||
		filter.MinUserMessages > 0 || filter.ExcludeOneShot ||
		filter.ActiveSince != "" || usageHasTerminationFilter(filter.Termination) {
		return false
	}
	return usageCSVMatches("cursor", filter.Agent, true) &&
		usageCSVMatches("cursor", filter.ExcludeAgent, false)
}

func cursorAutomationMatches(row bunCursorUsageProjection, filter UsageFilter) bool {
	scope := normalizeAutomatedScope(filter.AutomatedScope, filter.ExcludeAutomated)
	return scope == "all" || scope == "human" && !row.IsHeadless ||
		scope == "automated" && row.IsHeadless
}

func usageHasTerminationFilter(status string) bool {
	for part := range strings.SplitSeq(status, ",") {
		switch strings.TrimSpace(part) {
		case "active", "stale", "unclean", "clean", "awaiting_user":
			return true
		}
	}
	return false
}

func usageTerminationMatches(
	status string, activity time.Time, filter string, referenceTime time.Time,
) bool {
	if !usageHasTerminationFilter(filter) {
		return true
	}
	activeCutoff := referenceTime.UTC().Add(-activeWindow)
	staleCutoff := referenceTime.UTC().Add(-staleWindow)
	flagged := status == "tool_call_pending" || status == "truncated"
	for part := range strings.SplitSeq(filter, ",") {
		switch strings.TrimSpace(part) {
		case "active":
			if activity.After(activeCutoff) {
				return true
			}
		case "stale":
			if activity.After(staleCutoff) && !activity.After(activeCutoff) && flagged {
				return true
			}
		case "unclean":
			if !activity.After(staleCutoff) && flagged {
				return true
			}
		case "clean":
			if status == "clean" {
				return true
			}
		case "awaiting_user":
			if status == "awaiting_user" {
				return true
			}
		}
	}
	return false
}

func usageCSVMatches(value, csv string, include bool) bool {
	if csv == "" {
		return true
	}
	return usageValuesMatch(value, strings.Split(csv, ","), include)
}

func usageValuesMatch(value string, values []string, include bool) bool {
	if len(values) == 0 {
		return true
	}
	matched := slices.Contains(values, value)
	if include {
		return matched
	}
	return !matched
}

func usageProjectionTimestamp(row bunUsageProjection) string {
	if value := formatUsageTimestamp(row.UsageTimestamp); value != "" {
		return value
	}
	return formatUsageTimestamp(row.SessionStartedAt)
}

func usageSessionActivity(row bunUsageProjection) string {
	return formatRequiredUsageTimestamp(bunmodel.NewTimestamp(
		usageSessionActivityTime(row),
	))
}

func usageSessionActivityTime(row bunUsageProjection) time.Time {
	if row.SessionEndedAt != nil && !row.SessionEndedAt.IsZero() {
		return row.SessionEndedAt.Time
	}
	if row.SessionStartedAt != nil && !row.SessionStartedAt.IsZero() {
		return row.SessionStartedAt.Time
	}
	return row.SessionCreatedAt.Time
}

func usageSessionDisplayName(row bunUsageProjection) string {
	var candidate string
	if row.DisplayName != nil {
		candidate = *row.DisplayName
	} else if row.SessionName != nil {
		candidate = *row.SessionName
	}
	if candidate != "" {
		return candidate
	}
	if row.FirstMessage != nil && *row.FirstMessage != "" {
		return *row.FirstMessage
	}
	if row.Project != "" {
		return row.Project
	}
	return row.SessionID
}

func formatUsageTimestamp(value *bunmodel.Timestamp) string {
	if value == nil || value.IsZero() {
		return ""
	}
	return formatRequiredUsageTimestamp(*value)
}

func formatRequiredUsageTimestamp(value bunmodel.Timestamp) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func nullableUsageOrdinal(value *int) sql.NullInt64 {
	if value == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*value), Valid: true}
}

func nullableUsageCost(value *int64) sql.NullInt64 {
	if value == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *value, Valid: true}
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func sortDailyUsageRows(rows []dailyUsageScanRow) {
	sort.SliceStable(rows, func(left, right int) bool {
		return dailyUsageRowPrecedes(rows[left], rows[right])
	})
}

func dailyUsageRowPrecedes(left, right dailyUsageScanRow) bool {
	leftTimestamp := dailyUsageRowTimestamp(left)
	rightTimestamp := dailyUsageRowTimestamp(right)
	if leftTimestamp != rightTimestamp {
		return leftTimestamp < rightTimestamp
	}
	if left.sessionID != right.sessionID {
		return left.sessionID < right.sessionID
	}
	leftOrdinal, rightOrdinal := int64(-1), int64(-1)
	if left.messageOrdinal.Valid {
		leftOrdinal = left.messageOrdinal.Int64
	}
	if right.messageOrdinal.Valid {
		rightOrdinal = right.messageOrdinal.Int64
	}
	return leftOrdinal < rightOrdinal
}

func dailyUsageRowTimestamp(row dailyUsageScanRow) string {
	if row.ts != "" {
		return row.ts
	}
	return formatRequiredUsageTime(row.usageTime)
}

func sortUsageRows(rows []usageScanRow) {
	sort.SliceStable(rows, func(left, right int) bool {
		if rows[left].ts != rows[right].ts {
			return rows[left].ts < rows[right].ts
		}
		if rows[left].sessionID != rows[right].sessionID {
			return rows[left].sessionID < rows[right].sessionID
		}
		leftOrdinal, rightOrdinal := int64(-1), int64(-1)
		if rows[left].messageOrdinal.Valid {
			leftOrdinal = rows[left].messageOrdinal.Int64
		}
		if rows[right].messageOrdinal.Valid {
			rightOrdinal = rows[right].messageOrdinal.Int64
		}
		if leftOrdinal != rightOrdinal {
			return leftOrdinal < rightOrdinal
		}
		if rows[left].usageSource != rows[right].usageSource {
			return rows[left].usageSource < rows[right].usageSource
		}
		return rows[left].usageDedupKey < rows[right].usageDedupKey
	})
}
