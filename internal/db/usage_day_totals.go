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
	where := `s.timezone_id=` + b.Add(timezoneID)
	if from != "" {
		where += ` AND s.local_date>=` + b.Add(from)
	}
	if to != "" {
		where += ` AND s.local_date<=` + b.Add(to)
	}
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
	defer rows.Close()
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
			return usageFactsResult{}, err
		}
		if band >= 0 {
			group.BandThreshold = new(band)
		}
		result.Groups = append(result.Groups, group)
	}
	if err := rows.Err(); err != nil {
		return usageFactsResult{}, err
	}

	if err := readUsageOwners(ctx, conn, timezoneID, snapshot, filter); err != nil {
		return usageFactsResult{}, err
	}
	authoritative, err := readUsageDailyRollups(ctx, conn, timezoneID, *snapshot, filter, " AND i.has_authoritative=1")
	if err != nil {
		return usageFactsResult{}, err
	}
	result.Groups = append(result.Groups, authoritative.Groups...)
	result.MatchingSessions = authoritative.MatchingSessions
	if !filter.SkipSessionCounts {
		b = NewQueryBuilder(SQLiteQueryDialect(), 0)
		where = `s.timezone_id=` + b.Add(timezoneID)
		if from != "" {
			where += ` AND r.local_date>=` + b.Add(from)
		}
		if to != "" {
			where += ` AND r.local_date<=` + b.Add(to)
		}
		where = AppendUsagePredicates(where, BuildUsageSessionFilter(filter, b, ""), "")
		where = AppendUsagePredicates(where, BuildUsageSourceFilter(filter, b, "r.reported_model"), "")
		owner := usageOwnerPredicate("s", b, from, to)
		rows, err = conn.QueryContext(ctx, `WITH eligible AS (
            SELECT s.session_id, s.project, s.baked_agent, s.id, s.has_authoritative FROM usage_daily_rollups r
            JOIN (SELECT *, baked_agent AS agent FROM usage_rollup_installs) s ON s.id=r.rollup_install_id WHERE `+where+`
        ) SELECT CASE WHEN (`+owner+`) THEN s.session_id ELSE '' END AS owner_id,
            s.project, s.baked_agent, COUNT(DISTINCT s.session_id) FROM eligible s
            GROUP BY owner_id, s.project, s.baked_agent`, b.Args()...)
		if err != nil {
			return usageFactsResult{}, err
		}
		result.SessionCounts = NewUsageSessionCounts(nil)
		for rows.Next() {
			var id, project, agent string
			var count int
			if err := rows.Scan(&id, &project, &agent, &count); err != nil {
				_ = rows.Close()
				return usageFactsResult{}, err
			}
			if id != "" {
				result.MatchingSessions[id] = UsageSessionInfo{Project: project, Agent: agent}
				continue
			}
			result.SessionCounts.Total += count
			result.SessionCounts.ByProject[project] += count
			result.SessionCounts.ByAgent[agent] += count
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

func readUsageOwners(ctx context.Context, conn *sql.Conn, timezoneID int64, snapshot *usageQuerySnapshot, filter UsageFilter) error {
	from, to := usageRollupDateBounds(*snapshot)
	// Include every exception owner before ranking; filtering earlier can change
	// snapshot attribution. Authoritative sessions keep per-session allocation.
	b := NewQueryBuilder(SQLiteQueryDialect(), 0)
	passing := strings.Join(BuildUsageSessionFilter(filter, b, ""), " AND ")
	if passing == "" {
		passing = "1=1"
	}
	args := append(b.Args(), timezoneID)
	args = append(args, b.Args()...)
	b = NewQueryBuilder(SQLiteQueryDialect(), 0)
	dates := "1=1"
	if from != "" {
		dates += ` AND local_date>=` + b.Add(from)
	}
	if to != "" {
		dates += ` AND local_date<=` + b.Add(to)
	}
	args = append(args, b.Args()...)
	args = append(args, b.Args()...)
	rows, err := conn.QueryContext(ctx, `SELECT s.session_id, s.project, s.baked_agent, s.machine, s.baked_started_at, s.group_key, (`+passing+`)
        FROM (SELECT *, baked_agent AS agent FROM usage_rollup_installs) s WHERE s.timezone_id=? AND ((s.has_authoritative=1 AND (`+passing+`) AND s.id IN (
        SELECT r.rollup_install_id FROM usage_daily_rollups r WHERE `+dates+`)) OR s.id IN (
        SELECT e.rollup_install_id FROM usage_rollup_exceptions e WHERE `+dates+`))`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	snapshot.Sessions = nil
	for rows.Next() {
		var session usageQuerySession
		if err := rows.Scan(&session.ID, &session.Project, &session.Agent, &session.Machine, &session.StartedAt, &session.GroupKey, &session.PassesFilter); err != nil {
			return err
		}
		if millis, _, _ := usagefacts.ParseTimestamp(session.StartedAt); millis != nil {
			session.StartedAtMillis = millis
			session.StartedAtNanos = usagefacts.ParseTimestampNanos(session.StartedAt)
		}
		snapshot.Sessions = append(snapshot.Sessions, session)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return nil
}

func usageOwnerPredicate(alias string, b *QueryBuilder, from, to string) string {
	dates := "1=1"
	if from != "" {
		dates += ` AND local_date>=` + b.Add(from)
	}
	if to != "" {
		dates += ` AND local_date<=` + b.Add(to)
	}
	return alias + `.has_authoritative=1 OR ` + alias + `.id IN (SELECT rollup_install_id FROM usage_rollup_exceptions WHERE ` + dates + `)`
}
