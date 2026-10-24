package sync

import (
	"context"
	"log"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

type appliedNativeTitleStore struct {
	state  parser.SQLiteContainerState
	readAt time.Time
}

type nativeTitleRetryContextKey struct{}

type nativeTitleSource struct {
	agent               parser.AgentType
	storePath           func(string) (string, bool)
	read                func(context.Context, string) (map[string]string, parser.SQLiteContainerState, error)
	sessionRoot         func(string) (string, bool)
	blankClears         bool
	outsideWatchedRoots bool
}

var nativeTitleSources = []nativeTitleSource{
	{
		agent: parser.AgentAntigravity,
		storePath: func(root string) (string, bool) {
			return filepath.Join(root, parser.AntigravitySummariesDBName), true
		},
		read:                parser.AntigravityStoreTitles,
		sessionRoot:         parser.AntigravitySessionRoot,
		blankClears:         true,
		outsideWatchedRoots: false,
	},
	{
		agent:               parser.AgentQoder,
		storePath:           parser.QoderTitleDatabase,
		read:                parser.QoderStoreTitles,
		sessionRoot:         parser.QoderSessionRoot,
		blankClears:         false,
		outsideWatchedRoots: true,
	},
}

// Reconcile after parsing so an older parsed title cannot win.
func (e *Engine) reconcileNativeTitlesLocked(ctx context.Context, stats *SyncStats) {
	// Usage-only archives keep no titles. Antigravity still fills workspace
	// and parent from the same store.
	usageOnly := e.db.ArchiveContent().UsageOnly()
	sources := e.sources()
	if e.appliedNativeTitles == nil {
		e.appliedNativeTitles = make(map[string]appliedNativeTitleStore)
	}
	for _, source := range nativeTitleSources {
		if usageOnly && source.agent != parser.AgentAntigravity {
			continue
		}
		if _, enabled := sources.providerFactories[source.agent]; !enabled {
			continue
		}
		seen := make(map[string]bool)
		for _, root := range sources.agentDirs[source.agent] {
			if ctx.Err() != nil {
				return
			}
			store, ok := source.storePath(root)
			if !ok || seen[store] {
				continue
			}
			seen[store] = true
			state, exists := parser.StatSQLiteContainerState(store)
			now := time.Now()
			if prior, ok := e.appliedNativeTitles[store]; ok && exists && prior.state == state && now.Sub(prior.readAt) < parser.SQLiteContainerDigestVerifyInterval {
				continue
			}
			delete(e.appliedNativeTitles, store)
			parser.InvalidateSQLiteTitles(store)
			var err error
			if source.agent == parser.AgentAntigravity {
				parser.InvalidateAntigravityTitles(store)
				var summaries map[string]parser.AntigravitySummary
				summaries, state, err = parser.AntigravityStoreSummaries(ctx, store)
				if err == nil {
					var n int
					n, err = e.applyAntigravityStoreSummaries(ctx, root, summaries)
					stats.TitlesUpdated += n
				}
			} else {
				var titles map[string]string
				titles, state, err = source.read(ctx, store)
				if err == nil {
					var n int
					n, err = e.applyNativeStoreTitles(ctx, source, root, titles)
					stats.TitlesUpdated += n
				}
			}
			if err != nil {
				stats.RecordFailed()
				// Stores outside watched roots are retried by the next sync.
				if retry, ok := ctx.Value(nativeTitleRetryContextKey{}).(*WatchBatch); ok && !source.outsideWatchedRoots {
					retry.Paths = watchAppendUniqueString(retry.Paths, store)
				}
				parser.InvalidateSQLiteTitles(store)
				if source.agent == parser.AgentAntigravity {
					parser.InvalidateAntigravityTitles(store)
				}
				log.Printf("%s titles %s: %v", source.agent, store, err)
				continue
			}
			if state != (parser.SQLiteContainerState{}) {
				e.appliedNativeTitles[store] = appliedNativeTitleStore{state: state, readAt: now}
			}
		}
	}
}

func (e *Engine) invalidateNativeTitleCache(paths []string) {
	if paths == nil {
		clear(e.appliedNativeTitles)
	}
	for _, source := range nativeTitleSources {
		reachable := make(map[string]bool)
		for _, root := range e.sources().agentDirs[source.agent] {
			if store, ok := source.storePath(root); ok {
				reachable[store] = true
			}
		}
		if paths == nil {
			for store := range reachable {
				parser.InvalidateSQLiteTitles(store)
				if source.agent == parser.AgentAntigravity {
					parser.InvalidateAntigravityTitles(store)
				}
			}
			continue
		}
		for _, path := range paths {
			store := strings.TrimSuffix(filepath.Clean(path), "-wal")
			if !reachable[store] {
				continue
			}
			if source.agent == parser.AgentAntigravity {
				if _, ok := parser.SQLiteContainerPathForEvent(filepath.Dir(store), path, parser.AntigravitySummariesDBName, true); !ok {
					continue
				}
			}
			delete(e.appliedNativeTitles, store)
			parser.InvalidateSQLiteTitles(store)
			if source.agent == parser.AgentAntigravity {
				parser.InvalidateAntigravityTitles(store)
			}
		}
	}
}

// recordNativeTitleUpdates folds a title-only pass into the reported sync
// state. Titles change without any session being written, so a pass that only
// renamed sessions still counts as activity.
func (e *Engine) recordNativeTitleUpdates(stats SyncStats) {
	if stats.TitlesUpdated > 0 || stats.Failed > 0 {
		e.mu.Lock()
		if stats.TitlesUpdated > 0 && stats.ProcessingComplete() {
			e.lastSync = time.Now()
		}
		e.lastSyncStats.TitlesUpdated += stats.TitlesUpdated
		e.lastSyncStats.Failed += stats.Failed
		e.mu.Unlock()
	}
}

func (e *Engine) applyNativeStoreTitles(ctx context.Context, source nativeTitleSource, root string, titles map[string]string) (updated int, err error) {
	if len(titles) == 0 {
		return 0, nil
	}
	prefix := e.idPrefix + string(source.agent) + ":"
	ids := make([]string, 0, len(titles))
	for id := range maps.Keys(titles) {
		ids = append(ids, prefix+id)
	}
	slices.Sort(ids)
	stored, err := e.db.ListSessionWriteIdentitiesByID(ctx, ids)
	if err != nil {
		return 0, err
	}
	var archived map[string]db.SessionWriteIdentity
	if reader, ok := e.archiveStore.(sessionWriteIdentityReader); ok && e.deferredNativeTitles != nil {
		// A rebuild copies archived sessions without sources only after this pass.
		if archived, err = reader.ListSessionWriteIdentitiesByID(ctx, ids); err != nil {
			return 0, err
		}
	}
	for _, id := range ids {
		session, ok := stored[id]
		deferred := false
		if !ok {
			session, deferred = archived[id]
		}
		if !ok && !deferred {
			continue
		}
		title := titles[strings.TrimPrefix(id, prefix)]
		if title == "" && !source.blankClears {
			continue
		}
		if session.SessionName != nil && *session.SessionName == title ||
			session.SessionName == nil && title == "" {
			continue
		}
		if !e.nativeRootOwns(source, root, session.FilePath) || !e.cwdFilter.allows(session.Cwd) {
			continue
		}
		var name *string
		if title != "" {
			name = &title
		}
		if deferred {
			e.deferredNativeTitles[id] = name
			continue
		}
		// Trashed sessions take the title too, so restoring one needs no store change.
		if err := e.db.RefreshNativeSessionName(ctx, id, name); err != nil {
			return updated, err
		}
		updated++
	}
	return updated, nil
}

// nativeRootOwns keeps a store from renaming a same-ID session that
// belongs to another configured root.
func (e *Engine) nativeRootOwns(source nativeTitleSource, root, storedPath string) bool {
	if e.pathRewriter != nil {
		if e.storedPathResolver == nil {
			return false
		}
		resolved, ok := e.storedPathResolver(storedPath)
		if !ok {
			return false
		}
		storedPath = resolved
	}
	owner, ok := source.sessionRoot(storedPath)
	return ok && sameReconciliationSourcePath(owner, root)
}
