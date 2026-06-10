package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/sessionwatch"
)

func (s *Server) registerSessionRoutes() {
	group := newRouteGroup(s.api, "/api/v1", "Sessions")

	get(s, group, "/sessions", "List sessions", s.humaListSessions)
	get(s, group, "/sessions/sidebar-index", "List sidebar sessions", s.humaSidebarSessionIndex)
	get(s, group, "/sessions/{id}", "Get session", s.humaGetSession)
	get(s, group, "/sessions/{id}/messages", "List session messages", s.humaGetMessages)
	get(s, group, "/sessions/{id}/tool-calls", "List session tool calls", s.humaToolCalls)
	get(s, group, "/sessions/{id}/children", "List child sessions", s.humaGetChildSessions)
	get(s, group, "/sessions/{id}/activity", "Get session activity", s.humaGetSessionActivity)
	get(s, group, "/sessions/{id}/timing", "Get session timing", s.humaSessionTiming)
	get(s, group, "/sessions/{id}/usage", "Get session usage", s.humaSessionUsage)
	stream(s, group, http.MethodGet, "/sessions/{id}/watch", "Watch session events", s.humaWatchSession)
	stream(s, group, http.MethodGet, "/events", "Watch server events", s.humaEvents)
	raw(s, group, http.MethodGet, "/sessions/{id}/export", "Export session as HTML", s.humaExportSession)
	raw(s, group, http.MethodGet, "/sessions/{id}/md", "Export session as Markdown", s.humaMarkdownSession)
	post(s, group, "/sessions/{id}/publish", "Publish session", s.humaPublishSession)
	post(s, group, "/sessions/{id}/resume", "Resume session", s.humaResumeSession)
	get(s, group, "/sessions/{id}/directory", "Get session directory", s.humaGetSessionDir)
	get(s, group, "/sessions/{id}/search", "Search within a session", s.humaSearchSession)
	post(s, group, "/sessions/{id}/open", "Open session directory", s.humaOpenSession)
	post(s, group, "/sessions/upload", "Upload a session export", s.humaUploadSession)
	patch(s, group, "/sessions/{id}/rename", "Rename session", s.humaRenameSession)
	deleteRoute(s, group, "/sessions/{id}", "Delete session", s.humaDeleteSession)
	post(s, group, "/sessions/{id}/restore", "Restore session", s.humaRestoreSession)
	deleteRoute(s, group, "/sessions/{id}/permanent", "Permanently delete session", s.humaPermanentDeleteSession)
	get(s, group, "/trash", "List trash", s.humaListTrash)
	deleteRoute(s, group, "/trash", "Empty trash", s.humaEmptyTrash)
}

type messageDirection string

type markdownDepth string

type sessionFilterInput struct {
	Project          string           `query:"project" doc:"Filter by project"`
	ExcludeProject   string           `query:"exclude_project" doc:"Exclude a project"`
	Machine          string           `query:"machine" doc:"Filter by machine"`
	Agent            string           `query:"agent" doc:"Filter by agent"`
	Date             string           `query:"date" format:"date" doc:"Filter to a single YYYY-MM-DD date"`
	DateFrom         string           `query:"date_from" format:"date" doc:"Filter start date"`
	DateTo           string           `query:"date_to" format:"date" doc:"Filter end date"`
	ActiveSince      string           `query:"active_since" format:"date-time" doc:"Filter sessions active since this RFC3339 timestamp"`
	MinMessages      int              `query:"min_messages" minimum:"0" doc:"Minimum total message count"`
	MaxMessages      int              `query:"max_messages" minimum:"0" doc:"Maximum total message count"`
	MinUserMessages  int              `query:"min_user_messages" minimum:"0" doc:"Minimum user message count"`
	IncludeOneShot   bool             `query:"include_one_shot" doc:"Include one-shot sessions"`
	IncludeAutomated bool             `query:"include_automated" doc:"Include automated sessions"`
	IncludeChildren  bool             `query:"include_children" doc:"Include child sessions"`
	Outcome          string           `query:"outcome" doc:"Filter by detected outcome"`
	HealthGrade      string           `query:"health_grade" doc:"Filter by health grade"`
	Cursor           string           `query:"cursor" doc:"Opaque pagination cursor"`
	Limit            int              `query:"limit" minimum:"0" doc:"Maximum number of results"`
	Termination      string           `query:"termination" doc:"Filter by termination reason"`
	MinToolFailures  optionalIntParam `query:"min_tool_failures" minimum:"0" doc:"Minimum tool failure count"`
	HasSecret        bool             `query:"has_secret" doc:"Filter sessions with secret findings"`
}

type messageListInput struct {
	ID        string           `path:"id" required:"true" doc:"Session ID"`
	Limit     int              `query:"limit" minimum:"0" doc:"Maximum number of messages"`
	Direction messageDirection `query:"direction" enum:"asc,desc" doc:"Message ordering direction"`
	From      optionalIntParam `query:"from" minimum:"0" doc:"Starting message ordinal"`
}

type searchSessionInput struct {
	ID    string `path:"id" required:"true" doc:"Session ID"`
	Query string `query:"q" doc:"Search query"`
}

func (in *sessionFilterInput) listFilter() (service.ListFilter, error) {
	if err := validateDateFilterValues(in.Date, in.DateFrom, in.DateTo, in.ActiveSince); err != nil {
		return service.ListFilter{}, err
	}
	limit := clampLimit(in.Limit, db.DefaultSessionLimit, db.MaxSessionLimit)
	filter := service.ListFilter{
		Project:          in.Project,
		ExcludeProject:   in.ExcludeProject,
		Machine:          in.Machine,
		Agent:            in.Agent,
		Date:             in.Date,
		DateFrom:         in.DateFrom,
		DateTo:           in.DateTo,
		ActiveSince:      in.ActiveSince,
		MinMessages:      in.MinMessages,
		MaxMessages:      in.MaxMessages,
		MinUserMessages:  in.MinUserMessages,
		IncludeOneShot:   in.IncludeOneShot,
		IncludeAutomated: in.IncludeAutomated,
		IncludeChildren:  in.IncludeChildren,
		Outcome:          in.Outcome,
		HealthGrade:      in.HealthGrade,
		Cursor:           in.Cursor,
		Limit:            limit,
		Termination:      in.Termination,
		HasSecret:        in.HasSecret,
	}
	if in.MinToolFailures.IsSet {
		filter.MinToolFailures = &in.MinToolFailures.Value
	}
	return filter, nil
}

func (in *sessionFilterInput) dbFilter(includeChildren bool) (db.SessionFilter, error) {
	if err := validateDateFilterValues(in.Date, in.DateFrom, in.DateTo, in.ActiveSince); err != nil {
		return db.SessionFilter{}, err
	}
	return db.SessionFilter{
		Project:          in.Project,
		ExcludeProject:   in.ExcludeProject,
		Machine:          in.Machine,
		Agent:            in.Agent,
		Date:             in.Date,
		DateFrom:         in.DateFrom,
		DateTo:           in.DateTo,
		ActiveSince:      in.ActiveSince,
		MinMessages:      in.MinMessages,
		MaxMessages:      in.MaxMessages,
		MinUserMessages:  in.MinUserMessages,
		ExcludeOneShot:   !in.IncludeOneShot,
		ExcludeAutomated: !in.IncludeAutomated,
		IncludeChildren:  includeChildren,
		Termination:      in.Termination,
	}, nil
}

func (s *Server) humaListSessions(
	ctx context.Context,
	in *sessionFilterInput,
) (*jsonOutput[*service.SessionList], error) {
	filter, err := in.listFilter()
	if err != nil {
		return nil, err
	}
	page, err := s.sessions.List(ctx, filter)
	if err != nil {
		if errors.Is(err, db.ErrInvalidCursor) {
			return nil, apiError(http.StatusBadRequest, "invalid cursor")
		}
		return nil, serverError(err)
	}
	return &jsonOutput[*service.SessionList]{Body: page}, nil
}

func (s *Server) humaSidebarSessionIndex(
	ctx context.Context,
	in *sessionFilterInput,
) (*jsonOutput[db.SidebarSessionIndex], error) {
	filter, err := in.dbFilter(true)
	if err != nil {
		return nil, err
	}
	index, err := s.db.GetSidebarSessionIndex(ctx, filter)
	if err != nil {
		return nil, serverError(err)
	}
	return &jsonOutput[db.SidebarSessionIndex]{Body: index}, nil
}

func (s *Server) humaGetSession(
	ctx context.Context,
	in *idPathInput,
) (*jsonOutput[*service.SessionDetail], error) {
	detail, err := s.sessions.Get(ctx, in.ID)
	if err != nil {
		return nil, serverError(err)
	}
	if detail == nil {
		return nil, apiError(http.StatusNotFound, "session not found")
	}
	return &jsonOutput[*service.SessionDetail]{Body: detail}, nil
}

func (s *Server) humaGetChildSessions(
	ctx context.Context,
	in *idPathInput,
) (*jsonOutput[[]db.Session], error) {
	children, err := s.db.GetChildSessions(ctx, in.ID)
	if err != nil {
		return nil, serverError(err)
	}
	if children == nil {
		children = []db.Session{}
	}
	return &jsonOutput[[]db.Session]{Body: children}, nil
}

func (s *Server) humaGetMessages(
	ctx context.Context,
	in *messageListInput,
) (*jsonOutput[*service.MessageList], error) {
	limit := clampLimit(in.Limit, db.DefaultMessageLimit, db.MaxMessageLimit)
	filter := service.MessageFilter{
		Limit:     limit,
		Direction: string(in.Direction),
	}
	if in.From.IsSet {
		filter.From = &in.From.Value
	}
	list, err := s.sessions.Messages(ctx, in.ID, filter)
	if err != nil {
		return nil, serverError(err)
	}
	return &jsonOutput[*service.MessageList]{Body: list}, nil
}

func (s *Server) humaToolCalls(
	ctx context.Context,
	in *idPathInput,
) (*jsonOutput[*service.ToolCallList], error) {
	list, err := s.sessions.ToolCalls(ctx, in.ID)
	if err != nil {
		return nil, serverError(err)
	}
	return &jsonOutput[*service.ToolCallList]{Body: list}, nil
}

func (s *Server) humaGetSessionActivity(
	ctx context.Context,
	in *idPathInput,
) (*jsonOutput[*db.SessionActivityResponse], error) {
	resp, err := s.db.GetSessionActivity(ctx, in.ID)
	if err != nil {
		return nil, serverError(err)
	}
	return &jsonOutput[*db.SessionActivityResponse]{Body: resp}, nil
}

func (s *Server) humaSessionTiming(
	ctx context.Context,
	in *idPathInput,
) (*jsonOutput[*db.SessionTiming], error) {
	timing, err := s.db.GetSessionTiming(ctx, in.ID)
	if err != nil {
		return nil, serverError(err)
	}
	if timing == nil {
		return nil, apiError(http.StatusNotFound, "session not found")
	}
	return &jsonOutput[*db.SessionTiming]{Body: timing}, nil
}

type sessionUsageResponse struct {
	SessionID         string   `json:"session_id"`
	Agent             string   `json:"agent"`
	Project           string   `json:"project"`
	TotalOutputTokens int      `json:"total_output_tokens"`
	PeakContextTokens int      `json:"peak_context_tokens"`
	HasTokenData      bool     `json:"has_token_data"`
	CostUSD           float64  `json:"cost_usd"`
	HasCost           bool     `json:"has_cost"`
	Models            []string `json:"models"`
	UnpricedModels    []string `json:"unpriced_models"`
	ServerRunning     bool     `json:"server_running"`
}

type sessionUsageErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type sessionUsageError struct {
	Status int                   `json:"-"`
	Body   sessionUsageErrorBody `json:"error"`
}

func (e *sessionUsageError) Error() string {
	return e.Body.Message
}

func (e *sessionUsageError) GetStatus() int {
	return e.Status
}

func newSessionUsageHumaResponse(usage *db.SessionUsage) sessionUsageResponse {
	unpricedModels := usage.UnpricedModels
	if unpricedModels == nil {
		unpricedModels = []string{}
	}
	return sessionUsageResponse{
		SessionID:         usage.SessionID,
		Agent:             usage.Agent,
		Project:           usage.Project,
		TotalOutputTokens: usage.TotalOutputTokens,
		PeakContextTokens: usage.PeakContextTokens,
		HasTokenData:      usage.HasTokenData,
		CostUSD:           usage.CostUSD,
		HasCost:           usage.HasCost,
		Models:            usage.Models,
		UnpricedModels:    unpricedModels,
		ServerRunning:     true,
	}
}

func (s *Server) humaSessionUsage(
	ctx context.Context,
	in *idPathInput,
) (*jsonOutput[sessionUsageResponse], error) {
	usage, err := s.db.GetSessionUsage(ctx, in.ID)
	if err != nil {
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		return nil, &sessionUsageError{
			Status: http.StatusInternalServerError,
			Body: sessionUsageErrorBody{
				Code:    "usage_query_failed",
				Message: "failed to query session usage",
			},
		}
	}
	if usage == nil {
		return nil, &sessionUsageError{
			Status: http.StatusNotFound,
			Body: sessionUsageErrorBody{
				Code:    "session_not_found",
				Message: "session not found",
			},
		}
	}
	return &jsonOutput[sessionUsageResponse]{
		Body: newSessionUsageHumaResponse(usage),
	}, nil
}

func (s *Server) humaSearchSession(
	ctx context.Context,
	in *searchSessionInput,
) (*jsonOutput[ordinalsResponse], error) {
	if in.Query == "" {
		return &jsonOutput[ordinalsResponse]{Body: ordinalsResponse{Ordinals: []int{}}}, nil
	}
	ordinals, err := s.db.SearchSession(ctx, in.ID, in.Query)
	if err != nil {
		return nil, serverError(err)
	}
	if ordinals == nil {
		ordinals = []int{}
	}
	return &jsonOutput[ordinalsResponse]{Body: ordinalsResponse{Ordinals: ordinals}}, nil
}

type ordinalsResponse struct {
	Ordinals []int `json:"ordinals"`
}

type sessionDirectoryResponse struct {
	Path string `json:"path"`
}

type openSessionInput struct {
	ID   string `path:"id" required:"true" doc:"Session ID"`
	Body openRequest
}

type openSessionResponse struct {
	Launched bool   `json:"launched"`
	Opener   string `json:"opener"`
	Path     string `json:"path"`
}

type publishResponse struct {
	GistID  string `json:"gist_id"`
	GistURL string `json:"gist_url"`
	ViewURL string `json:"view_url"`
	RawURL  string `json:"raw_url"`
}

type renameSessionInput struct {
	ID   string `path:"id" required:"true" doc:"Session ID"`
	Body renameRequest
}

type renameRequest struct {
	DisplayName *string `json:"display_name"`
}

type trashResponse struct {
	Sessions []db.Session `json:"sessions"`
}

type emptyTrashResponse struct {
	Deleted int `json:"deleted"`
}

func (s *Server) humaGetSessionDir(
	ctx context.Context,
	in *idPathInput,
) (*jsonOutput[sessionDirectoryResponse], error) {
	if s.db.ReadOnly() {
		return nil, apiError(http.StatusNotImplemented, "not available in remote mode")
	}
	session, err := s.db.GetSessionFull(ctx, in.ID)
	if err != nil {
		return nil, internalError("get session directory", err)
	}
	if session == nil || session.DeletedAt != nil {
		return nil, apiError(http.StatusNotFound, "session not found")
	}
	return &jsonOutput[sessionDirectoryResponse]{
		Body: sessionDirectoryResponse{Path: resolveSessionDir(session)},
	}, nil
}

func (s *Server) humaOpenSession(
	ctx context.Context,
	in *openSessionInput,
) (*jsonOutput[openSessionResponse], error) {
	if s.db.ReadOnly() {
		return nil, apiError(http.StatusNotImplemented, "not available in remote mode")
	}
	session, err := s.db.GetSessionFull(ctx, in.ID)
	if err != nil {
		return nil, internalError("open session lookup", err)
	}
	if session == nil || session.DeletedAt != nil {
		return nil, apiError(http.StatusNotFound, "session not found")
	}
	projectDir := resolveSessionDir(session)
	if projectDir == "" {
		return nil, apiError(http.StatusBadRequest, "session has no project directory")
	}
	openers := detectOpeners()
	var opener *Opener
	for i := range openers {
		if openers[i].ID == in.Body.OpenerID {
			opener = &openers[i]
			break
		}
	}
	if opener == nil {
		return nil, apiError(http.StatusBadRequest,
			fmt.Sprintf("opener %q not found", in.Body.OpenerID))
	}
	if err := launchOpener(*opener, projectDir); err != nil {
		return nil, apiError(http.StatusInternalServerError, "failed to launch")
	}
	return &jsonOutput[openSessionResponse]{
		Body: openSessionResponse{
			Launched: true,
			Opener:   opener.Name,
			Path:     projectDir,
		},
	}, nil
}

func (s *Server) humaPublishSession(
	ctx context.Context,
	in *idPathInput,
) (*jsonOutput[publishResponse], error) {
	token := s.githubToken()
	if token == "" {
		return nil, apiError(http.StatusUnauthorized, "GitHub token not configured")
	}
	session, msgs, err := s.sessionWithMessages(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	htmlContent := generateExportHTML(session, msgs)
	filename := session.Project + "-" + formatDateShort(session.StartedAt) + ".html"
	first := ""
	if session.FirstMessage != nil {
		first = truncateStr(*session.FirstMessage, 100)
	}
	description := fmt.Sprintf("Agent session: %s - %s", session.Project, first)
	gist, err := createGist(ctx, token, filename, description, htmlContent)
	if err != nil {
		return nil, apiError(http.StatusBadGateway, err.Error())
	}
	if gist.ID == "" || gist.HTMLURL == "" {
		return nil, apiError(http.StatusBadGateway, "GitHub API returned incomplete gist data")
	}
	encoded := urlPathEscape(filename)
	rawURL := fmt.Sprintf(
		"https://gist.githubusercontent.com/%s/%s/raw/%s",
		gist.Owner.Login, gist.ID, encoded,
	)
	return &jsonOutput[publishResponse]{
		Body: publishResponse{
			GistID:  gist.ID,
			GistURL: gist.HTMLURL,
			ViewURL: "https://htmlpreview.github.io/?" + rawURL,
			RawURL:  rawURL,
		},
	}, nil
}

func urlPathEscape(s string) string {
	return url.PathEscape(s)
}

func (s *Server) humaRenameSession(
	ctx context.Context,
	in *renameSessionInput,
) (*jsonOutput[*db.Session], error) {
	session, err := s.db.GetSession(ctx, in.ID)
	if err != nil {
		return nil, internalError("rename session lookup", err)
	}
	if session == nil {
		return nil, apiError(http.StatusNotFound, "session not found")
	}
	displayName := in.Body.DisplayName
	if displayName != nil && *displayName == "" {
		displayName = nil
	}
	if err := s.db.RenameSession(in.ID, displayName); err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("rename session", err)
	}

	updated, err := s.db.GetSession(ctx, in.ID)
	if err != nil {
		return nil, internalError("rename session readback", err)
	}
	if updated == nil {
		return nil, apiError(http.StatusNotFound, "session not found")
	}
	return &jsonOutput[*db.Session]{Body: updated}, nil
}

func (s *Server) humaDeleteSession(
	ctx context.Context,
	in *idPathInput,
) (*noContentOutput, error) {
	session, err := s.db.GetSessionFull(ctx, in.ID)
	if err != nil {
		return nil, internalError("delete session lookup", err)
	}
	if session == nil {
		return nil, apiError(http.StatusNotFound, "session not found")
	}
	if err := s.db.SoftDeleteSession(in.ID); err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("soft delete session", err)
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

func (s *Server) humaRestoreSession(
	_ context.Context,
	in *idPathInput,
) (*noContentOutput, error) {
	n, err := s.db.RestoreSession(in.ID)
	if err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("restore session", err)
	}
	if n == 0 {
		return nil, apiError(http.StatusNotFound, "session not found or not in trash")
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

func (s *Server) humaPermanentDeleteSession(
	_ context.Context,
	in *idPathInput,
) (*noContentOutput, error) {
	n, err := s.db.DeleteSessionIfTrashed(in.ID)
	if err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("permanent delete session", err)
	}
	if n == 0 {
		return nil, apiError(http.StatusConflict, "session not found or not in trash")
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

func (s *Server) humaListTrash(
	ctx context.Context,
	_ *emptyInput,
) (*jsonOutput[trashResponse], error) {
	sessions, err := s.db.ListTrashedSessions(ctx)
	if err != nil {
		return nil, internalError("list trashed sessions", err)
	}
	return &jsonOutput[trashResponse]{Body: trashResponse{Sessions: sessions}}, nil
}

func (s *Server) humaEmptyTrash(
	_ context.Context,
	_ *emptyInput,
) (*jsonOutput[emptyTrashResponse], error) {
	count, err := s.db.EmptyTrash()
	if err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("empty trash", err)
	}
	return &jsonOutput[emptyTrashResponse]{Body: emptyTrashResponse{Deleted: count}}, nil
}

type uploadSessionInput struct {
	Project string `query:"project" required:"true" doc:"Project for imported session"`
	Machine string `query:"machine" default:"remote" doc:"Machine name for imported session"`
	RawBody huma.MultipartFormFiles[uploadSessionForm]
}

type uploadSessionForm struct {
	File huma.FormFile `form:"file" contentType:"application/octet-stream" required:"true"`
}

type uploadSessionResponse struct {
	SessionID string `json:"session_id"`
	Project   string `json:"project"`
	Machine   string `json:"machine"`
	Messages  int    `json:"messages"`
	Sessions  int    `json:"sessions"`
}

type markdownInput struct {
	ID    string        `path:"id" required:"true" doc:"Session ID"`
	Depth markdownDepth `query:"depth" enum:"1,all" doc:"Child session depth"`
}

func (s *Server) sessionWithMessages(
	ctx context.Context,
	id string,
) (*db.Session, []db.Message, error) {
	session, err := s.db.GetSession(ctx, id)
	if err != nil {
		return nil, nil, apiError(http.StatusInternalServerError, err.Error())
	}
	if session == nil {
		return nil, nil, apiError(http.StatusNotFound, "session not found")
	}
	msgs, err := s.db.GetAllMessages(ctx, id)
	if err != nil {
		return nil, nil, apiError(http.StatusInternalServerError, err.Error())
	}
	return session, msgs, nil
}

func (s *Server) humaExportSession(
	ctx context.Context,
	in *idPathInput,
) (*bytesOutput, error) {
	session, msgs, err := s.sessionWithMessages(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	htmlContent := generateExportHTML(session, msgs)
	filename := sanitizeFilename(session.Project + "-" + formatDateShort(session.StartedAt) + ".html")
	return &bytesOutput{
		ContentType:        "text/html; charset=utf-8",
		ContentDisposition: fmt.Sprintf(`attachment; filename="%s"`, filename),
		Body:               []byte(htmlContent),
	}, nil
}

func (s *Server) humaMarkdownSession(
	ctx context.Context,
	in *markdownInput,
) (*bytesOutput, error) {
	depth := string(in.Depth)
	tree, err := s.loadExportSessionTree(ctx, in.ID, depth, map[string]bool{}, 0)
	if err != nil {
		return nil, serverError(err)
	}
	if tree == nil || tree.Session == nil {
		return nil, apiError(http.StatusNotFound, "session not found")
	}
	md := generateExportMarkdownTree(tree, exportMarkdownOptions{Depth: depth})
	filename := sanitizeFilename(tree.Session.Project + "-" + formatDateShort(tree.Session.StartedAt) + ".md")
	return &bytesOutput{
		ContentType:        "text/markdown; charset=utf-8",
		ContentDisposition: fmt.Sprintf(`inline; filename="%s"`, filename),
		Body:               []byte(md),
	}, nil
}

func (s *Server) humaWatchSession(
	ctx context.Context,
	in *idPathInput,
) (*huma.StreamResponse, error) {
	sess, err := s.sessions.Get(ctx, in.ID)
	if err != nil {
		return nil, serverError(err)
	}
	if sess == nil {
		return nil, apiError(http.StatusNotFound, "session not found: "+in.ID)
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		stream, ok := newHumaSSEStream(hctx)
		if !ok {
			writeHumaJSON(hctx, http.StatusInternalServerError,
				apiErrorResponse{Message: "streaming not supported"})
			return
		}
		streamCtx := hctx.Context()
		updates := s.sessionMonitor(streamCtx, in.ID)
		heartbeat := time.NewTicker(
			sessionwatch.PollInterval * sessionwatch.HeartbeatTicks,
		)
		defer heartbeat.Stop()
		if t, err := s.db.GetSessionTiming(streamCtx, in.ID); err != nil {
			log.Printf("session timing initial: %v", err)
		} else if t != nil {
			stream.SendJSON("session.timing", t)
		}
		for {
			select {
			case <-streamCtx.Done():
				return
			case _, ok := <-updates:
				if !ok {
					return
				}
				stream.Send("session_updated", in.ID)
				if t, err := s.db.GetSessionTiming(streamCtx, in.ID); err != nil {
					log.Printf("session timing update: %v", err)
				} else if t != nil {
					stream.SendJSON("session.timing", t)
				}
			case <-heartbeat.C:
				stream.Send("heartbeat", time.Now().UTC().Format(time.RFC3339))
			}
		}
	}}, nil
}

func (s *Server) humaEvents(
	_ context.Context,
	_ *emptyInput,
) (*huma.StreamResponse, error) {
	if s.engine == nil || s.broadcaster == nil {
		return nil, huma.ErrorWithHeaders(
			apiError(http.StatusServiceUnavailable, "events not available in this mode"),
			http.Header{"Retry-After": []string{"300"}},
		)
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		stream, ok := newHumaSSEStream(hctx)
		if !ok {
			writeHumaJSON(hctx, http.StatusInternalServerError,
				apiErrorResponse{Message: "streaming not supported"})
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
			case <-hctx.Context().Done():
				return
			case ev, ok := <-sub:
				if !ok {
					return
				}
				stream.SendJSON("data_changed", map[string]string{"scope": ev.Scope})
			case <-heartbeat.C:
				stream.Send("heartbeat", time.Now().Format(time.RFC3339))
			}
		}
	}}, nil
}

func (s *Server) humaUploadSession(
	ctx context.Context,
	in *uploadSessionInput,
) (*jsonOutput[uploadSessionResponse], error) {
	if s.db.ReadOnly() {
		return nil, apiError(http.StatusNotImplemented,
			"uploads are not available in read-only mode")
	}
	project := strings.TrimSpace(in.Project)
	if project == "" {
		return nil, apiError(http.StatusBadRequest, "project required")
	}
	if !isSafeName(project) {
		return nil, apiError(http.StatusBadRequest, "invalid project name")
	}
	machine := in.Machine
	if machine == "" {
		machine = "remote"
	}
	file := in.RawBody.Data().File
	if !file.IsSet {
		return nil, apiError(http.StatusBadRequest, "file field required")
	}
	defer file.Close()
	if !strings.HasSuffix(file.Filename, ".jsonl") {
		return nil, apiError(http.StatusBadRequest, "file must be .jsonl")
	}
	safeName := filepath.Base(file.Filename)
	if safeName != file.Filename ||
		!isSafeName(strings.TrimSuffix(safeName, ".jsonl")) {
		return nil, apiError(http.StatusBadRequest, "invalid filename")
	}
	upload, err := s.stageUpload(project, safeName, file)
	if err != nil {
		log.Printf("Error saving upload: %v", err)
		return nil, apiError(http.StatusInternalServerError, "failed to save upload")
	}
	defer func() { _ = os.RemoveAll(upload.tempDir) }()
	results, err := parser.ParseClaudeSession(upload.tempPath, project, machine)
	if err != nil {
		return nil, apiError(http.StatusBadRequest,
			fmt.Sprintf("parsing session: %v", err))
	}
	if len(results) == 0 {
		return nil, apiError(http.StatusBadRequest, "no sessions parsed from upload")
	}
	parser.InferRelationshipTypes(results)
	for i := range results {
		results[i].Session.File.Path = upload.finalPath
	}
	writes := make([]db.SessionBatchWrite, len(results))
	for i, pr := range results {
		writes[i] = sessionBatchWriteFromParsed(pr.Session, pr.Messages)
	}
	var commitErr error
	var uploadCommit committedUpload
	_, err = s.db.WriteSessionBatchAtomic(writes, func() error {
		uploadCommit, commitErr = commitUpload(upload)
		return commitErr
	})
	if err != nil {
		if commitErr != nil {
			log.Printf("Error committing upload: %v", commitErr)
			return nil, apiError(http.StatusInternalServerError, "failed to save upload")
		}
		if uploadCommit.movedFinal {
			if rbErr := rollbackCommittedUpload(uploadCommit); rbErr != nil {
				log.Printf("Error rolling back upload after DB failure: %v", rbErr)
				return nil, apiError(http.StatusInternalServerError, "failed to save upload")
			}
			cleanupCommittedUpload(uploadCommit)
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		if errors.Is(err, db.ErrSessionExcluded) ||
			errors.Is(err, db.ErrSessionTrashed) {
			return nil, apiError(http.StatusConflict,
				"session upload rejected: session is excluded or trashed")
		}
		log.Printf("Error saving session to DB: %v", err)
		return nil, apiError(http.StatusInternalServerError,
			"failed to save session to database")
	}
	cleanupCommittedUpload(uploadCommit)
	main := results[0]
	return &jsonOutput[uploadSessionResponse]{
		Body: uploadSessionResponse{
			SessionID: main.Session.ID,
			Project:   project,
			Machine:   machine,
			Messages:  len(main.Messages),
			Sessions:  len(results),
		},
	}, nil
}

type resumeSessionInput struct {
	ID   string `path:"id" required:"true" doc:"Session ID"`
	Body resumeRequest
}

func (s *Server) humaResumeSession(
	ctx context.Context,
	in *resumeSessionInput,
) (*jsonOutput[resumeResponse], error) {
	session, err := s.db.GetSessionFull(ctx, in.ID)
	if err != nil {
		return nil, internalError("resume: session lookup failed", err)
	}
	if session == nil || session.DeletedAt != nil {
		return nil, apiError(http.StatusNotFound, "session not found")
	}
	if host, _ := parser.StripHostPrefix(in.ID); host != "" {
		return nil, apiError(http.StatusBadRequest, "cannot resume remote session")
	}
	tmpl, ok := resumeAgents[string(session.Agent)]
	if !ok {
		return nil, apiError(http.StatusBadRequest,
			fmt.Sprintf("agent %q does not support resume", session.Agent))
	}
	req := in.Body
	prefix := string(session.Agent) + ":"
	rawID := strings.TrimPrefix(in.ID, prefix)
	var cmd string
	if strings.Contains(tmpl, "%s") {
		cmd = fmt.Sprintf(tmpl, shellQuote(rawID))
	} else {
		cmd = tmpl
	}
	if string(session.Agent) == "claude" {
		if req.SkipPermissions {
			cmd += " --dangerously-skip-permissions"
		}
		if req.ForkSession {
			cmd += " --fork-session"
		}
	}
	launchDir, workspaceDir := resolveResumePaths(session)
	if string(session.Agent) == "cursor" && workspaceDir != "" {
		cmd += " --workspace " + shellQuote(workspaceDir)
	}
	responseCmd := cmd
	switch string(session.Agent) {
	case "claude", "kiro":
		responseCmd = commandWithCwd(cmd, launchDir)
	}
	if req.CommandOnly {
		return &jsonOutput[resumeResponse]{
			Body: resumeResponse{
				Launched: false,
				Command:  responseCmd,
				Cwd:      launchDir,
			},
		}, nil
	}
	if s.db.ReadOnly() {
		return nil, apiError(http.StatusNotImplemented,
			"session launch not available in remote mode")
	}
	if req.OpenerID != "" {
		return s.humaResumeWithOpener(session, rawID, cmd, responseCmd, launchDir, req.OpenerID)
	}
	s.mu.RLock()
	termCfg := s.cfg.Terminal
	s.mu.RUnlock()
	if termCfg.Mode == string(terminalModeClipboard) {
		return &jsonOutput[resumeResponse]{
			Body: resumeResponse{
				Launched: false,
				Command:  responseCmd,
				Cwd:      launchDir,
			},
		}, nil
	}
	detectCwd := launchDir
	if termCfg.Mode == string(terminalModeAuto) {
		detectCwd = resumeLaunchCwd(
			string(session.Agent), "auto", runtime.GOOS, launchDir,
		)
	}
	termBin, termArgs, termName, termErr := detectTerminal(cmd, detectCwd, termCfg)
	if termErr != nil {
		log.Printf("resume: terminal detection failed: %v", termErr)
		return &jsonOutput[resumeResponse]{
			Body: resumeResponse{
				Launched: false,
				Command:  responseCmd,
				Cwd:      launchDir,
				Error:    "no_terminal_found",
			},
		}, nil
	}
	proc := exec.Command(termBin, termArgs...)
	proc.Stdout = nil
	proc.Stderr = nil
	proc.Stdin = nil
	if detectCwd != "" {
		proc.Dir = detectCwd
	}
	if err := proc.Start(); err != nil {
		log.Printf("resume: terminal start failed: %v", err)
		return &jsonOutput[resumeResponse]{
			Body: resumeResponse{
				Launched: false,
				Command:  responseCmd,
				Cwd:      launchDir,
				Error:    "terminal_launch_failed",
			},
		}, nil
	}
	go func() { _ = proc.Wait() }()
	return &jsonOutput[resumeResponse]{
		Body: resumeResponse{
			Launched: true,
			Terminal: termName,
			Command:  responseCmd,
			Cwd:      launchDir,
		},
	}, nil
}

func (s *Server) humaResumeWithOpener(
	session *db.Session,
	rawID string,
	cmd string,
	responseCmd string,
	launchDir string,
	openerID string,
) (*jsonOutput[resumeResponse], error) {
	openers := detectOpeners()
	var opener *Opener
	for i := range openers {
		if openers[i].ID == openerID {
			opener = &openers[i]
			break
		}
	}
	if opener == nil {
		return nil, apiError(http.StatusBadRequest,
			fmt.Sprintf("opener %q not found", openerID))
	}
	if opener.ID == "claude-desktop" {
		if string(session.Agent) != "claude" {
			return nil, apiError(http.StatusBadRequest,
				"Claude Desktop resume only supports Claude sessions")
		}
		proc := launchClaudeDesktop(rawID, launchDir)
		if err := proc.Start(); err != nil {
			log.Printf("resume: Claude Desktop launch failed: %v", err)
			return &jsonOutput[resumeResponse]{
				Body: resumeResponse{
					Launched: false,
					Command:  responseCmd,
					Cwd:      launchDir,
					Error:    "desktop_launch_failed",
				},
			}, nil
		}
		go func() { _ = proc.Wait() }()
		return &jsonOutput[resumeResponse]{
			Body: resumeResponse{
				Launched: true,
				Terminal: opener.Name,
				Command:  responseCmd,
				Cwd:      launchDir,
			},
		}, nil
	}
	openerCwd := resumeLaunchCwd(
		string(session.Agent), opener.ID, runtime.GOOS, launchDir,
	)
	proc := launchResumeInOpener(*opener, cmd, openerCwd)
	if proc == nil {
		return &jsonOutput[resumeResponse]{
			Body: resumeResponse{
				Launched: false,
				Command:  responseCmd,
				Cwd:      launchDir,
				Error:    "unsupported_opener",
			},
		}, nil
	}
	if err := proc.Start(); err != nil {
		log.Printf("resume: opener start failed: %v", err)
		return &jsonOutput[resumeResponse]{
			Body: resumeResponse{
				Launched: false,
				Command:  responseCmd,
				Cwd:      launchDir,
				Error:    "terminal_launch_failed",
			},
		}, nil
	}
	go func() { _ = proc.Wait() }()
	return &jsonOutput[resumeResponse]{
		Body: resumeResponse{
			Launched: true,
			Terminal: opener.Name,
			Command:  responseCmd,
			Cwd:      launchDir,
		},
	}, nil
}
