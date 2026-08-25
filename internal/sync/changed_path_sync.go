package sync

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/parser"
)

// ChangedPathSyncResult describes bounded, non-destructive processing of a
// remote changed-path plan. Cache attribution remains process-local.
type ChangedPathSyncResult struct {
	Stats                   SyncStats
	FilesDiscovered         int
	FilesProcessed          int
	FallbackSources         int
	CachedSourceKeys        map[string]struct{}      `json:"-"`
	CachedFallbackProviders map[parser.AgentType]int `json:"-"`
}

// ChangedPathSyncOptions controls execution-only behavior for a planned
// changed-path import. ForceFullParse is the armed journal projection: its
// exact sources and fallback providers bypass freshness and incremental append.
// A durable skip entry may still suppress a source attempted after the
// journal's cache invalidation was consumed.
type ChangedPathSyncOptions struct {
	ForceFullParse ChangedPathPruneScope
}

// SyncChangedPathPlanContext processes exact sources and explicitly selected
// provider fallbacks without granting deletion authority over missing sources.
func (e *Engine) SyncChangedPathPlanContext(
	ctx context.Context,
	plan ChangedPathPlan,
	onProgress ProgressFunc,
) (ChangedPathSyncResult, error) {
	return e.SyncChangedPathPlanWithOptionsContext(
		ctx, plan, ChangedPathSyncOptions{}, onProgress,
	)
}

// SyncChangedPathPlanWithOptionsContext processes a plan with explicit
// execution scope. Keeping the armed scope separate from the import plan lets
// disarmed journal entries remain importable without re-forcing poison paths.
func (e *Engine) SyncChangedPathPlanWithOptionsContext(
	ctx context.Context,
	plan ChangedPathPlan,
	options ChangedPathSyncOptions,
	onProgress ProgressFunc,
) (ChangedPathSyncResult, error) {
	result := ChangedPathSyncResult{
		CachedSourceKeys:        make(map[string]struct{}),
		CachedFallbackProviders: make(map[parser.AgentType]int),
	}
	if e.refuseWriteInForceParse("SyncChangedPathPlan") {
		return result, nil
	}

	fallbackFiles, fallbackCounts, err := e.discoverChangedPathFallbackProviders(
		ctx, plan.FallbackProviders,
	)
	if err != nil {
		return result, err
	}
	forceSourceKeys := make(map[string]struct{}, len(options.ForceFullParse.Files))
	for _, file := range options.ForceFullParse.Files {
		forceSourceKeys[changedPathSourceKey(file)] = struct{}{}
	}
	forceProviders := make(map[parser.AgentType]struct{},
		len(options.ForceFullParse.FallbackProviders))
	for _, agent := range options.ForceFullParse.FallbackProviders {
		forceProviders[agent] = struct{}{}
	}
	planFiles := append([]parser.DiscoveredFile(nil), plan.Files...)
	for i := range planFiles {
		if _, force := forceSourceKeys[changedPathSourceKey(planFiles[i])]; force {
			// The journal scope supersedes plan-time ForceParse. Its cache was
			// already invalidated durably, so a surviving entry is proof of a
			// later attempt and must be allowed to suppress replay.
			planFiles[i].ForceParse = false
			planFiles[i].ForceFullParse = true
		}
	}
	for i := range fallbackFiles {
		if _, force := forceProviders[fallbackFiles[i].Agent]; force {
			fallbackFiles[i].ForceFullParse = true
		}
	}
	for _, count := range fallbackCounts {
		result.FallbackSources += count
	}
	exactKeys := make(map[string]struct{}, len(plan.Files))
	for _, file := range plan.Files {
		exactKeys[changedPathSourceKey(file)] = struct{}{}
	}
	fallbackKeys := make(map[string]parser.AgentType, len(fallbackFiles))
	for _, file := range fallbackFiles {
		key := changedPathSourceKey(file)
		if _, exact := exactKeys[key]; !exact {
			fallbackKeys[key] = file.Agent
		}
	}
	files := sortAndDedupeChangedPathFiles(append(
		planFiles, fallbackFiles...,
	))
	result.FilesDiscovered = len(files)
	if len(files) == 0 {
		return result, ctx.Err()
	}

	e.syncMu.Lock()
	var stats SyncStats
	defer func() {
		if stats.hasSessionChanges() {
			e.emit("sessions")
		}
	}()
	defer e.syncMu.Unlock()
	defer e.clearCurrentProgress()
	e.resetS3CodexIndexCache()
	e.anomalies.reset()
	var processErr error

	physicalPaths := make([]string, 0, len(files))
	for _, file := range files {
		physicalPaths = append(physicalPaths, file.Path)
	}
	preContainerStates := e.captureSQLiteContainerStates(physicalPaths)
	e.beginSQLiteContainerPass(files, preContainerStates)
	processingCtx := context.WithValue(ctx, deferGlobalLinkContextKey{}, true)
	results := e.startWorkers(processingCtx, files)
	affectedSessionIDs := make(map[string]struct{})
	stats = e.collectAndBatchWithOptions(
		processingCtx, results, len(files), len(files), func(progress Progress) {
			progress.FallbackProviders = len(plan.FallbackProviders)
			progress.FallbackSources = result.FallbackSources
			if onProgress != nil {
				onProgress(progress)
			}
		}, syncWriteDefault, collectAndBatchOptions{
			preserveMissingSources: true,
			observeResult: func(job syncJob) {
				result.FilesProcessed++
				if job.incremental != nil {
					affectedSessionIDs[job.incremental.sessionID] = struct{}{}
				}
				for _, parsed := range job.results {
					affectedSessionIDs[applyIDPrefixToID(e.idPrefix, parsed.Session.ID)] = struct{}{}
				}
				for _, id := range job.excludedSessionIDs {
					affectedSessionIDs[applyIDPrefixToID(e.idPrefix, id)] = struct{}{}
				}
				if !job.cachedSkip {
					return
				}
				key := changedPathSourceKey(parser.DiscoveredFile{
					Agent: job.agent, Path: job.path,
				})
				if _, exact := exactKeys[key]; exact {
					result.CachedSourceKeys[key] = struct{}{}
					return
				}
				if agent, fallback := fallbackKeys[key]; fallback {
					result.CachedFallbackProviders[agent]++
				}
			},
		},
	)
	if len(affectedSessionIDs) > 0 && !stats.Aborted {
		ids := make([]string, 0, len(affectedSessionIDs))
		for id := range affectedSessionIDs {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		if err := e.db.LinkSubagentSessionsForSessions(ids); err != nil {
			stats.RecordFailed()
			processErr = errors.Join(processErr,
				fmt.Errorf("link affected subagent sessions: %w", err))
			if queueErr := e.db.QueueSubagentParentRepairs(ids); queueErr != nil {
				processErr = errors.Join(processErr,
					fmt.Errorf("queue affected subagent parent repairs: %w", queueErr))
			}
		}
	}
	e.finishSQLiteContainerPass(true, false)
	e.anomalies.applyTo(&stats)
	if !e.ephemeral {
		e.persistSkipCache()
	}
	e.mu.Lock()
	e.lastSync = time.Now()
	e.lastSyncStats = stats
	e.mu.Unlock()
	result.Stats = stats

	if stats.Synced > 0 || stats.CwdUpdated > 0 {
		log.Printf("sync: %d file(s) updated", stats.Synced)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !stats.ProcessingComplete() {
		return result, errors.Join(processErr, fmt.Errorf(
			"changed-path plan sync incomplete: %d source or archive failures",
			stats.Failed,
		))
	}
	return result, processErr
}
