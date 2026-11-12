package clickhouse

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"hash"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/kept"
	"go.kenn.io/agentsview/internal/money"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
	"go.kenn.io/agentsview/internal/readbase"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/ext"
	"go.kenn.io/agentsview/internal/usagefacts"
)

const chTimestampSQL = "parseDateTime64BestEffort(?, 6, 'UTC')"

const chUsageDisplayNameSQL = "COALESCE(s.display_name, s.session_name, s.first_message, s.project, s.id)"

// chLoadPricing reads the mirrored model catalog and layers the reader's
// custom rates on top. Push passes no custom rates.
func chLoadPricing(
	ctx context.Context, conn *sql.DB,
	customPricing map[string]config.CustomModelRate,
) (map[string]export.ModelRates, error) {
	rows, err := readModelPricing(ctx, conn)
	if err != nil {
		return nil, err
	}
	out := map[string]export.ModelRates{}
	count := 0
	for _, p := range rows {
		if strings.HasPrefix(p.ModelPattern, "_") {
			continue
		}
		rates := db.ModelPricingRates(p)
		out[p.ModelPattern] = rates
		count++
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
	chApplyCustomPricing(out, customPricing)
	return out, nil
}

func chApplyCustomPricing(out map[string]export.ModelRates, customPricing map[string]config.CustomModelRate) {
	for model, custom := range customPricing {
		if custom.PriceAs != "" {
			continue
		}
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

type chUsageBounds struct {
	from string
	to   string
}

func chUsageBoundsForFilter(f db.UsageFilter) chUsageBounds {
	from, to := readbase.PaddedDateBounds(f.From, f.To)
	return chUsageBounds{from: from, to: to}
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

func appendChUsageSourceFilterClauses(
	where string, args []any, modelCol string, f db.UsageFilter,
) (string, []any) {
	b := db.NewQueryBuilder(db.ClickHouseQueryDialect(), 0)
	preds := db.BuildUsageSourceFilter(f, b, modelCol)
	where = db.AppendUsagePredicates(where, preds, "\t\t\t")
	return where, append(args, b.Args()...)
}

func appendChUsageSessionFilterClauses(
	where string, args []any, f db.UsageFilter, sessionID string,
) (string, []any) {
	b := db.NewQueryBuilder(db.ClickHouseQueryDialect(), 0)
	preds := db.BuildUsageSessionFilter(f, b, sessionID)
	where = db.AppendUsagePredicates(where, preds, "\t\t\t")
	return where, append(args, b.Args()...)
}

const chDailyCursorUsageRowsSQLTemplate = `
SELECT
	'' AS session_id,
	CAST(NULL AS Nullable(Int64)) AS message_ordinal,
	cu.agent AS source,
	cu.occurred_at AS ts,
	cu.occurred_at AS pricing_ts,
	if(cu.model = '', '<unknown>', cu.model) AS model,
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
	concat(cu.agent, '-reported') AS cost_source,
	CAST(NULL AS Nullable(Int64)) AS recorded_cost_microdollars,
	'' AS project,
	cu.agent AS agent,
	'' AS machine,
	toInt64(0) AS user_message_count,
	cu.is_headless AS is_automated,
	'' AS display_name,
	'' AS group_key, '' AS session_name,
	CAST(NULL AS Nullable(DateTime64(6, 'UTC'))) AS started_at,
	cu.occurred_at AS activity_at
FROM cursor_usage_events cu
WHERE %s`

// chUsageMessageSourceEligibility is the condition a message meets when its
// usage counts, before the join to its session.
const chUsageMessageSourceEligibility = `
			m.token_usage != ''
			AND m.model != ''
			AND m.model != '<synthetic>'`

const chUsageMessageEligibility = chUsageMessageSourceEligibility + `
			AND s.deleted_at IS NULL`

const chUsageMatchingMessageSourceEligibility = `
			m.role = 'assistant'
			AND m.model != '<synthetic>'`

const chUsageMatchingMessageEligibility = chUsageMatchingMessageSourceEligibility + `
			AND s.deleted_at IS NULL`

// chUsageEventUsable renders the condition a usage event meets when its
// usage counts, from the expression of its model field.
func chUsageEventUsable(model string) string {
	return model + ` != ''`
}

var chUsageEventSourceEligibility = `
			` + chUsageEventUsable("ue.model")

var chUsageEventEligibility = chUsageEventSourceEligibility + `
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
		false, messageWhere, messageArgs, eventWhere, eventArgs)
}

// chUsageJoinedSessionColumns selects a raw usage row's session columns from
// the session s it joins.
const chUsageJoinedSessionColumns = `
			s.project AS project, s.agent AS agent, s.machine AS machine,
			s.user_message_count AS user_message_count, s.is_automated AS is_automated,
			` + chUsageDisplayNameSQL + ` AS display_name,
			s.group_key AS group_key, ifNull(s.session_name, '') AS session_name,
			s.started_at AS started_at,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS activity_at`

// chUsageUnjoinedSessionColumns gives raw usage rows read without their
// session the session columns' types and empty values.
const chUsageUnjoinedSessionColumns = `
			'' AS project, '' AS agent, '' AS machine,
			toInt64(0) AS user_message_count, false AS is_automated,
			'' AS display_name, '' AS group_key, '' AS session_name,
			CAST(NULL AS Nullable(DateTime64(6, 'UTC'))) AS started_at,
			CAST(NULL AS Nullable(DateTime64(6, 'UTC'))) AS activity_at`

// chUsageRawMessageUsage selects a messages row's usage. Message rows carry
// the token JSON, which chUsageRawNormSource reads.
var chUsageRawMessageUsage = `m.token_usage AS token_json,
			m.claude_message_id AS claude_message_id,
			m.claude_request_id AS claude_request_id,
			m.source_uuid AS source_uuid,
			'' AS usage_dedup_key,
				0 AS input_tokens, 0 AS output_tokens,
				0 AS cache_create, 0 AS cache_read,
				JSONExtractInt(m.token_usage, 'reasoning_tokens') AS reasoning_tokens,
				JSONExtractInt(m.token_usage, 'cache_creation', 'ephemeral_1h_input_tokens') AS cache_create_1h,
				JSONExtractInt(m.token_usage, 'server_tool_use', 'web_search_requests') AS web_search_requests,
				NULL AS cost_microdollars, '' AS cost_source,
				` + chRecordedCostSQL("m.token_usage") + ` AS recorded_cost_microdollars,`

// chUsageStoredMessageUsage selects a usage_messages row's usage. The view
// extracted the counters from the token JSON at insert time, so message rows
// carry them in the counter columns, which chUsageStoredNormSource reads.
const chUsageStoredMessageUsage = `'' AS token_json,
			m.claude_message_id AS claude_message_id,
			m.claude_request_id AS claude_request_id,
			m.source_uuid AS source_uuid,
			'' AS usage_dedup_key,
				m.usage_input AS input_tokens, m.usage_output AS output_tokens,
				m.usage_cache_create AS cache_create, m.usage_cache_read AS cache_read,
				m.usage_reasoning AS reasoning_tokens,
				m.usage_cache_create_1h AS cache_create_1h,
				m.usage_web AS web_search_requests,
				NULL AS cost_microdollars, '' AS cost_source,
				m.` + usageRecordedCostColumn + ` AS recorded_cost_microdollars,`

// chUsageRawSQLFromWheres renders the message and usage-event raw rows.
// Readers join each row's session and read messages.
//
// pricing renders the rows for push pricing, which reads them without their
// sessions: no pricing input comes from the session, and a batch is priced
// before its session rows land. Its rows then carry empty session columns,
// and ts is the row's own timestamp. It reads message usage from
// usage_messages, which holds only the usage columns and indexes every 64
// rows. Reading messages instead read whole index granules of the wide table
// for each session in a batch: about 1.5 million rows for 100 sessions that
// held under 3,000 usage rows on a production mirror. Pricing callers
// normalize with chUsageStoredNormSource.
func chUsageRawSQLFromWheres(
	pricing bool, messageWhere string, messageArgs []any,
	eventWhere string, eventArgs []any,
) (string, []any) {
	messageTS, eventTS := "m.timestamp", "ue.occurred_at"
	messageFrom, messageUsage := "usage_messages m", chUsageStoredMessageUsage
	messageJoin, eventJoin := "", ""
	sessionColumns := chUsageUnjoinedSessionColumns
	if !pricing {
		messageTS = "COALESCE(m.timestamp, s.started_at)"
		eventTS = "COALESCE(ue.occurred_at, s.started_at)"
		messageFrom, messageUsage = "messages m", chUsageRawMessageUsage
		messageJoin = "\n\t\tJOIN sessions s ON s.id = m.session_id"
		eventJoin = "\n\t\tJOIN sessions s ON s.id = ue.session_id"
		sessionColumns = chUsageJoinedSessionColumns
	}
	query := fmt.Sprintf(`
		SELECT m.session_id AS session_id, m.ordinal AS message_ordinal,
			'message' AS source, %[1]s AS ts,
			m.timestamp AS pricing_ts,
			m.model AS model, m.provider_id AS provider_id, %[8]s%[5]s
		FROM %[9]s%[3]s
		WHERE %[6]s
		UNION ALL
		SELECT ue.session_id AS session_id, ue.message_ordinal AS message_ordinal,
			ue.source AS source, %[2]s AS ts,
			ue.occurred_at AS pricing_ts,
			ue.model AS model, ue.provider_id AS provider_id, '' AS token_json,
			'' AS claude_message_id, '' AS claude_request_id,
			'' AS source_uuid,
			if(ue.dedup_key != '',
				concat(ue.session_id, ':', ue.source, ':', ue.dedup_key),
				concat(ue.session_id, ':', ue.source, ':id:', ue.id)
			) AS usage_dedup_key,
				ue.input_tokens AS input_tokens, ue.output_tokens AS output_tokens,
				ue.cache_creation_input_tokens AS cache_create,
				ue.cache_read_input_tokens AS cache_read,
				ue.reasoning_tokens AS reasoning_tokens,
				0 AS cache_create_1h, 0 AS web_search_requests,
				if(ue.cost_status = 'recorded' AND ue.cost_source != 'copilot-reported', NULL, ue.cost_microdollars) AS cost_microdollars,
				ue.cost_source AS cost_source,
				if(ue.cost_status = 'recorded' AND ue.cost_source != 'copilot-reported', ue.cost_microdollars, NULL) AS recorded_cost_microdollars,%[5]s
		FROM usage_events ue%[4]s
		WHERE %[7]s`,
		messageTS, eventTS, messageJoin, eventJoin, sessionColumns,
		messageWhere, eventWhere, messageUsage, messageFrom)
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
		len(db.CSVFilterValues(f.Machine)) > 0 || f.GitBranch != "" || f.MinUserMessages > 0 ||
		f.ExcludeOneShot || hasTermFilter ||
		f.ActiveSince != "" {
		return "", nil, false
	}
	where := "1"
	builder := db.NewQueryBuilder(db.ClickHouseQueryDialect(), 0)
	if pred := builder.ValuesPredicate("cu.agent", db.CSVFilterValues(f.Agent), true); pred != "" {
		where += "\n\tAND " + pred
	}
	if pred := builder.ValuesPredicate("cu.agent", db.CSVFilterValues(f.ExcludeAgent), false); pred != "" {
		where += "\n\tAND " + pred
	}
	args := builder.Args()
	scope := db.NormalizeAutomatedScope(f.AutomatedScope, f.ExcludeAutomated)
	if pred := db.ClickHouseQueryDialect().AutomatedScopePredicate(scope, "cu.is_headless"); pred != "" {
		where += "\n\tAND " + pred
	}
	where, args = appendChUsageSourceFilterClauses(
		where, args, "if(cu.model = '', '<unknown>', cu.model)", f,
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
// from the live sessions row: usage_localized joins them for the rows that
// survive deduplication. Until then the rows carry the agent they were
// prepared with, which deduplication reads, and empty session columns.
func chPreparedUsageRawSQL(state preparedUsageState, f db.UsageFilter, sessionID string) (string, []any) {
	// Model and session filters apply after Claude snapshot attribution
	// (usage_kept), exactly as the raw source does: a filtered
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
			p.recorded_cost_microdollars AS recorded_cost_microdollars,
			'' AS project, p.agent AS agent, '' AS machine,
			toInt64(0) AS user_message_count, false AS is_automated,
			'' AS display_name, '' AS group_key, '' AS session_name,
			CAST(NULL AS Nullable(DateTime64(6, 'UTC'))) AS started_at,
			CAST(NULL AS Nullable(DateTime64(6, 'UTC'))) AS activity_at,
			p.price_model AS stored_price_model, p.price_key AS stored_price_key,
			` + chUsageStoredPriceSelect() + `
		FROM (` + sourceSQL + `) p
		WHERE p.session_id IN (SELECT s.id FROM sessions s WHERE ` + where + `)`, args
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
	if len(state.raw) > 0 {
		query += "\n\t\tUNION ALL\n\t\tSELECT " + chPreparedUsageRowColumns + " FROM (" + chRawHostUsageRowsSQL() + ") WHERE " + where
		args = append(args, state.pricingDigest)
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

// withUsageDeltaTables attaches the replaced session list, the raw hosts'
// session list, and the prepared delta rows a prepared read's SQL names,
// when the state has them, to the context the read runs under.
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
	if len(state.raw) > 0 {
		table, err := usageSessionListTable("usage_raw_host_sessions", state.raw)
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

// chUsageStoredPriceSelect selects the stored price columns of the joined
// price rows p.
func chUsageStoredPriceSelect() string {
	columns := make([]string, 0, len(chUsageStoredPriceColumns))
	for _, column := range chUsageStoredPriceColumns {
		columns = append(columns, "p."+column.stored+" AS "+column.stored)
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
	return chUsageCTEFromRawSource(f, rawSQL, args, true, chPreparedUsageNormSource(), sessionID)
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
	cte, args := chUsageCTEFromRawSource(f, sessionRowsSQL, sessionArgs, true, chPreparedUsageNormSource(), "")
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
	return chUsageCTEFromRawSource(f, rawSQL, args, true, chUsageRawNormSource(), sessionID)
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
	return chClampTokens("JSONExtractInt(" + strings.Join(args, ", ") + ")")
}

// chClampTokens clamps a token count expression to [0, db.MaxPlausibleTokens].
func chClampTokens(expr string) string {
	return fmt.Sprintf("least(greatest(%s, 0), %d)", expr, db.MaxPlausibleTokens)
}

// chUsageNormSource holds the per-row expressions usage_normalized computes
// for message rows. Raw rows carry the message's token JSON; prepared rows
// already carry the normalized counters.
type chUsageNormSource struct {
	input, output, cacheCreate, cacheCreate1h, cacheRead, reasoning, web string
	priceModel, priceKey                                                 string
}

func chUsageRawNormSource() chUsageNormSource {
	return chUsageNormSource{
		input:         chClampedJSONInt("token_json", "input_tokens"),
		output:        chClampedJSONInt("token_json", "output_tokens"),
		cacheCreate:   chClampedJSONInt("token_json", "cache_creation_input_tokens"),
		cacheCreate1h: chClampedJSONInt("token_json", "cache_creation", "ephemeral_1h_input_tokens"),
		cacheRead:     chClampedJSONInt("token_json", "cache_read_input_tokens"),
		reasoning:     chClampedJSONInt("token_json", "reasoning_tokens"),
		web:           "greatest(JSONExtractInt(token_json, 'server_tool_use', 'web_search_requests'), 0)",
		priceModel:    chPriceModelCaseSQL(),
		priceKey:      chUsagePriceKeySQL,
	}
}

// chUsageStoredNormSource normalizes the message rows push pricing reads
// from usage_messages, whose counters arrive in the counter columns. It
// clamps them as chUsageRawNormSource clamps the token JSON's values, so a
// row gets the same price key from either table.
func chUsageStoredNormSource() chUsageNormSource {
	return chUsageNormSource{
		input:         chClampTokens("input_tokens"),
		output:        chClampTokens("output_tokens"),
		cacheCreate:   chClampTokens("cache_create"),
		cacheCreate1h: chClampTokens("cache_create_1h"),
		cacheRead:     chClampTokens("cache_read"),
		reasoning:     chClampTokens("reasoning_tokens"),
		web:           "greatest(web_search_requests, 0)",
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
	return chUsageNormSource{
		input: "input_tokens", output: "output_tokens",
		cacheCreate: "cache_create", cacheCreate1h: "cache_create_1h",
		cacheRead: "cache_read", reasoning: "reasoning_tokens",
		web:        "web_search_requests",
		priceModel: "if(source = 'cursor', " + chPriceModelCaseSQL() + ", stored_price_model)",
		priceKey:   "if(source = 'cursor', " + chUsagePriceKeySQL + ", stored_price_key)",
	}
}

func chUsageCTEFromRaw(
	f db.UsageFilter, rawSQL string, args []any,
	preferCompleteClaudeSnapshots bool,
) (string, []any) {
	return chUsageCTEFromRawSource(f, rawSQL, args, preferCompleteClaudeSnapshots, chUsageRawNormSource(), "")
}

// chUsageCTEFromRawSource renders the usage CTEs over rawSQL. sessionID names
// the one session rawSQL is restricted to, or is empty for a read of many
// sessions.
func chUsageCTEFromRawSource(
	f db.UsageFilter, rawSQL string, args []any,
	preferCompleteClaudeSnapshots bool, norms chUsageNormSource, sessionID string,
) (string, []any) {
	localDateSQL, localDateArg := chUsageLocalDateSQL(f)
	priceModelSQL := norms.priceModel
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
	dedupCTE, dedupArgs := chUsageSnapshotDedupCTE(f, preferCompleteClaudeSnapshots, sessionID)
	maxTok := db.MaxPlausibleTokens
	query := fmt.Sprintf(`
		WITH usage_raw AS (
			%[1]s
		),
		usage_normalized AS (
			SELECT *,
				if(source = 'message', %[7]s,
					if(source = 'session' OR session_id = '', greatest(input_tokens, 0),
						least(greatest(input_tokens, 0), %[4]d))) AS input_tokens_norm,
				if(source = 'message', %[8]s,
					if(source = 'session' OR session_id = '', greatest(output_tokens, 0),
						least(greatest(output_tokens, 0), %[4]d))) AS output_tokens_norm,
				if(source = 'message', %[9]s,
					if(source = 'session' OR session_id = '', greatest(cache_create, 0),
						least(greatest(cache_create, 0), %[4]d))) AS cache_create_norm,
				if(source = 'message', %[10]s, 0) AS cache_create_1h_norm,
				if(source = 'message', %[11]s,
					if(source = 'session' OR session_id = '', greatest(cache_read, 0),
						least(greatest(cache_read, 0), %[4]d))) AS cache_read_norm,
				if(source = 'message', %[12]s,
					if(source = 'session' OR session_id = '', greatest(reasoning_tokens, 0),
						least(greatest(reasoning_tokens, 0), %[4]d))) AS reasoning_tokens_norm,
				if(source = 'message', %[14]s, 0) AS web_search_requests_norm,
				if(claude_message_id != '',
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
				%[13]s AS price_key
			FROM usage_raw
			WHERE %[3]s
		),%[6]s`, rawSQL, localDateSQL, datePred, maxTok,
		priceModelSQL, dedupCTE,
		norms.input, norms.output, norms.cacheCreate, norms.cacheCreate1h,
		norms.cacheRead, norms.reasoning,
		norms.priceKey, norms.web,
	)
	args = append(args, localDateArg)
	args = append(args, dateArgs...)
	args = append(args, dedupArgs...)
	return query, args
}

// chUsageSessionFilterSQL renders the session and model filters of f over a
// row whose session id is sessionColumn and whose model is modelColumn, or
// "1" when f has neither. Session predicates read the session through an IN
// subquery, so the filter reads sessions once however many rows it checks.
func chUsageSessionFilterSQL(f db.UsageFilter, sessionColumn, modelColumn string) (string, []any) {
	where, args := appendChUsageSourceFilterClauses("1", nil, modelColumn, f)
	sessionWhere, sessionArgs := appendChUsageSessionFilterClauses("1", nil, f, "")
	if sessionWhere != "1" {
		where += "\n\t\t\t\tAND " + sessionColumn + " IN (SELECT s.id FROM sessions s WHERE " + sessionWhere + ")"
		args = append(args, sessionArgs...)
	}
	return where, args
}

// chUsageSnapshotDedupCTE renders usage_localized: the usage_normalized rows
// that survive Claude snapshot ranking, the session and model filters, and
// deduplication, with their session columns.
//
// Without snapshot preference, rows deduplicate by dedup_group alone and keep
// the session columns usage_raw gave them.
//
// With it, each Claude request keeps the row with the most output, credited
// to the session that saw the request first; every other row keeps the first
// row of its dedup_group. Both rankings run as one window pass over the
// columns they read, partitioned by the Claude request for Claude rows and by
// dedup_group for the rest. A Claude request's dedup_group holds just its
// survivor, so ranking it again would change nothing. The surviving rows'
// remaining columns are joined back from usage_normalized, and their session
// columns from one read of sessions. Filters apply where the old per-stage
// form applied them: before deduplication for other rows, and to the
// credited session after ranking for Claude rows.
func chUsageSnapshotDedupCTE(
	f db.UsageFilter, preferCompleteClaudeSnapshots bool, sessionID string,
) (string, []any) {
	if !preferCompleteClaudeSnapshots {
		evidenceSelect := ""
		if f.PairwiseSides {
			evidenceSelect = `,
				arrayFilter(value -> value != model, groupUniqArray(model) OVER (
					PARTITION BY dedup_group ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING
				)) AS inexact_models,
				arrayFilter(value -> value != project, groupUniqArray(project) OVER (
					PARTITION BY dedup_group ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING
				)) AS inexact_projects`
		}
		return `
		usage_localized AS (
			SELECT *,
				row_number() OVER (
					PARTITION BY dedup_group
					ORDER BY ts ASC, session_id ASC,
						COALESCE(message_ordinal, -1) ASC
				) AS dedup_rank` + evidenceSelect + `
			FROM usage_normalized
			QUALIFY dedup_rank = 1
		)`, nil
	}
	var args []any
	rowFilter, rowFilterArgs := chUsageSessionFilterSQL(f, "session_id", "model")
	inputWhere := ""
	if rowFilter != "1" {
		// Cursor rows carry no session and were filtered when read.
		inputWhere = `
			WHERE is_claude OR session_id = '' OR (` + rowFilter + `)`
		args = append(args, rowFilterArgs...)
	}
	inputSource := "usage_normalized"
	inputProject := ""
	evidenceWindows, evidenceKept, evidenceSelect := "", "", ""
	if f.PairwiseSides {
		// Evidence lists the other models and projects a row's dedup_group
		// held. Prepared rows read their project with the other session
		// columns at the end, so the ranking pass reads it here.
		inputSource = `(
				SELECT n.*, if(ps.id = '', n.project, ps.project) AS row_project
				FROM usage_normalized n
				LEFT JOIN (SELECT id, project FROM sessions) ps ON ps.id = n.session_id
			)`
		inputProject = ", row_project"
		evidenceWindows = `,
				groupUniqArray(model) OVER usage_partition AS group_models,
				groupUniqArray(row_project) OVER usage_partition AS group_projects`
		evidenceKept = `,
				if(is_claude, [], arrayFilter(value -> value != model, group_models)) AS inexact_models,
				if(is_claude, [], arrayFilter(value -> value != row_project, group_projects)) AS inexact_projects`
		evidenceSelect = ", k.inexact_models AS inexact_models, k.inexact_projects AS inexact_projects"
	}
	keptWhere := ""
	keptFilter, keptFilterArgs := chUsageSessionFilterSQL(
		f, "snapshot_attribution_session_id", "model")
	if keptFilter != "1" {
		keptWhere = `
				AND (NOT is_claude OR (` + keptFilter + `))`
		args = append(args, keptFilterArgs...)
	}
	// Attribution credits a row only to a session that has a row in the
	// read, so a one-session read joins just that session.
	attributedSessions := "sessions"
	if sessionID != "" {
		attributedSessions = "(SELECT * FROM sessions WHERE id = ?)"
		args = append(args, sessionID)
	}
	// Claude rows rank by most output, then latest; other rows by earliest.
	// One ORDER BY serves both: each row's sort keys for the other kind are
	// constant within its partition.
	return `
		usage_rank_input AS (
			SELECT session_id, ts, message_ordinal, model,
				output_tokens_norm, web_search_requests_norm,
				claude_message_id != '' AS is_claude,
				claude_message_id AS rank_message_id,
				if(is_claude, claude_request_id, '') AS rank_request_id,
				if(is_claude, '', dedup_group) AS rank_group` + inputProject + `
			FROM ` + inputSource + inputWhere + `
		),
		usage_ranked AS (
			SELECT *,
				argMin(session_id, (
					ts IS NULL, ifNull(ts, toDateTime64(0, 6, 'UTC')),
					session_id, COALESCE(message_ordinal, -1)
				)) OVER usage_partition AS snapshot_attribution_session_id,
				sum(output_tokens_norm) OVER usage_partition
					- max(output_tokens_norm) OVER usage_partition
					AS snapshot_deduplicated_output_tokens,
				max(web_search_requests_norm) OVER usage_partition
					AS snapshot_web_search_requests` + evidenceWindows + `,
				row_number() OVER (
					PARTITION BY rank_message_id, rank_request_id, rank_group
					ORDER BY if(is_claude, output_tokens_norm, 0) DESC,
						if(is_claude, ts, NULL) DESC,
						if(is_claude, session_id, '') DESC,
						if(is_claude, COALESCE(message_ordinal, -1), 0) DESC,
						if(is_claude, NULL, ts) ASC,
						if(is_claude, '', session_id) ASC,
						if(is_claude, 0, COALESCE(message_ordinal, -1)) ASC
				) AS usage_rank
			FROM usage_rank_input
			WINDOW usage_partition AS (
				PARTITION BY rank_message_id, rank_request_id, rank_group
			)
		),
		usage_kept AS (
			SELECT rank_group AS kept_group, session_id AS kept_session_id,
				ts AS kept_ts, message_ordinal AS kept_ordinal,
				if(is_claude, snapshot_attribution_session_id, session_id) AS kept_attributed_session_id,
				if(is_claude, snapshot_deduplicated_output_tokens, 0) AS kept_deduplicated_output_tokens,
				if(is_claude, snapshot_web_search_requests, web_search_requests_norm) AS kept_web_search_requests` + evidenceKept + `
			FROM usage_ranked
			WHERE usage_rank = 1` + keptWhere + `
		),
		usage_deduplicated AS (
			-- Message rows are unique by session and ordinal, so the key finds
			-- each kept row. Other rows of a dedup_group can share every key
			-- column; ANY keeps one row per key, as the ranking kept one.
			SELECT u.* EXCEPT (session_id, web_search_requests_norm),
				k.kept_attributed_session_id AS session_id,
				k.kept_web_search_requests AS web_search_requests_norm,
				k.kept_deduplicated_output_tokens AS snapshot_deduplicated_output_tokens,
				toUInt64(1) AS dedup_rank` + evidenceSelect + `
			FROM usage_normalized u
			ANY INNER JOIN usage_kept k
				ON k.kept_group = if(u.claude_message_id = '', u.dedup_group, '')
				AND k.kept_session_id = u.session_id
				AND isNotDistinctFrom(k.kept_ts, u.ts)
				AND isNotDistinctFrom(k.kept_ordinal, u.message_ordinal)
		),
		usage_localized AS (
			-- Cursor rows have no session and keep the columns they were read with.
			SELECT d.* EXCEPT (` + chUsageSessionColumnNames + `),
				if(a.id = '', d.project, a.project) AS project,
				if(a.id = '', d.agent, a.agent) AS agent,
				if(a.id = '', d.machine, a.machine) AS machine,
				if(a.id = '', d.user_message_count, a.user_message_count) AS user_message_count,
				if(a.id = '', d.is_automated, a.is_automated) AS is_automated,
				if(a.id = '', d.display_name, COALESCE(
					a.display_name, a.session_name, a.first_message, a.project, a.id
				)) AS display_name,
				if(a.id = '', d.group_key, a.group_key) AS group_key,
				if(a.id = '', d.session_name, ifNull(a.session_name, '')) AS session_name,
				if(a.id = '', d.started_at, a.started_at) AS started_at,
				if(a.id = '', d.activity_at, COALESCE(a.ended_at, a.started_at, a.created_at)) AS activity_at
			FROM usage_deduplicated d
			LEFT JOIN ` + attributedSessions + ` a ON a.id = d.session_id
		)`, args
}

// chUsageSessionColumnNames lists the session columns usage_raw selects,
// which usage_localized replaces with the credited session's values.
const chUsageSessionColumnNames = "project, agent, machine, user_message_count, is_automated, " +
	"display_name, group_key, session_name, started_at, activity_at"

type chUsageBucket struct {
	inputTok  int
	outputTok int
	cacheCr   int
	cacheRd   int
	cost      money.Money
}

// CHUsageAggregateRow holds the fields shared by daily and session usage rows.
type CHUsageAggregateRow struct {
	Date                  string
	TS                    string
	PricingTS             string
	SessionID             string
	Project               string
	Agent                 string
	Machine               string
	Model                 string
	ProviderID            string
	PriceModel            string
	Source                string
	MessageOrdinal        sql.NullInt64
	DisplayName           string
	StartedAt             string
	InputTok              int
	OutputTok             int
	CacheCr               int
	CacheCr1h             int
	CacheRd               int
	BillableInput         int
	BillableOutput        int
	BillableReason        int
	BillableCacheCr       int
	BillableCacheCr1h     int
	BillableCacheRd       int
	BillableWebSearch     int
	ExplicitCost          int64
	ReportedCostRows      int
	AuthoritativeCost     int64
	AuthoritativeCostRows int
	SnapshotDedupOutput   int
	GroupKey, SessionName string
	// BreakdownEntry reports that the row is one of the session's usage
	// breakdown entries; see chSessionUsageBreakdownEntrySQL.
	BreakdownEntry bool
	// RecordedCost is a harness-recorded cost the SQL left out of
	// ExplicitCost; applyRecordedCost decides whether it prices the row.
	RecordedCost sql.NullInt64
}

// applyRecordedCost makes a harness-recorded cost the row's price when
// db.ReportedCostForPricing keeps it: the catalog has no rate for the model
// and the cost is above zero. It then counts as a reported cost and nothing
// on the row is billed from tokens. It reports whether it kept the cost.
func (r *CHUsageAggregateRow) applyRecordedCost(pricing *export.PricingResolver) bool {
	kept := db.ReportedCostForPricing(r.RecordedCost, usagefacts.CostStatusRecorded,
		func() bool {
			_, lookup := pricing.ResolveAt(
				r.Model, r.PriceModel, chUsagePricingTimestamp(r.PricingTS))
			return lookup.OK
		})
	if !kept.Valid {
		return false
	}
	r.ExplicitCost += kept.Int64
	r.ReportedCostRows++
	r.BillableInput, r.BillableOutput, r.BillableReason = 0, 0, 0
	r.BillableCacheCr, r.BillableCacheCr1h, r.BillableCacheRd = 0, 0, 0
	r.BillableWebSearch = 0
	return true
}

// chSessionUsageBreakdownEntrySQL selects the usage_localized rows that a
// session's usage breakdown lists: rows with a reported cost or a token count.
const chSessionUsageBreakdownEntrySQL = `(cost_microdollars IS NOT NULL AND cost_source != 'copilot-reported')
			OR input_tokens_norm != 0
			OR output_tokens_norm != 0
			OR cache_create_norm != 0
			OR cache_read_norm != 0
			OR reasoning_tokens_norm != 0
			OR web_search_requests_norm != 0`

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
	// recordedCost is a harness-recorded cost the SQL left out of cost.
	recordedCost sql.NullInt64
}

func chUsageLookupModel(model, ts string) string {
	timestamp, _ := readbase.ParseAnalyticsTime(ts)
	if canonical := pricingpkg.CanonicalModelForDate(model, timestamp); canonical != "" {
		return canonical
	}
	return model
}

// chCatalogPricesAt returns a db.ReportedCostForPricing probe for a
// ClickHouse usage row.
func chCatalogPricesAt(
	pricing *export.PricingResolver, model, pricingTS string,
) func() bool {
	return func() bool {
		_, lookup := pricing.ResolveAt(
			model, chUsageLookupModel(model, pricingTS),
			chUsagePricingTimestamp(pricingTS))
		return lookup.OK
	}
}

func chUsagePricingTimestamp(ts string) time.Time {
	timestamp, _ := readbase.ParseAnalyticsTime(ts)
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

func chSessionUsageRowCost(
	r chSessionUsageRow, pricing *export.PricingResolver,
) (money.Money, bool, bool, error) {
	if r.cost.Valid && r.costSource != db.CopilotReportedCostSource {
		return money.Money{Microdollars: r.cost.Int64}, true, true, nil
	}
	if recorded := db.ReportedCostForPricing(r.recordedCost, usagefacts.CostStatusRecorded,
		chCatalogPricesAt(pricing, r.model, r.pricingTS)); recorded.Valid {
		return money.Money{Microdollars: recorded.Int64}, true, true, nil
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
	CHUsageAggregateRow
	Explicit        bool
	Kind            string
	ContextID       string
	BandAbove       int64
	Events          int
	TokenCost       int64
	Savings         int64
	PriceError      string
	Overflow        bool
	InexactModels   []string
	InexactProjects []string
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
	top bool,
	visit func(chDailyUsageGroupRow) error,
) error {
	// The state and the key come from the caller's parts snapshot, taken
	// before any read the rows depend on.
	state, err := s.preparedUsageState(ctx)
	if err != nil {
		return err
	}
	var memoSlot, memoVersion string
	if !top {
		memoVersion, err = s.usageReadFingerprint(ctx, state, pricingDigest)
		if err != nil {
			return err
		}
		keyFilter := s.usageDiskFilter(ctx, state, f, memoVersion)
		if memoVersion != "" {
			memoSlot, err = usageRowMemoKeyFor("daily", keyFilter, "", pricingDigest, customModels)
		}
		if err != nil {
			return err
		}
		if rows, ok := s.dailyUsageRows.Get(memoSlot, memoVersion); ok {
			if state.ready && len(state.raw) == 0 {
				s.usageDisk.Keep(memoSlot, usageDiskVersion+memoVersion, func() ([]byte, error) { return encodeUsageRows(rows, dailyUsageRowsBytes, dailyUsageRowBytes) })
			}
			for _, r := range rows {
				if err := visit(r); err != nil {
					return err
				}
			}
			return nil
		}
	}
	// Persist only fully prepared reads.
	diskReady := !top && s.usageDisk.Enabled() && state.ready && len(state.raw) == 0 && memoSlot != ""
	if diskReady {
		if data, ok := s.usageDisk.Get(memoSlot, usageDiskVersion+memoVersion); ok {
			if keptRows, ok := decodeUsageRows[chDailyUsageGroupRow](data); ok {
				for _, row := range keptRows {
					if err := visit(row); err != nil {
						return err
					}
				}
				s.dailyUsageRows.Put(memoSlot, memoVersion, keptRows)
				return nil
			}
		}
	}
	var source chUsageSource
	if top {
		source = usageCTEFor(state, f, "")
	} else {
		source, err = s.dailyUsageCTE(ctx, state, f)
		if err != nil {
			return err
		}
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
	evidenceSelect := ""
	if f.PairwiseSides && !top {
		evidenceSelect = ", groupUniqArrayArray(inexact_models), groupUniqArrayArray(inexact_projects)"
	}
	// Visit rows in time order so the last Copilot-reported total is the session cost.
	order := `ORDER BY session_id ASC, local_date ASC, project ASC, agent ASC, group_machine ASC,
			row_ts ASC, COALESCE(row_message_ordinal, -1) ASC, row_source ASC,
			any(usage_dedup_key) ASC, model ASC, row_price_model ASC`
	if top {
		order = `ORDER BY session_id ASC, row_ts ASC, COALESCE(row_message_ordinal, -1) ASC,
			row_source ASC, any(usage_dedup_key) ASC, model ASC, row_price_model ASC`
	}
	query := cte + pricedSQL + `,
		usage_classified AS (
			SELECT *,
				` + machineSelect + ` AS group_machine,
				authoritative_cost_rows = 1 OR p_priced != 1 OR ` + customPred + `
					OR recorded_cost_microdollars IS NOT NULL AS explicit_row,
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
			if(explicit_row OR price_kind != '` + chUsagePriceKindRequest + `', -1, p_band) AS group_band,
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
			any(cache_create_1h_norm),
			any(billable_input_tokens), any(billable_output_tokens),
			any(billable_reasoning_tokens), any(billable_cache_creation_tokens),
			any(billable_cache_creation_1h_tokens), any(billable_cache_read_tokens),
			toInt64(any(reported_cost_rows)),
			any(authoritative_cost), toInt64(any(authoritative_cost_rows)),
			any(recorded_cost_microdollars)` + evidenceSelect + `
		FROM usage_classified
		GROUP BY session_id, local_date, project, agent, group_machine, model, provider_id,
			explicit_key, group_kind, group_context, group_band
		` + order
	args = append(args, pricedArgs...)
	args = append(args, customArgs...)
	readCtx, err := withUsageDeltaTables(ctx, state)
	if err != nil {
		return err
	}
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
		targets := []any{
			&r.SessionID, &r.Date, &r.Project, &r.Agent, &r.Machine, &r.Model,
			&r.ProviderID,
			&explicitKey, &r.Kind, &r.ContextID, &r.BandAbove, &r.Events,
			&r.InputTok, &r.OutputTok, &r.CacheCr, &r.CacheRd,
			&r.TokenCost, &r.Savings,
			&r.ExplicitCost, &r.BillableWebSearch,
			&r.PriceError, &r.Overflow,
			&r.PriceModel, &r.Source, &r.MessageOrdinal, &ts, &pricingTS,
			&r.CacheCr1h,
			&r.BillableInput, &r.BillableOutput, &r.BillableReason,
			&r.BillableCacheCr, &r.BillableCacheCr1h, &r.BillableCacheRd,
			&r.ReportedCostRows,
			&r.AuthoritativeCost, &r.AuthoritativeCostRows,
			&r.RecordedCost,
		}
		if f.PairwiseSides && !top {
			targets = append(targets, &r.InexactModels, &r.InexactProjects)
		}
		if err := rows.Scan(targets...); err != nil {
			return fmt.Errorf("scanning clickhouse daily usage aggregate: %w", err)
		}
		r.Explicit = explicitKey != ""
		r.TS = formatDBTime(ts)
		r.PricingTS = formatDBTime(pricingTS)
		if !top {
			memo = append(memo, r)
		}
		if err := visit(r); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating clickhouse daily usage aggregates: %w", err)
	}
	if !top && (!state.ready || len(state.raw) != 0 || s.usageCaptureStable(ctx)) {
		s.dailyUsageRows.Put(memoSlot, memoVersion, memo)
		if diskReady {
			if data, err := encodeUsageRows(memo, dailyUsageRowsBytes, dailyUsageRowBytes); err == nil {
				s.usageDisk.Put(memoSlot, usageDiskVersion+memoVersion, data)
			}
		}
	}
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
	if r.PriceError != "" {
		return money.Money{}, money.Money{}, decodeUsagePriceError(r.PriceError)
	}
	if r.Overflow {
		return money.Money{}, money.Money{}, fmt.Errorf(
			"summing clickhouse usage for model %q: %w", r.Model, money.ErrOverflow)
	}
	priceContext, ok := contexts[r.ContextID]
	if !ok {
		return money.Money{}, money.Money{}, fmt.Errorf(
			"%w: missing context %q for model %q under pricing digest %s",
			errUsagePriceContextChanged, r.ContextID, r.Model, pricingDigest)
	}
	if err := priceContext.record(pricing, r.Kind, r.BandAbove, r.Events); err != nil {
		return money.Money{}, money.Money{}, err
	}
	cost, err := money.Add(
		money.Money{Microdollars: r.ExplicitCost},
		money.Money{Microdollars: r.TokenCost})
	if err != nil {
		return money.Money{}, money.Money{},
			fmt.Errorf("summing clickhouse usage for model %q: %w", r.Model, err)
	}
	cost, err = export.AddWebSearchFee(cost, r.BillableWebSearch)
	if err != nil {
		return money.Money{}, money.Money{},
			fmt.Errorf("pricing clickhouse usage for model %q: %w", r.Model, err)
	}
	return cost, money.Money{Microdollars: r.Savings}, nil
}

func chUsageGroupCost(
	r chDailyUsageGroupRow,
	contexts map[string]chUsagePriceContext,
	pricingDigest string,
	pricing *export.PricingResolver,
) (money.Money, money.Money, error) {
	if !r.Explicit {
		return chPricedUsageGroupCost(r, contexts, pricingDigest, pricing)
	}
	r.applyRecordedCost(pricing)
	cost, savings, _, _, err := chUsageAggregateResolvedCost(
		r.Model, r.PriceModel, r.ProviderID, chUsagePricingTimestamp(r.PricingTS),
		r.InputTok, r.OutputTok, r.CacheCr, r.CacheCr1h, r.CacheRd,
		r.BillableInput, r.BillableOutput, r.BillableReason,
		r.BillableCacheCr, r.BillableCacheCr1h, r.BillableCacheRd,
		r.BillableWebSearch,
		r.ExplicitCost,
		r.ReportedCostRows > 0,
		db.UsageSourceIsRequestScoped(r.Source) || r.MessageOrdinal.Valid,
		pricing,
	)
	return cost, savings, err
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

// chDailyUsageLoadAttempts bounds how often a read restarts with stored
// prices because a push added price contexts between the context load and
// the usage query.
const chDailyUsageLoadAttempts = 3

var errUsagePriceContextChanged = errors.New("usage price context changed during read")

func (s *Store) GetDailyUsage(
	ctx context.Context, f db.UsageFilter,
) (db.DailyUsageResult, error) {
	// One parts snapshot names the prepared state and the kept read alike,
	// so a push that lands during the read makes the next request miss.
	ctx, err := s.withPartsSnapshot(ctx)
	if err != nil {
		return db.DailyUsageResult{}, err
	}
	snapshot, err := s.pricingSnapshot(ctx)
	if err != nil {
		return db.DailyUsageResult{}, err
	}
	return retryUsagePriceContexts(snapshot, func(snapshot *pricingSnapshot) (db.DailyUsageResult, error) {
		return s.dailyUsageForCatalog(ctx, f, snapshot)
	})
}

// chUsageUnstoredDigest names no price records, so a read under it prices
// every row from the catalog per request.
const chUsageUnstoredDigest = ""

// retryUsagePriceContexts runs load, and again with fresh contexts when a
// price record names a context the read did not load. When every attempt
// finds one, the read prices every row per request instead of failing:
// besides a push adding contexts mid-read, a push that revives a digest
// while another push drops it leaves records whose contexts are gone for
// good.
func retryUsagePriceContexts[T any](
	snapshot *pricingSnapshot, load func(*pricingSnapshot) (T, error),
) (T, error) {
	for range chDailyUsageLoadAttempts {
		result, err := load(snapshot)
		if !errors.Is(err, errUsagePriceContextChanged) {
			return result, err
		}
	}
	unstored := *snapshot
	unstored.catalog.digest = chUsageUnstoredDigest
	return load(&unstored)
}

func (s *Store) usagePricingForCatalog(
	ctx context.Context, snapshot *pricingSnapshot,
) (*export.PricingResolver, map[string]chUsagePriceContext, [][2]string, error) {
	catalog := snapshot.catalog
	resolver := export.NewPricingResolverWithDigest(catalog.rows, snapshot.catalogDigest)
	contexts, err := loadUsagePriceContexts(ctx, s.conn, catalog.digest)
	if err != nil {
		return nil, nil, nil, err
	}
	var customModels [][2]string
	if len(s.customPricing) > 0 {
		customModels = chCustomPricedModels(contexts, resolver)
	}
	return resolver, contexts, customModels, nil
}

// Each attempt owns its accumulator and resolver. If a push adds a context
// during the query, retry with fresh contexts instead of retaining raw rows.
func (s *Store) dailyUsageForCatalog(
	ctx context.Context, f db.UsageFilter, snapshot *pricingSnapshot,
) (db.DailyUsageResult, error) {
	catalog := snapshot.catalog
	rateResolver, priceContexts, customModels, err := s.usagePricingForCatalog(ctx, snapshot)
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
	accum := map[usageAccumKey]*chUsageBucket{}
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
	var sides *db.UsagePairwiseSides
	if f.PairwiseSides {
		sides = db.NewUsagePairwiseSides()
	}
	var totalSavings money.Money
	err = s.forEachDailyUsageGroupRow(ctx, f, catalog.digest, customModels, false, func(r chDailyUsageGroupRow) error {
		if sides != nil {
			for _, model := range r.InexactModels {
				sides.Models.MarkInexact(model)
			}
			for _, project := range r.InexactProjects {
				sides.Projects.MarkInexact(project)
			}
		}
		key := usageAccumKey{
			date: r.Date, project: r.Project, agent: r.Agent,
			machine: r.Machine, model: r.Model, providerID: r.ProviderID,
		}
		if r.Project != "" {
			projectLabels[r.Project] = true
		}
		if seenSessions != nil && r.SessionID != "" {
			seenSessions[r.SessionID] = db.UsageSessionInfo{
				Project: r.Project,
				Agent:   r.Agent,
			}
		}
		b := accum[key]
		if b == nil {
			b = &chUsageBucket{}
			accum[key] = b
		}
		var cost, savings money.Money
		var priceErr error
		cost, savings, priceErr = chUsageGroupCost(
			r, priceContexts, catalog.digest, rateResolver)
		if priceErr != nil {
			return priceErr
		}
		totalSavings, priceErr = money.Add(totalSavings, savings)
		if priceErr != nil {
			return fmt.Errorf(
				"summing clickhouse cache savings: %w", priceErr)
		}
		b.inputTok += r.InputTok
		b.outputTok += r.OutputTok
		b.cacheCr += r.CacheCr
		b.cacheRd += r.CacheRd
		sc := sessionCosts[r.SessionID]
		if sc.estimated == nil {
			sc.estimated = map[usageAccumKey]money.Money{}
		}
		sc.estimated[key], priceErr = money.Add(sc.estimated[key], cost)
		if priceErr != nil {
			return fmt.Errorf(
				"summing clickhouse usage: %w", priceErr)
		}
		if useAuthoritativeCost && r.AuthoritativeCostRows > 0 {
			v := money.Money{Microdollars: r.AuthoritativeCost}
			sc.authoritative = &v
			rateResolver.RecordUnattributedReported()
		}
		sessionCosts[r.SessionID] = sc
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
		// A model filter uses estimates even when Copilot reports a session total.
		if sides != nil {
			for key, estimated := range sc.estimated {
				sides.AddEstimate(key.model, estimated)
				sides.AddMembership(key.model, key.project, sessionID)
			}
		}
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
			costs, err := export.AllocateCostByWeightContext(ctx, *sc.authoritative, weights)
			if err != nil {
				return db.DailyUsageResult{}, err
			}
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
		if sides != nil {
			totals := db.UsageTotals{InputTokens: b.inputTok, OutputTokens: b.outputTok, CacheCreationTokens: b.cacheCr, CacheReadTokens: b.cacheRd}
			totals.TotalCost = b.cost
			sides.AddBucket(key.model, key.project, totals)
		}
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
	for _, date := range db.SortedKeys(days) {
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
	projects, err := s.BuildProjectIdentityMap(ctx, db.SortedKeys(projectLabels))
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
	result.Pairwise = sides
	if sides != nil && result.Bytes() > kept.DefaultMaxBytes {
		result.Pairwise = nil
	}
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
	visit func(CHUsageAggregateRow) error,
) error {
	var state preparedUsageState
	memoSlot, memoVersion, err := s.usageRowMemoKey(ctx, state, "session", f, sessionID, "", nil)
	if err != nil {
		return err
	}
	if rows, ok := s.sessionAggregateRows.Get(memoSlot, memoVersion); ok {
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
			pricing_ts, display_name, started_at, group_key, session_name, machine,
			input_tokens_norm AS input_tokens,
			output_tokens_norm AS output_tokens,
			snapshot_deduplicated_output_tokens,
			cache_create_norm AS cache_creation_tokens,
			cache_create_1h_norm AS cache_creation_1h_tokens,
			cache_read_norm AS cache_read_tokens,` + chUsageBillableSelect + `,
			toBool(` + chSessionUsageBreakdownEntrySQL + `) AS breakdown_entry,
			recorded_cost_microdollars
		FROM usage_localized
		-- Visit rows in time order so the last Copilot-reported total is the session cost.
		ORDER BY session_id ASC, ts ASC, COALESCE(message_ordinal, -1) ASC,
			source ASC, usage_dedup_key ASC, model ASC, price_model ASC`
	readCtx, err := withUsageDeltaTables(ctx, state)
	if err != nil {
		return err
	}
	rows, err := s.queryContext(readCtx, query, args...)
	if err != nil {
		return fmt.Errorf("querying clickhouse session usage aggregates: %w", err)
	}
	defer rows.Close()
	var memo []CHUsageAggregateRow
	for rows.Next() {
		var r CHUsageAggregateRow
		var ts, pricingTS, startedAt any
		if err := rows.Scan(
			&r.SessionID, &r.Project, &r.Agent, &r.Model, &r.ProviderID,
			&r.PriceModel, &r.Source, &r.MessageOrdinal, &ts, &pricingTS,
			&r.DisplayName, &startedAt, &r.GroupKey, &r.SessionName, &r.Machine,
			&r.InputTok, &r.OutputTok, &r.SnapshotDedupOutput,
			&r.CacheCr, &r.CacheCr1h, &r.CacheRd,
			&r.BillableInput, &r.BillableOutput, &r.BillableReason,
			&r.BillableCacheCr, &r.BillableCacheCr1h, &r.BillableCacheRd,
			&r.BillableWebSearch,
			&r.ExplicitCost, &r.ReportedCostRows,
			&r.AuthoritativeCost, &r.AuthoritativeCostRows,
			&r.BreakdownEntry,
			&r.RecordedCost,
		); err != nil {
			return fmt.Errorf("scanning clickhouse session usage aggregate: %w", err)
		}
		r.TS = formatDBTime(ts)
		r.PricingTS = formatDBTime(pricingTS)
		r.StartedAt = formatDBTime(startedAt)
		memo = append(memo, r)
		if err := visit(r); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating clickhouse session usage aggregates: %w", err)
	}
	s.sessionAggregateRows.Put(memoSlot, memoVersion, memo)
	return nil
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
			cost_microdollars, cost_source, recorded_cost_microdollars
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
			&r.recordedCost,
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
	// One parts snapshot names the prepared state and the kept read alike,
	// so a push that lands during the read makes the next request miss.
	ctx, err := s.withPartsSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	state, err := s.preparedUsageState(ctx)
	if err != nil {
		return nil, err
	}
	pricing, err := s.pricingSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	// Keep one total per session rather than every usage row. Grouped reads
	// also keep member metadata so repeat reads avoid scanning every title.
	memoVersion, err := s.usageReadFingerprint(ctx, state, pricing.catalog.digest)
	if err != nil {
		return nil, err
	}
	keyFilter := s.usageDiskFilter(ctx, state, f, memoVersion)
	var memoSlot string
	if memoVersion != "" {
		memoSlot, err = usageRowMemoKeyFor("top", keyFilter, "", pricing.digest+"|"+pricing.catalog.digest, nil)
	}
	if err != nil {
		return nil, err
	}
	diskReady := s.usageDisk.Enabled() && state.ready && len(state.raw) == 0 && memoSlot != ""
	out, ok := s.topSessionTotals.Get(memoSlot, memoVersion)
	if ok && diskReady {
		s.usageDisk.Keep(memoSlot, topUsageDiskVersion+memoVersion, func() ([]byte, error) { return encodeUsageRows(out, kept.DefaultMaxBytes, db.TopSessionBytes) })
	}
	if !ok && diskReady {
		if data, found := s.usageDisk.Get(memoSlot, topUsageDiskVersion+memoVersion); found {
			out, ok = decodeUsageRows[db.TopSessionEntry](data)
			if ok {
				s.topSessionTotals.Put(memoSlot, memoVersion, out)
			}
		}
	}
	if !ok {
		catalogDigest := pricing.catalog.digest
		out, err = retryUsagePriceContexts(pricing, func(pricing *pricingSnapshot) ([]db.TopSessionEntry, error) {
			catalogDigest = pricing.catalog.digest
			return s.topSessionsForCatalog(ctx, f, pricing)
		})
		if err != nil {
			return nil, err
		}
		if f.TopSessionsByGroup {
			if err := s.loadTopSessionMetadata(ctx, out); err != nil {
				return nil, err
			}
		}
		if catalogDigest == pricing.catalog.digest && (!state.ready || len(state.raw) != 0 || s.usageCaptureStable(ctx)) {
			s.topSessionTotals.Put(memoSlot, memoVersion, out)
			if diskReady {
				if data, err := encodeUsageRows(out, kept.DefaultMaxBytes, db.TopSessionBytes); err == nil {
					s.usageDisk.Put(memoSlot, topUsageDiskVersion+memoVersion, data)
				}
			}
		}
	}
	if f.TopSessionsByGroup {
		return db.GroupTopSessions(out, limit, f.TopSessionsSort, f.TopSessionsTokenTypes)
	}
	out = db.SortAndLimitTopSessions(slices.Clone(out), limit, f.TopSessionsSort, f.TopSessionsTokenTypes)
	if err := s.loadTopSessionMetadata(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) loadTopSessionMetadata(ctx context.Context, entries []db.TopSessionEntry) error {
	if len(entries) == 0 {
		return nil
	}
	ids := make([]string, len(entries))
	byID := make(map[string]int, len(entries))
	for i, entry := range entries {
		ids[i] = entry.SessionID
		byID[entry.SessionID] = i
	}
	table, err := usageSessionListTable("usage_top_sessions", ids)
	if err != nil {
		return err
	}
	rows, err := s.queryContext(chdriver.Context(ctx, chdriver.WithExternalTable(table)), `
		SELECT s.id,
			`+chUsageDisplayNameSQL+`,
			s.started_at, s.group_key, ifNull(s.session_name, ''), s.machine
		FROM sessions s
		WHERE s.id IN (SELECT id FROM usage_top_sessions)`)
	if err != nil {
		return fmt.Errorf("querying clickhouse top-session metadata: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, displayName, groupKey, sessionName, machine string
		var startedAt any
		if err := rows.Scan(&id, &displayName, &startedAt, &groupKey, &sessionName, &machine); err != nil {
			return fmt.Errorf("scanning clickhouse top-session metadata: %w", err)
		}
		entry := &entries[byID[id]]
		entry.DisplayName = displayName
		entry.StartedAt = formatDBTime(startedAt)
		entry.GroupKey = groupKey
		entry.SessionName = sessionName
		entry.Machine = machine
	}
	return rows.Err()
}

func (s *Store) topSessionsForCatalog(
	ctx context.Context, f db.UsageFilter, snapshot *pricingSnapshot,
) ([]db.TopSessionEntry, error) {
	catalog := snapshot.catalog
	rateResolver, priceContexts, customModels, err := s.usagePricingForCatalog(ctx, snapshot)
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
	err = s.forEachDailyUsageGroupRow(
		ctx, f, catalog.digest, customModels, true, func(r chDailyUsageGroupRow) error {
			a := bySession[r.SessionID]
			if a == nil {
				a = &acc{row: db.TopSessionEntry{
					SessionID: r.SessionID, Agent: r.Agent, Project: r.Project,
				}}
				bySession[r.SessionID] = a
			}
			var cost money.Money
			var priceErr error
			cost, _, priceErr = chUsageGroupCost(
				r, priceContexts, catalog.digest, rateResolver)
			if priceErr != nil {
				return priceErr
			}
			a.row.InputTokens += r.InputTok
			a.row.OutputTokens += r.OutputTok
			a.row.CacheCreationTokens += r.CacheCr
			a.row.CacheReadTokens += r.CacheRd
			a.tokens += r.InputTok + r.OutputTok + r.CacheCr + r.CacheRd
			a.cost, priceErr = money.Add(a.cost, cost)
			if priceErr != nil {
				return fmt.Errorf("summing clickhouse top-session cost: %w", priceErr)
			}
			if !f.HasModelFilter() && r.AuthoritativeCostRows > 0 {
				v := money.Money{Microdollars: r.AuthoritativeCost}
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
	return out, nil
}

func (s *Store) GetUsageSessionCounts(
	ctx context.Context, f db.UsageFilter,
) (db.UsageSessionCounts, error) {
	// One parts snapshot names the prepared state and the kept read alike,
	// so a push that lands during the read makes the next request miss.
	ctx, err := s.withPartsSnapshot(ctx)
	if err != nil {
		return db.UsageSessionCounts{}, err
	}
	state, err := s.preparedUsageState(ctx)
	if err != nil {
		return db.UsageSessionCounts{}, err
	}
	memoSlot, memoVersion, err := s.usageRowMemoKey(ctx, state, "counts", f, "", "", nil)
	if err != nil {
		return db.UsageSessionCounts{}, err
	}
	counted, ok := s.usageSessionRows.Get(memoSlot, memoVersion)
	if !ok {
		source := usageCTEFor(state, f, "")
		readCtx, err := withUsageDeltaTables(ctx, state)
		if err != nil {
			return db.UsageSessionCounts{}, err
		}
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
		s.usageSessionRows.Put(memoSlot, memoVersion, counted)
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
		version, err = s.usageReadFingerprint(ctx, state, pricingDigest)
	}
	if err != nil {
		return "", "", err
	}
	if version == "" {
		return "", "", nil
	}
	slot, err = usageRowMemoKeyFor(kind, f, sessionID, pricingDigest, customModels)
	return slot, version, err
}

// chUsageReadTables name the rows a prepared usage read depends on besides
// prepared_usage itself, whose rows the state's stamp identifies.
var chUsageReadTables = []string{
	"sessions", "usage_session_snapshots", "cursor_usage_events", "usage_event_prices",
	"usage_price_contexts", "model_pricing", "model_pricing_bands", "genai_pricing", "sync_metadata",
}

// chRawHostUsageTables are the tables a prepared read also reads while some
// host is read raw; see preparedUsageState.raw.
var chRawHostUsageTables = []string{"usage_messages", "usage_events"}

// usageReadFingerprint identifies the rows a usage read sees. Fully prepared
// reads keep their version across merges. Raw and mixed reads name parts.
// pricingDigest names the price records the read joins; see usageRowsVersion.
func (s *Store) usageReadFingerprint(ctx context.Context, state preparedUsageState, pricingDigest string) (string, error) {
	if !state.ready {
		return s.partsFingerprint(ctx)
	}
	if len(state.raw) == 0 {
		version, ok, err := s.usageRowsVersion(ctx, chUsageReadTables, pricingDigest)
		if err != nil || !ok {
			return "", err
		}
		return version + "|" + state.stamp, nil
	}
	// The raw hosts' rows come from the message and event tables.
	tables := slices.Concat(chUsageReadTables, chRawHostUsageTables)
	fingerprint, err := s.tablePartsFingerprint(ctx, tables)
	if err != nil {
		return "", err
	}
	return fingerprint + "|" + state.stamp, nil
}

type usageTableVersion struct {
	parts, identity string
}

// chUsagePriceTable keeps the price records of every pricing digest the
// mirror was ever priced under, and a push never removes a digest's records,
// so the table grows by a full set of records with each catalog change. A
// read joins only the records of one digest.
const chUsagePriceTable = "usage_event_prices"

// usageVersionKey names the rows of a table a read can see: the whole table,
// or for chUsagePriceTable only the records of pricingDigest.
func usageVersionKey(table, pricingDigest string) string {
	if table == chUsagePriceTable {
		return table + "@" + pricingDigest
	}
	return table
}

// usageRowsVersion names the rows of the tables as of the request's parts
// snapshot. The sessions, snapshot, and price record tables are followed
// by finalRows trackers, whose checks read only the keys new parts hold;
// the other, small tables are hashed again when their parts change. If
// parts move during the probe or the probe fails, the read is served
// uncached. The version names only pricingDigest's price records: hashing
// every digest's records read the whole table, which took two minutes on a
// mirror holding hundreds of digests.
func (s *Store) usageRowsVersion(ctx context.Context, tables []string, pricingDigest string) (string, bool, error) {
	snapshot, ok := ctx.Value(partsSnapshotKey{}).(partsSnapshot)
	if !ok {
		var err error
		if snapshot, err = s.readPartsSnapshot(ctx); err != nil {
			return "", false, err
		}
	}
	// The mutex shares a probe with concurrent requests, which recheck the
	// kept state after acquiring it.
	s.usageVersions.Lock()
	defer s.usageVersions.Unlock()
	identities, ok := s.usageIdentitiesLocked(ctx, snapshot, tables, pricingDigest)
	if !ok {
		return "", false, nil
	}
	return tableValuesFingerprint(tables, func(table string) string { return identities[table] }), true, nil
}

// usageIdentitiesLocked returns each table's row identity as of snapshot,
// keeping what it read. It reports false when a probe fails or the parts
// move while it reads. After a move each tracker still keeps a read that
// its parts still account for; see finalRowsChange.settle. The caller
// holds usageVersions.
func (s *Store) usageIdentitiesLocked(
	ctx context.Context, snapshot partsSnapshot, tables []string, pricingDigest string,
) (map[string]string, bool) {
	var changed []string
	var trackers []*finalRows
	var changes []trackedChange
	for _, table := range tables {
		if f := s.finalRowsTracker(table, pricingDigest); f != nil {
			change, err := s.prepareFinalRows(ctx, f, snapshot[table])
			if err != nil {
				s.logUsageReadiness(fmt.Sprintf("row identity probe failed, serving uncached: %v", err))
				return nil, false
			}
			trackers = append(trackers, f)
			if change != nil {
				changes = append(changes, trackedChange{f, change})
			}
			continue
		}
		kept, exists := s.usageVersions.tables[usageVersionKey(table, pricingDigest)]
		if !exists || kept.parts != snapshot[table].digest {
			changed = append(changed, table)
		}
	}
	read := map[string]string{}
	if len(changed) > 0 {
		var err error
		if read, err = s.readUsageRowIdentities(ctx, changed, pricingDigest); err != nil {
			s.logUsageReadiness(fmt.Sprintf("row identity probe failed, serving uncached: %v", err))
			return nil, false
		}
	}
	if len(changed) > 0 || len(changes) > 0 {
		if s.afterTrackerReads != nil {
			s.afterTrackerReads()
		}
		fresh, err := s.readPartsSnapshot(ctx)
		if err != nil {
			s.logUsageReadiness(fmt.Sprintf("parts recheck failed, serving uncached: %v", err))
			return nil, false
		}
		moved := slices.ContainsFunc(tables, func(table string) bool {
			return fresh[table].digest != snapshot[table].digest
		})
		if moved {
			// Each tracker keeps what it read while its parts still hold
			// those rows, so the next check reads only the parts that
			// changed. The trackers may now stand at different parts, so
			// their identities are not returned.
			for _, c := range changes {
				if c.change.settle(fresh[c.tracker.spec.table]) {
					c.tracker.apply(c.change)
				}
			}
			return nil, false
		}
	}
	for _, c := range changes {
		c.tracker.apply(c.change)
	}
	identities := make(map[string]string, len(tables))
	for _, f := range trackers {
		identities[f.spec.table] = f.identity()
	}
	for table, identity := range read {
		s.usageVersions.tables[usageVersionKey(table, pricingDigest)] = usageTableVersion{snapshot[table].digest, identity}
	}
	for _, table := range tables {
		if _, ok := identities[table]; !ok {
			identities[table] = s.usageVersions.tables[usageVersionKey(table, pricingDigest)].identity
		}
	}
	return identities, true
}

// trackedChange is a tracker's new state, applied once the parts it was
// read from prove stable.
type trackedChange struct {
	tracker *finalRows
	change  *finalRowsChange
}

// maxPriceTrackers bounds the pricing digests whose price records are
// tracked: the catalog digest prepared rows and reads share, the empty
// digest of the session counts, and the catalog digest a change replaced.
const maxPriceTrackers = 3

// finalRowsTracker returns the tracker of table's rows, or nil for a table
// hashed whole. The caller holds usageVersions.
func (s *Store) finalRowsTracker(table, pricingDigest string) *finalRows {
	v := &s.usageVersions
	switch table {
	case sessionsFinalTable.table:
		if v.sessions == nil {
			v.sessions = &finalRows{spec: sessionsFinalTable}
		}
		return v.sessions
	case snapshotsFinalTable.table:
		if v.snapshots == nil {
			v.snapshots = &finalRows{spec: snapshotsFinalTable}
		}
		return v.snapshots
	case pricesFinalTable.table:
		return s.priceRowsTracker(pricingDigest)
	}
	return nil
}

// priceRowsTracker returns the tracker of pricingDigest's price records.
// The caller holds usageVersions.
func (s *Store) priceRowsTracker(pricingDigest string) *finalRows {
	v := &s.usageVersions
	v.priceOrder = slices.DeleteFunc(v.priceOrder, func(d string) bool { return d == pricingDigest })
	v.priceOrder = append(v.priceOrder, pricingDigest)
	f, ok := v.prices[pricingDigest]
	if !ok {
		f = &finalRows{spec: pricesFinalTable, digest: pricingDigest}
		v.prices[pricingDigest] = f
	}
	if len(v.priceOrder) > maxPriceTrackers {
		delete(v.prices, v.priceOrder[0])
		v.priceOrder = v.priceOrder[1:]
	}
	return f
}

func chUsageRowHashSQL(columns string) string {
	return "reinterpretAsUInt128(sipHash128Reference(tuple(" + columns + ")))"
}

// readUsageRowIdentities returns each table's identity. The price table's
// identity covers only pricingDigest's records, which the table's sort key
// reads without touching the other digests.
func (s *Store) readUsageRowIdentities(
	ctx context.Context, tables []string, pricingDigest string,
) (map[string]string, error) {
	queries := make([]string, len(tables))
	var args []any
	for i, table := range tables {
		columns, where := "*", ""
		switch table {
		case "usage_session_snapshots":
			// Revision identifies the usage arrays without hashing them; push_version identifies publication.
			columns = "id, revision, push_version"
		case "sync_metadata":
			columns = "key, value"
			where = " WHERE key != ?"
			args = append(args, chExchangeProbeKey)
		case chUsagePriceTable:
			where = " WHERE pricing_digest = ?"
			args = append(args, pricingDigest)
		}
		queries[i] = fmt.Sprintf("assumeNotNull((SELECT toString(tuple(count(), sumWithOverflow(%s))) FROM %s%s))",
			chUsageRowHashSQL(columns), table, where)
	}
	identities := make([]string, len(tables))
	if err := s.queryRowContext(ctx, "SELECT ["+strings.Join(queries, ",")+"]", args...).Scan(&identities); err != nil {
		return nil, fmt.Errorf("reading clickhouse usage row identities: %w", err)
	}
	if len(identities) != len(tables) {
		return nil, fmt.Errorf("reading clickhouse usage row identities: got %d for %d tables", len(identities), len(tables))
	}
	byTable := make(map[string]string, len(tables))
	for i, table := range tables {
		byTable[table] = identities[i]
	}
	return byTable, nil
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
	if db.TerminationUsesTime(f.Termination) {
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
	return tableValuesFingerprint(tables, func(table string) string { return snapshot[table].digest }), nil
}

func tableValuesFingerprint(tables []string, value func(string) string) string {
	tables = slices.Sorted(slices.Values(tables))
	sum := sha256.New()
	for _, table := range slices.Compact(tables) {
		fmt.Fprintf(sum, "%s\x00%s\x00", table, value(table))
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// partsSnapshot maps each table to its active parts.
type partsSnapshot map[string]tableParts

// tableParts lists one table's active parts and a digest of their names and
// files.
type tableParts struct {
	digest string
	parts  []partInfo
}

type partsSnapshotKey struct{}

// readPartsSnapshot reads the active parts of every table in one query.
func (s *Store) readPartsSnapshot(ctx context.Context) (partsSnapshot, error) {
	rows, err := s.queryContext(ctx, `SELECT table, name, partition_id, min_block_number, max_block_number,
		data_version, rows, hash_of_all_files
		FROM system.parts WHERE database = currentDatabase() AND active ORDER BY table, name`)
	if err != nil {
		return nil, fmt.Errorf("reading clickhouse parts: %w", err)
	}
	defer rows.Close()
	snapshot := partsSnapshot{}
	digests := map[string]hash.Hash{}
	for rows.Next() {
		var table string
		var p partInfo
		var dataVersion uint64
		if err := rows.Scan(&table, &p.name, &p.partition, &p.minBlock, &p.maxBlock, &dataVersion, &p.rows, &p.files); err != nil {
			return nil, fmt.Errorf("scanning clickhouse parts: %w", err)
		}
		// A part's data version is its first block until a mutation
		// rewrites it.
		if int64(dataVersion) != p.minBlock {
			p.mutation = int64(dataVersion)
		}
		tp := snapshot[table]
		tp.parts = append(tp.parts, p)
		snapshot[table] = tp
		sum, ok := digests[table]
		if !ok {
			sum = sha256.New()
			digests[table] = sum
		}
		fmt.Fprintf(sum, "%s\x00%s\x00", p.name, p.files)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating clickhouse parts: %w", err)
	}
	for table, sum := range digests {
		tp := snapshot[table]
		tp.digest = hex.EncodeToString(sum.Sum(nil))
		snapshot[table] = tp
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

// GetSessionPlanUsage returns one session's plan usage row, or nil when
// the session has none.
func (s *Store) GetSessionPlanUsage(
	ctx context.Context, sessionID string,
) (*db.SessionPlanUsage, error) {
	rows, err := s.conn.QueryContext(ctx,
		`SELECT `+db.SessionPlanUsageColumns+` FROM session_plan_usage WHERE session_id = ?`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse plan usage for %s: %w", sessionID, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	p, err := db.ScanSessionPlanUsage(rows.Scan)
	if err != nil {
		return nil, fmt.Errorf("scanning clickhouse plan usage for %s: %w", sessionID, err)
	}
	return p, rows.Err()
}

// GetSessionUsageTotal returns the usage total the agent recorded for one
// session, or nil when it recorded none.
func (s *Store) GetSessionUsageTotal(
	ctx context.Context, sessionID string,
) (*db.SessionUsageTotal, error) {
	rows, err := s.conn.QueryContext(ctx,
		`SELECT `+db.SessionUsageTotalColumns+` FROM session_usage_totals WHERE session_id = ?`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse session total for %s: %w", sessionID, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	t, err := db.ScanSessionUsageTotal(rows.Scan)
	if err != nil {
		return nil, fmt.Errorf("scanning clickhouse session total for %s: %w", sessionID, err)
	}
	return t, rows.Err()
}

// sessionMayHaveUsageRows reports whether the session's usage read can
// return rows. A session with token totals is assumed to have them. For one
// without, a probe looks for a message or usage event the read would
// select; chUsageRawSQL reads no other table for a session's rows. The probe
// ignores whether the session is deleted, so it can only answer true for a
// read that turns out empty, never false for one that has rows.
func (s *Store) sessionMayHaveUsageRows(ctx context.Context, sess *db.Session) (bool, error) {
	if sess.HasTotalOutputTokens || sess.HasPeakContextTokens {
		return true, nil
	}
	var found uint64
	if err := s.queryRowContext(ctx, `SELECT count() FROM (
		SELECT 1 FROM messages m WHERE m.session_id = ? AND `+chUsageMessageSourceEligibility+` LIMIT 1
		UNION ALL
		SELECT 1 FROM usage_events ue WHERE ue.session_id = ? AND `+chUsageEventSourceEligibility+` LIMIT 1)`,
		sess.ID, sess.ID,
	).Scan(&found); err != nil {
		return false, fmt.Errorf("probing clickhouse usage rows for %s: %w", sess.ID, err)
	}
	return found > 0, nil
}

func (s *Store) GetSessionUsage(
	ctx context.Context, sessionID string, includeBreakdown bool,
) (*db.SessionUsage, error) {
	sess, err := s.GetSession(ctx, sessionID)
	if err != nil || sess == nil {
		return nil, err
	}
	mayHaveRows, err := s.sessionMayHaveUsageRows(ctx, sess)
	if err != nil {
		return nil, err
	}
	// Without usage rows there is nothing to price: the session's own
	// token totals and plan usage are the whole response.
	var rateResolver *export.PricingResolver
	var breakdownRows []chSessionUsageRow
	if mayHaveRows {
		if rateResolver, err = s.loadPricingResolver(ctx); err != nil {
			return nil, err
		}
		if includeBreakdown {
			if breakdownRows, err = s.sessionUsageRows(ctx, sessionID); err != nil {
				return nil, err
			}
		}
	}
	// Without the breakdown, its length is counted from the aggregate rows,
	// which read the same usage_localized rows.
	breakdownCount := 0
	models := map[string]bool{}
	unpriced := map[string]bool{}
	var totalCost money.Money
	var authoritativeCost *money.Money
	var hasComputedCost, hasReportedCost bool
	deduplicatedOutputTokens := 0
	hasRows := false
	if mayHaveRows {
		err = s.forEachSessionUsageAggregateRow(
			ctx, db.UsageFilter{}, sessionID,
			func(r CHUsageAggregateRow) error {
				deduplicatedOutputTokens += r.SnapshotDedupOutput
				// A cost-only evidence row is a breakdown entry only when its
				// recorded cost prices it.
				if r.applyRecordedCost(rateResolver) || r.BreakdownEntry {
					breakdownCount++
				}
				if r.AuthoritativeCostRows > 0 {
					v := money.Money{Microdollars: r.AuthoritativeCost}
					authoritativeCost = &v
				}
				cost, _, priced, contributes, priceErr := chUsageAggregateResolvedCost(
					r.Model, r.PriceModel, r.ProviderID, chUsagePricingTimestamp(r.PricingTS),
					r.InputTok, r.OutputTok, r.CacheCr, r.CacheCr1h, r.CacheRd,
					r.BillableInput, r.BillableOutput, r.BillableReason,
					r.BillableCacheCr, r.BillableCacheCr1h, r.BillableCacheRd,
					r.BillableWebSearch,
					r.ExplicitCost,
					r.ReportedCostRows > 0,
					db.UsageSourceIsRequestScoped(r.Source) || r.MessageOrdinal.Valid,
					rateResolver,
				)
				if priceErr != nil {
					return priceErr
				}
				if !contributes {
					return nil
				}
				hasRows = true
				models[r.Model] = true
				totalCost, priceErr = money.Add(totalCost, cost)
				if priceErr != nil {
					return fmt.Errorf("summing clickhouse session usage: %w", priceErr)
				}
				if r.ReportedCostRows > 0 {
					hasReportedCost = true
				}
				if r.BillableInput != 0 || r.BillableOutput != 0 ||
					r.BillableReason != 0 || r.BillableCacheCr != 0 ||
					r.BillableCacheRd != 0 || r.BillableWebSearch > 0 {
					hasComputedCost = true
				}
				if !priced {
					unpriced[r.Model] = true
				}
				return nil
			})
	}
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
		entry := chSessionUsageBreakdownEntry(r, len(breakdown)+1, cost, priced)
		if r.recordedCost.Valid {
			entry.RecordedCost = &money.Money{Microdollars: r.recordedCost.Int64}
			entry.RecordedCostPriced = db.ReportedCostForPricing(
				r.recordedCost, usagefacts.CostStatusRecorded,
				chCatalogPricesAt(rateResolver, r.model, r.pricingTS)).Valid
		}
		breakdown = append(breakdown, entry)
	}
	if authoritativeCost != nil && len(breakdown) > 0 {
		weights := make([]money.Money, len(breakdown))
		for i := range breakdown {
			weights[i] = breakdown[i].Cost
		}
		costs, err := export.AllocateCostByWeightContext(ctx, *authoritativeCost, weights)
		if err != nil {
			return nil, err
		}
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
	if out.PlanUsage, err = s.GetSessionPlanUsage(ctx, sess.ID); err != nil {
		return nil, err
	}
	if out.SessionTotal, err = s.GetSessionUsageTotal(ctx, sess.ID); err != nil {
		return nil, err
	}
	return out, nil
}
