package clickhouse

import (
	"context"
	"fmt"
	"maps"
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
	if cached, ok := s.analyticsSessionRows.Get(memoKey, fingerprint); ok {
		return slices.Clone(cached), nil
	}
	// The panels of one page ask at once; one read answers them all. The
	// read outlives a caller that gives up, so the others still get it.
	shared := s.analyticsListings.DoChan(memoKey+"\x00"+fingerprint, func() (any, error) {
		out, err := s.readAnalyticsSessions(context.WithoutCancel(ctx), where, args)
		if err != nil {
			return nil, err
		}
		s.analyticsSessionRows.Put(memoKey, fingerprint, out)
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
		SELECT `+readbase.AnalyticsSessionColumns+`, push_version,
			if(`+chFactsKnown+`, fact_frustration_markers, NULL)
		FROM sessions s
		WHERE `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse analytics sessions: %w", err)
	}
	defer rows.Close()

	return readbase.ScanAnalyticsSessions(rows, "clickhouse", formatDBTime, readbase.AnalyticsSessionExtras{
		PushVersion: true, FrustrationMarkers: true,
	})
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
	if modelPred, modelArgs := readbase.AnalyticsCSVPredicate("m.model", f.Model, db.ClickHouseQueryDialect()); modelPred != "" {
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

// VisitSessionIDSets sends every id as one external table, so each read
// over the selection is one statement whatever the selection's size.
func (s analyticsSQL) VisitSessionIDSets(ctx context.Context, ids []string, visit func(ctx context.Context, idSet string, args []any) error) error {
	ctx, idSet, err := analyticsSessionIDsContext(ctx, ids)
	if err != nil {
		return err
	}
	return visit(ctx, idSet, nil)
}

func (s analyticsSQL) ToolCountsSQL(idSet string, args []any) (string, []any) {
	return `
			SELECT tc.session_id, m.model, m.timestamp, toInt64(COUNT(*))
			FROM tool_calls tc
			JOIN (` + chAnalyticsToolCallMessagesSQL + idSet + `) m
				ON m.session_id = tc.session_id
				AND m.ordinal = tc.message_ordinal
			WHERE tc.session_id IN ` + idSet + `
			GROUP BY tc.session_id, m.model, m.timestamp`, slices.Concat(args, args)
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

func (s analyticsSQL) Autonomy(ctx context.Context, _ []string, f db.AnalyticsFilter) (map[string]int, error) {
	known, knownArgs, unknown := chAnalyticsFactSets(f)

	sessionIn, args := unknown.in("session_id")
	rows, err := s.QueryContext(ctx, `
		SELECT s.id, s.fact_user_messages, s.fact_tool_use_messages
		FROM sessions s
		WHERE `+known+`
		UNION ALL
		SELECT session_id,
			toInt64(countIf(role = 'user' AND is_system = false
				AND source_subtype != 'tool_result')) AS user_count,
			toInt64(countIf(role = 'assistant' AND has_tool_use = true)) AS tool_count
		FROM messages
		WHERE `+sessionIn+`
		GROUP BY session_id`,
		slices.Concat(knownArgs, args)...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse autonomy: %w", err)
	}
	defer rows.Close()
	return readbase.ScanAnalyticsAutonomy(rows, "clickhouse")
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

// chAnalyticsFactSets splits the sessions chAnalyticsSessionSet selects by
// whether their dashboard facts are current. known is a WHERE clause over
// the alias `s` for the sessions whose facts a read can use; unknown selects
// the rest, whose messages the read still scans.
func chAnalyticsFactSets(f db.AnalyticsFilter) (known string, knownArgs []any, unknown chSessionSet) {
	where, args := chBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.", true, true)
	return where + " AND s." + chFactsKnown, args,
		chSessionSetFromWhere(where+" AND NOT s."+chFactsKnown, slices.Clone(args))
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

func (s analyticsSQL) VelocitySessions(ctx context.Context, ids []string, f db.AnalyticsFilter, loc *time.Location) (map[string]db.SessionVelocity, error) {
	known, knownArgs, _ := chAnalyticsFactSets(f)

	out, err := s.knownVelocities(ctx, known, knownArgs)
	if err != nil {
		return nil, err
	}
	if err := s.scanMissingVelocities(ctx, ids, out, loc); err != nil {
		return nil, err
	}
	return out, nil
}

// knownVelocities reads the stored velocity facts of the sessions known
// selects.
func (s analyticsSQL) knownVelocities(ctx context.Context, known string, args []any) (map[string]db.SessionVelocity, error) {
	rows, err := s.QueryContext(ctx, `
		SELECT s.id, s.fact_velocity_messages, s.fact_velocity_turn_cycles,
			s.fact_velocity_first_response, s.fact_velocity_active_seconds,
			s.fact_velocity_assistant_chars
		FROM sessions s
		WHERE `+known, args...)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse velocity facts: %w", err)
	}
	defer rows.Close()
	out := make(map[string]db.SessionVelocity)
	for rows.Next() {
		var id string
		var v db.SessionVelocity
		if err := rows.Scan(&id, &v.Messages, &v.TurnCycles, &v.FirstResponse, &v.ActiveSeconds, &v.AssistantChars); err != nil {
			return nil, fmt.Errorf("scanning clickhouse velocity facts: %w", err)
		}
		out[id] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating clickhouse velocity facts: %w", err)
	}
	return out, nil
}

// scanMissingVelocities adds to out the velocity of each selected session
// the facts read did not return, computed from its messages. It works from
// what that read returned rather than asking again which facts are current,
// so a push that publishes a session's facts between the two reads cannot
// leave the session out of both.
func (s analyticsSQL) scanMissingVelocities(ctx context.Context, ids []string, out map[string]db.SessionVelocity, loc *time.Location) error {
	missing := slices.DeleteFunc(slices.Clone(ids), func(id string) bool {
		_, ok := out[id]
		return ok
	})
	if len(missing) == 0 {
		return nil
	}
	ctx, ph, err := analyticsSessionIDsContext(ctx, missing)
	if err != nil {
		return err
	}
	rows, err := s.QueryContext(ctx, `
		SELECT session_id, ordinal, role, timestamp, content_length
		FROM messages
		WHERE `+db.ExcludeClaudeHooksSQL("source_subtype")+` AND session_id IN `+ph+`
		ORDER BY session_id, ordinal`)
	if err != nil {
		return fmt.Errorf("querying clickhouse velocity messages: %w", err)
	}
	defer rows.Close()
	scanned, err := readbase.ScanAnalyticsVelocityMessages(
		rows, "clickhouse", formatDBTime, loc, make(map[string][]db.TimingMessage))
	if err != nil {
		return err
	}
	maps.Copy(out, db.SessionVelocities(scanned))
	return nil
}

func (s analyticsSQL) VelocityToolCounts(ctx context.Context, _ []string, f db.AnalyticsFilter) (map[string]int, error) {
	known, knownArgs, unknown := chAnalyticsFactSets(f)

	out := make(map[string]int)
	sessionIn, args := unknown.in("session_id")
	rows, err := s.QueryContext(ctx, `
		SELECT s.id, s.fact_tool_calls
		FROM sessions s
		WHERE `+known+` AND s.fact_tool_calls > 0
		UNION ALL
		SELECT session_id, toInt64(COUNT(*))
		FROM tool_calls
		WHERE `+sessionIn+`
		GROUP BY session_id`,
		slices.Concat(knownArgs, args)...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse velocity tool calls: %w", err)
	}
	defer rows.Close()
	return readbase.ScanAnalyticsVelocityToolCounts(rows, "clickhouse", out)
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

// chPopulateFrustrationMarkers fills each row's frustration marker count
// from the count push stored on its session, else from the memo, else by
// scanning the session's prompts with chFrustrationPromptsSQL. sessions
// are the listing rows were built from; a session absent from it is always
// scanned.
func (s *Store) chPopulateFrustrationMarkers(
	ctx context.Context,
	rows []db.SignalRow,
	sessions []readbase.AnalyticsSession,
) error {
	if len(rows) == 0 {
		return nil
	}
	versions := make(map[string]uint64, len(sessions))
	stored := make(map[string]int)
	for _, session := range sessions {
		versions[session.ID] = session.PushVersion
		if session.FrustrationMarkers != nil {
			stored[session.ID] = *session.FrustrationMarkers
		}
	}
	idx := make(map[string]int, len(rows))
	ids := make([]string, 0, len(rows))
	for i := range rows {
		if count, ok := stored[rows[i].ID]; ok {
			rows[i].FrustrationMarkerCount = count
			continue
		}
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
	ctx, ph, err := analyticsSessionIDsContext(ctx, ids)
	if err != nil {
		return err
	}
	msgRows, err := s.queryContext(ctx, fmt.Sprintf(chFrustrationPromptsSQL, ph), chFrustrationPromptsArgs...)
	if err != nil {
		return fmt.Errorf("querying clickhouse frustration markers: %w", err)
	}
	defer msgRows.Close()
	for msgRows.Next() {
		var sessionID, content string
		var decided bool
		if err := msgRows.Scan(&sessionID, &decided, &content); err != nil {
			return fmt.Errorf("scanning clickhouse frustration marker: %w", err)
		}
		i, ok := idx[sessionID]
		if ok && (decided || signals.IsFrustrationMarker(content)) {
			rows[i].FrustrationMarkerCount++
		}
	}
	if err := msgRows.Err(); err != nil {
		return fmt.Errorf("iterating clickhouse frustration markers: %w", err)
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
