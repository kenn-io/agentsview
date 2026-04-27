package server

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/wesm/agentsview/internal/sessionwatch"
	syncpkg "github.com/wesm/agentsview/internal/sync"
)

// sessionMonitor returns a channel that ticks whenever the
// session's DB state changes. Thin adapter around
// sessionwatch.Watcher, which contains the polling logic shared
// with the CLI `session watch` command.
func (s *Server) sessionMonitor(
	ctx context.Context, sessionID string,
) <-chan struct{} {
	return sessionwatch.New(s.db, s.engine).Events(ctx, sessionID)
}

func (s *Server) handleEvents(
	w http.ResponseWriter, r *http.Request,
) {
	if s.engine == nil || s.broadcaster == nil {
		w.Header().Set("Retry-After", "300")
		writeError(w, http.StatusServiceUnavailable,
			"events not available in this mode")
		return
	}

	stream, err := NewSSEStream(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"streaming not supported")
		return
	}

	sub, unsub := s.broadcaster.Subscribe()
	defer unsub()

	heartbeat := time.NewTicker(
		sessionwatch.PollInterval * sessionwatch.HeartbeatTicks,
	)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-sub:
			if !ok {
				return
			}
			stream.SendJSON("data_changed",
				map[string]string{"scope": ev.Scope})
		case <-heartbeat.C:
			stream.Send("heartbeat",
				time.Now().Format(time.RFC3339))
		}
	}
}

func (s *Server) handleWatchSession(
	w http.ResponseWriter, r *http.Request,
) {
	sessionID := r.PathValue("id")

	// Fail fast on unknown ids so a typo does not become an
	// indefinitely live heartbeat stream. The existence check
	// happens before NewSSEStream so the client sees a normal
	// 404 JSON error instead of an empty SSE body.
	sess, err := s.sessions.Get(r.Context(), sessionID)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if sess == nil {
		writeError(w, http.StatusNotFound,
			"session not found: "+sessionID)
		return
	}

	stream, err := NewSSEStream(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"streaming not supported")
		return
	}

	updates := s.sessionMonitor(r.Context(), sessionID)
	heartbeat := time.NewTicker(
		sessionwatch.PollInterval * sessionwatch.HeartbeatTicks,
	)
	defer heartbeat.Stop()

	// Push the initial timing snapshot on connect so the right
	// panel doesn't wait for the next change to populate.
	if t, err := s.db.GetSessionTiming(
		r.Context(), sessionID,
	); err != nil {
		log.Printf("session timing initial: %v", err)
	} else if t != nil {
		stream.SendJSON("session.timing", t)
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case _, ok := <-updates:
			if !ok {
				return
			}
			stream.Send("session_updated", sessionID)
			if t, err := s.db.GetSessionTiming(
				r.Context(), sessionID,
			); err != nil {
				log.Printf("session timing update: %v", err)
			} else if t != nil {
				stream.SendJSON("session.timing", t)
			}
		case <-heartbeat.C:
			stream.Send("heartbeat",
				time.Now().UTC().Format(time.RFC3339))
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
	q := r.URL.Query()
	excludeOneShot := q.Get("include_one_shot") != "true"
	excludeAutomated := q.Get("include_automated") != "true"
	stats, err := s.db.GetStats(r.Context(), excludeOneShot, excludeAutomated)
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
	q := r.URL.Query()
	excludeOneShot := q.Get("include_one_shot") != "true"
	excludeAutomated := q.Get("include_automated") != "true"
	projects, err := s.db.GetProjects(r.Context(), excludeOneShot, excludeAutomated)
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
	q := r.URL.Query()
	excludeOneShot := q.Get("include_one_shot") != "true"
	excludeAutomated := q.Get("include_automated") != "true"
	machines, err := s.db.GetMachines(r.Context(), excludeOneShot, excludeAutomated)
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
	q := r.URL.Query()
	excludeOneShot := q.Get("include_one_shot") != "true"
	excludeAutomated := q.Get("include_automated") != "true"
	agents, err := s.db.GetAgents(r.Context(), excludeOneShot, excludeAutomated)
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
