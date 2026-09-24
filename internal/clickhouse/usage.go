package clickhouse

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/ext"
)

const (
	chActiveWindow = 10 * time.Minute
	chStaleWindow  = 60 * time.Minute
	chTimestampSQL = "parseDateTime64BestEffort(?, 6, 'UTC')"
)

type chRates struct {
	input           money.Money
	output          money.Money
	cacheCreation   money.Money
	cacheCreation1h money.Money
	cacheRead       money.Money
	updatedAt       *time.Time
	source          export.PricingRowSource
	bands           []export.PricingBand
}

// chLoadPricing reads the mirrored model catalog and layers the reader's
// custom rates on top. Push passes no custom rates.
func chLoadPricing(
	ctx context.Context, conn *sql.DB,
	customPricing map[string]config.CustomModelRate,
) (map[string]chRates, error) {
	rows, err := readModelPricing(ctx, conn)
	if err != nil {
		return nil, err
	}
	out := map[string]chRates{}
	count := 0
	for _, p := range rows {
		if strings.HasPrefix(p.ModelPattern, "_") {
			continue
		}
		rates := chRates{
			input:           p.InputPerMTok,
			output:          p.OutputPerMTok,
			cacheCreation:   p.CacheCreationPerMTok,
			cacheCreation1h: p.CacheCreation1hPerMTok,
			cacheRead:       p.CacheReadPerMTok,
			bands:           chExportPricingBands(p.Bands),
		}
		if parsed, err := time.Parse(time.RFC3339Nano, p.UpdatedAt); err == nil {
			t := parsed.UTC()
			rates.updatedAt = &t
		}
		out[p.ModelPattern] = rates
		count++
	}
	if count == 0 {
		out = chFallbackPricingMap()
	} else {
		fallback := chFallbackPricingMap()
		for model, rates := range out {
			rates.source = chPricingSource(model, rates, fallback)
			out[model] = rates
		}
	}
	chApplyCustomPricing(out, customPricing)
	return out, nil
}

func chApplyCustomPricing(out map[string]chRates, customPricing map[string]config.CustomModelRate) {
	for model, custom := range customPricing {
		rates := chRates{
			input:  money.Money{Microdollars: custom.InputMicrodollarsPerMTok},
			output: money.Money{Microdollars: custom.OutputMicrodollarsPerMTok},
			cacheCreation: money.Money{
				Microdollars: custom.CacheCreationMicrodollarsPerMTok,
			},
			cacheCreation1h: money.Money{
				Microdollars: custom.CacheCreation1hMicrodollarsPerMTok,
			},
			cacheRead: money.Money{
				Microdollars: custom.CacheReadMicrodollarsPerMTok,
			},
		}
		rates.source = chCustomPricingSource()
		out[model] = rates
	}
}

// chLoadPricingRows returns the effective pricing rows plus the raw GenAI
// document they were built from; document is nil when the mirror has none.
func chLoadPricingRows(
	ctx context.Context, conn *sql.DB,
	customPricing map[string]config.CustomModelRate,
) ([]export.EffectivePricingRow, *db.GenAIPricingDocument, error) {
	pricing, err := chLoadPricing(ctx, conn, customPricing)
	if err != nil {
		return nil, nil, err
	}
	document, err := loadGenAIPricing(ctx, conn)
	if err != nil {
		return nil, nil, err
	}
	genAI, err := genAIEffectivePricingRow(document)
	if err != nil {
		return nil, nil, err
	}
	return append(chPricingRows(pricing), genAI), document, nil
}

func (s *Store) loadPricingResolver(
	ctx context.Context,
) (*export.PricingResolver, error) {
	snapshot, err := s.pricingSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	return export.NewPricingResolverWithDigest(snapshot.rows, snapshot.digest), nil
}

func chCustomPricingSource() export.PricingRowSource {
	return export.PricingRowSourceCustom
}

func chFallbackPricingMap() map[string]chRates {
	prices := pricingpkg.FallbackPricing()
	out := make(map[string]chRates, len(prices))
	for _, p := range prices {
		if strings.HasPrefix(p.ModelPattern, "_") {
			continue
		}
		out[p.ModelPattern] = chRates{
			input:           p.InputPerMTok,
			output:          p.OutputPerMTok,
			cacheCreation:   p.CacheCreationPerMTok,
			cacheCreation1h: p.CacheCreation1hPerMTok,
			cacheRead:       p.CacheReadPerMTok,
			source:          export.PricingRowSourceEmbedded,
			bands:           chCatalogPricingBands(p.Bands),
		}
	}
	return out
}

func chPricingSource(
	model string, rates chRates, fallback map[string]chRates,
) export.PricingRowSource {
	if f, ok := fallback[model]; ok &&
		f.input == rates.input &&
		f.output == rates.output &&
		f.cacheCreation == rates.cacheCreation &&
		f.cacheCreation1h == rates.cacheCreation1h &&
		f.cacheRead == rates.cacheRead &&
		chPricingBandsEqual(f.bands, rates.bands) {
		return export.PricingRowSourceEmbedded
	}
	return export.PricingRowSourceFetched
}

func chCatalogPricingBands(
	bands []pricingpkg.PricingBand,
) []export.PricingBand {
	out := make([]export.PricingBand, len(bands))
	for i, band := range bands {
		out[i] = export.PricingBand{
			AboveInputTokens:    band.AboveInputTokens,
			InputPerMTok:        band.InputPerMTok,
			OutputPerMTok:       band.OutputPerMTok,
			CacheWritePerMTok:   band.CacheCreationPerMTok,
			CacheWrite1hPerMTok: band.CacheCreation1hPerMTok,
			CacheReadPerMTok:    band.CacheReadPerMTok,
		}
	}
	return out
}

func chExportPricingBands(bands []db.PricingBand) []export.PricingBand {
	out := make([]export.PricingBand, 0, len(bands))
	for _, band := range bands {
		var parsedUpdatedAt *time.Time
		if parsed, err := time.Parse(time.RFC3339Nano, band.UpdatedAt); err == nil {
			t := parsed.UTC()
			parsedUpdatedAt = &t
		}
		out = append(out, export.PricingBand{
			AboveInputTokens:    band.AboveInputTokens,
			InputPerMTok:        band.InputPerMTok,
			OutputPerMTok:       band.OutputPerMTok,
			CacheWritePerMTok:   band.CacheCreationPerMTok,
			CacheWrite1hPerMTok: band.CacheCreation1hPerMTok,
			CacheReadPerMTok:    band.CacheReadPerMTok,
			UpdatedAt:           parsedUpdatedAt,
		})
	}
	return out
}

func chPricingBandsEqual(a, b []export.PricingBand) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].AboveInputTokens != b[i].AboveInputTokens ||
			a[i].InputPerMTok != b[i].InputPerMTok ||
			a[i].OutputPerMTok != b[i].OutputPerMTok ||
			a[i].CacheWritePerMTok != b[i].CacheWritePerMTok ||
			a[i].CacheWrite1hPerMTok != b[i].CacheWrite1hPerMTok ||
			a[i].CacheReadPerMTok != b[i].CacheReadPerMTok {
			return false
		}
	}
	return true
}

func chPricingRows(
	in map[string]chRates,
) []export.EffectivePricingRow {
	out := make([]export.EffectivePricingRow, 0, len(in))
	fallback := chFallbackPricingMap()
	for pattern, rates := range in {
		source := rates.source
		if source == "" {
			source = chPricingSource(pattern, rates, fallback)
		}
		out = append(out, export.EffectivePricingRow{
			ModelPattern: pattern,
			Rates: export.ModelRates{
				InputPerMTok:        rates.input,
				OutputPerMTok:       rates.output,
				CacheWritePerMTok:   rates.cacheCreation,
				CacheWrite1hPerMTok: rates.cacheCreation1h,
				CacheReadPerMTok:    rates.cacheRead,
				UpdatedAt:           rates.updatedAt,
				Source:              source,
				Bands:               append([]export.PricingBand(nil), rates.bands...),
			},
		})
	}
	return out
}

type chUsageBounds struct {
	from string
	to   string
}

func chUsagePaddedUTCBound(ts string, hours int) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return t.Add(time.Duration(hours) * time.Hour).Format(time.RFC3339)
}

func chUsageBoundsForFilter(f db.UsageFilter) chUsageBounds {
	var b chUsageBounds
	if f.From != "" {
		b.from = chUsagePaddedUTCBound(f.From+"T00:00:00Z", -14)
	}
	if f.To != "" {
		b.to = chUsagePaddedUTCBound(f.To+"T23:59:59Z", 14)
	}
	return b
}

func appendChUsageColumnBounds(
	where, col string, b chUsageBounds, args []any,
) (string, []any) {
	if b.from != "" {
		where += "\n\t\t\tAND " + col + " >= " + chTimestampSQL
		args = append(args, b.from)
	}
	if b.to != "" {
		where += "\n\t\t\tAND " + col + " <= " + chTimestampSQL
		args = append(args, b.to)
	}
	return where, args
}

func appendChUsageCSVFilter(
	where string, args []any, col, csv string, include bool,
) (string, []any) {
	if csv == "" {
		return where, args
	}
	parts := strings.Split(csv, ",")
	vals := make([]string, 0, len(parts))
	for _, value := range parts {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			vals = append(vals, trimmed)
		}
	}
	return appendChUsageValuesFilter(where, args, col, vals, include)
}

func appendChUsageValuesFilter(
	where string, args []any, col string, vals []string, include bool,
) (string, []any) {
	if len(vals) == 0 {
		return where, args
	}
	op := "IN"
	if !include {
		op = "NOT IN"
	}
	if len(vals) == 1 {
		if include {
			where += "\n\t\t\tAND " + col + " = ?"
		} else {
			where += "\n\t\t\tAND " + col + " != ?"
		}
		args = append(args, vals[0])
		return where, args
	}
	ph := make([]string, len(vals))
	for i, value := range vals {
		ph[i] = "?"
		args = append(args, value)
	}
	where += "\n\t\t\tAND " + col + " " + op +
		" (" + strings.Join(ph, ",") + ")"
	return where, args
}

func appendChUsageSourceFilterClauses(
	where string, args []any, modelCol string, f db.UsageFilter,
) (string, []any) {
	where, args = appendChUsageCSVFilter(where, args, modelCol, f.Model, true)
	return appendChUsageCSVFilter(where, args, modelCol, f.ExcludeModel, false)
}

func chNormalizeAutomatedScope(
	scope string,
	excludeAutomated bool,
) string {
	switch strings.TrimSpace(scope) {
	case "human", "all", "automated":
		return strings.TrimSpace(scope)
	}
	if excludeAutomated {
		return "human"
	}
	return "all"
}

func chAutomatedScopePredicate(scope, col string) string {
	switch scope {
	case "human":
		return col + " = false"
	case "automated":
		return col + " = true"
	default:
		return ""
	}
}

func appendChUsageSessionFilterClauses(
	where string, args []any, f db.UsageFilter, sessionID string,
) (string, []any) {
	where, args = appendChUsageCSVFilter(where, args, "s.agent", f.Agent, true)
	where, args = appendChUsageValuesFilter(
		where, args, "s.project", f.ProjectFilterLabels(), true,
	)
	where, args = appendChUsageCSVFilter(where, args, "s.machine", f.Machine, true)
	if f.GitBranch != "" {
		var clause string
		clause, args = db.BranchPairClauseArgs("s.project", "s.git_branch", f.GitBranch, args)
		where += "\n\t\t\tAND " + clause
	}
	where, args = appendChUsageValuesFilter(
		where, args, "s.project", f.ExcludedProjectFilterLabels(), false,
	)
	where, args = appendChUsageCSVFilter(where, args, "s.agent", f.ExcludeAgent, false)
	if sessionID != "" {
		where += "\n\t\t\tAND s.id = ?"
		args = append(args, sessionID)
	}
	if f.MinUserMessages > 0 {
		where += "\n\t\t\tAND s.user_message_count >= ?"
		args = append(args, f.MinUserMessages)
	}
	scope := chNormalizeAutomatedScope(
		f.AutomatedScope, f.ExcludeAutomated)
	if f.ExcludeOneShot {
		if scope == "human" {
			where += "\n\t\t\tAND s.user_message_count > 1"
		} else {
			where += "\n\t\t\tAND (s.user_message_count > 1 OR COALESCE(s.is_automated, false) = true)"
		}
	}
	if pred := chAutomatedScopePredicate(
		scope, "COALESCE(s.is_automated, false)"); pred != "" {
		where += "\n\t\t\tAND " + pred
	}
	if f.ActiveSince != "" {
		where += "\n\t\t\tAND COALESCE(s.ended_at, s.started_at, s.created_at) >= " + chTimestampSQL
		args = append(args, f.ActiveSince)
	}
	if pred, predArgs := chUsageTerminationPred(f.Termination); pred != "" {
		where += "\n\t\t\tAND " + pred
		args = append(args, predArgs...)
	}
	return where, args
}

func chUsageTerminationPred(status string) (string, []any) {
	return chTerminationPred(
		status,
		"COALESCE(s.ended_at, s.started_at, s.created_at)",
		"s.termination_status",
	)
}

// chTerminationUsesTime reports whether a termination filter compares
// session times with the current time; see chTerminationPred.
func chTerminationUsesTime(status string) bool {
	for part := range strings.SplitSeq(status, ",") {
		switch strings.TrimSpace(part) {
		case "active", "stale", "unclean":
			return true
		}
	}
	return false
}

func chTerminationPred(
	status string,
	activityExpr string,
	statusExpr string,
) (string, []any) {
	if status == "" || status == "all" {
		return "", nil
	}
	now := time.Now().UTC()
	activeCutoff := now.Add(-chActiveWindow)
	staleCutoff := now.Add(-chStaleWindow)
	flagged := statusExpr + " IN ('tool_call_pending', 'truncated')"
	var parts []string
	var args []any
	for part := range strings.SplitSeq(status, ",") {
		switch strings.TrimSpace(part) {
		case "active":
			parts = append(parts, activityExpr+" > "+chTimestampSQL)
			args = append(args, activeCutoff.Format(time.RFC3339))
		case "stale":
			parts = append(parts, "("+flagged+
				" AND "+activityExpr+" > "+chTimestampSQL+
				" AND "+activityExpr+" <= "+chTimestampSQL+")")
			args = append(args,
				staleCutoff.Format(time.RFC3339),
				activeCutoff.Format(time.RFC3339),
			)
		case "unclean":
			parts = append(parts, "("+flagged+
				" AND "+activityExpr+" <= "+chTimestampSQL+")")
			args = append(args, staleCutoff.Format(time.RFC3339))
		case "clean":
			parts = append(parts, statusExpr+" = 'clean'")
		case "awaiting_user":
			parts = append(parts, statusExpr+" = 'awaiting_user'")
		}
	}
	if len(parts) == 0 {
		return "", nil
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

const chDailyCursorUsageRowsSQLTemplate = `
SELECT
	'' AS session_id,
	CAST(NULL AS Nullable(Int64)) AS message_ordinal,
	'cursor' AS source,
	cu.occurred_at AS ts,
	cu.occurred_at AS pricing_ts,
	cu.model AS model,
	'' AS provider_id,
	'' AS token_json,
	'' AS claude_message_id,
	'' AS claude_request_id,
	'' AS source_uuid,
	cu.dedup_key AS usage_dedup_key,
	cu.input_tokens AS input_tokens,
	cu.output_tokens AS output_tokens,
	cu.cache_write_tokens AS cache_create,
	cu.cache_read_tokens AS cache_read,
	toInt64(0) AS reasoning_tokens,
	toInt64(0) AS cache_create_1h, toInt64(0) AS web_search_requests,
	CAST(cu.charged_microdollars AS Nullable(Int64)) AS cost_microdollars,
	'cursor-reported' AS cost_source,
	'' AS project,
	'cursor' AS agent,
	'' AS machine,
	toInt64(0) AS user_message_count,
	cu.is_headless AS is_automated,
	'' AS display_name,
	CAST(NULL AS Nullable(DateTime64(6, 'UTC'))) AS started_at,
	cu.occurred_at AS activity_at
FROM cursor_usage_events cu
WHERE %s`

const chUsageMessageEligibility = `
			m.token_usage != ''
			AND m.model != ''
			AND m.model != '<synthetic>'
			AND s.deleted_at IS NULL`

const chUsageMatchingMessageSourceEligibility = `
			m.role = 'assistant'
			AND m.model != '<synthetic>'`

const chUsageMatchingMessageEligibility = chUsageMatchingMessageSourceEligibility + `
			AND s.deleted_at IS NULL`

const chUsageEventSourceEligibility = `
			ue.model != ''`

const chUsageEventEligibility = chUsageEventSourceEligibility + `
			AND s.deleted_at IS NULL`

func chUsageSourceWheres(
	f db.UsageFilter, sessionID, messageEligibility string, b chUsageBounds,
) (string, []any, string, []any) {
	messageWhere := messageEligibility
	var messageArgs []any
	messageWhere, messageArgs = appendChUsageSourceFilterClauses(
		messageWhere, messageArgs, "m.model", f)
	messageWhere, messageArgs = appendChUsageSessionFilterClauses(
		messageWhere, messageArgs, f, sessionID)
	messageWhere, messageArgs = appendChUsageColumnBounds(
		messageWhere, "COALESCE(m.timestamp, s.started_at)", b, messageArgs)
	if b.from != "" || b.to != "" {
		// Filter timestamped rows before joining sessions; missing timestamps
		// still use the session start and the original bounds above.
		timestampWhere, timestampArgs := appendChUsageColumnBounds("1", "m.timestamp", b, nil)
		messageWhere += "\n\t\t\tAND (m.timestamp IS NULL OR (" + timestampWhere + "))"
		messageArgs = append(messageArgs, timestampArgs...)
	}

	eventWhere := chUsageEventEligibility
	var eventArgs []any
	eventWhere, eventArgs = appendChUsageSourceFilterClauses(
		eventWhere, eventArgs, "ue.model", f)
	eventWhere, eventArgs = appendChUsageSessionFilterClauses(
		eventWhere, eventArgs, f, sessionID)
	eventWhere, eventArgs = appendChUsageColumnBounds(
		eventWhere, "COALESCE(ue.occurred_at, s.started_at)", b, eventArgs)

	return messageWhere, messageArgs, eventWhere, eventArgs
}

func chUsageRawSQL(f db.UsageFilter, sessionID string) (string, []any) {
	messageWhere, messageArgs, eventWhere, eventArgs := chUsageSourceWheres(
		f, sessionID, chUsageMessageEligibility, chUsageBoundsForFilter(f))
	return chUsageRawSQLFromWheres(
		"JOIN", messageWhere, messageArgs, eventWhere, eventArgs)
}

// chUsageRawSQLFromWheres renders the message and usage-event raw rows.
// sessionJoin is "JOIN" for readers; push pricing uses "LEFT JOIN" because
// it prices a batch before that batch's session rows exist.
func chUsageRawSQLFromWheres(
	sessionJoin, messageWhere string, messageArgs []any,
	eventWhere string, eventArgs []any,
) (string, []any) {
	query := fmt.Sprintf(`
		SELECT m.session_id AS session_id, toNullable(m.ordinal) AS message_ordinal,
			'message' AS source, COALESCE(m.timestamp, s.started_at) AS ts,
			m.timestamp AS pricing_ts,
			m.model AS model, m.provider_id AS provider_id, m.token_usage AS token_json,
			m.claude_message_id AS claude_message_id,
			m.claude_request_id AS claude_request_id,
			m.source_uuid AS source_uuid,
			'' AS usage_dedup_key,
				toInt64(0) AS input_tokens, toInt64(0) AS output_tokens,
				toInt64(0) AS cache_create, toInt64(0) AS cache_read,
				JSONExtractInt(m.token_usage, 'reasoning_tokens') AS reasoning_tokens,
				JSONExtractInt(m.token_usage, 'cache_creation', 'ephemeral_1h_input_tokens') AS cache_create_1h,
				JSONExtractInt(m.token_usage, 'server_tool_use', 'web_search_requests') AS web_search_requests,
				CAST(NULL AS Nullable(Int64)) AS cost_microdollars, '' AS cost_source,
			s.project AS project, s.agent AS agent, s.machine AS machine,
			s.user_message_count AS user_message_count, s.is_automated AS is_automated,
			ifNull(COALESCE(s.display_name, s.session_name, s.first_message, s.project, s.id), '') AS display_name,
			s.started_at AS started_at,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS activity_at
		FROM messages m
		%[1]s sessions s ON s.id = m.session_id
		WHERE %[2]s
		UNION ALL
		SELECT ue.session_id AS session_id, ue.message_ordinal AS message_ordinal,
			ue.source AS source, COALESCE(ue.occurred_at, s.started_at) AS ts,
			ue.occurred_at AS pricing_ts,
			ue.model AS model, ue.provider_id AS provider_id, '' AS token_json,
			'' AS claude_message_id, '' AS claude_request_id,
			'' AS source_uuid,
			if(ue.dedup_key != '',
				concat(ue.session_id, ':', ue.source, ':', ue.dedup_key),
				concat(ue.session_id, ':', ue.source, ':id:', toString(ue.id))
			) AS usage_dedup_key,
				ue.input_tokens AS input_tokens, ue.output_tokens AS output_tokens,
				ue.cache_creation_input_tokens AS cache_create,
				ue.cache_read_input_tokens AS cache_read,
				ue.reasoning_tokens AS reasoning_tokens,
				toInt64(0) AS cache_create_1h, toInt64(0) AS web_search_requests,
				ue.cost_microdollars AS cost_microdollars,
				ue.cost_source AS cost_source,
			s.project AS project, s.agent AS agent, s.machine AS machine,
			s.user_message_count AS user_message_count, s.is_automated AS is_automated,
			ifNull(COALESCE(s.display_name, s.session_name, s.first_message, s.project, s.id), '') AS display_name,
			s.started_at AS started_at,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS activity_at
		FROM usage_events ue
		%[1]s sessions s ON s.id = ue.session_id
		WHERE %[3]s`,
		sessionJoin, messageWhere, eventWhere)
	args := make([]any, 0, len(messageArgs)+len(eventArgs))
	args = append(args, messageArgs...)
	args = append(args, eventArgs...)
	return query, args
}

func chMatchingUsageRawSQL(f db.UsageFilter) (string, []any) {
	messageWhere, messageArgs, eventWhere, eventArgs := chUsageSourceWheres(
		f, "", chUsageMatchingMessageEligibility, chUsageBoundsForFilter(f))

	query := fmt.Sprintf(`
		SELECT m.session_id AS session_id,
			COALESCE(m.timestamp, s.started_at) AS ts
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		WHERE %s
		UNION ALL
		SELECT ue.session_id AS session_id,
			COALESCE(ue.occurred_at, s.started_at) AS ts
		FROM usage_events ue
		JOIN sessions s ON s.id = ue.session_id
		WHERE %s`,
		messageWhere, eventWhere)
	args := make([]any, 0, len(messageArgs)+len(eventArgs))
	args = append(args, messageArgs...)
	args = append(args, eventArgs...)
	return query, args
}

// chCursorUsageRowsWhere is the predicate selecting the cursor usage rows a
// filter reads; ok is false when the filter excludes cursor rows entirely.
func chCursorUsageRowsWhere(
	f db.UsageFilter, b chUsageBounds,
) (string, []any, bool) {
	hasTermFilter := f.Termination != "" && f.Termination != "all"
	if len(f.ProjectFilterLabels()) > 0 ||
		len(f.ExcludedProjectFilterLabels()) > 0 ||
		f.Machine != "" || f.GitBranch != "" || f.MinUserMessages > 0 ||
		f.ExcludeOneShot || hasTermFilter ||
		f.ActiveSince != "" {
		return "", nil, false
	}
	if f.Agent != "" {
		vals := strings.Split(f.Agent, ",")
		for i := range vals {
			vals[i] = strings.TrimSpace(vals[i])
		}
		if !slices.Contains(vals, "cursor") {
			return "", nil, false
		}
	}
	if f.ExcludeAgent != "" {
		vals := strings.Split(f.ExcludeAgent, ",")
		for i := range vals {
			vals[i] = strings.TrimSpace(vals[i])
		}
		if slices.Contains(vals, "cursor") {
			return "", nil, false
		}
	}

	where := "cu.model != ''"
	var args []any
	scope := chNormalizeAutomatedScope(f.AutomatedScope, f.ExcludeAutomated)
	if pred := chAutomatedScopePredicate(scope, "cu.is_headless"); pred != "" {
		where += "\n\tAND " + pred
	}
	where, args = appendChUsageSourceFilterClauses(
		where, args, "cu.model", f,
	)
	where, args = appendChUsageColumnBounds(
		where, "cu.occurred_at", b, args,
	)
	return where, args, true
}

func chCursorUsageRowsSQLForBounds(
	f db.UsageFilter, b chUsageBounds,
) (string, []any, bool) {
	where, args, ok := chCursorUsageRowsWhere(f, b)
	if !ok {
		return "", nil, false
	}
	return fmt.Sprintf(chDailyCursorUsageRowsSQLTemplate, where), args, true
}

// cursorUsageRowsInBounds reports whether a daily read under f selects any
// cursor usage rows. The rows join the usage union from their own small
// table, and the union roughly doubles what ClickHouse must analyze for the
// read, so a read without them leaves the union out.
func (s *Store) cursorUsageRowsInBounds(ctx context.Context, f db.UsageFilter) (bool, error) {
	where, args, ok := chCursorUsageRowsWhere(f, chUsageBoundsForFilter(f))
	if !ok {
		return false, nil
	}
	var present uint8
	err := s.queryRowContext(ctx, "SELECT count() > 0 FROM cursor_usage_events cu WHERE "+where, args...).Scan(&present)
	if err != nil {
		return false, fmt.Errorf("checking clickhouse cursor usage rows: %w", err)
	}
	return present != 0, nil
}

func chDailyUsageRawSQL(f db.UsageFilter) (string, []any) {
	bounds := chUsageBoundsForFilter(f)
	sessionRowsSQL, sessionArgs := chUsageRawSQL(
		chUsageSnapshotInputFilter(f), "")
	cursorRowsSQL, cursorArgs, ok := chCursorUsageRowsSQLForBounds(f, bounds)
	if !ok {
		return sessionRowsSQL, sessionArgs
	}
	rowsSQL := sessionRowsSQL + "\n\t\tUNION ALL\n" + cursorRowsSQL
	args := make([]any, 0, len(sessionArgs)+len(cursorArgs))
	args = append(args, sessionArgs...)
	args = append(args, cursorArgs...)
	return rowsSQL, args
}

func chSQLString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func chUsageLocalDateSQL(f db.UsageFilter) (string, any) {
	if f.Timezone != "" {
		return "if(ts IS NULL, '', formatDateTime(ts, '%Y-%m-%d', ?))", f.Timezone
	}
	ref := time.Now().UTC()
	if f.From != "" {
		if t, err := time.Parse(time.RFC3339, f.From+"T12:00:00Z"); err == nil {
			ref = t
		}
	}
	_, offset := ref.In(time.Local).Zone() //nolint:forbidigo // Usage reports group UTC timestamps into local calendar dates when no timezone is selected.
	return "if(ts IS NULL, '', formatDateTime(ts + toIntervalSecond(?), '%Y-%m-%d', 'UTC'))", offset
}

// chPreparedUsageRawSQL renders usage_raw from prepared_usage instead of
// messages and usage_events. Prepared rows already hold the normalized
// counters, so they take the place of the token JSON and the raw counters;
// the normalization step is idempotent on them. Session columns still come
// from the live sessions row, exactly as the raw form joins them.
func chPreparedUsageRawSQL(state preparedUsageState, f db.UsageFilter, sessionID string) (string, []any) {
	// Model and session filters apply after Claude snapshot attribution
	// (usage_snapshot_filtered), exactly as the raw source does: a filtered
	// session's facts may belong to another session in the same request.
	inputFilter := chUsageSnapshotInputFilter(f)
	bounds := chUsageBoundsForFilter(inputFilter)
	rangeWhere, rangeArgs := appendChUsageColumnBounds("1", "ts", bounds, nil)
	rangeWhere, rangeArgs = appendChUsageColumnBounds(rangeWhere, chPreparedUsageKeyColumn, bounds, rangeArgs)
	sourceSQL, args := chPreparedUsageSourceSQL(state, rangeWhere, rangeArgs)
	where := "s.deleted_at IS NULL"
	where, args = appendChUsageSessionFilterClauses(where, args, inputFilter, sessionID)
	return `
		SELECT p.session_id AS session_id, p.message_ordinal AS message_ordinal,
			p.source AS source, p.ts AS ts, p.pricing_ts AS pricing_ts,
			p.model AS model, p.provider_id AS provider_id, '' AS token_json,
			p.claude_message_id AS claude_message_id,
			p.claude_request_id AS claude_request_id,
			p.source_uuid AS source_uuid,
			p.usage_dedup_key AS usage_dedup_key,
			p.input_tokens_norm AS input_tokens, p.output_tokens_norm AS output_tokens,
			p.cache_create_norm AS cache_create, p.cache_read_norm AS cache_read,
			p.reasoning_tokens_norm AS reasoning_tokens,
			p.cache_create_1h_norm AS cache_create_1h,
			p.web_search_requests_norm AS web_search_requests,
			p.cost_microdollars AS cost_microdollars, p.cost_source AS cost_source,
			s.project AS project, s.agent AS agent, s.machine AS machine,
			s.user_message_count AS user_message_count, s.is_automated AS is_automated,
			ifNull(COALESCE(s.display_name, s.session_name, s.first_message, s.project, s.id), '') AS display_name,
			s.started_at AS started_at,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS activity_at,
			p.price_model AS stored_price_model, p.price_key AS stored_price_key,
			` + chUsageStoredPriceSelect("p.") + `
		FROM (` + sourceSQL + `) p
		JOIN sessions s ON s.id = p.session_id
		WHERE ` + where, args
}

// chPreparedUsageSourceSQL renders the prepared rows a read may use under
// where, which names prepared columns without a prefix: the stored rows of
// every session whose snapshot is the one they were derived from, and rows
// prepared now for the sessions pushed since the refresh. The two sets are
// disjoint, so their union is what a refresh would store for the current
// snapshots. The session lists travel as external tables; see
// withUsageDeltaTables.
func chPreparedUsageSourceSQL(state preparedUsageState, where string, whereArgs []any) (string, []any) {
	query := "SELECT " + chPreparedUsageRowColumns + " FROM prepared_usage WHERE " + where
	args := slices.Clone(whereArgs)
	if len(state.replaced()) > 0 {
		query += " AND session_id NOT IN (SELECT id FROM usage_replaced_sessions)"
	}
	if len(state.changed) > 0 {
		query += "\n\t\tUNION ALL\n\t\tSELECT " + chPreparedUsageRowColumns + " FROM usage_delta_rows WHERE " + where
		args = append(args, whereArgs...)
	}
	return query, args
}

// usageSessionListTable is an external table of session ids under name.
func usageSessionListTable(name string, ids []string) (*ext.Table, error) {
	table, err := ext.NewTable(name, ext.Column("id", "String"))
	if err != nil {
		return nil, fmt.Errorf("creating %s table: %w", name, err)
	}
	for _, id := range ids {
		if err := table.Append(id); err != nil {
			return nil, fmt.Errorf("adding %s session: %w", name, err)
		}
	}
	return table, nil
}

// chExternalColumn declares an external table column of a type named as a
// string. The column type's Go type is inferred from ext.Column, so this
// package does not import the driver's column package, which the compile
// graph fixed before patching does not provide.
func chExternalColumn[T ~string](declare func(string, T) func(*ext.Table) error, name, typ string) func(*ext.Table) error {
	return declare(name, T(typ))
}

// withUsageDeltaTables attaches the replaced session list and the prepared
// delta rows a prepared read's SQL names, when the state has them, to the
// context the read runs under.
func withUsageDeltaTables(ctx context.Context, state preparedUsageState) (context.Context, error) {
	if !state.ready {
		return ctx, nil
	}
	var tables []*ext.Table
	if replaced := state.replaced(); len(replaced) > 0 {
		table, err := usageSessionListTable("usage_replaced_sessions", replaced)
		if err != nil {
			return nil, err
		}
		tables = append(tables, table)
	}
	if len(state.changed) > 0 {
		columns := make([]func(*ext.Table) error, 0, len(chPreparedUsageDeltaColumns))
		for _, c := range chPreparedUsageDeltaColumns {
			columns = append(columns, chExternalColumn(ext.Column, c.name, c.typ))
		}
		table, err := ext.NewTable("usage_delta_rows", columns...)
		if err != nil {
			return nil, fmt.Errorf("creating usage_delta_rows table: %w", err)
		}
		for _, row := range state.deltaRows {
			if err := table.Append(row...); err != nil {
				return nil, fmt.Errorf("adding usage_delta_rows row: %w", err)
			}
		}
		tables = append(tables, table)
	}
	if len(tables) == 0 {
		return ctx, nil
	}
	return chdriver.Context(ctx, chdriver.WithExternalTable(tables...)), nil
}

// chUsageStoredPriceSelect selects the stored price columns from prefix.
func chUsageStoredPriceSelect(prefix string) string {
	columns := make([]string, 0, len(chUsageStoredPriceColumns))
	for _, column := range chUsageStoredPriceColumns {
		columns = append(columns, prefix+column.stored+" AS "+column.stored)
	}
	return strings.Join(columns, ", ")
}

// chUsageStoredPriceZeroSelect gives rows from another table the stored
// price columns' shape; their values are ignored in favor of the join.
func chUsageStoredPriceZeroSelect() string {
	columns := make([]string, 0, len(chUsageStoredPriceColumns)+2)
	columns = append(columns, "'' AS stored_price_model", "'' AS stored_price_key")
	for _, column := range chUsageStoredPriceColumns {
		columns = append(columns, column.zero+" AS "+column.stored)
	}
	return strings.Join(columns, ", ")
}

// chUsageSource is a usage CTE and how its rows were sourced.
type chUsageSource struct {
	cte  string
	args []any
	// storedPricesDigest is the pricing digest whose records the rows carry
	// in their stored price columns; empty for raw rows.
	storedPricesDigest string
	// cursorRows reports whether the CTE unions cursor usage rows, which
	// carry no stored price record.
	cursorRows bool
}

// chPreparedUsageCTE is chUsageCTE over prepared usage rows.
func chPreparedUsageCTE(state preparedUsageState, f db.UsageFilter, sessionID string) (string, []any) {
	rawSQL, args := chPreparedUsageRawSQL(state, f, sessionID)
	return chUsageCTEFromRawSource(f, rawSQL, args, true, chPreparedUsageNormSource())
}

// chPreparedDailyUsageCTE is chDailyUsageCTE over prepared usage rows.
func chPreparedDailyUsageCTE(state preparedUsageState, f db.UsageFilter, includeCursor bool) (string, []any, bool) {
	sessionRowsSQL, sessionArgs := chPreparedUsageRawSQL(state, f, "")
	cursorRowsSQL, cursorArgs, ok := chCursorUsageRowsSQLForBounds(f, chUsageBoundsForFilter(f))
	ok = ok && includeCursor
	if ok {
		sessionRowsSQL += "\n\t\tUNION ALL\n\t\tSELECT c.*, " + chUsageStoredPriceZeroSelect() +
			"\n\t\tFROM (" + cursorRowsSQL + ") c"
		sessionArgs = append(sessionArgs, cursorArgs...)
	}
	cte, args := chUsageCTEFromRawSource(f, sessionRowsSQL, sessionArgs, true, chPreparedUsageNormSource())
	return cte, args, ok
}

// usageCTEFor selects the prepared form once every archive has published
// complete snapshots and the refresh has run; until then reads use the raw
// messages and usage events. A single-session read stays raw: prepared
// usage is ordered by time, not session, and one session is cheap by key.
func usageCTEFor(state preparedUsageState, f db.UsageFilter, sessionID string) chUsageSource {
	if sessionID == "" && state.ready {
		cte, args := chPreparedUsageCTE(state, f, "")
		return chUsageSource{cte: cte, args: args, storedPricesDigest: state.pricingDigest}
	}
	cte, args := chUsageCTE(f, sessionID)
	return chUsageSource{cte: cte, args: args}
}

func (s *Store) dailyUsageCTE(ctx context.Context, state preparedUsageState, f db.UsageFilter) (chUsageSource, error) {
	if state.ready {
		includeCursor, err := s.cursorUsageRowsInBounds(ctx, f)
		if err != nil {
			return chUsageSource{}, err
		}
		cte, args, cursorRows := chPreparedDailyUsageCTE(state, f, includeCursor)
		return chUsageSource{cte: cte, args: args, storedPricesDigest: state.pricingDigest, cursorRows: cursorRows}, nil
	}
	cte, args := chDailyUsageCTE(f)
	return chUsageSource{cte: cte, args: args}, nil
}

func chUsageCTE(f db.UsageFilter, sessionID string) (string, []any) {
	rawSQL, args := chUsageRawSQL(
		chUsageSnapshotInputFilter(f), sessionID)
	return chUsageCTEFromRaw(f, rawSQL, args, true)
}

func chDailyUsageCTE(f db.UsageFilter) (string, []any) {
	rawSQL, args := chDailyUsageRawSQL(f)
	return chUsageCTEFromRaw(f, rawSQL, args, true)
}

func chUsageSnapshotInputFilter(f db.UsageFilter) db.UsageFilter {
	return db.UsageFilter{From: f.From, To: f.To, Timezone: f.Timezone}
}

func chPriceModelCaseSQL() string {
	var b strings.Builder
	b.WriteString("CASE\n")
	for _, alias := range pricingpkg.FixedPricingAliases() {
		modelExpr := "replaceRegexpOne(model, '^.*/', '')"
		if alias.Exact {
			modelExpr = "model"
		}
		fmt.Fprintf(&b,
			"\t\tWHEN %s = %s THEN %s\n",
			modelExpr, chSQLString(alias.Name), chSQLString(alias.Canonical),
		)
	}
	aliases := pricingpkg.DateAliasedModels()
	quoted := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		quoted = append(quoted, chSQLString(alias))
	}
	cutoff := pricingpkg.KimiModelEraCutoff.UTC().Format("2006-01-02 15:04:05")
	fmt.Fprintf(&b, `		WHEN replaceRegexpOne(model, '^.*/', '') IN (%[1]s)
			AND (pricing_ts IS NULL OR pricing_ts >= toDateTime64(%[2]s, 6, 'UTC'))
			THEN %[3]s
		WHEN replaceRegexpOne(model, '^.*/', '') IN (%[1]s)
			THEN %[4]s
		ELSE model
	END`,
		strings.Join(quoted, ", "), chSQLString(cutoff),
		chSQLString(pricingpkg.KimiK3Canonical),
		chSQLString(pricingpkg.KimiK26Canonical),
	)
	return b.String()
}

func chClampedJSONInt(jsonExpr string, keys ...string) string {
	args := make([]string, 0, 1+len(keys))
	args = append(args, jsonExpr)
	for _, key := range keys {
		args = append(args, chSQLString(key))
	}
	extract := "JSONExtractInt(" + strings.Join(args, ", ") + ")"
	return fmt.Sprintf(
		"least(greatest(%s, toInt64(0)), toInt64(%d))",
		extract, db.MaxPlausibleTokens,
	)
}

// chUsageNormSource supplies the token expressions usage_normalized derives
// for a message row. Raw rows carry the message's token JSON; prepared rows
// already carry the normalized counters.
// chUsageNormSource holds the per-row expressions usage_normalized computes
// for message rows, and the columns survivors must carry past attribution.
type chUsageNormSource struct {
	input, output, cacheCreate, cacheCreate1h, cacheRead, reasoning, web string
	priceModel, priceKey                                                 string
	passthrough                                                          []string
}

func chUsageRawNormSource() chUsageNormSource {
	return chUsageNormSource{
		input:         chClampedJSONInt("token_json", "input_tokens"),
		output:        chClampedJSONInt("token_json", "output_tokens"),
		cacheCreate:   chClampedJSONInt("token_json", "cache_creation_input_tokens"),
		cacheCreate1h: chClampedJSONInt("token_json", "cache_creation", "ephemeral_1h_input_tokens"),
		cacheRead:     chClampedJSONInt("token_json", "cache_read_input_tokens"),
		reasoning:     chClampedJSONInt("token_json", "reasoning_tokens"),
		web:           "greatest(JSONExtractInt(token_json, 'server_tool_use', 'web_search_requests'), toInt64(0))",
		priceModel:    chPriceModelCaseSQL(),
		priceKey:      chUsagePriceKeySQL,
	}
}

// chPreparedUsageNormSource reads the counters chPreparedUsageRawSQL exposes
// under the raw column names; they are already normalized.
// Prepared rows also carry their price model, price key, and price record;
// cursor rows, which join the union from their own table, still compute
// the first two here and are priced by the per-request join.
func chPreparedUsageNormSource() chUsageNormSource {
	passthrough := make([]string, 0, len(chUsageStoredPriceColumns))
	for _, column := range chUsageStoredPriceColumns {
		passthrough = append(passthrough, column.stored)
	}
	return chUsageNormSource{
		input: "input_tokens", output: "output_tokens",
		cacheCreate: "cache_create", cacheCreate1h: "cache_create_1h",
		cacheRead: "cache_read", reasoning: "reasoning_tokens",
		web:         "web_search_requests",
		priceModel:  "if(source = 'cursor', " + chPriceModelCaseSQL() + ", stored_price_model)",
		priceKey:    "if(source = 'cursor', " + chUsagePriceKeySQL + ", stored_price_key)",
		passthrough: passthrough,
	}
}

func chUsageCTEFromRaw(
	f db.UsageFilter, rawSQL string, args []any,
	preferCompleteClaudeSnapshots bool,
) (string, []any) {
	return chUsageCTEFromRawSource(f, rawSQL, args, preferCompleteClaudeSnapshots, chUsageRawNormSource())
}

func chUsageCTEFromRawSource(
	f db.UsageFilter, rawSQL string, args []any,
	preferCompleteClaudeSnapshots bool, norms chUsageNormSource,
) (string, []any) {
	localDateSQL, localDateArg := chUsageLocalDateSQL(f)
	priceModelSQL := norms.priceModel
	var passthrough strings.Builder
	for _, column := range norms.passthrough {
		passthrough.WriteString("\n\t\t\t\tranked." + column + ",")
	}
	datePred := "1"
	var dateArgs []any
	if f.From != "" {
		datePred += " AND local_date >= ?"
		dateArgs = append(dateArgs, f.From)
	}
	if f.To != "" {
		datePred += " AND local_date <= ?"
		dateArgs = append(dateArgs, f.To)
	}
	snapshotCTE := ""
	rankedSource := "usage_normalized"
	var snapshotFilterArgs []any
	if preferCompleteClaudeSnapshots {
		snapshotFilter := "1"
		snapshotFilter, snapshotFilterArgs = appendChUsageSourceFilterClauses(
			snapshotFilter, snapshotFilterArgs, "survivor.model", f)
		snapshotFilter, snapshotFilterArgs = appendChUsageSessionFilterClauses(
			snapshotFilter, snapshotFilterArgs, f, "")
		snapshotCTE = `,
		usage_snapshot_ranked AS (
			SELECT *,
				CASE
					WHEN claude_message_id != '' AND claude_request_id != ''
						THEN first_value(session_id) OVER (
							PARTITION BY claude_message_id, claude_request_id
							ORDER BY ts ASC, session_id ASC,
								COALESCE(message_ordinal, -1) ASC
						)
					ELSE session_id
				END AS snapshot_attribution_session_id,
				CASE
					WHEN claude_message_id != '' AND claude_request_id != ''
						THEN row_number() OVER (
							PARTITION BY claude_message_id, claude_request_id
							ORDER BY output_tokens_norm DESC, ts DESC,
								session_id DESC,
								COALESCE(message_ordinal, -1) DESC
						)
					ELSE toUInt64(1)
				END AS snapshot_rank,
				CASE
					WHEN claude_message_id != '' AND claude_request_id != ''
						THEN sum(output_tokens_norm) OVER (
							PARTITION BY claude_message_id, claude_request_id
						) - max(output_tokens_norm) OVER (
							PARTITION BY claude_message_id, claude_request_id
						)
					ELSE toInt64(0)
				END AS snapshot_deduplicated_output_tokens,
				CASE
					WHEN claude_message_id != '' AND claude_request_id != ''
						THEN max(web_search_requests_norm) OVER (
							PARTITION BY claude_message_id, claude_request_id
						)
					ELSE web_search_requests_norm
				END AS snapshot_web_search_requests
			FROM usage_normalized
		),
		usage_snapshot_survivors AS (
			SELECT
				ranked.snapshot_attribution_session_id AS session_id,
				ranked.message_ordinal,
				ranked.source,
				ranked.ts,
				ranked.pricing_ts,
				ranked.model,
				ranked.provider_id,
				ranked.token_json,
				ranked.claude_message_id,
				ranked.claude_request_id,
				ranked.source_uuid,
				ranked.usage_dedup_key,
				ranked.input_tokens,
				ranked.output_tokens,
				ranked.cache_create,
				ranked.cache_read,
				ranked.reasoning_tokens,
				ranked.cost_microdollars,
				ranked.cost_source,
				ranked.input_tokens_norm,
				ranked.output_tokens_norm,
				ranked.cache_create_norm,
				ranked.cache_create_1h_norm,
				ranked.cache_read_norm,
				ranked.reasoning_tokens_norm,
				ranked.snapshot_web_search_requests AS web_search_requests_norm,
				ranked.dedup_group,
				ranked.local_date,
				ranked.price_model,
				ranked.price_key,` + passthrough.String() + `
				ranked.snapshot_deduplicated_output_tokens,
				if(attributed.id = '', ranked.project, attributed.project) AS project,
				if(attributed.id = '', ranked.agent, attributed.agent) AS agent,
				if(attributed.id = '', ranked.machine, attributed.machine) AS machine,
				if(attributed.id = '', ranked.user_message_count, attributed.user_message_count) AS user_message_count,
				if(attributed.id = '', ranked.is_automated, attributed.is_automated) AS is_automated,
				if(attributed.id = '', ranked.display_name, ifNull(COALESCE(
					attributed.display_name, attributed.session_name,
					attributed.first_message, attributed.project, attributed.id
				), '')) AS display_name,
				if(attributed.id = '', ranked.started_at, attributed.started_at) AS started_at,
				if(attributed.id = '', ranked.activity_at, COALESCE(
					attributed.ended_at, attributed.started_at,
					attributed.created_at
				)) AS activity_at
			FROM usage_snapshot_ranked ranked
			LEFT JOIN sessions attributed
				ON attributed.id = ranked.snapshot_attribution_session_id
			WHERE ranked.snapshot_rank = 1
		),
		usage_snapshot_filtered AS (
			SELECT survivor.*
			FROM usage_snapshot_survivors survivor
			LEFT JOIN sessions s ON s.id = survivor.session_id
			WHERE survivor.session_id = '' OR (` + snapshotFilter + `)
		)`
		rankedSource = "usage_snapshot_filtered"
	}
	maxTok := db.MaxPlausibleTokens
	query := fmt.Sprintf(`
		WITH usage_raw AS (
			%[1]s
		),
		usage_normalized AS (
			SELECT *,
				if(source = 'message', %[8]s,
					if(source = 'session', greatest(input_tokens, toInt64(0)),
						least(greatest(input_tokens, toInt64(0)), toInt64(%[4]d)))) AS input_tokens_norm,
				if(source = 'message', %[9]s,
					if(source = 'session', greatest(output_tokens, toInt64(0)),
						least(greatest(output_tokens, toInt64(0)), toInt64(%[4]d)))) AS output_tokens_norm,
				if(source = 'message', %[10]s,
					if(source = 'session', greatest(cache_create, toInt64(0)),
						least(greatest(cache_create, toInt64(0)), toInt64(%[4]d)))) AS cache_create_norm,
				if(source = 'message', %[11]s, toInt64(0)) AS cache_create_1h_norm,
				if(source = 'message', %[12]s,
					if(source = 'session', greatest(cache_read, toInt64(0)),
						least(greatest(cache_read, toInt64(0)), toInt64(%[4]d)))) AS cache_read_norm,
				if(source = 'message', %[13]s,
					if(source = 'session', greatest(reasoning_tokens, toInt64(0)),
						least(greatest(reasoning_tokens, toInt64(0)), toInt64(%[4]d)))) AS reasoning_tokens_norm,
				if(source = 'message', %[15]s, toInt64(0)) AS web_search_requests_norm,
				if(claude_message_id != '' AND claude_request_id != '',
					concat('claude:', claude_message_id, ':', claude_request_id),
					if(source = 'message' AND agent != '' AND source_uuid != '',
						concat('source:', agent, ':', source_uuid),
						if(usage_dedup_key != '',
							concat('usage:', usage_dedup_key),
							concat('row:', session_id, ':', source, ':',
								ifNull(toString(message_ordinal), ''), ':',
								ifNull(toString(ts), ''), ':', model)
						)
					)
				) AS dedup_group,
				%[2]s AS local_date,
				%[5]s AS price_model,
				%[14]s AS price_key
			FROM usage_raw
			WHERE %[3]s
		)%[6]s,
		usage_localized AS (
			SELECT *,
				row_number() OVER (
					PARTITION BY dedup_group
					ORDER BY ts ASC, session_id ASC,
						COALESCE(message_ordinal, -1) ASC
				) AS dedup_rank
			FROM %[7]s
			QUALIFY dedup_rank = 1
		)`, rawSQL, localDateSQL, datePred, maxTok,
		priceModelSQL, snapshotCTE, rankedSource,
		norms.input, norms.output, norms.cacheCreate, norms.cacheCreate1h,
		norms.cacheRead, norms.reasoning,
		norms.priceKey, norms.web,
	)
	args = append(args, localDateArg)
	args = append(args, dateArgs...)
	args = append(args, snapshotFilterArgs...)
	return query, args
}

type chUsageBucket struct {
	inputTok  int
	outputTok int
	cacheCr   int
	cacheRd   int
	cost      money.Money
}

type chUsageAggregateRow struct {
	date                  string
	ts                    string
	pricingTS             string
	sessionID             string
	project               string
	agent                 string
	machine               string
	model                 string
	providerID            string
	priceModel            string
	source                string
	messageOrdinal        sql.NullInt64
	displayName           string
	startedAt             string
	inputTok              int
	outputTok             int
	cacheCr               int
	cacheCr1h             int
	cacheRd               int
	billableInput         int
	billableOutput        int
	billableReason        int
	billableCacheCr       int
	billableCacheCr1h     int
	billableCacheRd       int
	billableWebSearch     int
	explicitCost          int64
	reportedCostRows      int
	authoritativeCost     int64
	authoritativeCostRows int
	snapshotDedupOutput   int
}

type chSessionUsageRow struct {
	sessionID         string
	messageOrdinal    sql.NullInt64
	source            string
	ts                string
	pricingTS         string
	model             string
	providerID        string
	inputTok          int
	outputTok         int
	cacheCr           int
	cacheCr1h         int
	cacheRd           int
	reasoningTok      int
	webSearchRequests int
	cost              sql.NullInt64
	costSource        string
}

func chUsageLookupModel(model, ts string) string {
	timestamp, _ := parseAnalyticsTime(ts)
	if canonical := pricingpkg.CanonicalModelForDate(model, timestamp); canonical != "" {
		return canonical
	}
	return model
}

func chUsagePricingTimestamp(ts string) time.Time {
	timestamp, _ := parseAnalyticsTime(ts)
	return timestamp
}

func chSessionUsageLookupModel(r chSessionUsageRow) string {
	return chUsageLookupModel(r.model, r.pricingTS)
}

func chUsageAggregateResolvedCost(
	reportedModel, canonicalModel, providerID string, pricingTimestamp time.Time,
	inputTok, outputTok, cacheCr, cacheCr1h, cacheRd int,
	billableInput, billableOutput, billableReasoning, billableCacheCr, billableCacheCr1h, billableCacheRd int,
	billableWebSearchRequests int,
	explicitCost int64,
	hasReportedCost bool,
	requestScoped bool,
	pricing *export.PricingResolver,
) (money.Money, money.Money, bool, bool, error) {
	pricedModel, lookup := pricing.ResolveAt(
		reportedModel, canonicalModel, pricingTimestamp,
	)
	var err error
	hasBillableTokens := billableInput != 0 || billableOutput != 0 ||
		billableReasoning != 0 || billableCacheCr != 0 || billableCacheRd != 0
	hasComputedUsage := hasBillableTokens || billableWebSearchRequests > 0
	if !hasReportedCost &&
		explicitCost == 0 &&
		inputTok == 0 && outputTok == 0 && cacheCr == 0 && cacheRd == 0 &&
		!hasBillableTokens && billableWebSearchRequests == 0 {
		pricing.RecordResolvedComputed(reportedModel, pricedModel, lookup)
		return money.Money{}, money.Money{}, true, false, nil
	}
	if !hasReportedCost {
		pricedModel, lookup, err = pricing.ResolveBilledAt(
			providerID, reportedModel, canonicalModel, pricingTimestamp)
		if err != nil {
			return money.Money{}, money.Money{}, false, false, err
		}
	}
	rates := lookup.Rates
	computed, err := rates.CostForTokensScoped(
		requestScoped,
		billableInput, billableOutput, billableReasoning,
		billableCacheCr, billableCacheCr1h, billableCacheRd)
	if err != nil {
		return money.Money{}, money.Money{}, false, false,
			fmt.Errorf("pricing clickhouse usage for model %q: %w", reportedModel, err)
	}
	computed, err = export.AddWebSearchFee(
		computed, billableWebSearchRequests)
	if err != nil {
		return money.Money{}, money.Money{}, false, false,
			fmt.Errorf("pricing clickhouse usage for model %q: %w", reportedModel, err)
	}
	cost, err := money.Add(
		money.Money{Microdollars: explicitCost}, computed)
	if err != nil {
		return money.Money{}, money.Money{}, false, false,
			fmt.Errorf("summing clickhouse usage for model %q: %w", reportedModel, err)
	}
	if hasReportedCost {
		pricing.RecordResolvedReported(reportedModel, pricedModel, lookup)
	}
	if hasComputedUsage {
		chRecordComputedUsagePricing(
			pricing, reportedModel, pricedModel, lookup, requestScoped,
			billableInput, billableCacheCr, billableCacheRd,
		)
	}
	selectedRates := rates
	if requestScoped {
		selectedRates = rates.RatesForTokens(inputTok, cacheCr, cacheRd)
	}
	savingsRates := selectedRates
	if hasReportedCost && (cacheCr != 0 || cacheRd != 0) {
		_, savingsLookup, err := pricing.ResolveBilledAt(
			providerID, reportedModel, canonicalModel, pricingTimestamp)
		if err != nil {
			return money.Money{}, money.Money{}, false, false,
				fmt.Errorf("pricing clickhouse reported usage cache savings for model %q: %w", reportedModel, err)
		}
		savingsRates = savingsLookup.Rates
		if requestScoped {
			savingsRates = savingsRates.RatesForTokens(inputTok, cacheCr, cacheRd)
		}
	}
	readRate, err := money.Sub(
		savingsRates.InputPerMTok, savingsRates.CacheReadPerMTok)
	if err != nil {
		return money.Money{}, money.Money{}, false, false,
			fmt.Errorf("deriving clickhouse cache read rate for model %q: %w", reportedModel, err)
	}
	creationRate, err := money.Sub(
		savingsRates.InputPerMTok, savingsRates.CacheWritePerMTok)
	if err != nil {
		return money.Money{}, money.Money{}, false, false,
			fmt.Errorf("deriving clickhouse cache creation rate for model %q: %w", reportedModel, err)
	}
	creation1hRate, err := money.Sub(
		savingsRates.InputPerMTok, savingsRates.EffectiveCacheWrite1hPerMTok())
	if err != nil {
		return money.Money{}, money.Money{}, false, false,
			fmt.Errorf("deriving clickhouse 1h cache creation rate for model %q: %w", reportedModel, err)
	}
	if cacheCr1h > cacheCr {
		cacheCr1h = cacheCr
	}
	savings, err := money.SignedCostPerMillion([]money.RatedTokens{
		{Tokens: int64(cacheRd), Rate: readRate},
		{Tokens: int64(cacheCr - cacheCr1h), Rate: creationRate},
		{Tokens: int64(cacheCr1h), Rate: creation1hRate},
	})
	if err != nil {
		return money.Money{}, money.Money{}, false, false,
			fmt.Errorf("pricing clickhouse cache savings for model %q: %w", reportedModel, err)
	}
	priced := lookup.OK
	if !hasBillableTokens && hasReportedCost {
		priced = true
	}
	return cost, savings, priced, true, nil
}

func chRecordComputedUsagePricing(
	pricing *export.PricingResolver,
	reportedModel, pricedModel string,
	lookup export.PricingLookup,
	requestScoped bool,
	inputTokens, cacheWriteTokens, cacheReadTokens int,
) {
	if requestScoped {
		pricing.RecordResolvedComputedRequest(
			reportedModel, pricedModel, lookup,
			inputTokens, cacheWriteTokens, cacheReadTokens)
		return
	}
	pricing.RecordResolvedComputedAggregate(reportedModel, pricedModel, lookup)
}

func chSessionUsageRowCost(
	r chSessionUsageRow, pricing *export.PricingResolver,
) (money.Money, bool, bool, error) {
	if r.cost.Valid && r.costSource != db.CopilotReportedCostSource {
		return money.Money{Microdollars: r.cost.Int64}, true, true, nil
	}
	if r.inputTok == 0 && r.outputTok == 0 && r.reasoningTok == 0 &&
		r.cacheCr == 0 && r.cacheRd == 0 && r.webSearchRequests == 0 {
		return money.Money{}, true, false, nil
	}
	_, lookup := pricing.ResolveAt(
		r.model, chSessionUsageLookupModel(r),
		chUsagePricingTimestamp(r.pricingTS),
	)
	if !lookup.OK {
		fee, feeErr := export.WebSearchFee(r.webSearchRequests)
		if feeErr != nil {
			return money.Money{}, false, false, feeErr
		}
		return fee, false, true, nil
	}
	_, lookup, err := pricing.ResolveBilledAt(
		r.providerID, r.model, chSessionUsageLookupModel(r),
		chUsagePricingTimestamp(r.pricingTS))
	if err != nil {
		return money.Money{}, false, false, err
	}
	requestScoped := db.UsageSourceIsRequestScoped(r.source) || r.messageOrdinal.Valid
	cost, err := lookup.Rates.CostForTokensScoped(
		requestScoped,
		r.inputTok, r.outputTok, r.reasoningTok, r.cacheCr, r.cacheCr1h,
		r.cacheRd,
	)
	if err != nil {
		return money.Money{}, false, false,
			fmt.Errorf("pricing clickhouse session usage for model %q: %w", r.model, err)
	}
	cost, err = export.AddWebSearchFee(cost, r.webSearchRequests)
	if err != nil {
		return money.Money{}, false, false,
			fmt.Errorf("pricing clickhouse session usage for model %q: %w", r.model, err)
	}
	return cost, true, true, nil
}

func chSessionUsageBreakdownEntry(
	r chSessionUsageRow,
	ordinal int,
	cost money.Money,
	priced bool,
) db.SessionUsageBreakdownEntry {
	entry := db.SessionUsageBreakdownEntry{
		Ordinal:                  ordinal,
		Source:                   r.source,
		Label:                    chSessionUsageBreakdownLabel(r),
		Timestamp:                r.ts,
		Model:                    r.model,
		InputTokens:              r.inputTok,
		OutputTokens:             r.outputTok,
		CacheCreationInputTokens: r.cacheCr,
		CacheReadInputTokens:     r.cacheRd,
		WebSearchRequests:        r.webSearchRequests,
		Cost:                     cost,
		HasCost:                  priced,
	}
	if r.messageOrdinal.Valid {
		messageOrdinal := int(r.messageOrdinal.Int64)
		entry.MessageOrdinal = &messageOrdinal
	}
	return entry
}

func chSessionUsageBreakdownLabel(r chSessionUsageRow) string {
	var ordinal *int
	if r.messageOrdinal.Valid {
		v := int(r.messageOrdinal.Int64)
		ordinal = &v
	}
	return db.SessionUsageBreakdownLabel(ordinal, r.source)
}

const chUsageBillableSelect = `
			CASE WHEN cost_microdollars IS NULL OR cost_source = 'copilot-reported' THEN input_tokens_norm ELSE 0 END AS billable_input_tokens,
			CASE
				WHEN cost_microdollars IS NOT NULL AND cost_source != 'copilot-reported' THEN 0
				WHEN output_tokens_norm = 0 THEN reasoning_tokens_norm
				ELSE output_tokens_norm
			END AS billable_output_tokens,
			toInt64(0) AS billable_reasoning_tokens,
			CASE WHEN cost_microdollars IS NULL OR cost_source = 'copilot-reported' THEN cache_create_norm ELSE 0 END AS billable_cache_creation_tokens,
			CASE WHEN cost_microdollars IS NULL OR cost_source = 'copilot-reported' THEN cache_create_1h_norm ELSE 0 END AS billable_cache_creation_1h_tokens,
			CASE WHEN cost_microdollars IS NULL OR cost_source = 'copilot-reported' THEN cache_read_norm ELSE 0 END AS billable_cache_read_tokens,
			CASE WHEN cost_microdollars IS NULL OR cost_source = 'copilot-reported' THEN web_search_requests_norm ELSE 0 END AS billable_web_search_requests,
			CASE WHEN cost_microdollars IS NOT NULL AND cost_source != 'copilot-reported' THEN cost_microdollars ELSE 0 END AS explicit_cost,
			CASE WHEN cost_microdollars IS NOT NULL AND cost_source != 'copilot-reported' THEN 1 ELSE 0 END AS reported_cost_rows,
			CASE WHEN cost_microdollars IS NOT NULL AND cost_source = 'copilot-reported' THEN cost_microdollars ELSE 0 END AS authoritative_cost,
			CASE WHEN cost_microdollars IS NOT NULL AND cost_source = 'copilot-reported' THEN 1 ELSE 0 END AS authoritative_cost_rows`

// chDailyUsageGroupRow is one row of the daily usage query. A group sums
// every event that shares a session, day, breakdown key, and pricing
// context, priced at push time. An explicit row is a single event that Go
// must still see whole: a Copilot authoritative cost, whose selection order
// decides the session's cost, an event the reader's custom rates reprice, or
// an event with no price record under the current digest. The last is an
// ordinary cache miss that Go prices for this read; only push writes records.
type chDailyUsageGroupRow struct {
	chUsageAggregateRow
	explicit   bool
	kind       string
	contextID  string
	bandAbove  int64
	events     int
	tokenCost  int64
	savings    int64
	priceError string
	overflow   bool
}

// chUsagePricedCTE attaches each row's price record under pricingDigest.
// Rows prepared under that digest already carry their record; only cursor
// rows, if any, still join the records, restricted to their own keys.
// Other rows join every record for the digest.
func chUsagePricedCTE(source chUsageSource, pricingDigest string) (string, []any) {
	columns := make([]string, 0, len(chUsageStoredPriceColumns))
	stored := source.storedPricesDigest != "" && source.storedPricesDigest == pricingDigest
	for _, column := range chUsageStoredPriceColumns {
		switch {
		case stored && source.cursorRows:
			columns = append(columns, "if(u.source = 'cursor', p."+column.joined+", u."+column.stored+") AS "+column.joined)
		case stored:
			columns = append(columns, "u."+column.stored+" AS "+column.joined)
		default:
			columns = append(columns, "p."+column.joined+" AS "+column.joined)
		}
	}
	query := `,
		usage_priced AS (
			SELECT u.*,` + chUsageBillableSelect + `,
				` + strings.Join(columns, ",\n\t\t\t\t") + `
			FROM usage_localized u`
	if stored && !source.cursorRows {
		return query + `
		)`, nil
	}
	query += `
			LEFT JOIN (
				` + chUsagePriceRowsSQL() + `
				WHERE pricing_digest = ?`
	if stored {
		query += `
					AND price_key IN (SELECT price_key FROM usage_normalized WHERE source = 'cursor')`
	}
	return query + `
			) p ON p.p_price_key = u.price_key
		)`, []any{pricingDigest}
}

func (s *Store) forEachDailyUsageGroupRow(
	ctx context.Context,
	f db.UsageFilter,
	pricingDigest string,
	customModels [][2]string,
	visit func(chDailyUsageGroupRow) error,
) error {
	// The key is taken before any read the rows depend on, so a change
	// that lands after it makes the next request miss.
	state, err := s.preparedUsageState(ctx)
	if err != nil {
		return err
	}
	memoSlot, memoVersion, err := s.usageRowMemoKey(ctx, state, "daily", f, "", pricingDigest, customModels)
	if err != nil {
		return err
	}
	if rows, ok := s.dailyUsageRows.get(memoSlot, memoVersion); ok {
		for _, r := range rows {
			if err := visit(r); err != nil {
				return err
			}
		}
		return nil
	}
	source, err := s.dailyUsageCTE(ctx, state, f)
	if err != nil {
		return err
	}
	cte, args := source.cte, source.args
	machineSelect := "''"
	if f.Breakdowns {
		machineSelect = "machine"
	}
	customPred := "0"
	var customArgs []any
	if len(customModels) > 0 {
		tuples := make([]string, len(customModels))
		for i, pair := range customModels {
			tuples[i] = "(?, ?)"
			customArgs = append(customArgs, pair[0], pair[1])
		}
		customPred = "(model, price_model) IN (" + strings.Join(tuples, ", ") + ")"
	}
	pricedSQL, pricedArgs := chUsagePricedCTE(source, pricingDigest)
	query := cte + pricedSQL + `,
		usage_classified AS (
			SELECT *,
				` + machineSelect + ` AS group_machine,
				authoritative_cost_rows = 1 OR p_priced != 1 OR ` + customPred + ` AS explicit_row,
				multiIf(
					reported_cost_rows = 1, '` + chUsagePriceKindReported + `',
					input_tokens_norm = 0 AND output_tokens_norm = 0
						AND reasoning_tokens_norm = 0 AND cache_create_norm = 0
						AND cache_read_norm = 0 AND web_search_requests_norm = 0,
						'` + chUsagePriceKindZero + `',
					p_request_scoped, '` + chUsagePriceKindRequest + `',
					'` + chUsagePriceKindAggregate + `'
				) AS price_kind
			FROM usage_priced
		)
		SELECT session_id, local_date, project, agent, group_machine, model, provider_id,
			if(explicit_row, dedup_group, '') AS explicit_key,
			if(explicit_row, '', price_kind) AS group_kind,
			if(explicit_row, '', if(
				price_kind IN ('` + chUsagePriceKindReported + `', '` + chUsagePriceKindZero + `'),
				p_unbilled_context_id, p_billed_context_id)) AS group_context,
			if(explicit_row OR price_kind != '` + chUsagePriceKindRequest + `', toInt64(-1), p_band) AS group_band,
			toInt64(count()) AS events,
			sum(input_tokens_norm), sum(output_tokens_norm),
			sum(cache_create_norm), sum(cache_read_norm),
			toInt64(sum(toInt128(p_token_cost))), toInt64(sum(toInt128(p_savings))),
			toInt64(sum(toInt128(explicit_cost))), sum(billable_web_search_requests),
			max(if(explicit_row OR price_kind = '` + chUsagePriceKindZero + `', '', p_price_error)),
			arrayExists(total -> total < toInt128('-9223372036854775808')
				OR total > toInt128('9223372036854775807'),
				[sum(toInt128(p_token_cost)), sum(toInt128(p_savings)), sum(toInt128(explicit_cost))]),
			any(price_model) AS row_price_model, any(source) AS row_source,
			any(message_ordinal) AS row_message_ordinal,
			any(ts) AS row_ts, any(pricing_ts),
			any(usage_dedup_key) AS row_usage_dedup_key,
			any(cache_create_1h_norm),
			any(billable_input_tokens), any(billable_output_tokens),
			any(billable_reasoning_tokens), any(billable_cache_creation_tokens),
			any(billable_cache_creation_1h_tokens), any(billable_cache_read_tokens),
			toInt64(any(reported_cost_rows)),
			any(authoritative_cost), toInt64(any(authoritative_cost_rows))
		FROM usage_classified
		GROUP BY session_id, local_date, project, agent, group_machine, model, provider_id,
			explicit_key, group_kind, group_context, group_band
		ORDER BY session_id ASC, local_date ASC, project ASC, agent ASC, group_machine ASC,
			model ASC, row_price_model ASC, row_ts ASC, COALESCE(row_message_ordinal, -1) ASC,
			row_source ASC, row_usage_dedup_key ASC`
	args = append(args, pricedArgs...)
	args = append(args, customArgs...)
	readCtx, err := withUsageDeltaTables(ctx, state)
	if err != nil {
		return err
	}
	s.usageReadQueries.Add(1)
	rows, err := s.queryContext(readCtx, query, args...)
	if err != nil {
		return fmt.Errorf("querying clickhouse daily usage aggregates: %w", err)
	}
	defer rows.Close()
	var memo []chDailyUsageGroupRow
	for rows.Next() {
		var r chDailyUsageGroupRow
		var explicitKey string
		var ts, pricingTS any
		if err := rows.Scan(
			&r.sessionID, &r.date, &r.project, &r.agent, &r.machine, &r.model,
			&r.providerID,
			&explicitKey, &r.kind, &r.contextID, &r.bandAbove, &r.events,
			&r.inputTok, &r.outputTok, &r.cacheCr, &r.cacheRd,
			&r.tokenCost, &r.savings,
			&r.explicitCost, &r.billableWebSearch,
			&r.priceError, &r.overflow,
			&r.priceModel, &r.source, &r.messageOrdinal, &ts, &pricingTS,
			new(string),
			&r.cacheCr1h,
			&r.billableInput, &r.billableOutput, &r.billableReason,
			&r.billableCacheCr, &r.billableCacheCr1h, &r.billableCacheRd,
			&r.reportedCostRows,
			&r.authoritativeCost, &r.authoritativeCostRows,
		); err != nil {
			return fmt.Errorf("scanning clickhouse daily usage aggregate: %w", err)
		}
		r.explicit = explicitKey != ""
		r.ts = formatDBTime(ts)
		r.pricingTS = formatDBTime(pricingTS)
		memo = append(memo, r)
		if err := visit(r); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating clickhouse daily usage aggregates: %w", err)
	}
	s.dailyUsageRows.put(memoSlot, memoVersion, memo)
	return nil
}

// chPricedUsageGroupCost turns one priced group into its cost and cache
// savings and replays the group's pricing context into the pricing block.
func chPricedUsageGroupCost(
	r chDailyUsageGroupRow,
	contexts map[string]chUsagePriceContext,
	pricingDigest string,
	pricing *export.PricingResolver,
) (money.Money, money.Money, error) {
	if r.priceError != "" {
		return money.Money{}, money.Money{}, decodeUsagePriceError(r.priceError)
	}
	if r.overflow {
		return money.Money{}, money.Money{}, fmt.Errorf(
			"summing clickhouse usage for model %q: %w", r.model, money.ErrOverflow)
	}
	priceContext, ok := contexts[r.contextID]
	if !ok {
		return money.Money{}, money.Money{}, fmt.Errorf(
			"%w: missing context %q for model %q under pricing digest %s",
			errUsagePriceContextChanged, r.contextID, r.model, pricingDigest)
	}
	if err := priceContext.record(pricing, r.kind, r.bandAbove, r.events); err != nil {
		return money.Money{}, money.Money{}, err
	}
	cost, err := money.Add(
		money.Money{Microdollars: r.explicitCost},
		money.Money{Microdollars: r.tokenCost})
	if err != nil {
		return money.Money{}, money.Money{},
			fmt.Errorf("summing clickhouse usage for model %q: %w", r.model, err)
	}
	cost, err = export.AddWebSearchFee(cost, r.billableWebSearch)
	if err != nil {
		return money.Money{}, money.Money{},
			fmt.Errorf("pricing clickhouse usage for model %q: %w", r.model, err)
	}
	return cost, money.Money{Microdollars: r.savings}, nil
}

// chCustomPricedModels returns the (model, price_model) pairs of contexts the
// reader's own custom rates can reach. Persisted prices come from the shared
// catalog, so events of these pairs are priced at request time instead.
func chCustomPricedModels(
	contexts map[string]chUsagePriceContext, pricing *export.PricingResolver,
) [][2]string {
	var out [][2]string
	seen := map[[2]string]bool{}
	for _, priceContext := range contexts {
		pair := [2]string{priceContext.ReportedModel, priceContext.CanonicalModel}
		if !seen[pair] && priceContext.customPriced(pricing) {
			seen[pair] = true
			out = append(out, pair)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		return out[i][1] < out[j][1]
	})
	return out
}

// chDailyUsageLoadAttempts bounds how often a read restarts because a push
// added price contexts between the context load and the usage query.
const chDailyUsageLoadAttempts = 3

var errUsagePriceContextChanged = errors.New("usage price context changed during read")

func (s *Store) GetDailyUsage(
	ctx context.Context, f db.UsageFilter,
) (db.DailyUsageResult, error) {
	s.recordUsageRead("daily", f, 0)
	return s.dailyUsage(ctx, f)
}

func (s *Store) dailyUsage(
	ctx context.Context, f db.UsageFilter,
) (db.DailyUsageResult, error) {
	snapshot, err := s.pricingSnapshot(ctx)
	if err != nil {
		return db.DailyUsageResult{}, err
	}
	for attempt := 1; ; attempt++ {
		result, err := s.dailyUsageForCatalog(ctx, f, snapshot)
		if !errors.Is(err, errUsagePriceContextChanged) || attempt == chDailyUsageLoadAttempts {
			return result, err
		}
	}
}

// Each attempt owns its accumulator and resolver. If a push adds a context
// during the query, retry with fresh contexts instead of retaining raw rows.
func (s *Store) dailyUsageForCatalog(
	ctx context.Context, f db.UsageFilter, snapshot *pricingSnapshot,
) (db.DailyUsageResult, error) {
	catalog := snapshot.catalog
	rateResolver := export.NewPricingResolverWithDigest(catalog.rows, snapshot.catalogDigest)
	priceContexts, err := loadUsagePriceContexts(ctx, s.conn, catalog.digest)
	if err != nil {
		return db.DailyUsageResult{}, err
	}
	var customModels [][2]string
	if len(s.customPricing) > 0 {
		customModels = chCustomPricedModels(priceContexts, rateResolver)
	}
	type usageAccumKey struct {
		date       string
		project    string
		agent      string
		machine    string
		model      string
		providerID string
	}
	accum := map[usageAccumKey]*chUsageBucket{}
	type sessionCost struct {
		estimated     map[usageAccumKey]money.Money
		authoritative *money.Money
	}
	sessionCosts := map[string]sessionCost{}
	useAuthoritativeCost := f.Model == "" && f.ExcludeModel == ""
	projectLabels := map[string]bool{}
	var seenSessions map[string]db.UsageSessionInfo
	if !f.SkipSessionCounts {
		seenSessions = map[string]db.UsageSessionInfo{}
	}
	var totalSavings money.Money
	err = s.forEachDailyUsageGroupRow(ctx, f, catalog.digest, customModels, func(r chDailyUsageGroupRow) error {
		key := usageAccumKey{
			date: r.date, project: r.project, agent: r.agent,
			machine: r.machine, model: r.model, providerID: r.providerID,
		}
		if r.project != "" {
			projectLabels[r.project] = true
		}
		if seenSessions != nil && r.sessionID != "" {
			seenSessions[r.sessionID] = db.UsageSessionInfo{
				Project: r.project,
				Agent:   r.agent,
			}
		}
		b := accum[key]
		if b == nil {
			b = &chUsageBucket{}
			accum[key] = b
		}
		var cost, savings money.Money
		var priceErr error
		if r.explicit {
			cost, savings, _, _, priceErr = chUsageAggregateResolvedCost(
				r.model, r.priceModel, r.providerID, chUsagePricingTimestamp(r.pricingTS),
				r.inputTok, r.outputTok, r.cacheCr, r.cacheCr1h, r.cacheRd,
				r.billableInput, r.billableOutput, r.billableReason,
				r.billableCacheCr, r.billableCacheCr1h, r.billableCacheRd,
				r.billableWebSearch,
				r.explicitCost,
				r.reportedCostRows > 0,
				db.UsageSourceIsRequestScoped(r.source) || r.messageOrdinal.Valid,
				rateResolver,
			)
		} else {
			cost, savings, priceErr = chPricedUsageGroupCost(
				r, priceContexts, catalog.digest, rateResolver)
		}
		if priceErr != nil {
			return priceErr
		}
		totalSavings, priceErr = money.Add(totalSavings, savings)
		if priceErr != nil {
			return fmt.Errorf(
				"summing clickhouse cache savings: %w", priceErr)
		}
		b.inputTok += r.inputTok
		b.outputTok += r.outputTok
		b.cacheCr += r.cacheCr
		b.cacheRd += r.cacheRd
		sc := sessionCosts[r.sessionID]
		if sc.estimated == nil {
			sc.estimated = map[usageAccumKey]money.Money{}
		}
		sc.estimated[key], priceErr = money.Add(sc.estimated[key], cost)
		if priceErr != nil {
			return fmt.Errorf(
				"summing clickhouse usage: %w", priceErr)
		}
		if useAuthoritativeCost && r.authoritativeCostRows > 0 {
			v := money.Money{Microdollars: r.authoritativeCost}
			sc.authoritative = &v
			rateResolver.RecordUnattributedReported()
		}
		sessionCosts[r.sessionID] = sc
		return nil
	})
	if err != nil {
		return db.DailyUsageResult{}, err
	}
	sessionIDs := make([]string, 0, len(sessionCosts))
	for sessionID := range sessionCosts {
		sessionIDs = append(sessionIDs, sessionID)
	}
	sort.Strings(sessionIDs)
	for _, sessionID := range sessionIDs {
		sc := sessionCosts[sessionID]
		if sc.authoritative != nil {
			keys := make([]usageAccumKey, 0, len(sc.estimated))
			for key := range sc.estimated {
				keys = append(keys, key)
			}
			sort.Slice(keys, func(i, j int) bool {
				a, b := keys[i], keys[j]
				if a.date != b.date {
					return a.date < b.date
				}
				if a.project != b.project {
					return a.project < b.project
				}
				if a.agent != b.agent {
					return a.agent < b.agent
				}
				if a.machine != b.machine {
					return a.machine < b.machine
				}
				return a.model < b.model
			})
			weights := make([]money.Money, len(keys))
			for i, key := range keys {
				weights[i] = sc.estimated[key]
			}
			costs := export.AllocateCostByWeight(*sc.authoritative, weights)
			for i, key := range keys {
				b := accum[key]
				if b == nil {
					b = &chUsageBucket{}
					accum[key] = b
				}
				b.cost, err = money.Add(b.cost, costs[i])
				if err != nil {
					return db.DailyUsageResult{}, fmt.Errorf(
						"summing allocated clickhouse usage cost: %w", err)
				}
			}
		} else {
			for key, cost := range sc.estimated {
				b := accum[key]
				if b == nil {
					b = &chUsageBucket{}
					accum[key] = b
				}
				b.cost, err = money.Add(b.cost, cost)
				if err != nil {
					return db.DailyUsageResult{}, fmt.Errorf(
						"summing estimated clickhouse usage cost: %w", err)
				}
			}
		}
	}

	type dayMaps struct {
		models    map[string]chUsageBucket
		projects  map[string]chUsageBucket
		agents    map[string]chUsageBucket
		machines  map[string]chUsageBucket
		totalCost money.Money
	}
	days := map[string]*dayMaps{}
	for key, b := range accum {
		day := days[key.date]
		if day == nil {
			day = &dayMaps{
				models:   map[string]chUsageBucket{},
				projects: map[string]chUsageBucket{},
				agents:   map[string]chUsageBucket{},
				machines: map[string]chUsageBucket{},
			}
			days[key.date] = day
		}
		if err := addChUsageBucket(day.models, key.model, *b); err != nil {
			return db.DailyUsageResult{}, err
		}
		day.totalCost, err = money.Add(day.totalCost, b.cost)
		if err != nil {
			return db.DailyUsageResult{}, fmt.Errorf(
				"summing clickhouse daily cost: %w", err)
		}
		if f.Breakdowns {
			if err := addChUsageBucket(day.projects, key.project, *b); err != nil {
				return db.DailyUsageResult{}, err
			}
			if err := addChUsageBucket(day.agents, key.agent, *b); err != nil {
				return db.DailyUsageResult{}, err
			}
			if err := addChUsageBucket(day.machines, key.machine, *b); err != nil {
				return db.DailyUsageResult{}, err
			}
		}
	}

	var result db.DailyUsageResult
	for _, date := range sortedKeys(days) {
		day := days[date]
		if day == nil {
			continue
		}
		entry := db.DailyUsageEntry{Date: date}
		modelNames := sortedChUsageBucketKeys(day.models)
		entry.ModelsUsed = modelNames
		for _, model := range modelNames {
			b := day.models[model]
			entry.InputTokens += b.inputTok
			entry.OutputTokens += b.outputTok
			entry.CacheCreationTokens += b.cacheCr
			entry.CacheReadTokens += b.cacheRd
			entry.ModelBreakdowns = append(entry.ModelBreakdowns, db.ModelBreakdown{
				ModelName:           model,
				InputTokens:         b.inputTok,
				OutputTokens:        b.outputTok,
				CacheCreationTokens: b.cacheCr,
				CacheReadTokens:     b.cacheRd,
				Cost:                b.cost,
			})
		}
		entry.TotalCost = day.totalCost
		if f.Breakdowns {
			for _, project := range sortedChUsageBucketKeys(day.projects) {
				b := day.projects[project]
				entry.ProjectBreakdowns = append(entry.ProjectBreakdowns, db.ProjectBreakdown{
					Project:             project,
					InputTokens:         b.inputTok,
					OutputTokens:        b.outputTok,
					CacheCreationTokens: b.cacheCr,
					CacheReadTokens:     b.cacheRd,
					Cost:                b.cost,
				})
			}
			for _, agent := range sortedChUsageBucketKeys(day.agents) {
				b := day.agents[agent]
				entry.AgentBreakdowns = append(entry.AgentBreakdowns, db.AgentBreakdown{
					Agent:               agent,
					InputTokens:         b.inputTok,
					OutputTokens:        b.outputTok,
					CacheCreationTokens: b.cacheCr,
					CacheReadTokens:     b.cacheRd,
					Cost:                b.cost,
				})
			}
			for _, machine := range sortedChUsageBucketKeys(day.machines) {
				b := day.machines[machine]
				entry.MachineBreakdowns = append(
					entry.MachineBreakdowns,
					db.MachineBreakdown{
						MachineName:         machine,
						InputTokens:         b.inputTok,
						OutputTokens:        b.outputTok,
						CacheCreationTokens: b.cacheCr,
						CacheReadTokens:     b.cacheRd,
						Cost:                b.cost,
					},
				)
			}
		}
		result.Daily = append(result.Daily, entry)
		result.Totals.InputTokens += entry.InputTokens
		result.Totals.OutputTokens += entry.OutputTokens
		result.Totals.CacheCreationTokens += entry.CacheCreationTokens
		result.Totals.CacheReadTokens += entry.CacheReadTokens
		result.Totals.TotalCost, err = money.Add(
			result.Totals.TotalCost, entry.TotalCost)
		if err != nil {
			return db.DailyUsageResult{}, fmt.Errorf(
				"summing clickhouse usage total: %w", err)
		}
	}
	result.Totals.CacheSavings = totalSavings

	var aiCredits float64
	for key, b := range accum {
		aiCredits += db.AICreditsFromCost(key.agent, b.cost)
	}
	if aiCredits > 0 {
		result.Totals.CopilotAICredits = aiCredits
	}

	if result.Daily == nil {
		result.Daily = []db.DailyUsageEntry{}
	}
	result.SchemaVersion = export.UsageDailySchemaVersion
	pricingBlock, err := rateResolver.BuildBlock()
	if err != nil {
		return db.DailyUsageResult{}, fmt.Errorf(
			"building pricing block: %w", err)
	}
	result.Pricing = &pricingBlock
	projects, err := s.BuildProjectIdentityMap(ctx, sortedKeys(projectLabels))
	if err != nil {
		if !errors.Is(err, errNotImplemented) {
			return db.DailyUsageResult{}, err
		}
		projects = map[string]export.ProjectMapEntry{}
	}
	result.Projects = export.ProjectMapForWire(projects)
	if seenSessions != nil {
		result.SessionCounts = db.NewUsageSessionCounts(seenSessions)
	}
	db.SanitizeDailyUsageProjectLabelsWithCatalog(&result, projects)
	return result, nil
}

func addChUsageBucket(
	m map[string]chUsageBucket, key string, b chUsageBucket,
) error {
	cur := m[key]
	cur.inputTok += b.inputTok
	cur.outputTok += b.outputTok
	cur.cacheCr += b.cacheCr
	cur.cacheRd += b.cacheRd
	var err error
	cur.cost, err = money.Add(cur.cost, b.cost)
	if err != nil {
		return fmt.Errorf("summing clickhouse usage breakdown cost: %w", err)
	}
	m[key] = cur
	return nil
}

func sortedChUsageBucketKeys(m map[string]chUsageBucket) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool {
		left := m[out[i]]
		right := m[out[j]]
		if left.cost.Microdollars != right.cost.Microdollars {
			return left.cost.Microdollars > right.cost.Microdollars
		}
		return out[i] < out[j]
	})
	return out
}

func (s *Store) forEachSessionUsageAggregateRow(
	ctx context.Context,
	f db.UsageFilter,
	sessionID string,
	visit func(chUsageAggregateRow) error,
) error {
	state, err := s.preparedUsageState(ctx)
	if err != nil {
		return err
	}
	memoSlot, memoVersion, err := s.usageRowMemoKey(ctx, state, "session", f, sessionID, "", nil)
	if err != nil {
		return err
	}
	if rows, ok := s.sessionAggregateRows.get(memoSlot, memoVersion); ok {
		for _, r := range rows {
			if err := visit(r); err != nil {
				return err
			}
		}
		return nil
	}
	source := usageCTEFor(state, f, sessionID)
	cte, args := source.cte, source.args
	query := cte + `
		SELECT session_id, project, agent, model, provider_id, price_model, source, message_ordinal, ts,
			pricing_ts, display_name, started_at,
			input_tokens_norm AS input_tokens,
			output_tokens_norm AS output_tokens,
			snapshot_deduplicated_output_tokens,
			cache_create_norm AS cache_creation_tokens,
			cache_create_1h_norm AS cache_creation_1h_tokens,
			cache_read_norm AS cache_read_tokens,` + chUsageBillableSelect + `
		FROM usage_localized
		ORDER BY session_id ASC, model ASC, price_model ASC, ts ASC,
			COALESCE(message_ordinal, -1) ASC, source ASC, usage_dedup_key ASC`
	readCtx, err := withUsageDeltaTables(ctx, state)
	if err != nil {
		return err
	}
	s.usageReadQueries.Add(1)
	rows, err := s.queryContext(readCtx, query, args...)
	if err != nil {
		return fmt.Errorf("querying clickhouse session usage aggregates: %w", err)
	}
	defer rows.Close()
	var memo []chUsageAggregateRow
	for rows.Next() {
		var r chUsageAggregateRow
		var ts, pricingTS, startedAt any
		if err := rows.Scan(
			&r.sessionID, &r.project, &r.agent, &r.model, &r.providerID,
			&r.priceModel, &r.source, &r.messageOrdinal, &ts, &pricingTS,
			&r.displayName, &startedAt,
			&r.inputTok, &r.outputTok, &r.snapshotDedupOutput,
			&r.cacheCr, &r.cacheCr1h, &r.cacheRd,
			&r.billableInput, &r.billableOutput, &r.billableReason,
			&r.billableCacheCr, &r.billableCacheCr1h, &r.billableCacheRd,
			&r.billableWebSearch,
			&r.explicitCost, &r.reportedCostRows,
			&r.authoritativeCost, &r.authoritativeCostRows,
		); err != nil {
			return fmt.Errorf("scanning clickhouse session usage aggregate: %w", err)
		}
		r.ts = formatDBTime(ts)
		r.pricingTS = formatDBTime(pricingTS)
		r.startedAt = formatDBTime(startedAt)
		memo = append(memo, r)
		if err := visit(r); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating clickhouse session usage aggregates: %w", err)
	}
	s.sessionAggregateRows.put(memoSlot, memoVersion, memo)
	return nil
}

func (s *Store) sessionUsageRowCount(
	ctx context.Context, sessionID string,
) (int, error) {
	cte, args := chUsageCTE(db.UsageFilter{}, sessionID)
	query := cte + `
		SELECT toInt64(COUNT(*))
		FROM usage_localized
		WHERE (cost_microdollars IS NOT NULL AND cost_source != 'copilot-reported')
			OR input_tokens_norm != 0
			OR output_tokens_norm != 0
			OR cache_create_norm != 0
			OR cache_read_norm != 0
			OR reasoning_tokens_norm != 0
			OR web_search_requests_norm != 0`
	var count int
	if err := s.queryRowContext(ctx, query, args...).
		Scan(&count); err != nil {
		return 0, fmt.Errorf(
			"counting clickhouse session usage rows: %w", err)
	}
	return count, nil
}

func (s *Store) sessionUsageRows(
	ctx context.Context, sessionID string,
) ([]chSessionUsageRow, error) {
	cte, args := chUsageCTE(db.UsageFilter{}, sessionID)
	query := cte + `
		SELECT session_id, message_ordinal, source, ts, pricing_ts, model, provider_id,
			input_tokens_norm, output_tokens_norm,
			cache_create_norm, cache_create_1h_norm, cache_read_norm,
			reasoning_tokens_norm, web_search_requests_norm,
			cost_microdollars, cost_source
		FROM usage_localized
		ORDER BY ts ASC, session_id ASC,
			COALESCE(message_ordinal, -1) ASC,
			source ASC,
			usage_dedup_key ASC`
	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse session usage rows: %w", err)
	}
	defer rows.Close()
	var out []chSessionUsageRow
	for rows.Next() {
		var r chSessionUsageRow
		var ts, pricingTS any
		if err := rows.Scan(
			&r.sessionID, &r.messageOrdinal, &r.source, &ts, &pricingTS, &r.model, &r.providerID,
			&r.inputTok, &r.outputTok, &r.cacheCr, &r.cacheCr1h, &r.cacheRd,
			&r.reasoningTok, &r.webSearchRequests, &r.cost, &r.costSource,
		); err != nil {
			return nil, fmt.Errorf("scanning clickhouse session usage row: %w", err)
		}
		r.ts = formatDBTime(ts)
		r.pricingTS = formatDBTime(pricingTS)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GetTopSessionsByCost(
	ctx context.Context, f db.UsageFilter, limit int,
) ([]db.TopSessionEntry, error) {
	s.recordUsageRead("top", f, limit)
	return s.topSessionsByCost(ctx, f, limit)
}

func (s *Store) topSessionsByCost(
	ctx context.Context, f db.UsageFilter, limit int,
) ([]db.TopSessionEntry, error) {
	rateResolver, err := s.loadPricingResolver(ctx)
	if err != nil {
		return nil, err
	}
	type acc struct {
		row               db.TopSessionEntry
		tokens            int
		cost              money.Money
		authoritativeCost *money.Money
	}
	bySession := map[string]*acc{}
	err = s.forEachSessionUsageAggregateRow(
		ctx, f, "", func(r chUsageAggregateRow) error {
			a := bySession[r.sessionID]
			if a == nil {
				a = &acc{row: db.TopSessionEntry{
					SessionID: r.sessionID, DisplayName: r.displayName,
					Agent: r.agent, Project: r.project, StartedAt: r.startedAt,
				}}
				bySession[r.sessionID] = a
			}
			cost, _, _, _, priceErr := chUsageAggregateResolvedCost(
				r.model, r.priceModel, r.providerID, chUsagePricingTimestamp(r.pricingTS),
				r.inputTok, r.outputTok, r.cacheCr, r.cacheCr1h, r.cacheRd,
				r.billableInput, r.billableOutput, r.billableReason,
				r.billableCacheCr, r.billableCacheCr1h, r.billableCacheRd,
				r.billableWebSearch,
				r.explicitCost,
				r.reportedCostRows > 0,
				db.UsageSourceIsRequestScoped(r.source) || r.messageOrdinal.Valid,
				rateResolver,
			)
			if priceErr != nil {
				return priceErr
			}
			a.row.InputTokens += r.inputTok
			a.row.OutputTokens += r.outputTok
			a.row.CacheCreationTokens += r.cacheCr
			a.row.CacheReadTokens += r.cacheRd
			a.tokens += r.inputTok + r.outputTok + r.cacheCr + r.cacheRd
			a.cost, priceErr = money.Add(a.cost, cost)
			if priceErr != nil {
				return fmt.Errorf("summing clickhouse top-session cost: %w", priceErr)
			}
			if f.Model == "" && f.ExcludeModel == "" && r.authoritativeCostRows > 0 {
				v := money.Money{Microdollars: r.authoritativeCost}
				a.authoritativeCost = &v
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	out := make([]db.TopSessionEntry, 0, len(bySession))
	for _, a := range bySession {
		a.row.TotalTokens = a.tokens
		if a.authoritativeCost != nil {
			a.row.Cost = *a.authoritativeCost
		} else {
			a.row.Cost = a.cost
		}
		out = append(out, a.row)
	}
	return db.SortAndLimitTopSessions(
		out, limit, f.TopSessionsSort, f.TopSessionsTokenTypes,
	), nil
}

func (s *Store) GetUsageSessionCounts(
	ctx context.Context, f db.UsageFilter,
) (db.UsageSessionCounts, error) {
	s.recordUsageRead("counts", f, 0)
	return s.usageSessionCounts(ctx, f)
}

func (s *Store) usageSessionCounts(
	ctx context.Context, f db.UsageFilter,
) (db.UsageSessionCounts, error) {
	state, err := s.preparedUsageState(ctx)
	if err != nil {
		return db.UsageSessionCounts{}, err
	}
	memoSlot, memoVersion, err := s.usageRowMemoKey(ctx, state, "counts", f, "", "", nil)
	if err != nil {
		return db.UsageSessionCounts{}, err
	}
	counted, ok := s.usageSessionRows.get(memoSlot, memoVersion)
	if !ok {
		source := usageCTEFor(state, f, "")
		readCtx, err := withUsageDeltaTables(ctx, state)
		if err != nil {
			return db.UsageSessionCounts{}, err
		}
		s.usageReadQueries.Add(1)
		rows, err := s.queryContext(readCtx, source.cte+`
			SELECT DISTINCT session_id, project, agent
			FROM usage_localized
			WHERE session_id != ''
			ORDER BY session_id`, source.args...)
		if err != nil {
			return db.UsageSessionCounts{}, fmt.Errorf(
				"querying clickhouse usage session counts: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r chUsageSessionRow
			if err := rows.Scan(&r.sessionID, &r.project, &r.agent); err != nil {
				return db.UsageSessionCounts{}, fmt.Errorf(
					"scanning clickhouse usage session count: %w", err)
			}
			counted = append(counted, r)
		}
		if err := rows.Err(); err != nil {
			return db.UsageSessionCounts{}, fmt.Errorf(
				"iterating clickhouse usage session counts: %w", err)
		}
		s.usageSessionRows.put(memoSlot, memoVersion, counted)
	}
	seen := make(map[string]db.UsageSessionInfo, len(counted))
	for _, r := range counted {
		seen[r.sessionID] = db.UsageSessionInfo{Project: r.project, Agent: r.agent}
	}
	return db.NewUsageSessionCounts(seen), nil
}

type chUsageSessionRow struct {
	sessionID, project, agent string
}

// usageRowMemoKey names a usage read's memo slot by the read's own
// parameters and its version by everything its rows depend on (see
// usageReadFingerprint). A single session's usage is read from the raw
// rows even when prepared rows are ready (see usageCTEFor), so its version
// names the parts of every table: a push that fails after writing the raw
// rows and before the snapshot changes no table a prepared read joins.
func (s *Store) usageRowMemoKey(
	ctx context.Context, state preparedUsageState, kind string, f db.UsageFilter,
	sessionID, pricingDigest string, customModels [][2]string,
) (slot, version string, err error) {
	if sessionID != "" {
		version, err = s.partsFingerprint(ctx)
	} else {
		version, err = s.usageReadFingerprint(ctx, state)
	}
	if err != nil {
		return "", "", err
	}
	slot, err = usageRowMemoKeyFor(kind, f, sessionID, pricingDigest, customModels)
	return slot, version, err
}

// chUsageReadTables are the tables a prepared usage read touches besides
// prepared_usage itself, whose rows the state's stamp identifies.
var chUsageReadTables = []string{
	"sessions", "usage_session_snapshots", "cursor_usage_events", "usage_event_prices",
	"usage_price_contexts", "model_pricing", "model_pricing_bands", "genai_pricing", "sync_metadata",
}

// usageReadFingerprint identifies what a usage read depends on. A prepared
// read depends on the prepared rows, named by the state's stamp, and on the
// active parts of the tables it joins or prices from; the large message and
// event tables, whose background merges rename parts long after a push, are
// not among them. A raw read depends on the parts of every table.
func (s *Store) usageReadFingerprint(ctx context.Context, state preparedUsageState) (string, error) {
	if !state.ready {
		return s.partsFingerprint(ctx)
	}
	fingerprint, err := s.tablePartsFingerprint(ctx, chUsageReadTables)
	if err != nil {
		return "", err
	}
	return fingerprint + "|" + state.stamp, nil
}

// usageRowMemoKeyFor renders the key. The filter is JSON and the custom
// models are Go syntax, so every field and string boundary is preserved.
// A filter that compares session times with the current time selects
// other sessions as time passes, with no write to name the change, so its
// key is empty and its rows are not kept.
func usageRowMemoKeyFor(
	kind string, f db.UsageFilter, sessionID, pricingDigest string,
	customModels [][2]string,
) (string, error) {
	if chTerminationUsesTime(f.Termination) {
		return "", nil
	}
	filter, err := json.Marshal(f)
	if err != nil {
		return "", fmt.Errorf("encoding usage filter for memo: %w", err)
	}
	return fmt.Sprintf("%s|%s|%s|%s|%#v", kind, filter, sessionID, pricingDigest, customModels), nil
}

// partsFingerprint identifies the active parts of every table in the
// mirror. Two reads under the same fingerprint see the same rows.
func (s *Store) partsFingerprint(ctx context.Context) (string, error) {
	return s.tablePartsFingerprint(ctx, nil)
}

// tablePartsFingerprint identifies the active parts of the named tables,
// or of every table when none are named. Within a request that took a
// parts snapshot, every fingerprint comes from that one snapshot.
func (s *Store) tablePartsFingerprint(ctx context.Context, tables []string) (string, error) {
	snapshot, ok := ctx.Value(partsSnapshotKey{}).(partsSnapshot)
	if !ok {
		var err error
		if snapshot, err = s.readPartsSnapshot(ctx); err != nil {
			return "", err
		}
	}
	if len(tables) == 0 {
		tables = slices.Collect(maps.Keys(snapshot))
	}
	tables = slices.Sorted(slices.Values(tables))
	sum := sha256.New()
	for _, table := range slices.Compact(tables) {
		fmt.Fprintf(sum, "%s\x00%s\x00", table, snapshot[table])
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// partsSnapshot maps each table to a digest of its active parts.
type partsSnapshot map[string]string

type partsSnapshotKey struct{}

// readPartsSnapshot reads the active parts of every table in one query.
func (s *Store) readPartsSnapshot(ctx context.Context) (partsSnapshot, error) {
	rows, err := s.queryContext(ctx, `SELECT table,
		hex(SHA256(toString(arraySort(groupArray((name, hash_of_all_files))))))
		FROM system.parts WHERE database = currentDatabase() AND active GROUP BY table`)
	if err != nil {
		return nil, fmt.Errorf("reading clickhouse parts: %w", err)
	}
	defer rows.Close()
	snapshot := partsSnapshot{}
	for rows.Next() {
		var table, digest string
		if err := rows.Scan(&table, &digest); err != nil {
			return nil, fmt.Errorf("scanning clickhouse parts: %w", err)
		}
		snapshot[table] = digest
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating clickhouse parts: %w", err)
	}
	return snapshot, nil
}

// withPartsSnapshot makes the fingerprints of the reads under the returned
// context come from one snapshot of the parts, taken before any of them.
func (s *Store) withPartsSnapshot(ctx context.Context) (context.Context, error) {
	snapshot, err := s.readPartsSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, partsSnapshotKey{}, snapshot), nil
}

// usageRowMemo keeps the rows of recent usage reads. A slot names the read
// and holds only the rows of its latest version, which names the parts the
// rows were read from. A push replaces a slot's rows instead of adding a
// second copy beside them, and a stale version is never returned. The
// empty slot keeps nothing.
type usageRowMemo[T any] struct {
	mu      sync.Mutex
	entries map[string]usageRowMemoEntry[T]
	order   []string
}

type usageRowMemoEntry[T any] struct {
	version string
	rows    []T
}

const usageRowMemoLimit = 64

func (m *usageRowMemo[T]) get(slot, version string) ([]T, bool) {
	if slot == "" {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[slot]
	if !ok || entry.version != version {
		return nil, false
	}
	return entry.rows, true
}

func (m *usageRowMemo[T]) put(slot, version string, rows []T) {
	if slot == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = map[string]usageRowMemoEntry[T]{}
	}
	if _, ok := m.entries[slot]; !ok {
		m.order = append(m.order, slot)
		if len(m.order) > usageRowMemoLimit {
			delete(m.entries, m.order[0])
			m.order = m.order[1:]
		}
	}
	m.entries[slot] = usageRowMemoEntry[T]{version: version, rows: rows}
}

func appendChUsageMatchingActivityClauses(
	where string, args []any, f db.UsageFilter,
) (string, []any) {
	var messageArgs []any
	messageWhere, messageArgs := appendChUsageSourceFilterClauses(
		chUsageMatchingMessageSourceEligibility, messageArgs, "m.model", f,
	)
	var eventArgs []any
	eventWhere, eventArgs := appendChUsageSourceFilterClauses(
		chUsageEventSourceEligibility, eventArgs, "ue.model", f,
	)

	where += `
		AND (
			s.id IN (
				SELECT m.session_id
				FROM messages m
				WHERE ` + messageWhere + `
			)
			OR s.id IN (
				SELECT ue.session_id
				FROM usage_events ue
				WHERE ` + eventWhere + `
			)
		)`
	args = append(args, messageArgs...)
	args = append(args, eventArgs...)
	return where, args
}

func (s *Store) GetUsageMatchingSessionCount(
	ctx context.Context, f db.UsageFilter,
) (int, error) {
	if f.From == "" && f.To == "" {
		where, args := appendChUsageSessionFilterClauses(
			"s.deleted_at IS NULL", nil, f, "")
		where, args = appendChUsageMatchingActivityClauses(where, args, f)

		var count int
		err := s.queryRowContext(ctx, `
			SELECT toInt64(COUNT(*))
			FROM sessions s WHERE `+where, args...).Scan(&count)
		if err != nil {
			return 0, fmt.Errorf("querying matching usage sessions: %w", err)
		}
		return count, nil
	}

	query, args := chMatchingUsageRawSQL(f)
	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("querying matching usage sessions: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]struct{})
	for rows.Next() {
		var (
			id string
			ts any
		)
		if err := rows.Scan(&id, &ts); err != nil {
			return 0, fmt.Errorf("scanning matching usage session: %w", err)
		}
		date := analyticsLocalDate(formatDBTime(ts), f.Timezone)
		if date == "" {
			continue
		}
		if f.From != "" && date < f.From {
			continue
		}
		if f.To != "" && date > f.To {
			continue
		}
		seen[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterating matching usage sessions: %w", err)
	}
	return len(seen), nil
}

func (s *Store) GetSessionUsage(
	ctx context.Context, sessionID string, includeBreakdown bool,
) (*db.SessionUsage, error) {
	sess, err := s.GetSession(ctx, sessionID)
	if err != nil || sess == nil {
		return nil, err
	}
	rateResolver, err := s.loadPricingResolver(ctx)
	if err != nil {
		return nil, err
	}
	var breakdownRows []chSessionUsageRow
	breakdownCount := 0
	if includeBreakdown {
		breakdownRows, err = s.sessionUsageRows(ctx, sessionID)
	} else {
		breakdownCount, err = s.sessionUsageRowCount(ctx, sessionID)
	}
	if err != nil {
		return nil, err
	}
	models := map[string]bool{}
	unpriced := map[string]bool{}
	var totalCost money.Money
	var authoritativeCost *money.Money
	var hasComputedCost, hasReportedCost bool
	deduplicatedOutputTokens := 0
	hasRows := false
	err = s.forEachSessionUsageAggregateRow(
		ctx, db.UsageFilter{}, sessionID,
		func(r chUsageAggregateRow) error {
			deduplicatedOutputTokens += r.snapshotDedupOutput
			if r.authoritativeCostRows > 0 {
				v := money.Money{Microdollars: r.authoritativeCost}
				authoritativeCost = &v
			}
			cost, _, priced, contributes, priceErr := chUsageAggregateResolvedCost(
				r.model, r.priceModel, r.providerID, chUsagePricingTimestamp(r.pricingTS),
				r.inputTok, r.outputTok, r.cacheCr, r.cacheCr1h, r.cacheRd,
				r.billableInput, r.billableOutput, r.billableReason,
				r.billableCacheCr, r.billableCacheCr1h, r.billableCacheRd,
				r.billableWebSearch,
				r.explicitCost,
				r.reportedCostRows > 0,
				db.UsageSourceIsRequestScoped(r.source) || r.messageOrdinal.Valid,
				rateResolver,
			)
			if priceErr != nil {
				return priceErr
			}
			if !contributes {
				return nil
			}
			hasRows = true
			models[r.model] = true
			totalCost, priceErr = money.Add(totalCost, cost)
			if priceErr != nil {
				return fmt.Errorf("summing clickhouse session usage: %w", priceErr)
			}
			if r.reportedCostRows > 0 {
				hasReportedCost = true
			}
			if r.billableInput != 0 || r.billableOutput != 0 ||
				r.billableReason != 0 || r.billableCacheCr != 0 ||
				r.billableCacheRd != 0 || r.billableWebSearch > 0 {
				hasComputedCost = true
			}
			if !priced {
				unpriced[r.model] = true
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	breakdown := make([]db.SessionUsageBreakdownEntry, 0, len(breakdownRows))
	for _, r := range breakdownRows {
		cost, priced, contributes, priceErr := chSessionUsageRowCost(r, rateResolver)
		if priceErr != nil {
			return nil, priceErr
		}
		if !contributes {
			continue
		}
		breakdown = append(breakdown, chSessionUsageBreakdownEntry(
			r, len(breakdown)+1, cost, priced))
	}
	if authoritativeCost != nil && len(breakdown) > 0 {
		weights := make([]money.Money, len(breakdown))
		for i := range breakdown {
			weights[i] = breakdown[i].Cost
		}
		costs := export.AllocateCostByWeight(*authoritativeCost, weights)
		for i := range breakdown {
			breakdown[i].Cost = costs[i]
			breakdown[i].HasCost = true
		}
	}
	if includeBreakdown {
		breakdownCount = len(breakdown)
	}
	out := &db.SessionUsage{
		SessionID: sessionID, Agent: sess.Agent, Project: sess.Project,
		TotalOutputTokens: max(sess.TotalOutputTokens-deduplicatedOutputTokens, 0),
		PeakContextTokens: sess.PeakContextTokens,
		HasTokenData:      sess.HasTotalOutputTokens || sess.HasPeakContextTokens,
		Models:            sortedKeys(models),
		UnpricedModels:    sortedKeys(unpriced),
		BreakdownCount:    breakdownCount,
		Breakdown:         breakdown,
	}
	if authoritativeCost != nil {
		out.HasCost = true
		out.Cost = *authoritativeCost
		out.CostSource = export.CostSourceReported
	} else if len(unpriced) == 0 && hasRows {
		out.HasCost = true
		out.Cost = totalCost
		out.CostSource = export.CombinedCostSource(hasComputedCost, hasReportedCost)
	}
	if out.HasCost {
		out.AICredits = db.AICreditsFromCost(sess.Agent, out.Cost)
	}
	out.CostUSD = db.CostUSDFromCost(out.HasCost, out.Cost)
	return out, nil
}
