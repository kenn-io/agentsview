package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/wesm/agentsview/internal/db"
)

const pgUsageMessageEligibility = `
	m.token_usage != ''
	AND m.model != ''
	AND m.model != '<synthetic>'
	AND s.deleted_at IS NULL`

func usageLocation(f db.UsageFilter) *time.Location {
	if f.Timezone == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(f.Timezone)
	if err != nil {
		return time.Local
	}
	return loc
}

func paddedUTCBound(ts string, hours int) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return t.Add(time.Duration(hours) * time.Hour).
		Format(time.RFC3339)
}

func appendPGUsageFilterClauses(
	query string, pb *paramBuilder, f db.UsageFilter,
) (string, []any) {
	appendCSV := func(
		q, col, csv string, include bool,
	) string {
		if csv == "" {
			return q
		}
		vals := strings.Split(csv, ",")
		op := "IN"
		if !include {
			op = "NOT IN"
		}
		if len(vals) == 1 {
			if include {
				return q + " AND " + col + " = " + pb.add(vals[0])
			}
			return q + " AND " + col + " != " + pb.add(vals[0])
		}
		placeholders := make([]string, len(vals))
		for i, v := range vals {
			placeholders[i] = pb.add(v)
		}
		return q + " AND " + col + " " + op + " (" +
			strings.Join(placeholders, ",") + ")"
	}

	query = appendCSV(query, "s.agent", f.Agent, true)
	query = appendCSV(query, "s.project", f.Project, true)
	query = appendCSV(query, "m.model", f.Model, true)
	query = appendCSV(query, "s.project", f.ExcludeProject, false)
	query = appendCSV(query, "s.agent", f.ExcludeAgent, false)
	query = appendCSV(query, "m.model", f.ExcludeModel, false)

	return query, pb.args
}

func usageDate(ts sql.NullTime, loc *time.Location) string {
	if !ts.Valid {
		return ""
	}
	return ts.Time.In(loc).Format("2006-01-02")
}

// GetDailyUsage returns token usage and cost aggregated by day.
func (s *Store) GetDailyUsage(
	ctx context.Context, f db.UsageFilter,
) (db.DailyUsageResult, error) {
	loc := usageLocation(f)

	pricing, err := s.loadPricingMap(ctx)
	if err != nil {
		return db.DailyUsageResult{},
			fmt.Errorf("loading pg pricing: %w", err)
	}

	var query string
	if f.Breakdowns {
		query = `
SELECT
	COALESCE(m.timestamp, s.started_at) as ts,
	m.model,
	m.token_usage,
	m.claude_message_id,
	m.claude_request_id,
	s.project,
	s.agent
FROM messages m
JOIN sessions s ON m.session_id = s.id
WHERE ` + pgUsageMessageEligibility
	} else {
		query = `
SELECT
	COALESCE(m.timestamp, s.started_at) as ts,
	m.model,
	m.token_usage,
	m.claude_message_id,
	m.claude_request_id
FROM messages m
JOIN sessions s ON m.session_id = s.id
WHERE ` + pgUsageMessageEligibility
	}

	pb := &paramBuilder{}
	if f.From != "" {
		padded := paddedUTCBound(f.From+"T00:00:00Z", -14)
		query += " AND COALESCE(m.timestamp, s.started_at) >= " +
			pb.add(padded) + "::timestamptz"
	}
	if f.To != "" {
		padded := paddedUTCBound(f.To+"T23:59:59Z", 14)
		query += " AND COALESCE(m.timestamp, s.started_at) <= " +
			pb.add(padded) + "::timestamptz"
	}
	query, _ = appendPGUsageFilterClauses(query, pb, f)
	query += ` ORDER BY COALESCE(m.timestamp, s.started_at) ASC,
		m.session_id ASC, m.ordinal ASC`

	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return db.DailyUsageResult{},
			fmt.Errorf("querying daily usage: %w", err)
	}
	defer rows.Close()

	type accumKey struct {
		date    string
		project string
		agent   string
		model   string
	}
	type bucket struct {
		inputTok  int
		outputTok int
		cacheCr   int
		cacheRd   int
		cost      float64
	}
	type dedupKey struct {
		msgID string
		reqID string
	}

	accum := make(map[accumKey]*bucket)
	seen := make(map[dedupKey]struct{})
	var totalSavings float64

	var (
		ts        sql.NullTime
		model     string
		tokenJSON string
		msgID     string
		reqID     string
		project   string
		agent     string
	)
	for rows.Next() {
		var scanErr error
		if f.Breakdowns {
			scanErr = rows.Scan(
				&ts, &model, &tokenJSON,
				&msgID, &reqID, &project, &agent,
			)
		} else {
			scanErr = rows.Scan(
				&ts, &model, &tokenJSON,
				&msgID, &reqID,
			)
		}
		if scanErr != nil {
			return db.DailyUsageResult{},
				fmt.Errorf("scanning daily usage row: %w", scanErr)
		}

		date := usageDate(ts, loc)
		if f.From != "" && date < f.From {
			continue
		}
		if f.To != "" && date > f.To {
			continue
		}

		if msgID != "" && reqID != "" {
			key := dedupKey{msgID: msgID, reqID: reqID}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		}

		usage := gjson.Parse(tokenJSON)
		inputTok := int(usage.Get("input_tokens").Int())
		outputTok := int(usage.Get("output_tokens").Int())
		cacheCrTok := int(
			usage.Get("cache_creation_input_tokens").Int(),
		)
		cacheRdTok := int(
			usage.Get("cache_read_input_tokens").Int(),
		)

		rates := pricing[model]
		cost := (float64(inputTok)*rates.input +
			float64(outputTok)*rates.output +
			float64(cacheCrTok)*rates.cacheCreation +
			float64(cacheRdTok)*rates.cacheRead) / 1_000_000

		readDelta := float64(cacheRdTok) *
			(rates.input - rates.cacheRead) / 1_000_000
		createDelta := float64(cacheCrTok) *
			(rates.input - rates.cacheCreation) / 1_000_000
		totalSavings += readDelta + createDelta

		key := accumKey{
			date:    date,
			project: project,
			agent:   agent,
			model:   model,
		}
		b, ok := accum[key]
		if !ok {
			b = &bucket{}
			accum[key] = b
		}
		b.inputTok += inputTok
		b.outputTok += outputTok
		b.cacheCr += cacheCrTok
		b.cacheRd += cacheRdTok
		b.cost += cost
	}
	if err := rows.Err(); err != nil {
		return db.DailyUsageResult{},
			fmt.Errorf("iterating daily usage rows: %w", err)
	}

	if !f.Breakdowns {
		type dateModelKey struct {
			date  string
			model string
		}
		type modelAccum struct {
			inputTok  int
			outputTok int
			cacheCr   int
			cacheRd   int
			cost      float64
		}
		dm := make(map[dateModelKey]*modelAccum)
		for key, b := range accum {
			dmk := dateModelKey{date: key.date, model: key.model}
			ma, ok := dm[dmk]
			if !ok {
				ma = &modelAccum{}
				dm[dmk] = ma
			}
			ma.inputTok += b.inputTok
			ma.outputTok += b.outputTok
			ma.cacheCr += b.cacheCr
			ma.cacheRd += b.cacheRd
			ma.cost += b.cost
		}

		type dayData struct {
			models map[string]*modelAccum
		}
		days := make(map[string]*dayData)
		for key, ma := range dm {
			dd, ok := days[key.date]
			if !ok {
				dd = &dayData{models: make(map[string]*modelAccum)}
				days[key.date] = dd
			}
			dd.models[key.model] = ma
		}

		dateKeys := make([]string, 0, len(days))
		for d := range days {
			dateKeys = append(dateKeys, d)
		}
		sort.Strings(dateKeys)

		daily := make([]db.DailyUsageEntry, 0, len(dateKeys))
		var totals db.UsageTotals
		for _, date := range dateKeys {
			dd, ok := days[date]
			if !ok || dd == nil {
				continue
			}
			var entry db.DailyUsageEntry
			entry.Date = date

			modelNames := make([]string, 0, len(dd.models))
			for m := range dd.models {
				modelNames = append(modelNames, m)
			}
			sort.Slice(modelNames, func(i, j int) bool {
				left := dd.models[modelNames[i]]
				right := dd.models[modelNames[j]]
				if left == nil || right == nil {
					return left != nil
				}
				ci := left.cost
				cj := right.cost
				if ci != cj {
					return ci > cj
				}
				return modelNames[i] < modelNames[j]
			})
			entry.ModelsUsed = modelNames
			mbd := make([]db.ModelBreakdown, 0, len(modelNames))
			for _, m := range modelNames {
				ma, ok := dd.models[m]
				if !ok || ma == nil {
					continue
				}
				entry.InputTokens += ma.inputTok
				entry.OutputTokens += ma.outputTok
				entry.CacheCreationTokens += ma.cacheCr
				entry.CacheReadTokens += ma.cacheRd
				entry.TotalCost += ma.cost
				mbd = append(mbd, db.ModelBreakdown{
					ModelName:           m,
					InputTokens:         ma.inputTok,
					OutputTokens:        ma.outputTok,
					CacheCreationTokens: ma.cacheCr,
					CacheReadTokens:     ma.cacheRd,
					Cost:                ma.cost,
				})
			}
			entry.ModelBreakdowns = mbd
			daily = append(daily, entry)

			totals.InputTokens += entry.InputTokens
			totals.OutputTokens += entry.OutputTokens
			totals.CacheCreationTokens += entry.CacheCreationTokens
			totals.CacheReadTokens += entry.CacheReadTokens
			totals.TotalCost += entry.TotalCost
		}
		if daily == nil {
			daily = []db.DailyUsageEntry{}
		}
		totals.CacheSavings = totalSavings
		return db.DailyUsageResult{
			Daily:  daily,
			Totals: totals,
		}, nil
	}

	type dayMaps struct {
		models   map[string]bucket
		projects map[string]bucket
		agents   map[string]bucket
	}
	days := make(map[string]*dayMaps, 64)
	for key, b := range accum {
		dm, ok := days[key.date]
		if !ok {
			dm = &dayMaps{
				models:   make(map[string]bucket, 4),
				projects: make(map[string]bucket, 8),
				agents:   make(map[string]bucket, 4),
			}
			days[key.date] = dm
		}

		cur := dm.models[key.model]
		cur.inputTok += b.inputTok
		cur.outputTok += b.outputTok
		cur.cacheCr += b.cacheCr
		cur.cacheRd += b.cacheRd
		cur.cost += b.cost
		dm.models[key.model] = cur

		cur = dm.projects[key.project]
		cur.inputTok += b.inputTok
		cur.outputTok += b.outputTok
		cur.cacheCr += b.cacheCr
		cur.cacheRd += b.cacheRd
		cur.cost += b.cost
		dm.projects[key.project] = cur

		cur = dm.agents[key.agent]
		cur.inputTok += b.inputTok
		cur.outputTok += b.outputTok
		cur.cacheCr += b.cacheCr
		cur.cacheRd += b.cacheRd
		cur.cost += b.cost
		dm.agents[key.agent] = cur
	}

	dateKeys := make([]string, 0, len(days))
	for d := range days {
		dateKeys = append(dateKeys, d)
	}
	sort.Strings(dateKeys)

	daily := make([]db.DailyUsageEntry, 0, len(dateKeys))
	var totals db.UsageTotals
	for _, date := range dateKeys {
		dm, ok := days[date]
		if !ok || dm == nil {
			continue
		}
		var entry db.DailyUsageEntry
		entry.Date = date

		modelNames := make([]string, 0, len(dm.models))
		for m := range dm.models {
			modelNames = append(modelNames, m)
		}
		sort.Slice(modelNames, func(i, j int) bool {
			left := dm.models[modelNames[i]]
			right := dm.models[modelNames[j]]
			ci := left.cost
			cj := right.cost
			if ci != cj {
				return ci > cj
			}
			return modelNames[i] < modelNames[j]
		})
		entry.ModelsUsed = modelNames
		mbd := make([]db.ModelBreakdown, 0, len(modelNames))
		for _, m := range modelNames {
			b, ok := dm.models[m]
			if !ok {
				continue
			}
			entry.InputTokens += b.inputTok
			entry.OutputTokens += b.outputTok
			entry.CacheCreationTokens += b.cacheCr
			entry.CacheReadTokens += b.cacheRd
			entry.TotalCost += b.cost
			mbd = append(mbd, db.ModelBreakdown{
				ModelName:           m,
				InputTokens:         b.inputTok,
				OutputTokens:        b.outputTok,
				CacheCreationTokens: b.cacheCr,
				CacheReadTokens:     b.cacheRd,
				Cost:                b.cost,
			})
		}
		entry.ModelBreakdowns = mbd

		pbd := make([]db.ProjectBreakdown, 0, len(dm.projects))
		for p, b := range dm.projects {
			pbd = append(pbd, db.ProjectBreakdown{
				Project:             p,
				InputTokens:         b.inputTok,
				OutputTokens:        b.outputTok,
				CacheCreationTokens: b.cacheCr,
				CacheReadTokens:     b.cacheRd,
				Cost:                b.cost,
			})
		}
		sort.Slice(pbd, func(i, j int) bool {
			if pbd[i].Cost != pbd[j].Cost {
				return pbd[i].Cost > pbd[j].Cost
			}
			return pbd[i].Project < pbd[j].Project
		})
		entry.ProjectBreakdowns = pbd

		abd := make([]db.AgentBreakdown, 0, len(dm.agents))
		for a, b := range dm.agents {
			abd = append(abd, db.AgentBreakdown{
				Agent:               a,
				InputTokens:         b.inputTok,
				OutputTokens:        b.outputTok,
				CacheCreationTokens: b.cacheCr,
				CacheReadTokens:     b.cacheRd,
				Cost:                b.cost,
			})
		}
		sort.Slice(abd, func(i, j int) bool {
			if abd[i].Cost != abd[j].Cost {
				return abd[i].Cost > abd[j].Cost
			}
			return abd[i].Agent < abd[j].Agent
		})
		entry.AgentBreakdowns = abd

		daily = append(daily, entry)

		totals.InputTokens += entry.InputTokens
		totals.OutputTokens += entry.OutputTokens
		totals.CacheCreationTokens += entry.CacheCreationTokens
		totals.CacheReadTokens += entry.CacheReadTokens
		totals.TotalCost += entry.TotalCost
	}

	if daily == nil {
		daily = []db.DailyUsageEntry{}
	}

	totals.CacheSavings = totalSavings
	return db.DailyUsageResult{
		Daily:  daily,
		Totals: totals,
	}, nil
}

// GetTopSessionsByCost returns sessions ranked by total cost.
func (s *Store) GetTopSessionsByCost(
	ctx context.Context, f db.UsageFilter, limit int,
) ([]db.TopSessionEntry, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	pricing, err := s.loadPricingMap(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading pg pricing: %w", err)
	}

	query := `
SELECT
	s.id,
	COALESCE(s.display_name, s.id),
	s.agent,
	s.project,
	s.started_at,
	m.model,
	m.token_usage,
	m.claude_message_id,
	m.claude_request_id,
	COALESCE(m.timestamp, s.started_at) as ts
FROM messages m
JOIN sessions s ON m.session_id = s.id
WHERE ` + pgUsageMessageEligibility

	pb := &paramBuilder{}
	if f.From != "" {
		padded := paddedUTCBound(f.From+"T00:00:00Z", -14)
		query += " AND COALESCE(m.timestamp, s.started_at) >= " +
			pb.add(padded) + "::timestamptz"
	}
	if f.To != "" {
		padded := paddedUTCBound(f.To+"T23:59:59Z", 14)
		query += " AND COALESCE(m.timestamp, s.started_at) <= " +
			pb.add(padded) + "::timestamptz"
	}
	query, _ = appendPGUsageFilterClauses(query, pb, f)
	query += ` ORDER BY COALESCE(m.timestamp, s.started_at) ASC,
		m.session_id ASC, m.ordinal ASC`

	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("querying top sessions: %w", err)
	}
	defer rows.Close()

	loc := usageLocation(f)

	type sessAccum struct {
		displayName string
		agent       string
		project     string
		startedAt   string
		totalTokens int
		cost        float64
	}
	type dedupKey struct {
		msgID string
		reqID string
	}

	accum := make(map[string]*sessAccum)
	var order []string
	seen := make(map[dedupKey]struct{})

	var (
		sid         string
		displayName string
		agent       string
		project     string
		startedAt   sql.NullTime
		model       string
		tokenJSON   string
		msgID       string
		reqID       string
		ts          sql.NullTime
	)
	for rows.Next() {
		if err := rows.Scan(
			&sid, &displayName, &agent, &project,
			&startedAt, &model, &tokenJSON,
			&msgID, &reqID, &ts,
		); err != nil {
			return nil, fmt.Errorf("scanning top sessions row: %w", err)
		}

		date := usageDate(ts, loc)
		if f.From != "" && date < f.From {
			continue
		}
		if f.To != "" && date > f.To {
			continue
		}

		if msgID != "" && reqID != "" {
			key := dedupKey{msgID: msgID, reqID: reqID}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		}

		usage := gjson.Parse(tokenJSON)
		inputTok := int(usage.Get("input_tokens").Int())
		outputTok := int(usage.Get("output_tokens").Int())
		cacheCrTok := int(
			usage.Get("cache_creation_input_tokens").Int(),
		)
		cacheRdTok := int(
			usage.Get("cache_read_input_tokens").Int(),
		)

		rates := pricing[model]
		cost := (float64(inputTok)*rates.input +
			float64(outputTok)*rates.output +
			float64(cacheCrTok)*rates.cacheCreation +
			float64(cacheRdTok)*rates.cacheRead) / 1_000_000

		sa, ok := accum[sid]
		if !ok {
			started := ""
			if startedAt.Valid {
				started = FormatISO8601(startedAt.Time)
			}
			sa = &sessAccum{
				displayName: displayName,
				agent:       agent,
				project:     project,
				startedAt:   started,
			}
			accum[sid] = sa
			order = append(order, sid)
		}
		sa.totalTokens += inputTok + outputTok + cacheCrTok + cacheRdTok
		sa.cost += cost
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating top sessions rows: %w", err)
	}

	result := make([]db.TopSessionEntry, 0, len(order))
	for _, id := range order {
		sa, ok := accum[id]
		if !ok || sa == nil {
			continue
		}
		result = append(result, db.TopSessionEntry{
			SessionID:   id,
			DisplayName: sa.displayName,
			Agent:       sa.agent,
			Project:     sa.project,
			StartedAt:   sa.startedAt,
			TotalTokens: sa.totalTokens,
			Cost:        sa.cost,
		})
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].Cost != result[j].Cost {
			return result[i].Cost > result[j].Cost
		}
		return result[i].SessionID < result[j].SessionID
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

// GetUsageSessionCounts returns distinct session counts grouped by project and agent.
func (s *Store) GetUsageSessionCounts(
	ctx context.Context, f db.UsageFilter,
) (db.UsageSessionCounts, error) {
	query := `
SELECT
	s.id,
	s.project,
	s.agent,
	m.claude_message_id,
	m.claude_request_id,
	COALESCE(m.timestamp, s.started_at) as ts
FROM messages m
JOIN sessions s ON m.session_id = s.id
WHERE ` + pgUsageMessageEligibility

	pb := &paramBuilder{}
	if f.From != "" {
		padded := paddedUTCBound(f.From+"T00:00:00Z", -14)
		query += " AND COALESCE(m.timestamp, s.started_at) >= " +
			pb.add(padded) + "::timestamptz"
	}
	if f.To != "" {
		padded := paddedUTCBound(f.To+"T23:59:59Z", 14)
		query += " AND COALESCE(m.timestamp, s.started_at) <= " +
			pb.add(padded) + "::timestamptz"
	}
	query, _ = appendPGUsageFilterClauses(query, pb, f)
	query += ` ORDER BY COALESCE(m.timestamp, s.started_at) ASC,
		m.session_id ASC, m.ordinal ASC`

	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return db.UsageSessionCounts{},
			fmt.Errorf("querying session counts: %w", err)
	}
	defer rows.Close()

	loc := usageLocation(f)

	type sessInfo struct {
		project string
		agent   string
	}
	type dedupKey struct {
		msgID string
		reqID string
	}

	seen := make(map[string]sessInfo)
	dedup := make(map[dedupKey]struct{})

	var (
		sid     string
		project string
		agent   string
		msgID   string
		reqID   string
		ts      sql.NullTime
	)
	for rows.Next() {
		if err := rows.Scan(
			&sid, &project, &agent,
			&msgID, &reqID, &ts,
		); err != nil {
			return db.UsageSessionCounts{},
				fmt.Errorf("scanning session counts: %w", err)
		}

		date := usageDate(ts, loc)
		if f.From != "" && date < f.From {
			continue
		}
		if f.To != "" && date > f.To {
			continue
		}

		if msgID != "" && reqID != "" {
			key := dedupKey{msgID: msgID, reqID: reqID}
			if _, dup := dedup[key]; dup {
				continue
			}
			dedup[key] = struct{}{}
		}

		if _, ok := seen[sid]; !ok {
			seen[sid] = sessInfo{
				project: project,
				agent:   agent,
			}
		}
	}
	if err := rows.Err(); err != nil {
		return db.UsageSessionCounts{},
			fmt.Errorf("iterating session counts: %w", err)
	}

	out := db.UsageSessionCounts{
		Total:     len(seen),
		ByProject: make(map[string]int),
		ByAgent:   make(map[string]int),
	}
	for _, info := range seen {
		out.ByProject[info.project]++
		out.ByAgent[info.agent]++
	}

	return out, nil
}
