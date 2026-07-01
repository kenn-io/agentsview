// Package sessionwatch polls a session's DB state and source-file
// mtime, emitting a tick each time the session version changes.
// Shared by the HTTP SSE handler and the CLI `session watch` command.
package sessionwatch

import (
	"context"
	"log"
	"os"
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
)

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
	ch := make(chan struct{})
	lastCount, lastDBVersion, _ := w.db.GetSessionVersion(
		sessionID,
	)
	go func() {
		defer close(ch)

		if w.engine == nil {
			// PG read mode: poll GetSessionVersion only,
			// no file watching or fallback sync.
			w.pollDBOnly(ctx, ch, sessionID,
				lastCount, lastDBVersion)
			return
		}

		// Track file mtime for fallback sync.
		sourcePath := w.engine.FindSourceFile(sessionID)
		var lastFileMtime int64
		var fileMtimeChangedAt time.Time
		if sourcePath != "" {
			lastFileMtime = w.engine.SourceMtime(sessionID)
		}

		ticker := time.NewTicker(pollInterval())
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				changed := w.checkDBForChanges(
					sessionID,
					&lastCount,
					&lastDBVersion,
					&sourcePath,
					&lastFileMtime,
					&fileMtimeChangedAt,
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

// pollDBOnly polls GetSessionVersion on a timer and signals ch
// when changes are detected. Used in PG-read mode where there is
// no sync engine or file watcher.
func (w *Watcher) pollDBOnly(
	ctx context.Context, ch chan<- struct{},
	sessionID string, lastCount int, lastDBVersion int64,
) {
	ticker := time.NewTicker(pollInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			count, dbVersion, ok := w.db.GetSessionVersion(sessionID)
			if ok && (count != lastCount || dbVersion != lastDBVersion) {
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
func (w *Watcher) checkDBForChanges(
	sessionID string,
	lastCount *int,
	lastDBVersion *int64,
	sourcePath *string,
	lastFileMtime *int64,
	fileMtimeChangedAt *time.Time,
) bool {
	// Primary: check if the DB has new data. The version marker covers
	// message appends and metadata/content-only updates.
	if count, dbVersion, ok := w.db.GetSessionVersion(
		sessionID,
	); ok && (count != *lastCount ||
		dbVersion != *lastDBVersion) {
		*lastCount = count
		*lastDBVersion = dbVersion
		// DB was updated; clear any pending fallback.
		*fileMtimeChangedAt = time.Time{}
		return true
	}

	// Track file mtime for the fallback path.
	if *sourcePath == "" {
		*sourcePath = w.engine.FindSourceFile(sessionID)
		if *sourcePath == "" {
			return false
		}
		*lastFileMtime = w.engine.SourceMtime(sessionID)
		// Source file (re-)resolved — trigger fallback sync
		// immediately since content likely differs from DB.
		past := time.Now().Add(-syncFallbackDelay())
		*fileMtimeChangedAt = past
	}

	mtime := w.engine.SourceMtime(sessionID)
	if mtime == 0 {
		// File disappeared; try to re-resolve later.
		*sourcePath = ""
		*lastFileMtime = 0
		*fileMtimeChangedAt = time.Time{}
		return false
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
		if err := w.engine.SyncSingleSession(
			sessionID,
		); err != nil {
			log.Printf("watch sync error: %v", err)
			return false
		}
		// Re-check the DB after syncing.
		if count, dbVersion, ok := w.db.GetSessionVersion(
			sessionID,
		); ok && (count != *lastCount ||
			dbVersion != *lastDBVersion) {
			*lastCount = count
			*lastDBVersion = dbVersion
			return true
		}
	}

	return false
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
