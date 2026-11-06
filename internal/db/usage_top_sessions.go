package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
)

type usageTopSessionCandidates struct {
	Entries       []TopSessionEntry
	Owners        []usageQuerySession
	Total         TopSessionEntry
	Units         int
	SelectedUnits int
}

// readUsageTopSessions ranks ordinary units in SQL and leaves exceptional owners
// to the shared survivor fold. Owner-sharing units must survive the SQL limit.
func readUsageTopSessions(ctx context.Context, conn *sql.Conn, timezoneID int64, snapshot *usageQuerySnapshot, filter UsageFilter, limit int) (usageFactsResult, error) {
	if err := readUsageOwners(ctx, conn, timezoneID, snapshot, filter); err != nil {
		return usageFactsResult{}, err
	}
	from, to := usageRollupDateBounds(*snapshot)
	owners := NewQueryBuilder(SQLiteQueryDialect(), 0)
	restriction := " AND (" + usageOwnerPredicate("i", owners, from, to) + ")"
	result, err := readUsageDailyRollups(ctx, conn, timezoneID, *snapshot, filter, restriction, owners.Args()...)
	if err != nil {
		return usageFactsResult{}, err
	}
	candidates := &usageTopSessionCandidates{Owners: snapshot.Sessions, Entries: make([]TopSessionEntry, 0)}
	result.TopSessions = candidates
	b := NewQueryBuilder(SQLiteQueryDialect(), 0)
	where, owner := usageOrdinaryWindowWhere(b, timezoneID, *snapshot, filter)
	where = `NOT (` + owner + `) AND ` + where
	unit := `json_array(s.session_id)`
	sessionID := `s.session_id`
	if filter.TopSessionsByGroup {
		unit = `CASE WHEN s.group_key='' THEN json_array(s.session_id) ELSE json_array(s.project,s.machine,s.group_key) END`
		sessionID = `CASE WHEN s.group_key='' THEN s.session_id ELSE '' END`
	}
	var ownerKeys [][]string
	for _, owner := range snapshot.Sessions {
		if filter.TopSessionsByGroup && owner.GroupKey != "" {
			ownerKeys = append(ownerKeys, []string{owner.Project, owner.Machine, owner.GroupKey})
		}
	}
	keys, err := json.Marshal(ownerKeys)
	if err != nil {
		return usageFactsResult{}, err
	}
	order := "cost DESC"
	if strings.EqualFold(filter.TopSessionsSort, TopSessionsSortTokens) {
		columns := []string{"0"}
		for _, col := range []struct {
			bit  UsageTokenTypes
			name string
		}{{UsageTokenTypeInput, "input"}, {UsageTokenTypeOutput, "output"}, {UsageTokenTypeCacheWrite, "cache_write"}, {UsageTokenTypeCacheRead, "cache_read"}} {
			if filter.TopSessionsTokenTypes.normalized()&col.bit != 0 {
				columns = append(columns, col.name)
			}
		}
		order = strings.Join(columns, "+") + " DESC"
	}
	// A group needs at most three runs: the stable agent, the latest start,
	// and the latest usable label. SQL keeps these IDs bounded even for a
	// group containing every session in the window.
	representatives := `json_array(MIN(session_id))`
	windowTotals := `0 AS units, 0 AS total_input, 0 AS total_output, 0 AS total_write, 0 AS total_read, 0 AS total_cost`
	if filter.TopSessionsByGroup {
		representatives = `json_array(MIN(session_id), substr(MAX(started_order || session_id),25), substr(MAX(CASE WHEN group_label!='' THEN started_order || session_id END),25))`
		windowTotals = `COUNT(*) OVER () AS units, SUM(input) OVER () AS total_input, SUM(output) OVER () AS total_output, SUM(cache_write) OVER () AS total_write, SUM(cache_read) OVER () AS total_read, SUM(cost) OVER () AS total_cost`
	}
	rows, err := conn.QueryContext(ctx, `WITH sessions AS (
        SELECT `+unit+` AS unit, `+sessionID+` AS sort_id, s.session_id, s.project, s.machine, s.group_key, s.group_label,
        agentsview_top_session_started_order(s.baked_started_at) AS started_order,
        SUM(r.input_tokens) AS input, SUM(r.output_tokens) AS output, SUM(r.cache_creation_tokens) AS cache_write, SUM(r.cache_read_tokens) AS cache_read, SUM(r.estimated_cost_microdollars) AS cost
        FROM usage_daily_rollups r JOIN (SELECT *, baked_agent AS agent FROM usage_rollup_installs) s ON s.id=r.rollup_install_id
        WHERE `+where+` GROUP BY s.session_id
    ), units AS (
        SELECT unit, sort_id, project, machine, group_key, `+representatives+` AS members,
        SUM(input) AS input, SUM(output) AS output, SUM(cache_write) AS cache_write, SUM(cache_read) AS cache_read, SUM(cost) AS cost FROM sessions GROUP BY unit
    ), ranked AS (
        SELECT *, ROW_NUMBER() OVER (ORDER BY `+order+`, sort_id, project, group_key, machine) AS rank,
        `+windowTotals+` FROM units
    ) SELECT members, input, output, cache_write, cache_read, cost, units, total_input, total_output, total_write, total_read, total_cost
    FROM ranked WHERE rank<=`+b.Add(normalizeTopSessionsLimit(limit))+` OR (group_key!='' AND EXISTS (SELECT 1 FROM json_each(`+b.Add(string(keys))+`) k WHERE json_extract(k.value,'$[0]')=project AND json_extract(k.value,'$[1]')=machine AND json_extract(k.value,'$[2]')=group_key))`, b.Args()...)
	if err != nil {
		return usageFactsResult{}, err
	}
	totals := make(map[string]TopSessionEntry)
	var ids []string
	for rows.Next() {
		var members string
		var entry TopSessionEntry
		if err := rows.Scan(&members, &entry.InputTokens, &entry.OutputTokens, &entry.CacheCreationTokens, &entry.CacheReadTokens, &entry.Cost.Microdollars, &candidates.Units, &candidates.Total.InputTokens, &candidates.Total.OutputTokens, &candidates.Total.CacheCreationTokens, &candidates.Total.CacheReadTokens, &candidates.Total.Cost.Microdollars); err != nil {
			_ = rows.Close()
			return usageFactsResult{}, err
		}
		var unitIDs []*string
		if err := json.Unmarshal([]byte(members), &unitIDs); err != nil {
			_ = rows.Close()
			return usageFactsResult{}, err
		}
		candidates.SelectedUnits++
		entry.TotalTokens = entry.InputTokens + entry.OutputTokens + entry.CacheCreationTokens + entry.CacheReadTokens
		totals[*unitIDs[0]] = entry
		for _, id := range unitIDs {
			if id != nil {
				ids = append(ids, *id)
			}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return usageFactsResult{}, err
	}
	if err := rows.Close(); err != nil {
		return usageFactsResult{}, err
	}
	candidates.Total.TotalTokens = candidates.Total.InputTokens + candidates.Total.OutputTokens + candidates.Total.CacheCreationTokens + candidates.Total.CacheReadTokens
	rawIDs, err := json.Marshal(ids)
	if err != nil {
		return usageFactsResult{}, err
	}
	rows, err = conn.QueryContext(ctx, `SELECT session_id, baked_agent, project, machine, group_key, baked_started_at, group_label FROM usage_rollup_installs WHERE timezone_id=? AND session_id IN (SELECT value FROM json_each(?)) ORDER BY session_id`, timezoneID, string(rawIDs))
	if err != nil {
		return usageFactsResult{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var entry TopSessionEntry
		if err := rows.Scan(&entry.SessionID, &entry.Agent, &entry.Project, &entry.Machine, &entry.GroupKey, &entry.StartedAt, &entry.GroupLabel); err != nil {
			return usageFactsResult{}, err
		}
		if !filter.TopSessionsByGroup {
			entry.GroupLabel = ""
		}
		total := totals[entry.SessionID]
		if err := addTopSessionTotals(&entry, total); err != nil {
			return usageFactsResult{}, err
		}
		candidates.Entries = append(candidates.Entries, entry)
	}
	return result, rows.Err()
}

func (db *DB) finishUsageTopSessions(ctx context.Context, facts usageFactsResult, filter UsageFilter, limit int) ([]TopSessionEntry, error) {
	candidates := facts.TopSessions
	owners, err := foldUsageTopSessions(candidates.Owners, facts.Groups, filter)
	if err != nil {
		return nil, err
	}
	total := candidates.Total
	if filter.TopSessionsByGroup {
		for _, owner := range owners {
			if err := addTopSessionTotals(&total, owner); err != nil {
				return nil, err
			}
		}
	}

	entries := append(candidates.Entries, owners...)
	ids := make([]string, 0, len(entries))
	for _, entry := range candidates.Entries {
		if !filter.TopSessionsByGroup || entry.GroupKey == "" {
			ids = append(ids, entry.SessionID)
		}
	}
	for _, owner := range owners {
		ids = append(ids, owner.SessionID)
	}

	err = queryChunked(ids, func(chunk []string) error {
		metadata, err := db.loadTopSessionMetadata(ctx, chunk)
		if err != nil {
			return err
		}
		for i := range entries {
			if meta, ok := metadata[entries[i].SessionID]; ok {
				entries[i].DisplayName = meta.displayName
				entries[i].SessionName = meta.sessionName
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !filter.TopSessionsByGroup {
		return SortAndLimitTopSessions(entries, limit, filter.TopSessionsSort, filter.TopSessionsTokenTypes), nil
	}
	slices.SortFunc(entries, func(a, b TopSessionEntry) int { return strings.Compare(a.SessionID, b.SessionID) })
	entries, err = mergeTopSessions(entries)
	if err != nil {
		return nil, err
	}
	return limitGroupedTopSessions(entries, limit, filter.TopSessionsSort, filter.TopSessionsTokenTypes, total, candidates.Units+len(entries)-candidates.SelectedUnits)
}
