// Package sessionwatch polls a session's DB state and source-file
// mtime, emitting a tick each time the session version changes.
// Shared by the HTTP SSE handler and the CLI `session watch` command.
package sessionwatch

import (
	"context"
	"log"
	"os"
	"slices"
	"sync/atomic"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/sync"
)

const (
	// PollInterval is how often the session monitor checks
	// the database for changes.
	PollInterval = 1500 * time.Millisecond
	// HeartbeatTicks is how often a keepalive is sent to
	// the client. Expressed as a multiple of PollInterval
	// (~30s).
	HeartbeatTicks = 20
	// SyncFallbackDelay is how long to wait after detecting
	// a file mtime change before attempting a direct sync.
	// This gives the file watcher time to process the change
	// through the normal SyncPaths pipeline.
	SyncFallbackDelay = 5 * time.Second
)

var (
	pollIntervalNanos      int64 = int64(PollInterval)
	syncFallbackDelayNanos int64 = int64(SyncFallbackDelay)
	sourceMissingObserver  atomic.Value
)

func init() {
	sourceMissingObserver.Store(func(string) {})
}

func pollInterval() time.Duration {
	return time.Duration(atomic.LoadInt64(&pollIntervalNanos))
}

func syncFallbackDelay() time.Duration {
	return time.Duration(atomic.LoadInt64(&syncFallbackDelayNanos))
}

// SetTimingsForTest overrides watcher timing knobs and returns a restore
// function. It is intended for tests that exercise polling behavior without
// paying production-scale intervals.
func SetTimingsForTest(
	pollIntervalOverride time.Duration,
	syncFallbackDelayOverride time.Duration,
) func() {
	oldPoll := atomic.SwapInt64(
		&pollIntervalNanos, int64(pollIntervalOverride),
	)
	oldFallback := atomic.SwapInt64(
		&syncFallbackDelayNanos, int64(syncFallbackDelayOverride),
	)
	return func() {
		atomic.StoreInt64(&pollIntervalNanos, oldPoll)
		atomic.StoreInt64(&syncFallbackDelayNanos, oldFallback)
	}
}

// SetSourceMissingObserverForTest installs a callback for tests that need to
// coordinate with source disappearance processing.
func SetSourceMissingObserverForTest(observer func(string)) func() {
	previous := sourceMissingObserver.Load().(func(string))
	sourceMissingObserver.Store(observer)
	return func() { sourceMissingObserver.Store(previous) }
}

// Watcher emits a tick on Events() each time the session's DB state
// changes, with an optional file-mtime-triggered direct sync when the
// engine is non-nil.
type Watcher struct {
	db     db.Store
	engine *sync.Engine // may be nil (PG-read mode)
}

// New returns a Watcher backed by the given store. engine may be
// nil to disable file-mtime fallback sync (PG-read mode).
func New(d db.Store, engine *sync.Engine) *Watcher {
	return &Watcher{db: d, engine: engine}
}

// Events polls the database for session changes and signals the
// returned channel when the session version changes. This is
// decoupled from file I/O — the file watcher handles syncing
// files to the database, and this monitor detects the resulting
// DB changes.
//
// As a fallback when file watching or incremental sync misses a
// DB update, it also monitors the source file's mtime and
// triggers a direct sync when the DB hasn't been updated within
// SyncFallbackDelay.
func (w *Watcher) Events(
	ctx context.Context, sessionID string,
) <-chan struct{} {
	return w.events(ctx, sessionID, false)
}

// TreeEvents also detects changes to the session's direct children.
func (w *Watcher) TreeEvents(ctx context.Context, sessionID string) <-chan struct{} {
	return w.events(ctx, sessionID, true)
}

func (w *Watcher) events(ctx context.Context, sessionID string, tree bool) <-chan struct{} {
	ch := make(chan struct{})
	var marker db.SessionTreeSnapshot
	var markerErr error
	failureReported := false
	if tree {
		marker, markerErr = w.db.GetSessionTreeMarker(ctx, sessionID)
	}
	checkTree := func() (bool, bool) {
		if !tree {
			return false, false
		}
		current, err := w.db.GetSessionTreeMarker(ctx, sessionID)
		changed := err != nil && !failureReported || err == nil && (markerErr != nil || current.Marker != marker.Marker)
		parentChanged := err == nil && markerErr == nil && treeParentChanged(marker, current)
		failureReported = err != nil
		marker, markerErr = current, err
		return changed, parentChanged
	}
	if identity, ok := w.db.(db.SessionWatchStateStore); ok {
		initial, err := identity.GetSessionWatchState(sessionID)
		var lastCount int
		var lastVersion int64
		if !tree {
			lastCount, lastVersion, _ = w.db.GetSessionVersion(ctx, sessionID)
		}
		go func() {
			defer close(ch)
			if err == nil && initial.State != db.SessionWatchResolved {
				select {
				case ch <- struct{}{}:
				case <-ctx.Done():
				}
				return
			}
			ticker := time.NewTicker(pollInterval())
			defer ticker.Stop()
			// After an initial read error, the first successful read
			// becomes the baseline and is announced once.
			recovering := err != nil
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				current, e := identity.GetSessionWatchState(sessionID)
				if e == nil && current.State != db.SessionWatchResolved {
					select {
					case ch <- struct{}{}:
					case <-ctx.Done():
					}
					return
				}
				treeChanged, _ := checkTree()
				var count int
				var version int64
				var versionOK bool
				if !tree && e == nil {
					count, version, versionOK = w.db.GetSessionVersion(ctx, sessionID)
				}
				versionChanged := versionOK && (count != lastCount || version != lastVersion)
				transition := e == nil && !recovering && watchStateChanged(initial, current)
				if treeChanged || versionChanged {
					// A change that lands between the watch-state read and the
					// tree or version read advances that second baseline while
					// current still holds the previous watch state. Re-read so
					// the next poll does not emit a second tick for the same update.
					if refreshed, re := identity.GetSessionWatchState(sessionID); re == nil {
						current, e = refreshed, nil
					}
				}
				if (e == nil && (recovering || transition)) || versionChanged || treeChanged {
					select {
					case ch <- struct{}{}:
					case <-ctx.Done():
						return
					}
					if e == nil {
						if current.State != db.SessionWatchResolved {
							return
						}
						initial, recovering = current, false
					}
					if versionOK {
						lastCount, lastVersion = count, version
					}
				}
			}
		}()
		return ch
	}
	var lastCount int
	var lastDBVersion int64
	if !tree {
		lastCount, lastDBVersion, _ = w.db.GetSessionVersion(ctx, sessionID)
	}
	go func() {
		defer close(ch)

		if w.engine == nil {
			// PG read mode: poll GetSessionVersion only,
			// no file watching or fallback sync.
			w.pollDBOnly(ctx, ch, sessionID,
				lastCount, lastDBVersion, tree, checkTree)
			return
		}

		// Track file mtime for fallback sync.
		sourcePath := w.engine.FindSourceFile(ctx, sessionID)
		var lastFileMtime int64
		var fileMtimeChangedAt time.Time
		if sourcePath != "" {
			lastFileMtime = w.engine.SourceMtime(ctx, sessionID)
		}

		ticker := time.NewTicker(pollInterval())
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				changed := w.checkDBForChanges(ctx,
					sessionID,
					&lastCount,
					&lastDBVersion,
					&sourcePath,
					&lastFileMtime,
					&fileMtimeChangedAt,
					tree, checkTree,
				)
				if changed {
					select {
					case ch <- struct{}{}:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	return ch
}

func watchStateChanged(previous, current db.SessionWatchState) bool {
	return current.State != previous.State ||
		current.PublicID != previous.PublicID ||
		current.ContentRevision != previous.ContentRevision ||
		!slices.Equal(current.Variants, previous.Variants)
}

func treeParentChanged(previous, current db.SessionTreeSnapshot) bool {
	return previous.ParentCount != current.ParentCount || previous.ParentVersion != current.ParentVersion
}

// pollDBOnly polls GetSessionVersion on a timer and signals ch
// when changes are detected. Used in PG-read mode where there is
// no sync engine or file watcher.
func (w *Watcher) pollDBOnly(
	ctx context.Context, ch chan<- struct{},
	sessionID string, lastCount int, lastDBVersion int64,
	tree bool, checkTree func() (bool, bool),
) {
	ticker := time.NewTicker(pollInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var count int
			var dbVersion int64
			var ok bool
			changed, _ := checkTree()
			if !tree {
				count, dbVersion, ok = w.db.GetSessionVersion(ctx, sessionID)
			}
			if changed || ok && (count != lastCount || dbVersion != lastDBVersion) {
				lastCount = count
				lastDBVersion = dbVersion
				select {
				case ch <- struct{}{}:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

// checkDBForChanges polls the database for a session version change.
// As a fallback, it monitors source file mtime and triggers a direct
// sync when the watcher hasn't updated the DB.
func (w *Watcher) checkDBForChanges(ctx context.Context,
	sessionID string,
	lastCount *int,
	lastDBVersion *int64,
	sourcePath *string,
	lastFileMtime *int64,
	fileMtimeChangedAt *time.Time,
	tree bool, checkTree func() (bool, bool),
) bool {
	// Primary: check if the DB has new data. The version marker covers
	// message appends and metadata/content-only updates.
	changed, parentChanged := false, false
	if tree {
		changed, parentChanged = checkTree()
	} else if count, dbVersion, ok := w.db.GetSessionVersion(ctx,
		sessionID,
	); ok && (count != *lastCount ||
		dbVersion != *lastDBVersion) {
		*lastCount = count
		*lastDBVersion = dbVersion
		parentChanged = true
	}
	if parentChanged {
		// DB was updated; clear any pending fallback.
		*fileMtimeChangedAt = time.Time{}
		return true
	}

	// Track file mtime for the fallback path.
	if *sourcePath == "" {
		*sourcePath = w.engine.FindSourceFile(ctx, sessionID)
		if *sourcePath == "" {
			sourceMissingObserver.Load().(func(string))(sessionID)
			return changed
		}
		*lastFileMtime = w.engine.SourceMtime(ctx, sessionID)
		// Source file (re-)resolved — trigger fallback sync
		// immediately since content likely differs from DB.
		past := time.Now().Add(-syncFallbackDelay())
		*fileMtimeChangedAt = past
	}

	mtime := w.engine.SourceMtime(ctx, sessionID)
	if mtime == 0 {
		// File disappeared; try to re-resolve later.
		*sourcePath = ""
		*lastFileMtime = 0
		*fileMtimeChangedAt = time.Time{}
		sourceMissingObserver.Load().(func(string))(sessionID)
		return changed
	}

	if mtime != *lastFileMtime {
		*lastFileMtime = mtime
		if fileMtimeChangedAt.IsZero() {
			now := time.Now()
			*fileMtimeChangedAt = now
		}
	}

	// Fallback: if the file changed but the DB hasn't been
	// updated within SyncFallbackDelay, trigger a direct
	// sync.
	if !fileMtimeChangedAt.IsZero() &&
		time.Since(*fileMtimeChangedAt) >= syncFallbackDelay() {
		*fileMtimeChangedAt = time.Time{}
		if err := w.engine.SyncSingleSessionContext(
			ctx, sessionID,
		); err != nil {
			log.Printf("watch sync error: %v", err)
			return changed
		}
		// Re-check the DB after syncing.
		if tree {
			after, _ := checkTree()
			return changed || after
		}
		if count, dbVersion, ok := w.db.GetSessionVersion(ctx,
			sessionID,
		); ok && (count != *lastCount ||
			dbVersion != *lastDBVersion) {
			*lastCount = count
			*lastDBVersion = dbVersion
			return true
		}
	}

	return changed
}

// StatMtime returns the file's modification time in
// nanoseconds, or 0 if the file cannot be stat'd.
func StatMtime(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.ModTime().UnixNano()
}
