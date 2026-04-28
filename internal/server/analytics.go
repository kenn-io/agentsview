package server

import (
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/timeutil"
)

// defaultDateRange returns (from, to) defaulting to the last
// 30 days if not provided.
func defaultDateRange(
	from, to string,
) (string, string) {
	now := time.Now().UTC()
	if to == "" {
		to = now.Format("2006-01-02")
	}
	if from == "" {
		t, err := time.Parse("2006-01-02", to)
		if err != nil {
			t = now
		}
		from = t.AddDate(0, 0, -30).Format("2006-01-02")
	}
	return from, to
}

// parseAnalyticsFilter extracts the common analytics filter
// params from a request.
func parseAnalyticsFilter(
	w http.ResponseWriter, r *http.Request,
) (db.AnalyticsFilter, bool) {
	q := r.URL.Query()
	tz := q.Get("timezone")
	if tz == "" {
		tz = "UTC"
	}
	if _, err := time.LoadLocation(tz); err != nil {
		writeError(w, http.StatusBadRequest,
			"invalid timezone: "+tz)
		return db.AnalyticsFilter{}, false
	}

	from, to := defaultDateRange(q.Get("from"), q.Get("to"))

	if !timeutil.IsValidDate(from) || !timeutil.IsValidDate(to) {
		writeError(w, http.StatusBadRequest,
			"invalid date format: use YYYY-MM-DD")
		return db.AnalyticsFilter{}, false
	}
	if from > to {
		writeError(w, http.StatusBadRequest,
			"from must not be after to")
		return db.AnalyticsFilter{}, false
	}

	var dow *int
	if s := q.Get("dow"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 || v > 6 {
			writeError(w, http.StatusBadRequest,
				"dow must be 0-6 (Mon=0, Sun=6)")
			return db.AnalyticsFilter{}, false
		}
		dow = &v
	}

	var hour *int
	if s := q.Get("hour"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 || v > 23 {
			writeError(w, http.StatusBadRequest,
				"hour must be 0-23")
			return db.AnalyticsFilter{}, false
		}
		hour = &v
	}

	minUserMsgs, ok := parseIntParam(w, r, "min_user_messages")
	if !ok {
		return db.AnalyticsFilter{}, false
	}

	activeSince := q.Get("active_since")
	if activeSince != "" && !timeutil.IsValidTimestamp(activeSince) {
		writeError(w, http.StatusBadRequest,
			"invalid active_since: use RFC3339 timestamp")
		return db.AnalyticsFilter{}, false
	}

	includeOneShot := q.Get("include_one_shot") == "true"
	includeAutomated := q.Get("include_automated") == "true"

	return db.AnalyticsFilter{
		From:             from,
		To:               to,
		Machine:          q.Get("machine"),
		Project:          q.Get("project"),
		Agent:            q.Get("agent"),
		Timezone:         tz,
		DayOfWeek:        dow,
		Hour:             hour,
		MinUserMessages:  minUserMsgs,
		ExcludeOneShot:   !includeOneShot,
		ExcludeAutomated: !includeAutomated,
		ActiveSince:      activeSince,
		Termination:      q.Get("termination"),
	}, true
}

func (s *Server) handleAnalyticsSummary(
	w http.ResponseWriter, r *http.Request,
) {
	f, ok := parseAnalyticsFilter(w, r)
	if !ok {
		return
	}

	result, err := s.db.GetAnalyticsSummary(r.Context(), f)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		log.Printf("analytics error: %v", err)
		writeError(w, http.StatusInternalServerError,
			"internal server error")
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleAnalyticsActivity(
	w http.ResponseWriter, r *http.Request,
) {
	f, ok := parseAnalyticsFilter(w, r)
	if !ok {
		return
	}

	granularity := r.URL.Query().Get("granularity")
	if granularity == "" {
		granularity = "day"
	}
	switch granularity {
	case "day", "week", "month":
		// valid
	default:
		writeError(w, http.StatusBadRequest,
			"invalid granularity: must be day, week, or month")
		return
	}

	result, err := s.db.GetAnalyticsActivity(
		r.Context(), f, granularity,
	)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		log.Printf("analytics error: %v", err)
		writeError(w, http.StatusInternalServerError,
			"internal server error")
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleAnalyticsHeatmap(
	w http.ResponseWriter, r *http.Request,
) {
	f, ok := parseAnalyticsFilter(w, r)
	if !ok {
		return
	}

	metric := r.URL.Query().Get("metric")
	if metric == "" {
		metric = "messages"
	}
	switch metric {
	case "messages", "sessions", "output_tokens":
		// valid
	default:
		writeError(w, http.StatusBadRequest,
			"invalid metric: must be messages, sessions, or output_tokens")
		return
	}

	result, err := s.db.GetAnalyticsHeatmap(
		r.Context(), f, metric,
	)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		log.Printf("analytics error: %v", err)
		writeError(w, http.StatusInternalServerError,
			"internal server error")
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleAnalyticsProjects(
	w http.ResponseWriter, r *http.Request,
) {
	f, ok := parseAnalyticsFilter(w, r)
	if !ok {
		return
	}

	result, err := s.db.GetAnalyticsProjects(r.Context(), f)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		log.Printf("analytics error: %v", err)
		writeError(w, http.StatusInternalServerError,
			"internal server error")
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleAnalyticsHourOfWeek(
	w http.ResponseWriter, r *http.Request,
) {
	f, ok := parseAnalyticsFilter(w, r)
	if !ok {
		return
	}

	result, err := s.db.GetAnalyticsHourOfWeek(
		r.Context(), f,
	)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		log.Printf("analytics error: %v", err)
		writeError(w, http.StatusInternalServerError,
			"internal server error")
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleAnalyticsSessionShape(
	w http.ResponseWriter, r *http.Request,
) {
	f, ok := parseAnalyticsFilter(w, r)
	if !ok {
		return
	}

	result, err := s.db.GetAnalyticsSessionShape(
		r.Context(), f,
	)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		log.Printf("analytics error: %v", err)
		writeError(w, http.StatusInternalServerError,
			"internal server error")
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleAnalyticsTools(
	w http.ResponseWriter, r *http.Request,
) {
	f, ok := parseAnalyticsFilter(w, r)
	if !ok {
		return
	}

	result, err := s.db.GetAnalyticsTools(r.Context(), f)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		log.Printf("analytics error: %v", err)
		writeError(w, http.StatusInternalServerError,
			"internal server error")
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleAnalyticsVelocity(
	w http.ResponseWriter, r *http.Request,
) {
	f, ok := parseAnalyticsFilter(w, r)
	if !ok {
		return
	}

	result, err := s.db.GetAnalyticsVelocity(
		r.Context(), f,
	)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		log.Printf("analytics error: %v", err)
		writeError(w, http.StatusInternalServerError,
			"internal server error")
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleAnalyticsSignals(
	w http.ResponseWriter, r *http.Request,
) {
	f, ok := parseAnalyticsFilter(w, r)
	if !ok {
		return
	}

	result, err := s.db.GetAnalyticsSignals(r.Context(), f)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		log.Printf("analytics signals error: %v", err)
		writeError(w, http.StatusInternalServerError,
			"internal server error")
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleAnalyticsTopSessions(
	w http.ResponseWriter, r *http.Request,
) {
	f, ok := parseAnalyticsFilter(w, r)
	if !ok {
		return
	}

	metric := r.URL.Query().Get("metric")
	if metric == "" {
		metric = "messages"
	}
	switch metric {
	case "messages", "duration", "output_tokens":
		// valid
	default:
		writeError(w, http.StatusBadRequest,
			"invalid metric: must be messages, duration, or output_tokens")
		return
	}

	result, err := s.db.GetAnalyticsTopSessions(
		r.Context(), f, metric,
	)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		log.Printf("analytics error: %v", err)
		writeError(w, http.StatusInternalServerError,
			"internal server error")
		return
	}

	writeJSON(w, http.StatusOK, result)
}
