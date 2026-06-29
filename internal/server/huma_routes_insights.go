package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	stdsync "sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/insight"
	"go.kenn.io/agentsview/internal/timeutil"
)

func (s *Server) registerInsightsRoutes() {
	group := newRouteGroup(s.api, "/api/v1/insights", "Insights")

	get(s, group, "", "List insights", s.humaListInsights)
	get(s, group, "/{id}", "Get insight", s.humaGetInsight)
	raw(s, group, http.MethodGet, "/{id}/export", "Export insight as HTML", s.humaExportInsight)
	raw(s, group, http.MethodGet, "/{id}/md", "Export insight as Markdown", s.humaMarkdownInsight)
	post(s, group, "/{id}/publish", "Publish insight", s.humaPublishInsight)
	deleteRoute(s, group, "/{id}", "Delete insight", s.humaDeleteInsight)
	stream(s, group, http.MethodPost, "/generate", "Generate insight", s.humaGenerateInsight)
}

type insightType string

type insightsInput struct {
	Type     insightType `query:"type" enum:"daily_activity,agent_analysis,llm_canned" doc:"Insight type"`
	Project  string      `query:"project" doc:"Filter by project"`
	DateFrom string      `query:"date_from" format:"date" doc:"Filter date_from >= (YYYY-MM-DD)"`
	DateTo   string      `query:"date_to" format:"date" doc:"Filter date_to <= (YYYY-MM-DD)"`
}

type insightsResponse struct {
	Insights []db.Insight `json:"insights"`
}

type generateInsightInput struct {
	Body generateInsightRequest
}

type publishInsightInput struct {
	ID     int64 `path:"id" required:"true" doc:"Insight ID"`
	Secret bool  `query:"secret" doc:"Create a secret gist instead of a public one"`
}

type insightGenerationCapableStore interface {
	InsightGenerationAvailable() bool
}

func supportsInsightGeneration(store db.Store) bool {
	if store == nil {
		return false
	}
	if !store.ReadOnly() {
		return true
	}
	capable, ok := store.(insightGenerationCapableStore)
	return ok && capable.InsightGenerationAvailable()
}

func (s *Server) humaListInsights(
	ctx context.Context,
	in *insightsInput,
) (*jsonOutput[insightsResponse], error) {
	if err := validateDateFilterValues("", in.DateFrom, in.DateTo, ""); err != nil {
		return nil, err
	}
	insights, err := s.db.ListInsights(ctx, db.InsightFilter{
		Type:     string(in.Type),
		Project:  in.Project,
		DateFrom: in.DateFrom,
		DateTo:   in.DateTo,
	})
	if err != nil {
		return nil, serverError(err)
	}
	if insights == nil {
		insights = []db.Insight{}
	}
	return &jsonOutput[insightsResponse]{
		Body: insightsResponse{Insights: insights},
	}, nil
}

func (s *Server) humaGetInsight(
	ctx context.Context,
	in *intIDPathInput,
) (*jsonOutput[*db.Insight], error) {
	result, err := s.db.GetInsight(ctx, in.ID)
	if err != nil {
		return nil, serverError(err)
	}
	if result == nil {
		return nil, apiError(http.StatusNotFound, "insight not found")
	}
	return &jsonOutput[*db.Insight]{Body: result}, nil
}

func (s *Server) insightByID(
	ctx context.Context,
	id int64,
) (*db.Insight, error) {
	result, err := s.db.GetInsight(ctx, id)
	if err != nil {
		return nil, serverError(err)
	}
	if result == nil {
		return nil, apiError(http.StatusNotFound, "insight not found")
	}
	return result, nil
}

func (s *Server) humaExportInsight(
	ctx context.Context,
	in *intIDPathInput,
) (*bytesOutput, error) {
	insight, err := s.insightByID(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	return &bytesOutput{
		ContentType:        "text/html; charset=utf-8",
		ContentDisposition: fmt.Sprintf(`attachment; filename="%s"`, insightExportHTMLFilename(insight)),
		Body:               []byte(generateInsightExportHTML(insight)),
	}, nil
}

func (s *Server) humaMarkdownInsight(
	ctx context.Context,
	in *intIDPathInput,
) (*bytesOutput, error) {
	insight, err := s.insightByID(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	return &bytesOutput{
		ContentType:        "text/markdown; charset=utf-8",
		ContentDisposition: fmt.Sprintf(`inline; filename="%s"`, insightExportMarkdownFilename(insight)),
		Body:               []byte(insight.Content),
	}, nil
}

func (s *Server) humaPublishInsight(
	ctx context.Context,
	in *publishInsightInput,
) (*jsonOutput[publishResponse], error) {
	token := s.githubToken(ctx)
	if token == "" {
		return nil, apiError(http.StatusUnauthorized, "GitHub token not configured")
	}
	insight, err := s.insightByID(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	resp, err := publishExportHTML(
		ctx,
		token,
		insightExportHTMLFilename(insight),
		insightPublishDescription(insight),
		generateInsightExportHTML(insight),
		!in.Secret,
	)
	if err != nil {
		return nil, apiError(http.StatusBadGateway, err.Error())
	}
	return &jsonOutput[publishResponse]{Body: *resp}, nil
}

func (s *Server) humaDeleteInsight(
	ctx context.Context,
	in *intIDPathInput,
) (*noContentOutput, error) {
	if _, err := s.insightByID(ctx, in.ID); err != nil {
		return nil, err
	}
	if err := s.db.DeleteInsight(in.ID); err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, serverError(err)
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

func (s *Server) humaGenerateInsight(
	ctx context.Context,
	in *generateInsightInput,
) (*huma.StreamResponse, error) {
	if !supportsInsightGeneration(s.db) {
		return nil, apiError(http.StatusNotImplemented,
			"insight generation is not available in read-only mode")
	}
	req := in.Body
	if !validInsightTypes[req.Type] {
		return nil, apiError(http.StatusBadRequest,
			"invalid type: must be daily_activity, agent_analysis, or llm_canned")
	}
	if req.Type == insight.CannedType {
		return s.humaGenerateCannedInsight(req)
	}
	if !timeutil.IsValidDate(req.DateFrom) {
		return nil, apiError(http.StatusBadRequest,
			"invalid date_from: use YYYY-MM-DD")
	}
	if !timeutil.IsValidDate(req.DateTo) {
		return nil, apiError(http.StatusBadRequest,
			"invalid date_to: use YYYY-MM-DD")
	}
	if req.DateTo < req.DateFrom {
		return nil, apiError(http.StatusBadRequest,
			"date_to must be >= date_from")
	}
	if req.Agent == "" {
		req.Agent = "claude"
	}
	if !insight.ValidAgents[req.Agent] {
		return nil, apiError(http.StatusBadRequest,
			"invalid agent: must be one of "+
				strings.Join(insight.ValidAgentNames, ", "))
	}
	scope, ok := normalizeInsightAutomatedScope(req.AutomatedScope)
	if !ok {
		return nil, apiError(http.StatusBadRequest,
			"automated_scope must be human, all, or automated")
	}
	req.AutomatedScope = scope
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		stream, ok := newHumaSSEStream(hctx)
		if !ok {
			writeHumaJSON(hctx, http.StatusInternalServerError,
				apiErrorResponse{Message: "streaming not supported"})
			return
		}
		var streamMu stdsync.Mutex
		sendJSON := func(event string, v any) bool {
			streamMu.Lock()
			defer streamMu.Unlock()
			return stream.SendJSON(event, v)
		}
		if !sendJSON("status", map[string]string{"phase": "generating"}) {
			return
		}
		genReq := insight.GenerateRequest{
			Type:           req.Type,
			DateFrom:       req.DateFrom,
			DateTo:         req.DateTo,
			Project:        req.Project,
			Prompt:         req.Prompt,
			AutomatedScope: req.AutomatedScope,
		}
		// Attach the activity summary for any valid range, single day
		// included: daily_activity insights are commonly one day and the
		// summary (concurrency, peak, breakdowns) is exactly that day's
		// overview. The validator above guarantees DateTo >= DateFrom.
		if req.DateTo >= req.DateFrom {
			summary, err := s.activityRangeSummary(hctx.Context(), req)
			if err != nil {
				log.Printf("insight activity summary error: %v", err)
			} else {
				genReq.Summary = summary
			}
		}
		prompt, err := insight.BuildPrompt(hctx.Context(), s.db, genReq)
		if err != nil {
			log.Printf("insight prompt error: %v", err)
			sendJSON("error", map[string]string{"message": "failed to build prompt"})
			return
		}
		genCtx, cancel := context.WithTimeout(hctx.Context(), 10*time.Minute)
		defer cancel()

		const (
			maxBufferedLogEvents = 256
		)
		logDrainTimeout := s.insightLogDrainTimeout
		logStopWaitTimeout := s.insightLogStopWaitTimeout
		logCh := make(chan insight.LogEvent, maxBufferedLogEvents)
		logDone := make(chan struct{})
		logStop := make(chan struct{})
		var logStopOnce stdsync.Once
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
			logStateMu    stdsync.Mutex
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
				log.Printf("insight log stream drain timed out after %s", logDrainTimeout)
				dropped += len(logCh)
				stopLogSender()
				select {
				case <-logDone:
					return dropped, false, true, true
				case <-time.After(logStopWaitTimeout):
					log.Printf("insight log sender stop timed out after %s", logStopWaitTimeout)
					stream.ForceWriteDeadlineNow()
					select {
					case <-logDone:
						return dropped, false, true, true
					case <-time.After(logStopWaitTimeout):
						log.Printf("insight log sender did not stop after forced deadline")
						return dropped, false, false, true
					}
				}
			}
		}

		result, err := s.generateStreamFunc(genCtx, req.Agent, prompt, enqueueLog)
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
				Line:   fmt.Sprintf("dropped %d log line(s) %s", dropped, suffix),
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
				"message": insightGenerateClientMessage(req.Agent, err),
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
			sendJSON("error", map[string]string{"message": "failed to save insight"})
			return
		}
		saved, err := s.db.GetInsight(hctx.Context(), id)
		if err != nil || saved == nil {
			log.Printf("insight get error: id=%d err=%v", id, err)
			sendJSON("error", map[string]string{
				"message": "failed to retrieve saved insight",
			})
			return
		}
		sendJSON("done", saved)
	}}, nil
}

// activityRangeSummary resolves the requested range into an activity report and
// condenses it into a RangeSummary for the insight prompt. The range spans the
// local days [DateFrom, DateTo] in req.Timezone (empty means UTC): the bounds
// are that zone's midnights, matching the activity dashboard the dates were
// derived from, so a non-UTC viewer's summary covers the window the dashboard
// shows rather than a UTC-shifted one. It applies the same automated-session
// scope as BuildPrompt's session list so the summary reflects the same work the
// prompt focuses on; the two otherwise select sessions differently (this uses
// the activity report's half-open window with an ended_at fallback, BuildPrompt
// uses ListSessions' calendar-date match on the start date), so the summary is a
// range-level overview, not a row-for-row mirror of BuildPrompt's session list.
func (s *Server) activityRangeSummary(
	ctx context.Context, req generateInsightRequest,
) (*insight.RangeSummary, error) {
	tz := req.Timezone
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("loading timezone %q: %w", tz, err)
	}
	// Local midnights of DateFrom and the day after DateTo bound the half-open
	// window, expressed as absolute instants so ResolveQuery's custom-range
	// parse keeps them exact; Timezone drives only the bucket calendar.
	from, err := time.ParseInLocation("2006-01-02", req.DateFrom, loc)
	if err != nil {
		return nil, fmt.Errorf("parsing date_from %q: %w", req.DateFrom, err)
	}
	toDay, err := time.ParseInLocation("2006-01-02", req.DateTo, loc)
	if err != nil {
		return nil, fmt.Errorf("parsing date_to %q: %w", req.DateTo, err)
	}
	to := toDay.AddDate(0, 0, 1)
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset:   "custom",
		From:     from.UTC().Format(time.RFC3339),
		To:       to.UTC().Format(time.RFC3339),
		Timezone: tz,
	}, time.Now())
	if err != nil {
		return nil, fmt.Errorf("resolving activity range: %w", err)
	}
	r, err := s.db.GetActivityReport(ctx, db.AnalyticsFilter{
		Timezone:       tz,
		Project:        req.Project,
		AutomatedScope: req.AutomatedScope,
	}, q)
	if err != nil {
		return nil, fmt.Errorf("activity report: %w", err)
	}
	summary := insight.SummarizeReport(r, 10)
	return &summary, nil
}
