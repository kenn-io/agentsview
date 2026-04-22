// Package sessionwatch polls a session's DB state and source-file
// mtime, emitting a tick each time the session version changes.
// Shared by the HTTP SSE handler and the CLI `session watch` command.
package sessionwatch

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/sync"
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
// returned channel when the message count changes. This is
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
	go func() {
		defer close(ch)

		// Seed initial state from the database.
		lastCount, lastDBMtime, _ := w.db.GetSessionVersion(
			sessionID,
		)

		if w.engine == nil {
			// PG read mode: poll GetSessionVersion only,
			// no file watching or fallback sync.
			w.pollDBOnly(ctx, ch, sessionID,
				lastCount, lastDBMtime)
			return
		}

		// Track file mtime for fallback sync.
		sourcePath := w.engine.FindSourceFile(sessionID)
		var lastFileMtime int64
		var fileMtimeChangedAt time.Time
		if sourcePath != "" {
			lastFileMtime = w.engine.SourceMtime(sessionID)
		}

		ticker := time.NewTicker(PollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				changed := w.checkDBForChanges(
					sessionID,
					&lastCount,
					&lastDBMtime,
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
	sessionID string, lastCount int, lastDBMtime int64,
) {
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			count, dbMtime, ok := w.db.GetSessionVersion(sessionID)
			if ok && (count != lastCount || dbMtime != lastDBMtime) {
				lastCount = count
				lastDBMtime = dbMtime
				select {
				case ch <- struct{}{}:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

// checkDBForChanges polls the database for a session's
// message_count and file_mtime. If either changed, it
// returns true. As a fallback, it monitors source file
// mtime and triggers a direct sync when the watcher
// hasn't updated the DB.
func (w *Watcher) checkDBForChanges(
	sessionID string,
	lastCount *int,
	lastDBMtime *int64,
	sourcePath *string,
	lastFileMtime *int64,
	fileMtimeChangedAt *time.Time,
) bool {
	// Primary: check if the DB has new data (message count
	// or file_mtime changed, covering both message appends
	// and metadata-only updates like progress events).
	if count, dbMtime, ok := w.db.GetSessionVersion(
		sessionID,
	); ok && (count != *lastCount ||
		dbMtime != *lastDBMtime) {
		*lastCount = count
		*lastDBMtime = dbMtime
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
		past := time.Now().Add(-SyncFallbackDelay)
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
		time.Since(*fileMtimeChangedAt) >= SyncFallbackDelay {
		*fileMtimeChangedAt = time.Time{}
		if err := w.engine.SyncSingleSession(
			sessionID,
		); err != nil {
			log.Printf("watch sync error: %v", err)
			return false
		}
		// Re-check the DB after syncing.
		if count, dbMtime, ok := w.db.GetSessionVersion(
			sessionID,
		); ok && (count != *lastCount ||
			dbMtime != *lastDBMtime) {
			*lastCount = count
			*lastDBMtime = dbMtime
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
