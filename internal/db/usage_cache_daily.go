package db

import (
	"context"
	"fmt"
	"sort"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

type usageDailyFactsBucket struct {
	input, output, cacheWrite, cacheRead int
	cost                                 money.Money
}

type usageDailyFactsSessionCost struct {
	estimated     map[usageCostAllocationKey]money.Money
	authoritative *money.Money
}

// GetDailyUsage serves exact daily usage from timezone rollups. Archive state
// is captured first, missing facts and rollups are built without holding that
// snapshot, and one cache read transaction verifies the installed rows.
func (db *DB) GetDailyUsage(
	ctx context.Context, filter UsageFilter,
) (DailyUsageResult, error) {
	_, facts, resolver, err := db.queryUsageRollups(
		ctx, filter, usageQueryKindToken, true)
	if err != nil {
		return DailyUsageResult{}, err
	}
	return db.assembleDailyUsageFacts(ctx, filter, facts, resolver)
}

func (snapshot *usageQuerySnapshot) dropDeleted(
	results map[string]usageFillResult,
) {
	if len(results) == 0 {
		return
	}
	sessions := snapshot.Sessions[:0]
	for _, session := range snapshot.Sessions {
		if !results[session.ID].Deleted {
			sessions = append(sessions, session)
		}
	}
	versions := snapshot.Versions[:0]
	for _, version := range snapshot.Versions {
		if !results[version.SessionID].Deleted {
			versions = append(versions, version)
		} else {
			delete(results, version.SessionID)
		}
	}
	snapshot.Sessions = sessions
	snapshot.Versions = versions
}

func (db *DB) assembleDailyUsageFacts(
	ctx context.Context, filter UsageFilter, facts usageFactsResult,
	resolver *export.PricingResolver,
) (DailyUsageResult, error) {
	accum := make(map[usageCostAllocationKey]*usageDailyFactsBucket)
	sessionCosts := make(map[string]usageDailyFactsSessionCost)
	projectLabels := make(map[string]struct{})
	useAuthoritative := filter.Model == "" && filter.ExcludeModel == ""
	var totalSavings money.Money

	for _, group := range facts.Groups {
		key := usageCostAllocationKey{
			date: group.Date, project: group.Project, agent: group.Agent,
			machine: group.Machine, model: group.Model,
			providerID: group.ProviderID,
		}
		bucket := accum[key]
		if bucket == nil {
			bucket = &usageDailyFactsBucket{}
			accum[key] = bucket
		}
		bucket.input += int(group.InputTokens)
		bucket.output += int(group.OutputTokens)
		bucket.cacheWrite += int(group.CacheCreationTokens)
		bucket.cacheRead += int(group.CacheReadTokens)

		groupCost := money.Money{Microdollars: group.CostMicrodollars}
		session := sessionCosts[group.SessionID]
		if session.estimated == nil {
			session.estimated = make(map[usageCostAllocationKey]money.Money)
		}
		var addErr error
		session.estimated[key], addErr = money.Add(session.estimated[key], groupCost)
		if addErr != nil {
			return DailyUsageResult{}, fmt.Errorf(
				"summing daily usage session cost: %w", addErr)
		}
		if useAuthoritative && group.AuthoritativeCostMicrodollars != nil {
			value := money.Money{Microdollars: *group.AuthoritativeCostMicrodollars}
			session.authoritative = &value
		}
		sessionCosts[group.SessionID] = session

		totalSavings, addErr = money.Add(totalSavings,
			money.Money{Microdollars: group.SavingsMicrodollars})
		if addErr != nil {
			return DailyUsageResult{}, fmt.Errorf(
				"summing daily usage cache savings: %w", addErr)
		}
		if group.Project != "" {
			projectLabels[group.Project] = struct{}{}
		}
		if err := recordUsageFactsPricing(resolver, group); err != nil {
			return DailyUsageResult{}, fmt.Errorf("recording daily usage pricing: %w", err)
		}
	}

	sessionIDs := make([]string, 0, len(sessionCosts))
	for sessionID := range sessionCosts {
		sessionIDs = append(sessionIDs, sessionID)
	}
	sort.Strings(sessionIDs)
	for _, sessionID := range sessionIDs {
		session := sessionCosts[sessionID]
		costs := session.estimated
		if session.authoritative != nil {
			costs = allocateUsageCostByKey(*session.authoritative, session.estimated)
			resolver.RecordUnattributedReported()
		}
		for key, cost := range costs {
			bucket := accum[key]
			if bucket == nil {
				return DailyUsageResult{}, fmt.Errorf(
					"daily usage cost has no token bucket")
			}
			var addErr error
			bucket.cost, addErr = money.Add(bucket.cost, cost)
			if addErr != nil {
				return DailyUsageResult{}, fmt.Errorf(
					"summing daily usage cost: %w", addErr)
			}
		}
	}

	daily, totals, err := buildDailyUsageFactsEntries(accum, filter.Breakdowns)
	if err != nil {
		return DailyUsageResult{}, err
	}
	totals.CacheSavings = totalSavings
	for key, bucket := range accum {
		totals.CopilotAICredits += AICreditsFromCost(key.agent, bucket.cost)
	}

	var sessionCounts UsageSessionCounts
	if !filter.SkipSessionCounts {
		sessionCounts = NewUsageSessionCounts(facts.MatchingSessions)
	}
	projects, err := db.BuildProjectIdentityMap(ctx, sortedSetKeys(projectLabels))
	if err != nil {
		return DailyUsageResult{}, err
	}
	projectRows := DailyUsageResult{Daily: daily, SessionCounts: sessionCounts}
	SanitizeDailyUsageProjectLabelsWithCatalog(&projectRows, projects)
	pricingBlock, err := resolver.BuildBlock()
	if err != nil {
		return DailyUsageResult{}, fmt.Errorf("building pricing block: %w", err)
	}
	return DailyUsageResult{
		SchemaVersion: export.UsageDailySchemaVersion,
		Pricing:       &pricingBlock,
		Projects:      export.ProjectMapForWire(projects),
		Daily:         projectRows.Daily,
		Totals:        totals,
		SessionCounts: projectRows.SessionCounts,
	}, nil
}

func recordUsageFactsPricing(
	resolver *export.PricingResolver, group usageFactsGroup,

) error {
	timestamp := usagePricingTimestamp(group.PricingTimestamp)
	_, baseLookup := resolver.ResolveAt(
		group.Model, group.PricedModel,
		timestamp,
	)
	for range group.ReportedCount {
		resolver.RecordResolvedReported(group.Model, group.PricedModel, baseLookup)
	}
	requestCount := group.ComputedRequestCount
	if group.BandThreshold == nil {
		requestCount = group.BaseRequestCount
	}
	if group.ComputedAggregateCount == 0 && requestCount == 0 {
		return nil
	}
	_, computedLookup, err := resolver.ResolveBilledAt(
		group.ProviderID, group.Model, group.PricedModel, timestamp)
	if err != nil {
		return fmt.Errorf("resolving billed pricing for model %q: %w", group.Model, err)
	}
	for range group.ComputedAggregateCount {
		resolver.RecordResolvedComputedAggregate(
			group.Model, group.PricedModel, computedLookup)
	}
	inputTokens := 0
	if group.BandThreshold != nil {
		inputTokens = *group.BandThreshold + 1
	}
	for range requestCount {
		resolver.RecordResolvedComputedRequest(
			group.Model, group.PricedModel, computedLookup, inputTokens, 0, 0)
	}
	return nil
}

func buildDailyUsageFactsEntries(
	accum map[usageCostAllocationKey]*usageDailyFactsBucket,
	breakdowns bool,
) ([]DailyUsageEntry, UsageTotals, error) {
	type dayFacts struct {
		models, projects, agents, machines map[string]usageDailyFactsBucket
	}
	days := make(map[string]*dayFacts)
	for key, bucket := range accum {
		day := days[key.date]
		if day == nil {
			day = &dayFacts{models: make(map[string]usageDailyFactsBucket)}
			if breakdowns {
				day.projects = make(map[string]usageDailyFactsBucket)
				day.agents = make(map[string]usageDailyFactsBucket)
				day.machines = make(map[string]usageDailyFactsBucket)
			}
			days[key.date] = day
		}
		var err error
		day.models[key.model], err = addUsageDailyFactsBucket(day.models[key.model], *bucket)
		if err != nil {
			return nil, UsageTotals{}, err
		}
		if breakdowns {
			day.projects[key.project], err = addUsageDailyFactsBucket(
				day.projects[key.project], *bucket)
			if err == nil {
				day.agents[key.agent], err = addUsageDailyFactsBucket(
					day.agents[key.agent], *bucket)
			}
			if err == nil {
				day.machines[key.machine], err = addUsageDailyFactsBucket(
					day.machines[key.machine], *bucket)
			}
			if err != nil {
				return nil, UsageTotals{}, err
			}
		}
	}

	dates := make([]string, 0, len(days))
	for date := range days {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	daily := make([]DailyUsageEntry, 0, len(dates))
	var totals UsageTotals
	for _, date := range dates {
		day := days[date]
		if day == nil {
			return nil, UsageTotals{}, fmt.Errorf(
				"daily usage date %q has no aggregate", date)
		}
		entry := DailyUsageEntry{Date: date}
		models := sortedUsageDailyBucketNames(day.models)
		entry.ModelsUsed = append([]string(nil), models...)
		for _, model := range models {
			bucket := day.models[model]
			if err := addUsageDailyFactsEntryTotals(&entry, bucket); err != nil {
				return nil, UsageTotals{}, err
			}
			entry.ModelBreakdowns = append(entry.ModelBreakdowns, ModelBreakdown{
				ModelName: model, InputTokens: bucket.input,
				OutputTokens: bucket.output, CacheCreationTokens: bucket.cacheWrite,
				CacheReadTokens: bucket.cacheRead, Cost: bucket.cost,
			})
		}
		if breakdowns {
			entry.ProjectBreakdowns = usageDailyProjectBreakdowns(day.projects)
			entry.AgentBreakdowns = usageDailyAgentBreakdowns(day.agents)
			entry.MachineBreakdowns = usageDailyMachineBreakdowns(day.machines)
		}
		var err error
		totals.TotalCost, err = money.Add(totals.TotalCost, entry.TotalCost)
		if err != nil {
			return nil, UsageTotals{}, fmt.Errorf("summing daily usage total: %w", err)
		}
		totals.InputTokens += entry.InputTokens
		totals.OutputTokens += entry.OutputTokens
		totals.CacheCreationTokens += entry.CacheCreationTokens
		totals.CacheReadTokens += entry.CacheReadTokens
		daily = append(daily, entry)
	}
	return daily, totals, nil
}

func addUsageDailyFactsBucket(
	left, right usageDailyFactsBucket,
) (usageDailyFactsBucket, error) {
	left.input += right.input
	left.output += right.output
	left.cacheWrite += right.cacheWrite
	left.cacheRead += right.cacheRead
	var err error
	left.cost, err = money.Add(left.cost, right.cost)
	if err != nil {
		return usageDailyFactsBucket{}, fmt.Errorf("summing daily breakdown cost: %w", err)
	}
	return left, nil
}

func addUsageDailyFactsEntryTotals(
	entry *DailyUsageEntry, bucket usageDailyFactsBucket,
) error {
	entry.InputTokens += bucket.input
	entry.OutputTokens += bucket.output
	entry.CacheCreationTokens += bucket.cacheWrite
	entry.CacheReadTokens += bucket.cacheRead
	var err error
	entry.TotalCost, err = money.Add(entry.TotalCost, bucket.cost)
	if err != nil {
		return fmt.Errorf("summing daily entry cost: %w", err)
	}
	return nil
}

func sortedUsageDailyBucketNames(
	buckets map[string]usageDailyFactsBucket,
) []string {
	names := make([]string, 0, len(buckets))
	for name := range buckets {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		left, right := buckets[names[i]], buckets[names[j]]
		if left.cost.Microdollars != right.cost.Microdollars {
			return left.cost.Microdollars > right.cost.Microdollars
		}
		return names[i] < names[j]
	})
	return names
}

func usageDailyProjectBreakdowns(
	buckets map[string]usageDailyFactsBucket,
) []ProjectBreakdown {
	result := make([]ProjectBreakdown, 0, len(buckets))
	for name, bucket := range buckets {
		result = append(result, ProjectBreakdown{
			Project: name, InputTokens: bucket.input, OutputTokens: bucket.output,
			CacheCreationTokens: bucket.cacheWrite,
			CacheReadTokens:     bucket.cacheRead, Cost: bucket.cost,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Cost.Microdollars != result[j].Cost.Microdollars {
			return result[i].Cost.Microdollars > result[j].Cost.Microdollars
		}
		return result[i].Project < result[j].Project
	})
	return result
}

func usageDailyAgentBreakdowns(
	buckets map[string]usageDailyFactsBucket,
) []AgentBreakdown {
	result := make([]AgentBreakdown, 0, len(buckets))
	for name, bucket := range buckets {
		result = append(result, AgentBreakdown{
			Agent: name, InputTokens: bucket.input, OutputTokens: bucket.output,
			CacheCreationTokens: bucket.cacheWrite,
			CacheReadTokens:     bucket.cacheRead, Cost: bucket.cost,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Cost.Microdollars != result[j].Cost.Microdollars {
			return result[i].Cost.Microdollars > result[j].Cost.Microdollars
		}
		return result[i].Agent < result[j].Agent
	})
	return result
}

func usageDailyMachineBreakdowns(
	buckets map[string]usageDailyFactsBucket,
) []MachineBreakdown {
	result := make([]MachineBreakdown, 0, len(buckets))
	for name, bucket := range buckets {
		result = append(result, MachineBreakdown{
			MachineName: name, InputTokens: bucket.input,
			OutputTokens: bucket.output, CacheCreationTokens: bucket.cacheWrite,
			CacheReadTokens: bucket.cacheRead, Cost: bucket.cost,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Cost.Microdollars != result[j].Cost.Microdollars {
			return result[i].Cost.Microdollars > result[j].Cost.Microdollars
		}
		return result[i].MachineName < result[j].MachineName
	})
	return result
}
