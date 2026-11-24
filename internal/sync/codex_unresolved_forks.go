package sync

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"slices"
	gosync "sync"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// settleUnresolvedCodexForksContextKey marks background watcher and
// reconciliation passes where an unchanged Codex fork whose replay parent is
// still absent settles instead of keeping the retry loop alive forever.
// Explicit SyncAll passes still report those sessions in stats.Deferred while
// skipping redundant decompression and database rewrites.
type settleUnresolvedCodexForksContextKey struct{}

func withSettleUnresolvedCodexForks(ctx context.Context) context.Context {
	return context.WithValue(ctx, settleUnresolvedCodexForksContextKey{}, true)
}

func shouldSettleUnresolvedCodexForks(ctx context.Context) bool {
	settle, _ := ctx.Value(settleUnresolvedCodexForksContextKey{}).(bool)
	return settle
}

type unresolvedCodexForkEntry struct {
	signature verifiedSourceSignature
	parentID  string
	sessionID string
	mtime     int64
}

// unresolvedCodexForkTracker remembers Codex-format forks whose replay parent
// was absent on a completed parse so later passes can verify file stat and
// parent-rollout absence in memory instead of re-reading and rewriting the
// rollout forever. It also maps each missing parent thread ID to waiting child
// paths so parent arrival wakes the child immediately.
type unresolvedCodexForkTracker struct {
	mu              gosync.Mutex
	epoch           uint64
	settled         map[verifiedSourceKey]unresolvedCodexForkEntry
	waitersByParent map[string]map[verifiedSourceKey]struct{}
	waitersLoaded   bool
}

func (t *unresolvedCodexForkTracker) currentEpoch() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.epoch
}

func (t *unresolvedCodexForkTracker) lookup(
	key verifiedSourceKey, sig verifiedSourceSignature,
) (unresolvedCodexForkEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.settled[key]
	if !ok || entry.signature != sig {
		return unresolvedCodexForkEntry{}, false
	}
	return entry, true
}

func (t *unresolvedCodexForkTracker) record(
	key verifiedSourceKey,
	sig verifiedSourceSignature,
	epoch uint64,
	parentID, sessionID string,
	mtime int64,
) {
	if key.agent == "" || key.path == "" || parentID == "" {
		return
	}
	key.path = filepath.Clean(key.path)
	t.mu.Lock()
	defer t.mu.Unlock()
	if epoch != t.epoch {
		return
	}
	if prev, ok := t.settled[key]; ok && prev.parentID != parentID {
		t.removeWaiterLocked(prev.parentID, key)
	}
	if t.settled == nil {
		t.settled = make(map[verifiedSourceKey]unresolvedCodexForkEntry)
	}
	t.settled[key] = unresolvedCodexForkEntry{
		signature: sig,
		parentID:  parentID,
		sessionID: sessionID,
		mtime:     mtime,
	}
	t.addWaiterLocked(parentID, key)
}

func (t *unresolvedCodexForkTracker) resolve(agent parser.AgentType, path string) {
	if agent == "" || path == "" {
		return
	}
	key := verifiedSourceKey{agent: agent, path: filepath.Clean(path)}
	t.mu.Lock()
	defer t.mu.Unlock()
	if entry, ok := t.settled[key]; ok {
		t.removeWaiterLocked(entry.parentID, key)
		delete(t.settled, key)
		return
	}
	for parentID, waiters := range t.waitersByParent {
		if _, ok := waiters[key]; ok {
			t.removeWaiterLocked(parentID, key)
		}
	}
}

func (t *unresolvedCodexForkTracker) invalidate(key verifiedSourceKey) {
	if key.agent == "" || key.path == "" {
		return
	}
	key.path = filepath.Clean(key.path)
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.settled, key)
}

func (t *unresolvedCodexForkTracker) forgetPath(path string) {
	if path == "" {
		return
	}
	clean := filepath.Clean(path)
	t.mu.Lock()
	defer t.mu.Unlock()
	for key, entry := range t.settled {
		if key.path == clean {
			t.removeWaiterLocked(entry.parentID, key)
			delete(t.settled, key)
		}
	}
	for parentID, waiters := range t.waitersByParent {
		for key := range waiters {
			if key.path == clean {
				t.removeWaiterLocked(parentID, key)
			}
		}
	}
}

func (t *unresolvedCodexForkTracker) clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.epoch++
	t.settled = nil
	t.waitersByParent = nil
	t.waitersLoaded = false
}

func (t *unresolvedCodexForkTracker) addWaiterLocked(parentID string, key verifiedSourceKey) {
	if parentID == "" {
		return
	}
	if t.waitersByParent == nil {
		t.waitersByParent = make(map[string]map[verifiedSourceKey]struct{})
	}
	waiters := t.waitersByParent[parentID]
	if waiters == nil {
		waiters = make(map[verifiedSourceKey]struct{})
		t.waitersByParent[parentID] = waiters
	}
	waiters[key] = struct{}{}
}

func (t *unresolvedCodexForkTracker) removeWaiterLocked(parentID string, key verifiedSourceKey) {
	waiters := t.waitersByParent[parentID]
	if waiters == nil {
		return
	}
	delete(waiters, key)
	if len(waiters) == 0 {
		delete(t.waitersByParent, parentID)
	}
}

func (t *unresolvedCodexForkTracker) ensureWaitersLoaded(ctx context.Context, database *db.DB) {
	t.mu.Lock()
	loaded := t.waitersLoaded
	epoch := t.epoch
	t.mu.Unlock()
	if loaded || database == nil {
		return
	}
	sources, err := database.ListUnresolvedCodexForkSources(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("list unresolved Codex forks: %v", err)
		}
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.waitersLoaded || t.epoch != epoch {
		return
	}
	for _, src := range sources {
		if src.Agent == "" || src.FilePath == "" || src.ParentID == "" {
			continue
		}
		t.addWaiterLocked(src.ParentID, verifiedSourceKey{
			agent: parser.AgentType(src.Agent),
			path:  filepath.Clean(src.FilePath),
		})
	}
	t.waitersLoaded = true
}

func (t *unresolvedCodexForkTracker) waitersForParent(
	ctx context.Context, database *db.DB, parentID string,
) []verifiedSourceKey {
	if parentID == "" {
		return nil
	}
	t.ensureWaitersLoaded(ctx, database)
	t.mu.Lock()
	defer t.mu.Unlock()
	waiters := t.waitersByParent[parentID]
	if len(waiters) == 0 {
		return nil
	}
	keys := make([]verifiedSourceKey, 0, len(waiters))
	for key := range waiters {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b verifiedSourceKey) int {
		if a.path != b.path {
			if a.path < b.path {
				return -1
			}
			return 1
		}
		if a.agent < b.agent {
			return -1
		}
		if a.agent > b.agent {
			return 1
		}
		return 0
	})
	return keys
}

// unresolvedCodexForkSkip short-circuits an unchanged Codex-format fork whose
// replay parent is still absent on disk. Background watcher and reconciliation
// passes settle it without returning a deferred retry error; explicit SyncAll
// passes keep reporting it in stats.Deferred without re-reading or rewriting
// the rollout.
func (e *Engine) unresolvedCodexForkSkip(
	ctx context.Context,
	provider parser.Provider,
	file parser.DiscoveredFile,
	capture verifiedSourceCapture,
	wantMtime int64,
) (processResult, bool) {
	if e.archiveStore != nil {
		return processResult{}, false
	}
	entry, ok := e.unresolvedCodexForks.lookup(capture.key, capture.signature)
	if !ok {
		return processResult{}, false
	}
	project, dataVersion, storedSize, storedMtime, found := e.db.GetSourceRepairStateByAgentPath(
		ctx, capture.key.path, string(file.Agent),
	)
	staleVersion := max(db.CurrentDataVersion()-1, 0)
	if !found || parser.NeedsProjectReparse(project) ||
		storedSize != capture.signature.size ||
		storedMtime != wantMtime ||
		dataVersion != staleVersion {
		e.unresolvedCodexForks.invalidate(capture.key)
		return processResult{}, false
	}
	checker, ok := provider.(parser.CodexParentRolloutChecker)
	if !ok {
		return processResult{}, false
	}
	hasParent, err := checker.HasCodexParentRollout(ctx, capture.key.path, entry.parentID)
	if err != nil || hasParent {
		e.unresolvedCodexForks.invalidate(capture.key)
		return processResult{}, false
	}
	if shouldSettleUnresolvedCodexForks(ctx) {
		return processResult{
			skip:        true,
			mtime:       wantMtime,
			noCacheSkip: true,
		}, true
	}
	return processResult{
		skip:          true,
		mtime:         wantMtime,
		noCacheSkip:   true,
		deferredCount: 1,
		retryCauses: []retryCause{{
			sessionID: entry.sessionID,
			path:      file.Path,
			reason:    "could not read the Codex session it was forked from (" + entry.parentID + ")",
		}},
	}, true
}

func (e *Engine) recordUnresolvedCodexForksAfterWrite(
	pending []pendingWrite, written []bool,
) {
	if e.archiveStore != nil {
		return
	}
	for i, pw := range pending {
		if i >= len(written) || !written[i] || pw.sess.CodexFork == nil {
			continue
		}
		if pw.sess.CodexFork.Resolved {
			e.unresolvedCodexForks.resolve(pw.sess.Agent, pw.sess.File.Path)
			continue
		}
		if pw.verifiedStateOK && pw.sess.CodexFork.ParentID != "" {
			e.unresolvedCodexForks.record(
				pw.verifiedCapture.key,
				pw.verifiedCapture.signature,
				pw.unresolvedForkEpoch,
				pw.sess.CodexFork.ParentID,
				pw.sess.ID,
				pw.verifiedMtime,
			)
		}
	}
}

// appendWaitingCodexForkPaths expands a changed-path batch with any unresolved
// Codex fork whose missing replay parent just arrived on disk.
func (e *Engine) appendWaitingCodexForkPaths(ctx context.Context, paths []string) []string {
	if e.pathRewriter != nil || len(paths) == 0 {
		return paths
	}
	var extra []string
	seenParent := make(map[string]struct{})
	for _, p := range paths {
		key := parser.CodexSessionUUIDFromFilename(filepath.Base(p))
		if key == "" || key != parser.CodexThreadIDFromSessionKey(key) {
			continue
		}
		if _, dup := seenParent[key]; dup {
			continue
		}
		seenParent[key] = struct{}{}
		waiters := e.unresolvedCodexForks.waitersForParent(ctx, e.db, key)
		if len(waiters) == 0 {
			continue
		}
		info, err := os.Stat(p)
		if err != nil || info.IsDir() {
			continue
		}
		for _, waiter := range waiters {
			e.unresolvedCodexForks.invalidate(waiter)
			childPath := waiter.path
			if slices.Contains(paths, childPath) || slices.Contains(extra, childPath) {
				continue
			}
			if _, statErr := os.Stat(childPath); statErr == nil {
				extra = append(extra, childPath)
			}
		}
	}
	if len(extra) == 0 {
		return paths
	}
	combined := make([]string, 0, len(paths)+len(extra))
	combined = append(combined, paths...)
	combined = append(combined, extra...)
	return combined
}
