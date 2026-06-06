package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	stdsync "sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/shlex"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/importer"
	"go.kenn.io/agentsview/internal/insight"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/sessionwatch"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/timeutil"
	"go.kenn.io/agentsview/internal/update"
	"go.kenn.io/kit/daemon"
)

type emptyInput struct{}

type jsonOutput[T any] struct {
	Body T
}

type createdOutput[T any] struct {
	Status int `status:"201"`
	Body   T
}

type noContentOutput struct {
	Status int `status:"204"`
}

type bytesOutput struct {
	ContentType        string `header:"Content-Type"`
	ContentDisposition string `header:"Content-Disposition"`
	NoSniff            string `header:"X-Content-Type-Options"`
	CacheControl       string `header:"Cache-Control"`
	Body               []byte
}

type apiErrorResponse struct {
	Status  int    `json:"-"`
	Message string `json:"error"`
}

func (e *apiErrorResponse) Error() string {
	return e.Message
}

func (e *apiErrorResponse) GetStatus() int {
	return e.Status
}

func apiError(status int, message string) error {
	return &apiErrorResponse{Status: status, Message: message}
}

var configureHumaErrorsOnce stdsync.Once

func configureHumaErrors() {
	configureHumaErrorsOnce.Do(func() {
		huma.NewError = func(status int, message string, errs ...error) huma.StatusError {
			if status == http.StatusUnprocessableEntity {
				status = http.StatusBadRequest
			}
			if len(errs) > 0 {
				var details []string
				for _, err := range errs {
					if err == nil {
						continue
					}
					details = append(details, err.Error())
				}
				if len(details) > 0 {
					message = strings.Join(details, "; ")
				}
			}
			if strings.Contains(message, "(query.type:") {
				message = "invalid type: " + message
			}
			return &apiErrorResponse{
				Status:  status,
				Message: message,
			}
		}
		huma.NewErrorWithContext = func(
			_ huma.Context,
			status int,
			message string,
			errs ...error,
		) huma.StatusError {
			return huma.NewError(status, message, errs...)
		}
	})
}

type requestInfo struct {
	RemoteAddr string
	Forwarded  bool
}

type optionalIntParam struct {
	Value int
	IsSet bool
}

func (p optionalIntParam) Schema(r huma.Registry) *huma.Schema {
	return huma.SchemaFromType(r, reflect.TypeOf(p.Value))
}

func (p *optionalIntParam) Receiver() reflect.Value {
	return reflect.ValueOf(p).Elem().FieldByName("Value")
}

func (p *optionalIntParam) OnParamSet(isSet bool, _ any) {
	p.IsSet = isSet
}

func optionalIntValue(p optionalIntParam) *int {
	if !p.IsSet {
		return nil
	}
	return &p.Value
}

const ctxKeyHumaRequestInfo contextKey = 100

func humaRequestInfoMiddleware(ctx huma.Context, next func(huma.Context)) {
	info := requestInfo{
		RemoteAddr: ctx.RemoteAddr(),
		Forwarded: ctx.Header("X-Forwarded-For") != "" ||
			ctx.Header("X-Real-IP") != "" ||
			ctx.Header("Forwarded") != "",
	}
	next(huma.WithValue(ctx, ctxKeyHumaRequestInfo, info))
}

func isLocalhostContext(ctx context.Context) bool {
	info, _ := ctx.Value(ctxKeyHumaRequestInfo).(requestInfo)
	if info.Forwarded {
		return false
	}
	host, _, err := net.SplitHostPort(info.RemoteAddr)
	if err != nil {
		host = info.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func agentsViewSchemaNamer(t reflect.Type, hint string) string {
	name := huma.DefaultSchemaNamer(t, hint)
	base := schemaNamedType(t)
	pkgPath := base.PkgPath()
	const internalPrefix = "go.kenn.io/agentsview/internal/"
	if pkgPath == "" ||
		!strings.HasPrefix(pkgPath, internalPrefix) ||
		strings.HasSuffix(pkgPath, "/server") {
		return name
	}
	pkg := strings.TrimPrefix(pkgPath, internalPrefix)
	pkg = strings.NewReplacer("/", "", "-", "", "_", "").Replace(pkg)
	if pkg == "" {
		return name
	}
	return pascalASCII(pkg) + name
}

func schemaNamedType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer ||
		t.Kind() == reflect.Slice ||
		t.Kind() == reflect.Array {
		t = t.Elem()
	}
	return t
}

func pascalASCII(s string) string {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return s
	}
	return string(s[0]-('a'-'A')) + s[1:]
}

func get[I, O any](
	s *Server, group routeGroup, path, summary string,
	handler func(context.Context, *I) (*O, error),
) {
	registerRoute(group, http.MethodGet, path, summary, handler, s.humaTimeout())
}

func post[I, O any](
	s *Server, group routeGroup, path, summary string,
	handler func(context.Context, *I) (*O, error),
) {
	registerRoute(group, http.MethodPost, path, summary, handler, s.humaTimeout())
}

func put[I, O any](
	s *Server, group routeGroup, path, summary string,
	handler func(context.Context, *I) (*O, error),
) {
	registerRoute(group, http.MethodPut, path, summary, handler, s.humaTimeout())
}

func patch[I, O any](
	s *Server, group routeGroup, path, summary string,
	handler func(context.Context, *I) (*O, error),
) {
	registerRoute(group, http.MethodPatch, path, summary, handler, s.humaTimeout())
}

func deleteRoute[I, O any](
	s *Server, group routeGroup, path, summary string,
	handler func(context.Context, *I) (*O, error),
) {
	registerRoute(group, http.MethodDelete, path, summary, handler, s.humaTimeout())
}

func stream[I any](
	_ *Server, group routeGroup, method, path, summary string,
	handler func(context.Context, *I) (*huma.StreamResponse, error),
) {
	registerRoute(group, method, path, summary, handler, streamResponse())
}

func raw[I any](
	_ *Server, group routeGroup, method, path, summary string,
	handler func(context.Context, *I) (*bytesOutput, error),
) {
	registerRoute(group, method, path, summary, handler)
}

func operationID(method, path string) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(method))
	lastDash := false
	for _, r := range path {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
			lastDash = false
		default:
			if b.Len() > 0 && !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func registerRoute[I, O any](
	group routeGroup, method, path, summary string,
	handler func(context.Context, *I) (*O, error),
	options ...func(*huma.Operation),
) {
	op := huma.Operation{
		OperationID: operationID(method, group.fullPath(path)),
		Method:      method,
		Path:        path,
		Summary:     summary,
		Errors: []int{
			http.StatusBadRequest,
			http.StatusUnauthorized,
			http.StatusForbidden,
			http.StatusNotFound,
			http.StatusConflict,
			http.StatusInternalServerError,
			http.StatusNotImplemented,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout,
		},
	}
	for _, option := range options {
		option(&op)
	}
	huma.Register(group.api, op, handler)
}

func streamResponse() func(*huma.Operation) {
	return func(op *huma.Operation) {
		op.Responses = map[string]*huma.Response{
			"200": {
				Description: "OK",
				Content: map[string]*huma.MediaType{
					"text/event-stream": {Schema: &huma.Schema{Type: huma.TypeString}},
				},
			},
		}
	}
}

func (s *Server) humaTimeout() func(*huma.Operation) {
	return func(op *huma.Operation) {
		op.Middlewares = append(op.Middlewares, func(ctx huma.Context, next func(huma.Context)) {
			if s.handlerDelay > 0 {
				timer := time.NewTimer(s.cfg.WriteTimeout)
				defer timer.Stop()
				select {
				case <-time.After(s.handlerDelay):
				case <-timer.C:
					ctx.SetHeader("Content-Type", "application/json")
					ctx.SetStatus(http.StatusServiceUnavailable)
					_, _ = io.WriteString(ctx.BodyWriter(), `{"error":"request timed out"}`)
					return
				case <-ctx.Context().Done():
					next(ctx)
					return
				}
			}
			if s.cfg.WriteTimeout <= 0 {
				next(ctx)
				return
			}
			reqCtx, cancel := context.WithTimeout(ctx.Context(), s.cfg.WriteTimeout)
			defer cancel()
			next(huma.WithContext(ctx, reqCtx))
		})
	}
}

func handleHumaContextError(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return apiError(http.StatusGatewayTimeout, "gateway timeout")
	}
	return nil
}

func handleHumaReadOnly(err error) error {
	if errors.Is(err, db.ErrReadOnly) {
		return apiError(http.StatusNotImplemented, "not available in remote mode")
	}
	return nil
}

func serverError(err error) error {
	if handled := handleHumaContextError(err); handled != nil {
		return handled
	}
	return apiError(http.StatusInternalServerError, err.Error())
}

func internalError(logPrefix string, err error) error {
	if handled := handleHumaContextError(err); handled != nil {
		return handled
	}
	if err != nil {
		log.Printf("%s: %v", logPrefix, err)
	}
	return apiError(http.StatusInternalServerError, "internal error")
}

type idPathInput struct {
	ID string `path:"id" required:"true" doc:"Session ID"`
}

type intIDPathInput struct {
	ID int64 `path:"id" required:"true" doc:"Numeric ID"`
}

type messagePathInput struct {
	ID        string `path:"id" required:"true" doc:"Session ID"`
	MessageID int64  `path:"messageId" required:"true" doc:"Message ordinal"`
}

type paginationInput struct {
	Limit  int `query:"limit" minimum:"0" doc:"Maximum number of results"`
	Cursor int `query:"cursor" minimum:"0" doc:"Pagination cursor"`
}

type BoolIncludeInput struct {
	IncludeOneShot   bool `query:"include_one_shot" doc:"Include one-shot sessions"`
	IncludeAutomated bool `query:"include_automated" doc:"Include automated sessions"`
}

type terminalMode string

const (
	terminalModeAuto      terminalMode = "auto"
	terminalModeCustom    terminalMode = "custom"
	terminalModeClipboard terminalMode = "clipboard"
)

type messageDirection string

const (
	messageDirectionAsc  messageDirection = "asc"
	messageDirectionDesc messageDirection = "desc"
)

type searchSort string

const (
	searchSortRelevance searchSort = "relevance"
	searchSortRecency   searchSort = "recency"
)

type contentSearchMode string

const (
	contentSearchModeSubstring contentSearchMode = "substring"
	contentSearchModeRegex     contentSearchMode = "regex"
	contentSearchModeFTS       contentSearchMode = "fts"
)

type analyticsGranularity string

const (
	analyticsGranularityDay   analyticsGranularity = "day"
	analyticsGranularityWeek  analyticsGranularity = "week"
	analyticsGranularityMonth analyticsGranularity = "month"
)

type heatmapMetric string

const (
	heatmapMetricMessages     heatmapMetric = "messages"
	heatmapMetricSessions     heatmapMetric = "sessions"
	heatmapMetricOutputTokens heatmapMetric = "output_tokens"
)

type topSessionMetric string

const (
	topSessionMetricMessages     topSessionMetric = "messages"
	topSessionMetricDuration     topSessionMetric = "duration"
	topSessionMetricOutputTokens topSessionMetric = "output_tokens"
)

type markdownDepth string

const (
	markdownDepthOne markdownDepth = "1"
	markdownDepthAll markdownDepth = "all"
)

type insightType string

const (
	insightTypeDailyActivity insightType = "daily_activity"
	insightTypeAgentAnalysis insightType = "agent_analysis"
)

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

type searchInput struct {
	Query   string     `query:"q" required:"true" doc:"Search query"`
	Project string     `query:"project" doc:"Filter by project"`
	Sort    searchSort `query:"sort" enum:"relevance,recency" default:"relevance" doc:"Sort order"`
	Limit   int        `query:"limit" minimum:"0" doc:"Maximum number of results"`
	Cursor  int        `query:"cursor" minimum:"0" doc:"Pagination cursor"`
}

type contentSearchInput struct {
	Pattern          string            `query:"pattern" required:"true" doc:"Pattern to search for"`
	Mode             contentSearchMode `query:"mode" enum:"substring,regex,fts" doc:"Search mode"`
	In               string            `query:"in" doc:"Comma-separated content sources"`
	ExcludeSystem    bool              `query:"exclude_system" doc:"Exclude system messages"`
	Reveal           bool              `query:"reveal" doc:"Return unredacted secret matches for localhost callers"`
	Project          string            `query:"project" doc:"Filter by project"`
	ExcludeProject   string            `query:"exclude_project" doc:"Exclude a project"`
	Machine          string            `query:"machine" doc:"Filter by machine"`
	Agent            string            `query:"agent" doc:"Filter by agent"`
	Date             string            `query:"date" format:"date" doc:"Filter to a single YYYY-MM-DD date"`
	DateFrom         string            `query:"date_from" format:"date" doc:"Filter start date"`
	DateTo           string            `query:"date_to" format:"date" doc:"Filter end date"`
	ActiveSince      string            `query:"active_since" format:"date-time" doc:"Filter sessions active since this RFC3339 timestamp"`
	IncludeChildren  bool              `query:"include_children" doc:"Include child sessions"`
	IncludeAutomated bool              `query:"include_automated" doc:"Include automated sessions"`
	IncludeOneShot   bool              `query:"include_one_shot" doc:"Include one-shot sessions"`
	Limit            int               `query:"limit" minimum:"0" doc:"Maximum number of results"`
	Cursor           int               `query:"cursor" minimum:"0" doc:"Pagination cursor"`
}

type secretListInput struct {
	Project    string `query:"project" doc:"Filter by project"`
	Agent      string `query:"agent" doc:"Filter by agent"`
	DateFrom   string `query:"date_from" format:"date" doc:"Filter start date"`
	DateTo     string `query:"date_to" format:"date" doc:"Filter end date"`
	Rule       string `query:"rule" doc:"Filter by secret rule"`
	Confidence string `query:"confidence" doc:"Filter by confidence"`
	Reveal     bool   `query:"reveal" doc:"Return unredacted matches for localhost callers"`
	Limit      int    `query:"limit" minimum:"0" doc:"Maximum number of results"`
	Cursor     int    `query:"cursor" minimum:"0" doc:"Pagination cursor"`
}

type AnalyticsFilterInput struct {
	From             string           `query:"from" format:"date" doc:"Range start date"`
	To               string           `query:"to" format:"date" doc:"Range end date"`
	Timezone         string           `query:"timezone" doc:"IANA timezone name"`
	Machine          string           `query:"machine" doc:"Filter by machine"`
	Project          string           `query:"project" doc:"Filter by project"`
	Agent            string           `query:"agent" doc:"Filter by agent"`
	DayOfWeek        optionalIntParam `query:"dow" minimum:"0" maximum:"6" doc:"Day of week, Monday=0 through Sunday=6"`
	Hour             optionalIntParam `query:"hour" minimum:"0" maximum:"23" doc:"Hour of day, 0 through 23"`
	MinUserMessages  int              `query:"min_user_messages" minimum:"0" doc:"Minimum user message count"`
	ActiveSince      string           `query:"active_since" format:"date-time" doc:"Filter sessions active since this RFC3339 timestamp"`
	IncludeOneShot   bool             `query:"include_one_shot" doc:"Include one-shot sessions"`
	IncludeAutomated bool             `query:"include_automated" doc:"Include automated sessions"`
	Termination      string           `query:"termination" doc:"Filter by termination reason"`
}

type analyticsActivityInput struct {
	AnalyticsFilterInput
	Granularity analyticsGranularity `query:"granularity" enum:"day,week,month" default:"day" doc:"Time bucket granularity"`
}

type analyticsHeatmapInput struct {
	AnalyticsFilterInput
	Metric heatmapMetric `query:"metric" enum:"messages,sessions,output_tokens" default:"messages" doc:"Heatmap metric"`
}

type analyticsTopSessionsInput struct {
	AnalyticsFilterInput
	Metric topSessionMetric `query:"metric" enum:"messages,duration,output_tokens" default:"messages" doc:"Ranking metric"`
}

type trendsTermsInput struct {
	AnalyticsFilterInput
	Term        []string             `query:"term,explode" doc:"Terms to trend"`
	Granularity analyticsGranularity `query:"granularity" enum:"day,week,month" default:"week" doc:"Time bucket granularity"`
}

type UsageFilterInput struct {
	From             string `query:"from" format:"date" doc:"Range start date"`
	To               string `query:"to" format:"date" doc:"Range end date"`
	Timezone         string `query:"timezone" doc:"IANA timezone name"`
	Agent            string `query:"agent" doc:"Filter by agent"`
	Project          string `query:"project" doc:"Filter by project"`
	Machine          string `query:"machine" doc:"Filter by machine"`
	ExcludeProject   string `query:"exclude_project" doc:"Exclude a project"`
	ExcludeAgent     string `query:"exclude_agent" doc:"Exclude an agent"`
	ExcludeModel     string `query:"exclude_model" doc:"Exclude a model"`
	Model            string `query:"model" doc:"Filter by model"`
	MinUserMessages  int    `query:"min_user_messages" minimum:"0" doc:"Minimum user message count"`
	ActiveSince      string `query:"active_since" format:"date-time" doc:"Filter sessions active since this RFC3339 timestamp"`
	IncludeOneShot   bool   `query:"include_one_shot" default:"true" doc:"Include one-shot sessions"`
	IncludeAutomated bool   `query:"include_automated" doc:"Include automated sessions"`
}

type usageTopSessionsInput struct {
	UsageFilterInput
	Limit int `query:"limit" minimum:"0" maximum:"100" default:"20" doc:"Maximum number of sessions"`
}

func (s *Server) humaPing(
	_ context.Context,
	_ *emptyInput,
) (*jsonOutput[daemon.PingInfo], error) {
	return &jsonOutput[daemon.PingInfo]{
		Body: daemon.PingInfo{
			OK:      true,
			Service: daemonService,
			Version: s.version.Version,
			PID:     os.Getpid(),
		},
	}, nil
}

func validateDateFilterValues(date, dateFrom, dateTo, activeSince string) error {
	for _, d := range []string{date, dateFrom, dateTo} {
		if d != "" && !timeutil.IsValidDate(d) {
			return apiError(http.StatusBadRequest, "invalid date format: use YYYY-MM-DD")
		}
	}
	if dateFrom != "" && dateTo != "" && dateFrom > dateTo {
		return apiError(http.StatusBadRequest, "date_from must not be after date_to")
	}
	if activeSince != "" && !timeutil.IsValidTimestamp(activeSince) {
		return apiError(http.StatusBadRequest, "invalid active_since: use RFC3339 timestamp")
	}
	return nil
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

func (s *Server) humaSearch(
	ctx context.Context,
	in *searchInput,
) (*jsonOutput[searchResponse], error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return nil, apiError(http.StatusBadRequest, "query required")
	}
	if !s.db.HasFTS() {
		return nil, apiError(http.StatusNotImplemented, "search not available")
	}
	limit := clampLimit(in.Limit, db.DefaultSearchLimit, db.MaxSearchLimit)
	page, err := s.db.Search(ctx, db.SearchFilter{
		Query:   prepareFTSQuery(query),
		Project: in.Project,
		Sort:    string(in.Sort),
		Cursor:  in.Cursor,
		Limit:   limit,
	})
	if err != nil {
		return nil, serverError(err)
	}
	results := page.Results
	if results == nil {
		results = []db.SearchResult{}
	}
	return &jsonOutput[searchResponse]{
		Body: searchResponse{
			Query:   query,
			Results: results,
			Count:   len(results),
			Next:    page.NextCursor,
		},
	}, nil
}

func (s *Server) humaSearchContent(
	ctx context.Context,
	in *contentSearchInput,
) (*jsonOutput[*service.ContentSearchResult], error) {
	if in.Reveal && !isLocalhostContext(ctx) {
		return nil, apiError(http.StatusForbidden, "reveal is only permitted from localhost")
	}
	var sources []string
	if in.In != "" {
		sources = strings.Split(in.In, ",")
	}
	if err := validateDateFilterValues(in.Date, in.DateFrom, in.DateTo, in.ActiveSince); err != nil {
		return nil, err
	}
	res, err := s.sessions.SearchContent(ctx, service.ContentSearchRequest{
		Pattern:          in.Pattern,
		Mode:             string(in.Mode),
		Sources:          sources,
		ExcludeSystem:    in.ExcludeSystem,
		Reveal:           in.Reveal,
		Project:          in.Project,
		ExcludeProject:   in.ExcludeProject,
		Machine:          in.Machine,
		Agent:            in.Agent,
		Date:             in.Date,
		DateFrom:         in.DateFrom,
		DateTo:           in.DateTo,
		ActiveSince:      in.ActiveSince,
		IncludeChildren:  in.IncludeChildren,
		IncludeAutomated: in.IncludeAutomated,
		IncludeOneShot:   in.IncludeOneShot,
		Limit:            in.Limit,
		Cursor:           in.Cursor,
	})
	if err != nil {
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		var inputErr *db.SearchInputError
		if errors.As(err, &inputErr) {
			return nil, apiError(http.StatusBadRequest, err.Error())
		}
		return nil, apiError(http.StatusInternalServerError, err.Error())
	}
	if res.Matches == nil {
		res.Matches = []db.ContentMatch{}
	}
	return &jsonOutput[*service.ContentSearchResult]{Body: res}, nil
}

func (s *Server) humaListSecrets(
	ctx context.Context,
	in *secretListInput,
) (*jsonOutput[*service.SecretFindingList], error) {
	if in.Reveal && !isLocalhostContext(ctx) {
		return nil, apiError(http.StatusForbidden, "reveal is only permitted from localhost")
	}
	if err := validateDateFilterValues("", in.DateFrom, in.DateTo, ""); err != nil {
		return nil, err
	}
	res, err := s.sessions.ListSecrets(ctx, service.SecretListFilter{
		Project:    in.Project,
		Agent:      in.Agent,
		DateFrom:   in.DateFrom,
		DateTo:     in.DateTo,
		Rule:       in.Rule,
		Confidence: in.Confidence,
		Reveal:     in.Reveal,
		Limit:      in.Limit,
		Cursor:     in.Cursor,
	})
	if err != nil {
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, apiError(http.StatusInternalServerError, err.Error())
	}
	if res.Findings == nil {
		res.Findings = []db.SecretFindingRow{}
	}
	return &jsonOutput[*service.SecretFindingList]{Body: res}, nil
}

func analyticsFilterFromInput(in AnalyticsFilterInput) (db.AnalyticsFilter, error) {
	tz := in.Timezone
	if tz == "" {
		tz = "UTC"
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return db.AnalyticsFilter{}, apiError(http.StatusBadRequest, "invalid timezone: "+tz)
	}
	from, to := defaultDateRange(in.From, in.To)
	if !timeutil.IsValidDate(from) || !timeutil.IsValidDate(to) {
		return db.AnalyticsFilter{}, apiError(http.StatusBadRequest, "invalid date format: use YYYY-MM-DD")
	}
	if from > to {
		return db.AnalyticsFilter{}, apiError(http.StatusBadRequest, "from must not be after to")
	}
	if in.ActiveSince != "" && !timeutil.IsValidTimestamp(in.ActiveSince) {
		return db.AnalyticsFilter{}, apiError(http.StatusBadRequest, "invalid active_since: use RFC3339 timestamp")
	}
	return db.AnalyticsFilter{
		From:             from,
		To:               to,
		Machine:          in.Machine,
		Project:          in.Project,
		Agent:            in.Agent,
		Timezone:         tz,
		DayOfWeek:        optionalIntValue(in.DayOfWeek),
		Hour:             optionalIntValue(in.Hour),
		MinUserMessages:  in.MinUserMessages,
		ExcludeOneShot:   !in.IncludeOneShot,
		ExcludeAutomated: !in.IncludeAutomated,
		ActiveSince:      in.ActiveSince,
		Termination:      in.Termination,
	}, nil
}

func (s *Server) humaAnalyticsSummary(
	ctx context.Context,
	in *AnalyticsFilterInput,
) (*jsonOutput[db.AnalyticsSummary], error) {
	f, err := analyticsFilterFromInput(*in)
	if err != nil {
		return nil, err
	}
	result, err := s.db.GetAnalyticsSummary(ctx, f)
	if err != nil {
		return nil, internalError("analytics error", err)
	}
	return &jsonOutput[db.AnalyticsSummary]{Body: result}, nil
}

func (s *Server) humaAnalyticsActivity(
	ctx context.Context,
	in *analyticsActivityInput,
) (*jsonOutput[db.ActivityResponse], error) {
	f, err := analyticsFilterFromInput(in.AnalyticsFilterInput)
	if err != nil {
		return nil, err
	}
	result, err := s.db.GetAnalyticsActivity(ctx, f, string(in.Granularity))
	if err != nil {
		return nil, internalError("analytics error", err)
	}
	return &jsonOutput[db.ActivityResponse]{Body: result}, nil
}

func (s *Server) humaAnalyticsHeatmap(
	ctx context.Context,
	in *analyticsHeatmapInput,
) (*jsonOutput[db.HeatmapResponse], error) {
	f, err := analyticsFilterFromInput(in.AnalyticsFilterInput)
	if err != nil {
		return nil, err
	}
	result, err := s.db.GetAnalyticsHeatmap(ctx, f, string(in.Metric))
	if err != nil {
		return nil, internalError("analytics error", err)
	}
	return &jsonOutput[db.HeatmapResponse]{Body: result}, nil
}

func (s *Server) humaAnalyticsProjects(
	ctx context.Context,
	in *AnalyticsFilterInput,
) (*jsonOutput[db.ProjectsAnalyticsResponse], error) {
	f, err := analyticsFilterFromInput(*in)
	if err != nil {
		return nil, err
	}
	result, err := s.db.GetAnalyticsProjects(ctx, f)
	if err != nil {
		return nil, internalError("analytics error", err)
	}
	return &jsonOutput[db.ProjectsAnalyticsResponse]{Body: result}, nil
}

func (s *Server) humaAnalyticsHourOfWeek(
	ctx context.Context,
	in *AnalyticsFilterInput,
) (*jsonOutput[db.HourOfWeekResponse], error) {
	f, err := analyticsFilterFromInput(*in)
	if err != nil {
		return nil, err
	}
	result, err := s.db.GetAnalyticsHourOfWeek(ctx, f)
	if err != nil {
		return nil, internalError("analytics error", err)
	}
	return &jsonOutput[db.HourOfWeekResponse]{Body: result}, nil
}

func (s *Server) humaAnalyticsSessionShape(
	ctx context.Context,
	in *AnalyticsFilterInput,
) (*jsonOutput[db.SessionShapeResponse], error) {
	f, err := analyticsFilterFromInput(*in)
	if err != nil {
		return nil, err
	}
	result, err := s.db.GetAnalyticsSessionShape(ctx, f)
	if err != nil {
		return nil, internalError("analytics error", err)
	}
	return &jsonOutput[db.SessionShapeResponse]{Body: result}, nil
}

func (s *Server) humaAnalyticsVelocity(
	ctx context.Context,
	in *AnalyticsFilterInput,
) (*jsonOutput[db.VelocityResponse], error) {
	f, err := analyticsFilterFromInput(*in)
	if err != nil {
		return nil, err
	}
	result, err := s.db.GetAnalyticsVelocity(ctx, f)
	if err != nil {
		return nil, internalError("analytics error", err)
	}
	return &jsonOutput[db.VelocityResponse]{Body: result}, nil
}

func (s *Server) humaAnalyticsTools(
	ctx context.Context,
	in *AnalyticsFilterInput,
) (*jsonOutput[db.ToolsAnalyticsResponse], error) {
	f, err := analyticsFilterFromInput(*in)
	if err != nil {
		return nil, err
	}
	result, err := s.db.GetAnalyticsTools(ctx, f)
	if err != nil {
		return nil, internalError("analytics error", err)
	}
	return &jsonOutput[db.ToolsAnalyticsResponse]{Body: result}, nil
}

func (s *Server) humaAnalyticsTopSessions(
	ctx context.Context,
	in *analyticsTopSessionsInput,
) (*jsonOutput[db.TopSessionsResponse], error) {
	f, err := analyticsFilterFromInput(in.AnalyticsFilterInput)
	if err != nil {
		return nil, err
	}
	result, err := s.db.GetAnalyticsTopSessions(ctx, f, string(in.Metric))
	if err != nil {
		return nil, internalError("analytics error", err)
	}
	return &jsonOutput[db.TopSessionsResponse]{Body: result}, nil
}

func (s *Server) humaAnalyticsSignals(
	ctx context.Context,
	in *AnalyticsFilterInput,
) (*jsonOutput[db.SignalsAnalyticsResponse], error) {
	f, err := analyticsFilterFromInput(*in)
	if err != nil {
		return nil, err
	}
	result, err := s.db.GetAnalyticsSignals(ctx, f)
	if err != nil {
		return nil, internalError("analytics signals error", err)
	}
	return &jsonOutput[db.SignalsAnalyticsResponse]{Body: result}, nil
}

func (s *Server) humaTrendsTerms(
	ctx context.Context,
	in *trendsTermsInput,
) (*jsonOutput[db.TrendsTermsResponse], error) {
	f, err := analyticsFilterFromInput(in.AnalyticsFilterInput)
	if err != nil {
		return nil, err
	}
	terms, err := db.ParseTrendTerms(in.Term)
	if err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	result, err := s.db.GetTrendsTerms(ctx, f, terms, string(in.Granularity))
	if err != nil {
		return nil, internalError("trends terms error", err)
	}
	return &jsonOutput[db.TrendsTermsResponse]{Body: result}, nil
}

func usageFilterFromInput(in UsageFilterInput) (db.UsageFilter, error) {
	tz := in.Timezone
	if tz == "" {
		tz = "UTC"
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return db.UsageFilter{}, apiError(http.StatusBadRequest, "invalid timezone: "+tz)
	}
	from, to := defaultDateRange(in.From, in.To)
	if !timeutil.IsValidDate(from) || !timeutil.IsValidDate(to) {
		return db.UsageFilter{}, apiError(http.StatusBadRequest, "invalid date format: use YYYY-MM-DD")
	}
	if from > to {
		return db.UsageFilter{}, apiError(http.StatusBadRequest, "from must not be after to")
	}
	if in.ActiveSince != "" && !timeutil.IsValidTimestamp(in.ActiveSince) {
		return db.UsageFilter{}, apiError(http.StatusBadRequest, "invalid active_since: use RFC3339 timestamp")
	}
	return db.UsageFilter{
		From:             from,
		To:               to,
		Agent:            in.Agent,
		Project:          in.Project,
		Machine:          in.Machine,
		ExcludeProject:   in.ExcludeProject,
		ExcludeAgent:     in.ExcludeAgent,
		ExcludeModel:     in.ExcludeModel,
		Model:            in.Model,
		Timezone:         tz,
		MinUserMessages:  in.MinUserMessages,
		ExcludeOneShot:   !in.IncludeOneShot,
		ExcludeAutomated: !in.IncludeAutomated,
		ActiveSince:      in.ActiveSince,
		Breakdowns:       true,
	}, nil
}

func (s *Server) humaUsageSummary(
	ctx context.Context,
	in *UsageFilterInput,
) (*jsonOutput[UsageSummaryResponse], error) {
	f, err := usageFilterFromInput(*in)
	if err != nil {
		return nil, err
	}
	result, err := s.db.GetDailyUsage(ctx, f)
	if err != nil {
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("usage summary error", err)
	}
	scFilter := f
	scFilter.Breakdowns = false
	sessionCounts, err := s.db.GetUsageSessionCounts(ctx, scFilter)
	if err != nil {
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("usage session counts error", err)
	}
	resp := UsageSummaryResponse{
		From:          f.From,
		To:            f.To,
		Totals:        result.Totals,
		Daily:         result.Daily,
		ProjectTotals: foldProjectTotals(result.Daily),
		ModelTotals:   foldModelTotals(result.Daily),
		AgentTotals:   foldAgentTotals(result.Daily),
		SessionCounts: sessionCounts,
		CacheStats:    computeCacheStats(result.Totals),
		Comparison:    s.computeUsageComparison(ctx, f, result.Totals.TotalCost),
	}
	return &jsonOutput[UsageSummaryResponse]{Body: resp}, nil
}

func (s *Server) computeUsageComparison(
	ctx context.Context,
	f db.UsageFilter,
	currentCost float64,
) *Comparison {
	fromT, err := time.Parse("2006-01-02", f.From)
	if err != nil {
		return nil
	}
	toT, err := time.Parse("2006-01-02", f.To)
	if err != nil {
		return nil
	}
	days := int(toT.Sub(fromT).Hours()/24) + 1
	priorTo := fromT.AddDate(0, 0, -1)
	priorFrom := priorTo.AddDate(0, 0, -(days - 1))
	priorFilter := db.UsageFilter{
		From:             priorFrom.Format("2006-01-02"),
		To:               priorTo.Format("2006-01-02"),
		Agent:            f.Agent,
		Project:          f.Project,
		Machine:          f.Machine,
		Model:            f.Model,
		ExcludeProject:   f.ExcludeProject,
		ExcludeAgent:     f.ExcludeAgent,
		ExcludeModel:     f.ExcludeModel,
		Timezone:         f.Timezone,
		MinUserMessages:  f.MinUserMessages,
		ExcludeOneShot:   f.ExcludeOneShot,
		ExcludeAutomated: f.ExcludeAutomated,
		ActiveSince:      f.ActiveSince,
		Breakdowns:       false,
	}
	priorResult, err := s.db.GetDailyUsage(ctx, priorFilter)
	if err != nil {
		log.Printf("usage comparison error: %v", err)
		return nil
	}
	c := &Comparison{
		PriorFrom:      priorFilter.From,
		PriorTo:        priorFilter.To,
		PriorTotalCost: priorResult.Totals.TotalCost,
	}
	if c.PriorTotalCost > 0 {
		c.DeltaPct = (currentCost - c.PriorTotalCost) / c.PriorTotalCost
	}
	return c
}

func (s *Server) humaUsageTopSessions(
	ctx context.Context,
	in *usageTopSessionsInput,
) (*jsonOutput[[]db.TopSessionEntry], error) {
	f, err := usageFilterFromInput(in.UsageFilterInput)
	if err != nil {
		return nil, err
	}
	f.Breakdowns = false
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	entries, err := s.db.GetTopSessionsByCost(ctx, f, limit)
	if err != nil {
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("usage top sessions error", err)
	}
	return &jsonOutput[[]db.TopSessionEntry]{Body: entries}, nil
}

type openersResponse struct {
	Openers []Opener `json:"openers"`
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

type githubConfigResponse struct {
	Configured bool `json:"configured"`
}

type setGithubConfigInput struct {
	Body struct {
		Token string `json:"token" required:"true" minLength:"1" doc:"GitHub token"`
	}
}

type setGithubConfigResponse struct {
	Success  bool   `json:"success"`
	Username string `json:"username"`
}

type settingsInput struct {
	Body settingsUpdateRequest
}

type terminalConfigInput struct {
	Body terminalConfigBody
}

type terminalConfigBody struct {
	Mode       terminalMode `json:"mode" enum:"auto,custom,clipboard" doc:"Terminal launch mode"`
	CustomBin  string       `json:"custom_bin,omitempty" doc:"Terminal binary path when mode is custom"`
	CustomArgs string       `json:"custom_args,omitempty" doc:"Argument template containing {cmd} when mode is custom"`
}

func terminalConfigBodyFromConfig(tc config.TerminalConfig) terminalConfigBody {
	mode := terminalMode(tc.Mode)
	if mode == "" {
		mode = terminalModeAuto
	}
	return terminalConfigBody{
		Mode:       mode,
		CustomBin:  tc.CustomBin,
		CustomArgs: tc.CustomArgs,
	}
}

func (b terminalConfigBody) config() config.TerminalConfig {
	return config.TerminalConfig{
		Mode:       string(b.Mode),
		CustomBin:  b.CustomBin,
		CustomArgs: b.CustomArgs,
	}
}

type worktreeMappingCreateInput struct {
	Body worktreeMappingRequest
}

type worktreeMappingUpdateInput struct {
	ID   string `path:"id" required:"true" doc:"Mapping ID"`
	Body worktreeMappingRequest
}

type worktreeMappingPathInput struct {
	ID string `path:"id" required:"true" doc:"Mapping ID"`
}

type bulkStarInput struct {
	Body struct {
		SessionIDs []string `json:"session_ids" required:"true" doc:"Session IDs to star"`
	}
}

type renameSessionInput struct {
	ID   string `path:"id" required:"true" doc:"Session ID"`
	Body renameRequest
}

type trashResponse struct {
	Sessions []db.Session `json:"sessions"`
}

type emptyTrashResponse struct {
	Deleted int `json:"deleted"`
}

type starredResponse struct {
	SessionIDs []string `json:"session_ids"`
}

type pinsInput struct {
	Project string `query:"project" doc:"Filter by project"`
}

type pinsResponse struct {
	Pins []db.PinnedMessage `json:"pins"`
}

type pinMessageInput struct {
	ID        string `path:"id" required:"true" doc:"Session ID"`
	MessageID int64  `path:"messageId" required:"true" doc:"Message ordinal"`
	Body      pinRequest
}

type pinMessageResponse struct {
	ID int64 `json:"id"`
}

func (s *Server) humaListOpeners(
	_ context.Context,
	_ *emptyInput,
) (*jsonOutput[openersResponse], error) {
	openers := detectOpeners()
	if openers == nil {
		openers = []Opener{}
	}
	return &jsonOutput[openersResponse]{Body: openersResponse{Openers: openers}}, nil
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

func (s *Server) humaGetGithubConfig(
	_ context.Context,
	_ *emptyInput,
) (*jsonOutput[githubConfigResponse], error) {
	return &jsonOutput[githubConfigResponse]{
		Body: githubConfigResponse{Configured: s.githubToken() != ""},
	}, nil
}

func (s *Server) humaSetGithubConfig(
	ctx context.Context,
	in *setGithubConfigInput,
) (*jsonOutput[setGithubConfigResponse], error) {
	token := strings.TrimSpace(in.Body.Token)
	if token == "" {
		return nil, apiError(http.StatusBadRequest, "token required")
	}
	username, err := validateGithubToken(ctx, token)
	if err != nil {
		return nil, apiError(http.StatusUnauthorized, err.Error())
	}
	s.mu.Lock()
	err = s.cfg.SaveGithubToken(token)
	s.mu.Unlock()
	if err != nil {
		return nil, apiError(http.StatusInternalServerError, "failed to save token")
	}
	return &jsonOutput[setGithubConfigResponse]{
		Body: setGithubConfigResponse{Success: true, Username: username},
	}, nil
}

func (s *Server) humaGetTerminalConfig(
	_ context.Context,
	_ *emptyInput,
) (*jsonOutput[terminalConfigBody], error) {
	s.mu.RLock()
	tc := s.cfg.Terminal
	s.mu.RUnlock()
	return &jsonOutput[terminalConfigBody]{
		Body: terminalConfigBodyFromConfig(tc),
	}, nil
}

func (s *Server) humaSetTerminalConfig(
	_ context.Context,
	in *terminalConfigInput,
) (*jsonOutput[terminalConfigBody], error) {
	body := in.Body
	tc := body.config()
	switch terminalMode(tc.Mode) {
	case terminalModeAuto, terminalModeCustom, terminalModeClipboard:
	default:
		return nil, apiError(http.StatusBadRequest,
			`mode must be "auto", "custom", or "clipboard"`)
	}
	if tc.Mode == string(terminalModeCustom) && tc.CustomBin == "" {
		return nil, apiError(http.StatusBadRequest,
			`custom_bin is required when mode is "custom"`)
	}
	if tc.Mode == string(terminalModeCustom) {
		if tc.CustomArgs != "" && !strings.Contains(tc.CustomArgs, "{cmd}") {
			return nil, apiError(http.StatusBadRequest,
				`custom_args must contain the {cmd} placeholder so the resume command is passed to the terminal`)
		}
		if tc.CustomArgs != "" {
			if _, splitErr := shlex.Split(tc.CustomArgs); splitErr != nil {
				return nil, apiError(http.StatusBadRequest,
					fmt.Sprintf("custom_args has invalid shell syntax: %v", splitErr))
			}
		}
	}
	s.mu.Lock()
	err := s.cfg.SaveTerminalConfig(tc)
	s.mu.Unlock()
	if err != nil {
		return nil, internalError("save terminal config", err)
	}
	return &jsonOutput[terminalConfigBody]{
		Body: terminalConfigBodyFromConfig(tc),
	}, nil
}

func (s *Server) humaGetSettings(
	ctx context.Context,
	_ *emptyInput,
) (*jsonOutput[settingsResponse], error) {
	s.mu.RLock()
	dirs := make(map[string][]string)
	for _, def := range parser.Registry {
		if !def.FileBased && def.EnvVar == "" {
			continue
		}
		d := s.cfg.AgentDirs[def.Type]
		if d == nil {
			d = []string{}
		}
		dirs[string(def.Type)] = d
	}
	tc := s.cfg.Terminal
	if tc.Mode == "" {
		tc.Mode = string(terminalModeAuto)
	}
	resp := settingsResponse{
		AgentDirs: dirs,
		Terminal: terminalResponse{
			Mode:       tc.Mode,
			CustomBin:  tc.CustomBin,
			CustomArgs: tc.CustomArgs,
		},
		GithubConfigured: s.cfg.GithubToken != "",
		Host:             s.cfg.Host,
		Port:             s.cfg.Port,
		RequireAuth:      s.cfg.RequireAuth,
	}
	if isLocalhostContext(ctx) {
		resp.AuthToken = s.cfg.AuthToken
	}
	s.mu.RUnlock()
	return &jsonOutput[settingsResponse]{Body: resp}, nil
}

func (s *Server) humaUpdateSettings(
	ctx context.Context,
	in *settingsInput,
) (*jsonOutput[settingsResponse], error) {
	if s.db.ReadOnly() {
		return nil, apiError(http.StatusNotImplemented,
			"settings cannot be modified in read-only mode")
	}
	if in.Body.Terminal != nil {
		return nil, apiError(http.StatusBadRequest,
			"terminal config must be updated via POST /api/v1/config/terminal")
	}
	patch := make(map[string]any)
	if in.Body.AuthToken != nil {
		patch["auth_token"] = *in.Body.AuthToken
	}
	if in.Body.RequireAuth != nil {
		patch["require_auth"] = *in.Body.RequireAuth
	}
	if len(patch) > 0 {
		s.mu.Lock()
		err := s.cfg.SaveSettings(patch)
		if err == nil && s.cfg.RequireAuth {
			err = s.cfg.EnsureAuthToken()
		}
		s.mu.Unlock()
		if err != nil {
			return nil, internalError("save settings", err)
		}
	}
	return s.humaGetSettings(ctx, &emptyInput{})
}

func (s *Server) localWorktreeMappingHumaDB() (*db.DB, string, error) {
	localDB, ok := s.db.(*db.DB)
	if !ok || localDB == nil || localDB.ReadOnly() || s.engine == nil {
		return nil, "", apiError(http.StatusNotImplemented, "not available in remote mode")
	}
	machine := strings.TrimSpace(s.engine.Machine())
	if machine == "" {
		machine = "local"
	}
	return localDB, machine, nil
}

func (s *Server) humaListWorktreeMappings(
	ctx context.Context,
	_ *emptyInput,
) (*jsonOutput[worktreeMappingsResponse], error) {
	localDB, machine, err := s.localWorktreeMappingHumaDB()
	if err != nil {
		return nil, err
	}
	mappings, err := localDB.ListWorktreeProjectMappings(ctx, machine)
	if err != nil {
		return nil, internalError("list worktree mappings", err)
	}
	return &jsonOutput[worktreeMappingsResponse]{
		Body: worktreeMappingsResponse{Machine: machine, Mappings: mappings},
	}, nil
}

func (s *Server) humaCreateWorktreeMapping(
	ctx context.Context,
	in *worktreeMappingCreateInput,
) (*createdOutput[db.WorktreeProjectMapping], error) {
	localDB, machine, err := s.localWorktreeMappingHumaDB()
	if err != nil {
		return nil, err
	}
	if in.Body.PathPrefix == nil || in.Body.Project == nil {
		return nil, apiError(http.StatusBadRequest, "path_prefix and project are required")
	}
	enabled := true
	if in.Body.Enabled != nil {
		enabled = *in.Body.Enabled
	}
	mapping, err := localDB.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine:    machine,
		PathPrefix: *in.Body.PathPrefix,
		Project:    *in.Body.Project,
		Enabled:    enabled,
	})
	if err != nil {
		return nil, humaWorktreeMappingError(err)
	}
	return &createdOutput[db.WorktreeProjectMapping]{Status: http.StatusCreated, Body: mapping}, nil
}

func (s *Server) humaUpdateWorktreeMapping(
	ctx context.Context,
	in *worktreeMappingUpdateInput,
) (*jsonOutput[db.WorktreeProjectMapping], error) {
	localDB, machine, err := s.localWorktreeMappingHumaDB()
	if err != nil {
		return nil, err
	}
	id, err := parseWorktreeMappingHumaID(in.ID)
	if err != nil {
		return nil, err
	}
	if in.Body.PathPrefix == nil || in.Body.Project == nil || in.Body.Enabled == nil {
		return nil, apiError(http.StatusBadRequest,
			"path_prefix, project, and enabled are required")
	}
	mapping, err := localDB.UpdateWorktreeProjectMapping(ctx, machine, id, db.WorktreeProjectMapping{
		PathPrefix: *in.Body.PathPrefix,
		Project:    *in.Body.Project,
		Enabled:    *in.Body.Enabled,
	})
	if err != nil {
		return nil, humaWorktreeMappingError(err)
	}
	return &jsonOutput[db.WorktreeProjectMapping]{Body: mapping}, nil
}

func (s *Server) humaDeleteWorktreeMapping(
	ctx context.Context,
	in *worktreeMappingPathInput,
) (*noContentOutput, error) {
	localDB, machine, err := s.localWorktreeMappingHumaDB()
	if err != nil {
		return nil, err
	}
	id, err := parseWorktreeMappingHumaID(in.ID)
	if err != nil {
		return nil, err
	}
	err = localDB.DeleteWorktreeProjectMapping(ctx, machine, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apiError(http.StatusNotFound, "mapping not found")
	}
	if err != nil {
		return nil, internalError("delete worktree mapping", err)
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

func parseWorktreeMappingHumaID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, apiError(http.StatusNotFound, "mapping not found")
	}
	return id, nil
}

func (s *Server) humaApplyWorktreeMappings(
	ctx context.Context,
	_ *emptyInput,
) (*jsonOutput[applyWorktreeMappingsResponse], error) {
	localDB, machine, err := s.localWorktreeMappingHumaDB()
	if err != nil {
		return nil, err
	}
	result, err := localDB.ApplyWorktreeProjectMappings(ctx, machine)
	if err != nil {
		return nil, internalError("apply worktree mappings", err)
	}
	return &jsonOutput[applyWorktreeMappingsResponse]{
		Body: applyWorktreeMappingsResponse{
			Machine:                            machine,
			ApplyWorktreeProjectMappingsResult: result,
		},
	}, nil
}

func humaWorktreeMappingError(err error) error {
	switch {
	case strings.Contains(err.Error(), "required"):
		return apiError(http.StatusBadRequest, err.Error())
	case errors.Is(err, db.ErrWorktreeMappingDuplicate):
		return apiError(http.StatusConflict, "worktree mapping already exists")
	case errors.Is(err, sql.ErrNoRows):
		return apiError(http.StatusNotFound, "mapping not found")
	default:
		return internalError("worktree mapping write", err)
	}
}

func (s *Server) humaListStarred(
	ctx context.Context,
	_ *emptyInput,
) (*jsonOutput[starredResponse], error) {
	ids, err := s.db.ListStarredSessionIDs(ctx)
	if err != nil {
		return nil, internalError("list starred", err)
	}
	if ids == nil {
		ids = []string{}
	}
	return &jsonOutput[starredResponse]{Body: starredResponse{SessionIDs: ids}}, nil
}

func (s *Server) humaStarSession(
	_ context.Context,
	in *idPathInput,
) (*noContentOutput, error) {
	ok, err := s.db.StarSession(in.ID)
	if err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("star session", err)
	}
	if !ok {
		return nil, apiError(http.StatusNotFound, "session not found")
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

func (s *Server) humaUnstarSession(
	_ context.Context,
	in *idPathInput,
) (*noContentOutput, error) {
	if err := s.db.UnstarSession(in.ID); err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("unstar session", err)
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

func (s *Server) humaBulkStar(
	_ context.Context,
	in *bulkStarInput,
) (*noContentOutput, error) {
	if len(in.Body.SessionIDs) == 0 {
		return &noContentOutput{Status: http.StatusNoContent}, nil
	}
	if err := s.db.BulkStarSessions(in.Body.SessionIDs); err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("bulk star", err)
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
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

func (s *Server) humaListPins(
	ctx context.Context,
	in *pinsInput,
) (*jsonOutput[pinsResponse], error) {
	pins, err := s.db.ListPinnedMessages(ctx, "", in.Project)
	if err != nil {
		return nil, internalError("list pins", err)
	}
	if pins == nil {
		pins = []db.PinnedMessage{}
	}
	return &jsonOutput[pinsResponse]{Body: pinsResponse{Pins: pins}}, nil
}

func (s *Server) humaListSessionPins(
	ctx context.Context,
	in *idPathInput,
) (*jsonOutput[pinsResponse], error) {
	pins, err := s.db.ListPinnedMessages(ctx, in.ID, "")
	if err != nil {
		return nil, internalError("list session pins", err)
	}
	if pins == nil {
		pins = []db.PinnedMessage{}
	}
	return &jsonOutput[pinsResponse]{Body: pinsResponse{Pins: pins}}, nil
}

func (s *Server) humaPinMessage(
	_ context.Context,
	in *pinMessageInput,
) (*createdOutput[pinMessageResponse], error) {
	id, err := s.db.PinMessage(in.ID, in.MessageID, in.Body.Note)
	if err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("pin message", err)
	}
	if id == 0 {
		return nil, apiError(http.StatusBadRequest,
			"message does not belong to this session")
	}
	return &createdOutput[pinMessageResponse]{
		Status: http.StatusCreated,
		Body:   pinMessageResponse{ID: id},
	}, nil
}

func (s *Server) humaUnpinMessage(
	_ context.Context,
	in *messagePathInput,
) (*noContentOutput, error) {
	if err := s.db.UnpinMessage(in.ID, in.MessageID); err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("unpin message", err)
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

type statsInput struct {
	BoolIncludeInput
}

type projectsResponse struct {
	Projects []db.ProjectInfo `json:"projects"`
}

type machinesResponse struct {
	Machines []string `json:"machines"`
}

type agentsResponse struct {
	Agents []db.AgentInfo `json:"agents"`
}

type syncStatusResponse struct {
	LastSync string             `json:"last_sync"`
	Stats    *syncpkg.SyncStats `json:"stats"`
}

type syncStatsResponse = syncpkg.SyncStats

type scanSecretsInput struct {
	Backfill bool   `query:"backfill" doc:"Backfill all matching sessions"`
	Project  string `query:"project" doc:"Filter by project"`
	Agent    string `query:"agent" doc:"Filter by agent"`
	DateFrom string `query:"date_from" format:"date" doc:"Filter start date"`
	DateTo   string `query:"date_to" format:"date" doc:"Filter end date"`
}

type sessionSyncInput struct {
	Body service.SyncInput
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

type importArchiveInput struct {
	Accept  string `header:"Accept" doc:"Use text/event-stream to stream progress"`
	RawBody huma.MultipartFormFiles[importArchiveForm]
}

type importArchiveForm struct {
	File huma.FormFile `form:"file" contentType:"application/octet-stream" required:"true"`
}

type assetInput struct {
	Filename string `path:"filename" required:"true" doc:"Asset filename"`
}

type markdownInput struct {
	ID    string        `path:"id" required:"true" doc:"Session ID"`
	Depth markdownDepth `query:"depth" enum:"1,all" doc:"Child session depth"`
}

type insightsInput struct {
	Type    insightType `query:"type" enum:"daily_activity,agent_analysis" doc:"Insight type"`
	Project string      `query:"project" doc:"Filter by project"`
}

type insightsResponse struct {
	Insights []db.Insight `json:"insights"`
}

type generateInsightInput struct {
	Body generateInsightRequest
}

func (s *Server) humaGetStats(
	ctx context.Context,
	in *statsInput,
) (*jsonOutput[db.Stats], error) {
	stats, err := s.db.GetStats(ctx, !in.IncludeOneShot, !in.IncludeAutomated)
	if err != nil {
		return nil, serverError(err)
	}
	return &jsonOutput[db.Stats]{Body: stats}, nil
}

func (s *Server) humaListProjects(
	ctx context.Context,
	in *statsInput,
) (*jsonOutput[projectsResponse], error) {
	projects, err := s.db.GetProjects(ctx, !in.IncludeOneShot, !in.IncludeAutomated)
	if err != nil {
		return nil, serverError(err)
	}
	return &jsonOutput[projectsResponse]{Body: projectsResponse{Projects: projects}}, nil
}

func (s *Server) humaListMachines(
	ctx context.Context,
	in *statsInput,
) (*jsonOutput[machinesResponse], error) {
	machines, err := s.db.GetMachines(ctx, !in.IncludeOneShot, !in.IncludeAutomated)
	if err != nil {
		return nil, serverError(err)
	}
	return &jsonOutput[machinesResponse]{Body: machinesResponse{Machines: machines}}, nil
}

func (s *Server) humaListAgents(
	ctx context.Context,
	in *statsInput,
) (*jsonOutput[agentsResponse], error) {
	agents, err := s.db.GetAgents(ctx, !in.IncludeOneShot, !in.IncludeAutomated)
	if err != nil {
		return nil, serverError(err)
	}
	return &jsonOutput[agentsResponse]{Body: agentsResponse{Agents: agents}}, nil
}

func (s *Server) humaGetVersion(
	_ context.Context,
	_ *emptyInput,
) (*jsonOutput[VersionInfo], error) {
	return &jsonOutput[VersionInfo]{Body: s.version}, nil
}

func (s *Server) humaSyncStatus(
	_ context.Context,
	_ *emptyInput,
) (*jsonOutput[syncStatusResponse], error) {
	if s.engine == nil {
		return &jsonOutput[syncStatusResponse]{Body: syncStatusResponse{}}, nil
	}
	lastSync := s.engine.LastSync()
	stats := s.engine.LastSyncStats()
	var lastSyncStr string
	if !lastSync.IsZero() {
		lastSyncStr = lastSync.Format(time.RFC3339)
	}
	return &jsonOutput[syncStatusResponse]{
		Body: syncStatusResponse{LastSync: lastSyncStr, Stats: &stats},
	}, nil
}

func (s *Server) humaTriggerSync(
	ctx context.Context,
	_ *emptyInput,
) (*huma.StreamResponse, error) {
	if s.engine == nil {
		return nil, apiError(http.StatusNotImplemented, "not available in remote mode")
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		stream, ok := newHumaSSEStream(hctx)
		if !ok {
			stats := s.engine.SyncAll(ctx, nil)
			writeHumaJSON(hctx, http.StatusOK, stats)
			return
		}
		stats := s.engine.SyncAll(ctx, func(p syncpkg.Progress) {
			stream.SendJSON("progress", p)
		})
		stream.SendJSON("done", stats)
	}}, nil
}

func (s *Server) humaTriggerResync(
	ctx context.Context,
	_ *emptyInput,
) (*huma.StreamResponse, error) {
	if s.engine == nil {
		return nil, apiError(http.StatusNotImplemented, "not available in remote mode")
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		stream, ok := newHumaSSEStream(hctx)
		if !ok {
			stats := s.engine.ResyncAll(ctx, nil)
			writeHumaJSON(hctx, http.StatusOK, stats)
			return
		}
		stats := s.engine.ResyncAll(ctx, func(p syncpkg.Progress) {
			stream.SendJSON("progress", p)
		})
		stream.SendJSON("done", stats)
	}}, nil
}

func (s *Server) humaScanSecrets(
	ctx context.Context,
	in *scanSecretsInput,
) (*huma.StreamResponse, error) {
	if err := validateDateFilterValues("", in.DateFrom, in.DateTo, ""); err != nil {
		return nil, err
	}
	if s.sessions == nil {
		return nil, apiError(http.StatusNotImplemented, "not available in remote mode")
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		stream, ok := newHumaSSEStream(hctx)
		if !ok {
			writeHumaJSON(hctx, http.StatusInternalServerError,
				apiErrorResponse{Message: "streaming not supported"})
			return
		}
		summary, err := s.sessions.ScanSecrets(ctx, service.SecretScanInput{
			Backfill: in.Backfill,
			Project:  in.Project,
			Agent:    in.Agent,
			DateFrom: in.DateFrom,
			DateTo:   in.DateTo,
		}, func(p service.SecretScanProgress) {
			stream.SendJSON("progress", p)
		})
		if err != nil {
			stream.SendJSON("error", map[string]string{"error": err.Error()})
			return
		}
		stream.SendJSON("done", summary)
	}}, nil
}

func (s *Server) humaSyncSession(
	ctx context.Context,
	in *sessionSyncInput,
) (*jsonOutput[*service.SessionDetail], error) {
	if (in.Body.Path == "" && in.Body.ID == "") ||
		(in.Body.Path != "" && in.Body.ID != "") {
		return nil, apiError(http.StatusBadRequest, "exactly one of 'path' or 'id' is required")
	}
	detail, err := s.sessions.Sync(ctx, in.Body)
	if err != nil {
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		if errors.Is(err, db.ErrSessionExcluded) ||
			errors.Is(err, db.ErrSessionTrashed) {
			return nil, apiError(http.StatusConflict, err.Error())
		}
		return nil, apiError(http.StatusInternalServerError, err.Error())
	}
	return &jsonOutput[*service.SessionDetail]{Body: detail}, nil
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

func (s *Server) humaGetAsset(
	_ context.Context,
	in *assetInput,
) (*bytesOutput, error) {
	filename := in.Filename
	if filename == "" {
		return nil, apiError(http.StatusBadRequest, "missing filename")
	}
	if strings.Contains(filename, "..") ||
		strings.Contains(filename, "/") ||
		strings.Contains(filename, "\\") {
		return nil, apiError(http.StatusBadRequest, "invalid filename")
	}
	ext := strings.ToLower(filepath.Ext(filename))
	contentType, ok := safeImageTypes[ext]
	if !ok {
		return nil, apiError(http.StatusForbidden, "unsupported asset type")
	}
	filePath := filepath.Join(s.cfg.DataDir, "assets", filename)
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, apiError(http.StatusNotFound, "asset not found")
	}
	return &bytesOutput{
		ContentType:  contentType,
		NoSniff:      "nosniff",
		CacheControl: "public, max-age=31536000, immutable",
		Body:         data,
	}, nil
}

func newHumaSSEStream(ctx huma.Context) (*SSEStream, bool) {
	w, ok := ctx.BodyWriter().(http.ResponseWriter)
	if !ok {
		return nil, false
	}
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	ctx.SetHeader("Content-Type", "text/event-stream")
	ctx.SetHeader("Cache-Control", "no-cache")
	ctx.SetHeader("Connection", "keep-alive")
	f.Flush()
	return &SSEStream{w: w, f: f}, true
}

func writeHumaJSON(ctx huma.Context, status int, value any) {
	ctx.SetHeader("Content-Type", "application/json")
	ctx.SetStatus(status)
	_ = sjson(ctx.BodyWriter(), value)
}

func sjson(w io.Writer, value any) error {
	return json.NewEncoder(w).Encode(value)
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

func (s *Server) humaImportClaudeAI(
	ctx context.Context,
	in *importArchiveInput,
) (*huma.StreamResponse, error) {
	if s.db.ReadOnly() {
		return nil, apiError(http.StatusNotImplemented,
			"import not available in read-only mode")
	}
	file := in.RawBody.Data().File
	if !file.IsSet {
		return nil, apiError(http.StatusBadRequest,
			"missing 'file' field in form data")
	}
	if !strings.Contains(in.Accept, "text/event-stream") {
		stats, err := s.importClaudeAIFromFile(ctx, file)
		if err != nil {
			return nil, err
		}
		return jsonStreamResponse(stats), nil
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		stream, ok := newHumaSSEStream(hctx)
		if !ok {
			writeHumaJSON(hctx, http.StatusInternalServerError,
				apiErrorResponse{Message: "streaming not supported"})
			return
		}
		stats, err := s.importClaudeAIFromFileWithCallbacks(hctx.Context(), file, &importer.ImportCallbacks{
			OnProgress: func(stats importer.ImportStats) {
				stream.SendJSON("progress", stats)
			},
			OnIndexing: func() {
				stream.SendJSON("indexing", struct{}{})
			},
		})
		if err != nil {
			stream.SendJSON("error", map[string]string{"error": err.Error()})
			return
		}
		stream.SendJSON("done", stats)
	}}, nil
}

func (s *Server) importClaudeAIFromFile(
	ctx context.Context,
	file huma.FormFile,
) (importer.ImportStats, error) {
	return s.importClaudeAIFromFileWithCallbacks(ctx, file, nil)
}

func (s *Server) importClaudeAIFromFileWithCallbacks(
	ctx context.Context,
	file huma.FormFile,
	cb *importer.ImportCallbacks,
) (importer.ImportStats, error) {
	reader, cleanup, err := claudeImportReader(file)
	if err != nil {
		return importer.ImportStats{}, err
	}
	defer cleanup()
	stats, err := importer.ImportClaudeAI(ctx, s.db, reader, cb)
	if err != nil {
		return importer.ImportStats{}, apiError(http.StatusInternalServerError,
			"import failed: "+err.Error())
	}
	return stats, nil
}

func claudeImportReader(file huma.FormFile) (io.Reader, func(), error) {
	cleanup := func() {}
	reader := io.Reader(file)
	if strings.HasSuffix(strings.ToLower(file.Filename), ".zip") {
		tmpFile, tmpErr := os.CreateTemp("", "claude-import-*.zip")
		if tmpErr != nil {
			return nil, cleanup, apiError(http.StatusInternalServerError,
				"failed to create temp file")
		}
		tmpName := tmpFile.Name()
		cleanup = func() { _ = os.Remove(tmpName) }
		if _, tmpErr = io.Copy(tmpFile, file); tmpErr != nil {
			_ = tmpFile.Close()
			cleanup()
			return nil, func() {}, apiError(http.StatusInternalServerError,
				"failed to save upload")
		}
		_ = tmpFile.Close()
		dir, zipCleanup, extractErr := importer.ExtractZip(tmpName)
		if extractErr != nil {
			cleanup()
			return nil, func() {}, apiError(http.StatusBadRequest,
				"failed to extract zip: "+extractErr.Error())
		}
		cleanup = func() {
			zipCleanup()
			_ = os.Remove(tmpName)
		}
		jsonPath := filepath.Join(dir, "conversations.json")
		jsonFile, openErr := os.Open(jsonPath)
		if openErr != nil {
			cleanup()
			return nil, func() {}, apiError(http.StatusBadRequest,
				"no conversations.json found in zip")
		}
		oldCleanup := cleanup
		cleanup = func() {
			_ = jsonFile.Close()
			oldCleanup()
		}
		reader = jsonFile
	}
	return reader, cleanup, nil
}

func (s *Server) humaImportChatGPT(
	ctx context.Context,
	in *importArchiveInput,
) (*huma.StreamResponse, error) {
	if s.db.ReadOnly() {
		return nil, apiError(http.StatusNotImplemented,
			"import not available in read-only mode")
	}
	file := in.RawBody.Data().File
	if !file.IsSet {
		return nil, apiError(http.StatusBadRequest,
			"missing 'file' field in form data")
	}
	if !strings.HasSuffix(strings.ToLower(file.Filename), ".zip") {
		return nil, apiError(http.StatusBadRequest,
			"ChatGPT import requires a .zip file")
	}
	if !strings.Contains(in.Accept, "text/event-stream") {
		stats, err := s.importChatGPTFromFile(ctx, file, nil)
		if err != nil {
			return nil, err
		}
		return jsonStreamResponse(stats), nil
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		stream, ok := newHumaSSEStream(hctx)
		if !ok {
			writeHumaJSON(hctx, http.StatusInternalServerError,
				apiErrorResponse{Message: "streaming not supported"})
			return
		}
		stats, err := s.importChatGPTFromFile(hctx.Context(), file, &importer.ImportCallbacks{
			OnProgress: func(stats importer.ImportStats) {
				stream.SendJSON("progress", stats)
			},
			OnIndexing: func() {
				stream.SendJSON("indexing", struct{}{})
			},
		})
		if err != nil {
			stream.SendJSON("error", map[string]string{"error": err.Error()})
			return
		}
		stream.SendJSON("done", stats)
	}}, nil
}

func (s *Server) importChatGPTFromFile(
	ctx context.Context,
	file huma.FormFile,
	cb *importer.ImportCallbacks,
) (importer.ImportStats, error) {
	tmpFile, err := os.CreateTemp("", "chatgpt-import-*.zip")
	if err != nil {
		return importer.ImportStats{}, apiError(http.StatusInternalServerError,
			"failed to create temp file")
	}
	tmpName := tmpFile.Name()
	defer os.Remove(tmpName)
	if _, err = io.Copy(tmpFile, file); err != nil {
		_ = tmpFile.Close()
		return importer.ImportStats{}, apiError(http.StatusInternalServerError,
			"failed to save upload")
	}
	_ = tmpFile.Close()
	dir, cleanup, err := importer.ExtractZip(tmpName)
	if err != nil {
		return importer.ImportStats{}, apiError(http.StatusBadRequest,
			"failed to extract zip: "+err.Error())
	}
	defer cleanup()
	stats, err := importer.ImportChatGPT(ctx, s.db, dir,
		filepath.Join(s.cfg.DataDir, "assets"), cb)
	if err != nil {
		return importer.ImportStats{}, apiError(http.StatusInternalServerError,
			"import failed: "+err.Error())
	}
	return stats, nil
}

func jsonStreamResponse(value any) *huma.StreamResponse {
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		hctx.SetHeader("Content-Type", "application/json")
		_ = json.NewEncoder(hctx.BodyWriter()).Encode(value)
	}}
}

func (s *Server) humaListInsights(
	ctx context.Context,
	in *insightsInput,
) (*jsonOutput[insightsResponse], error) {
	insights, err := s.db.ListInsights(ctx, db.InsightFilter{
		Type:    string(in.Type),
		Project: in.Project,
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

func (s *Server) humaDeleteInsight(
	ctx context.Context,
	in *intIDPathInput,
) (*noContentOutput, error) {
	existing, err := s.db.GetInsight(ctx, in.ID)
	if err != nil {
		return nil, serverError(err)
	}
	if existing == nil {
		return nil, apiError(http.StatusNotFound, "insight not found")
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
	if s.db.ReadOnly() {
		return nil, apiError(http.StatusNotImplemented,
			"insight generation is not available in read-only mode")
	}
	req := in.Body
	if !validInsightTypes[req.Type] {
		return nil, apiError(http.StatusBadRequest,
			"invalid type: must be daily_activity or agent_analysis")
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
		prompt, err := insight.BuildPrompt(hctx.Context(), s.db, insight.GenerateRequest{
			Type:     req.Type,
			DateFrom: req.DateFrom,
			DateTo:   req.DateTo,
			Project:  req.Project,
			Prompt:   req.Prompt,
		})
		if err != nil {
			log.Printf("insight prompt error: %v", err)
			sendJSON("error", map[string]string{"message": "failed to build prompt"})
			return
		}
		genCtx, cancel := context.WithTimeout(hctx.Context(), 10*time.Minute)
		defer cancel()

		const (
			maxBufferedLogEvents = 256
			logDrainTimeout      = 2 * time.Second
			logStopWaitTimeout   = 500 * time.Millisecond
		)
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

func (s *Server) humaCheckUpdate(
	_ context.Context,
	_ *emptyInput,
) (*jsonOutput[updateCheckResponse], error) {
	if s.cfg.DisableUpdateCheck {
		return &jsonOutput[updateCheckResponse]{
			Body: updateCheckResponse{CurrentVersion: s.version.Version},
		}, nil
	}
	checkFn := s.updateCheckFn
	if checkFn == nil {
		checkFn = update.CheckForUpdate
	}
	info, err := checkFn(s.version.Version, false, s.dataDir)
	if err != nil || info == nil {
		return &jsonOutput[updateCheckResponse]{
			Body: updateCheckResponse{CurrentVersion: s.version.Version},
		}, nil
	}
	return &jsonOutput[updateCheckResponse]{
		Body: updateCheckResponse{
			UpdateAvailable: !info.IsDevBuild,
			CurrentVersion:  info.CurrentVersion,
			LatestVersion:   info.LatestVersion,
			IsDevBuild:      info.IsDevBuild,
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
