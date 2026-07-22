package sync

import "slices"

// WatchScope identifies one configured provider root whose changes are covered
// by a logical watcher root. A physical root may cover multiple configured
// directories or providers, so registration must preserve every exact scope.
type WatchScope struct {
	Agent   string
	SyncDir string
}

// WatchRoot is one desired logical watcher root. Exists describes startup
// state, not whether the root remains desired: missing roots stay in the plan
// so a lifecycle-aware backend can establish ancestor coverage later.
type WatchRoot struct {
	Path      string
	Recursive bool
	Exists    bool
	Scopes    []WatchScope
}

// RegisterRoots passes the complete desired root plan to the watcher before
// its backend starts. The current fsnotify backend activates existing roots;
// later lifecycle-aware backends can also retain pending missing roots without
// changing the daemon-side plan contract.
func (w *Watcher) RegisterRoots(
	roots []WatchRoot,
	recursiveBudget int,
) []RecursiveWatchResult {
	for _, root := range roots {
		for _, scope := range root.Scopes {
			if scope.SyncDir != "" {
				w.registeredSyncRoots = append(w.registeredSyncRoots, scope.SyncDir)
			}
		}
	}
	slices.Sort(w.registeredSyncRoots)
	w.registeredSyncRoots = slices.Compact(w.registeredSyncRoots)
	if observer, ok := w.backend.(watchRootPlanObserver); ok {
		observer.setWatchRootPlan(roots)
	}
	if backend, ok := w.backend.(completeRootPlanBackend); ok {
		for _, root := range roots {
			agents := make([]string, 0, len(root.Scopes))
			for _, scope := range root.Scopes {
				if scope.Agent != "" && !slices.Contains(agents, scope.Agent) {
					agents = append(agents, scope.Agent)
				}
			}
			w.SetRootAgents(root.Path, agents)
		}
		return backend.RegisterRoots(roots, recursiveBudget)
	}
	results := make([]RecursiveWatchResult, len(roots))
	remaining := recursiveBudget
	for i, root := range roots {
		agents := make([]string, 0, len(root.Scopes))
		for _, scope := range root.Scopes {
			if scope.Agent != "" && !slices.Contains(agents, scope.Agent) {
				agents = append(agents, scope.Agent)
			}
		}
		w.SetRootAgents(root.Path, agents)
		if !root.Exists {
			continue
		}
		if !root.Recursive {
			err := w.backend.AddShallow(root.Path)
			results[i].Err = err
			if err == nil {
				results[i].Watched = 1
			} else {
				results[i].Unwatched = 1
			}
			continue
		}
		results[i] = w.backend.AddRecursive(root.Path, remaining)
		remaining -= results[i].Watched
	}
	return results
}

type backendItemType uint8

const (
	backendItemUnknown backendItemType = iota
	backendItemFile
	backendItemDirectory
)

type backendOp uint8

const backendOpUnknown backendOp = 0

const (
	backendOpCreate backendOp = 1 << iota
	backendOpWrite
	backendOpRemove
	backendOpRename
	backendOpReconcileRootChange
	backendOpFullSync
)

type backendEvent struct {
	Path      string
	Root      string
	Op        backendOp
	ItemType  backendItemType
	Lifecycle backendLifecycleToken
}

type backendLifecycleGate interface {
	acknowledgeLifecycle(uint64)
}

type backendLifecycleToken struct {
	gate       backendLifecycleGate
	generation uint64
}

func (t backendLifecycleToken) valid() bool { return t.gate != nil }

type watchBackend interface {
	Events() <-chan backendEvent
	Errors() <-chan error
	AddRecursive(root string, budget int) RecursiveWatchResult
	AddShallow(root string) error
	Remove(root string) error
	Start() error
	Stop()
	Name() string
}

// directBackendEventSink lets a native callback publish a complete native
// delivery through the watcher's bounded handoff. Implementations must return
// promptly without waiting for consumer ownership. Capacity overflow becomes
// one authoritative full-sync marker; ordinary consumer activity does not.
// Control workers use RetainAuthoritative when the marker must carry a durable
// lifecycle acknowledgement token.
type directBackendEventSink interface {
	TryAccumulate(func(add func(backendEvent) bool) bool) bool
	RetainAuthoritative(backendLifecycleToken)
}

// directEventBackend is implemented by backends whose native delivery already
// provides burst coalescing. Binding the direct sink also tells Watcher not to
// add its Go batch delay after the native latency; the dispatch floor remains.
type directEventBackend interface {
	bindEventSink(directBackendEventSink)
}

// coverageDegradationBackend accepts runtime roots that must be transferred to
// a completeness backstop before native recursive coverage can be retired.
type coverageDegradationBackend interface {
	bindCoverageDegraded(func([]string) error)
}

// watchRootPlanObserver retains logical-root to configured-scope ownership for
// backends that can lose coverage after startup and must transfer the exact
// affected scopes to polling.
type watchRootPlanObserver interface {
	setWatchRootPlan([]WatchRoot)
}

// createdSubtreePathFilter marks portable backends whose directory-create
// events need a bounded userspace walk to surface files that existed before
// the new subtree entered a watched root. Native direct-callback backends do
// not implement this contract, so their callbacks never perform filesystem
// work.
type createdSubtreePathFilter interface {
	shouldEnumerateCreatedSubtree(root, path string) bool
	includeCreatedSubtreePath(root, path string) bool
}

// pollingOwnershipBackend reports independently keyed polling obligations so
// overlapping configured roots cannot release one another's coverage.
type pollingOwnershipBackend interface {
	bindPollingOwnership(
		func(PollingObligation) error,
		func(string) error,
	)
}
