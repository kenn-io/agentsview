package duckdb

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ccoveille/go-safecast/v2"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
	"go.kenn.io/agentsview/internal/readbase"
	"go.kenn.io/agentsview/internal/signals"
)

// loadAnalyticsSessions leaves paired model/time filtering to the shared base.
func (s *Store) loadAnalyticsSessions(
	ctx context.Context, f db.AnalyticsFilter,
	includeDate, includeTime bool,
	extraPred string, extraArgs []any,
) ([]readbase.AnalyticsSession, error) {
	where, args := duckBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.",
		includeDate, includeTime)
	if extraPred != "" {
		where += " AND " + extraPred
		args = append(args, extraArgs...)
	}
	rows, err := s.queryContext(ctx, `
		SELECT `+readbase.AnalyticsSessionColumns+`
		FROM sessions s
		WHERE `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb analytics sessions: %w", err)
	}
	defer rows.Close()

	return readbase.ScanAnalyticsSessions(rows, "duckdb", formatDBTime, false)
}

func duckBuildAnalyticsWhere(f db.AnalyticsFilter, dateCol, tablePrefix string, includeDate, includeTime bool) (string, []any) {
	var localDate string
	var localDateArgs []any
	if includeDate {
		localDate, localDateArgs = duckAnalyticsLocalDateExpr(dateCol, f)
	}
	where, args := readbase.AnalyticsWhere(f, dateCol, tablePrefix, includeDate, "CAST(? AS TIMESTAMP)", localDate, localDateArgs, db.DuckDBQueryDialect())
	if includeTime && (f.DayOfWeek != nil || f.Hour != nil) {
		pred, pargs := duckAnalyticsMessageTimeExists(f, tablePrefix+"id")
		where += " AND " + pred
		args = append(args, pargs...)
	}
	return where, args
}

func duckAnalyticsLocalDateExpr(
	tsExpr string, f db.AnalyticsFilter,
) (string, []any) {
	if f.Timezone != "" {
		return "strftime(timezone(?, timezone('UTC', " + tsExpr + ")), '%Y-%m-%d')",
			[]any{f.Timezone}
	}
	return "strftime(" + tsExpr + ", '%Y-%m-%d')", nil
}

func duckAnalyticsLocalTimeExpr(
	tsExpr string, f db.AnalyticsFilter,
) (string, []any) {
	if f.Timezone != "" {
		return "timezone(?, timezone('UTC', " + tsExpr + "))", []any{f.Timezone}
	}
	return tsExpr, nil
}

func duckAnalyticsMessageTimeExists(
	f db.AnalyticsFilter, sessionIDExpr string,
) (string, []any) {
	preds := []string{
		"m.session_id = " + sessionIDExpr,
		"m.timestamp IS NOT NULL",
	}
	var args []any
	if modelPred, modelArgs := readbase.AnalyticsCSVPredicate("m.model", f.Model, db.DuckDBQueryDialect()); modelPred != "" {
		preds = append(preds, modelPred)
		args = append(args, modelArgs...)
	}
	if f.DayOfWeek != nil {
		local, localArgs := duckAnalyticsLocalTimeExpr("m.timestamp", f)
		preds = append(preds,
			"((CAST(strftime("+local+", '%w') AS INTEGER) + 6) % 7) = ?")
		args = append(args, append(localArgs, *f.DayOfWeek)...)
	}
	if f.Hour != nil {
		local, localArgs := duckAnalyticsLocalTimeExpr("m.timestamp", f)
		preds = append(preds,
			"CAST(strftime("+local+", '%H') AS INTEGER) = ?")
		args = append(args, append(localArgs, *f.Hour)...)
	}
	return "EXISTS (SELECT 1 FROM messages m WHERE " +
		strings.Join(preds, " AND ") + ")", args
}

func (s analyticsSQL) ToolCountsSQL(ids []string) (string, []any) {
	ph, args := db.InPlaceholders(ids)
	return `
			SELECT tc.session_id, m.model, m.timestamp, COUNT(*)
			FROM tool_calls tc
			JOIN messages m
				ON m.session_id = tc.session_id
				AND m.id = tc.message_id
			WHERE tc.session_id IN ` + ph + `
			GROUP BY tc.session_id, m.model, m.timestamp`, args
}

func duckAnalyticsBucketExpr(dateExpr, granularity string) string {
	switch granularity {
	case "week":
		return "strftime(date_trunc('week', CAST(" + dateExpr + " AS DATE)), '%Y-%m-%d')"
	case "month":
		return "strftime(date_trunc('month', CAST(" + dateExpr + " AS DATE)), '%Y-%m-%d')"
	default:
		return dateExpr
	}
}

func duckAnalyticsToolMessageJoin(
	toolAlias string, model string,
) string {
	if model == "" {
		return ""
	}
	return `
			JOIN messages m
				ON m.session_id = ` + toolAlias + `.session_id
				AND m.id = ` + toolAlias + `.message_id`
}

func (s analyticsSQL) Autonomy(ctx context.Context, ids []string, _ db.AnalyticsFilter) (map[string]int, error) {
	if len(ids) == 0 {
		return map[string]int{}, nil
	}
	ph, args := db.InPlaceholders(ids)
	rows, err := s.QueryContext(ctx, `
		SELECT session_id,
			SUM(CASE WHEN role = 'user' AND is_system = FALSE
				AND COALESCE(source_subtype, '') != 'tool_result' THEN 1 ELSE 0 END) AS user_count,
			SUM(CASE WHEN role = 'assistant' AND has_tool_use = TRUE THEN 1 ELSE 0 END) AS tool_count
		FROM messages
		WHERE session_id IN `+ph+`
		GROUP BY session_id`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb autonomy: %w", err)
	}
	defer rows.Close()
	return readbase.ScanAnalyticsAutonomy(rows, "duckdb")
}

func (s analyticsSQL) VelocityMessages(ctx context.Context, ids []string, _ db.AnalyticsFilter, loc *time.Location) (map[string][]db.TimingMessage, error) {
	out := make(map[string][]db.TimingMessage)
	if len(ids) == 0 {
		return out, nil
	}
	ph, args := db.InPlaceholders(ids)
	rows, err := s.QueryContext(ctx, `
		SELECT session_id, ordinal, role, timestamp, content_length
		FROM messages
		WHERE session_id IN `+ph+`
		ORDER BY session_id, ordinal`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb velocity messages: %w", err)
	}
	defer rows.Close()
	return readbase.ScanAnalyticsVelocityMessages(rows, "duckdb", formatDBTime, loc, out)
}

func (s analyticsSQL) VelocityToolCounts(ctx context.Context, ids []string, _ db.AnalyticsFilter) (map[string]int, error) {
	out := make(map[string]int)
	if len(ids) == 0 {
		return out, nil
	}
	ph, args := db.InPlaceholders(ids)
	rows, err := s.QueryContext(ctx, `
		SELECT session_id, COUNT(*)
		FROM tool_calls
		WHERE session_id IN `+ph+`
		GROUP BY session_id`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb velocity tool calls: %w", err)
	}
	defer rows.Close()
	return readbase.ScanAnalyticsVelocityToolCounts(rows, "duckdb", out)
}

func (s *Store) duckPopulateFrustrationMarkers(
	ctx context.Context,
	rows []db.SignalRow,
) error {
	if len(rows) == 0 {
		return nil
	}
	idx := make(map[string]int, len(rows))
	placeholders := make([]string, len(rows))
	args := make([]any, len(rows))
	for i := range rows {
		idx[rows[i].ID] = i
		placeholders[i] = "?"
		args[i] = rows[i].ID
	}
	q := `SELECT session_id, content, is_system
		FROM messages
		WHERE role = 'user' AND COALESCE(source_subtype, '') <> 'tool_result' AND session_id IN (` +
		strings.Join(placeholders, ",") + `)`
	msgRows, err := s.queryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("querying duckdb frustration markers: %w", err)
	}
	defer msgRows.Close()
	for msgRows.Next() {
		var sessionID, content string
		var isSystem bool
		if err := msgRows.Scan(
			&sessionID, &content, &isSystem,
		); err != nil {
			return fmt.Errorf("scanning duckdb frustration marker: %w", err)
		}
		i, ok := idx[sessionID]
		if !ok || isSystem {
			continue
		}
		if signals.IsFrustrationMarker(content) {
			rows[i].FrustrationMarkerCount++
		}
	}
	if err := msgRows.Err(); err != nil {
		return fmt.Errorf("iterating duckdb frustration markers: %w", err)
	}
	return nil
}

func (s *Store) loadPricing(ctx context.Context) (map[string]export.ModelRates, error) {
	rows, err := s.queryContext(ctx, `
		SELECT p.model_pattern, p.input_microdollars_per_mtok,
			p.output_microdollars_per_mtok,
			p.cache_creation_microdollars_per_mtok,
			p.cache_creation_1h_microdollars_per_mtok,
			p.cache_read_microdollars_per_mtok, p.updated_at,
			b.above_input_tokens, b.input_microdollars_per_mtok,
			b.output_microdollars_per_mtok,
			b.cache_creation_microdollars_per_mtok,
			b.cache_creation_1h_microdollars_per_mtok,
			b.cache_read_microdollars_per_mtok, b.updated_at
		FROM model_pricing p
		LEFT JOIN model_pricing_bands b ON b.model_pattern = p.model_pattern
		ORDER BY p.model_pattern, b.above_input_tokens`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]export.ModelRates{}
	count := 0
	for rows.Next() {
		var model string
		var rates export.ModelRates
		var updatedAt string
		var threshold, input, output, cacheCreation, cacheCreation1h,
			cacheRead sql.NullInt64
		var bandUpdatedAt sql.NullString
		if err := rows.Scan(
			&model, &rates.InputPerMTok, &rates.OutputPerMTok,
			&rates.CacheWritePerMTok, &rates.CacheWrite1hPerMTok,
			&rates.CacheReadPerMTok, &updatedAt,
			&threshold, &input, &output, &cacheCreation, &cacheCreation1h,
			&cacheRead, &bandUpdatedAt,
		); err != nil {
			return nil, err
		}
		if strings.HasPrefix(model, "_") {
			continue
		}
		stored, exists := out[model]
		if !exists {
			if parsed, err := time.Parse(time.RFC3339Nano, updatedAt); err == nil {
				t := parsed.UTC()
				rates.UpdatedAt = &t
			}
			stored = rates
			count++
		}
		if threshold.Valid {
			aboveInputTokens, err := safecast.Convert[int](threshold.Int64)
			if err != nil {
				return nil, fmt.Errorf(
					"converting duckdb pricing threshold for %q: %w",
					model, err,
				)
			}
			var parsedUpdatedAt *time.Time
			if parsed, err := time.Parse(
				time.RFC3339Nano, bandUpdatedAt.String,
			); err == nil {
				t := parsed.UTC()
				parsedUpdatedAt = &t
			}
			stored.Bands = append(stored.Bands, export.PricingBand{
				AboveInputTokens:    aboveInputTokens,
				InputPerMTok:        money.Money{Microdollars: input.Int64},
				OutputPerMTok:       money.Money{Microdollars: output.Int64},
				CacheWritePerMTok:   money.Money{Microdollars: cacheCreation.Int64},
				CacheWrite1hPerMTok: money.Money{Microdollars: cacheCreation1h.Int64},
				CacheReadPerMTok:    money.Money{Microdollars: cacheRead.Int64},
				UpdatedAt:           parsedUpdatedAt,
			})
		}
		out[model] = stored
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if count == 0 {
		out = db.FallbackPricingMap()
	} else {
		fallback := db.FallbackPricingMap()
		for model, rates := range out {
			rates.Source = db.ModelPricingSource(model, rates, fallback)
			out[model] = rates
		}
	}
	for model, custom := range s.customPricing {
		rates := export.ModelRates{
			InputPerMTok:  money.Money{Microdollars: custom.InputMicrodollarsPerMTok},
			OutputPerMTok: money.Money{Microdollars: custom.OutputMicrodollarsPerMTok},
			CacheWritePerMTok: money.Money{
				Microdollars: custom.CacheCreationMicrodollarsPerMTok,
			},
			CacheWrite1hPerMTok: money.Money{
				Microdollars: custom.CacheCreation1hMicrodollarsPerMTok,
			},
			CacheReadPerMTok: money.Money{
				Microdollars: custom.CacheReadMicrodollarsPerMTok,
			},
		}
		rates.Source = export.PricingRowSourceCustom
		out[model] = rates
	}
	return out, nil
}

func (s *Store) loadPricingResolver(
	ctx context.Context,
) (*export.PricingResolver, error) {
	pricing, err := s.loadPricing(ctx)
	if err != nil {
		return nil, err
	}
	document, err := scanDuckGenAIPricing(s.queryRowContext(ctx, `
		SELECT version, source_ref, source, data_json, updated_at
		FROM genai_pricing WHERE singleton = 1`))
	if err != nil {
		return nil, err
	}
	genAI, err := duckGenAIEffectivePricingRow(document)
	if err != nil {
		return nil, err
	}
	rows := db.MirrorPricingRows(pricing)
	return export.NewPricingResolver(append(rows, genAI)), nil
}

type duckUsageBounds struct {
	from string
	to   string
}

func duckAnalyticsMessageWindowPred(col, from, to string) (string, []any) {
	return readbase.AnalyticsMessageWindowPred(col, from, to, "CAST(? AS TIMESTAMP)")
}

var analyticsQueryObserver func(string)

func observeAnalyticsQuery(query string) {
	if analyticsQueryObserver != nil {
		analyticsQueryObserver(query)
	}
}

func duckAnalyticsToolSessionWindow(f db.AnalyticsFilter) (string, []any) {
	from, to := readbase.AnalyticsWindowBounds(f)
	sessionPred, args := duckAnalyticsMessageWindowPred("COALESCE(s.started_at, s.created_at)", from, to)
	if sessionPred == "" {
		return "", nil
	}
	messagePred, messageArgs := duckAnalyticsMessageWindowPred("wm.timestamp", from, to)
	return "(" + sessionPred + " OR EXISTS (SELECT 1 FROM messages wm WHERE wm.session_id = s.id AND " + messagePred + "))", append(args, messageArgs...)
}

func duckUsageBoundsForFilter(f db.UsageFilter) duckUsageBounds {
	from, to := readbase.PaddedDateBounds(f.From, f.To)
	return duckUsageBounds{from: from, to: to}
}

func appendDuckUsageColumnBounds(
	where, col string, b duckUsageBounds, args []any,
) (string, []any) {
	if b.from != "" {
		where += "\n\t\t\tAND " + col + " >= CAST(? AS TIMESTAMP)"
		args = append(args, b.from)
	}
	if b.to != "" {
		where += "\n\t\t\tAND " + col + " <= CAST(? AS TIMESTAMP)"
		args = append(args, b.to)
	}
	return where, args
}

func appendDuckUsageSourceFilterClauses(
	where string, args []any, modelCol string, f db.UsageFilter,
) (string, []any) {
	b := db.NewQueryBuilder(db.DuckDBQueryDialect(), 0)
	preds := db.BuildUsageSourceFilter(f, b, modelCol)
	where = db.AppendUsagePredicates(where, preds, "\t\t\t")
	return where, append(args, b.Args()...)
}

func appendDuckUsageSessionFilterClauses(
	where string, args []any, f db.UsageFilter, sessionID string,
) (string, []any) {
	b := db.NewQueryBuilder(db.DuckDBQueryDialect(), 0)
	preds := db.BuildUsageSessionFilter(f, b, sessionID)
	where = db.AppendUsagePredicates(where, preds, "\t\t\t")
	return where, append(args, b.Args()...)
}

const duckDailyCursorUsageRowsSQLTemplate = `
SELECT
	'' AS session_id,
	NULL AS message_ordinal,
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
	0 AS reasoning_tokens,
	cu.charged_microdollars AS cost_microdollars,
	'cursor-reported' AS cost_source,
	'' AS project,
	'cursor' AS agent,
	'' AS machine,
	0 AS user_message_count,
	cu.is_headless AS is_automated,
	'' AS display_name,
	'' AS group_key, '' AS session_name,
	NULL AS started_at,
	cu.occurred_at AS activity_at
FROM cursor_usage_events cu
WHERE %s`

const duckUsageMessageEligibility = `
			m.token_usage != ''
			AND m.model != ''
			AND m.model != '<synthetic>'
			AND s.deleted_at IS NULL`

// duckUsageMatchingMessageSourceEligibility is the message-only half of
// duckUsageMessageEligibility with the token-presence requirement removed
// and the model-presence requirement relaxed to a role check, for
// GetUsageMatchingSessionCount. See the usageMatchingMessageEligibility
// doc comment in internal/db.
const duckUsageMatchingMessageSourceEligibility = `
			m.role = 'assistant'
			AND m.model != '<synthetic>'`

const duckUsageMatchingMessageEligibility = duckUsageMatchingMessageSourceEligibility + `
			AND s.deleted_at IS NULL`

const duckUsageEventSourceEligibility = `
			ue.model != ''`

const duckUsageEventEligibility = duckUsageEventSourceEligibility + `
			AND s.deleted_at IS NULL`

// duckUsageSourceWheres builds the message/event WHERE clauses shared by
// duckUsageRawSQL and duckMatchingUsageRawSQL; the two callers differ only
// in the message eligibility predicate.
func duckUsageSourceWheres(
	f db.UsageFilter, sessionID, messageEligibility string, b duckUsageBounds,
) (string, []any, string, []any) {
	messageWhere := messageEligibility
	var messageArgs []any
	messageWhere, messageArgs = appendDuckUsageSourceFilterClauses(
		messageWhere, messageArgs, "m.model", f)
	messageWhere, messageArgs = appendDuckUsageSessionFilterClauses(
		messageWhere, messageArgs, f, sessionID)
	messageWhere, messageArgs = appendDuckUsageColumnBounds(
		messageWhere, "COALESCE(m.timestamp, s.started_at)", b, messageArgs)

	eventWhere := duckUsageEventEligibility
	var eventArgs []any
	eventWhere, eventArgs = appendDuckUsageSourceFilterClauses(
		eventWhere, eventArgs, "ue.model", f)
	eventWhere, eventArgs = appendDuckUsageSessionFilterClauses(
		eventWhere, eventArgs, f, sessionID)
	eventWhere, eventArgs = appendDuckUsageColumnBounds(
		eventWhere, "COALESCE(ue.occurred_at, s.started_at)", b, eventArgs)

	return messageWhere, messageArgs, eventWhere, eventArgs
}

func duckUsageRawSQL(f db.UsageFilter, sessionID string) (string, []any) {
	messageWhere, messageArgs, eventWhere, eventArgs := duckUsageSourceWheres(
		f, sessionID, duckUsageMessageEligibility, duckUsageBoundsForFilter(f))

	query := fmt.Sprintf(`
		SELECT m.session_id AS session_id, m.ordinal AS message_ordinal,
			'message' AS source, COALESCE(m.timestamp, s.started_at) AS ts,
			m.timestamp AS pricing_ts,
			m.model AS model, m.provider_id AS provider_id, m.token_usage AS token_json,
			m.claude_message_id AS claude_message_id,
			m.claude_request_id AS claude_request_id,
			m.source_uuid AS source_uuid,
			'' AS usage_dedup_key,
				0 AS input_tokens, 0 AS output_tokens,
				0 AS cache_create, 0 AS cache_read,
				COALESCE(TRY_CAST(json_extract_string(m.token_usage, '$.reasoning_tokens') AS BIGINT), 0) AS reasoning_tokens,
				NULL AS cost_microdollars, '' AS cost_source,
			s.project AS project, s.agent AS agent, s.machine AS machine,
			s.user_message_count AS user_message_count, s.is_automated AS is_automated,
			COALESCE(s.display_name, s.session_name, s.first_message, s.project, s.id) AS display_name,
			s.group_key AS group_key, COALESCE(s.session_name, '') AS session_name,
			s.started_at AS started_at,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS activity_at
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		WHERE %s
		UNION ALL
		SELECT ue.session_id AS session_id, ue.message_ordinal AS message_ordinal,
			ue.source AS source, COALESCE(ue.occurred_at, s.started_at) AS ts,
			ue.occurred_at AS pricing_ts,
			ue.model AS model, ue.provider_id AS provider_id, '' AS token_json,
			'' AS claude_message_id, '' AS claude_request_id,
			'' AS source_uuid,
			CASE
				WHEN ue.dedup_key != '' THEN ue.session_id || ':' || ue.source || ':' || ue.dedup_key
				ELSE ue.session_id || ':' || ue.source || ':id:' || CAST(ue.id AS VARCHAR)
			END AS usage_dedup_key,
				ue.input_tokens AS input_tokens, ue.output_tokens AS output_tokens,
				ue.cache_creation_input_tokens AS cache_create,
				ue.cache_read_input_tokens AS cache_read,
				ue.reasoning_tokens AS reasoning_tokens,
				ue.cost_microdollars AS cost_microdollars,
				ue.cost_source AS cost_source,
			s.project AS project, s.agent AS agent, s.machine AS machine,
			s.user_message_count AS user_message_count, s.is_automated AS is_automated,
			COALESCE(s.display_name, s.session_name, s.first_message, s.project, s.id) AS display_name,
			s.group_key AS group_key, COALESCE(s.session_name, '') AS session_name,
			s.started_at AS started_at,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS activity_at
		FROM usage_events ue
		JOIN sessions s ON s.id = ue.session_id
		WHERE %s`,
		messageWhere, eventWhere)
	args := make([]any, 0, len(messageArgs)+len(eventArgs))
	args = append(args, messageArgs...)
	args = append(args, eventArgs...)
	return query, args
}

// duckMatchingUsageRawSQL builds the bounded-range row source for
// GetUsageMatchingSessionCount. It shares duckUsageRawSQL's WHERE
// assembly (via duckUsageSourceWheres) but relaxes the message predicate:
// no token_usage requirement (Copilot messages never populate it) and no
// model-presence requirement (some Copilot assistant messages parse
// before a model name is known), scoping to assistant rows via m.role
// instead. Model/ExcludeModel filters are still applied per-row, same as
// duckUsageRawSQL.
func duckMatchingUsageRawSQL(f db.UsageFilter) (string, []any) {
	messageWhere, messageArgs, eventWhere, eventArgs := duckUsageSourceWheres(
		f, "", duckUsageMatchingMessageEligibility, duckUsageBoundsForFilter(f))

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

func duckCursorUsageRowsSQLForBounds(
	f db.UsageFilter, b duckUsageBounds,
) (string, []any, bool) {
	hasTermFilter := f.Termination != "" && f.Termination != "all"
	// Cursor usage rows carry no project or git branch and bypass the session
	// filter, so any filter they cannot satisfy (project, machine, branch)
	// must exclude them entirely rather than let them leak into totals.
	if len(f.ProjectFilterLabels()) > 0 ||
		len(f.ExcludedProjectFilterLabels()) > 0 ||
		len(db.CSVFilterValues(f.Machine)) > 0 || f.GitBranch != "" || f.MinUserMessages > 0 ||
		f.ExcludeOneShot || hasTermFilter ||
		f.ActiveSince != "" {
		return "", nil, false
	}
	if vals := db.CSVFilterValues(f.Agent); len(vals) > 0 && !slices.Contains(vals, "cursor") {
		return "", nil, false
	}
	if vals := db.CSVFilterValues(f.ExcludeAgent); slices.Contains(vals, "cursor") {
		return "", nil, false
	}

	where := "cu.model != ''"
	var args []any
	scope := db.NormalizeAutomatedScope(f.AutomatedScope, f.ExcludeAutomated)
	if pred := db.DuckDBQueryDialect().AutomatedScopePredicate(scope, "cu.is_headless"); pred != "" {
		where += "\n\tAND " + pred
	}
	where, args = appendDuckUsageSourceFilterClauses(
		where, args, "cu.model", f,
	)
	where, args = appendDuckUsageColumnBounds(
		where, "cu.occurred_at", b, args,
	)
	return fmt.Sprintf(duckDailyCursorUsageRowsSQLTemplate, where), args, true
}

func duckDailyUsageRawSQL(f db.UsageFilter) (string, []any) {
	bounds := duckUsageBoundsForFilter(f)
	sessionRowsSQL, sessionArgs := duckUsageRawSQL(
		duckUsageSnapshotInputFilter(f), "")
	cursorRowsSQL, cursorArgs, ok := duckCursorUsageRowsSQLForBounds(f, bounds)
	if !ok {
		return sessionRowsSQL, sessionArgs
	}
	rowsSQL := sessionRowsSQL + "\n\t\tUNION ALL\n" + cursorRowsSQL
	args := make([]any, 0, len(sessionArgs)+len(cursorArgs))
	args = append(args, sessionArgs...)
	args = append(args, cursorArgs...)
	return rowsSQL, args
}

func duckUsageLocalDateSQL(f db.UsageFilter) (string, any) {
	name := f.Timezone
	// The shared default zone converts per timestamp, so dates follow DST.
	if location := f.Location(); name == "" && location.String() != "Local" {
		name = location.String()
	}
	if name != "" {
		return "COALESCE(strftime(timezone(?, timezone('UTC', ts)), '%Y-%m-%d'), '')", name
	}
	ref := time.Now().UTC()
	if f.From != "" {
		if t, err := time.Parse(time.RFC3339, f.From+"T12:00:00Z"); err == nil {
			ref = t
		}
	}
	_, offset := ref.In(f.Location()).Zone()
	return "COALESCE(strftime(ts + (? * INTERVAL 1 SECOND), '%Y-%m-%d'), '')", offset
}

func duckUsageCTE(f db.UsageFilter, sessionID string) (string, []any) {
	rawSQL, args := duckUsageRawSQL(
		duckUsageSnapshotInputFilter(f), sessionID)
	return duckUsageCTEFromRaw(f, rawSQL, args, true)
}

func duckDailyUsageCTE(f db.UsageFilter) (string, []any) {
	rawSQL, args := duckDailyUsageRawSQL(f)
	return duckUsageCTEFromRaw(f, rawSQL, args, true)
}

func duckUsageSnapshotInputFilter(f db.UsageFilter) db.UsageFilter {
	return db.UsageFilter{From: f.From, To: f.To, Timezone: f.Timezone}
}

// duckSQLString quotes a SQL string literal. Alias names come from the
// curated pricing tables, not from stored session data.
func duckSQLString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// duckPriceModelCaseSQL renders runtime-alias canonicalization as SQL so
// each usage row carries the fixed or timestamp-selected model whose
// rates apply.
func duckPriceModelCaseSQL() string {
	var b strings.Builder
	b.WriteString("CASE\n")
	for _, alias := range pricingpkg.FixedPricingAliases() {
		modelExpr := "regexp_replace(model, '^.*/', '')"
		if alias.Exact {
			modelExpr = "model"
		}
		fmt.Fprintf(&b,
			"\t\tWHEN %s = %s THEN %s\n",
			modelExpr, duckSQLString(alias.Name), duckSQLString(alias.Canonical),
		)
	}
	aliases := pricingpkg.DateAliasedModels()
	quoted := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		quoted = append(quoted, duckSQLString(alias))
	}
	cutoff := pricingpkg.KimiModelEraCutoff.UTC().Format("2006-01-02 15:04:05")
	fmt.Fprintf(&b, `		WHEN regexp_replace(model, '^.*/', '') IN (%[1]s)
			AND (pricing_ts IS NULL OR pricing_ts >= TIMESTAMP '%[2]s')
			THEN %[3]s
		WHEN regexp_replace(model, '^.*/', '') IN (%[1]s)
			THEN %[4]s
		ELSE model
	END`,
		strings.Join(quoted, ", "), cutoff,
		duckSQLString(pricingpkg.KimiK3Canonical),
		duckSQLString(pricingpkg.KimiK26Canonical),
	)
	return b.String()
}

func duckUsageCTEFromRaw(
	f db.UsageFilter, rawSQL string, args []any,
	preferCompleteClaudeSnapshots bool,
) (string, []any) {
	localDateSQL, localDateArg := duckUsageLocalDateSQL(f)
	priceModelSQL := duckPriceModelCaseSQL()
	// Apply the local-date window BEFORE deduping so an out-of-range
	// duplicate (pulled in by the padded UTC bounds) cannot win
	// dedup_rank = 1 and suppress the in-range row. Mirrors the
	// dedup-after-date-filter order in internal/db/usage.go.
	datePred := "TRUE"
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
	rankedSource := "usage_windowed"
	var snapshotFilterArgs []any
	if preferCompleteClaudeSnapshots {
		snapshotFilter := "TRUE"
		snapshotFilter, snapshotFilterArgs = appendDuckUsageSourceFilterClauses(
			snapshotFilter, snapshotFilterArgs, "survivor.model", f)
		snapshotFilter, snapshotFilterArgs = appendDuckUsageSessionFilterClauses(
			snapshotFilter, snapshotFilterArgs, f, "")
		snapshotCTE = `,
		usage_snapshot_ranked AS (
			SELECT *,
				CASE
					WHEN claude_message_id != '' AND claude_request_id != ''
						THEN FIRST_VALUE(session_id) OVER (
							PARTITION BY claude_message_id, claude_request_id
							ORDER BY ts ASC, session_id ASC,
								COALESCE(message_ordinal, -1) ASC
						)
					ELSE session_id
				END AS snapshot_attribution_session_id,
				CASE
					WHEN claude_message_id != '' AND claude_request_id != ''
						THEN ROW_NUMBER() OVER (
							PARTITION BY claude_message_id, claude_request_id
							ORDER BY output_tokens_norm DESC, ts DESC,
								session_id DESC,
								COALESCE(message_ordinal, -1) DESC
						)
					ELSE 1
				END AS snapshot_rank,
				CASE
					WHEN claude_message_id != '' AND claude_request_id != ''
						THEN SUM(output_tokens_norm) OVER (
							PARTITION BY claude_message_id, claude_request_id
						) - MAX(output_tokens_norm) OVER (
							PARTITION BY claude_message_id, claude_request_id
						)
					ELSE 0
				END AS snapshot_deduplicated_output_tokens,
				CASE
					WHEN claude_message_id != '' AND claude_request_id != ''
						THEN MAX(web_search_requests_norm) OVER (
							PARTITION BY claude_message_id, claude_request_id
						)
					ELSE web_search_requests_norm
				END AS snapshot_web_search_requests
			FROM usage_windowed
		),
		usage_snapshot_survivors AS (
			SELECT ranked.* EXCLUDE (
				snapshot_rank, session_id, snapshot_attribution_session_id,
				web_search_requests_norm, snapshot_web_search_requests,
				project, agent, machine, user_message_count, is_automated,
				display_name, started_at, activity_at, group_key, session_name
			),
				ranked.snapshot_attribution_session_id AS session_id,
				ranked.snapshot_web_search_requests AS web_search_requests_norm,
				CASE WHEN attributed.id IS NULL THEN ranked.project
					ELSE attributed.project END AS project,
				CASE WHEN attributed.id IS NULL THEN ranked.agent
					ELSE attributed.agent END AS agent,
				CASE WHEN attributed.id IS NULL THEN ranked.machine
					ELSE attributed.machine END AS machine,
				CASE WHEN attributed.id IS NULL THEN ranked.user_message_count
					ELSE attributed.user_message_count END AS user_message_count,
				CASE WHEN attributed.id IS NULL THEN ranked.is_automated
					ELSE attributed.is_automated END AS is_automated,
				CASE WHEN attributed.id IS NULL THEN ranked.display_name ELSE COALESCE(
					attributed.display_name, attributed.session_name,
					attributed.first_message, attributed.project, attributed.id
				) END AS display_name,
				CASE WHEN attributed.id IS NULL THEN ranked.group_key ELSE attributed.group_key END AS group_key,
				CASE WHEN attributed.id IS NULL THEN ranked.session_name ELSE COALESCE(attributed.session_name, '') END AS session_name,
				CASE WHEN attributed.id IS NULL THEN ranked.started_at
					ELSE attributed.started_at END AS started_at,
				CASE WHEN attributed.id IS NULL THEN ranked.activity_at ELSE COALESCE(
					attributed.ended_at, attributed.started_at,
					attributed.created_at
				) END AS activity_at
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
	query := fmt.Sprintf(`
		WITH usage_raw AS (
			%[1]s
		),
		usage_normalized AS (
			SELECT *,
				CASE
					WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.input_tokens') AS BIGINT), 0), 0), %[4]d)
					WHEN source = 'session' THEN GREATEST(input_tokens, 0)
					ELSE LEAST(GREATEST(input_tokens, 0), %[4]d)
				END AS input_tokens_norm,
				CASE
					WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.output_tokens') AS BIGINT), 0), 0), %[4]d)
					WHEN source = 'session' THEN GREATEST(output_tokens, 0)
					ELSE LEAST(GREATEST(output_tokens, 0), %[4]d)
				END AS output_tokens_norm,
				CASE
					WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.cache_creation_input_tokens') AS BIGINT), 0), 0), %[4]d)
					WHEN source = 'session' THEN GREATEST(cache_create, 0)
					ELSE LEAST(GREATEST(cache_create, 0), %[4]d)
				END AS cache_create_norm,
					-- 1h-TTL subset of cache_create_norm from the nested
					-- Anthropic breakdown; only message rows carry it.
					CASE
						WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.cache_creation.ephemeral_1h_input_tokens') AS BIGINT), 0), 0), %[4]d)
						ELSE 0
					END AS cache_create_1h_norm,
					CASE
						WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.cache_read_input_tokens') AS BIGINT), 0), 0), %[4]d)
						WHEN source = 'session' THEN GREATEST(cache_read, 0)
						ELSE LEAST(GREATEST(cache_read, 0), %[4]d)
					END AS cache_read_norm,
					CASE
						WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.reasoning_tokens') AS BIGINT), 0), 0), %[4]d)
						WHEN source = 'session' THEN GREATEST(reasoning_tokens, 0)
						ELSE LEAST(GREATEST(reasoning_tokens, 0), %[4]d)
					END AS reasoning_tokens_norm,
					-- Anthropic bills server-side web search per request on
					-- top of tokens. Only per-message rows carry a usage
					-- blob; usage events never report server tool use.
					CASE
						WHEN source = 'message' THEN GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.server_tool_use.web_search_requests') AS BIGINT), 0), 0)
						ELSE 0
					END AS web_search_requests_norm,
				CASE
					WHEN claude_message_id != '' AND claude_request_id != ''
						THEN 'claude:' || claude_message_id || ':' || claude_request_id
					WHEN source = 'message' AND agent != '' AND source_uuid != ''
						THEN 'source:' || agent || ':' || source_uuid
					WHEN usage_dedup_key != ''
						THEN 'usage:' || usage_dedup_key
					ELSE 'row:' || session_id || ':' || source || ':' ||
						COALESCE(CAST(message_ordinal AS VARCHAR), '') || ':' ||
						COALESCE(CAST(ts AS VARCHAR), '') || ':' || model
				END AS dedup_group,
				%[2]s AS local_date,
				%[5]s AS price_model
			FROM usage_raw
		),
		usage_windowed AS (
			SELECT *
			FROM usage_normalized
			WHERE %[3]s
		)%[6]s,
		usage_ranked AS (
			SELECT *,
				ROW_NUMBER() OVER (
					PARTITION BY dedup_group
					ORDER BY ts ASC, session_id ASC,
						COALESCE(message_ordinal, -1) ASC
				) AS dedup_rank
			FROM %[7]s
		),
		usage_localized AS (
			SELECT *
			FROM usage_ranked
			WHERE dedup_rank = 1
		)`, rawSQL, localDateSQL, datePred, db.MaxPlausibleTokens,
		priceModelSQL, snapshotCTE, rankedSource)
	args = append(args, localDateArg)
	args = append(args, dateArgs...)
	args = append(args, snapshotFilterArgs...)
	return query, args
}

type duckUsageBucket struct {
	inputTok  int
	outputTok int
	cacheCr   int
	cacheRd   int
	cost      money.Money
}

type duckUsageAggregateRow struct {
	date           string
	ts             string
	pricingTS      string
	sessionID      string
	project        string
	agent          string
	machine        string
	model          string
	providerID     string
	priceModel     string
	source         string
	messageOrdinal sql.NullInt64
	displayName    string
	startedAt      string
	inputTok       int
	outputTok      int
	cacheCr        int
	cacheCr1h      int
	cacheRd        int
	billableInput  int
	// Output-rate billable tokens. SQL folds reasoning-only rows into this
	// value before grouping because reasoning is otherwise a row-level choice.
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
	groupKey, sessionName string
}

type duckSessionUsageRow struct {
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

func duckUsageLookupModel(model, ts string) string {
	timestamp, _ := readbase.ParseAnalyticsTime(ts)
	if canonical := pricingpkg.CanonicalModelForDate(model, timestamp); canonical != "" {
		return canonical
	}
	return model
}

func duckUsagePricingTimestamp(ts string) time.Time {
	timestamp, _ := readbase.ParseAnalyticsTime(ts)
	return timestamp
}

func duckSessionUsageLookupModel(r duckSessionUsageRow) string {
	return duckUsageLookupModel(r.model, r.pricingTS)
}

func duckUsageAggregateCost(
	model string,
	inputTok, outputTok, cacheCr, cacheCr1h, cacheRd int,
	billableInput, billableOutput, billableReasoning, billableCacheCr, billableCacheCr1h, billableCacheRd int,
	billableWebSearchRequests int,
	explicitCost int64,
	hasReportedCost bool,
	requestScoped bool,
	pricing *export.PricingResolver,
) (money.Money, money.Money, bool, bool, error) {
	return duckUsageAggregateResolvedCost(
		model, model, "", time.Time{},
		inputTok, outputTok, cacheCr, cacheCr1h, cacheRd,
		billableInput, billableOutput, billableReasoning,
		billableCacheCr, billableCacheCr1h, billableCacheRd,
		billableWebSearchRequests,
		explicitCost, hasReportedCost, requestScoped, pricing)
}

// duckUsageAggregateResolvedCost prices one deduped usage row.
// billableWebSearchRequests is the Anthropic per-request web search fee's
// input; the SQL zeroes it for rows that carry their own reported cost, the
// same way it zeroes the billable token counts, so a reported cost is never
// topped up with a fee it already settles.
func duckUsageAggregateResolvedCost(
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
			fmt.Errorf("pricing duckdb usage for model %q: %w", reportedModel, err)
	}
	computed, err = export.AddWebSearchFee(
		computed, billableWebSearchRequests)
	if err != nil {
		return money.Money{}, money.Money{}, false, false,
			fmt.Errorf("pricing duckdb usage for model %q: %w", reportedModel, err)
	}
	cost, err := money.Add(
		money.Money{Microdollars: explicitCost}, computed)
	if err != nil {
		return money.Money{}, money.Money{}, false, false,
			fmt.Errorf("summing duckdb usage for model %q: %w", reportedModel, err)
	}
	if hasReportedCost {
		pricing.RecordResolvedReported(reportedModel, pricedModel, lookup)
	}
	if hasComputedUsage {
		db.RecordComputedUsagePricing(
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
				fmt.Errorf("pricing duckdb reported usage cache savings for model %q: %w", reportedModel, err)
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
			fmt.Errorf("deriving duckdb cache read rate for model %q: %w", reportedModel, err)
	}
	creationRate, err := money.Sub(
		savingsRates.InputPerMTok, savingsRates.CacheWritePerMTok)
	if err != nil {
		return money.Money{}, money.Money{}, false, false,
			fmt.Errorf("deriving duckdb cache creation rate for model %q: %w", reportedModel, err)
	}
	creation1hRate, err := money.Sub(
		savingsRates.InputPerMTok, savingsRates.EffectiveCacheWrite1hPerMTok())
	if err != nil {
		return money.Money{}, money.Money{}, false, false,
			fmt.Errorf("deriving duckdb 1h cache creation rate for model %q: %w", reportedModel, err)
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
			fmt.Errorf("pricing duckdb cache savings for model %q: %w", reportedModel, err)
	}
	priced := lookup.OK
	if !hasBillableTokens && hasReportedCost {
		priced = true
	}
	return cost, savings, priced, true, nil
}

func duckSessionUsageRowCost(
	r duckSessionUsageRow, pricing *export.PricingResolver,
) (money.Money, bool, bool, error) {
	if r.cost.Valid && r.costSource != db.CopilotReportedCostSource {
		return money.Money{Microdollars: r.cost.Int64}, true, true, nil
	}
	if r.inputTok == 0 && r.outputTok == 0 && r.reasoningTok == 0 &&
		r.cacheCr == 0 && r.cacheRd == 0 && r.webSearchRequests == 0 {
		return money.Money{}, true, false, nil
	}
	_, lookup := pricing.ResolveAt(
		r.model, duckSessionUsageLookupModel(r),
		duckUsagePricingTimestamp(r.pricingTS),
	)
	if !lookup.OK {
		fee, feeErr := export.WebSearchFee(r.webSearchRequests)
		if feeErr != nil {
			return money.Money{}, false, false, feeErr
		}
		return fee, false, true, nil
	}
	_, lookup, err := pricing.ResolveBilledAt(
		r.providerID, r.model, duckSessionUsageLookupModel(r),
		duckUsagePricingTimestamp(r.pricingTS))
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
			fmt.Errorf("pricing duckdb session usage for model %q: %w", r.model, err)
	}
	cost, err = export.AddWebSearchFee(cost, r.webSearchRequests)
	if err != nil {
		return money.Money{}, false, false,
			fmt.Errorf("pricing duckdb session usage for model %q: %w", r.model, err)
	}
	return cost, true, true, nil
}

func duckSessionUsageBreakdownEntry(
	r duckSessionUsageRow,
	ordinal int,
	cost money.Money,
	priced bool,
) db.SessionUsageBreakdownEntry {
	entry := db.SessionUsageBreakdownEntry{
		Ordinal:                  ordinal,
		Source:                   r.source,
		Label:                    duckSessionUsageBreakdownLabel(r),
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

func duckSessionUsageBreakdownLabel(r duckSessionUsageRow) string {
	var ordinal *int
	if r.messageOrdinal.Valid {
		v := int(r.messageOrdinal.Int64)
		ordinal = &v
	}
	return db.SessionUsageBreakdownLabel(ordinal, r.source)
}

func (s *Store) forEachDailyUsageAggregateRow(
	ctx context.Context,
	f db.UsageFilter,
	visit func(duckUsageAggregateRow) error,
) error {
	cte, args := duckDailyUsageCTE(f)
	machineSelect := "'' AS machine"
	machineOrder := ""
	if f.Breakdowns {
		machineSelect = "machine"
		machineOrder = ", machine ASC"
	}
	// Keep one result per deduplicated usage row. CostForTokens quantizes each
	// row to whole microdollars; grouping token counts before that boundary can
	// turn several unrepresentable sub-microdollar rows into stored cost.
	query := cte + `
		SELECT session_id, local_date, project, agent, ` + machineSelect + `, model, provider_id, price_model,
			source, message_ordinal, ts, pricing_ts,
			input_tokens_norm AS input_tokens,
			output_tokens_norm AS output_tokens,
			cache_create_norm AS cache_creation_tokens,
			cache_create_1h_norm AS cache_creation_1h_tokens,
			cache_read_norm AS cache_read_tokens,
			CASE WHEN cost_microdollars IS NULL OR cost_source = 'copilot-reported' THEN input_tokens_norm ELSE 0 END AS billable_input_tokens,
			CASE
				WHEN cost_microdollars IS NOT NULL AND cost_source != 'copilot-reported' THEN 0
				WHEN output_tokens_norm = 0 THEN reasoning_tokens_norm
				ELSE output_tokens_norm
			END AS billable_output_tokens,
			CAST(0 AS BIGINT) AS billable_reasoning_tokens,
			CASE WHEN cost_microdollars IS NULL OR cost_source = 'copilot-reported' THEN cache_create_norm ELSE 0 END AS billable_cache_creation_tokens,
			CASE WHEN cost_microdollars IS NULL OR cost_source = 'copilot-reported' THEN cache_create_1h_norm ELSE 0 END AS billable_cache_creation_1h_tokens,
			CASE WHEN cost_microdollars IS NULL OR cost_source = 'copilot-reported' THEN cache_read_norm ELSE 0 END AS billable_cache_read_tokens,
			CASE WHEN cost_microdollars IS NULL OR cost_source = 'copilot-reported' THEN web_search_requests_norm ELSE 0 END AS billable_web_search_requests,
			CASE WHEN cost_microdollars IS NOT NULL AND cost_source != 'copilot-reported' THEN cost_microdollars ELSE 0 END AS explicit_cost,
			CASE WHEN cost_microdollars IS NOT NULL AND cost_source != 'copilot-reported' THEN 1 ELSE 0 END AS reported_cost_rows,
			CASE WHEN cost_microdollars IS NOT NULL AND cost_source = 'copilot-reported' THEN cost_microdollars ELSE 0 END AS authoritative_cost,
			CASE WHEN cost_microdollars IS NOT NULL AND cost_source = 'copilot-reported' THEN 1 ELSE 0 END AS authoritative_cost_rows
		FROM usage_localized
		ORDER BY session_id ASC, local_date ASC, project ASC, agent ASC` + machineOrder + `, model ASC, price_model ASC, ts ASC, COALESCE(message_ordinal, -1) ASC, source ASC, usage_dedup_key ASC`
	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("querying duckdb daily usage aggregates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r duckUsageAggregateRow
		var ts, pricingTS any
		if err := rows.Scan(
			&r.sessionID, &r.date, &r.project, &r.agent, &r.machine, &r.model,
			&r.providerID,
			&r.priceModel, &r.source, &r.messageOrdinal, &ts, &pricingTS,
			&r.inputTok, &r.outputTok, &r.cacheCr, &r.cacheCr1h, &r.cacheRd,
			&r.billableInput, &r.billableOutput, &r.billableReason,
			&r.billableCacheCr, &r.billableCacheCr1h, &r.billableCacheRd,
			&r.billableWebSearch,
			&r.explicitCost, &r.reportedCostRows,
			&r.authoritativeCost, &r.authoritativeCostRows,
		); err != nil {
			return fmt.Errorf("scanning duckdb daily usage aggregate: %w", err)
		}
		r.ts = formatDBTime(ts)
		r.pricingTS = formatDBTime(pricingTS)
		if err := visit(r); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating duckdb daily usage aggregates: %w", err)
	}
	return nil
}

func (s *Store) GetDailyUsage(
	ctx context.Context, f db.UsageFilter,
) (db.DailyUsageResult, error) {
	rateResolver, err := s.loadPricingResolver(ctx)
	if err != nil {
		return db.DailyUsageResult{}, err
	}
	type usageAccumKey struct {
		date       string
		project    string
		agent      string
		machine    string
		model      string
		providerID string
	}
	accum := map[usageAccumKey]*duckUsageBucket{}
	type sessionCost struct {
		estimated     map[usageAccumKey]money.Money
		authoritative *money.Money
	}
	sessionCosts := map[string]sessionCost{}
	useAuthoritativeCost := !f.HasModelFilter()
	projectLabels := map[string]bool{}
	var seenSessions map[string]db.UsageSessionInfo
	if !f.SkipSessionCounts {
		seenSessions = map[string]db.UsageSessionInfo{}
	}
	var totalSavings money.Money
	err = s.forEachDailyUsageAggregateRow(ctx, f, func(r duckUsageAggregateRow) error {
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
			b = &duckUsageBucket{}
			accum[key] = b
		}
		cost, savings, _, _, priceErr := duckUsageAggregateResolvedCost(
			r.model, r.priceModel, r.providerID, duckUsagePricingTimestamp(r.pricingTS),
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
		totalSavings, priceErr = money.Add(totalSavings, savings)
		if priceErr != nil {
			return fmt.Errorf("summing duckdb cache savings: %w", priceErr)
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
			return fmt.Errorf("summing duckdb usage: %w", priceErr)
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
					b = &duckUsageBucket{}
					accum[key] = b
				}
				b.cost, err = money.Add(b.cost, costs[i])
				if err != nil {
					return db.DailyUsageResult{}, fmt.Errorf(
						"summing allocated duckdb usage cost: %w", err)
				}
			}
		} else {
			for key, cost := range sc.estimated {
				b := accum[key]
				if b == nil {
					b = &duckUsageBucket{}
					accum[key] = b
				}
				b.cost, err = money.Add(b.cost, cost)
				if err != nil {
					return db.DailyUsageResult{}, fmt.Errorf(
						"summing estimated duckdb usage cost: %w", err)
				}
			}
		}
	}

	type dayMaps struct {
		models    map[string]duckUsageBucket
		projects  map[string]duckUsageBucket
		agents    map[string]duckUsageBucket
		machines  map[string]duckUsageBucket
		totalCost money.Money
	}
	days := map[string]*dayMaps{}
	for key, b := range accum {
		day := days[key.date]
		if day == nil {
			day = &dayMaps{
				models:   map[string]duckUsageBucket{},
				projects: map[string]duckUsageBucket{},
				agents:   map[string]duckUsageBucket{},
				machines: map[string]duckUsageBucket{},
			}
			days[key.date] = day
		}
		if err := addUsageBucket(day.models, key.model, *b); err != nil {
			return db.DailyUsageResult{}, err
		}
		day.totalCost, err = money.Add(day.totalCost, b.cost)
		if err != nil {
			return db.DailyUsageResult{}, fmt.Errorf(
				"summing duckdb daily cost: %w", err)
		}
		if f.Breakdowns {
			if err := addUsageBucket(day.projects, key.project, *b); err != nil {
				return db.DailyUsageResult{}, err
			}
			if err := addUsageBucket(day.agents, key.agent, *b); err != nil {
				return db.DailyUsageResult{}, err
			}
			if err := addUsageBucket(day.machines, key.machine, *b); err != nil {
				return db.DailyUsageResult{}, err
			}
		}
	}

	var result db.DailyUsageResult
	for _, date := range db.SortedKeys(days) {
		day := days[date]
		if day == nil {
			continue
		}
		entry := db.DailyUsageEntry{Date: date}
		modelNames := sortedUsageBucketKeys(day.models)
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
			for _, project := range sortedUsageBucketKeys(day.projects) {
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
			for _, agent := range sortedUsageBucketKeys(day.agents) {
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
			for _, machine := range sortedUsageBucketKeys(day.machines) {
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
				"summing duckdb usage total: %w", err)
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
	projects, err := s.BuildProjectIdentityMap(ctx, db.SortedKeys(projectLabels))
	if err != nil {
		return db.DailyUsageResult{}, err
	}
	result.Projects = export.ProjectMapForWire(projects)
	if seenSessions != nil {
		result.SessionCounts = db.NewUsageSessionCounts(seenSessions)
	}
	db.SanitizeDailyUsageProjectLabelsWithCatalog(&result, projects)
	return result, nil
}

func addUsageBucket(
	m map[string]duckUsageBucket, key string, b duckUsageBucket,
) error {
	cur := m[key]
	cur.inputTok += b.inputTok
	cur.outputTok += b.outputTok
	cur.cacheCr += b.cacheCr
	cur.cacheRd += b.cacheRd
	var err error
	cur.cost, err = money.Add(cur.cost, b.cost)
	if err != nil {
		return fmt.Errorf("summing duckdb usage breakdown cost: %w", err)
	}
	m[key] = cur
	return nil
}

func sortedUsageBucketKeys(m map[string]duckUsageBucket) []string {
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
	visit func(duckUsageAggregateRow) error,
) error {
	cte, args := duckUsageCTE(f, sessionID)
	// Top-session and session-usage callers aggregate these rows in Go only
	// after each row has been quantized to whole microdollars.
	query := cte + `
		SELECT session_id, project, agent, model, provider_id, price_model, source, message_ordinal, ts,
			pricing_ts, display_name, started_at, group_key, session_name, machine,
			input_tokens_norm AS input_tokens,
			output_tokens_norm AS output_tokens,
			snapshot_deduplicated_output_tokens,
			cache_create_norm AS cache_creation_tokens,
			cache_create_1h_norm AS cache_creation_1h_tokens,
			cache_read_norm AS cache_read_tokens,
			CASE WHEN cost_microdollars IS NULL OR cost_source = 'copilot-reported' THEN input_tokens_norm ELSE 0 END AS billable_input_tokens,
			CASE
				WHEN cost_microdollars IS NOT NULL AND cost_source != 'copilot-reported' THEN 0
				WHEN output_tokens_norm = 0 THEN reasoning_tokens_norm
				ELSE output_tokens_norm
			END AS billable_output_tokens,
			CAST(0 AS BIGINT) AS billable_reasoning_tokens,
			CASE WHEN cost_microdollars IS NULL OR cost_source = 'copilot-reported' THEN cache_create_norm ELSE 0 END AS billable_cache_creation_tokens,
			CASE WHEN cost_microdollars IS NULL OR cost_source = 'copilot-reported' THEN cache_create_1h_norm ELSE 0 END AS billable_cache_creation_1h_tokens,
			CASE WHEN cost_microdollars IS NULL OR cost_source = 'copilot-reported' THEN cache_read_norm ELSE 0 END AS billable_cache_read_tokens,
			CASE WHEN cost_microdollars IS NULL OR cost_source = 'copilot-reported' THEN web_search_requests_norm ELSE 0 END AS billable_web_search_requests,
			CASE WHEN cost_microdollars IS NOT NULL AND cost_source != 'copilot-reported' THEN cost_microdollars ELSE 0 END AS explicit_cost,
			CASE WHEN cost_microdollars IS NOT NULL AND cost_source != 'copilot-reported' THEN 1 ELSE 0 END AS reported_cost_rows,
			CASE WHEN cost_microdollars IS NOT NULL AND cost_source = 'copilot-reported' THEN cost_microdollars ELSE 0 END AS authoritative_cost,
			CASE WHEN cost_microdollars IS NOT NULL AND cost_source = 'copilot-reported' THEN 1 ELSE 0 END AS authoritative_cost_rows
		FROM usage_localized
		ORDER BY session_id ASC, model ASC, price_model ASC, ts ASC,
			COALESCE(message_ordinal, -1) ASC, source ASC, usage_dedup_key ASC`
	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("querying duckdb session usage aggregates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r duckUsageAggregateRow
		var ts, pricingTS, startedAt any
		if err := rows.Scan(
			&r.sessionID, &r.project, &r.agent, &r.model, &r.providerID,
			&r.priceModel, &r.source, &r.messageOrdinal, &ts, &pricingTS,
			&r.displayName, &startedAt, &r.groupKey, &r.sessionName, &r.machine,
			&r.inputTok, &r.outputTok, &r.snapshotDedupOutput,
			&r.cacheCr, &r.cacheCr1h, &r.cacheRd,
			&r.billableInput, &r.billableOutput, &r.billableReason,
			&r.billableCacheCr, &r.billableCacheCr1h, &r.billableCacheRd,
			&r.billableWebSearch,
			&r.explicitCost, &r.reportedCostRows,
			&r.authoritativeCost, &r.authoritativeCostRows,
		); err != nil {
			return fmt.Errorf("scanning duckdb session usage aggregate: %w", err)
		}
		r.ts = formatDBTime(ts)
		r.pricingTS = formatDBTime(pricingTS)
		r.startedAt = formatDBTime(startedAt)
		if err := visit(r); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating duckdb session usage aggregates: %w", err)
	}
	return nil
}

// sessionUsageRowCount counts the deduped usage rows that would
// contribute breakdown entries, mirroring duckSessionUsageRowCost's
// contributes rule (a non-copilot-reported explicit cost or any
// nonzero token counter) without shipping the rows.
func (s *Store) sessionUsageRowCount(
	ctx context.Context, sessionID string,
) (int, error) {
	cte, args := duckUsageCTE(db.UsageFilter{}, sessionID)
	query := cte + `
		SELECT COUNT(*)
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
			"counting duckdb session usage rows: %w", err)
	}
	return count, nil
}

func (s *Store) sessionUsageRows(
	ctx context.Context, sessionID string,
) ([]duckSessionUsageRow, error) {
	cte, args := duckUsageCTE(db.UsageFilter{}, sessionID)
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
		return nil, fmt.Errorf("querying duckdb session usage rows: %w", err)
	}
	defer rows.Close()
	var out []duckSessionUsageRow
	for rows.Next() {
		var r duckSessionUsageRow
		var ts, pricingTS any
		if err := rows.Scan(
			&r.sessionID, &r.messageOrdinal, &r.source, &ts, &pricingTS, &r.model, &r.providerID,
			&r.inputTok, &r.outputTok, &r.cacheCr, &r.cacheCr1h, &r.cacheRd,
			&r.reasoningTok, &r.webSearchRequests, &r.cost, &r.costSource,
		); err != nil {
			return nil, fmt.Errorf("scanning duckdb session usage row: %w", err)
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
		ctx, f, "", func(r duckUsageAggregateRow) error {
			a := bySession[r.sessionID]
			if a == nil {
				a = &acc{row: db.TopSessionEntry{
					SessionID: r.sessionID, DisplayName: r.displayName,
					Agent: r.agent, Project: r.project, StartedAt: r.startedAt,
					GroupKey: r.groupKey, SessionName: r.sessionName, Machine: r.machine,
				}}
				bySession[r.sessionID] = a
			}
			cost, _, _, _, priceErr := duckUsageAggregateResolvedCost(
				r.model, r.priceModel, r.providerID, duckUsagePricingTimestamp(r.pricingTS),
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
				return fmt.Errorf("summing duckdb top-session cost: %w", priceErr)
			}
			if !f.HasModelFilter() && r.authoritativeCostRows > 0 {
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
	if f.TopSessionsByGroup {
		return db.GroupTopSessions(out, limit, f.TopSessionsSort, f.TopSessionsTokenTypes)
	}
	return db.SortAndLimitTopSessions(
		out, limit, f.TopSessionsSort, f.TopSessionsTokenTypes,
	), nil
}

func (s *Store) GetUsageSessionCounts(
	ctx context.Context, f db.UsageFilter,
) (db.UsageSessionCounts, error) {
	cte, args := duckUsageCTE(f, "")
	rows, err := s.queryContext(ctx, cte+`
		SELECT DISTINCT session_id, project, agent
		FROM usage_localized
		WHERE session_id != ''
		ORDER BY session_id`, args...)
	if err != nil {
		return db.UsageSessionCounts{}, fmt.Errorf(
			"querying duckdb usage session counts: %w", err)
	}
	defer rows.Close()
	seen := map[string]db.UsageSessionInfo{}
	for rows.Next() {
		var sessionID string
		var info db.UsageSessionInfo
		if err := rows.Scan(&sessionID, &info.Project, &info.Agent); err != nil {
			return db.UsageSessionCounts{}, fmt.Errorf(
				"scanning duckdb usage session count: %w", err)
		}
		seen[sessionID] = info
	}
	if err := rows.Err(); err != nil {
		return db.UsageSessionCounts{}, fmt.Errorf(
			"iterating duckdb usage session counts: %w", err)
	}
	return db.NewUsageSessionCounts(seen), nil
}

// appendDuckUsageMatchingActivityClauses requires the session to have at
// least one row that GetUsageMatchingSessionCount's bounded branch would
// count, mirroring appendUsageMatchingActivityClauses in internal/db so
// bounded and unbounded requests agree on which sessions match.
func appendDuckUsageMatchingActivityClauses(
	where string, args []any, f db.UsageFilter,
) (string, []any) {
	var messageArgs []any
	messageWhere, messageArgs := appendDuckUsageSourceFilterClauses(
		duckUsageMatchingMessageSourceEligibility, messageArgs, "m.model", f,
	)
	var eventArgs []any
	eventWhere, eventArgs := appendDuckUsageSourceFilterClauses(
		duckUsageEventSourceEligibility, eventArgs, "ue.model", f,
	)

	where += `
		AND (
			EXISTS (
				SELECT 1
				FROM messages m
				WHERE m.session_id = s.id
					AND ` + messageWhere + `
			)
			OR EXISTS (
				SELECT 1
				FROM usage_events ue
				WHERE ue.session_id = s.id
					AND ` + eventWhere + `
			)
		)`
	args = append(args, messageArgs...)
	args = append(args, eventArgs...)
	return where, args
}

// GetUsageMatchingSessionCount counts sessions that match the usage filter
// even when they have no token-bearing usage rows. Bounded ranges are
// resolved against message/usage_events timestamps (falling back to
// s.started_at), the same shape duckUsageRawSQL already uses for the
// normal usage query, so a session whose activity falls outside the
// window but whose message timestamp falls inside it is still counted.
func (s *Store) GetUsageMatchingSessionCount(
	ctx context.Context, f db.UsageFilter,
) (int, error) {
	if f.From == "" && f.To == "" {
		where, args := appendDuckUsageSessionFilterClauses(
			"s.deleted_at IS NULL", nil, f, "")
		where, args = appendDuckUsageMatchingActivityClauses(where, args, f)

		var count int
		err := s.queryRowContext(ctx, `
			SELECT COUNT(*)
			FROM sessions s WHERE `+where, args...).Scan(&count)
		if err != nil {
			return 0, fmt.Errorf("querying matching usage sessions: %w", err)
		}
		return count, nil
	}

	query, args := duckMatchingUsageRawSQL(f)
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
		date := readbase.AnalyticsLocalDate(formatDBTime(ts), f.Timezone)
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
	var breakdownRows []duckSessionUsageRow
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
	// hasRows is "any contributing row processed" and feeds cost
	// aggregation only. HasTokenData is computed from the session
	// flags alone (sess.HasTotalOutputTokens || sess.HasPeakContextTokens),
	// exactly like SQLite and Postgres; no row-derived term may
	// contribute, so neither cost-only rows nor token-bearing rows
	// can flip it when the session flags are false.
	hasRows := false
	err = s.forEachSessionUsageAggregateRow(
		ctx, db.UsageFilter{}, sessionID,
		func(r duckUsageAggregateRow) error {
			deduplicatedOutputTokens += r.snapshotDedupOutput
			if r.authoritativeCostRows > 0 {
				v := money.Money{Microdollars: r.authoritativeCost}
				authoritativeCost = &v
			}
			cost, _, priced, contributes, priceErr := duckUsageAggregateResolvedCost(
				r.model, r.priceModel, r.providerID, duckUsagePricingTimestamp(r.pricingTS),
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
			// Cost-only copilot-reported carrier rows never contribute, so
			// they must not surface as token data or a model, matching the
			// SQLite and PostgreSQL session usage paths.
			if !contributes {
				return nil
			}
			hasRows = true
			models[r.model] = true
			totalCost, priceErr = money.Add(totalCost, cost)
			if priceErr != nil {
				return fmt.Errorf("summing duckdb session usage: %w", priceErr)
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
		cost, priced, contributes, priceErr := duckSessionUsageRowCost(r, rateResolver)
		if priceErr != nil {
			return nil, priceErr
		}
		if !contributes {
			continue
		}
		breakdown = append(breakdown, duckSessionUsageBreakdownEntry(
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
		Models:            db.SortedKeys(models),
		UnpricedModels:    db.SortedKeys(unpriced),
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
