package db

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
)

func (s *BunStore) GetActivityReport(
	ctx context.Context, f AnalyticsFilter, q activity.Query,
) (activity.Report, error) {
	artifacts, err := s.BuildActivityReportArtifacts(ctx, f, q, nil)
	if err != nil {
		return activity.Report{}, err
	}
	artifacts.Report.BySession = artifacts.Sessions
	artifacts.Report.SessionsTotal = len(artifacts.Sessions)
	return artifacts.Report, nil
}

func (s *BunStore) BuildActivityReportArtifacts(
	ctx context.Context,
	f AnalyticsFilter,
	q activity.Query,
	onProgress activity.ProgressFunc,
) (activity.CandidateArtifacts, error) {
	reportProgress(onProgress, activity.Progress{Phase: activity.ProgressLoadingSessions})
	f.IncludeSubagents = true
	f.IncludeForks = true
	lowerBound := paddedUTCBound(q.RangeStart.UTC().Format(time.RFC3339), -14)
	upperBound := paddedUTCBound(q.RangeEnd.UTC().Format(time.RFC3339), 14)

	var artifacts activity.CandidateArtifacts
	err := s.consistentView(ctx, func(store bun.IDB) error {
		candidateSessions, err := s.bunAnalyticsSessionsFrom(ctx, store, f, false)
		if err != nil {
			return err
		}
		candidateMessages, err := bunAnalyticsMessagesFrom(
			ctx, store, bunAnalyticsSessionIDs(candidateSessions),
		)
		if err != nil {
			return err
		}
		messagesBySession := bunAnalyticsMessagesBySession(candidateMessages)
		var sessions []activity.SessionMeta
		var ids []string
		for _, row := range candidateSessions {
			start := bunAnalyticsSessionTime(row)
			end := start
			if row.EndedAt != nil {
				end = row.EndedAt.UTC()
			} else {
				for _, message := range messagesBySession[row.ID] {
					if message.Timestamp != nil && message.Timestamp.After(end) {
						end = message.Timestamp.UTC()
					}
				}
			}
			if end.Before(q.RangeStart.UTC()) || !start.Before(q.RangeEnd.UTC()) {
				continue
			}
			title := row.ID
			for _, candidate := range []*string{
				row.DisplayName, row.SessionName, new(row.Project),
			} {
				if candidate != nil && *candidate != "" {
					title = *candidate
					break
				}
			}
			sessions = append(sessions, activity.SessionMeta{
				SessionID: row.ID, Title: title, Project: row.Project,
				Agent: row.Agent, Machine: row.Machine,
				StartedAt:   bunAnalyticsTimeString(row.StartedAt),
				EndedAt:     bunAnalyticsTimeString(row.EndedAt),
				IsAutomated: row.IsAutomated,
			})
			ids = append(ids, row.ID)
		}
		sort.Slice(sessions, func(i, j int) bool {
			return sessions[i].SessionID < sessions[j].SessionID
		})
		ids = ids[:0]
		allowed := make(map[string]struct{}, len(sessions))
		for _, session := range sessions {
			ids = append(ids, session.SessionID)
			allowed[session.SessionID] = struct{}{}
		}
		events := make([]activity.ActivityEvent, 0, len(candidateMessages))
		for _, message := range candidateMessages {
			if _, ok := allowed[message.SessionID]; !ok || message.Timestamp == nil {
				continue
			}
			events = append(events, activity.ActivityEvent{
				SessionID: message.SessionID, Ordinal: message.Ordinal,
				Role: message.Role, Timestamp: bunAnalyticsTimeString(message.Timestamp),
				Model: message.Model,
			})
		}
		sort.Slice(events, func(i, j int) bool {
			if events[i].SessionID != events[j].SessionID {
				return events[i].SessionID < events[j].SessionID
			}
			return events[i].Ordinal < events[j].Ordinal
		})
		reportProgress(onProgress, activity.Progress{
			Phase: activity.ProgressLoadingUsage, SessionsTotal: len(sessions),
		})
		usage, pricing, err := s.bunActivityReportUsageFrom(
			ctx, store, ids, lowerBound, upperBound, q,
		)
		if err != nil {
			return err
		}

		candidates := activity.PairActivityEvents(
			events, q.RangeStart, q.EffectiveEnd,
			time.Duration(q.GapCapSeconds)*time.Second,
		)
		rowsProcessed := int64(0)
		built, err := activity.BuildCandidateArtifactsFromSourceWithSurvivorUsage(
			ctx,
			activity.Params{
				RangeStart:    q.RangeStart,
				RangeEnd:      q.RangeEnd,
				Loc:           q.Loc,
				EffectiveEnd:  q.EffectiveEnd,
				Partial:       q.Partial,
				GapCapSeconds: q.GapCapSeconds,
				Bucket:        q.Bucket,
			},
			sessions,
			func(
				ctx context.Context,
				yield func(activity.IntervalCandidate) error,
			) error {
				reportProgress(onProgress, activity.Progress{
					Phase:         activity.ProgressScanningActivity,
					SessionsTotal: len(sessions),
				})
				for _, candidate := range candidates {
					if err := ctx.Err(); err != nil {
						return err
					}
					rowsProcessed++
					reportProgress(onProgress, activity.Progress{
						Phase:         activity.ProgressScanningActivity,
						SessionsTotal: len(sessions),
						RowsProcessed: rowsProcessed,
					})
					if err := yield(candidate); err != nil {
						return err
					}
				}
				return nil
			},
			usage,
		)
		if err != nil {
			return fmt.Errorf("aggregating Bun activity report: %w", err)
		}
		reportProgress(onProgress, activity.Progress{
			Phase:             activity.ProgressFinalizing,
			SessionsTotal:     len(sessions),
			SessionsProcessed: len(sessions),
			RowsProcessed:     rowsProcessed,
		})
		built.Report.SchemaVersion = export.ActivityReportSchemaVersion
		built.Report.Pricing = pricing
		projects, err := buildBunProjectIdentityMapFrom(
			ctx, store, activityReportProjectLabels(sessions),
		)
		if err != nil {
			return err
		}
		built.Report.BySession = built.Sessions
		activity.SanitizeProjectLabels(&built.Report, projects)
		built.Sessions = built.Report.BySession
		built.Report.BySession = []activity.SessionRow{}
		built.Report.Projects = export.ProjectMapForWire(projects)
		reportProgress(onProgress, activity.Progress{
			Phase:             activity.ProgressDone,
			SessionsTotal:     len(sessions),
			SessionsProcessed: len(sessions),
			RowsProcessed:     rowsProcessed,
		})
		artifacts = built
		return nil
	})
	return artifacts, err
}

func (s *BunStore) bunActivityReportUsageFrom(
	ctx context.Context,
	store bun.IDB,
	ids []string,
	lowerBound, upperBound string,
	q activity.Query,
) ([]activity.UsageRow, *export.PricingBlock, error) {
	pricing, err := s.loadPricingMapFrom(ctx, store)
	if err != nil {
		return nil, nil, fmt.Errorf("loading activity-report pricing: %w", err)
	}
	rateResolver := export.NewPricingResolver(pricing)
	if len(ids) == 0 {
		block, blockErr := rateResolver.BuildBlock()
		return []activity.UsageRow{}, &block, blockErr
	}
	loc := q.Loc
	if loc == nil {
		loc = time.UTC
	}
	rows, err := s.loadDailyUsageRowsFrom(ctx, store, UsageFilter{
		From:     q.RangeStart.In(loc).Format("2006-01-02"),
		To:       q.RangeEnd.In(loc).Format("2006-01-02"),
		Timezone: q.Timezone,
	}, false, false)
	if err != nil {
		return nil, nil, err
	}
	lower, lowerErr := parseTimestamp(lowerBound)
	upper, upperErr := parseTimestamp(upperBound)
	var candidates []activityReportUsageCandidate
	for _, row := range rows {
		parsed, parseErr := parseTimestamp(row.ts)
		if parseErr == nil {
			if lowerErr == nil && parsed.Before(lower) {
				continue
			}
			if upperErr == nil && parsed.After(upper) {
				continue
			}
		}
		ordinal := int64(-1)
		if row.messageOrdinal.Valid {
			ordinal = row.messageOrdinal.Int64
		}
		candidates = append(candidates, activityReportUsageCandidate{
			scan: row, ts: parsed, validTS: parseErr == nil, ordinal: ordinal,
			row: activity.UsageRow{
				SessionID: row.sessionID, Model: row.model, Timestamp: row.ts,
				Project: row.project, Machine: row.machine, MessageOrdinal: ordinal,
				UsageSource: row.usageSource, Agent: row.agent,
				ClaudeMessageID: row.claudeMessageID,
				ClaudeRequestID: row.claudeRequestID, SourceUUID: row.sourceUUID,
				UsageDedupKey: row.usageDedupKey,
			},
		})
	}
	sortActivityReportUsageCandidates(candidates)
	baseRows := make([]activity.UsageRow, len(candidates))
	for i, candidate := range candidates {
		row := candidate.row
		_, row.OutputTokens, _, _, _ = dailyUsageRowTokens(candidate.scan)
		row.WebSearchRequests = usageRowWebSearchRequests(
			candidate.scan.usageSource, candidate.scan.tokenJSON)
		baseRows[i] = row
	}
	mask, attribution, webSearchRequests :=
		activity.UsageSurvivorSelectionForSessions(
			q.RangeStart, q.RangeEnd, q.EffectiveEnd, baseRows, ids,
		)
	return materializeActivityReportUsageCandidates(
		candidates, mask, attribution, webSearchRequests, rateResolver,
	)
}
