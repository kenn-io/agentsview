package db

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/usagefacts"
)

type usagePriceInput struct {
	Fact                     usagefacts.Fact
	Timestamp, ReportedModel string
}

type usagePriceResult struct {
	PricedModel, MatchedPattern, RateHash string
	RateOK                                bool
	Cost, Savings                         money.Money
	AuthoritativeCost                     *money.Money
	BandThreshold                         *int
	ComputedRequest, ComputedAggregate    int
	Reported, BaseRequest                 int
}

type usageRollupFact struct {
	CachedSessionID      int64
	FactIndex            int
	SourceSessionID      string
	AttributionSessionID string
	LocalDate            string
	Model                string
	Agent                string
	EffectiveMillis      *int64
	EffectiveNanos       *int64
	DedupTimestamp       string
	Fact                 usagefacts.Fact
}

type usageDailyContribution struct {
	AttributedSessionID, LocalDate             string
	ReportedModel, PricedModel, MatchedPattern string
	RateOK                                     bool
	RateHash                                   string
	BandThreshold                              *int
	InputTokens, OutputTokens, ReasoningTokens int64
	CacheCreationTokens, CacheReadTokens       int64
	WebSearchRequests                          int64
	CostMicrodollars, SavingsMicrodollars      int64
	AuthoritativeCostMicrodollars              *int64
	ComputedRequestCount                       int
	ComputedAggregateCount                     int
	ReportedCount, BaseRequestCount            int
	DiscardedSnapshotOutputTokens              int64
}

type usageActivityContribution struct {
	AttributedSessionID, LocalDate, Model string
	UserMessageCount                      int
}

type usageExceptionRow struct {
	GroupKind, GroupKey string
	Fact                usageRollupFact
}

type usageRollupBuild struct {
	SessionID, Agent, StartedAt, PricingHash string
	Source                                   usageSourceVersion
	FactRevision                             int64
	Daily                                    []usageDailyContribution
	Activity                                 []usageActivityContribution
	Exceptions                               []usageExceptionRow
}

func isUsageRollupException(fact usageRollupFact) bool {
	return usageRollupSnapshotKey(fact) != "" || usageRollupGeneralKey(fact) != ""
}

func priceUsageFact(
	input usagePriceInput, resolver *export.PricingResolver,
) (usagePriceResult, error) {
	model := input.ReportedModel
	if model == "" {
		model = input.Fact.Model
	}
	pricedModel, lookup := resolver.Resolve(model, usageLookupModel(model, input.Timestamp))
	if err := validateNonnegativeUsageRates(lookup.Rates); err != nil {
		return usagePriceResult{}, fmt.Errorf("pricing usage row for model %q: %w", model, err)
	}
	result := usagePriceResult{
		PricedModel: pricedModel, MatchedPattern: lookup.Pattern,
		RateHash: usageRateHash(
			model, pricedModel, lookup.Pattern, lookup.OK, lookup.Rates),
		RateOK: lookup.OK,
	}
	selectedRates := lookup.Rates
	if input.Fact.RequestScoped {
		selectedRates, result.BandThreshold = usageRatesAndBandForFact(
			lookup.Rates, input.Fact)
	}
	var err error
	reported := input.Fact.ReportedCostMicrodollars
	if reported != nil && input.Fact.CostSource != CopilotReportedCostSource {
		result.Cost = money.Money{Microdollars: *reported}
		result.Reported = 1
	} else {
		result.Cost, err = selectedRates.CostForTokensScoped(
			false, int(input.Fact.InputTokens), int(input.Fact.OutputTokens),
			int(input.Fact.ReasoningTokens), int(input.Fact.CacheCreationTokens),
			int(input.Fact.CacheReadTokens))
		if err != nil {
			return usagePriceResult{}, fmt.Errorf("pricing usage row for model %q: %w", model, err)
		}
		result.Cost, err = export.AddWebSearchFee(
			result.Cost, int(input.Fact.WebSearchRequests))
		if err != nil {
			return usagePriceResult{}, fmt.Errorf("pricing usage row for model %q: %w", model, err)
		}
		if input.Fact.RequestScoped {
			result.ComputedRequest = 1
			if result.BandThreshold == nil {
				result.BaseRequest = 1
			}
		} else {
			result.ComputedAggregate = 1
		}
		if reported != nil {
			value := money.Money{Microdollars: *reported}
			result.AuthoritativeCost = &value
		}
	}
	readRate, err := money.Sub(selectedRates.InputPerMTok, selectedRates.CacheReadPerMTok)
	if err != nil {
		return usagePriceResult{}, fmt.Errorf("deriving cache read rate for model %q: %w", model, err)
	}
	writeRate, err := money.Sub(selectedRates.InputPerMTok, selectedRates.CacheWritePerMTok)
	if err != nil {
		return usagePriceResult{}, fmt.Errorf("deriving cache creation rate for model %q: %w", model, err)
	}
	result.Savings, err = money.SignedCostPerMillion([]money.RatedTokens{
		{Tokens: input.Fact.CacheReadTokens, Rate: readRate},
		{Tokens: input.Fact.CacheCreationTokens, Rate: writeRate},
	})
	if err != nil {
		return usagePriceResult{}, fmt.Errorf("pricing cache savings for model %q: %w", model, err)
	}
	return result, nil
}

func usageRatesAndBandForFact(
	rates export.ModelRates, fact usagefacts.Fact,
) (export.ModelRates, *int) {
	totalInput := fact.InputTokens + fact.CacheCreationTokens + fact.CacheReadTokens
	var threshold *int
	for _, band := range rates.Bands {
		if totalInput > int64(band.AboveInputTokens) &&
			(threshold == nil || band.AboveInputTokens > *threshold) {
			value := band.AboveInputTokens
			threshold = &value
		}
	}
	if threshold == nil {
		return rates, nil
	}
	return rates.RatesForTokens(
		int(fact.InputTokens), int(fact.CacheCreationTokens),
		int(fact.CacheReadTokens)), threshold
}

func buildUsageDailyContributions(
	facts []usageRollupFact, resolver *export.PricingResolver,
) ([]usageDailyContribution, []usageRollupFact, error) {
	type key struct {
		session, date, model, priced, pattern, rateHash string
		rateOK                                          bool
		band                                            int
	}
	rows := make(map[key]*usageDailyContribution)
	var exceptions []usageRollupFact
	for _, fact := range facts {
		if !fact.Fact.TokenEligible {
			continue
		}
		if isUsageRollupException(fact) {
			exceptions = append(exceptions, fact)
			continue
		}
		timestamp := fact.Fact.RawTimestamp
		if timestamp == "" {
			timestamp = fact.DedupTimestamp
		}
		priced, err := priceUsageFact(usagePriceInput{
			Fact: fact.Fact, Timestamp: timestamp, ReportedModel: fact.Model,
		}, resolver)
		if err != nil {
			return nil, nil, err
		}
		band := -1
		if priced.BandThreshold != nil {
			band = *priced.BandThreshold
		}
		itemKey := key{
			session: fact.AttributionSessionID, date: fact.LocalDate,
			model: fact.Model, priced: priced.PricedModel,
			pattern: priced.MatchedPattern, rateHash: priced.RateHash,
			rateOK: priced.RateOK, band: band,
		}
		row := rows[itemKey]
		if row == nil {
			row = &usageDailyContribution{
				AttributedSessionID: fact.AttributionSessionID,
				LocalDate:           fact.LocalDate, ReportedModel: fact.Model,
				PricedModel: priced.PricedModel, MatchedPattern: priced.MatchedPattern,
				RateOK: priced.RateOK, RateHash: priced.RateHash,
				BandThreshold: priced.BandThreshold,
			}
			rows[itemKey] = row
		}
		if err := addUsageFactToDailyContribution(row, fact, priced); err != nil {
			return nil, nil, err
		}
	}
	result := make([]usageDailyContribution, 0, len(rows))
	for _, row := range rows {
		result = append(result, *row)
	}
	slices.SortFunc(result, compareUsageDailyContribution)
	slices.SortFunc(exceptions, compareUsageRollupFactIdentity)
	return result, exceptions, nil
}

func addUsageFactToDailyContribution(
	row *usageDailyContribution, fact usageRollupFact, priced usagePriceResult,
) error {
	fields := []struct {
		target *int64
		value  int64
		name   string
	}{
		{&row.InputTokens, fact.Fact.InputTokens, "input tokens"},
		{&row.OutputTokens, fact.Fact.OutputTokens, "output tokens"},
		{&row.ReasoningTokens, fact.Fact.ReasoningTokens, "reasoning tokens"},
		{&row.CacheCreationTokens, fact.Fact.CacheCreationTokens, "cache creation tokens"},
		{&row.CacheReadTokens, fact.Fact.CacheReadTokens, "cache read tokens"},
		{&row.WebSearchRequests, fact.Fact.WebSearchRequests, "web search requests"},
		{&row.CostMicrodollars, priced.Cost.Microdollars, "cost"},
		{&row.SavingsMicrodollars, priced.Savings.Microdollars, "savings"},
	}
	for _, field := range fields {
		value, err := addUsageInt64(*field.target, field.value)
		if err != nil {
			return fmt.Errorf("summing usage %s: %w", field.name, err)
		}
		*field.target = value
	}
	if priced.AuthoritativeCost != nil &&
		(row.AuthoritativeCostMicrodollars == nil ||
			priced.AuthoritativeCost.Microdollars > *row.AuthoritativeCostMicrodollars) {
		value := priced.AuthoritativeCost.Microdollars
		row.AuthoritativeCostMicrodollars = &value
	}
	row.ComputedRequestCount += priced.ComputedRequest
	row.ComputedAggregateCount += priced.ComputedAggregate
	row.ReportedCount += priced.Reported
	row.BaseRequestCount += priced.BaseRequest
	return nil
}

func loadUsageRollupFacts(
	ctx context.Context, conn *sql.Conn, sessions map[string]usageQuerySession,
) ([]usageRollupFact, error) {
	if _, err := conn.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS
		usage_rollup_build_sessions(session_id TEXT PRIMARY KEY) WITHOUT ROWID;
		DELETE FROM usage_rollup_build_sessions`); err != nil {
		return nil, fmt.Errorf("preparing rollup build sessions: %w", err)
	}
	ids := make([]string, 0, len(sessions))
	for id := range sessions {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for start := 0; start < len(ids); start += usageFillInstallBatchSize {
		end := min(start+usageFillInstallBatchSize, len(ids))
		query := `INSERT INTO usage_rollup_build_sessions(session_id) VALUES ` +
			strings.TrimSuffix(strings.Repeat("(?),", end-start), ",")
		args := make([]any, 0, end-start)
		for _, id := range ids[start:end] {
			args = append(args, id)
		}
		if _, err := conn.ExecContext(ctx, query, args...); err != nil {
			return nil, err
		}
	}
	rows, err := conn.QueryContext(ctx, `SELECT f.cached_session_id, f.fact_index,
		cs.session_id, f.source, f.message_ordinal, f.timestamp_ms, f.timestamp_ns,
		f.raw_timestamp, f.uses_session_start, f.model, f.input_tokens,
		f.output_tokens, f.reasoning_tokens, f.cache_creation_tokens,
		f.cache_read_tokens, f.web_search_requests, f.reported_cost_microdollars,
		f.cost_source, f.request_scoped, f.claude_message_id, f.claude_request_id,
		f.source_uuid, f.usage_dedup_key, f.token_eligible, f.activity_eligible
		FROM usage_rollup_build_sessions selected
		JOIN usage_cached_sessions cs ON cs.session_id = selected.session_id
		JOIN usage_facts f ON f.cached_session_id = cs.id
		ORDER BY cs.session_id, f.fact_index`)
	if err != nil {
		return nil, fmt.Errorf("loading rollup facts: %w", err)
	}
	defer rows.Close()
	var facts []usageRollupFact
	for rows.Next() {
		var item usageRollupFact
		var ordinal, millis, nanos, reported sql.NullInt64
		var usesStart, requestScoped, tokenEligible, activityEligible int
		if err := rows.Scan(
			&item.CachedSessionID, &item.FactIndex, &item.SourceSessionID,
			&item.Fact.Source, &ordinal, &millis, &nanos, &item.Fact.RawTimestamp,
			&usesStart, &item.Fact.Model, &item.Fact.InputTokens,
			&item.Fact.OutputTokens, &item.Fact.ReasoningTokens,
			&item.Fact.CacheCreationTokens, &item.Fact.CacheReadTokens,
			&item.Fact.WebSearchRequests, &reported, &item.Fact.CostSource,
			&requestScoped, &item.Fact.ClaudeMessageID, &item.Fact.ClaudeRequestID,
			&item.Fact.SourceUUID, &item.Fact.UsageDedupKey,
			&tokenEligible, &activityEligible); err != nil {
			return nil, err
		}
		if ordinal.Valid {
			value := int(ordinal.Int64)
			item.Fact.MessageOrdinal = &value
		}
		if millis.Valid {
			value := millis.Int64
			item.Fact.TimestampMillis = &value
		}
		if nanos.Valid {
			value := nanos.Int64
			item.Fact.TimestampNanos = &value
		}
		if reported.Valid {
			value := reported.Int64
			item.Fact.ReportedCostMicrodollars = &value
		}
		item.Fact.UsesSessionStart = usesStart != 0
		item.Fact.RequestScoped = requestScoped != 0
		item.Fact.TokenEligible = tokenEligible != 0
		item.Fact.ActivityEligible = activityEligible != 0
		session := sessions[item.SourceSessionID]
		item.AttributionSessionID = item.SourceSessionID
		item.Agent, item.Model = session.Agent, item.Fact.Model
		item.EffectiveMillis, item.EffectiveNanos = item.Fact.TimestampMillis, item.Fact.TimestampNanos
		item.DedupTimestamp = item.Fact.RawTimestamp
		if item.Fact.UsesSessionStart {
			item.EffectiveMillis, item.EffectiveNanos = session.StartedAtMillis, session.StartedAtNanos
			item.DedupTimestamp = session.StartedAt
		}
		facts = append(facts, item)
	}
	return facts, rows.Err()
}

func loadCursorUsageRollupBuild(
	ctx context.Context, conn *sql.Conn, highWater int64,
	location *time.Location, pricingHash string,
) (usageRollupBuild, error) {
	build := usageRollupBuild{
		SessionID: usageRollupCursorSessionID, Agent: "cursor",
		PricingHash: pricingHash,
		Source: usageSourceVersion{
			SessionID:  usageRollupCursorSessionID,
			SyncMarker: strconv.FormatInt(highWater, 10),
		},
		FactRevision: highWater,
	}
	rows, err := conn.QueryContext(ctx, `SELECT source_id, timestamp_ms,
		timestamp_ns, raw_timestamp, model, input_tokens, output_tokens,
		cache_creation_tokens, cache_read_tokens, charged_microdollars,
		dedup_key FROM cursor_usage_facts WHERE source_id <= ? ORDER BY source_id`,
		highWater)
	if err != nil {
		return build, err
	}
	defer rows.Close()
	var facts []usageRollupFact
	for rows.Next() {
		var fact usageRollupFact
		var sourceID int64
		var millis, nanos sql.NullInt64
		var charged int64
		if err := rows.Scan(&sourceID, &millis, &nanos, &fact.Fact.RawTimestamp,
			&fact.Model, &fact.Fact.InputTokens, &fact.Fact.OutputTokens,
			&fact.Fact.CacheCreationTokens, &fact.Fact.CacheReadTokens, &charged,
			&fact.Fact.UsageDedupKey); err != nil {
			return build, err
		}
		fact.FactIndex = int(sourceID)
		fact.Fact.Source = "cursor"
		fact.Fact.Model = fact.Model
		fact.Fact.CostSource = "cursor-reported"
		fact.Fact.RequestScoped = true
		fact.Fact.TokenEligible = true
		fact.Fact.ReportedCostMicrodollars = &charged
		if millis.Valid {
			value := millis.Int64
			fact.Fact.TimestampMillis, fact.EffectiveMillis = &value, &value
		}
		if nanos.Valid {
			value := nanos.Int64
			fact.Fact.TimestampNanos, fact.EffectiveNanos = &value, &value
		}
		fact.DedupTimestamp = fact.Fact.RawTimestamp
		fact.LocalDate = usageRollupLocalDate(fact, location)
		facts = append(facts, fact)
	}
	if err := rows.Err(); err != nil {
		return build, err
	}
	daily, exceptions, err := buildUsageDailyContributions(facts, nil)
	if err != nil {
		return build, err
	}
	// Cursor rows always carry a dedup key, so pricing remains deferred until
	// their general-dedup group is resolved with session exceptions.
	if len(daily) != 0 {
		return build, fmt.Errorf("Cursor rollup unexpectedly produced daily rows")
	}
	build.Exceptions = usageRollupExceptionRows(exceptions)
	return build, nil
}

func buildUsageRollupSessions(
	facts []usageRollupFact, sessions map[string]usageQuerySession,
	versions map[string]usageSourceVersion, fills map[string]usageFillResult,
	location *time.Location, resolver *export.PricingResolver, pricingHash string,
) ([]usageRollupBuild, error) {
	if location == nil {
		location = time.Local
	}
	for index := range facts {
		facts[index].LocalDate = usageRollupLocalDate(facts[index], location)
	}
	ids := make([]string, 0, len(sessions))
	for id := range sessions {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	builds := make([]usageRollupBuild, 0, len(ids))
	factIndex := 0
	for _, id := range ids {
		start := factIndex
		for factIndex < len(facts) && facts[factIndex].SourceSessionID == id {
			factIndex++
		}
		sessionFacts := facts[start:factIndex]
		daily, exceptions, err := buildUsageDailyContributions(sessionFacts, resolver)
		if err != nil {
			return nil, err
		}
		builds = append(builds, usageRollupBuild{
			SessionID: id, Agent: sessions[id].Agent, StartedAt: sessions[id].StartedAt,
			PricingHash: pricingHash,
			Source:      versions[id], FactRevision: fills[id].InstallRevision,
			Daily: daily, Activity: usageRollupActivityContributions(id, sessionFacts),
			Exceptions: usageRollupExceptionRows(exceptions),
		})
	}
	return builds, nil
}

func usageRollupSnapshotKey(fact usageRollupFact) string {
	if fact.Fact.ClaudeMessageID == "" || fact.Fact.ClaudeRequestID == "" {
		return ""
	}
	return fmt.Sprintf("%d:%s%s", len(fact.Fact.ClaudeMessageID),
		fact.Fact.ClaudeMessageID, fact.Fact.ClaudeRequestID)
}

func usageRollupGeneralKey(fact usageRollupFact) string {
	if fact.Fact.Source == "message" && fact.Agent != "" &&
		fact.Fact.SourceUUID != "" && usageRollupSnapshotKey(fact) == "" {
		return fmt.Sprintf("source:%d:%s%s", len(fact.Agent), fact.Agent, fact.Fact.SourceUUID)
	}
	if fact.Fact.UsageDedupKey != "" {
		return "usage:" + fact.Fact.UsageDedupKey
	}
	return ""
}

func usageRollupExceptionRows(facts []usageRollupFact) []usageExceptionRow {
	var rows []usageExceptionRow
	for _, fact := range facts {
		for _, group := range []struct{ kind, key string }{
			{"snapshot", usageRollupSnapshotKey(fact)},
			{"general", usageRollupGeneralKey(fact)},
		} {
			if group.key != "" {
				rows = append(rows, usageExceptionRow{
					GroupKind: group.kind, GroupKey: group.key, Fact: fact})
			}
		}
	}
	return rows
}

func usageRollupActivityContributions(
	sessionID string, facts []usageRollupFact,
) []usageActivityContribution {
	type key struct{ date, model string }
	counts := make(map[key]int)
	for _, fact := range facts {
		if fact.Fact.ActivityEligible && fact.LocalDate != "" {
			counts[key{fact.LocalDate, fact.Fact.Model}]++
		}
	}
	keys := make([]key, 0, len(counts))
	for item := range counts {
		keys = append(keys, item)
	}
	slices.SortFunc(keys, func(left, right key) int {
		if order := cmp.Compare(left.date, right.date); order != 0 {
			return order
		}
		return cmp.Compare(left.model, right.model)
	})
	result := make([]usageActivityContribution, 0, len(keys))
	for _, item := range keys {
		result = append(result, usageActivityContribution{
			AttributedSessionID: sessionID,
			LocalDate:           item.date, Model: item.model,
			UserMessageCount: counts[item]})
	}
	return result
}

func usageRollupLocalDate(fact usageRollupFact, location *time.Location) string {
	if millis := fact.effectiveMillis(); millis != nil {
		return time.UnixMilli(*millis).In(location).Format(time.DateOnly)
	}
	raw := fact.DedupTimestamp
	if raw == "" {
		raw = fact.Fact.RawTimestamp
	}
	if len(raw) >= len(time.DateOnly) {
		return raw[:len(time.DateOnly)]
	}
	return ""
}

func (fact usageRollupFact) effectiveMillis() *int64 {
	if fact.EffectiveMillis != nil {
		return fact.EffectiveMillis
	}
	return fact.Fact.TimestampMillis
}

func compareUsageRollupFactIdentity(left, right usageRollupFact) int {
	if order := cmp.Compare(left.CachedSessionID, right.CachedSessionID); order != 0 {
		return order
	}
	return cmp.Compare(left.FactIndex, right.FactIndex)
}

func compareUsageDailyContribution(left, right usageDailyContribution) int {
	for _, values := range [][2]string{
		{left.AttributedSessionID, right.AttributedSessionID},
		{left.LocalDate, right.LocalDate}, {left.ReportedModel, right.ReportedModel},
		{left.RateHash, right.RateHash},
	} {
		if order := cmp.Compare(values[0], values[1]); order != 0 {
			return order
		}
	}
	return 0
}

func usageRollupFactIdentity(fact usageRollupFact) string {
	return fmt.Sprintf("%d:%d", fact.CachedSessionID, fact.FactIndex)
}

func addUsageInt64(left, right int64) (int64, error) {
	if (right > 0 && left > math.MaxInt64-right) ||
		(right < 0 && left < math.MinInt64-right) {
		return 0, money.ErrOverflow
	}
	return left + right, nil
}
