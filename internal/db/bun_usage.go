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
	"go.kenn.io/agentsview/internal/db/bunmodel"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

const bunPricingWriteBatchSize = 500

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
		for start := 0; start < len(prices); start += bunPricingWriteBatchSize {
			end := min(start+bunPricingWriteBatchSize, len(prices))
			batch := prices[start:end]
			if _, err := tx.NewInsert().Model(&batch).Column(
				"model_pattern", "input_microdollars_per_mtok",
				"output_microdollars_per_mtok",
				"cache_creation_microdollars_per_mtok",
				"cache_read_microdollars_per_mtok", "updated_at",
			).
				On("CONFLICT (model_pattern) DO UPDATE").
				Set("input_microdollars_per_mtok = EXCLUDED.input_microdollars_per_mtok").
				Set("output_microdollars_per_mtok = EXCLUDED.output_microdollars_per_mtok").
				Set("cache_creation_microdollars_per_mtok = EXCLUDED.cache_creation_microdollars_per_mtok").
				Set("cache_read_microdollars_per_mtok = EXCLUDED.cache_read_microdollars_per_mtok").
				Set("updated_at = EXCLUDED.updated_at").Exec(ctx); err != nil {
				return fmt.Errorf("upserting model pricing rows: %w", err)
			}
		}
		return ReplaceModelPricingBandRows(ctx, tx, patterns, bands)
	})
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
		batch := bands[start:end]
		if _, err := store.NewInsert().Model(&batch).Column(
			"model_pattern", "above_input_tokens",
			"input_microdollars_per_mtok", "output_microdollars_per_mtok",
			"cache_creation_microdollars_per_mtok",
			"cache_read_microdollars_per_mtok", "updated_at",
		).Exec(ctx); err != nil {
			return fmt.Errorf("inserting model pricing bands: %w", err)
		}
	}
	return nil
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
	prices, err := listBunModelPricing(ctx, store)
	if err != nil {
		return nil, err
	}

	s.pricingMu.RLock()
	custom := maps.Clone(s.pricing.custom)
	effective := cloneModelRates(s.pricing.effective)
	emptyCatalog := cloneModelRates(s.pricing.emptyCatalog)
	s.pricingMu.RUnlock()

	fallback := fallbackRateMap()
	out := make(map[string]export.ModelRates)
	for _, price := range prices {
		if strings.HasPrefix(price.ModelPattern, "_") {
			continue
		}
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

func (s *BunStore) loadPricingMap(
	ctx context.Context,
) ([]export.EffectivePricingRow, error) {
	return s.LoadPricingMap(ctx)
}

func listBunModelPricing(
	ctx context.Context, store bun.IDB,
) ([]ModelPricing, error) {
	var rows []bunmodel.ModelPricing
	if err := store.NewSelect().Model(&rows).
		OrderExpr("model_pattern ASC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing model pricing: %w", err)
	}
	var bandRows []bunmodel.ModelPricingBand
	if err := store.NewSelect().Model(&bandRows).
		OrderExpr("model_pattern ASC, above_input_tokens ASC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing model pricing bands: %w", err)
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

const bunMessageUsageColumns = `
	m.session_id AS session_id,
	m.ordinal AS message_ordinal,
	m.timestamp AS usage_timestamp,
	m.model AS model,
	m.token_usage AS token_json,
	m.claude_message_id AS claude_message_id,
	m.claude_request_id AS claude_request_id,
	m.source_uuid AS source_uuid`

const bunEventUsageColumns = `
	ue.id AS id,
	ue.session_id AS session_id,
	ue.message_ordinal AS message_ordinal,
	ue.occurred_at AS usage_timestamp,
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

func (s *BunStore) loadDailyUsageRows(
	ctx context.Context, filter UsageFilter, includeCursor, matching bool,
) ([]dailyUsageScanRow, error) {
	var staged []dailyUsageScanRow
	err := s.consistentView(ctx, func(store bun.IDB) error {
		rows, err := loadBunSessionUsageRows(ctx, store, filter, matching)
		if err != nil {
			return err
		}
		if includeCursor {
			cursor, err := loadBunCursorUsageRows(ctx, store, filter)
			if err != nil {
				return err
			}
			rows = append(rows, cursor...)
		}
		sortDailyUsageRows(rows)
		staged = rows
		return nil
	})
	return staged, err
}

func (s *BunStore) loadSessionUsageRows(
	ctx context.Context, sessionID string,
) ([]usageScanRow, error) {
	filter := UsageFilter{}
	var staged []usageScanRow
	err := s.consistentView(ctx, func(store bun.IDB) error {
		rows, err := loadBunUsageProjections(ctx, store, filter, false, sessionID)
		if err != nil {
			return err
		}
		out := make([]usageScanRow, 0, len(rows))
		for _, row := range rows {
			out = append(out, usageProjectionToFullRow(row))
		}
		sortUsageRows(out)
		staged = out
		return nil
	})
	return staged, err
}

func loadBunSessionUsageRows(
	ctx context.Context, store bun.IDB, filter UsageFilter, matching bool,
) ([]dailyUsageScanRow, error) {
	projections, err := loadBunUsageProjections(ctx, store, filter, matching, "")
	if err != nil {
		return nil, err
	}
	rows := make([]dailyUsageScanRow, 0, len(projections))
	for _, row := range projections {
		rows = append(rows, usageProjectionToDailyRow(row))
	}
	return rows, nil
}

func loadBunUsageProjections(
	ctx context.Context, store bun.IDB, filter UsageFilter,
	matching bool, sessionID string,
) ([]bunUsageProjection, error) {
	var messages []bunUsageProjection
	messageQuery := store.NewSelect().TableExpr("messages AS m").
		ColumnExpr(bunMessageUsageColumns + "," + bunUsageSessionColumns).
		Join("JOIN sessions AS s ON s.id = m.session_id").
		Where("s.deleted_at IS NULL")
	if matching {
		messageQuery = messageQuery.Where("m.role = ?", "assistant").
			Where("m.model != ?", "<synthetic>")
	} else {
		messageQuery = messageQuery.Where("m.token_usage != ?", "").
			Where("m.model != ?", "").Where("m.model != ?", "<synthetic>")
	}
	if sessionID != "" {
		messageQuery = messageQuery.Where("m.session_id = ?", sessionID)
	}
	messageQuery = appendBunUsageLowerBound(messageQuery, filter, "m.timestamp")
	if err := messageQuery.Scan(ctx, &messages); err != nil {
		return nil, fmt.Errorf("querying usage messages: %w", err)
	}
	for index := range messages {
		messages[index].CostStatus = ""
		messages[index].CostSource = ""
	}

	var events []bunUsageProjection
	eventQuery := store.NewSelect().TableExpr("usage_events AS ue").
		ColumnExpr(bunEventUsageColumns+","+bunUsageSessionColumns).
		Join("JOIN sessions AS s ON s.id = ue.session_id").
		Where("s.deleted_at IS NULL").Where("ue.model != ?", "")
	if sessionID != "" {
		eventQuery = eventQuery.Where("ue.session_id = ?", sessionID)
	}
	eventQuery = appendBunUsageLowerBound(eventQuery, filter, "ue.occurred_at")
	if err := eventQuery.Scan(ctx, &events); err != nil {
		return nil, fmt.Errorf("querying usage events: %w", err)
	}

	rows := make([]bunUsageProjection, 0, len(messages)+len(events))
	for _, row := range messages {
		if !usageSourceMatches(row.Model, filter) || !usageSessionMatches(row, filter) {
			continue
		}
		row.UsageDedupKey = ""
		rows = append(rows, row)
	}
	for _, row := range events {
		if !usageSourceMatches(row.Model, filter) || !usageSessionMatches(row, filter) {
			continue
		}
		row.UsageDedupKey = usageEventProjectionDedupKey(row)
		rows = append(rows, row)
	}
	return rows, nil
}

func appendBunUsageLowerBound(
	query *bun.SelectQuery, filter UsageFilter, timestampColumn string,
) *bun.SelectQuery {
	bounds := usageBoundsForFilter(filter)
	if bounds.from == "" {
		return query
	}
	parsed, err := time.Parse(time.RFC3339, bounds.from)
	if err != nil {
		return query
	}
	return query.Where(
		"("+timestampColumn+" >= ? OR s.started_at >= ?)", parsed, parsed,
	)
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

func loadBunCursorUsageRows(
	ctx context.Context, store bun.IDB, filter UsageFilter,
) ([]dailyUsageScanRow, error) {
	if !cursorUsageMatchesFilter(filter) {
		return nil, nil
	}
	var rows []bunCursorUsageProjection
	query := store.NewSelect().TableExpr("cursor_usage_events AS cu").
		Column("occurred_at", "model", "input_tokens", "output_tokens",
			"cache_write_tokens", "cache_read_tokens", "charged_microdollars",
			"is_headless", "dedup_key").
		Where("model != ?", "")
	if bounds := usageBoundsForFilter(filter); bounds.from != "" {
		if parsed, err := time.Parse(time.RFC3339, bounds.from); err == nil {
			query = query.Where("occurred_at >= ?", parsed)
		}
	}
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

func usageSessionMatches(row bunUsageProjection, filter UsageFilter) bool {
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
		stringValue(row.TerminationStatus), activity, filter.Termination,
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

func usageTerminationMatches(status string, activity time.Time, filter string) bool {
	if !usageHasTerminationFilter(filter) {
		return true
	}
	now := time.Now().UTC()
	activeCutoff := now.Add(-activeWindow)
	staleCutoff := now.Add(-staleWindow)
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
		return leftOrdinal < rightOrdinal
	})
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
