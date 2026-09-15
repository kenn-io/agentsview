package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

// GetSessionUsageRows returns the backend-priced usage rows for the supplied
// sessions, with the same cross-session deduplication as activity reports.
type pgSessionUsageOrderedRow struct {
	scan    pgUsageScanRow
	tsText  string
	ordinal int64
}

func (s *Store) GetSessionUsageRows(
	ctx context.Context, ids []string,
) (*activity.SessionUsageRows, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	pricing, err := s.LoadPricingMap(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading pg pricing: %w", err)
	}
	rateResolver := export.NewPricingResolver(pricing)
	sessionOrder := make(map[string]int, len(ids))
	for i, id := range ids {
		sessionOrder[id] = i
	}
	var rowsAcc []pgSessionUsageOrderedRow
	err = pgQueryChunked(ids, func(chunk []string) error {
		pb := &paramBuilder{}
		ph := pgInPlaceholders(chunk, pb)
		query := pgUsageRowSelect() + " AND u.session_id IN " + ph
		rows, queryErr := s.pg.QueryContext(ctx, query, pb.args...)
		if queryErr != nil {
			return fmt.Errorf("querying pg session usage rows: %w", queryErr)
		}
		defer rows.Close()
		for rows.Next() {
			r, scanErr := scanPGUsageRow(rows)
			if scanErr != nil {
				return fmt.Errorf(
					"scanning pg session usage rows: %w", scanErr)
			}
			ordinal := int64(-1)
			if r.messageOrdinal.Valid {
				ordinal = r.messageOrdinal.Int64
			}
			rowsAcc = append(rowsAcc, pgSessionUsageOrderedRow{
				scan:    r,
				tsText:  startedAtString(r.ts),
				ordinal: ordinal,
			})
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(rowsAcc, func(i, j int) bool {
		return pgSessionUsageRowLess(rowsAcc[i], rowsAcc[j], sessionOrder)
	})
	snapshotRows := make([]activity.UsageRow, len(rowsAcc))
	rowContributes := make([]bool, len(rowsAcc))
	rawOutputTokensBySession := make(map[string]int)
	for i, o := range rowsAcc {
		inputTok, outputTok, cacheCrTok, cacheRdTok, reasoningTok :=
			pgDailyUsageRowTokens(
				pgDailyUsageScanRow{
					messageOrdinal:           o.scan.messageOrdinal,
					usageSource:              o.scan.usageSource,
					tokenJSON:                o.scan.tokenJSON,
					inputTokens:              o.scan.inputTokens,
					outputTokens:             o.scan.outputTokens,
					cacheCreationInputTokens: o.scan.cacheCreationInputTokens,
					cacheReadInputTokens:     o.scan.cacheReadInputTokens,
					reasoningTokens:          o.scan.reasoningTokens,
				},
			)
		snapshotRows[i] = activity.UsageRow{
			SessionID:           o.scan.sessionID,
			Timestamp:           o.tsText,
			MessageOrdinal:      o.ordinal,
			UsageSource:         o.scan.usageSource,
			InputTokens:         inputTok,
			OutputTokens:        outputTok,
			CacheCreationTokens: cacheCrTok,
			CacheReadTokens:     cacheRdTok,
			WebSearchRequests: pgUsageRowWebSearchRequests(
				o.scan.usageSource, o.scan.tokenJSON),
			Agent:           o.scan.agent,
			ProviderID:      o.scan.providerID,
			ClaudeMessageID: o.scan.claudeMessageID,
			ClaudeRequestID: o.scan.claudeRequestID,
			SourceUUID:      o.scan.sourceUUID,
			UsageDedupKey:   o.scan.usageDedupKey,
		}
		rowContributes[i] = activity.UsageDataContributes(
			o.scan.cost.Valid, inputTok, outputTok, reasoningTok,
			cacheCrTok, cacheRdTok,
			pgUsageRowWebSearchRequests(o.scan.usageSource, o.scan.tokenJSON))
		rawOutputTokensBySession[o.scan.sessionID] += outputTok
	}
	canonicalTokenCoverageBySession, err :=
		activity.CanonicalSessionTokenCoverageContext(ctx, snapshotRows)
	if err != nil {
		return nil, err
	}
	snapshotMask, snapshotAttribution, snapshotWebSearchRequests :=
		activity.ClaudeSnapshotSurvivorSelection(snapshotRows)
	seen := make(map[pgUsageDedupToken]struct{})
	deduplicatedOutputTokens := make(map[string]int)
	discardedContributingSessions := make(map[string]struct{})
	out := make([]activity.UsageRow, 0, len(rowsAcc))
	for i, o := range rowsAcc {
		if !snapshotMask[i] {
			deduplicatedOutputTokens[o.scan.sessionID] +=
				snapshotRows[i].OutputTokens
			if rowContributes[i] {
				discardedContributingSessions[o.scan.sessionID] = struct{}{}
			}
			continue
		}
		r := o.scan
		inputTok, outputTok, cacheCrTok, cacheRdTok, _ :=
			pgDailyUsageRowTokens(
				pgDailyUsageScanRow{
					messageOrdinal:           r.messageOrdinal,
					usageSource:              r.usageSource,
					tokenJSON:                r.tokenJSON,
					inputTokens:              r.inputTokens,
					outputTokens:             r.outputTokens,
					cacheCreationInputTokens: r.cacheCreationInputTokens,
					cacheReadInputTokens:     r.cacheReadInputTokens,
					reasoningTokens:          r.reasoningTokens,
				},
			)
		attributionSessionID := snapshotAttribution[i]
		if attributionSessionID != r.sessionID {
			deduplicatedOutputTokens[r.sessionID] += outputTok
			if rowContributes[i] {
				discardedContributingSessions[r.sessionID] = struct{}{}
			}
		}
		if key, ok := pgUsageDedupTokenForRow(
			r.usageSource, r.agent, r.claudeMessageID,
			r.claudeRequestID, r.sourceUUID, r.usageDedupKey,
		); ok {
			if _, dup := seen[key]; dup {
				deduplicatedOutputTokens[r.sessionID] += outputTok
				if rowContributes[i] {
					discardedContributingSessions[r.sessionID] = struct{}{}
				}
				continue
			}
			seen[key] = struct{}{}
		}
		costRow := r
		var sessionCost *money.Money
		if r.costSource == db.CopilotReportedCostSource && r.cost.Valid {
			v := money.Money{Microdollars: r.cost.Int64}
			sessionCost = &v
			costRow.cost = sql.NullInt64{}
			rateResolver.RecordUnattributedReported()
		}
		cost, priced, contributes, priceErr :=
			pgSessionRowCostWithWebSearchRequests(
				costRow, snapshotWebSearchRequests[i], rateResolver)
		if priceErr != nil {
			return nil, priceErr
		}
		costSource := export.CostSourceComputed
		if costRow.cost.Valid {
			costSource = export.CostSourceReported
		}
		out = append(out, activity.UsageRow{
			SessionID:       attributionSessionID,
			SourceSessionID: r.sessionID,
			Model:           r.model,
			Timestamp:       o.tsText,
			OutputTokens:    outputTok,
			Cost:            cost,
			CostSource:      costSource,
			SessionCost:     sessionCost,
			Priced:          priced,
			Contributes:     contributes,
			Agent:           r.agent,
			ProviderID:      r.providerID,
			ClaudeMessageID: r.claudeMessageID,
			ClaudeRequestID: r.claudeRequestID,
			SourceUUID:      r.sourceUUID,
			UsageDedupKey:   r.usageDedupKey,

			UsageSource:         r.usageSource,
			MessageOrdinal:      pgUsageRowMessageOrdinal(r.messageOrdinal),
			InputTokens:         inputTok,
			CacheCreationTokens: cacheCrTok,
			CacheReadTokens:     cacheRdTok,
			WebSearchRequests:   snapshotWebSearchRequests[i],
		})
	}
	return &activity.SessionUsageRows{
		Rows:                            out,
		RawOutputTokensBySession:        rawOutputTokensBySession,
		DeduplicatedOutputTokens:        deduplicatedOutputTokens,
		DiscardedContributingSessions:   discardedContributingSessions,
		CanonicalTokenCoverageBySession: canonicalTokenCoverageBySession,
	}, nil
}

// pgUsageRowMessageOrdinal renders a nullable message ordinal in
// activity.UsageRow's COALESCE(message_ordinal, -1) convention.
func pgUsageRowMessageOrdinal(v sql.NullInt64) int64 {
	if !v.Valid {
		return -1
	}
	return v.Int64
}

func pgSessionUsageRowLess(
	a, b pgSessionUsageOrderedRow,
	sessionOrder map[string]int,
) bool {
	if a.scan.ts.Valid && b.scan.ts.Valid {
		if !a.scan.ts.Time.Equal(b.scan.ts.Time) {
			return a.scan.ts.Time.Before(b.scan.ts.Time)
		}
	} else if a.scan.ts.Valid != b.scan.ts.Valid {
		return a.scan.ts.Valid
	}
	if ai, ok := sessionOrder[a.scan.sessionID]; ok {
		if bi, ok := sessionOrder[b.scan.sessionID]; ok && ai != bi {
			return ai < bi
		}
	}
	if a.scan.sessionID != b.scan.sessionID {
		return a.scan.sessionID < b.scan.sessionID
	}
	if a.ordinal != b.ordinal {
		return a.ordinal < b.ordinal
	}
	if a.scan.usageSource != b.scan.usageSource {
		return a.scan.usageSource < b.scan.usageSource
	}
	if a.scan.usageDedupKey != b.scan.usageDedupKey {
		return a.scan.usageDedupKey < b.scan.usageDedupKey
	}
	return !a.scan.ts.Valid && a.tsText < b.tsText
}

func pgActivityReportRowStatus(
	r pgDailyUsageScanRow, pricing *export.PricingResolver,
) (cost money.Money, priced, contributes bool, err error) {
	return pgActivityReportRowStatusWithWebSearchRequests(
		r, pgDailyUsageRowWebSearchRequests(r), pricing)
}

func pgActivityReportRowStatusWithWebSearchRequests(
	r pgDailyUsageScanRow, webSearches int, pricing *export.PricingResolver,
) (cost money.Money, priced, contributes bool, err error) {
	pricedModel, lookup := pricing.ResolveAt(
		r.model, pgUsageLookupModel(r.model, r.pricingTS),
		pgUsagePricingTimestamp(r.pricingTS),
	)
	inTok, outTok, crTok, rdTok, reasoningTok := pgDailyUsageRowTokens(r)
	cr1hTok := pgUsageRowCacheCreation1hTokens(
		r.usageSource, r.tokenJSON, crTok)
	if r.cost.Valid {
		pricing.RecordResolvedReported(r.model, pricedModel, lookup)
		return money.Money{Microdollars: r.cost.Int64}, true, true, nil
	}
	if inTok == 0 && outTok == 0 && reasoningTok == 0 &&
		crTok == 0 && rdTok == 0 && webSearches == 0 {
		return money.Money{}, true, false, nil
	}
	if !lookup.OK {
		pricing.RecordResolvedComputed(r.model, pricedModel, lookup)
		fee, feeErr := export.WebSearchFee(webSearches)
		if feeErr != nil {
			return money.Money{}, false, false, feeErr
		}
		return fee, false, true, nil
	}
	pricedModel, lookup, err = pricing.ResolveBilledAt(
		r.providerID, r.model, pgUsageLookupModel(r.model, r.pricingTS),
		pgUsagePricingTimestamp(r.pricingTS))
	if err != nil {
		return money.Money{}, false, false, err
	}
	requestScoped := pgUsageRowIsRequestScoped(r.usageSource, r.messageOrdinal)
	cost, err = lookup.Rates.CostForTokensScoped(
		requestScoped,
		inTok, outTok, reasoningTok, crTok, cr1hTok, rdTok)
	if err != nil {
		return money.Money{}, false, false,
			fmt.Errorf("pricing pg activity usage for model %q: %w", r.model, err)
	}
	cost, err = export.AddWebSearchFee(cost, webSearches)
	if err != nil {
		return money.Money{}, false, false,
			fmt.Errorf("pricing pg activity usage for model %q: %w", r.model, err)
	}
	pgRecordComputedUsagePricing(
		pricing, r.model, pricedModel, lookup,
		requestScoped, inTok, crTok, rdTok)
	return cost, true, true, nil
}
