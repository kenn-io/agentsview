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

// GetActivityReport assembles a concurrency- and usage-oriented report
// for the resolved range `q`, reading from the PostgreSQL store. It
// mirrors the SQLite (*DB).GetActivityReport: three fetches scoped to the
// SAME candidate session-ID set so the concurrency timeline, sessions
// table, and usage totals stay mutually consistent (no orphan usage
// rows), then the in-memory streams are handed to activity.Aggregate.
//
// The filter `f` is honored as-is: callers that want one-shot or
// automated sessions included must pass them through with the
// corresponding exclusions disabled. Subagent and fork sessions are
// always counted so the cost totals match GetDailyUsage, which never
// filters by relationship_type. Fork sessions hold only their own
// rewound-branch messages (the parsers partition entries across
// branches), so counting them adds no duplicate activity; any usage
// rows that do recur across sessions collapse in the aggregator's
// dedup, the same guarantee GetDailyUsage relies on.

// GetSessionUsageRows returns the backend-priced usage rows for the supplied
// sessions, with the same cross-session deduplication as activity reports.
type pgSessionUsageOrderedRow struct {
	scan    pgUsageScanRow
	tsText  string
	ordinal int64
}

func (s *Store) GetSessionUsageRows(
	ctx context.Context, ids []string,
) ([]activity.UsageRow, error) {
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
	seen := make(map[pgUsageDedupToken]struct{})
	out := make([]activity.UsageRow, 0, len(rowsAcc))
	for _, o := range rowsAcc {
		r := o.scan
		if key, ok := pgUsageDedupTokenForRow(
			r.usageSource, r.agent, r.claudeMessageID,
			r.claudeRequestID, r.sourceUUID, r.usageDedupKey,
		); ok {
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		}
		_, outputTok, _, _, _ := pgDailyUsageRowTokens(
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
		costRow := r
		var sessionCost *money.Money
		if r.costSource == db.CopilotReportedCostSource && r.cost.Valid {
			v := money.Money{Microdollars: r.cost.Int64}
			sessionCost = &v
			costRow.cost = sql.NullInt64{}
			rateResolver.RecordUnattributedReported()
		}
		cost, priced, contributes, priceErr :=
			pgSessionRowCost(costRow, rateResolver)
		if priceErr != nil {
			return nil, priceErr
		}
		costSource := export.CostSourceComputed
		if costRow.cost.Valid {
			costSource = export.CostSourceReported
		}
		out = append(out, activity.UsageRow{
			SessionID:       r.sessionID,
			Model:           r.model,
			Timestamp:       o.tsText,
			OutputTokens:    outputTok,
			Cost:            cost,
			CostSource:      costSource,
			SessionCost:     sessionCost,
			Priced:          priced,
			Contributes:     contributes,
			Agent:           r.agent,
			ClaudeMessageID: r.claudeMessageID,
			ClaudeRequestID: r.claudeRequestID,
			SourceUUID:      r.sourceUUID,
			UsageDedupKey:   r.usageDedupKey,
		})
	}
	return out, nil
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
	if a.tsText != b.tsText {
		return a.tsText < b.tsText
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
	return a.scan.usageDedupKey < b.scan.usageDedupKey
}

func pgActivityReportRowStatus(
	r pgDailyUsageScanRow, pricing *export.PricingResolver,
) (cost money.Money, priced, contributes bool, err error) {
	pricedModel, lookup := pricing.Resolve(
		r.model, pgUsageLookupModel(r.model, r.ts))
	inTok, outTok, crTok, rdTok, reasoningTok := pgDailyUsageRowTokens(r)

	if r.cost.Valid {
		pricing.RecordResolvedReported(r.model, pricedModel, lookup)
		return money.Money{Microdollars: r.cost.Int64}, true, true, nil
	}
	if inTok == 0 && outTok == 0 && reasoningTok == 0 &&
		crTok == 0 && rdTok == 0 {
		return money.Money{}, true, false, nil
	}
	if !lookup.OK {
		pricing.RecordResolvedComputed(r.model, pricedModel, lookup)
		return money.Money{}, false, true, nil
	}
	requestScoped := pgUsageRowIsRequestScoped(r.usageSource, r.messageOrdinal)
	cost, err = lookup.Rates.CostForTokensScoped(
		requestScoped,
		inTok, outTok, reasoningTok, crTok, rdTok)
	if err != nil {
		return money.Money{}, false, false,
			fmt.Errorf("pricing pg activity usage for model %q: %w", r.model, err)
	}
	pgRecordComputedUsagePricing(
		pricing, r.model, pricedModel, lookup,
		requestScoped, inTok, crTok, rdTok)
	return cost, true, true, nil
}
