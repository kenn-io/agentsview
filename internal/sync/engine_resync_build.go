package sync

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// resyncBuildLocked builds a complete replacement archive at the temp path from
// the current sources plus any contributors, copies preserved state (orphans,
// insights, recall, user metadata) from the original, persists the fresh skip
// cache into the replacement, and closes it. It reads the original but never
// closes or swaps it; the caller performs the swap. The caller holds syncMu and
// guarantees the original receives no writes for the duration.
func (e *Engine) resyncBuildLocked(
	ctx context.Context, onProgress ProgressFunc, opts RebuildOptions,
	ops rebuildOperations, restoreActiveWriterOnAbort bool,
) (stats SyncStats, retErr error) {
	if e.rebuildSources != nil {
		sources, err := e.rebuildSources(ctx)
		if err != nil {
			return SyncStats{Aborted: true, Warnings: []string{err.Error()}}, err
		}
		opts.Contributors = append(opts.Contributors, sources.Contributors...)
		opts.UnavailableContributorIDPrefixes = append(opts.UnavailableContributorIDPrefixes, sources.UnavailableContributorIDPrefixes...)
	}
	if e.sourceSyncErr != nil && (!errors.Is(e.sourceSyncErr, db.ErrArchiveOnly) || len(opts.Contributors) > 0) {
		return SyncStats{Aborted: true}, e.sourceSyncErr
	}
	e.clearPiebaldFailureMemo()
	defer e.clearPiebaldFailureMemo()
	// Rebuild tombstones and links commit only inside the replacement, and every
	// aborted or failed build discards it. Hold them here and publish the
	// count only on the successful return, so no failure branch stores or
	// returns removals that never reached the archive.
	var pendingTombstoned, pendingLinksUpdated int
	reportResyncProgress := func(p Progress) {
		p.Resync = true
		if p.Phase == PhaseSyncing && p.Detail == "" {
			p.Detail = "Syncing sessions into rebuilt database"
		}
		e.reportProgress(onProgress, p)
	}
	reportResyncPhase := func(phase Phase, detail, hint string) {
		reportResyncProgress(Progress{
			Phase:  phase,
			Detail: detail,
			Hint:   hint,
		})
	}

	// Resync rebuilds the archive from scratch, so every shared-SQLite
	// container and storage session must be re-verified against the
	// fresh database.
	e.clearTrustedSQLiteContainers()
	e.clearTrustedOpenCodeStorageSessions()
	e.clearVerifiedSources()

	origDB := e.db
	origPath := origDB.Path()
	tempPath := origPath + resyncTempSuffix
	reportResyncPhase(
		PhasePreparingResync,
		"Preparing full resync",
		"",
	)

	// Snapshot old non-OpenCode-format file-backed session count
	// to detect empty-discovery. OpenCode-format agents are
	// excluded entirely because a root may legitimately fall back
	// between storage and SQLite sources across resyncs. Fail closed:
	// if we can't query, assume old DB has file-backed data
	// worth protecting.
	oldFileSessions, err := e.protectedFileSessionCount(ctx, origDB, "", "", false)
	if err != nil {
		log.Printf("resync: get old file count: %v", err)
		oldFileSessions = 1
	}
	prefixes, err := origDB.AttachedArchiveIDPrefixes(ctx)
	if err != nil {
		return SyncStats{Aborted: true}, err
	}
	opts.UnavailableContributorIDPrefixes = append(opts.UnavailableContributorIDPrefixes, prefixes...)
	localOldFileSessions := oldFileSessions
	rebuildOldFileSessions := oldFileSessions
	contributorOldFileSessions := make([]int, len(opts.Contributors))
	if len(opts.Contributors) > 0 ||
		len(opts.UnavailableContributorIDPrefixes) > 0 {
		contributorPrefixes := slices.Clone(
			opts.UnavailableContributorIDPrefixes,
		)
		contributorPrefixes = slices.Grow(
			contributorPrefixes, len(opts.Contributors),
		)
		for _, contributor := range opts.Contributors {
			contributorPrefixes = append(contributorPrefixes, contributor.IDPrefixes...)
			if contributor.Config.IDPrefix != "" {
				contributorPrefixes = append(
					contributorPrefixes, contributor.Config.IDPrefix,
				)
			}
		}
		disabledStorage := storageAgentsForDisabledProviders(e.sources().preserveAgents)
		// A disabled ICodeMate provider discovers nothing, so its CLI JSONL
		// rows are preserved rather than rediscovered and must leave the
		// protected count with the container rows.
		excludedAgents := []db.RebuildAgentExclusion{
			{Agent: string(parser.AgentOpenCode)},
			{Agent: string(parser.AgentKilo)},
			{Agent: string(parser.AgentMiMoCode)},
			{
				Agent: string(parser.AgentIcodemate),
				KeepJSONLRows: !slices.Contains(
					disabledStorage, parser.AgentIcodemate,
				),
			},
		}
		for _, agent := range disabledStorage {
			excludedAgents = append(
				excludedAgents, db.RebuildAgentExclusion{Agent: string(agent)},
			)
		}
		localOldFileSessions, err = origDB.FileBackedSessionCountForRebuildOwner(
			ctx, e.machine, contributorPrefixes, excludedAgents,
		)
		if err != nil {
			log.Printf("resync: get old local rebuild file count: %v", err)
			localOldFileSessions = 1
		}
		for i, contributor := range opts.Contributors {
			if contributor.ArchiveSources != nil {
				for _, source := range contributor.ArchiveSources {
					count, err := origDB.FileBackedSessionCountForArchiveSource(ctx, source)
					if err != nil {
						return SyncStats{Aborted: true}, err
					}
					contributorOldFileSessions[i] += count
				}
				continue
			}
			count, countErr := protectedFileSessionCount(ctx,
				origDB, contributor.Config.Machine,
				contributor.Config.IDPrefix,
				contributor.Config.Machine != "",
				contributor.Config.DisabledAgents,
			)
			if countErr != nil {
				log.Printf(
					"resync: get old contributor %q file count: %v",
					contributor.Name, countErr,
				)
				count = 1
			}
			contributorOldFileSessions[i] = count
		}
		rebuildOldFileSessions = localOldFileSessions
		for _, count := range contributorOldFileSessions {
			rebuildOldFileSessions += count
		}
	}

	// Clean up stale temp DB from a prior crash.
	removeTempDB(tempPath)

	// 1. Snapshot and clear in-memory skip cache. The
	// snapshot is restored on early failure so behavior
	// matches the persisted DB until the next restart.
	e.skipMu.Lock()
	savedSkipCache := e.skipCache
	savedSkipHashKeys := e.skipHashKeys
	e.skipCache = make(map[string]int64)
	e.skipCacheDirty = true
	e.skipHashKeys = make(map[string]string)
	e.skipMu.Unlock()
	// A resync parses every source again, so the failures it records replace
	// the old ones. They describe source files, not archive rows, and stay
	// valid when the build is discarded; they are not restored with the rest.
	e.failures.Reset()

	restoreSkipCache := func() {
		e.skipMu.Lock()
		e.skipCache = savedSkipCache
		e.skipCacheDirty = true
		e.skipHashKeys = savedSkipHashKeys
		e.skipMu.Unlock()
		if ctx.Err() == nil {
			e.flushFailureCacheInto(ctx, origDB)
		}
	}

	// 2. Open a fresh DB at the temp path.
	reportResyncPhase(
		PhasePreparingResync,
		"Opening temporary database",
		"",
	)
	newDB, err := db.OpenWithArchiveContent(ctx, tempPath, e.db.ArchiveContent())
	if err != nil {
		log.Printf("resync: open temp db: %v", err)
		restoreSkipCache()
		stats = SyncStats{
			Aborted: true,
			Warnings: []string{
				"resync failed: " + err.Error(),
			},
		}
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}
	newDB.SetToolResultImages(e.toolResultImages)
	newDB.SetAssetsDir(e.db.AssetsDir())
	if err := newDB.CopyArchiveIdentityFrom(ctx, origPath); err != nil {
		log.Printf("resync: preserve archive identity: %v", err)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		stats = SyncStats{
			Aborted: true,
			Warnings: []string{
				"resync failed: preserve archive identity: " + err.Error(),
			},
		}
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return
	}
	if err := newDB.CopyAttachedArchiveStateFrom(ctx, origPath); err != nil {
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		return SyncStats{Aborted: true}, err
	}

	// 2b. Copy excluded session IDs from the old DB so that
	// UpsertSession skips permanently deleted sessions during
	// the sync. This must happen before syncAllLocked.
	reportResyncPhase(
		PhasePreparingResync,
		"Copying deletion state into temporary database",
		"",
	)
	if err := newDB.CopyExcludedSessionsFrom(ctx, origPath); err != nil {
		log.Printf("resync: pre-sync copy excluded sessions: %v", err)
		// Non-fatal: worst case, deleted sessions reappear.
	}
	if err := newDB.CopyCommitTrailersFrom(ctx, origPath); err != nil {
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		return SyncStats{Aborted: true}, err
	}
	trashedCopied := 0
	var copiedSessionIDs []string
	if ids, err := newDB.CopyTrashedDataFrom(ctx, origPath); err != nil {
		log.Printf("resync: pre-sync copy trashed sessions: %v", err)
		// Non-fatal: worst case, trashed sessions are reparsed
		// and then re-marked as trashed by metadata copy.
	} else if len(ids) > 0 {
		copiedSessionIDs = append(copiedSessionIDs, ids...)
		trashedCopied = len(ids)
		log.Printf("resync: pre-sync copied %d trashed sessions", len(ids))
	}
	// The temp DB is not swapped into production until the end,
	// so avoid per-row FTS trigger work during the bulk load and
	// rebuild the index once all message rows are final.
	ftsDropped := false
	if newDB.HasFTS(ctx) {
		tFTS := time.Now()
		reportResyncPhase(
			PhasePreparingResync,
			"Disabling temporary search index updates",
			"",
		)
		if err := newDB.DropFTS(ctx); err != nil {
			log.Printf("resync: drop temp fts: %v", err)
			newDB.Close()
			removeTempDB(tempPath)
			restoreSkipCache()
			stats = SyncStats{
				Aborted: true,
				Warnings: []string{
					"resync failed: drop temp fts: " +
						err.Error(),
				},
			}
			e.mu.Lock()
			e.lastSyncStats = stats
			e.mu.Unlock()
			return stats, err
		}
		ftsDropped = true
		log.Printf(
			"resync: drop temp fts: %s",
			time.Since(tFTS).Round(time.Millisecond),
		)
	}

	// Same trade as FTS: the usage, activity, and tool-result indexes are
	// pure derived state, so skip their per-row maintenance during the
	// bulk load and build each B-tree once before the swap. Read-only
	// opens require these indexes, so the rebuild must succeed before
	// the replacement is installed.
	if err := newDB.DropBulkImportIndexes(ctx); err != nil {
		log.Printf("resync: drop temp usage indexes: %v", err)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		stats = SyncStats{
			Aborted: true,
			Warnings: []string{
				"resync failed: drop temp usage indexes: " +
					err.Error(),
			},
		}
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}

	// 3. Point engine at newDB and sync into it. Report discovery as its
	// own phase first: syncAllLocked walks every source before emitting
	// its first syncing event, and on a large archive that walk takes
	// minutes. Without this marker the progress printer credits that
	// silent time to the preceding (instant) "Disabling ..." phase.
	// The archive is write-barriered for the whole rebuild, so one snapshot of
	// its source ownership serves every parsed file. Querying the archive
	// per file would open cold reader connections while workers are busy,
	// which SQLite's busy handler turns into sleeps on every open.
	archiveIndex, err := loadArchiveRebuildIndex(ctx, origDB, e.collisionPolicyAgents(opts.Contributors))
	if err != nil {
		log.Printf("resync: snapshot archive source ownership: %v", err)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		stats = SyncStats{
			Aborted: true,
			Warnings: []string{
				"resync failed: snapshot archive source ownership: " + err.Error(),
			},
		}
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}
	e.archiveStore = origDB
	e.archiveRebuildIndex = archiveIndex
	deferredSourceCwd := newSourceCwdReconciliationBatch()
	e.deferredSourceCwd = deferredSourceCwd
	defer func() { e.deferredSourceCwd = nil }()
	deferredTitles := make(map[string]*string)
	e.deferredNativeTitles = deferredTitles
	defer func() { e.deferredNativeTitles = nil }()
	e.db = newDB
	reportResyncPhase(
		PhaseDiscovering,
		"Discovering sessions",
		"",
	)
	stats = e.syncAllLocked(
		ctx, reportResyncProgress, time.Time{}, nil, syncWriteBulk, true, false,
	)
	e.db = origDB // restore immediately
	e.archiveStore = nil
	e.archiveRebuildIndex = nil
	pendingTombstoned += stats.Tombstoned
	stats.Tombstoned = 0
	pendingLinksUpdated += stats.LinksUpdated
	stats.LinksUpdated = 0
	e.phaseStats.Log("resync")
	if opts.includePhaseDiagnostics {
		stats.RebuildPhases = append(stats.RebuildPhases,
			phaseSnapshot("local", &e.phaseStats))
	}
	localStats := stats
	if stats.Deferred > 0 {
		warning := fmt.Sprintf("resync: %s retry", countNoun(stats.Deferred, "session needs", "sessions need"))
		if sample := stats.retryCauseSample(); sample != "" {
			warning += "; why: " + sample
		}
		log.Print(warning)
		stats.Warnings = append(stats.Warnings, warning)
	}
	if stats.Aborted || ctx.Err() != nil {
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		cause := passAbortCause(ctx, stats)
		stats.abort(cause)
		stats.Warnings = append(stats.Warnings, fmt.Sprintf(
			"resync aborted: %d synced, %d failed: %s",
			stats.Synced, stats.Failed, cause,
		))
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		return stats, nil
	}

	for contributorIndex, contributor := range opts.Contributors {
		if contributor.Started != nil {
			contributor.Started()
		}
		contributorEngine := NewEngine(ctx, newDB, contributor.Config)
		contributorEngine.archiveStore = origDB
		contributorEngine.archiveRebuildIndex = archiveIndex
		contributorEngine.deferredSourceCwd = deferredSourceCwd
		contributorEngine.deferredNativeTitles = deferredTitles
		contributorEngine.forceFullParse = contributor.ForceParse ||
			contributor.ForceFullParseAfterCache
		contributorEngine.forceFullParseAllowsCache =
			contributor.ForceFullParseAfterCache && !contributor.ForceParse
		contributorProgress := func(p Progress) {
			if contributor.Progress != nil {
				p = contributor.Progress(p)
			}
			if p.Phase != PhaseFinalizing {
				p.SessionsDone += stats.TotalSessions
				p.SessionsTotal += stats.TotalSessions
				p.MessagesIndexed += stats.messagesIndexed
			}
			reportResyncProgress(p)
		}
		var contributorStats SyncStats
		var contributorErr error
		if contributor.Run != nil {
			contributorStats, contributorErr = contributor.Run(ctx, contributorEngine, newDB, contributorProgress)
		} else {
			contributorStats = contributorEngine.syncAllLocked(ctx, contributorProgress, time.Time{}, nil, syncWriteBulk, true, contributor.ForceParse)
		}
		e.failures.Merge(&contributorEngine.failures)
		contributorEngine.phaseStats.Log("resync contributor " + contributor.Name)
		phase := phaseSnapshot(contributor.Name, &contributorEngine.phaseStats)
		contributorSafetyReason := resyncSwapAbortReason(
			contributorStats,
			contributorOldFileSessions[contributorIndex],
			0,
		)
		contributorSafetyAbort := contributorSafetyReason != ""
		if contributorSafetyAbort {
			contributorStats.abort(keptArchive + contributorSafetyReason)
		}
		mergeSyncStats(&stats, contributorStats)
		pendingTombstoned += stats.Tombstoned
		stats.Tombstoned = 0
		pendingLinksUpdated += stats.LinksUpdated
		stats.LinksUpdated = 0
		if opts.includePhaseDiagnostics {
			stats.RebuildPhases = append(stats.RebuildPhases, phase)
		}
		runAfterFailure := func() error {
			defer e.failures.Merge(&contributorEngine.failures)
			if restoreActiveWriterOnAbort && origDB.WriterClosed() {
				if err := origDB.ReopenWriter(); err != nil {
					return fmt.Errorf(
						"reopen active writer for contributor failure: %w", err,
					)
				}
			}
			if contributor.AfterFailure != nil {
				return contributor.AfterFailure(contributorEngine, origDB)
			}
			return nil
		}
		if contributorErr != nil || !contributorStats.ProcessingComplete() || stats.Aborted || ctx.Err() != nil ||
			contributorSafetyAbort {
			failureHookErr := errors.Join(contributorErr, runAfterFailure())
			contributorEngine.Close()
			if contributor.Finished != nil {
				contributor.Finished(
					contributorStats, errors.Join(ctx.Err(), failureHookErr),
				)
			}
			newDB.Close()
			removeTempDB(tempPath)
			restoreSkipCache()
			cause := contributorAbortCause(
				ctx, contributorErr, contributorStats, stats, contributorSafetyReason,
			)
			// The contributor's outcome replaces any cause merged from its stats.
			stats.Aborted = true
			stats.abortCause = fmt.Sprintf("sync source %q did not finish: %s", contributor.Name, cause)
			stats.Warnings = append(stats.Warnings, "resync aborted: "+stats.abortCause)
			e.mu.Lock()
			e.lastSyncStats = stats
			e.mu.Unlock()
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			if failureHookErr != nil {
				return stats, &RebuildContributorError{
					Contributor: contributor.Name,
					Err:         failureHookErr,
				}
			}
			return stats, nil
		}
		if contributor.AfterSync != nil {
			if err := contributor.AfterSync(contributorEngine, newDB); err != nil {
				failureErr := errors.Join(err, runAfterFailure())
				contributorEngine.Close()
				if contributor.Finished != nil {
					contributor.Finished(contributorStats, failureErr)
				}
				newDB.Close()
				removeTempDB(tempPath)
				restoreSkipCache()
				stats.Aborted = true
				stats.Warnings = append(stats.Warnings, fmt.Sprintf(
					"resync contributor %q failed: %v",
					contributor.Name, err,
				))
				e.mu.Lock()
				e.lastSyncStats = stats
				e.mu.Unlock()
				return stats, &RebuildContributorError{
					Contributor: contributor.Name,
					Err:         failureErr,
				}
			}
		}
		contributorEngine.Close()
		if contributor.Finished != nil {
			contributor.Finished(contributorStats, nil)
		}
	}

	swapAbortReason := ""
	if len(opts.Contributors) > 0 ||
		len(opts.UnavailableContributorIDPrefixes) > 0 {
		// Evaluate local safety after contributors so a contributor's own
		// cancellation or failure remains the reported abort reason. Trash
		// copied from the old archive cannot make an empty local pass safe.
		if reason := resyncSwapAbortReason(
			localStats, localOldFileSessions, 0,
		); reason != "" {
			swapAbortReason = "in this machine's session folders, " + reason
		}
	}
	if swapAbortReason == "" {
		swapAbortReason = resyncSwapAbortReason(stats, rebuildOldFileSessions, trashedCopied)
	}
	if swapAbortReason != "" {
		cause := keptArchive + swapAbortReason
		log.Printf(
			"resync: aborting swap, %d synced / %d failed / %d total: %s",
			stats.Synced, stats.Failed, stats.TotalSessions, cause,
		)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		stats.Aborted = true
		stats.abortCause = cause
		stats.Warnings = append(stats.Warnings, fmt.Sprintf(
			"resync aborted: %d synced, %d failed: %s",
			stats.Synced, stats.Failed, cause,
		))

		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		return stats, nil
	}

	// Copy preserved state from the original into the replacement. The caller
	// guarantees the original is quiesced: the in-process swap owns the final
	// CloseConnections, and the worker path runs behind the daemon's write
	// barrier. These ATTACH reads therefore see a consistent committed snapshot
	// without the build closing the original here.
	//
	// Re-copy excluded session IDs to catch permanent deletes recorded during
	// the sync window, and purge any sessions synced into newDB before the
	// exclusion was recorded.
	reportResyncPhase(
		PhaseCopyingMetadata,
		"Copying sync metadata",
		"",
	)
	if err := newDB.CopyExcludedSessionsFrom(ctx, origPath); err != nil {
		log.Printf("resync: post-sync copy excluded sessions: %v", err)
	}
	if err := newDB.PurgeExcludedSessions(ctx); err != nil {
		log.Printf("resync: purge excluded sessions: %v", err)
	}
	if err := newDB.CopySyncStateFrom(ctx, origPath); err != nil {
		log.Printf("resync: copy sync state: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"sync state copy failed, aborting swap: "+err.Error(),
		)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}

	// Insights and recall entries carry transcript-derived text, which a
	// usage-only archive promises not to store, so those copies are skipped
	// there. This also applies when a full archive is rebuilt under the
	// usage policy for the first time.
	copyDerivedText := !newDB.ArchiveContent().UsageOnly()

	// Copy insights into newDB from the quiesced old DB file.
	tInsights := time.Now()
	reportResyncPhase(
		PhaseCopyingMetadata,
		"Copying cached insights",
		"",
	)
	if copyDerivedText {
		if err := newDB.CopyInsightsFrom(ctx, origPath); err != nil {
			log.Printf("resync: copy insights: %v", err)
			stats.Aborted = true
			stats.Warnings = append(stats.Warnings,
				"insights copy failed, aborting swap: "+
					err.Error(),
			)
			newDB.Close()
			removeTempDB(tempPath)
			restoreSkipCache()
			e.mu.Lock()
			e.lastSyncStats = stats
			e.mu.Unlock()
			return stats, err
		}
		log.Printf(
			"resync: copy insights: %s",
			time.Since(tInsights).Round(time.Millisecond),
		)
	}
	// Digest snapshots and Markdown may contain transcript text. Preserve
	// them only when the rebuilt archive stores that content.
	if copyDerivedText {
		if err := newDB.CopyFrictionStateFrom(ctx, origPath); err != nil {
			log.Printf("resync: copy friction state: %v", err)
			stats.Aborted = true
			stats.Warnings = append(stats.Warnings,
				"friction state copy failed, aborting swap: "+err.Error(),
			)
			newDB.Close()
			removeTempDB(tempPath)
			restoreSkipCache()
			e.mu.Lock()
			e.lastSyncStats = stats
			e.mu.Unlock()
			return stats, err
		}
	}

	// Copy model pricing so usage costs survive the swap. The
	// startup seed only runs once per daemon lifetime, so a
	// resync triggered through the sync API would otherwise
	// leave the rebuilt DB with an empty pricing table and
	// every usage cost reading $0.00 until the next restart.
	// Non-fatal: a failed copy degrades cost display but does
	// not justify aborting the resync, and the next daemon
	// startup re-seeds pricing.
	if err := newDB.CopyModelPricingFrom(ctx, origPath); err != nil {
		log.Printf("resync: copy model pricing: %v", err)
		stats.Warnings = append(stats.Warnings,
			"model pricing copy failed; usage costs show as $0.00 "+
				"until the next daemon restart re-seeds pricing: "+
				err.Error(),
		)
	}

	// Copy orphaned sessions (source files gone) from the
	// old DB so archived data is preserved. Failure aborts
	// the swap to avoid losing archived sessions.
	reportResyncPhase(
		PhaseCopyingOrphans,
		"Copying archived sessions",
		"",
	)
	orphaned, err := newDB.CopyOrphanedDataFromExcluding(
		ctx, origPath, stats.parserExcludedIDs,
	)
	if err != nil {
		log.Printf("resync: copy orphaned sessions: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"orphaned session copy failed, aborting swap: "+
				err.Error(),
		)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}
	if copyDerivedText && !newDB.ArchiveContent().OmitsToolContent() {
		if err := newDB.CopyImageTextWithOccurrencesFrom(ctx, origPath); err != nil {
			stats.Aborted = true
			stats.Warnings = append(stats.Warnings, "image text copy failed, aborting swap: "+err.Error())
			newDB.Close()
			removeTempDB(tempPath)
			restoreSkipCache()
			e.mu.Lock()
			e.lastSyncStats = stats
			e.mu.Unlock()
			return stats, err
		}
	}

	stats.OrphanedCopied = len(orphaned)
	copiedSessionIDs = append(copiedSessionIDs, orphaned...)
	abortPreserved := func(err error) (SyncStats, error) {
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings, fmt.Sprintf("resync aborted: %v", err))
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}
	var demoteIDs []string
	for _, id := range stats.deferredPreservedIDs {
		stored, readErr := newDB.GetSessionFull(ctx, id)
		if readErr != nil || stored == nil {
			if readErr == nil {
				readErr = errors.New("preserved session missing")
			}
			return abortPreserved(fmt.Errorf("recover deferred session %s: %w", id, readErr))
		}
		if stored.DataVersion >= db.CurrentDataVersion() {
			demoteIDs = append(demoteIDs, id)
		}
	}
	if err := newDB.SetSessionDataVersionsContext(ctx, demoteIDs, max(db.CurrentDataVersion()-1, 0)); err != nil {
		return abortPreserved(fmt.Errorf("mark preserved deferred sessions for retry: %w", err))
	}
	for id, name := range deferredTitles {
		if err := newDB.RefreshNativeSessionName(ctx, id, name); err != nil {
			stats.RecordFailed()
			stats.Aborted = true
			newDB.Close()
			removeTempDB(tempPath)
			restoreSkipCache()
			e.mu.Lock()
			e.lastSyncStats = stats
			e.mu.Unlock()
			return stats, fmt.Errorf("apply deferred native title: %w", err)
		}
	}
	deferredCwdUpdated, err := e.applyDeferredSourceCwd(ctx,
		newDB, deferredSourceCwd,
	)
	if err != nil {
		log.Printf("resync: apply deferred source cwd: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"deferred source cwd reconciliation failed, aborting swap: "+
				err.Error(),
		)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}
	stats.RecordCwdUpdated(deferredCwdUpdated)
	e.deferredSourceCwd = nil

	// Scan once every session and its cwd have been restored.
	e.db = newDB
	pendingLinksUpdated += e.reconcileCommitTrailersLocked(ctx)
	e.db = origDB

	// Re-link subagent sessions after orphan copy so copied
	// tool_calls.subagent_session_id references are resolved.
	var repaired int
	if len(orphaned) > 0 {
		reportResyncPhase(
			PhaseCopyingOrphans,
			"Relinking archived subagent sessions",
			"",
		)
		repaired, err = newDB.LinkSubagentSessionsContext(ctx)
		pendingLinksUpdated += repaired
	}

	// CopySyncStateFrom runs after the fresh archive's normal linking pass so
	// pending hierarchy work from the original must be consumed explicitly.
	// Wait until orphan restoration is complete so every queued session and
	// copied spawn edge is present. A failed repair leaves hierarchy state
	// uncertain and must abort before the replacement can be installed.
	if err == nil {
		repaired, err = newDB.RepairQueuedSubagentParentsContext(ctx, func(done, total int) {
			e.reportSubagentRepairProgress(reportResyncProgress, done, total)
		})
		pendingLinksUpdated += repaired
	}
	if err != nil {
		log.Printf("resync: repair copied subagent parents: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"hierarchy repair failed, aborting swap: "+err.Error(),
		)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}

	// Copy recall entries and their evidence from the quiesced old DB.
	// The fresh DB is built from source files, which never contain
	// recall entries, so without this every accepted entry is lost on
	// resync. Runs after the orphan copy so referenced sessions exist.
	// Failure aborts the swap to avoid destroying the recall archive.
	if copyDerivedText {
		if err := newDB.CopyRecallEntriesFrom(ctx, origPath); err != nil {
			log.Printf("resync: copy recall entries: %v", err)
			stats.Aborted = true
			stats.Warnings = append(stats.Warnings,
				"recall copy failed, aborting swap: "+err.Error(),
			)
			newDB.Close()
			removeTempDB(tempPath)
			restoreSkipCache()
			e.mu.Lock()
			e.lastSyncStats = stats
			e.mu.Unlock()
			return stats, err
		}
	}

	// Merge user-managed data and trustworthy immutable project-identity
	// snapshots from the old DB. Snapshot copy happens after parsing because the
	// destination rows reference freshly parsed sessions. Pre-source-snapshot
	// archives retain the fresh parse results instead. Failure must abort the
	// swap: a fresh database without valid snapshots could no longer export
	// stable identity after a source working directory disappears.
	reportResyncPhase(
		PhaseCopyingMetadata,
		"Copying user-managed session metadata",
		"",
	)
	if err := newDB.CopySessionMetadataFrom(ctx, origPath); err != nil {
		log.Printf("resync: copy session metadata: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"session metadata copy failed, aborting swap: "+err.Error(),
		)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}
	if _, err := newDB.RestoreSessionProjectsFromIdentitySnapshots(
		ctx,
	); err != nil {
		log.Printf("resync: restore session project identity: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"session project identity restore failed, aborting swap: "+err.Error(),
		)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}
	mappingMachines, err := ops.listActiveWorktreeMappingMachines(ctx, newDB)
	if err != nil {
		warning := "worktree mapping machine discovery failed, aborting swap: " +
			err.Error()
		log.Printf("resync: %s", warning)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings, warning)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, fmt.Errorf(
			"discovering active worktree mapping machines: %w", err,
		)
	}
	for _, machine := range mappingMachines {
		if _, applyErr := ops.applyWorktreeMappings(
			ctx, newDB, machine,
		); applyErr != nil {
			warning := fmt.Sprintf(
				"worktree mapping apply failed for machine %q, aborting swap: %v",
				machine, applyErr,
			)
			log.Printf("resync: %s", warning)
			stats.Aborted = true
			stats.Warnings = append(stats.Warnings, warning)
			newDB.Close()
			removeTempDB(tempPath)
			restoreSkipCache()
			e.mu.Lock()
			e.lastSyncStats = stats
			e.mu.Unlock()
			return stats, fmt.Errorf(
				"applying worktree mappings for machine %q: %w",
				machine, applyErr,
			)
		}
	}

	// Metadata restoration deliberately copies user-owned deletion state from
	// the original archive. Reconcile archive-only Claude members afterwards so
	// an available legacy fork cannot overwrite the source-missing state that
	// this rebuild just established.
	deferredTombstoned, err := e.reconcileCopiedSourceMissingMembers(
		ctx, newDB, origPath, stats.sourceMissingArchiveMembers,
	)
	if err != nil {
		log.Printf("resync: mark copied sessions source-missing: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"copied source-missing reconciliation failed, aborting swap: "+
				err.Error(),
		)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}
	pendingTombstoned += deferredTombstoned

	// Reclassify is_automated across every row. Orphan-copied
	// rows carry is_automated values computed against the OLD
	// DB's classifier set; the temp DB's at-Open backfill ran on
	// an empty table and stamped the current hash, so without
	// this pass those rows would be permanently stuck with stale
	// flags. Non-fatal: worst case, some sessions keep their
	// pre-resync classification until the next algorithm bump.
	reportResyncPhase(
		PhaseReclassifying,
		"Reclassifying sessions",
		"",
	)
	if err := newDB.ForceBackfillIsAutomated(ctx); err != nil {
		log.Printf("resync: reclassify is_automated: %v", err)
	}

	if newDB.ToolResultImages() != config.ToolResultImagesKeep {
		if err := newDB.ProjectToolImagesForSessions(ctx, copiedSessionIDs); err != nil {
			log.Printf("resync: project copied tool-result images: %v", err)
			stats.Aborted = true
			stats.Warnings = append(stats.Warnings,
				"copied tool-result image projection failed, aborting swap: "+err.Error(),
			)
			newDB.Close()
			removeTempDB(tempPath)
			restoreSkipCache()
			e.mu.Lock()
			e.lastSyncStats = stats
			e.mu.Unlock()
			return stats, err
		}
	}

	if !e.disableSignalRecompute {
		reportResyncPhase(PhaseReclassifying, "Recomputing archived session signals", "")
		e.db = newDB
		err := newDB.BackfillSignals(ctx, e.BackfillSignalComputer())
		e.db = origDB
		if err != nil {
			stats.Aborted = true
			stats.Warnings = append(stats.Warnings,
				"archived session signal recompute failed, aborting swap: "+err.Error(),
			)
			newDB.Close()
			removeTempDB(tempPath)
			restoreSkipCache()
			e.mu.Lock()
			e.lastSyncStats = stats
			e.mu.Unlock()
			return stats, err
		}
	}

	if ftsDropped {
		tFTS := time.Now()
		reportResyncPhase(
			PhaseRebuildingSearch,
			"Rebuilding search index",
			"Rebuilding the search index may take a while on large archives.",
		)
		if err := ops.rebuildFTS(ctx, newDB); err != nil {
			log.Printf("resync: rebuild fts: %v", err)
			stats.Aborted = true
			stats.Warnings = append(stats.Warnings,
				"fts rebuild failed, aborting swap: "+
					err.Error(),
			)
			newDB.Close()
			removeTempDB(tempPath)
			restoreSkipCache()
			if rerr := origDB.Reopen(); rerr != nil {
				log.Printf("resync: recovery reopen: %v", rerr)
			}
			e.mu.Lock()
			e.lastSyncStats = stats
			e.mu.Unlock()
			return stats, err
		}
		log.Printf(
			"resync: rebuild fts: %s",
			time.Since(tFTS).Round(time.Millisecond),
		)
	}

	tUsageIndexes := time.Now()
	reportResyncPhase(
		PhaseRebuildingSearch,
		"Rebuilding usage indexes",
		"",
	)
	if err := ops.rebuildUsageIndexes(ctx, newDB); err != nil {
		log.Printf("resync: rebuild usage indexes: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"usage index rebuild failed, aborting swap: "+
				err.Error(),
		)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		if rerr := origDB.Reopen(); rerr != nil {
			log.Printf("resync: recovery reopen: %v", rerr)
		}
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}
	log.Printf(
		"resync: rebuild usage indexes: %s",
		time.Since(tUsageIndexes).Round(time.Millisecond),
	)

	// Persist the fresh skip state into the replacement so the post-swap engine
	// loads warm state: this engine after an in-process swap, or the daemon
	// after a worker build. Then checkpoint and close the replacement so its
	// main file is complete on disk for the caller's rename. A failed
	// checkpoint or close means committed rows may sit only in its WAL; the
	// swap renames only the main file and deletes that WAL, so proceeding
	// would install an archive missing those rows. Abort instead.
	e.persistSkipCacheInto(ctx, newDB)
	tCheckpoint := time.Now()
	reportResyncPhase(PhaseFinalizing, "Checkpointing rebuilt database", "")
	if err := newDB.CheckpointWALTruncate(ctx); err != nil {
		log.Printf("resync: checkpoint replacement db: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"replacement checkpoint failed, aborting swap: "+err.Error(),
		)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}
	log.Printf(
		"resync: checkpoint replacement db: %s",
		time.Since(tCheckpoint).Round(time.Millisecond),
	)
	reportResyncPhase(PhaseFinalizing, "Closing rebuilt database", "")
	if err := newDB.Close(); err != nil {
		log.Printf("resync: close replacement db: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"replacement close failed, aborting swap: "+err.Error(),
		)
		removeTempDB(tempPath)
		restoreSkipCache()
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}
	if err := ctx.Err(); err != nil {
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"resync canceled before swap: "+err.Error(),
		)
		removeTempDB(tempPath)
		restoreSkipCache()
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}
	stats.Tombstoned = pendingTombstoned
	stats.LinksUpdated = pendingLinksUpdated
	return stats, nil
}
