package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/insight"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/timeutil"
)

var validInsightTypes = map[string]bool{
	"daily_activity":   true,
	"agent_analysis":   true,
	insight.CannedType: true,
}

type generateInsightRequest struct {
	Type           string                        `json:"type"`
	DateFrom       string                        `json:"date_from"`
	DateTo         string                        `json:"date_to"`
	Project        string                        `json:"project,omitempty"`
	Prompt         string                        `json:"prompt,omitempty"`
	SessionID      string                        `json:"session_id,omitempty"`
	Agent          string                        `json:"agent,omitempty"`
	Kind           string                        `json:"kind,omitempty"`
	LLMOptIn       bool                          `json:"llm_opt_in,omitempty"`
	ForceRefresh   bool                          `json:"force_refresh,omitempty"`
	AutomatedScope string                        `json:"automated_scope,omitempty"`
	Filters        *insight.CannedSessionFilters `json:"filters,omitempty"`
	// Timezone is the IANA zone the caller's date range is expressed in, so
	// the attached activity summary covers the same local-day window as the
	// activity dashboard the dates were derived from. Empty means UTC.
	Timezone string `json:"timezone,omitempty"`
}

func normalizeInsightAutomatedScope(scope string) (string, bool) {
	switch strings.TrimSpace(scope) {
	case "", "human":
		return "human", true
	case "all", "automated":
		return strings.TrimSpace(scope), true
	default:
		return "", false
	}
}

func normalizeCannedSessionFilters(
	req generateInsightRequest,
) (insight.CannedSessionFilters, string, bool) {
	requestTimezone := strings.TrimSpace(req.Timezone)
	filters := insight.CannedSessionFilters{
		Timezone:       requestTimezone,
		AutomatedScope: req.AutomatedScope,
	}
	if req.Filters != nil {
		filters = *req.Filters
	}
	filters.Timezone = strings.TrimSpace(filters.Timezone)
	if filters.Timezone == "" {
		filters.Timezone = requestTimezone
	}
	if filters.Timezone == "" {
		filters.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(filters.Timezone); err != nil {
		return insight.CannedSessionFilters{},
			"invalid timezone: " + filters.Timezone, false
	}
	filters.Machine = strings.TrimSpace(filters.Machine)
	filters.Agent = strings.TrimSpace(filters.Agent)
	filters.Termination = strings.TrimSpace(filters.Termination)
	filters.ActiveSince = strings.TrimSpace(filters.ActiveSince)
	if filters.MinUserMessages < 0 {
		return insight.CannedSessionFilters{},
			"filters.min_user_messages must be >= 0", false
	}
	if filters.ActiveSince != "" &&
		!timeutil.IsValidTimestamp(filters.ActiveSince) {
		return insight.CannedSessionFilters{},
			"filters.active_since must be RFC3339 timestamp", false
	}
	scopeInput := filters.AutomatedScope
	if strings.TrimSpace(scopeInput) == "" {
		scopeInput = req.AutomatedScope
	}
	scope, ok := normalizeInsightAutomatedScope(scopeInput)
	if !ok {
		return insight.CannedSessionFilters{},
			"filters.automated_scope must be human, all, or automated",
			false
	}
	filters.AutomatedScope = scope
	return filters, "", true
}

func insightGenerateClientMessage(
	agent string, err error,
) string {
	if err == nil {
		return fmt.Sprintf("%s generation failed", agent)
	}
	msg := err.Error()
	// Strip stderr dump after newline for the short client message; full details
	// are in the log stream.
	if idx := strings.Index(msg, "\nstderr:"); idx > 0 {
		msg = msg[:idx]
	}
	if idx := strings.Index(msg, "\nraw:"); idx > 0 {
		msg = msg[:idx]
	}
	return msg
}

func (s *Server) humaGenerateCannedInsight(
	req generateInsightRequest,
) (*huma.StreamResponse, error) {
	req.Prompt = strings.TrimSpace(req.Prompt)
	kind := insight.CannedKind(req.Kind)
	if !insight.ValidCannedKinds[kind] {
		return nil, apiError(http.StatusBadRequest,
			"invalid kind: unsupported canned insight")
	}
	if req.Type != "" && req.Type != insight.CannedType {
		return nil, apiError(http.StatusBadRequest,
			"type must be llm_canned for canned insights")
	}
	if !req.LLMOptIn {
		return nil, apiError(http.StatusBadRequest,
			"llm_opt_in must be true for canned insights")
	}
	if len([]rune(req.Prompt)) > insight.MaxCannedFocusRunes {
		return nil, apiError(http.StatusBadRequest,
			"prompt is too long for canned insight focus")
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
	filters, message, ok := normalizeCannedSessionFilters(req)
	if !ok {
		return nil, apiError(http.StatusBadRequest, message)
	}
	req.Filters = &filters
	req.AutomatedScope = filters.AutomatedScope

	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		stream, ok := newHumaSSEStream(hctx)
		if !ok {
			writeHumaJSON(hctx, http.StatusInternalServerError,
				apiErrorResponse{Message: "streaming not supported"})
			return
		}
		s.generateCannedInsight(hctx.Context(), stream, kind, req)
	}}, nil
}

func (s *Server) generateCannedInsight(
	ctx context.Context,
	stream *SSEStream,
	kind insight.CannedKind,
	req generateInsightRequest,
) {
	sendJSON := func(event string, v any) bool {
		return stream.SendJSON(event, v)
	}
	status := func(phase string) bool {
		return sendJSON("status", map[string]string{
			"phase": phase,
		})
	}

	if !status("building_payload") {
		return
	}
	payload, aggregateHash, cacheKey, err := s.buildCannedPayload(
		ctx, kind, req,
	)
	if err != nil {
		log.Printf("canned insight payload error: %v", err)
		sendJSON("error", map[string]string{
			"message": "failed to build canned insight payload",
		})
		return
	}

	if !req.ForceRefresh {
		cached, err := s.db.GetCachedInsight(ctx, cacheKey)
		if err != nil {
			log.Printf("canned insight cache lookup error: %v", err)
			sendJSON("error", map[string]string{
				"message": "failed to check insight cache",
			})
			return
		}
		if cached != nil {
			markInsightCacheHit(cached)
			if !status("cache_hit") {
				return
			}
			sendJSON("done", cached)
			return
		}
	}

	prompt, err := insight.BuildCannedPrompt(payload, aggregateHash)
	if err != nil {
		log.Printf("canned insight prompt error: %v", err)
		sendJSON("error", map[string]string{
			"message": "failed to build canned prompt",
		})
		return
	}

	if !status("generating") {
		return
	}
	genCtx, cancel := context.WithTimeout(
		ctx, 3*time.Minute,
	)
	defer cancel()
	result, err := s.generateStreamFunc(
		genCtx, req.Agent, prompt, nil,
	)
	if err != nil {
		log.Printf("canned insight generate error: %v", err)
		sendJSON("error", map[string]string{
			"message": insightGenerateClientMessage(
				req.Agent, err,
			),
		})
		return
	}

	if !status("validating") {
		return
	}
	envelope, err := insight.ParseCannedEnvelope(result.Content)
	if err != nil {
		log.Printf("canned insight parse error: %v", err)
		sendJSON("error", map[string]string{
			"message": "generated insight was not valid JSON",
		})
		return
	}
	if err := insight.ValidateCannedEnvelope(envelope, payload); err != nil {
		log.Printf("canned insight validation error: %v", err)
		sendJSON("error", map[string]string{
			"message": fmt.Sprintf(
				"generated insight failed validation: %v", err,
			),
		})
		return
	}

	if !status("saving") {
		return
	}
	model := result.Model
	prov, err := insight.NewCannedProvenance(
		payload, aggregateHash, cacheKey, "fresh",
		result.Agent, model, time.Now(),
	)
	if err != nil {
		log.Printf("canned insight provenance error: %v", err)
		sendJSON("error", map[string]string{
			"message": "failed to build insight provenance",
		})
		return
	}
	provJSON, err := json.Marshal(prov)
	if err != nil {
		log.Printf("canned insight provenance JSON error: %v", err)
		sendJSON("error", map[string]string{
			"message": "failed to encode insight provenance",
		})
		return
	}
	structuredJSON, err := json.Marshal(envelope)
	if err != nil {
		log.Printf("canned insight structured JSON error: %v", err)
		sendJSON("error", map[string]string{
			"message": "failed to encode generated insight",
		})
		return
	}

	var project *string
	if req.Project != "" {
		project = &req.Project
	}
	var modelPtr *string
	if model != "" {
		modelPtr = &model
	}
	var promptPtr *string
	if req.Prompt != "" {
		promptPtr = &req.Prompt
	}

	id, err := s.db.InsertInsight(db.Insight{
		Type:            insight.CannedType,
		DateFrom:        req.DateFrom,
		DateTo:          req.DateTo,
		Project:         project,
		Agent:           result.Agent,
		Model:           modelPtr,
		Prompt:          promptPtr,
		Content:         insight.RenderCannedMarkdown(envelope, prov),
		Kind:            string(kind),
		SchemaVersion:   insight.CannedSchemaVersion,
		TemplateID:      prov.TemplateID,
		TemplateVersion: prov.TemplateVersion,
		AggregateHash:   aggregateHash,
		CacheKey:        cacheKey,
		CacheStatus:     "fresh",
		ProvenanceJSON:  string(provJSON),
		StructuredJSON:  string(structuredJSON),
	})
	if err != nil {
		log.Printf("canned insight insert error: %v", err)
		sendJSON("error", map[string]string{
			"message": "failed to save insight",
		})
		return
	}

	saved, err := s.db.GetInsight(ctx, id)
	if err != nil || saved == nil {
		log.Printf("canned insight get error: id=%d err=%v",
			id, err)
		sendJSON("error", map[string]string{
			"message": "failed to retrieve saved insight",
		})
		return
	}
	sendJSON("done", saved)
}

func markInsightCacheHit(s *db.Insight) {
	if s == nil {
		return
	}
	s.CacheStatus = "hit"
	if strings.TrimSpace(s.ProvenanceJSON) == "" {
		return
	}
	var prov map[string]any
	if err := json.Unmarshal([]byte(s.ProvenanceJSON), &prov); err != nil {
		return
	}
	if prov == nil {
		return
	}
	prov["cache_status"] = "hit"
	data, err := json.Marshal(prov)
	if err != nil {
		return
	}
	s.ProvenanceJSON = string(data)
}

func (s *Server) buildCannedPayload(
	ctx context.Context,
	kind insight.CannedKind,
	req generateInsightRequest,
) (insight.CannedAggregatePayload, string, string, error) {
	filters := insight.CannedSessionFilters{
		Timezone:       "UTC",
		AutomatedScope: req.AutomatedScope,
	}
	if req.Filters != nil {
		filters = *req.Filters
	}
	analyticsFilter := db.AnalyticsFilter{
		From:            req.DateFrom,
		To:              req.DateTo,
		Project:         req.Project,
		Machine:         filters.Machine,
		Agent:           filters.Agent,
		Timezone:        filters.Timezone,
		MinUserMessages: filters.MinUserMessages,
		ExcludeOneShot:  !filters.IncludeOneShot,
		AutomatedScope:  filters.AutomatedScope,
		ActiveSince:     filters.ActiveSince,
		Termination:     filters.Termination,
	}
	signals, err := s.db.GetAnalyticsSignals(ctx, analyticsFilter)
	if err != nil {
		return insight.CannedAggregatePayload{}, "", "", err
	}

	usageFilter := db.UsageFilter{
		From:            req.DateFrom,
		To:              req.DateTo,
		Project:         req.Project,
		Machine:         filters.Machine,
		Agent:           filters.Agent,
		Timezone:        filters.Timezone,
		MinUserMessages: filters.MinUserMessages,
		ExcludeOneShot:  !filters.IncludeOneShot,
		AutomatedScope:  filters.AutomatedScope,
		ActiveSince:     filters.ActiveSince,
		Termination:     filters.Termination,
		Breakdowns:      false,
	}
	usageResult, err := s.db.GetDailyUsage(ctx, usageFilter)
	if err != nil {
		return insight.CannedAggregatePayload{}, "", "", err
	}
	topSessions, err := s.db.GetTopSessionsByCost(ctx, usageFilter, 5)
	if err != nil {
		return insight.CannedAggregatePayload{}, "", "", err
	}
	usageSummary := &insight.CannedUsageSummary{
		InputTokens:         usageResult.Totals.InputTokens,
		OutputTokens:        usageResult.Totals.OutputTokens,
		CacheCreationTokens: usageResult.Totals.CacheCreationTokens,
		CacheReadTokens:     usageResult.Totals.CacheReadTokens,
		TotalCost:           usageResult.Totals.TotalCost,
		CacheSavings:        usageResult.Totals.CacheSavings,
		ModelBreakdowns:     foldCannedModelBreakdowns(usageResult.Daily),
		TopSessionsByCost:   topSessions,
	}
	coachSessions, err := s.listCannedCoachSessions(ctx, req)
	if err != nil {
		return insight.CannedAggregatePayload{}, "", "", err
	}
	coachSummary := insight.BuildCannedCoachSummary(coachSessions)

	payload := insight.CannedAggregatePayload{
		Kind:           kind,
		DateFrom:       req.DateFrom,
		DateTo:         req.DateTo,
		Project:        req.Project,
		AutomatedScope: filters.AutomatedScope,
		Filters:        filters,
		Focus:          req.Prompt,
		Signals:        signals,
		Usage:          usageSummary,
		Coach:          coachSummary,
	}
	payload.EvidenceRefs = insight.CannedEvidenceRefs(
		signals, usageSummary, coachSummary,
	)

	aggregateHash, err := insight.CannedAggregateHash(payload)
	if err != nil {
		return insight.CannedAggregatePayload{}, "", "", err
	}
	cacheKey, err := insight.CannedCacheKey(
		kind, req.DateFrom, req.DateTo, req.Project,
		req.Agent, req.Prompt, aggregateHash,
		filters.AutomatedScope,
		filters,
	)
	if err != nil {
		return insight.CannedAggregatePayload{}, "", "", err
	}
	return payload, aggregateHash, cacheKey, nil
}

func foldCannedModelBreakdowns(
	daily []db.DailyUsageEntry,
) []insight.CannedModelBreakdown {
	type modelAccum struct {
		inputTok  int
		outputTok int
		cacheCr   int
		cacheRd   int
		cost      money.Money
	}
	byModel := make(map[string]*modelAccum)
	for _, day := range daily {
		for _, model := range day.ModelBreakdowns {
			acc, ok := byModel[model.ModelName]
			if !ok {
				acc = &modelAccum{}
				byModel[model.ModelName] = acc
			}
			acc.inputTok += model.InputTokens
			acc.outputTok += model.OutputTokens
			acc.cacheCr += model.CacheCreationTokens
			acc.cacheRd += model.CacheReadTokens
			acc.cost = money.MustAdd(acc.cost, model.Cost)
		}
	}
	out := make([]insight.CannedModelBreakdown, 0, len(byModel))
	for model, acc := range byModel {
		out = append(out, insight.CannedModelBreakdown{
			ModelName:           model,
			InputTokens:         acc.inputTok,
			OutputTokens:        acc.outputTok,
			CacheCreationTokens: acc.cacheCr,
			CacheReadTokens:     acc.cacheRd,
			Cost:                acc.cost,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cost.Microdollars != out[j].Cost.Microdollars {
			return out[i].Cost.Microdollars > out[j].Cost.Microdollars
		}
		return out[i].ModelName < out[j].ModelName
	})
	return out
}

func (s *Server) listCannedCoachSessions(
	ctx context.Context,
	req generateInsightRequest,
) ([]db.Session, error) {
	filters := insight.CannedSessionFilters{
		Timezone:       "UTC",
		AutomatedScope: req.AutomatedScope,
	}
	if req.Filters != nil {
		filters = *req.Filters
	}
	loc := cannedCoachLocation(filters.Timezone)
	dateFrom, dateTo := cannedCoachUTCDateBounds(
		req.DateFrom, req.DateTo, loc,
	)
	filter := db.SessionFilter{
		DateFrom:        dateFrom,
		DateTo:          dateTo,
		Project:         req.Project,
		Machine:         filters.Machine,
		Agent:           filters.Agent,
		ActiveSince:     filters.ActiveSince,
		MinUserMessages: filters.MinUserMessages,
		ExcludeOneShot:  !filters.IncludeOneShot,
		AutomatedScope:  filters.AutomatedScope,
		Termination:     filters.Termination,
		Limit:           db.MaxSessionLimit,
	}
	var out []db.Session
	for {
		page, err := s.db.ListSessions(ctx, filter)
		if err != nil {
			return nil, err
		}
		for _, session := range page.Sessions {
			if cannedCoachSessionInDateRange(
				session, req.DateFrom, req.DateTo, loc,
			) {
				out = append(out, session)
			}
		}
		if page.NextCursor == "" {
			return out, nil
		}
		filter.Cursor = page.NextCursor
	}
}

func cannedCoachLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

func cannedCoachUTCDateBounds(
	from, to string,
	loc *time.Location,
) (string, string) {
	start, err := time.ParseInLocation("2006-01-02", from, loc)
	if err != nil {
		return from, to
	}
	end, err := time.ParseInLocation("2006-01-02", to, loc)
	if err != nil {
		return from, to
	}
	end = end.AddDate(0, 0, 1).Add(-time.Nanosecond)
	return start.UTC().Format("2006-01-02"),
		end.UTC().Format("2006-01-02")
}

func cannedCoachSessionInDateRange(
	session db.Session,
	from, to string,
	loc *time.Location,
) bool {
	date := cannedCoachSessionLocalDate(session, loc)
	if date == "" {
		return false
	}
	if from != "" && date < from {
		return false
	}
	if to != "" && date > to {
		return false
	}
	return true
}

func cannedCoachSessionLocalDate(
	session db.Session,
	loc *time.Location,
) string {
	ts := session.CreatedAt
	if session.StartedAt != nil && *session.StartedAt != "" {
		ts = *session.StartedAt
	}
	t, ok := cannedCoachLocalTime(ts, loc)
	if !ok {
		if len(ts) >= 10 {
			return ts[:10]
		}
		return ""
	}
	return t.Format("2006-01-02")
}

func cannedCoachLocalTime(
	ts string,
	loc *time.Location,
) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05Z", ts)
		if err != nil {
			return time.Time{}, false
		}
	}
	return t.In(loc), true
}
