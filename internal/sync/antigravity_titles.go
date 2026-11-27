package sync

import (
	"context"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/parser"
)

func isAntigravitySummariesEventPath(agent parser.AgentType, path string) bool {
	return (agent == parser.AgentAntigravity || agent == parser.AgentAntigravityCLI) &&
		strings.TrimSuffix(filepath.Base(path), "-wal") == parser.AntigravitySummariesDBName
}

// classifyAntigravitySummariesPath maps a conversation_summaries.db (or -wal)
// change to only the stored sessions under that store whose non-title summary
// fingerprint digest (workspace or parent) no longer matches. Title-only edits
// keep every stored digest unchanged and return no files.
//
// A non-context summary store read failure is returned separately in
// summaryReadErr so callers can retry or retain the store without blocking
// unrelated agents in the same batch.
func (e *Engine) classifyAntigravitySummariesPath(
	ctx context.Context,
	agent parser.AgentType,
	provider parser.Provider,
	roots []string,
	path string,
) (files []parser.DiscoveredFile, summaryReadErr, fallbackErr, err error) {
	store := strings.TrimSuffix(filepath.Clean(path), "-wal")
	storeRoot := filepath.Dir(store)
	if _, ok := parser.SQLiteContainerPathForEvent(
		storeRoot, path, parser.AntigravitySummariesDBName, true,
	); !ok {
		return nil, nil, nil, nil
	}
	owned := false
	for _, root := range roots {
		if agent == parser.AgentAntigravityCLI {
			if parser.AntigravityCLITitleStoreOwner(root, store) {
				owned = true
				break
			}
		} else if sameReconciliationSourcePath(root, storeRoot) {
			owned = true
			break
		}
	}
	if !owned {
		return nil, nil, nil, nil
	}
	parser.InvalidateAntigravitySummaries(storeRoot)
	readDigests := e.antigravitySummaryDigests
	if readDigests == nil {
		readDigests = parser.AntigravitySummaryDigests
	}
	digests, digestErr := readDigests(ctx, storeRoot)
	if digestErr != nil {
		if errors.Is(digestErr, context.Canceled) ||
			errors.Is(digestErr, context.DeadlineExceeded) {
			return nil, nil, nil, digestErr
		}
		log.Printf("%s summaries %s: %v", agent, store, digestErr)
		return nil, digestErr, nil, nil
	}
	if e.pathRewriter != nil && e.storedPathResolver == nil {
		return nil, nil, nil, nil
	}
	queryRoot := storeRoot
	if e.pathRewriter != nil {
		queryRoot = e.pathRewriter(storeRoot)
	}
	identities, listErr := e.db.ListSessionSummaryIdentitiesUnderPathContext(
		ctx, string(agent), queryRoot,
	)
	if listErr != nil {
		if errors.Is(listErr, context.Canceled) ||
			errors.Is(listErr, context.DeadlineExceeded) {
			return nil, nil, nil, listErr
		}
		return nil, nil, listErr, nil
	}
	if len(identities) == 0 {
		return nil, nil, nil, nil
	}
	sessionRoot := parser.AntigravitySessionRoot
	if agent == parser.AgentAntigravityCLI {
		sessionRoot = parser.AntigravityCLISessionRoot
	}
	prefix := e.idPrefix + string(agent) + ":"
	for _, stored := range identities {
		if !strings.HasPrefix(stored.ID, prefix) {
			continue
		}
		resolved, ok := e.resolveStoredNativePath(stored.FilePath)
		if !ok {
			continue
		}
		owner, ok := sessionRoot(resolved)
		if !ok || !sameReconciliationSourcePath(owner, storeRoot) {
			continue
		}
		rawID := strings.TrimPrefix(stored.ID, prefix)
		summaryID := rawID
		if agent == parser.AgentAntigravityCLI {
			summaryID = parser.AntigravityCLISummaryID(rawID)
		}
		if parser.AntigravityHashCoversSummary(stored.FileHash, digests[summaryID]) {
			continue
		}
		source, found, findErr := provider.FindSource(ctx, parser.FindSourceRequest{
			RawSessionID:       rawID,
			StoredFilePath:     resolved,
			RequireFreshSource: true,
		})
		if findErr != nil {
			if errors.Is(findErr, context.Canceled) ||
				errors.Is(findErr, context.DeadlineExceeded) {
				return nil, nil, nil, findErr
			}
			fallbackErr = errors.Join(fallbackErr, findErr)
			continue
		}
		if !found {
			continue
		}
		if file, ok := e.changedPathDiscoveredFile(provider, source); ok {
			files = append(files, file)
		}
	}
	return files, nil, fallbackErr, nil
}

// recordFailedAntigravitySummaryStoresLocked ensures a metadata-read failure
// during changed-path classification is never masked by a subsequent title-read
// success: the store's applied title state is cleared, watcher retries keep the
// store path, and stats records a failure so remote imports retain pending work.
func (e *Engine) recordFailedAntigravitySummaryStoresLocked(
	ctx context.Context,
	failedStores []string,
	failedTitleStores map[string]bool,
	stats *SyncStats,
) {
	if len(failedStores) == 0 {
		return
	}
	e.queueFailedAntigravitySummaryStoreRetriesLocked(ctx, failedStores)
	for _, store := range failedStores {
		if !failedTitleStores[store] {
			stats.RecordFailed()
		}
	}
}

func (e *Engine) queueFailedAntigravitySummaryStoreRetriesLocked(
	ctx context.Context,
	failedStores []string,
) {
	retry, hasRetry := ctx.Value(nativeTitleRetryContextKey{}).(*WatchBatch)
	for _, store := range failedStores {
		delete(e.appliedNativeTitles, store)
		parser.InvalidateSQLiteTitles(store)
		parser.InvalidateAntigravitySummaries(filepath.Dir(store))
		if hasRetry {
			retry.Paths = watchAppendUniqueString(retry.Paths, store)
		}
	}
}

func antigravityTitleRoots(agent parser.AgentType, configured []string) []string {
	if agent != parser.AgentAntigravityCLI {
		return configured
	}
	var roots []string
	seen := make(map[string]struct{}, len(configured))
	for _, root := range configured {
		expanded, _ := parser.ExpandAntigravityCLIRoot(root)
		for _, candidate := range expanded {
			if _, dup := seen[candidate]; dup {
				continue
			}
			seen[candidate] = struct{}{}
			roots = append(roots, candidate)
		}
	}
	return roots
}

func (e *Engine) SnapshotAntigravityTitleStates() map[string]parser.SQLiteContainerState {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	states := make(map[string]parser.SQLiteContainerState)
	for _, agent := range []parser.AgentType{parser.AgentAntigravity, parser.AgentAntigravityCLI} {
		for _, root := range antigravityTitleRoots(agent, e.sources().agentDirs[agent]) {
			path := filepath.Join(root, parser.AntigravitySummariesDBName)
			if applied, ok := e.appliedNativeTitles[path]; ok {
				states[path] = applied.state
			}
		}
	}
	return states
}

// Mirror snapshots change only on extraction, so persisted states can seed a delta engine.
func (e *Engine) InjectAntigravityTitleStates(states map[string]parser.SQLiteContainerState) {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	if e.appliedNativeTitles == nil {
		e.appliedNativeTitles = make(map[string]appliedNativeTitleStore)
	}
	now := time.Now()
	for _, agent := range []parser.AgentType{parser.AgentAntigravity, parser.AgentAntigravityCLI} {
		for _, root := range antigravityTitleRoots(agent, e.sources().agentDirs[agent]) {
			path := filepath.Join(root, parser.AntigravitySummariesDBName)
			delete(e.appliedNativeTitles, path)
			if state, ok := states[path]; ok {
				e.appliedNativeTitles[path] = appliedNativeTitleStore{state: state, readAt: now}
			}
		}
	}
}
