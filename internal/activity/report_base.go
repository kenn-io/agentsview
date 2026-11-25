package activity

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"go.kenn.io/agentsview/internal/export"
)

// ReportBackend supplies every backend operation needed to assemble a report.
type ReportBackend interface {
	// ActivityReportUsage returns the usage survivors of the candidate
	// sessions. The report only sums survivors, after it apportions a
	// session total (SessionCost) over its session's rows by their costs.
	// So a session without one may hand back its survivors summed per
	// model and bucket window, each sum stamped with one of its rows'
	// timestamps.
	ActivityReportUsage(context.Context, []string, Query) ([]UsageRow, *export.PricingBlock, error)
	ActivityReportCandidateSource([]string, Query) CandidateSource
	ActivityReportMessageCounts(context.Context, []string, Query, *CandidateArtifacts) error
	BuildProjectIdentityMap(context.Context, []string) (map[string]export.ProjectMapEntry, error)
}

// ConcurrentUsageBackend reserves a second read for usage while candidates stream.
type ConcurrentUsageBackend interface {
	ActivityReportSecondRead() (release func(), ok bool)
}

// EvidenceBackend supplies subagent runs that may be excluded by report filters.
type EvidenceBackend interface {
	ActivityReportEvidence(context.Context, Query) ([]SessionMeta, error)
}

// BuildReportArtifacts assembles a report and returns the emitted completion event.
func BuildReportArtifacts(
	ctx context.Context, backend ReportBackend, sessions []SessionMeta,
	ids []string, q Query, onProgress ProgressFunc, aggregationLabel string,
) (CandidateArtifacts, Progress, error) {
	selected := make(map[string]bool, len(ids))
	for _, id := range ids {
		selected[id] = true
	}
	var evidence []SessionMeta
	if source, ok := backend.(EvidenceBackend); ok {
		var err error
		evidence, err = source.ActivityReportEvidence(ctx, q)
		if err != nil {
			return CandidateArtifacts{}, Progress{}, err
		}
		evidence = slices.DeleteFunc(evidence, func(session SessionMeta) bool {
			return selected[session.SessionID]
		})
	}
	var ancestors Launchers
	if launchers, ok := backend.(LauncherBackend); ok {
		var err error
		ancestors, err = launchers.ActivityReportLaunchers(ctx, append(slices.Clone(sessions), evidence...), selected, q)
		if err != nil {
			return CandidateArtifacts{}, Progress{}, err
		}
	}
	sweepIDs := slices.Clone(ids)
	hidden := make(map[string]bool)
	for _, run := range evidence {
		if ancestors.Waiting[run.SessionID] != "" {
			sweepIDs = append(sweepIDs, run.SessionID)
			hidden[run.SessionID] = true
		}
	}
	var usage []UsageRow
	var pricing *export.PricingBlock
	var usageErr error
	readCtx := ctx
	loadUsage := func() ([]UsageRow, error) { return usage, nil }
	var ready chan struct{}
	var cancel context.CancelCauseFunc
	if concurrent, ok := backend.(ConcurrentUsageBackend); ok {
		if release, ok := concurrent.ActivityReportSecondRead(); ok {
			readCtx, cancel = context.WithCancelCause(ctx)
			defer cancel(nil)
			ready = make(chan struct{})
			go func() {
				defer close(ready)
				defer release()
				usage, pricing, usageErr = backend.ActivityReportUsage(readCtx, ids, q)
				if usageErr != nil {
					cancel(usageErr)
				}
			}()
			loadUsage = func() ([]UsageRow, error) {
				<-ready
				return usage, usageErr
			}
		}
	}
	if ready == nil {
		usage, pricing, usageErr = backend.ActivityReportUsage(ctx, ids, q)
		if usageErr != nil {
			return CandidateArtifacts{}, Progress{}, usageErr
		}
	}

	rowsProcessed := int64(0)
	params := Params{
		RangeStart:    q.RangeStart,
		RangeEnd:      q.RangeEnd,
		Loc:           q.Loc,
		EffectiveEnd:  q.EffectiveEnd,
		Partial:       q.Partial,
		GapCapSeconds: q.GapCapSeconds,
		Bucket:        q.Bucket,
		Waiting:       ancestors.Waiting,
		Evidence:      hidden,
	}
	scanning := func() {
		ReportProgress(onProgress, Progress{
			Phase: ProgressScanningActivity, SessionsTotal: len(sessions),
		})
	}
	processed := func() {
		rowsProcessed++
		ReportProgress(onProgress, Progress{
			Phase:         ProgressScanningActivity,
			SessionsTotal: len(sessions), RowsProcessed: rowsProcessed,
		})
	}
	var artifacts CandidateArtifacts
	var err error
	if runs, ok := backend.(RunSourceBackend); ok {
		source := runs.ActivityReportRunSource(sweepIDs, q)
		artifacts, err = BuildRunArtifactsWithSurvivorUsage(readCtx, params, sessions, func(
			ctx context.Context, yield func(Run) error,
		) error {
			scanning()
			return source(ctx, func(run Run) error {
				processed()
				return yield(run)
			})
		}, loadUsage)
	} else {
		source := backend.ActivityReportCandidateSource(sweepIDs, q)
		artifacts, err = BuildCandidateArtifactsFromSourceWithSurvivorUsage(readCtx, params, sessions, func(
			ctx context.Context, yield func(IntervalCandidate) error,
		) error {
			scanning()
			return source(ctx, func(candidate IntervalCandidate) error {
				processed()
				return yield(candidate)
			})
		}, loadUsage)
	}
	if err != nil {
		if ready != nil {
			cancel(err)
			<-ready
			// Keep the initiating stage's error, before its peer observes cancellation.
			err = context.Cause(readCtx)
		}
		return CandidateArtifacts{}, Progress{}, fmt.Errorf("%s: %w", aggregationLabel, err)
	}
	if err := backend.ActivityReportMessageCounts(ctx, ids, q, &artifacts); err != nil {
		return CandidateArtifacts{}, Progress{}, err
	}
	ReportProgress(onProgress, Progress{
		Phase: ProgressFinalizing, SessionsTotal: len(sessions),
		SessionsProcessed: len(sessions), RowsProcessed: rowsProcessed,
	})
	artifacts.Report.SchemaVersion = export.ActivityReportSchemaVersion
	artifacts.Report.Pricing = pricing
	projects, err := backend.BuildProjectIdentityMap(ctx,
		ProjectLabels(sessions))
	if err != nil {
		return CandidateArtifacts{}, Progress{}, err
	}
	artifacts.Report.BySession = artifacts.Sessions
	SanitizeProjectLabels(&artifacts.Report, projects)
	artifacts.Sessions = artifacts.Report.BySession
	artifacts.Report.BySession = []SessionRow{}
	artifacts.Report.Projects = export.ProjectMapForWire(projects)
	done := Progress{
		Phase: ProgressDone, SessionsTotal: len(sessions),
		SessionsProcessed: len(sessions), RowsProcessed: rowsProcessed,
	}
	ReportProgress(onProgress, done)
	return artifacts, done, nil
}

// ReportWithSessions includes the session rows in the full report response.
func ReportWithSessions(artifacts CandidateArtifacts) Report {
	artifacts.Report.BySession = artifacts.Sessions
	artifacts.Report.SessionsTotal = len(artifacts.Sessions)
	return artifacts.Report
}

// ReportProgress calls callback when it is set.
func ReportProgress(callback ProgressFunc, progress Progress) {
	if callback != nil {
		callback(progress)
	}
}

// ProjectLabels returns the distinct session projects, sorted.
func ProjectLabels(sessions []SessionMeta) []string {
	set := make(map[string]struct{}, len(sessions))
	for _, session := range sessions {
		set[session.Project] = struct{}{}
	}
	if len(set) == 0 {
		return []string{}
	}
	return slices.Sorted(maps.Keys(set))
}
