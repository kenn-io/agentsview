package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"reflect"
	"strings"
	stdsync "sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/timeutil"
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
	return huma.SchemaFromType(r, reflect.TypeFor[int]())
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

// optionalBoolParam distinguishes an omitted query param from an explicit
// false, so a sort key's canonical direction is used unless ?descending is set.
type optionalBoolParam struct {
	Value bool
	IsSet bool
}

func (p optionalBoolParam) Schema(r huma.Registry) *huma.Schema {
	return huma.SchemaFromType(r, reflect.TypeFor[bool]())
}

func (p *optionalBoolParam) Receiver() reflect.Value {
	return reflect.ValueOf(p).Elem().FieldByName("Value")
}

func (p *optionalBoolParam) OnParamSet(isSet bool, _ any) {
	p.IsSet = isSet
}

func optionalBoolValue(p optionalBoolParam) *bool {
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
	options ...func(*huma.Operation),
) {
	routeOptions := append([]func(*huma.Operation){streamResponse()}, options...)
	registerRoute(group, method, path, summary, handler, routeOptions...)
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

func streamJSONResponse() func(*huma.Operation) {
	return func(op *huma.Operation) {
		if op.Responses == nil {
			op.Responses = map[string]*huma.Response{}
		}
		resp := op.Responses["200"]
		if resp == nil {
			resp = &huma.Response{Description: "OK"}
			op.Responses["200"] = resp
		}
		if resp.Content == nil {
			resp.Content = map[string]*huma.MediaType{}
		}
		resp.Content["application/json"] = &huma.MediaType{
			Schema: &huma.Schema{Type: huma.TypeObject},
		}
	}
}

func (s *Server) humaTimeout() func(*huma.Operation) {
	return func(op *huma.Operation) {
		op.Middlewares = append(op.Middlewares, func(ctx huma.Context, next func(huma.Context)) {
			if errors.Is(ctx.Context().Err(), context.Canceled) {
				return
			}
			if errors.Is(ctx.Context().Err(), context.DeadlineExceeded) {
				next(ctx)
				return
			}
			if s.cfg.WriteTimeout <= 0 {
				if s.handlerDelay > 0 {
					time.Sleep(s.handlerDelay)
				}
				next(ctx)
				return
			}

			req, writer := humago.Unwrap(ctx)
			timeoutHandler := http.TimeoutHandler(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if s.handlerDelay > 0 {
						time.Sleep(s.handlerDelay)
					}
					next(huma.WithContext(humago.NewContext(ctx.Operation(), r, w), r.Context()))
				}),
				s.cfg.WriteTimeout,
				`{"error":"request timed out"}`,
			)
			tw := &contentTypeWrapper{
				ResponseWriter: writer,
				contentType:    "application/json",
				triggerStatus:  http.StatusServiceUnavailable,
			}
			timeoutHandler.ServeHTTP(tw, req.WithContext(ctx.Context()))
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
	if errors.Is(err, context.Canceled) {
		return nil
	}
	if handled := handleHumaContextError(err); handled != nil {
		return handled
	}
	return apiError(http.StatusInternalServerError, err.Error())
}

func internalError(logPrefix string, err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
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

type BoolIncludeInput struct {
	IncludeOneShot   bool `query:"include_one_shot" doc:"Include one-shot sessions"`
	IncludeAutomated bool `query:"include_automated" doc:"Include automated sessions"`
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
