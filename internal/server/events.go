package server

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	syncpkg "github.com/wesm/agentsview/internal/sync"
)

// statMtime returns the file's modification time in
// nanoseconds, or 0 if the file cannot be stat'd.
func statMtime(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.ModTime().UnixNano()
}

const (
	// pollInterval is how often the session monitor checks
	// the database for changes.
	pollInterval = 1500 * time.Millisecond
	// heartbeatInterval is how often a keepalive is sent to
	// the client. Expressed as a multiple of pollInterval
	// (~30s).
	heartbeatTicks = 20
	// syncFallbackDelay is how long to wait after detecting
	// a file mtime change before attempting a direct sync.
	// This gives the file watcher time to process the change
	// through the normal SyncPaths pipeline.
	syncFallbackDelay = 5 * time.Second
)

// sessionMonitor polls the database for session changes and
// signals the returned channel when the message count changes.
// This is decoupled from file I/O — the file watcher handles
// syncing files to the database, and this monitor detects the
// resulting DB changes.
//
// As a fallback for sessions the file watcher skips (e.g.
// codex_exec sessions), it also monitors the source file's
// mtime and triggers a direct sync when the DB hasn't been
// updated within syncFallbackDelay.
func (s *Server) sessionMonitor(
	ctx context.Context, sessionID string,
) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		defer close(ch)

		// Seed initial state from the database.
		lastCount, lastDBMtime, _ := s.db.GetSessionVersion(
			sessionID,
		)

		if s.engine == nil {
			// PG read mode: poll GetSessionVersion only,
			// no file watching or fallback sync.
			s.pollDBOnly(ctx, ch, sessionID,
				lastCount, lastDBMtime)
			return
		}

		// Track file mtime for fallback sync.
		sourcePath := s.engine.FindSourceFile(sessionID)
		var lastFileMtime int64
		var fileMtimeChangedAt time.Time
		if sourcePath != "" {
			lastFileMtime = statMtime(sourcePath)
		}

		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				changed := s.checkDBForChanges(
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
func (s *Server) pollDBOnly(
	ctx context.Context, ch chan<- struct{},
	sessionID string, lastCount int, lastDBMtime int64,
) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			count, dbMtime, ok := s.db.GetSessionVersion(sessionID)
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
func (s *Server) checkDBForChanges(
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
	if count, dbMtime, ok := s.db.GetSessionVersion(
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
		*sourcePath = s.engine.FindSourceFile(sessionID)
		if *sourcePath == "" {
			return false
		}
		*lastFileMtime = statMtime(*sourcePath)
		// Source file (re-)resolved — trigger fallback sync
		// immediately since content likely differs from DB.
		past := time.Now().Add(-syncFallbackDelay)
		*fileMtimeChangedAt = past
	}

	mtime := statMtime(*sourcePath)
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
	// updated within syncFallbackDelay, trigger a direct
	// sync. This handles sessions the watcher skips (e.g.
	// codex_exec).
	if !fileMtimeChangedAt.IsZero() &&
		time.Since(*fileMtimeChangedAt) >= syncFallbackDelay {
		*fileMtimeChangedAt = time.Time{}
		if err := s.engine.SyncSingleSession(
			sessionID,
		); err != nil {
			log.Printf("watch sync error: %v", err)
			return false
		}
		// Re-check the DB after syncing.
		if count, dbMtime, ok := s.db.GetSessionVersion(
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

func (s *Server) handleWatchSession(
	w http.ResponseWriter, r *http.Request,
) {
	sessionID := r.PathValue("id")

	stream, err := NewSSEStream(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"streaming not supported")
		return
	}

	updates := s.sessionMonitor(r.Context(), sessionID)
	heartbeat := time.NewTicker(
		pollInterval * heartbeatTicks,
	)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case _, ok := <-updates:
			if !ok {
				return
			}
			stream.Send("session_updated", sessionID)
		case <-heartbeat.C:
			stream.Send("heartbeat",
				time.Now().Format(time.RFC3339))
		}
	}
}

func (s *Server) handleTriggerSync(
	w http.ResponseWriter, r *http.Request,
) {
	if s.engine == nil {
		writeError(w, http.StatusNotImplemented,
			"not available in remote mode")
		return
	}
	stream, err := NewSSEStream(w)
	if err != nil {
		// Non-streaming fallback
		stats := s.engine.SyncAll(r.Context(), nil)
		writeJSON(w, http.StatusOK, stats)
		return
	}

	stats := s.engine.SyncAll(r.Context(), func(p syncpkg.Progress) {
		stream.SendJSON("progress", p)
	})
	stream.SendJSON("done", stats)
}

func (s *Server) handleTriggerResync(
	w http.ResponseWriter, r *http.Request,
) {
	if s.engine == nil {
		writeError(w, http.StatusNotImplemented,
			"not available in remote mode")
		return
	}
	stream, err := NewSSEStream(w)
	if err != nil {
		stats := s.engine.ResyncAll(r.Context(), nil)
		writeJSON(w, http.StatusOK, stats)
		return
	}

	stats := s.engine.ResyncAll(r.Context(), func(p syncpkg.Progress) {
		stream.SendJSON("progress", p)
	})
	stream.SendJSON("done", stats)
}

func (s *Server) handleSyncStatus(
	w http.ResponseWriter, r *http.Request,
) {
	if s.engine == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"last_sync": "",
			"stats":     nil,
		})
		return
	}
	lastSync := s.engine.LastSync()
	stats := s.engine.LastSyncStats()

	var lastSyncStr string
	if !lastSync.IsZero() {
		lastSyncStr = lastSync.Format(time.RFC3339)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"last_sync": lastSyncStr,
		"stats":     stats,
	})
}

func (s *Server) handleGetStats(
	w http.ResponseWriter, r *http.Request,
) {
	excludeOneShot := r.URL.Query().Get("include_one_shot") != "true"
	stats, err := s.db.GetStats(r.Context(), excludeOneShot)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleListProjects(
	w http.ResponseWriter, r *http.Request,
) {
	excludeOneShot := r.URL.Query().Get("include_one_shot") != "true"
	projects, err := s.db.GetProjects(r.Context(), excludeOneShot)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"projects": projects,
	})
}

func (s *Server) handleListMachines(
	w http.ResponseWriter, r *http.Request,
) {
	excludeOneShot := r.URL.Query().Get("include_one_shot") != "true"
	machines, err := s.db.GetMachines(r.Context(), excludeOneShot)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"machines": machines,
	})
}

func (s *Server) handleListAgents(
	w http.ResponseWriter, r *http.Request,
) {
	excludeOneShot := r.URL.Query().Get("include_one_shot") != "true"
	agents, err := s.db.GetAgents(r.Context(), excludeOneShot)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agents": agents,
	})
}
