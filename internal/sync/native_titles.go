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
	expandRoots         func(string) ([]string, error)
	storePath           func(string) (string, bool)
	ownsStore           func(configuredRoot, store string) bool
	read                func(context.Context, string) (map[string]string, parser.SQLiteContainerState, error)
	sessionRoot         func(string) (string, bool)
	blankClears         bool
	ignoreFramelessWAL  bool
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
		ignoreFramelessWAL:  true,
		outsideWatchedRoots: false,
	},
	{
		agent:       parser.AgentAntigravityCLI,
		expandRoots: parser.ExpandAntigravityCLIRoot,
		storePath: func(root string) (string, bool) {
			return filepath.Join(root, parser.AntigravitySummariesDBName), true
		},
		ownsStore:           parser.AntigravityCLITitleStoreOwner,
		read:                parser.AntigravityStoreTitles,
		sessionRoot:         parser.AntigravityCLISessionRoot,
		blankClears:         true,
		ignoreFramelessWAL:  true,
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
func (e *Engine) reconcileNativeTitlesLocked(ctx context.Context, stats *SyncStats) map[string]bool {
	if e.db.ArchiveContent().UsageOnly() {
		return nil
	}
	sources := e.sources()
	if e.appliedNativeTitles == nil {
		e.appliedNativeTitles = make(map[string]appliedNativeTitleStore)
	}
	var failedStores map[string]bool
	for _, source := range nativeTitleSources {
		if _, enabled := sources.providerFactories[source.agent]; !enabled {
			continue
		}
		seen := make(map[string]bool)
		for _, configuredRoot := range sources.agentDirs[source.agent] {
			roots := []string{configuredRoot}
			if source.expandRoots != nil {
				var expandErr error
				roots, expandErr = source.expandRoots(configuredRoot)
				if expandErr != nil {
					stats.RecordFailed()
					log.Printf("%s titles %s: %v", source.agent, configuredRoot, expandErr)
				}
			}
			for _, root := range roots {
				if ctx.Err() != nil {
					return failedStores
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
				titles, state, err := source.read(ctx, store)
				if err == nil {
					var n int
					n, err = e.applyNativeStoreTitles(ctx, source, root, titles)
					stats.TitlesUpdated += n
				}
				if err != nil {
					if failedStores == nil {
						failedStores = make(map[string]bool)
					}
					failedStores[store] = true
					stats.RecordFailed()
					// Stores outside watched roots are retried by the next sync.
					if retry, ok := ctx.Value(nativeTitleRetryContextKey{}).(*WatchBatch); ok && !source.outsideWatchedRoots {
						retry.Paths = watchAppendUniqueString(retry.Paths, store)
					}
					parser.InvalidateSQLiteTitles(store)
					log.Printf("%s titles %s: %v", source.agent, store, err)
					continue
				}
				if state != (parser.SQLiteContainerState{}) {
					e.appliedNativeTitles[store] = appliedNativeTitleStore{state: state, readAt: now}
				}
			}
		}
	}
	return failedStores
}

func (e *Engine) invalidateNativeTitleCache(paths []string) {
	if paths == nil {
		clear(e.appliedNativeTitles)
	}
	for _, source := range nativeTitleSources {
		configuredRoots := e.sources().agentDirs[source.agent]
		if paths == nil {
			for _, configuredRoot := range configuredRoots {
				roots := []string{configuredRoot}
				if source.expandRoots != nil {
					roots, _ = source.expandRoots(configuredRoot)
				}
				for _, root := range roots {
					if store, ok := source.storePath(root); ok {
						parser.InvalidateSQLiteTitles(store)
					}
				}
			}
			continue
		}
		var reachable map[string]bool
		if source.ownsStore == nil {
			reachable = make(map[string]bool, len(configuredRoots))
			for _, root := range configuredRoots {
				if store, ok := source.storePath(root); ok {
					reachable[store] = true
				}
			}
		}
		for _, path := range paths {
			store := strings.TrimSuffix(filepath.Clean(path), "-wal")
			owned := false
			if source.ownsStore != nil {
				for _, root := range configuredRoots {
					if source.ownsStore(root, store) {
						owned = true
						break
					}
				}
			} else {
				owned = reachable[store]
			}
			if !owned {
				continue
			}
			if source.ignoreFramelessWAL {
				if _, ok := parser.SQLiteContainerPathForEvent(filepath.Dir(store), path, parser.AntigravitySummariesDBName, true); !ok {
					continue
				}
			}
			delete(e.appliedNativeTitles, store)
			parser.InvalidateSQLiteTitles(store)
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
	resolved, ok := e.resolveStoredNativePath(storedPath)
	if !ok {
		return false
	}
	owner, ok := source.sessionRoot(resolved)
	return ok && sameReconciliationSourcePath(owner, root)
}

func (e *Engine) resolveStoredNativePath(storedPath string) (string, bool) {
	if e.pathRewriter != nil {
		if e.storedPathResolver == nil {
			return "", false
		}
		return e.storedPathResolver(storedPath)
	}
	return storedPath, true
}
