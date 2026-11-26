package sync

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/parser"
)

// classifyAntigravitySummariesPath maps a conversation_summaries.db (or -wal)
// change to only the stored sessions under that store whose non-title summary
// fingerprint digest (workspace or parent) no longer matches. Title-only edits
// keep every stored digest unchanged and return no files.
func (e *Engine) classifyAntigravitySummariesPath(
	ctx context.Context,
	agent parser.AgentType,
	provider parser.Provider,
	roots []string,
	path string,
) (files []parser.DiscoveredFile, fallback bool, err error) {
	store := strings.TrimSuffix(filepath.Clean(path), "-wal")
	storeRoot := filepath.Dir(store)
	if _, ok := parser.SQLiteContainerPathForEvent(
		storeRoot, path, parser.AntigravitySummariesDBName, true,
	); !ok {
		return nil, false, nil
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
		return nil, false, nil
	}
	parser.InvalidateAntigravitySummaries(storeRoot)
	digests, digestErr := parser.AntigravitySummaryDigests(ctx, storeRoot)
	if digestErr != nil {
		if errors.Is(digestErr, context.Canceled) ||
			errors.Is(digestErr, context.DeadlineExceeded) {
			return nil, false, digestErr
		}
		// An unreadable store cannot supply workspace or parent updates;
		// native-title reconciliation surfaces the store read failure.
		return nil, false, nil
	}
	records, listErr := e.db.ListSessionPathRecordsForAgents(ctx, []string{string(agent)})
	if listErr != nil {
		if errors.Is(listErr, context.Canceled) ||
			errors.Is(listErr, context.DeadlineExceeded) {
			return nil, false, listErr
		}
		return nil, true, nil
	}
	sessionRoot := parser.AntigravitySessionRoot
	if agent == parser.AgentAntigravityCLI {
		sessionRoot = parser.AntigravityCLISessionRoot
	}
	prefix := e.idPrefix + string(agent) + ":"
	ids := make([]string, 0, len(records))
	localPaths := make(map[string]string, len(records))
	for _, rec := range records {
		if !strings.HasPrefix(rec.ID, prefix) {
			continue
		}
		resolved, ok := e.resolveStoredNativePath(rec.FilePath)
		if !ok {
			continue
		}
		owner, ok := sessionRoot(resolved)
		if !ok || !sameReconciliationSourcePath(owner, storeRoot) {
			continue
		}
		ids = append(ids, rec.ID)
		localPaths[rec.ID] = resolved
	}
	if len(ids) == 0 {
		return nil, false, nil
	}
	slices.Sort(ids)
	identities, idErr := e.db.ListSessionWriteIdentitiesByID(ctx, ids)
	if idErr != nil {
		if errors.Is(idErr, context.Canceled) ||
			errors.Is(idErr, context.DeadlineExceeded) {
			return nil, false, idErr
		}
		return nil, true, nil
	}
	for _, id := range ids {
		stored, ok := identities[id]
		if !ok {
			continue
		}
		rawID := strings.TrimPrefix(id, prefix)
		summaryID := rawID
		if agent == parser.AgentAntigravityCLI {
			summaryID = parser.AntigravityCLISummaryID(rawID)
		}
		if parser.AntigravityHashCoversSummary(stored.FileHash, digests[summaryID]) {
			continue
		}
		source, found, findErr := provider.FindSource(ctx, parser.FindSourceRequest{
			RawSessionID:       rawID,
			StoredFilePath:     localPaths[id],
			RequireFreshSource: true,
		})
		if findErr != nil {
			if errors.Is(findErr, context.Canceled) ||
				errors.Is(findErr, context.DeadlineExceeded) {
				return nil, false, findErr
			}
			fallback = true
			continue
		}
		if !found {
			continue
		}
		if file, ok := e.changedPathDiscoveredFile(provider, source); ok {
			files = append(files, file)
		}
	}
	return files, fallback, nil
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
