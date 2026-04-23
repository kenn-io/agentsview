package db

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// UsageFilter controls the date range, agent, and timezone
// for daily usage aggregation queries.
type UsageFilter struct {
	From           string // YYYY-MM-DD, inclusive
	To             string // YYYY-MM-DD, inclusive
	Agent          string // "" for all; supports comma-separated
	Project        string // "" for all; supports comma-separated
	Model          string // "" for all; supports comma-separated
	ExcludeProject string // comma-separated projects to exclude
	ExcludeAgent   string // comma-separated agents to exclude
	ExcludeModel   string // comma-separated models to exclude
	Timezone       string // IANA timezone, "" for UTC
	Breakdowns     bool   // populate Project/AgentBreakdowns per day
}

// appendFilterClauses appends WHERE clauses for all include and
// exclude filters onto the given query and args. Reused by
// GetDailyUsage, GetTopSessionsByCost, and GetUsageSessionCounts
// so the filter contract stays in lockstep.
func (f UsageFilter) appendFilterClauses(
	query string, args []any,
) (string, []any) {
	appendCSV := func(
		q string, a []any, col, csv string, include bool,
	) (string, []any) {
		if csv == "" {
			return q, a
		}
		vals := strings.Split(csv, ",")
		op := "IN"
		if !include {
			op = "NOT IN"
		}
		if len(vals) == 1 {
			if include {
				q += " AND " + col + " = ?"
			} else {
				q += " AND " + col + " != ?"
			}
			a = append(a, vals[0])
		} else {
			ph := make([]string, len(vals))
			for i, v := range vals {
				ph[i] = "?"
				a = append(a, v)
			}
			q += " AND " + col + " " + op +
				" (" + strings.Join(ph, ",") + ")"
		}
		return q, a
	}

	// Include filters.
	query, args = appendCSV(
		query, args, "s.agent", f.Agent, true)
	query, args = appendCSV(
		query, args, "s.project", f.Project, true)
	query, args = appendCSV(
		query, args, "m.model", f.Model, true)

	// Exclude filters.
	query, args = appendCSV(
		query, args, "s.project", f.ExcludeProject, false)
	query, args = appendCSV(
		query, args, "s.agent", f.ExcludeAgent, false)
	query, args = appendCSV(
		query, args, "m.model", f.ExcludeModel, false)

	return query, args
}

// location loads the timezone or returns the system local timezone.
func (f UsageFilter) location() *time.Location {
	if f.Timezone == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(f.Timezone)
	if err != nil {
		return time.Local
	}
	return loc
}

// usageMessageEligibility is the WHERE-clause fragment that selects
// messages eligible for usage / cost aggregation. Every usage query
// (GetDailyUsage, GetUsageSessionCounts, GetTopSessionsByCost) MUST
// reference this constant so the set of counted messages stays
// identical across queries. Drift here is the bug that makes
// sessionCounts and daily totals disagree.
//
// Note: this does NOT filter by s.relationship_type. Duplicate
// messages across fork/subagent boundaries are handled by the
// per-query claude_message_id + claude_request_id dedup in
// GetDailyUsage, which is more precise than a blanket exclusion:
// a fork session can legitimately contribute unique-keyed messages
// that should still be counted (see
// TestGetDailyUsage_DedupesByClaudeMessageAndRequestID).
const usageMessageEligibility = `
    m.token_usage != ''
    AND m.model != ''
    AND m.model != '<synthetic>'
    AND s.deleted_at IS NULL`

// DailyUsageEntry holds token counts and cost for one day.
type DailyUsageEntry struct {
	Date                string             `json:"date"`
	InputTokens         int                `json:"inputTokens"`
	OutputTokens        int                `json:"outputTokens"`
	CacheCreationTokens int                `json:"cacheCreationTokens"`
	CacheReadTokens     int                `json:"cacheReadTokens"`
	TotalCost           float64            `json:"totalCost"`
	ModelsUsed          []string           `json:"modelsUsed"`
	ModelBreakdowns     []ModelBreakdown   `json:"modelBreakdowns,omitempty"`
	ProjectBreakdowns   []ProjectBreakdown `json:"projectBreakdowns,omitempty"`
	AgentBreakdowns     []AgentBreakdown   `json:"agentBreakdowns,omitempty"`
}

// ModelBreakdown holds per-model token and cost breakdown.
type ModelBreakdown struct {
	ModelName           string  `json:"modelName"`
	InputTokens         int     `json:"inputTokens"`
	OutputTokens        int     `json:"outputTokens"`
	CacheCreationTokens int     `json:"cacheCreationTokens"`
	CacheReadTokens     int     `json:"cacheReadTokens"`
	Cost                float64 `json:"cost"`
}

// ProjectBreakdown is the per-project slice of a day's usage.
type ProjectBreakdown struct {
	Project             string  `json:"project"`
	InputTokens         int     `json:"inputTokens"`
	OutputTokens        int     `json:"outputTokens"`
	CacheCreationTokens int     `json:"cacheCreationTokens"`
	CacheReadTokens     int     `json:"cacheReadTokens"`
	Cost                float64 `json:"cost"`
}

// AgentBreakdown is the per-agent slice of a day's usage.
type AgentBreakdown struct {
	Agent               string  `json:"agent"`
	InputTokens         int     `json:"inputTokens"`
	OutputTokens        int     `json:"outputTokens"`
	CacheCreationTokens int     `json:"cacheCreationTokens"`
	CacheReadTokens     int     `json:"cacheReadTokens"`
	Cost                float64 `json:"cost"`
}

// UsageTotals holds aggregate token and cost totals.
type UsageTotals struct {
	InputTokens         int     `json:"inputTokens"`
	OutputTokens        int     `json:"outputTokens"`
	CacheCreationTokens int     `json:"cacheCreationTokens"`
	CacheReadTokens     int     `json:"cacheReadTokens"`
	TotalCost           float64 `json:"totalCost"`
	// CacheSavings is the net dollar delta vs an uncached run:
	// cache reads save (input_rate - cache_read_rate) per token,
	// cache creations cost (input_rate - cache_creation_rate)
	// per token (usually negative because creation is billed
	// above the input rate). Computed from per-model rates so
	// mixed-model workloads get the right number, not a fixed
	// Sonnet proxy.
	CacheSavings float64 `json:"cacheSavings"`
}

// DailyUsageResult wraps the daily entries and totals.
type DailyUsageResult struct {
	Daily  []DailyUsageEntry `json:"daily"`
	Totals UsageTotals       `json:"totals"`
}

// modelRates holds per-model pricing in rate-per-token form.
type modelRates struct {
	input         float64
	output        float64
	cacheCreation float64
	cacheRead     float64
}

// loadPricingMap reads the model_pricing table into a map for
// in-memory joins. This is much faster than a SQL LEFT JOIN
// on every row of the daily usage scan, since the pricing
// table is tiny (a few dozen rows) and lookups are O(1).
func (db *DB) loadPricingMap(
	ctx context.Context,
) (map[string]modelRates, error) {
	rows, err := db.getReader().QueryContext(ctx,
		`SELECT model_pattern,
			input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok
		 FROM model_pricing
		 WHERE model_pattern NOT LIKE '\_%' ESCAPE '\'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]modelRates)
	for rows.Next() {
		var (
			pattern string
			rates   modelRates
		)
		if err := rows.Scan(
			&pattern,
			&rates.input, &rates.output,
			&rates.cacheCreation, &rates.cacheRead,
		); err != nil {
			return nil, err
		}
		out[pattern] = rates
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for model, cp := range db.customPricing {
		out[model] = modelRates{
			input:         cp.Input,
			output:        cp.Output,
			cacheCreation: cp.CacheCreation,
			cacheRead:     cp.CacheRead,
		}
	}

	return out, nil
}

// paddedUTCBound pads a UTC timestamp by hours to cover timezone
// offsets. Positive hours pad forward, negative pad backward.
func paddedUTCBound(ts string, hours int) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return t.Add(time.Duration(hours) * time.Hour).Format(time.RFC3339)
}

// GetDailyUsage returns token usage and cost aggregated by day.
// It scans messages with non-empty token_usage JSON blobs,
// parses them in Go (faster than SQLite's json_extract per row),
// joins against an in-memory pricing map, and buckets by
// local date.
func (db *DB) GetDailyUsage(
	ctx context.Context, f UsageFilter,
) (DailyUsageResult, error) {
	loc := f.location()

	pricing, err := db.loadPricingMap(ctx)
	if err != nil {
		return DailyUsageResult{},
			fmt.Errorf("loading pricing: %w", err)
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
WHERE ` + usageMessageEligibility
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
WHERE ` + usageMessageEligibility
	}

	var args []any

	// Filter on message timestamp (not session started_at) so
	// long-lived sessions that span date boundaries are included.
	// Pad by ±14h to cover all timezone offsets — the actual
	// date filtering happens post-query via localDate.
	if f.From != "" {
		padded := paddedUTCBound(f.From+"T00:00:00Z", -14)
		query += " AND COALESCE(m.timestamp, s.started_at) >= ?"
		args = append(args, padded)
	}
	if f.To != "" {
		padded := paddedUTCBound(f.To+"T23:59:59Z", 14)
		query += " AND COALESCE(m.timestamp, s.started_at) <= ?"
		args = append(args, padded)
	}
	query, args = f.appendFilterClauses(query, args)
	query += ` ORDER BY COALESCE(m.timestamp, s.started_at) ASC,
		m.session_id ASC, m.ordinal ASC`

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return DailyUsageResult{},
			fmt.Errorf("querying daily usage: %w", err)
	}
	defer rows.Close()

	// 4-tuple key for per-(date, project, agent, model) accumulation.
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

	accum := make(map[accumKey]*bucket)

	type dedupKey struct {
		msgID, reqID string
	}
	seen := make(map[dedupKey]struct{})

	// totalSavings is the running sum of per-message cache
	// savings using each row's actual per-model rates. We sum
	// at the message level instead of deriving from totals
	// later because the rate mix varies per workload and a
	// single fallback rate would misreport mixed-model periods.
	var totalSavings float64

	var (
		ts        string
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
				&msgID, &reqID,
				&project, &agent,
			)
		} else {
			scanErr = rows.Scan(
				&ts, &model, &tokenJSON,
				&msgID, &reqID,
			)
		}
		if scanErr != nil {
			return DailyUsageResult{},
				fmt.Errorf("scanning daily usage row: %w", scanErr)
		}

		date := localDate(ts, loc)
		if f.From != "" && date < f.From {
			continue
		}
		if f.To != "" && date > f.To {
			continue
		}

		// Dedup AFTER the date filter so out-of-range rows
		// (pulled in by the ±14h timezone padding) don't mark
		// a key as seen and suppress the in-range duplicate.
		if msgID != "" && reqID != "" {
			key := dedupKey{msgID: msgID, reqID: reqID}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		}

		// token_usage is written by our parsers and never by
		// user input, so we trust it to be valid JSON. gjson
		// is permissive enough that a truncated-tail row still
		// yields its leading fields; a fully garbage row would
		// return zeros, but that path is not reachable from
		// any known parser. Skipping gjson.Valid here preserves
		// the hot-path speedup (O(n) per row -> not free on a
		// 310k-row scan).
		usage := gjson.Parse(tokenJSON)
		inputTok := int(usage.Get("input_tokens").Int())
		outputTok := int(usage.Get("output_tokens").Int())
		cacheCrTok := int(
			usage.Get("cache_creation_input_tokens").Int())
		cacheRdTok := int(
			usage.Get("cache_read_input_tokens").Int())

		rates := pricing[model]
		cost := (float64(inputTok)*rates.input +
			float64(outputTok)*rates.output +
			float64(cacheCrTok)*rates.cacheCreation +
			float64(cacheRdTok)*rates.cacheRead) / 1_000_000

		// Per-message cache delta: reads earn (input - cacheRead)
		// per token, creations earn (input - cacheCreate) per
		// token (usually negative). Zero-rate fallbacks fold
		// through cleanly.
		readDelta := float64(cacheRdTok) *
			(rates.input - rates.cacheRead) / 1_000_000
		crDelta := float64(cacheCrTok) *
			(rates.input - rates.cacheCreation) / 1_000_000
		totalSavings += readDelta + crDelta

		key := accumKey{
			date: date, project: project,
			agent: agent, model: model,
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
		return DailyUsageResult{},
			fmt.Errorf("iterating daily usage rows: %w", err)
	}

	// Two paths: without breakdowns (CLI, fast) and with breakdowns
	// (web UI). The fast path uses the original (date, model)
	// grouping with no extra column reads. The breakdown path adds
	// project/agent dimensions and builds three decomposition slices.

	if !f.Breakdowns {
		// Fast path: group by (date, model) only.
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
				dd = &dayData{
					models: make(map[string]*modelAccum),
				}
				days[key.date] = dd
			}
			dd.models[key.model] = ma
		}

		dateKeys := make([]string, 0, len(days))
		for d := range days {
			dateKeys = append(dateKeys, d)
		}
		sort.Strings(dateKeys)

		daily := make([]DailyUsageEntry, 0, len(dateKeys))
		var totals UsageTotals

		for _, date := range dateKeys {
			dd := days[date]
			var entry DailyUsageEntry
			entry.Date = date

			modelNames := make([]string, 0, len(dd.models))
			for m := range dd.models {
				modelNames = append(modelNames, m)
			}
			sort.Slice(modelNames, func(i, j int) bool {
				ci := dd.models[modelNames[i]].cost
				cj := dd.models[modelNames[j]].cost
				if ci != cj {
					return ci > cj
				}
				return modelNames[i] < modelNames[j]
			})
			entry.ModelsUsed = modelNames
			mbd := make(
				[]ModelBreakdown, 0, len(modelNames),
			)
			for _, m := range modelNames {
				ma := dd.models[m]
				entry.InputTokens += ma.inputTok
				entry.OutputTokens += ma.outputTok
				entry.CacheCreationTokens += ma.cacheCr
				entry.CacheReadTokens += ma.cacheRd
				entry.TotalCost += ma.cost
				mbd = append(mbd, ModelBreakdown{
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
			daily = []DailyUsageEntry{}
		}
		totals.CacheSavings = totalSavings
		return DailyUsageResult{
			Daily:  daily,
			Totals: totals,
		}, nil
	}

	// Breakdown path: single walk builds model/project/agent maps.
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

	daily := make([]DailyUsageEntry, 0, len(dateKeys))
	var totals UsageTotals

	for _, date := range dateKeys {
		dm := days[date]
		var entry DailyUsageEntry
		entry.Date = date

		modelNames := make([]string, 0, len(dm.models))
		for m := range dm.models {
			modelNames = append(modelNames, m)
		}
		sort.Slice(modelNames, func(i, j int) bool {
			ci := dm.models[modelNames[i]].cost
			cj := dm.models[modelNames[j]].cost
			if ci != cj {
				return ci > cj
			}
			return modelNames[i] < modelNames[j]
		})
		entry.ModelsUsed = modelNames
		mbd := make(
			[]ModelBreakdown, 0, len(modelNames),
		)
		for _, m := range modelNames {
			b := dm.models[m]
			entry.InputTokens += b.inputTok
			entry.OutputTokens += b.outputTok
			entry.CacheCreationTokens += b.cacheCr
			entry.CacheReadTokens += b.cacheRd
			entry.TotalCost += b.cost
			mbd = append(mbd, ModelBreakdown{
				ModelName:           m,
				InputTokens:         b.inputTok,
				OutputTokens:        b.outputTok,
				CacheCreationTokens: b.cacheCr,
				CacheReadTokens:     b.cacheRd,
				Cost:                b.cost,
			})
		}
		entry.ModelBreakdowns = mbd

		pbd := make(
			[]ProjectBreakdown, 0, len(dm.projects),
		)
		for p, b := range dm.projects {
			pbd = append(pbd, ProjectBreakdown{
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

		abd := make(
			[]AgentBreakdown, 0, len(dm.agents),
		)
		for a, b := range dm.agents {
			abd = append(abd, AgentBreakdown{
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
		daily = []DailyUsageEntry{}
	}

	totals.CacheSavings = totalSavings
	return DailyUsageResult{
		Daily:  daily,
		Totals: totals,
	}, nil
}

// TopSessionEntry is one row in the "top sessions by cost" result.
type TopSessionEntry struct {
	SessionID   string  `json:"sessionId"`
	DisplayName string  `json:"displayName"`
	Agent       string  `json:"agent"`
	Project     string  `json:"project"`
	StartedAt   string  `json:"startedAt"`
	TotalTokens int     `json:"totalTokens"`
	Cost        float64 `json:"cost"`
}

// GetTopSessionsByCost returns sessions ranked by total cost
// over the filter range. Default limit 20, max 100.
func (db *DB) GetTopSessionsByCost(
	ctx context.Context, f UsageFilter, limit int,
) ([]TopSessionEntry, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	pricing, err := db.loadPricingMap(ctx)
	if err != nil {
		return nil,
			fmt.Errorf("loading pricing: %w", err)
	}

	query := `
SELECT
	s.id,
	COALESCE(NULLIF(s.display_name, ''), NULLIF(s.first_message, ''), NULLIF(s.project, ''), s.id),
	s.agent,
	s.project,
	COALESCE(s.started_at, ''),
	m.model,
	m.token_usage,
	m.claude_message_id,
	m.claude_request_id,
	COALESCE(m.timestamp, s.started_at) as ts
FROM messages m
JOIN sessions s ON m.session_id = s.id
WHERE ` + usageMessageEligibility

	var args []any

	if f.From != "" {
		padded := paddedUTCBound(f.From+"T00:00:00Z", -14)
		query += " AND COALESCE(m.timestamp, s.started_at) >= ?"
		args = append(args, padded)
	}
	if f.To != "" {
		padded := paddedUTCBound(f.To+"T23:59:59Z", 14)
		query += " AND COALESCE(m.timestamp, s.started_at) <= ?"
		args = append(args, padded)
	}
	query, args = f.appendFilterClauses(query, args)
	// Deterministic order so the dedup "winner" (the session
	// that gets credit for a duplicate message.id + request.id
	// pair) is stable across runs: earliest timestamp wins,
	// then session_id, then message ordinal.
	query += ` ORDER BY COALESCE(m.timestamp, s.started_at) ASC,
		m.session_id ASC, m.ordinal ASC`

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil,
			fmt.Errorf("querying top sessions: %w", err)
	}
	defer rows.Close()

	loc := f.location()

	type sessAccum struct {
		displayName string
		agent       string
		project     string
		startedAt   string
		totalTokens int
		cost        float64
	}

	accum := make(map[string]*sessAccum)
	// Track insertion order for stable iteration.
	var order []string

	// Dedup duplicate Claude messages across fork/subagent
	// boundaries so per-session totals match the aggregate
	// totals from GetDailyUsage. Same key and ordering rules.
	type dedupKey struct {
		msgID, reqID string
	}
	seen := make(map[dedupKey]struct{})

	var (
		sid         string
		displayName string
		agent       string
		project     string
		startedAt   string
		model       string
		tokenJSON   string
		msgID       string
		reqID       string
		ts          string
	)
	for rows.Next() {
		if err := rows.Scan(
			&sid, &displayName, &agent, &project,
			&startedAt, &model, &tokenJSON,
			&msgID, &reqID, &ts,
		); err != nil {
			return nil,
				fmt.Errorf("scanning top sessions row: %w", err)
		}

		// Post-query date filter (same as GetDailyUsage).
		date := localDate(ts, loc)
		if f.From != "" && date < f.From {
			continue
		}
		if f.To != "" && date > f.To {
			continue
		}

		// Dedup AFTER the date filter, matching GetDailyUsage,
		// so out-of-range rows pulled in by the ±14h padding
		// don't claim a key and suppress the in-range duplicate.
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
			usage.Get("cache_creation_input_tokens").Int())
		cacheRdTok := int(
			usage.Get("cache_read_input_tokens").Int())

		rates := pricing[model]
		cost := (float64(inputTok)*rates.input +
			float64(outputTok)*rates.output +
			float64(cacheCrTok)*rates.cacheCreation +
			float64(cacheRdTok)*rates.cacheRead) / 1_000_000

		sa, ok := accum[sid]
		if !ok {
			sa = &sessAccum{
				displayName: displayName,
				agent:       agent,
				project:     project,
				startedAt:   startedAt,
			}
			accum[sid] = sa
			order = append(order, sid)
		}
		sa.totalTokens += inputTok + outputTok +
			cacheCrTok + cacheRdTok
		sa.cost += cost
	}
	if err := rows.Err(); err != nil {
		return nil,
			fmt.Errorf("iterating top sessions rows: %w", err)
	}

	result := make([]TopSessionEntry, 0, len(order))
	for _, id := range order {
		sa := accum[id]
		result = append(result, TopSessionEntry{
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

// UsageSessionCounts holds distinct session counts grouped by
// project and agent over a filter range.
type UsageSessionCounts struct {
	Total     int            `json:"total"`
	ByProject map[string]int `json:"byProject"`
	ByAgent   map[string]int `json:"byAgent"`
}

// GetUsageSessionCounts returns distinct session counts grouped
// by project and agent. Sessions spanning multiple days count
// once. Soft-deleted sessions are excluded via
// usageMessageEligibility.
//
// Like GetDailyUsage and GetTopSessionsByCost, this query pads
// the UTC bounds by +/-14h and applies a post-query localDate
// filter so timezone-boundary messages are counted correctly.
func (db *DB) GetUsageSessionCounts(
	ctx context.Context, f UsageFilter,
) (UsageSessionCounts, error) {
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
WHERE ` + usageMessageEligibility

	var args []any

	if f.From != "" {
		padded := paddedUTCBound(f.From+"T00:00:00Z", -14)
		query += " AND COALESCE(m.timestamp, s.started_at) >= ?"
		args = append(args, padded)
	}
	if f.To != "" {
		padded := paddedUTCBound(f.To+"T23:59:59Z", 14)
		query += " AND COALESCE(m.timestamp, s.started_at) <= ?"
		args = append(args, padded)
	}
	query, args = f.appendFilterClauses(query, args)
	// Deterministic ordering so the Claude dedup winner — the
	// session that "owns" a shared message — is stable across
	// runs. Matches GetDailyUsage / GetTopSessionsByCost so all
	// three queries agree on which session gets credit.
	query += ` ORDER BY COALESCE(m.timestamp, s.started_at) ASC,
		m.session_id ASC, m.ordinal ASC`

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return UsageSessionCounts{},
			fmt.Errorf("querying session counts: %w", err)
	}
	defer rows.Close()

	loc := f.location()

	// Track which sessions pass the localDate filter via a
	// set of seen session IDs. Each session is counted once
	// regardless of how many qualifying messages it has.
	type sessInfo struct {
		project string
		agent   string
	}
	seen := make(map[string]sessInfo)

	// Claude message dedup mirrors GetDailyUsage: if a session
	// only qualifies because of messages that are duplicates of
	// an earlier session's rows (fork/subagent replays), that
	// session should NOT be counted. Otherwise sessionCounts
	// would disagree with the deduped token totals — a fork
	// with zero unique messages would inflate the count even
	// though it contributes zero cost.
	type dedupKey struct {
		msgID, reqID string
	}
	dedup := make(map[dedupKey]struct{})

	var (
		sid     string
		project string
		agent   string
		msgID   string
		reqID   string
		ts      string
	)
	for rows.Next() {
		if err := rows.Scan(
			&sid, &project, &agent,
			&msgID, &reqID, &ts,
		); err != nil {
			return UsageSessionCounts{},
				fmt.Errorf("scanning session counts: %w", err)
		}

		// Post-query date filter (same as GetDailyUsage).
		date := localDate(ts, loc)
		if f.From != "" && date < f.From {
			continue
		}
		if f.To != "" && date > f.To {
			continue
		}

		// Dedup AFTER the date filter, matching the other two
		// queries so ±14h padding rows don't claim keys.
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
		return UsageSessionCounts{},
			fmt.Errorf("iterating session counts: %w", err)
	}

	out := UsageSessionCounts{
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
