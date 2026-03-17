package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/insight"
)

var validInsightTypes = map[string]bool{
	"daily_activity": true,
	"agent_analysis": true,
}

func (s *Server) handleListInsights(
	w http.ResponseWriter, r *http.Request,
) {
	q := r.URL.Query()

	typ := q.Get("type")
	if typ != "" && !validInsightTypes[typ] {
		writeError(w, http.StatusBadRequest,
			"invalid type: must be daily_activity or agent_analysis")
		return
	}

	filter := db.InsightFilter{
		Type:    typ,
		Project: q.Get("project"),
	}

	insights, err := s.db.ListInsights(
		r.Context(), filter,
	)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		writeError(
			w, http.StatusInternalServerError, err.Error(),
		)
		return
	}
	if insights == nil {
		insights = []db.Insight{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"insights": insights,
	})
}

func (s *Server) handleGetInsight(
	w http.ResponseWriter, r *http.Request,
) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	result, err := s.db.GetInsight(r.Context(), id)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		writeError(
			w, http.StatusInternalServerError, err.Error(),
		)
		return
	}
	if result == nil {
		writeError(w, http.StatusNotFound, "insight not found")
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleDeleteInsight(
	w http.ResponseWriter, r *http.Request,
) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	existing, err := s.db.GetInsight(r.Context(), id)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		writeError(
			w, http.StatusInternalServerError, err.Error(),
		)
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "insight not found")
		return
	}

	if err := s.db.DeleteInsight(id); err != nil {
		if handleReadOnly(w, err) {
			return
		}
		writeError(
			w, http.StatusInternalServerError, err.Error(),
		)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

type generateInsightRequest struct {
	Type     string `json:"type"`
	DateFrom string `json:"date_from"`
	DateTo   string `json:"date_to"`
	Project  string `json:"project"`
	Prompt   string `json:"prompt"`
	Agent    string `json:"agent"`
}

func insightGenerateClientMessage(agent string) string {
	return fmt.Sprintf("%s generation failed", agent)
}

func (s *Server) handleGenerateInsight(
	w http.ResponseWriter, r *http.Request,
) {
	if s.db.ReadOnly() {
		writeError(w, http.StatusNotImplemented,
			"insight generation is not available in read-only mode")
		return
	}

	var req generateInsightRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest,
			"invalid JSON body")
		return
	}

	if !validInsightTypes[req.Type] {
		writeError(w, http.StatusBadRequest,
			"invalid type: must be daily_activity or agent_analysis")
		return
	}
	if !isValidDate(req.DateFrom) {
		writeError(w, http.StatusBadRequest,
			"invalid date_from: use YYYY-MM-DD")
		return
	}
	if !isValidDate(req.DateTo) {
		writeError(w, http.StatusBadRequest,
			"invalid date_to: use YYYY-MM-DD")
		return
	}
	if req.DateTo < req.DateFrom {
		writeError(w, http.StatusBadRequest,
			"date_to must be >= date_from")
		return
	}

	if req.Agent == "" {
		req.Agent = "claude"
	}
	if !insight.ValidAgents[req.Agent] {
		writeError(w, http.StatusBadRequest,
			"invalid agent: must be claude, codex, or gemini")
		return
	}

	stream, err := NewSSEStream(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"streaming not supported")
		return
	}

	var streamMu sync.Mutex
	sendJSON := func(event string, v any) bool {
		streamMu.Lock()
		defer streamMu.Unlock()
		return stream.SendJSON(event, v)
	}

	if !sendJSON("status", map[string]string{
		"phase": "generating",
	}) {
		return
	}

	prompt, err := insight.BuildPrompt(
		r.Context(), s.db, insight.GenerateRequest{
			Type:     req.Type,
			DateFrom: req.DateFrom,
			DateTo:   req.DateTo,
			Project:  req.Project,
			Prompt:   req.Prompt,
		},
	)
	if err != nil {
		log.Printf("insight prompt error: %v", err)
		sendJSON("error", map[string]string{
			"message": "failed to build prompt",
		})
		return
	}

	genCtx, cancel := context.WithTimeout(
		r.Context(), 10*time.Minute,
	)
	defer cancel()

	const (
		maxBufferedLogEvents = 256
		logDrainTimeout      = 2 * time.Second
		logStopWaitTimeout   = 500 * time.Millisecond
	)
	logCh := make(chan insight.LogEvent, maxBufferedLogEvents)
	logDone := make(chan struct{})
	logStop := make(chan struct{})
	var logStopOnce sync.Once
	stopLogSender := func() {
		logStopOnce.Do(func() { close(logStop) })
	}
	go func() {
		defer close(logDone)
		for {
			select {
			case <-logStop:
				return
			default:
			}
			select {
			case <-logStop:
				return
			case ev, ok := <-logCh:
				if !ok {
					return
				}
				if !sendJSON("log", ev) {
					stopLogSender()
					return
				}
			}
		}
	}()
	var (
		logStateMu    sync.Mutex
		logStreamDone bool
		droppedLogs   int
	)
	enqueueLog := func(ev insight.LogEvent) {
		logStateMu.Lock()
		defer logStateMu.Unlock()
		if logStreamDone {
			return
		}
		select {
		case logCh <- ev:
		default:
			droppedLogs++
		}
	}
	finishLogStream := func() (dropped int, drained bool, senderStopped bool, timedOut bool) {
		logStateMu.Lock()
		logStreamDone = true
		close(logCh)
		dropped = droppedLogs
		logStateMu.Unlock()
		select {
		case <-logDone:
			return dropped, true, true, false
		case <-time.After(logDrainTimeout):
			log.Printf(
				"insight log stream drain timed out after %s",
				logDrainTimeout,
			)
			// Count remaining buffered events as dropped since they will
			// not be delivered once we abort the stream.
			dropped += len(logCh)
			stopLogSender()
			select {
			case <-logDone:
				return dropped, false, true, true
			case <-time.After(logStopWaitTimeout):
				log.Printf(
					"insight log sender stop timed out after %s",
					logStopWaitTimeout,
				)
				// Try to force-unblock any in-flight writer and wait one
				// more bounded interval for sender shutdown.
				stream.ForceWriteDeadlineNow()
				select {
				case <-logDone:
					return dropped, false, true, true
				case <-time.After(logStopWaitTimeout):
					log.Printf(
						"insight log sender did not stop after forced deadline",
					)
					return dropped, false, false, true
				}
			}
		}
	}

	result, err := s.generateStreamFunc(
		genCtx, req.Agent, prompt,
		enqueueLog,
	)
	dropped, drained, senderStopped, timedOut := finishLogStream()
	if !senderStopped {
		stream.ForceWriteDeadlineNow()
		log.Printf("insight log stream sender did not stop; aborting terminal SSE events")
		return
	}
	if dropped > 0 {
		suffix := "due to slow client"
		if timedOut {
			suffix = "due to slow client and log stream timeout"
		}
		sendJSON("log", insight.LogEvent{
			Stream: "stderr",
			Line: fmt.Sprintf(
				"dropped %d log line(s) %s", dropped, suffix,
			),
		})
	}
	if timedOut || !drained {
		log.Printf("insight log stream did not fully drain before completion")
		sendJSON("error", map[string]string{
			"message": "insight log stream timed out before completion",
		})
		return
	}
	if err != nil {
		log.Printf("insight generate error: %v", err)
		sendJSON("error", map[string]string{
			"message": insightGenerateClientMessage(req.Agent),
		})
		return
	}

	if strings.TrimSpace(result.Content) == "" {
		sendJSON("error", map[string]string{
			"message": "agent returned empty content",
		})
		return
	}

	var project *string
	if req.Project != "" {
		project = &req.Project
	}
	var model *string
	if result.Model != "" {
		model = &result.Model
	}
	var promptPtr *string
	if req.Prompt != "" {
		promptPtr = &req.Prompt
	}

	id, err := s.db.InsertInsight(db.Insight{
		Type:     req.Type,
		DateFrom: req.DateFrom,
		DateTo:   req.DateTo,
		Project:  project,
		Agent:    result.Agent,
		Model:    model,
		Prompt:   promptPtr,
		Content:  result.Content,
	})
	if err != nil {
		log.Printf("insight insert error: %v", err)
		sendJSON("error", map[string]string{
			"message": "failed to save insight",
		})
		return
	}

	saved, err := s.db.GetInsight(r.Context(), id)
	if err != nil || saved == nil {
		log.Printf("insight get error: id=%d err=%v",
			id, err)
		sendJSON("error", map[string]string{
			"message": "failed to retrieve saved insight",
		})
		return
	}

	sendJSON("done", saved)
}
