package db

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

// usageRollupSlowRequestThreshold gates the privacy-safe phase-timing log so
// warm reads stay silent and slow requests explain where the time went.
const usageRollupSlowRequestThreshold = 2 * time.Second

type usageRollupRequestTimings struct {
	capture, sweep, fill, read time.Duration
	rollup                     usageRollupMetrics
	candidates                 int
}

func (timings usageRollupRequestTimings) logIfSlow(started time.Time) {
	total := time.Since(started)
	if total < usageRollupSlowRequestThreshold {
		return
	}
	round := func(value time.Duration) time.Duration {
		return value.Round(time.Millisecond)
	}
	log.Printf("usage rollup read took %s: capture %s, deletion sweep %s, "+
		"fact fill %s, rollup build %s, rollup install %s, cache read %s "+
		"(%d candidate sessions; built %d daily rows, %d exception rows in %d groups)",
		round(total), round(timings.capture), round(timings.sweep),
		round(timings.fill), round(timings.rollup.BuildDuration),
		round(timings.rollup.InstallDuration), round(timings.read),
		timings.candidates, timings.rollup.DailyRows,
		timings.rollup.ExceptionRows, timings.rollup.ExceptionGroups)
}

func (db *DB) queryUsageRollups(
	ctx context.Context, filter UsageFilter, kind usageQueryKind,
	includeCursor bool,
) (usageQuerySnapshot, usageFactsResult, *export.PricingResolver, error) {
	for attempt := 1; attempt <= usageFillMaxAttempts; attempt++ {
		started := time.Now()
		var timings usageRollupRequestTimings
		snapshot, captureErr := db.captureUsageQuery(ctx, filter, kind)
		if captureErr != nil {
			return usageQuerySnapshot{}, usageFactsResult{}, nil, captureErr
		}
		timings.capture = time.Since(started)
		timings.candidates = len(snapshot.Versions)
		if !includeCursor || !usageCursorIncluded(filter) {
			snapshot.CursorHighWater = 0
		}
		cache, release, generationErr := db.usageCache.acquireGeneration(
			ctx, snapshot.DatabaseID)
		if generationErr != nil {
			if errors.Is(generationErr, errUsageCacheSourceChanged) &&
				attempt < usageFillMaxAttempts {
				continue
			}
			return usageQuerySnapshot{}, usageFactsResult{}, nil, generationErr
		}
		sweepStarted := time.Now()
		if sweepErr := cache.sweepDeletionJournal(ctx, db); sweepErr != nil {
			release()
			return usageQuerySnapshot{}, usageFactsResult{}, nil, sweepErr
		}
		timings.sweep = time.Since(sweepStarted)
		fillStarted := time.Now()
		fills, fillErr := cache.fill.Ensure(
			ctx, snapshot.Versions, snapshot.CursorHighWater,
		)
		if fillErr != nil {
			release()
			if errors.Is(fillErr, errUsageCacheSourceChanged) &&
				attempt < usageFillMaxAttempts {
				continue
			}
			return usageQuerySnapshot{}, usageFactsResult{}, nil, fillErr
		}
		timings.fill = time.Since(fillStarted)
		snapshot.dropDeleted(fills)
		resolver := export.NewPricingResolver(snapshot.PricingRows)
		installs, rollupMetrics, rollupErr := cache.rollup.Ensure(
			ctx, snapshot, fills, resolver)
		if rollupErr != nil {
			release()
			if errors.Is(rollupErr, errUsageCacheSourceChanged) &&
				attempt < usageFillMaxAttempts {
				continue
			}
			return usageQuerySnapshot{}, usageFactsResult{}, nil, rollupErr
		}
		timings.rollup = rollupMetrics
		readStarted := time.Now()
		var facts usageFactsResult
		var queryErr error
		if kind == usageQueryKindActivity {
			var matching map[string]UsageSessionInfo
			matching, queryErr = cache.usageRollupActivityQuery(
				ctx, snapshot, filter, installs)
			facts.MatchingSessions = matching
		} else {
			facts, queryErr = cache.usageRollupQuery(
				ctx, snapshot, filter, installs, resolver)
		}
		timings.read = time.Since(readStarted)
		if queryErr == nil {
			release()
			timings.logIfSlow(started)
			return snapshot, facts, resolver, nil
		}
		release()
		if !usageCacheReadShouldRecapture(queryErr) || attempt == usageFillMaxAttempts {
			return usageQuerySnapshot{}, usageFactsResult{}, nil, queryErr
		}
	}
	return usageQuerySnapshot{}, usageFactsResult{}, nil, fmt.Errorf(
		"usage cache read could not stabilize after %d attempts",
		usageFillMaxAttempts,
	)
}

// GetTopSessionsByCost returns filtered sessions ranked by cost or tokens.
func (db *DB) GetTopSessionsByCost(
	ctx context.Context, filter UsageFilter, limit int,
) ([]TopSessionEntry, error) {
	snapshot, facts, _, err := db.queryUsageRollups(
		ctx, filter, usageQueryKindToken, false)
	if err != nil {
		return nil, err
	}
	type totals struct {
		input, output, cacheWrite, cacheRead int
		cost                                 money.Money
		authoritative                        *money.Money
	}
	bySession := make(map[string]*totals)
	for _, group := range facts.Groups {
		if group.SessionID == "" {
			continue
		}
		current := bySession[group.SessionID]
		if current == nil {
			current = &totals{}
			bySession[group.SessionID] = current
		}
		current.input += int(group.InputTokens)
		current.output += int(group.OutputTokens)
		current.cacheWrite += int(group.CacheCreationTokens)
		current.cacheRead += int(group.CacheReadTokens)
		current.cost, err = money.Add(current.cost,
			money.Money{Microdollars: group.CostMicrodollars})
		if err != nil {
			return nil, fmt.Errorf("summing top-session cost: %w", err)
		}
		if filter.Model == "" && filter.ExcludeModel == "" &&
			group.AuthoritativeCostMicrodollars != nil {
			value := money.Money{Microdollars: *group.AuthoritativeCostMicrodollars}
			current.authoritative = &value
		}
	}
	metadata := make(map[string]usageQuerySession, len(snapshot.Sessions))
	for _, session := range snapshot.Sessions {
		metadata[session.ID] = session
	}
	result := make([]TopSessionEntry, 0, len(bySession))
	for sessionID, value := range bySession {
		cost := value.cost
		if value.authoritative != nil {
			cost = *value.authoritative
		}
		entry := TopSessionEntry{
			SessionID: sessionID, DisplayName: sessionID,
			InputTokens: value.input, OutputTokens: value.output,
			CacheCreationTokens: value.cacheWrite,
			CacheReadTokens:     value.cacheRead,
			TotalTokens:         value.input + value.output + value.cacheWrite + value.cacheRead,
			Cost:                cost,
		}
		if session, ok := metadata[sessionID]; ok {
			entry.DisplayName = session.DisplayName
			entry.Agent = session.Agent
			entry.Project = session.Project
			entry.StartedAt = session.StartedAt
		}
		result = append(result, entry)
	}
	return SortAndLimitTopSessions(
		result, limit, filter.TopSessionsSort, filter.TopSessionsTokenTypes,
	), nil
}

// GetUsageSessionCounts returns distinct billed survivor owners by metadata.
func (db *DB) GetUsageSessionCounts(
	ctx context.Context, filter UsageFilter,
) (UsageSessionCounts, error) {
	_, facts, _, err := db.queryUsageRollups(
		ctx, filter, usageQueryKindToken, false)
	if err != nil {
		return UsageSessionCounts{}, err
	}
	return NewUsageSessionCounts(facts.MatchingSessions), nil
}

// GetUsageMatchingSessionCount counts sessions with eligible usage activity.
func (db *DB) GetUsageMatchingSessionCount(
	ctx context.Context, filter UsageFilter,
) (int, error) {
	_, facts, _, err := db.queryUsageRollups(
		ctx, filter, usageQueryKindActivity, false)
	if err != nil {
		return 0, err
	}
	return len(facts.MatchingSessions), nil
}

// GetSessionUsage returns one session's exact, intra-session usage summary.
func (db *DB) GetSessionUsage(
	ctx context.Context, sessionID string, includeBreakdown bool,
) (*SessionUsage, error) {
	return db.getSessionUsageLegacy(ctx, sessionID, includeBreakdown)
}
