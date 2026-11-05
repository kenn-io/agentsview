package db

import (
	"context"
	"database/sql"
	"strings"

	"go.kenn.io/agentsview/internal/usagefacts"
)

// readUsageDayTotals reads ordinary totals and prepares the exceptional session
// metadata for the shared rollup query's exception fold.
func readUsageDayTotals(ctx context.Context, conn *sql.Conn, timezoneID int64, snapshot *usageQuerySnapshot, filter UsageFilter) (usageFactsResult, error) {
	from, to := usageRollupDateBounds(*snapshot)
	b := NewQueryBuilder(SQLiteQueryDialect(), 0)
	where := `s.timezone_id=` + b.Add(timezoneID) + ` AND (` + b.Add(from) + `='' OR s.local_date>=` + b.Add(from) + `) AND (` + b.Add(to) + `='' OR s.local_date<=` + b.Add(to) + `)`
	where = AppendUsagePredicates(where, BuildUsageSessionFilter(filter, b, ""), "")
	where = AppendUsagePredicates(where, BuildUsageSourceFilter(filter, b, "s.reported_model"), "")
	rows, err := conn.QueryContext(ctx, `SELECT s.local_date, s.project, s.agent, s.machine,
        s.reported_model, s.provider_id, s.priced_model, s.pricing_timestamp,
        SUM(s.input_tokens), SUM(s.output_tokens),
        SUM(s.cache_creation_tokens), SUM(s.cache_read_tokens),
        SUM(s.estimated_cost_microdollars), SUM(s.savings_microdollars),
        SUM(s.computed_request_count), SUM(s.computed_aggregate_count), SUM(s.reported_count), SUM(s.base_request_count), s.band_threshold
        FROM usage_day_totals s WHERE `+where+`
        GROUP BY s.local_date, s.project, s.agent, s.machine, s.reported_model, s.provider_id, s.rate_hash, s.band_threshold`, b.Args()...)
	if err != nil {
		return usageFactsResult{}, err
	}
	result := usageFactsResult{MatchingSessions: make(map[string]UsageSessionInfo)}
	for rows.Next() {
		var group usageFactsGroup
		var band int
		group.SessionID = "\x00usage-day-totals"
		if err := rows.Scan(&group.Date, &group.Project, &group.Agent, &group.Machine,
			&group.Model, &group.ProviderID, &group.PricedModel, &group.PricingTimestamp,
			&group.InputTokens, &group.OutputTokens, &group.CacheCreationTokens, &group.CacheReadTokens,
			&group.CostMicrodollars, &group.SavingsMicrodollars, &group.ComputedRequestCount, &group.ComputedAggregateCount,
			&group.ReportedCount, &group.BaseRequestCount, &band); err != nil {
			_ = rows.Close()
			return usageFactsResult{}, err
		}
		if band >= 0 {
			group.BandThreshold = new(band)
		}
		result.Groups = append(result.Groups, group)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return usageFactsResult{}, err
	}
	if err := rows.Close(); err != nil {
		return usageFactsResult{}, err
	}

	// Include every exception owner before ranking; filtering earlier can change
	// snapshot attribution. Authoritative sessions keep per-session allocation.
	b = NewQueryBuilder(SQLiteQueryDialect(), 0)
	passing := strings.Join(BuildUsageSessionFilter(filter, b, ""), " AND ")
	if passing == "" {
		passing = "1=1"
	}
	args := append(b.Args(), timezoneID)
	args = append(args, b.Args()...)
	args = append(args, from, from, to, to, from, from, to, to)
	rows, err = conn.QueryContext(ctx, `SELECT s.session_id, s.project, s.baked_agent, s.machine, s.baked_started_at, (`+passing+`)
        FROM (SELECT *, baked_agent AS agent FROM usage_rollup_installs) s WHERE s.timezone_id=? AND ((s.has_authoritative=1 AND (`+passing+`) AND s.id IN (
        SELECT r.rollup_install_id FROM usage_daily_rollups r WHERE (?='' OR r.local_date>=?) AND (?='' OR r.local_date<=?))) OR s.id IN (
        SELECT e.rollup_install_id FROM usage_rollup_exceptions e WHERE (?='' OR e.local_date>=?) AND (?='' OR e.local_date<=?)))`, args...)
	if err != nil {
		return usageFactsResult{}, err
	}
	snapshot.Sessions = nil
	for rows.Next() {
		var session usageQuerySession
		if err := rows.Scan(&session.ID, &session.Project, &session.Agent, &session.Machine, &session.StartedAt, &session.PassesFilter); err != nil {
			_ = rows.Close()
			return usageFactsResult{}, err
		}
		if millis, _, _ := usagefacts.ParseTimestamp(session.StartedAt); millis != nil {
			session.StartedAtMillis = millis
			session.StartedAtNanos = usagefacts.ParseTimestampNanos(session.StartedAt)
		}
		snapshot.Sessions = append(snapshot.Sessions, session)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return usageFactsResult{}, err
	}
	if err := rows.Close(); err != nil {
		return usageFactsResult{}, err
	}
	authoritative, err := readUsageDailyRollups(ctx, conn, timezoneID, *snapshot, filter, true)
	if err != nil {
		return usageFactsResult{}, err
	}
	result.Groups = append(result.Groups, authoritative.Groups...)
	if !filter.SkipSessionCounts {
		b = NewQueryBuilder(SQLiteQueryDialect(), 0)
		where = `s.timezone_id=` + b.Add(timezoneID) + ` AND (` + b.Add(from) + `='' OR r.local_date>=` + b.Add(from) + `) AND (` + b.Add(to) + `='' OR r.local_date<=` + b.Add(to) + `)`
		where = AppendUsagePredicates(where, BuildUsageSessionFilter(filter, b, ""), "")
		where = AppendUsagePredicates(where, BuildUsageSourceFilter(filter, b, "r.reported_model"), "")
		rows, err = conn.QueryContext(ctx, `SELECT DISTINCT s.session_id, s.project, s.baked_agent FROM usage_daily_rollups r JOIN (SELECT *, baked_agent AS agent FROM usage_rollup_installs) s ON s.id=r.rollup_install_id WHERE `+where, b.Args()...)
		if err != nil {
			return usageFactsResult{}, err
		}
		for rows.Next() {
			var id string
			var info UsageSessionInfo
			if err := rows.Scan(&id, &info.Project, &info.Agent); err != nil {
				_ = rows.Close()
				return usageFactsResult{}, err
			}
			result.MatchingSessions[id] = info
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return usageFactsResult{}, err
		}
		if err := rows.Close(); err != nil {
			return usageFactsResult{}, err
		}
	}
	return result, nil
}
