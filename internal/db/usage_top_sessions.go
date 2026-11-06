package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"strings"
)

// readUsageTopSessionOwners loads only the daily contributions that need the
// shared exception fold before they can enter SQL ranking.
func readUsageTopSessionOwners(ctx context.Context, conn *sql.Conn, timezoneID int64, snapshot *usageQuerySnapshot, filter UsageFilter) (usageFactsResult, error) {
	if err := readUsageOwners(ctx, conn, timezoneID, snapshot, filter); err != nil {
		return usageFactsResult{}, err
	}
	ids := make([]string, 0, len(snapshot.Sessions))
	for _, owner := range snapshot.Sessions {
		ids = append(ids, owner.ID)
	}
	if len(ids) == 0 {
		return usageFactsResult{MatchingSessions: make(map[string]UsageSessionInfo)}, nil
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		return usageFactsResult{}, err
	}
	return readUsageDailyRollups(ctx, conn, timezoneID, *snapshot, filter, " AND i.session_id IN (SELECT value FROM json_each(?))", string(raw))
}

// readUsageTopSessions ranks ordinary and resolved owner contributions together.
// Only the returned units load their cached representatives and archive names.
func readUsageTopSessions(ctx context.Context, conn *sql.Conn, timezoneID int64, snapshot usageQuerySnapshot, facts usageFactsResult, filter UsageFilter, limit int) ([]TopSessionEntry, error) {
	owners, err := foldUsageTopSessions(snapshot.Sessions, facts.Groups, filter)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(owners)
	if err != nil {
		return nil, err
	}
	b := NewQueryBuilder(SQLiteQueryDialect(), 0)
	where, owner := usageOrdinaryWindowWhere(b, timezoneID, snapshot, filter)
	where = `NOT (` + owner + `) AND ` + where
	contributions := `WITH sessions AS MATERIALIZED (
        SELECT s.session_id, s.project, s.machine, s.group_key, s.group_label, s.baked_agent AS agent, s.baked_started_at AS started_at,
        r.input_tokens AS input, r.output_tokens AS output, r.cache_creation_tokens AS cache_write, r.cache_read_tokens AS cache_read, r.estimated_cost_microdollars AS cost
        FROM usage_daily_rollups r JOIN (SELECT *, baked_agent AS agent FROM usage_rollup_installs) s ON s.id=r.rollup_install_id
        WHERE ` + where + `
        UNION ALL SELECT
        value->>'$.sessionId', value->>'$.project', COALESCE(value->>'$.machine',''), COALESCE(value->>'$.groupKey',''), COALESCE(value->>'$.groupLabel',''), value->>'$.agent', value->>'$.startedAt',
        value->>'$.inputTokens', value->>'$.outputTokens', value->>'$.cacheCreationTokens', value->>'$.cacheReadTokens', value->>'$.cost.microdollars'
        FROM json_each(` + b.Add(string(raw)) + `)
    )`
	unit := `json_array(session_id)`
	sortID := `session_id`
	representatives := `MIN(session_id) AS agent_id, MIN(session_id) AS started_id, MIN(session_id) AS label_id`
	if filter.TopSessionsByGroup {
		unit = `CASE WHEN group_key='' THEN json_array(session_id) ELSE json_array(project,machine,group_key) END`
		sortID = `CASE WHEN group_key='' THEN session_id ELSE '' END`
		representatives = `MIN(session_id) AS agent_id,
            substr(MAX(agentsview_top_session_started_order(started_at) || session_id),25) AS started_id,
            substr(MAX(CASE WHEN group_label!='' THEN agentsview_top_session_started_order(started_at) || session_id END),25) AS label_id`
	}
	order := "cost DESC"
	if strings.EqualFold(filter.TopSessionsSort, TopSessionsSortTokens) {
		order = filter.TopSessionsTokenTypes.SQLTotal("input", "output", "cache_write", "cache_read") + " DESC"
	}
	query := contributions + `, units AS MATERIALIZED (
        SELECT ` + sortID + ` AS sort_id, project, machine, group_key, ` + representatives + `,
        SUM(input) AS input, SUM(output) AS output, SUM(cache_write) AS cache_write, SUM(cache_read) AS cache_read, SUM(cost) AS cost
        FROM sessions GROUP BY ` + unit + `
    ), selected AS MATERIALIZED (
        SELECT * FROM units ORDER BY ` + order + `, sort_id, project, group_key, machine LIMIT ` + b.Add(normalizeTopSessionsLimit(limit)) + `
    ), returned AS (
        SELECT *, 0 AS remainder FROM selected`
	if filter.TopSessionsByGroup {
		query += ` UNION ALL SELECT '', '', '', '', NULL, NULL, NULL,
            SUM(input)-(SELECT COALESCE(SUM(input),0) FROM selected),
            SUM(output)-(SELECT COALESCE(SUM(output),0) FROM selected),
            SUM(cache_write)-(SELECT COALESCE(SUM(cache_write),0) FROM selected),
            SUM(cache_read)-(SELECT COALESCE(SUM(cache_read),0) FROM selected),
            SUM(cost)-(SELECT COALESCE(SUM(cost),0) FROM selected), 1
            FROM units HAVING COUNT(*)>(SELECT COUNT(*) FROM selected)`
	}
	query += `) SELECT u.sort_id, u.project, u.machine, u.group_key,
        COALESCE(a.baked_agent,''), COALESCE(s.baked_started_at,''), COALESCE(l.group_label,''),
        u.input, u.output, u.cache_write, u.cache_read, u.cost
        FROM returned u
        LEFT JOIN usage_rollup_installs a ON a.timezone_id=` + b.Add(timezoneID) + ` AND a.session_id=u.agent_id
        LEFT JOIN usage_rollup_installs s ON s.timezone_id=` + b.Add(timezoneID) + ` AND s.session_id=u.started_id
        LEFT JOIN usage_rollup_installs l ON l.timezone_id=` + b.Add(timezoneID) + ` AND l.session_id=u.label_id
        ORDER BY remainder, ` + order + `, sort_id, u.project, u.group_key, u.machine`
	rows, err := conn.QueryContext(ctx, query, b.Args()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]TopSessionEntry, 0)
	for rows.Next() {
		var entry TopSessionEntry
		if err := rows.Scan(&entry.SessionID, &entry.Project, &entry.Machine, &entry.GroupKey, &entry.Agent, &entry.StartedAt, &entry.GroupLabel,
			&entry.InputTokens, &entry.OutputTokens, &entry.CacheCreationTokens, &entry.CacheReadTokens, &entry.Cost.Microdollars); err != nil {
			return nil, err
		}
		entry.TotalTokens = entry.InputTokens + entry.OutputTokens + entry.CacheCreationTokens + entry.CacheReadTokens
		if entry.SessionID != "" {
			entry.GroupLabel = ""
		} else if entry.GroupKey != "" {
			entry.DisplayName = entry.GroupLabel
			if entry.DisplayName == "" {
				entry.DisplayName, _, _ = strings.Cut(entry.GroupKey, ":")
			}
		}
		result = append(result, entry)
	}
	return result, rows.Err()
}

func (db *DB) finishUsageTopSessions(ctx context.Context, facts usageFactsResult) ([]TopSessionEntry, error) {
	entries := facts.TopSessions
	ids := make([]string, 0, len(entries))
	for i := range entries {
		if entries[i].SessionID != "" {
			entries[i].DisplayName = entries[i].SessionID
			ids = append(ids, entries[i].SessionID)
		}
	}
	metadata, err := db.loadTopSessionMetadata(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		if meta, ok := metadata[entries[i].SessionID]; ok {
			entries[i].DisplayName = meta.displayName
			entries[i].SessionName = meta.sessionName
		}
	}
	return entries, nil
}
