package clickhouse

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/ext"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/readbase"
	"go.kenn.io/agentsview/internal/signals"
)

// loadAnalyticsSessions leaves paired model/time filtering to the shared base.
func (s *Store) loadAnalyticsSessions(
	ctx context.Context, f db.AnalyticsFilter,
	includeDate, includeTime bool,
	extraPred string, extraArgs []any,
) ([]readbase.AnalyticsSession, error) {
	where, args := chBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.",
		includeDate, includeTime)
	if extraPred != "" {
		where += " AND " + extraPred
		args = append(args, extraArgs...)
	}
	// Every analytics panel on a page lists the same sessions; keep the
	// rows per parts and predicate so one page reads them once.
	fingerprint, err := s.tablePartsFingerprint(ctx, []string{"sessions", "messages"})
	if err != nil {
		return nil, err
	}
	memoKey := analyticsSessionMemoKey(where, args)
	if cached, ok := s.analyticsSessionRows.get(memoKey, fingerprint); ok {
		return slices.Clone(cached), nil
	}
	// The panels of one page ask at once; one read answers them all. The
	// read outlives a caller that gives up, so the others still get it.
	shared := s.analyticsListings.DoChan(memoKey+"\x00"+fingerprint, func() (any, error) {
		out, err := s.readAnalyticsSessions(context.WithoutCancel(ctx), where, args)
		if err != nil {
			return nil, err
		}
		s.analyticsSessionRows.put(memoKey, fingerprint, out)
		return out, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-shared:
		if result.Err != nil {
			return nil, result.Err
		}
		return slices.Clone(result.Val.([]readbase.AnalyticsSession)), nil
	}
}

func (s *Store) readAnalyticsSessions(ctx context.Context, where string, args []any) ([]readbase.AnalyticsSession, error) {
	rows, err := s.queryContext(ctx, `
		SELECT id, project, machine, agent, first_message,
			COALESCE(display_name, session_name) AS display_name,
			started_at, ended_at, created_at, message_count,
			user_message_count, total_output_tokens,
			has_total_output_tokens, is_automated,
			termination_status, health_score, health_grade, outcome,
			outcome_confidence, tool_failure_signal_count,
			tool_retry_count, edit_churn_count, compaction_count,
			mid_task_compaction_count, context_pressure_max,
			quality_signal_version, short_prompt_count,
			unstructured_start, missing_success_criteria_count,
			missing_verification_count, duplicate_prompt_count,
			no_code_context_count, runaway_tool_loop_count, push_version
		FROM sessions s
		WHERE `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse analytics sessions: %w", err)
	}
	defer rows.Close()

	var out []readbase.AnalyticsSession
	for rows.Next() {
		var r readbase.AnalyticsSession
		var startedAt, endedAt, createdAt any
		if err := rows.Scan(
			&r.ID, &r.Project, &r.Machine, &r.Agent,
			&r.FirstMessage, &r.DisplayName,
			&startedAt, &endedAt, &createdAt,
			&r.MessageCount, &r.UserMessageCount,
			&r.TotalOutputTokens, &r.HasTotalOutputTokens,
			&r.IsAutomated, &r.TerminationStatus,
			&r.HealthScore, &r.HealthGrade, &r.Outcome,
			&r.OutcomeConfidence, &r.ToolFailures, &r.ToolRetries,
			&r.EditChurn, &r.Compactions, &r.MidTaskCompactions,
			&r.ContextPressureMax, &r.QualitySignalVersion,
			&r.ShortPromptCount, &r.UnstructuredStart,
			&r.MissingSuccessCriteriaCount, &r.MissingVerificationCount,
			&r.DuplicatePromptCount, &r.NoCodeContextCount,
			&r.RunawayToolLoopCount, &r.PushVersion,
		); err != nil {
			return nil, fmt.Errorf("scanning clickhouse analytics session: %w", err)
		}
		r.StartedAt = formatDBTime(startedAt)
		r.EndedAt = formatDBTime(endedAt)
		r.CreatedAt = formatDBTime(createdAt)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// analyticsSessionMemoKey names a session listing by predicate and
// arguments. Arguments are rendered in Go syntax so their boundaries and
// types survive: ["a b", "c"] and ["a", "b c"] are different keys.
func analyticsSessionMemoKey(where string, args []any) string {
	return fmt.Sprintf("%s|%#v", where, args)
}

func chBuildAnalyticsWhere(f db.AnalyticsFilter, dateCol, tablePrefix string, includeDate, includeTime bool) (string, []any) {
	var localDate string
	var localDateArgs []any
	if includeDate {
		localDate, localDateArgs = chAnalyticsLocalDateExpr(dateCol, f)
	}
	where, args := readbase.AnalyticsWhere(f, dateCol, tablePrefix, includeDate, chTimestampSQL, localDate, localDateArgs, db.ClickHouseQueryDialect())
	if includeTime && (f.DayOfWeek != nil || f.Hour != nil) {
		pred, pargs := chAnalyticsMessageTimeExists(f, tablePrefix+"id")
		where += " AND " + pred
		args = append(args, pargs...)
	}
	return where, args
}

func chAnalyticsCSVPredicate(col, raw string) (string, []any) {
	b := db.NewQueryBuilder(db.ClickHouseQueryDialect(), 0)
	pred := b.ValuesPredicate(col, db.CSVFilterValues(raw), true)
	return pred, b.Args()
}

func chAnalyticsLocalDateExpr(
	tsExpr string, f db.AnalyticsFilter,
) (string, []any) {
	if f.Timezone != "" {
		return "formatDateTime(" + tsExpr + ", '%Y-%m-%d', ?)", []any{f.Timezone}
	}
	return "formatDateTime(" + tsExpr + ", '%Y-%m-%d')", nil
}

// chAnalyticsDayOfWeekExpr is Monday=0..Sunday=6, matching DuckDB
// ((strftime('%w') + 6) % 7). ClickHouse toDayOfWeek mode 1 uses that mapping.
func chAnalyticsDayOfWeekExpr(tsExpr string, f db.AnalyticsFilter) (string, []any) {
	if f.Timezone != "" {
		return "toDayOfWeek(" + tsExpr + ", 1, ?)", []any{f.Timezone}
	}
	return "toDayOfWeek(" + tsExpr + ", 1)", nil
}

func chAnalyticsHourExpr(tsExpr string, f db.AnalyticsFilter) (string, []any) {
	if f.Timezone != "" {
		return "toHour(" + tsExpr + ", ?)", []any{f.Timezone}
	}
	return "toHour(" + tsExpr + ")", nil
}

func chAnalyticsMessageTimeExists(
	f db.AnalyticsFilter, sessionIDExpr string,
) (string, []any) {
	preds := []string{
		"m.timestamp IS NOT NULL",
	}
	var args []any
	if modelPred, modelArgs := chAnalyticsCSVPredicate("m.model", f.Model); modelPred != "" {
		preds = append(preds, modelPred)
		args = append(args, modelArgs...)
	}
	if f.DayOfWeek != nil {
		expr, exprArgs := chAnalyticsDayOfWeekExpr("m.timestamp", f)
		preds = append(preds, expr+" = ?")
		args = append(args, append(exprArgs, *f.DayOfWeek)...)
	}
	if f.Hour != nil {
		expr, exprArgs := chAnalyticsHourExpr("m.timestamp", f)
		preds = append(preds, expr+" = ?")
		args = append(args, append(exprArgs, *f.Hour)...)
	}
	return sessionIDExpr + " IN (SELECT m.session_id FROM messages m WHERE " +
		strings.Join(preds, " AND ") + ")", args
}

func (s analyticsSQL) VisitModels(ctx context.Context, sessionIDs []string, emit func(string)) error {
	return chQueryChunked(sessionIDs, func(chunk []string) error {
		query, args := readbase.AnalyticsModelsSQL(chunk)
		rows, err := s.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("querying clickhouse analytics models: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var model string
			if err := rows.Scan(&model); err != nil {
				return fmt.Errorf("scanning clickhouse analytics model: %w", err)
			}
			emit(model)
		}
		return rows.Err()
	})
}

func (s analyticsSQL) VisitModelTimes(ctx context.Context, sessionIDs []string, emit func(model, timestamp string)) error {
	return chQueryChunked(sessionIDs, func(chunk []string) error {
		query, args := readbase.AnalyticsModelTimesSQL(chunk)
		rows, err := s.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("querying clickhouse filtered analytics models: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var model string
			var ts any
			if err := rows.Scan(&model, &ts); err != nil {
				return fmt.Errorf("scanning clickhouse filtered analytics model: %w", err)
			}
			emit(model, formatDBTime(ts))
		}
		return rows.Err()
	})
}

func (s analyticsSQL) VisitToolCounts(ctx context.Context, sessionIDs []string, emit func(sessionID, model, timestamp string, count int)) error {
	return chQueryChunked(sessionIDs, func(chunk []string) error {
		ph, args := chInPlaceholders(chunk)
		rows, err := s.QueryContext(ctx, `
			SELECT tc.session_id, m.model, m.timestamp, toInt64(COUNT(*))
			FROM tool_calls tc
			JOIN messages m
				ON m.session_id = tc.session_id
				AND m.ordinal = tc.message_ordinal
			WHERE tc.session_id IN `+ph+`
			GROUP BY tc.session_id, m.model, m.timestamp`, args...)
		if err != nil {
			return fmt.Errorf(
				"querying clickhouse filtered analytics tool calls: %w",
				err,
			)
		}
		defer rows.Close()

		for rows.Next() {
			var sessionID, model string
			var ts any
			var count int
			if err := rows.Scan(&sessionID, &model, &ts, &count); err != nil {
				return fmt.Errorf(
					"scanning clickhouse filtered analytics tool calls: %w",
					err,
				)
			}
			emit(sessionID, model, formatDBTime(ts), count)
		}
		return rows.Err()
	})
}

func chAnalyticsBucketExpr(dateExpr, granularity string) string {
	switch granularity {
	case "week":
		// toStartOfWeek mode 1 starts the week on Monday, matching DuckDB
		// date_trunc('week').
		return "formatDateTime(toStartOfWeek(toDate(" + dateExpr + "), 1), '%Y-%m-%d')"
	case "month":
		return "formatDateTime(toStartOfMonth(toDate(" + dateExpr + ")), '%Y-%m-%d')"
	default:
		return dateExpr
	}
}

func chAnalyticsMessageFilterClause(col, raw string) string {
	pred, _ := chAnalyticsCSVPredicate(col, raw)
	if pred == "" {
		return ""
	}
	return "WHERE " + pred
}

func chAnalyticsAndClause(pred string) string {
	if pred == "" {
		return ""
	}
	return " AND " + pred
}

func chAnalyticsToolMessageJoin(
	toolAlias string, model string,
) string {
	if model == "" {
		return ""
	}
	return `
			JOIN messages m
				ON m.session_id = ` + toolAlias + `.session_id
				AND m.ordinal = ` + toolAlias + `.message_ordinal`
}

func (s analyticsSQL) Autonomy(ctx context.Context, sessionIDs []string, f db.AnalyticsFilter) (map[string]int, error) {
	sessions := chAnalyticsSessionSet(f)

	counts := map[string]int{}
	sessionIn, args := sessions.in("session_id")
	rows, err := s.QueryContext(ctx, `
		SELECT session_id,
			toInt64(countIf(role = 'user' AND is_system = false
				AND COALESCE(source_subtype, '') != 'tool_result')) AS user_count,
			toInt64(countIf(role = 'assistant' AND has_tool_use = true)) AS tool_count
		FROM messages
		WHERE `+sessionIn+`
		GROUP BY session_id`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse autonomy: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sessionID string
		var userCount, toolCount int
		if err := rows.Scan(&sessionID, &userCount, &toolCount); err != nil {
			return nil, fmt.Errorf("scanning clickhouse autonomy: %w", err)
		}
		if userCount > 0 {
			counts[db.AutonomyBucket(float64(toolCount)/float64(userCount))]++
		}
	}
	return counts, rows.Err()
}

// chMaxSQLVars bounds the IN-list size per query to stay well under
// driver bind-variable limits; larger ID sets are split into chunks.
const chMaxSQLVars = 900

func chInPlaceholders(ids []string) (string, []any) {
	return db.InPlaceholders(ids)
}

// chSessionSet is a relation of session IDs that a query embeds as
// `col IN (...)`. clickhouse-go inlines every bound argument into the
// statement text and the server rejects statements over max_query_size
// (256 KiB by default), so sets the database already knows are selected in
// SQL instead of being round-tripped through Go as placeholder lists.
type chSessionSet struct {
	body string
	args []any
}

// chSessionSetFromWhere selects session IDs with a WHERE clause written
// against the alias `s`.
func chSessionSetFromWhere(where string, args []any) chSessionSet {
	return chSessionSet{
		body: "SELECT s.id FROM sessions s WHERE " + where,
		args: args,
	}
}

// chSessionSetFromIDs embeds an explicit ID list. It exists for callers
// that pin exact sessions, such as contract tests; the list still grows the
// statement, so production paths derive the set in SQL instead.
func chSessionSetFromIDs(ids []string) chSessionSet {
	ph, args := chInPlaceholders(ids)
	return chSessionSet{body: "SELECT arrayJoin([" + strings.Trim(ph, "()") + "]) AS id", args: args}
}

// in returns `col IN (...)` with a fresh copy of the bound arguments so
// callers can embed the set more than once in one statement.
func (set chSessionSet) in(col string) (string, []any) {
	return col + " IN (" + set.body + ")", slices.Clone(set.args)
}

// chAnalyticsSessionSet is the SQL form of analyticsSessions for filters
// without a model, which is the only case analyticsSessions answers from
// chBuildAnalyticsWhere alone.
func chAnalyticsSessionSet(f db.AnalyticsFilter) chSessionSet {
	where, args := chBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.", true, true)
	return chSessionSetFromWhere(where, args)
}

func chQueryChunked(ids []string, fn func(chunk []string) error) error {
	for i := 0; i < len(ids); i += chMaxSQLVars {
		end := min(i+chMaxSQLVars, len(ids))
		if err := fn(ids[i:end]); err != nil {
			return err
		}
	}
	return nil
}

// chAnalyticsToolCallMessagesSQL selects the message columns tool call
// analytics join, limited to the selected sessions so the join hashes those
// sessions' messages rather than every message. The join keys on session
// id, so rows outside the selection could never match.
const chAnalyticsToolCallMessagesSQL = `SELECT session_id, ordinal, timestamp, model
					FROM messages WHERE session_id IN `

// analyticsSessionIDsContext attaches every selected session id as an
// external table, so a read over the selection is one statement and one
// scan of each table instead of one per chunk of placeholders. The returned
// subquery selects the ids.
func analyticsSessionIDsContext(ctx context.Context, ids []string) (context.Context, string, error) {
	table, err := ext.NewTable("analytics_session_ids", ext.Column("id", "String"))
	if err != nil {
		return nil, "", fmt.Errorf("creating analytics session table: %w", err)
	}
	for _, id := range ids {
		if err := table.Append(id); err != nil {
			return nil, "", fmt.Errorf("adding analytics session: %w", err)
		}
	}
	return chdriver.Context(ctx, chdriver.WithExternalTable(table)), "(SELECT id FROM analytics_session_ids)", nil
}

func (s analyticsSQL) VelocityMessages(ctx context.Context, sessionIDs []string, f db.AnalyticsFilter, loc *time.Location) (map[string][]db.TimingMessage, error) {
	sessions := chAnalyticsSessionSet(f)

	out := make(map[string][]db.TimingMessage)
	sessionIn, args := sessions.in("session_id")
	rows, err := s.QueryContext(ctx, `
		SELECT session_id, ordinal, role, timestamp, content_length
		FROM messages
		WHERE `+sessionIn+`
		ORDER BY session_id, ordinal`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse velocity messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sid, role string
		var ordinal int
		var ts any
		var contentLength int
		if err := rows.Scan(&sid, &ordinal, &role, &ts, &contentLength); err != nil {
			return nil, fmt.Errorf("scanning clickhouse velocity message: %w", err)
		}
		parsed, ok := readbase.AnalyticsLocalTime(formatDBTime(ts), loc)
		out[sid] = append(out[sid], db.TimingMessage{
			Role:          role,
			Time:          parsed,
			Valid:         ok,
			ContentLength: contentLength,
		})
	}
	return out, rows.Err()
}

func (s analyticsSQL) VelocityToolCounts(ctx context.Context, sessionIDs []string, f db.AnalyticsFilter) (map[string]int, error) {
	sessions := chAnalyticsSessionSet(f)

	out := make(map[string]int)
	sessionIn, args := sessions.in("session_id")
	rows, err := s.QueryContext(ctx, `
		SELECT session_id, toInt64(COUNT(*))
		FROM tool_calls
		WHERE `+sessionIn+`
		GROUP BY session_id`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse velocity tool calls: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sid string
		var count int
		if err := rows.Scan(&sid, &count); err != nil {
			return nil, fmt.Errorf("scanning clickhouse velocity tool call count: %w", err)
		}
		out[sid] = count
	}
	return out, rows.Err()
}

func chSessionPushVersions(sessions []readbase.AnalyticsSession) map[string]uint64 {
	versions := make(map[string]uint64, len(sessions))
	for _, session := range sessions {
		versions[session.ID] = session.PushVersion
	}
	return versions
}

// frustrationMarkerMemo remembers each session's marker count under the
// session push version it was read at. A push republishes every message of a
// changed session under a newer version, so a stale count is never reused,
// while unchanged sessions skip reading and scanning their prompts again.
type frustrationMarkerMemo struct {
	mu     sync.Mutex
	counts map[string]frustrationMarkerEntry
}

type frustrationMarkerEntry struct {
	pushVersion uint64
	count       int
}

func (m *frustrationMarkerMemo) lookup(id string, pushVersion uint64) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.counts[id]
	if !ok || entry.pushVersion != pushVersion {
		return 0, false
	}
	return entry.count, true
}

func (m *frustrationMarkerMemo) store(id string, pushVersion uint64, count int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.counts == nil {
		m.counts = make(map[string]frustrationMarkerEntry)
	}
	m.counts[id] = frustrationMarkerEntry{pushVersion: pushVersion, count: count}
}

// chPopulateFrustrationMarkers counts frustration markers for rows whose
// session version is not memoized. versions maps session IDs to the push
// version their row was read at; a session absent from it is always scanned.
func (s *Store) chPopulateFrustrationMarkers(
	ctx context.Context,
	rows []db.SignalRow,
	versions map[string]uint64,
) error {
	if len(rows) == 0 {
		return nil
	}
	idx := make(map[string]int, len(rows))
	ids := make([]string, 0, len(rows))
	for i := range rows {
		if version, ok := versions[rows[i].ID]; ok {
			if count, hit := s.frustrationMarkers.lookup(rows[i].ID, version); hit {
				rows[i].FrustrationMarkerCount = count
				continue
			}
		}
		idx[rows[i].ID] = i
		ids = append(ids, rows[i].ID)
	}
	if len(ids) == 0 {
		return nil
	}
	err := chQueryChunked(ids, func(chunk []string) error {
		ph, args := chInPlaceholders(chunk)
		q := `SELECT session_id, content, is_system
			FROM messages
			WHERE role = 'user' AND COALESCE(source_subtype, '') <> 'tool_result' AND session_id IN ` + ph
		msgRows, err := s.queryContext(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("querying clickhouse frustration markers: %w", err)
		}
		defer msgRows.Close()
		for msgRows.Next() {
			var sessionID, content string
			var isSystem bool
			if err := msgRows.Scan(
				&sessionID, &content, &isSystem,
			); err != nil {
				return fmt.Errorf("scanning clickhouse frustration marker: %w", err)
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
			return fmt.Errorf("iterating clickhouse frustration markers: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Only a complete scan is memoized; a failed or canceled read leaves
	// every session in the batch to be scanned again next time.
	for id, i := range idx {
		if version, ok := versions[id]; ok {
			s.frustrationMarkers.store(id, version, rows[i].FrustrationMarkerCount)
		}
	}
	return nil
}

func (s analyticsSQL) VisitSignalMessages(ctx context.Context, ids []string, emit func(db.SignalMessage)) error {
	query, args := readbase.AnalyticsSignalMessagesSQL(ids)
	msgRows, err := s.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("querying clickhouse signal messages: %w", err)
	}
	defer msgRows.Close()
	for msgRows.Next() {
		var m db.SignalMessage
		var ts any
		if err := msgRows.Scan(
			&m.SessionID, &m.Ordinal, &m.Role,
			&m.Content, &ts,
			&m.IsSystem, &m.HasToolUse, &m.SourceSubtype,
		); err != nil {
			return fmt.Errorf("scanning clickhouse signal message: %w", err)
		}
		m.Timestamp = formatDBTime(ts)
		emit(m)
	}
	if err := msgRows.Err(); err != nil {
		return fmt.Errorf("iterating clickhouse signal messages: %w", err)
	}
	return nil
}

func chAnalyticsMessageWindowPred(col, from, to string) (string, []any) {
	return readbase.AnalyticsMessageWindowPred(col, from, to, chTimestampSQL)
}

func chAnalyticsToolSessionWindow(f db.AnalyticsFilter) (string, []any) {
	from, to := readbase.AnalyticsWindowBounds(f)
	sessionPred, args := chAnalyticsMessageWindowPred("COALESCE(s.started_at, s.created_at)", from, to)
	if sessionPred == "" {
		return "", nil
	}
	messagePred, messageArgs := chAnalyticsMessageWindowPred("wm.timestamp", from, to)
	return "(" + sessionPred + " OR s.id IN (SELECT wm.session_id FROM messages wm WHERE " + messagePred + "))", append(args, messageArgs...)
}
