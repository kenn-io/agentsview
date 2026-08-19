package db

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/export"
)

type usageFactsGroup struct {
	SessionID, Date, Project, Agent, Machine     string
	Model, PricedModel, MatchedPattern           string
	RateOK                                       bool
	InputTokens, OutputTokens, ReasoningTokens   int64
	CacheCreationTokens, CacheReadTokens         int64
	CostMicrodollars, SavingsMicrodollars        int64
	AuthoritativeCostMicrodollars                *int64
	ComputedRequestCount, ComputedAggregateCount int
	ReportedCount, BaseRequestCount              int
	BandThreshold                                *int
	DiscardedSnapshotOutputTokens                int64
}

type usageFactsResult struct {
	Groups           []usageFactsGroup
	MatchingSessions map[string]UsageSessionInfo
}

type usageCacheMissingSessionError struct{ SessionID string }

func (err *usageCacheMissingSessionError) Error() string {
	return "required usage cache session is missing: " + err.SessionID
}

func usageCacheReadShouldRecapture(err error) bool {
	if errors.Is(err, errUsageCacheSourceChanged) {
		return true
	}
	var missing *usageCacheMissingSessionError
	return errors.As(err, &missing)
}

func usageCursorIncluded(filter UsageFilter) bool {
	_, _, ok := cursorUsageRowsSQLForBounds(
		filter, usageBoundsForFilter(filter))
	return ok
}

// usageRollupQuery reads compact daily rows and resolves only facts whose
// deduplication can depend on the requested window or live session filters.
func (cache *usageCache) usageRollupQuery(
	ctx context.Context, snapshot usageQuerySnapshot, filter UsageFilter,
	installs map[string]usageRollupInstall, resolver *export.PricingResolver,
) (usageFactsResult, error) {
	if len(snapshot.Sessions) == 0 && snapshot.CursorHighWater == 0 {
		return usageFactsResult{
			MatchingSessions: make(map[string]UsageSessionInfo),
		}, nil
	}
	conn, err := cache.db.Conn(ctx)
	if err != nil {
		return usageFactsResult{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN`); err != nil {
		return usageFactsResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	identity := usageTimezoneIdentityFor(snapshot.location, snapshot.Intervals)
	timezoneID, err := verifyUsageRollupQuery(
		ctx, conn, identity.Key, snapshot, installs)
	if err != nil {
		return usageFactsResult{}, err
	}
	result, err := readUsageDailyRollups(
		ctx, conn, timezoneID, snapshot, filter)
	if err != nil {
		return usageFactsResult{}, err
	}
	exceptions, err := readUsageRollupExceptions(
		ctx, conn, timezoneID, snapshot, filter)
	if err != nil {
		return usageFactsResult{}, err
	}
	exceptionGroups, err := aggregateUsageRollupExceptions(
		exceptions, snapshot, filter, resolver)
	if err != nil {
		return usageFactsResult{}, err
	}
	result.Groups = append(result.Groups, exceptionGroups...)
	slices.SortFunc(result.Groups, compareUsageFactsGroup)
	for _, group := range exceptionGroups {
		if group.SessionID != "" {
			result.MatchingSessions[group.SessionID] = UsageSessionInfo{
				Project: group.Project, Agent: group.Agent,
			}
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return usageFactsResult{}, err
	}
	committed = true
	return result, nil
}

func verifyUsageRollupQuery(
	ctx context.Context, conn *sql.Conn, timezoneKey string,
	snapshot usageQuerySnapshot, installs map[string]usageRollupInstall,
) (int64, error) {
	var timezoneID int64
	if err := conn.QueryRowContext(ctx, `SELECT id FROM usage_rollup_timezones
		WHERE timezone_key = ?`, timezoneKey).Scan(&timezoneID); err != nil {
		return 0, err
	}
	for _, session := range snapshot.Sessions {
		if err := verifyUsageRollupInstall(
			ctx, conn, timezoneID, session.ID, installs,
		); err != nil {
			return 0, err
		}
	}
	if snapshot.CursorHighWater > 0 {
		if err := verifyUsageRollupInstall(
			ctx, conn, timezoneID, usageRollupCursorSessionID, installs,
		); err != nil {
			return 0, err
		}
	}
	return timezoneID, nil
}

func verifyUsageRollupInstall(
	ctx context.Context, conn *sql.Conn, timezoneID int64, sessionID string,
	installs map[string]usageRollupInstall,
) error {
	required, ok := installs[sessionID]
	if !ok {
		return &usageCacheMissingSessionError{SessionID: sessionID}
	}
	var revision int64
	var pricingHash string
	if err := conn.QueryRowContext(ctx, `SELECT install_revision, pricing_hash
		FROM usage_rollup_installs WHERE id = ? AND timezone_id = ?`,
		required.ID, timezoneID).Scan(&revision, &pricingHash); err != nil {
		if err == sql.ErrNoRows {
			return &usageCacheMissingSessionError{SessionID: sessionID}
		}
		return err
	}
	if revision != required.InstallRevision || pricingHash != required.PricingHash {
		return fmt.Errorf("%w: usage rollup session %s changed before read",
			errUsageCacheSourceChanged, sessionID)
	}
	return nil
}

func readUsageDailyRollups(
	ctx context.Context, conn *sql.Conn, timezoneID int64,
	snapshot usageQuerySnapshot, filter UsageFilter,
) (usageFactsResult, error) {
	from, to := usageRollupDateBounds(snapshot)
	rows, err := conn.QueryContext(ctx, `SELECT i.session_id, r.local_date,
		r.reported_model, r.priced_model, r.matched_pattern, r.rate_ok,
		r.input_tokens, r.output_tokens, r.reasoning_tokens,
		r.cache_creation_tokens, r.cache_read_tokens,
		r.estimated_cost_microdollars, r.savings_microdollars,
		r.authoritative_cost_microdollars, r.computed_request_count,
		r.computed_aggregate_count, r.reported_count, r.base_request_count,
		r.band_threshold, r.discarded_snapshot_output_tokens
		FROM usage_daily_rollups r
		JOIN usage_rollup_installs i ON i.id = r.rollup_install_id
		WHERE i.timezone_id = ? AND (? = '' OR r.local_date >= ?)
		  AND (? = '' OR r.local_date <= ?)
		ORDER BY r.local_date, i.session_id, r.reported_model, r.band_threshold`,
		timezoneID, from, from, to, to)
	if err != nil {
		return usageFactsResult{}, err
	}
	defer rows.Close()
	sessions := usageRollupSessionMap(snapshot)
	result := usageFactsResult{MatchingSessions: make(map[string]UsageSessionInfo)}
	for rows.Next() {
		var group usageFactsGroup
		var rateOK int
		var authoritative, band sql.NullInt64
		if err := rows.Scan(&group.SessionID, &group.Date, &group.Model,
			&group.PricedModel, &group.MatchedPattern, &rateOK,
			&group.InputTokens, &group.OutputTokens, &group.ReasoningTokens,
			&group.CacheCreationTokens, &group.CacheReadTokens,
			&group.CostMicrodollars, &group.SavingsMicrodollars, &authoritative,
			&group.ComputedRequestCount, &group.ComputedAggregateCount,
			&group.ReportedCount, &group.BaseRequestCount, &band,
			&group.DiscardedSnapshotOutputTokens); err != nil {
			return usageFactsResult{}, err
		}
		session, ok := sessions[group.SessionID]
		if !ok || !session.PassesFilter || !usageRollupModelPasses(filter, group.Model) {
			continue
		}
		group.Project, group.Agent, group.Machine =
			session.Project, session.Agent, session.Machine
		group.RateOK = rateOK != 0
		if authoritative.Valid {
			value := authoritative.Int64
			group.AuthoritativeCostMicrodollars = &value
		}
		if band.Valid && band.Int64 >= 0 {
			value := int(band.Int64)
			group.BandThreshold = &value
		}
		result.Groups = append(result.Groups, group)
		result.MatchingSessions[group.SessionID] = UsageSessionInfo{
			Project: group.Project, Agent: group.Agent,
		}
	}
	return result, rows.Err()
}

func readUsageRollupExceptions(
	ctx context.Context, conn *sql.Conn, timezoneID int64,
	snapshot usageQuerySnapshot, filter UsageFilter,
) ([]usageRollupFact, error) {
	from, to := usageRollupDateBounds(snapshot)
	rows, err := conn.QueryContext(ctx, `SELECT e.cached_session_id, e.fact_index,
		e.source_session_id, e.local_date, e.source, e.message_ordinal,
		e.timestamp_ms, e.timestamp_ns, e.raw_timestamp, e.uses_session_start,
		e.model, e.input_tokens, e.output_tokens, e.reasoning_tokens,
		e.cache_creation_tokens, e.cache_read_tokens, e.web_search_requests,
		e.reported_cost_microdollars, e.cost_source, e.request_scoped,
		e.is_headless, e.claude_message_id, e.claude_request_id, e.source_uuid,
		e.usage_dedup_key, COUNT(*) OVER ()
		FROM usage_rollup_exceptions e
		JOIN usage_rollup_installs i ON i.id = e.rollup_install_id
		WHERE i.timezone_id = ? AND (? = '' OR e.local_date >= ?)
		  AND (? = '' OR e.local_date <= ?)
		  AND (i.session_id != ? OR e.fact_index <= ?)
		GROUP BY e.rollup_install_id, e.cached_session_id, e.fact_index
		ORDER BY e.cached_session_id, e.fact_index`,
		timezoneID, from, from, to, to, usageRollupCursorSessionID,
		snapshot.CursorHighWater)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sessions := usageRollupSessionMap(snapshot)
	var result []usageRollupFact
	for rows.Next() {
		var fact usageRollupFact
		var ordinal, millis, nanos, reported sql.NullInt64
		var usesStart, requestScoped, isHeadless, resultCount int
		if err := rows.Scan(&fact.CachedSessionID, &fact.FactIndex,
			&fact.SourceSessionID, &fact.LocalDate, &fact.Fact.Source, &ordinal,
			&millis, &nanos, &fact.Fact.RawTimestamp, &usesStart, &fact.Model,
			&fact.Fact.InputTokens, &fact.Fact.OutputTokens,
			&fact.Fact.ReasoningTokens, &fact.Fact.CacheCreationTokens,
			&fact.Fact.CacheReadTokens, &fact.Fact.WebSearchRequests, &reported,
			&fact.Fact.CostSource, &requestScoped, &isHeadless,
			&fact.Fact.ClaudeMessageID,
			&fact.Fact.ClaudeRequestID, &fact.Fact.SourceUUID,
			&fact.Fact.UsageDedupKey, &resultCount); err != nil {
			return nil, err
		}
		if result == nil && resultCount > 0 {
			result = make([]usageRollupFact, 0, resultCount)
		}
		if _, ok := sessions[fact.SourceSessionID]; !ok &&
			fact.SourceSessionID != "" {
			continue
		}
		if ordinal.Valid {
			value := int(ordinal.Int64)
			fact.Fact.MessageOrdinal = &value
		}
		if millis.Valid {
			value := millis.Int64
			fact.Fact.TimestampMillis = &value
			fact.EffectiveMillis = &value
		}
		if nanos.Valid {
			value := nanos.Int64
			fact.Fact.TimestampNanos = &value
			fact.EffectiveNanos = &value
		}
		if reported.Valid {
			value := reported.Int64
			fact.Fact.ReportedCostMicrodollars = &value
		}
		fact.Fact.UsesSessionStart = usesStart != 0
		fact.Fact.RequestScoped = requestScoped != 0
		fact.IsHeadless = isHeadless != 0
		fact.Fact.TokenEligible = true
		fact.DedupTimestamp = fact.Fact.RawTimestamp
		session := sessions[fact.SourceSessionID]
		fact.Agent = session.Agent
		fact.AttributionSessionID = fact.SourceSessionID
		if fact.Fact.UsesSessionStart {
			fact.EffectiveMillis, fact.EffectiveNanos =
				session.StartedAtMillis, session.StartedAtNanos
			fact.DedupTimestamp = session.StartedAt
		}
		result = append(result, fact)
	}
	return result, rows.Err()
}

func (cache *usageCache) usageRollupActivityQuery(
	ctx context.Context, snapshot usageQuerySnapshot, filter UsageFilter,
	installs map[string]usageRollupInstall,
) (map[string]UsageSessionInfo, error) {
	if len(snapshot.Sessions) == 0 {
		return make(map[string]UsageSessionInfo), nil
	}
	conn, err := cache.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN`); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	identity := usageTimezoneIdentityFor(snapshot.location, snapshot.Intervals)
	timezoneID, err := verifyUsageRollupQuery(
		ctx, conn, identity.Key, snapshot, installs)
	if err != nil {
		return nil, err
	}
	from, to := usageRollupDateBounds(snapshot)
	rows, err := conn.QueryContext(ctx, `SELECT i.session_id, a.model
		FROM usage_activity_rollups a
		JOIN usage_rollup_installs i ON i.id = a.rollup_install_id
		WHERE i.timezone_id = ? AND (? = '' OR a.local_date >= ?)
		  AND (? = '' OR a.local_date <= ?)
		GROUP BY i.session_id, a.model`, timezoneID, from, from, to, to)
	if err != nil {
		return nil, err
	}
	sessions := usageRollupSessionMap(snapshot)
	result := make(map[string]UsageSessionInfo)
	for rows.Next() {
		var sessionID, model string
		if err := rows.Scan(&sessionID, &model); err != nil {
			_ = rows.Close()
			return nil, err
		}
		session, ok := sessions[sessionID]
		if ok && session.PassesFilter && usageRollupModelPasses(filter, model) {
			result[sessionID] = UsageSessionInfo{
				Project: session.Project, Agent: session.Agent,
			}
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return nil, err
	}
	committed = true
	return result, nil
}

func aggregateUsageRollupExceptions(
	facts []usageRollupFact, snapshot usageQuerySnapshot, filter UsageFilter,
	resolver *export.PricingResolver,
) ([]usageFactsGroup, error) {
	sessions := usageRollupSessionMap(snapshot)
	plain, general, discarded := rankUsageRollupSnapshots(facts)
	plain = filterUsageRollupOwners(plain, sessions, filter)
	general = filterUsageRollupOwners(general, sessions, filter)
	general = deduplicateUsageRollupGeneral(general)
	survivors := append(plain, general...)
	type key struct {
		session, date, model, priced, pattern, rateHash string
		rateOK                                          bool
		band                                            int
	}
	groups := make(map[key]*usageFactsGroup)
	type authoritativeCandidate struct {
		fact  usageRollupFact
		cost  int64
		group *usageFactsGroup
	}
	authoritative := make(map[string]authoritativeCandidate)
	for _, fact := range survivors {
		priced, err := priceUsageFact(usagePriceInput{
			Fact: fact.Fact, Timestamp: fact.DedupTimestamp,
			ReportedModel: fact.Model,
		}, resolver)
		if err != nil {
			return nil, err
		}
		band := -1
		if priced.BandThreshold != nil {
			band = *priced.BandThreshold
		}
		itemKey := key{fact.AttributionSessionID, fact.LocalDate, fact.Model,
			priced.PricedModel, priced.MatchedPattern, priced.RateHash,
			priced.RateOK, band}
		group := groups[itemKey]
		if group == nil {
			session := sessions[fact.AttributionSessionID]
			if fact.AttributionSessionID == "" {
				session.Agent = "cursor"
			}
			group = &usageFactsGroup{
				SessionID: fact.AttributionSessionID, Date: fact.LocalDate,
				Project: session.Project, Agent: session.Agent, Machine: session.Machine,
				Model: fact.Model, PricedModel: priced.PricedModel,
				MatchedPattern: priced.MatchedPattern, RateOK: priced.RateOK,
				BandThreshold:                 priced.BandThreshold,
				DiscardedSnapshotOutputTokens: discarded,
			}
			groups[itemKey] = group
		}
		if err := addUsageFactToGroup(group, fact, priced); err != nil {
			return nil, err
		}
		if priced.AuthoritativeCost != nil {
			candidate, exists := authoritative[fact.AttributionSessionID]
			if !exists || compareUsageAuthoritativeOrder(candidate.fact, fact) < 0 {
				authoritative[fact.AttributionSessionID] = authoritativeCandidate{
					fact: fact, cost: priced.AuthoritativeCost.Microdollars, group: group,
				}
			}
		}
	}
	for _, group := range groups {
		group.AuthoritativeCostMicrodollars = nil
	}
	for _, candidate := range authoritative {
		value := candidate.cost
		candidate.group.AuthoritativeCostMicrodollars = &value
	}
	result := make([]usageFactsGroup, 0, len(groups))
	for _, group := range groups {
		result = append(result, *group)
	}
	return result, nil
}

func compareUsageAuthoritativeOrder(left, right usageRollupFact) int {
	if order := cmp.Compare(left.DedupTimestamp, right.DedupTimestamp); order != 0 {
		return order
	}
	if order := cmp.Compare(left.SourceSessionID, right.SourceSessionID); order != 0 {
		return order
	}
	leftOrdinal, rightOrdinal := -1, -1
	if left.Fact.MessageOrdinal != nil {
		leftOrdinal = *left.Fact.MessageOrdinal
	}
	if right.Fact.MessageOrdinal != nil {
		rightOrdinal = *right.Fact.MessageOrdinal
	}
	if order := cmp.Compare(leftOrdinal, rightOrdinal); order != 0 {
		return order
	}
	return compareUsageRollupFactIdentity(left, right)
}

func rankUsageRollupSnapshots(
	facts []usageRollupFact,
) (plain, general []usageRollupFact, discarded int64) {
	type survivorRef struct {
		index   int
		general bool
	}
	type snapshotGroup struct {
		first int
		rest  []int
	}
	// Keep only indexes in the grouping map. usageRollupFact is intentionally
	// wide enough to resolve every dedup edge; copying it into each group made
	// warm aggregate reads allocate another full exception set.
	bySnapshot := make(map[string]snapshotGroup)
	survivors := make([]survivorRef, 0, len(facts))
	for index := range facts {
		key := usageRollupSnapshotKey(facts[index])
		if key == "" {
			survivors = append(survivors, survivorRef{index: index, general: true})
			continue
		}
		group, exists := bySnapshot[key]
		if !exists {
			bySnapshot[key] = snapshotGroup{first: index}
			continue
		}
		group.rest = append(group.rest, index)
		bySnapshot[key] = group
	}
	keys := usageSortedMapKeys(bySnapshot)
	for _, key := range keys {
		group := bySnapshot[key]
		if len(group.rest) == 0 {
			member := facts[group.first]
			survivors = append(survivors, survivorRef{
				index: group.first, general: usageRollupGeneralKey(member) != "",
			})
			continue
		}
		attribution := facts[group.first]
		winnerIndex := group.first
		winner := attribution
		for _, index := range group.rest {
			member := facts[index]
			if compareUsageSnapshotAttribution(member, attribution) < 0 {
				attribution = member
			}
			if compareUsageSnapshotWinner(member, winner) > 0 {
				winnerIndex = index
				winner = member
			}
		}
		winner.AttributionSessionID = attribution.SourceSessionID
		first := facts[group.first]
		winner.Fact.WebSearchRequests = max(
			winner.Fact.WebSearchRequests, first.Fact.WebSearchRequests)
		if usageRollupFactIdentity(first) != usageRollupFactIdentity(winner) {
			discarded += first.Fact.OutputTokens
		}
		for _, index := range group.rest {
			member := facts[index]
			winner.Fact.WebSearchRequests = max(
				winner.Fact.WebSearchRequests, member.Fact.WebSearchRequests)
			if usageRollupFactIdentity(member) != usageRollupFactIdentity(winner) {
				discarded += member.Fact.OutputTokens
			}
		}
		facts[winnerIndex] = winner
		survivors = append(survivors, survivorRef{
			index: winnerIndex, general: winner.Fact.UsageDedupKey != "",
		})
	}

	// Compact survivors into the input backing array in source order. The nth
	// survivor's source index is always at least n, so no unread survivor is
	// overwritten. Partitioning that compacted prefix then gives both result
	// slices without allocating another wide-fact backing array.
	slices.SortFunc(survivors, func(a, b survivorRef) int {
		return cmp.Compare(a.index, b.index)
	})
	classes := make([]bool, len(survivors))
	plainCount := 0
	for output, survivor := range survivors {
		facts[output] = facts[survivor.index]
		classes[output] = survivor.general
		if !survivor.general {
			plainCount++
		}
	}
	left, right := 0, len(survivors)-1
	for left < plainCount {
		if !classes[left] {
			left++
			continue
		}
		for classes[right] {
			right--
		}
		facts[left], facts[right] = facts[right], facts[left]
		classes[left], classes[right] = classes[right], classes[left]
		left++
		right--
	}
	return facts[:plainCount], facts[plainCount:len(survivors)], discarded
}

func filterUsageRollupOwners(
	facts []usageRollupFact, sessions map[string]usageQuerySession,
	filter UsageFilter,
) []usageRollupFact {
	result := facts[:0]
	for _, fact := range facts {
		if fact.AttributionSessionID == "" {
			if usageCursorIncluded(filter) &&
				usageCursorAutomatedScopePasses(filter, fact.IsHeadless) &&
				usageRollupModelPasses(filter, fact.Model) {
				fact.Agent = "cursor"
				result = append(result, fact)
			}
			continue
		}
		owner, ok := sessions[fact.AttributionSessionID]
		if ok && owner.PassesFilter && usageRollupModelPasses(filter, fact.Model) {
			fact.Agent = owner.Agent
			result = append(result, fact)
		}
	}
	return result
}

func usageCursorAutomatedScopePasses(filter UsageFilter, isHeadless bool) bool {
	switch normalizeAutomatedScope(filter.AutomatedScope, filter.ExcludeAutomated) {
	case "human":
		return !isHeadless
	case "automated":
		return isHeadless
	default:
		return true
	}
}

func deduplicateUsageRollupGeneral(facts []usageRollupFact) []usageRollupFact {
	byKey := make(map[string][]usageRollupFact)
	for _, fact := range facts {
		byKey[usageRollupGeneralKey(fact)] = append(
			byKey[usageRollupGeneralKey(fact)], fact)
	}
	keys := usageSortedMapKeys(byKey)
	result := make([]usageRollupFact, 0, len(keys))
	for _, key := range keys {
		result = append(result, slices.MinFunc(byKey[key], compareUsageGeneralWinner))
	}
	return result
}

func addUsageFactToGroup(
	group *usageFactsGroup, fact usageRollupFact, priced usagePriceResult,
) error {
	row := usageDailyContribution{}
	if err := addUsageFactToDailyContribution(&row, fact, priced); err != nil {
		return err
	}
	fields := []struct {
		target *int64
		value  int64
	}{
		{&group.InputTokens, row.InputTokens},
		{&group.OutputTokens, row.OutputTokens},
		{&group.ReasoningTokens, row.ReasoningTokens},
		{&group.CacheCreationTokens, row.CacheCreationTokens},
		{&group.CacheReadTokens, row.CacheReadTokens},
		{&group.CostMicrodollars, row.CostMicrodollars},
		{&group.SavingsMicrodollars, row.SavingsMicrodollars},
	}
	for _, field := range fields {
		value, err := addUsageInt64(*field.target, field.value)
		if err != nil {
			return err
		}
		*field.target = value
	}
	if row.AuthoritativeCostMicrodollars != nil &&
		(group.AuthoritativeCostMicrodollars == nil ||
			*row.AuthoritativeCostMicrodollars > *group.AuthoritativeCostMicrodollars) {
		value := *row.AuthoritativeCostMicrodollars
		group.AuthoritativeCostMicrodollars = &value
	}
	group.ComputedRequestCount += row.ComputedRequestCount
	group.ComputedAggregateCount += row.ComputedAggregateCount
	group.ReportedCount += row.ReportedCount
	group.BaseRequestCount += row.BaseRequestCount
	return nil
}

func compareUsageSnapshotAttribution(left, right usageRollupFact) int {
	if order := compareNullableInt64(left.effectiveMillis(), right.effectiveMillis(), true); order != 0 {
		return order
	}
	if order := compareNullableInt64(left.EffectiveNanos, right.EffectiveNanos, true); order != 0 {
		return order
	}
	return compareUsageFactTies(left, right)
}

func compareUsageSnapshotWinner(left, right usageRollupFact) int {
	if order := cmp.Compare(left.Fact.OutputTokens, right.Fact.OutputTokens); order != 0 {
		return order
	}
	if order := compareNullableInt64(left.effectiveMillis(), right.effectiveMillis(), false); order != 0 {
		return order
	}
	if order := compareNullableInt64(left.EffectiveNanos, right.EffectiveNanos, false); order != 0 {
		return order
	}
	return compareUsageFactTies(left, right)
}

func compareUsageGeneralWinner(left, right usageRollupFact) int {
	if order := cmp.Compare(left.DedupTimestamp, right.DedupTimestamp); order != 0 {
		return order
	}
	return compareUsageFactTies(left, right)
}

func compareUsageFactTies(left, right usageRollupFact) int {
	values := [][2]string{{left.SourceSessionID, right.SourceSessionID},
		{left.Fact.Source, right.Fact.Source}, {left.Fact.SourceUUID, right.Fact.SourceUUID},
		{left.Fact.UsageDedupKey, right.Fact.UsageDedupKey}}
	leftOrdinal, rightOrdinal := -1, -1
	if left.Fact.MessageOrdinal != nil {
		leftOrdinal = *left.Fact.MessageOrdinal
	}
	if right.Fact.MessageOrdinal != nil {
		rightOrdinal = *right.Fact.MessageOrdinal
	}
	orders := []int{cmp.Compare(values[0][0], values[0][1]),
		cmp.Compare(leftOrdinal, rightOrdinal)}
	for _, pair := range values[1:] {
		orders = append(orders, cmp.Compare(pair[0], pair[1]))
	}
	orders = append(orders, cmp.Compare(left.FactIndex, right.FactIndex))
	for _, order := range orders {
		if order != 0 {
			return order
		}
	}
	return 0
}

func compareNullableInt64(left, right *int64, nilHigh bool) int {
	if left == nil || right == nil {
		if left == right {
			return 0
		}
		if left == nil {
			if nilHigh {
				return 1
			}
			return -1
		}
		if nilHigh {
			return -1
		}
		return 1
	}
	return cmp.Compare(*left, *right)
}

func compareUsageFactsGroup(left, right usageFactsGroup) int {
	for _, pair := range [][2]string{{left.Date, right.Date},
		{left.SessionID, right.SessionID}, {left.Model, right.Model}} {
		if order := cmp.Compare(pair[0], pair[1]); order != 0 {
			return order
		}
	}
	return 0
}

func usageRollupModelPasses(filter UsageFilter, model string) bool {
	if filter.Model != "" && !slices.Contains(strings.Split(filter.Model, ","), model) {
		return false
	}
	return filter.ExcludeModel == "" ||
		!slices.Contains(strings.Split(filter.ExcludeModel, ","), model)
}

func usageRollupSessionMap(snapshot usageQuerySnapshot) map[string]usageQuerySession {
	result := make(map[string]usageQuerySession, len(snapshot.Sessions))
	for _, session := range snapshot.Sessions {
		result[session.ID] = session
	}
	return result
}

func usageRollupDateBounds(snapshot usageQuerySnapshot) (string, string) {
	if len(snapshot.Intervals) == 0 {
		return "", ""
	}
	return snapshot.Intervals[0].FromLocalDate,
		snapshot.Intervals[len(snapshot.Intervals)-1].ToLocalDate
}

func usageSortedMapKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
