// ABOUTME: httpBackend implements SessionService by proxying HTTP
// ABOUTME: calls to a running agentsview daemon.
package servicehttp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

// errHTTPNotFound is returned by the generated client adapter for 404 responses so callers
// can distinguish "no such resource" from other transport errors
// without string-matching the status code. Kept unexported since
// only Get currently consumes it; other paths map status codes
// explicitly below.
var errHTTPNotFound = errors.New("http: not found")

// errHTTPNotImplemented is returned (wrapped in *notImplementedBodyError) by
// the generated client adapter for 501 responses so callers can map a capability-absent daemon
// (e.g. search with no FTS index) to a typed sentinel instead of
// string-matching the status.
var errHTTPNotImplemented = errors.New("http: not implemented")

// notImplementedBodyError wraps errHTTPNotImplemented with the 501 response's
// error message, so callers that need cause-specific detail — e.g.
// SearchContent's "index is building: N% complete" or "index is stale ...
// --full-rebuild" remediation — can recover it instead of seeing only the
// bare sentinel. errors.Is(err, errHTTPNotImplemented) still holds for every
// caller that only cares about the status.
type notImplementedBodyError struct {
	message string
}

func (e *notImplementedBodyError) Error() string { return errHTTPNotImplemented.Error() }
func (e *notImplementedBodyError) Unwrap() error { return errHTTPNotImplemented }

type httpStatusError struct {
	method     string
	path       string
	statusCode int
	body       []byte
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf(
		"%s %s: HTTP %d: %s", e.method, e.path, e.statusCode, e.body,
	)
}

func (e *httpStatusError) message() string {
	return notImplementedMessage(e.body)
}

// notImplementedMessage extracts the {"error": "..."} message huma's error
// responses carry, falling back to the raw (trimmed) body when it isn't in
// that shape.
func notImplementedMessage(body []byte) string {
	var apiErr struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Error != "" {
		return apiErr.Error
	}
	return strings.TrimSpace(string(body))
}

type httpBackend struct {
	baseURL           string
	browserURL        string
	client            *http.Client
	longRunningClient *http.Client
	readOnly          bool
	recallQueries     bool
	token             string
}

const recallNonRecordingAPIVersion = 4

// HTTPServerCapabilities is the subset of version metadata needed to expose
// client features safely for an explicitly selected daemon.
type HTTPServerCapabilities struct {
	ReadOnly   bool `json:"read_only"`
	APIVersion int  `json:"api_version"`
}

// NewHTTPBackend constructs a SessionService that proxies to a
// running agentsview daemon at baseURL. When readOnly is true,
// Sync returns a clear error without making the HTTP round-trip.
// token, when non-empty, is attached as `Authorization: Bearer ...`
// on every request so the backend works against daemons running
// with require_auth=true. browserURL selects the browser-facing address;
// an empty value uses baseURL.
func NewHTTPBackend(baseURL, token string, readOnly bool, browserURL string) service.SessionService {
	b := newHTTPBackend(baseURL, token, readOnly, !readOnly)
	if browserURL != "" {
		b.browserURL = browserURL
	}
	return b
}

// NewHTTPBackendForServer constructs a backend whose advertised capabilities
// are limited by metadata probed from an explicitly selected daemon.
func NewHTTPBackendForServer(
	baseURL, token string, capabilities HTTPServerCapabilities,
) service.SessionService {
	return newHTTPBackend(
		baseURL,
		token,
		capabilities.ReadOnly,
		!capabilities.ReadOnly &&
			capabilities.APIVersion >= recallNonRecordingAPIVersion,
	)
}

func newHTTPBackend(
	baseURL, token string, readOnly, recallQueries bool,
) *httpBackend {
	return &httpBackend{
		baseURL:           strings.TrimSuffix(baseURL, "/"),
		browserURL:        baseURL,
		client:            &http.Client{Timeout: 30 * time.Second},
		longRunningClient: &http.Client{Timeout: 0},
		readOnly:          readOnly,
		recallQueries:     recallQueries,
		token:             token,
	}
}

// ProbeHTTPServerCapabilities reads the metadata needed to expose tools safely
// for an explicit daemon URL. Such callers do not have a local runtime record.
func ProbeHTTPServerCapabilities(
	ctx context.Context, baseURL, token string,
) (HTTPServerCapabilities, error) {
	b := newHTTPBackend(baseURL, token, false, false)
	api, err := b.apiClient(b.client)
	if err != nil {
		return HTTPServerCapabilities{}, err
	}
	response, err := api.GetAPIV1VersionWithResponse(ctx)
	if response == nil {
		return HTTPServerCapabilities{}, err
	}
	err = serviceResponseError(response.HTTPResponse, response.Body, err)
	if err != nil {
		return HTTPServerCapabilities{},
			fmt.Errorf("probing server capabilities: %w", err)
	}
	return HTTPServerCapabilities{ReadOnly: response.JSON200.ReadOnly != nil && *response.JSON200.ReadOnly, APIVersion: int(response.JSON200.APIVersion)}, nil
}

func (b *httpBackend) SupportsRecallQueries() bool { return b.recallQueries }

func (b *httpBackend) MachineLabels(
	ctx context.Context,
) (service.MachineLabelCatalog, error) {
	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	response, err := api.GetAPIV1MachinesWithResponse(ctx, &apiclient.GetAPIV1MachinesRequestOptions{})
	if response == nil {
		return nil, err
	}
	if err := serviceResponseError(response.HTTPResponse, response.Body, err); err != nil {
		return nil, err
	}
	return service.MachineLabelCatalog(response.JSON200.MachineLabels), nil
}

func (b *httpBackend) Get(
	ctx context.Context, id string,
) (*service.SessionDetail, error) {
	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	response, err := api.GetAPIV1SessionsIDWithResponse(ctx, &apiclient.GetAPIV1SessionsIDRequestOptions{PathParams: &apiclient.GetAPIV1SessionsIDPath{ID: url.PathEscape(id)}})
	if response == nil {
		return nil, err
	}
	out := response.JSON200
	err = serviceResponseError(response.HTTPResponse, response.Body, err)
	if errors.Is(err, errHTTPNotFound) {
		// Match directBackend.Get: absent session returns (nil, nil)
		// so transport swaps stay neutral.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out.WebURL = b.sessionWebURL(out.ID)
	return out, nil
}

func (b *httpBackend) FindSessionIDsByPartial(
	ctx context.Context, partial string, limit int,
) ([]string, error) {
	q := &apiclient.GetAPIV1SessionIdsResolveQuery{}
	q.Partial = partial
	if limit > 0 {
		q.Limit = new(int64(limit))
	}
	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	response, err := api.GetAPIV1SessionIdsResolveWithResponse(ctx, &apiclient.GetAPIV1SessionIdsResolveRequestOptions{Query: q})
	if response == nil {
		return nil, err
	}
	err = serviceResponseError(response.HTTPResponse, response.Body, err)
	if err != nil {
		return nil, err
	}
	return response.JSON200.Ids, nil
}

func (b *httpBackend) FindSessionIDsByRawSuffix(
	ctx context.Context, raw string, limit int,
) ([]string, error) {
	q := &apiclient.GetAPIV1SessionIdsResolveQuery{Partial: raw, RawSuffix: new(true)}
	if limit > 0 {
		q.Limit = new(int64(limit))
	}
	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	response, err := api.GetAPIV1SessionIdsResolveWithResponse(ctx, &apiclient.GetAPIV1SessionIdsResolveRequestOptions{Query: q})
	if response == nil {
		return nil, err
	}
	if err := serviceResponseError(response.HTTPResponse, response.Body, err); err != nil {
		return nil, err
	}
	if response.JSON200.RawSuffix == nil || !*response.JSON200.RawSuffix {
		return nil, errors.New(
			"server does not acknowledge raw session ID lookup",
		)
	}
	return response.JSON200.Ids, nil
}

func (b *httpBackend) List(
	ctx context.Context, f service.ListFilter,
) (*service.SessionList, error) {
	q, err := filterToQuery(f)
	if err != nil {
		return nil, err
	}
	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	response, err := api.GetAPIV1SessionsWithResponse(ctx, &apiclient.GetAPIV1SessionsRequestOptions{Query: q})
	if response == nil {
		return nil, err
	}
	out := response.JSON200
	err = serviceResponseError(response.HTTPResponse, response.Body, err)
	if err != nil {
		return nil, err
	}
	for i := range out.Sessions {
		out.Sessions[i].WebURL = b.sessionWebURL(out.Sessions[i].ID)
	}
	return out, nil
}

// filterToQuery converts a ListFilter into the URL query params
// expected by handleListSessions. Field mapping mirrors the
// server-side parser in internal/server/sessions.go.
func filterToQuery(f service.ListFilter) (*apiclient.GetAPIV1SessionsQuery, error) {
	q := &apiclient.GetAPIV1SessionsQuery{}
	if f.Project != "" {
		q.Project = new(f.Project)
	}
	if f.ExcludeProject != "" {
		q.ExcludeProject = new(f.ExcludeProject)
	}
	if f.Machine != "" {
		q.Machine = new(f.Machine)
	}
	if f.GitBranch != "" {
		q.GitBranch = new(f.GitBranch)
	}
	if f.Agent != "" {
		q.Agent = new(f.Agent)
	}
	if f.Date != "" {
		parsedDate, err := time.Parse(time.DateOnly, f.Date)
		if err != nil {
			return nil, err
		}
		q.Date = &runtime.Date{Time: parsedDate}
	}
	if f.DateFrom != "" {
		parsedDateFrom, err := time.Parse(time.DateOnly, f.DateFrom)
		if err != nil {
			return nil, err
		}
		q.DateFrom = &runtime.Date{Time: parsedDateFrom}
	}
	if f.DateTo != "" {
		parsedDateTo, err := time.Parse(time.DateOnly, f.DateTo)
		if err != nil {
			return nil, err
		}
		q.DateTo = &runtime.Date{Time: parsedDateTo}
	}
	if f.Timezone != "" {
		q.Timezone = new(f.Timezone)
	}
	if f.ActiveSince != "" {
		parsedActiveSince, err := time.Parse(time.RFC3339, f.ActiveSince)
		if err != nil {
			return nil, err
		}
		q.ActiveSince = &parsedActiveSince
	}
	if f.MinMessages > 0 {
		q.MinMessages = new(int64(f.MinMessages))
	}
	if f.MaxMessages > 0 {
		q.MaxMessages = new(int64(f.MaxMessages))
	}
	if f.MinUserMessages > 0 {
		q.MinUserMessages = new(int64(f.MinUserMessages))
	}
	if f.IncludeOneShot {
		q.IncludeOneShot = new(true)
	}
	if f.IncludeAutomated {
		q.IncludeAutomated = new(true)
	}
	if f.IncludeChildren {
		q.IncludeChildren = new(true)
	}
	if f.IncludeSource {
		q.IncludeSource = new(true)
	}
	if f.Outcome != "" {
		q.Outcome = new(f.Outcome)
	}
	if f.HealthGrade != "" {
		q.HealthGrade = new(f.HealthGrade)
	}
	if f.Termination != "" {
		q.Termination = new(f.Termination)
	}
	if f.MinToolFailures != nil {
		q.MinToolFailures = new(int64(*f.MinToolFailures))
	}
	if f.HasSecret {
		q.HasSecret = new(true)
	}
	if f.Starred {
		q.Starred = new(true)
	}
	if f.Cursor != "" {
		q.Cursor = new(f.Cursor)
	}
	if f.Limit > 0 {
		q.Limit = new(int64(f.Limit))
	}
	if f.OrderBy != "" {
		q.OrderBy = new(f.OrderBy)
	}
	if f.Descending != nil {
		q.Descending = new(*f.Descending)
	}
	return q, nil
}

func (b *httpBackend) Messages(
	ctx context.Context, id string, f service.MessageFilter,
) (*service.MessageList, error) {
	q := &apiclient.GetAPIV1SessionsIDMessagesQuery{}
	if f.From != nil {
		q.From = new(int64(*f.From))
	}
	if f.Limit > 0 {
		q.Limit = new(int64(f.Limit))
	}
	if f.Direction != "" {
		q.Direction = new(apiclient.GetAPIV1SessionsIDMessagesQueryDirection(f.Direction))
	}
	if f.Around != nil {
		q.Around = new(int64(*f.Around))
	}
	if f.Before != nil {
		q.Before = new(int64(*f.Before))
	}
	if f.After != nil {
		q.After = new(int64(*f.After))
	}
	if len(f.Roles) > 0 {
		q.Roles = new(strings.Join(f.Roles, ","))
	}
	if f.ExpectedRevision != "" {
		q.ExpectedRevision = new(f.ExpectedRevision)
	}
	if f.EvidenceSource != "" {
		q.EvidenceSource = new(f.EvidenceSource)
	}
	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	response, err := api.GetAPIV1SessionsIDMessagesWithResponse(ctx, &apiclient.GetAPIV1SessionsIDMessagesRequestOptions{Query: q, PathParams: &apiclient.GetAPIV1SessionsIDMessagesPath{ID: url.PathEscape(id)}})
	if response == nil {
		return nil, err
	}
	out := response.JSON200
	err = serviceResponseError(response.HTTPResponse, response.Body, err)
	if err != nil {
		if response.HTTPResponse != nil && response.StatusCode == http.StatusConflict &&
			strings.Contains(string(response.Body), "source_changed") {
			return nil, service.ErrSourceChanged
		}
		if response.HTTPResponse != nil && response.StatusCode == http.StatusNotImplemented {
			return nil, service.ErrRevisionBoundReadUnavailable
		}
		return nil, err
	}
	return out, nil
}

func (b *httpBackend) ToolCalls(
	ctx context.Context, id string,
) (*service.ToolCallList, error) {
	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	response, err := api.GetAPIV1SessionsIDToolCallsWithResponse(ctx, &apiclient.GetAPIV1SessionsIDToolCallsRequestOptions{PathParams: &apiclient.GetAPIV1SessionsIDToolCallsPath{ID: url.PathEscape(id)}})
	if response == nil {
		return nil, err
	}
	out := response.JSON200
	err = serviceResponseError(response.HTTPResponse, response.Body, err)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (b *httpBackend) Sync(
	ctx context.Context, in service.SyncInput,
) (*service.SessionDetail, error) {
	if b.readOnly {
		// Return the shared sentinel so callers can
		// errors.Is(err, db.ErrReadOnly) regardless of
		// transport.
		return nil, fmt.Errorf(
			"sync: daemon at %s is read-only: %w",
			b.baseURL, db.ErrReadOnly,
		)
	}
	api, err := b.apiClient(b.longRunningClient)
	if err != nil {
		return nil, err
	}
	response, err := api.PostAPIV1SessionsSyncWithResponse(ctx, &apiclient.PostAPIV1SessionsSyncRequestOptions{Body: &apiclient.PostAPIV1SessionsSyncBody{ID: new(in.ID), Path: new(in.Path), Subagents: new(in.Subagents)}})
	if response == nil {
		return nil, err
	}
	detail := response.JSON200
	if response.StatusCode == http.StatusNotImplemented {
		return nil, fmt.Errorf("sync: daemon at %s: %w", b.baseURL, db.ErrReadOnly)
	}
	if err := serviceResponseError(response.HTTPResponse, response.Body, err); err != nil {
		return nil, err
	}

	detail.WebURL = b.sessionWebURL(detail.ID)
	return detail, nil
}

func (b *httpBackend) Watch(
	ctx context.Context, id string,
) (<-chan service.Event, error) {
	client, err := b.apiClient(b.longRunningClient)
	if err != nil {
		return nil, err
	}
	response, err := client.GetAPIV1SessionsIDWatchStreamWithResponse(ctx,
		&apiclient.GetAPIV1SessionsIDWatchRequestOptions{
			PathParams: &apiclient.GetAPIV1SessionsIDWatchPath{ID: url.PathEscape(id)},
		})
	if err != nil {
		if apiErr, ok := errors.AsType[*runtime.ClientAPIError](err); ok {
			if apiErr.StatusCode() == http.StatusNotFound {
				return nil, fmt.Errorf("watch: session not found: %s", id)
			}
			return nil, fmt.Errorf("watch: HTTP %d", apiErr.StatusCode())
		}
		return nil, err
	}

	stream := response.Stream200
	out := make(chan service.Event)
	go func() {
		defer close(out)
		defer stream.Close()
		// Closing out signals a dropped watch stream to the consumer.
		for stream.Next() {
			frame := stream.Event()
			select {
			case out <- service.Event{Event: frame.Type, Data: string(frame.Data)}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (b *httpBackend) Stats(
	ctx context.Context, f service.StatsFilter,
) (*service.SessionStats, error) {
	q := &apiclient.GetAPIV1SessionStatsQuery{}
	if f.Since != "" {
		q.Since = new(f.Since)
	}
	if f.Until != "" {
		q.Until = new(f.Until)
	}
	if f.Agent != "" {
		q.Agent = new(f.Agent)
	}
	if f.Timezone != "" {
		q.Timezone = new(f.Timezone)
	}
	includeOneShot := f.IncludeOneShot
	includeAutomated := f.IncludeAutomated
	if !f.ApplyDefaultVisibility {
		includeOneShot = true
		includeAutomated = true
	}
	q.IncludeOneShot = new(includeOneShot)
	q.IncludeAutomated = new(includeAutomated)
	q.IncludeProject = append(q.IncludeProject, f.IncludeProjects...)
	q.ExcludeProject = append(q.ExcludeProject, f.ExcludeProjects...)
	q.IncludeGitOutcomes = new(f.IncludeGitOutcomes)
	q.IncludeGithubOutcomes = new(f.IncludeGitHubOutcomes)

	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	response, err := api.GetAPIV1SessionStatsWithResponse(ctx, &apiclient.GetAPIV1SessionStatsRequestOptions{Query: q})
	if response == nil {
		return nil, err
	}
	out := response.JSON200
	err = serviceResponseError(response.HTTPResponse, response.Body, err)
	if errors.Is(err, errHTTPNotImplemented) {
		return nil, fmt.Errorf(
			"stats: daemon at %s: %w", b.baseURL, db.ErrReadOnly,
		)
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (b *httpBackend) Search(
	ctx context.Context, req service.SearchRequest,
) (*service.SessionSearchResult, error) {
	q := &apiclient.GetAPIV1SearchQuery{}
	q.Q = req.Query
	if req.Project != "" {
		q.Project = new(req.Project)
	}
	if req.DateFrom != "" {
		parsedDateFrom, err := time.Parse(time.DateOnly, req.DateFrom)
		if err != nil {
			return nil, err
		}
		q.DateFrom = &runtime.Date{Time: parsedDateFrom}
	}
	if req.DateTo != "" {
		parsedDateTo, err := time.Parse(time.DateOnly, req.DateTo)
		if err != nil {
			return nil, err
		}
		q.DateTo = &runtime.Date{Time: parsedDateTo}
	}
	if req.Sort != "" {
		q.Sort = new(apiclient.GetAPIV1SearchQuerySort(req.Sort))
	}
	if req.Cursor > 0 {
		q.Cursor = new(int64(req.Cursor))
	}
	if req.Limit > 0 {
		q.Limit = new(int64(req.Limit))
	}
	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	response, err := api.GetAPIV1SearchWithResponse(ctx, &apiclient.GetAPIV1SearchRequestOptions{Query: q})
	if response == nil {
		return nil, err
	}
	err = serviceResponseError(response.HTTPResponse, response.Body, err)
	if err != nil {
		if errors.Is(err, errHTTPNotImplemented) {
			return nil, service.ErrSearchUnavailable
		}
		return nil, err
	}
	out := response.JSON200
	for i := range out.Results {
		out.Results[i].WebURL = b.sessionWebURL(out.Results[i].SessionID)
	}
	results := out.Results
	if results == nil {
		results = []db.SearchResult{}
	}
	return &service.SessionSearchResult{Results: results, NextCursor: int(out.Next)}, nil
}

func (b *httpBackend) SearchContent(
	ctx context.Context, req service.ContentSearchRequest,
) (*service.ContentSearchResult, error) {
	q := &apiclient.GetAPIV1SearchContentQuery{}
	q.Pattern = req.Pattern
	if req.Mode != "" {
		q.Mode = new(apiclient.GetAPIV1SearchContentQueryMode(req.Mode))
	}
	if len(req.Sources) > 0 {
		q.In = new(strings.Join(req.Sources, ","))
	}
	if req.ExcludeSystem {
		q.ExcludeSystem = new(true)
	}
	if req.Reveal {
		q.Reveal = new(true)
	}
	if req.Project != "" {
		q.Project = new(req.Project)
	}
	if req.ExcludeProject != "" {
		q.ExcludeProject = new(req.ExcludeProject)
	}
	if req.Machine != "" {
		q.Machine = new(req.Machine)
	}
	if req.GitBranch != "" {
		q.GitBranch = new(req.GitBranch)
	}
	if req.SessionID != "" {
		q.SessionID = new(req.SessionID)
	}
	if req.GitBranchExact != "" {
		q.GitBranchExact = new(req.GitBranchExact)
	}
	if req.Agent != "" {
		q.Agent = new(req.Agent)
	}
	if req.Date != "" {
		parsedDate, err := time.Parse(time.DateOnly, req.Date)
		if err != nil {
			return nil, err
		}
		q.Date = &runtime.Date{Time: parsedDate}
	}
	if req.DateFrom != "" {
		parsedDateFrom, err := time.Parse(time.DateOnly, req.DateFrom)
		if err != nil {
			return nil, err
		}
		q.DateFrom = &runtime.Date{Time: parsedDateFrom}
	}
	if req.DateTo != "" {
		parsedDateTo, err := time.Parse(time.DateOnly, req.DateTo)
		if err != nil {
			return nil, err
		}
		q.DateTo = &runtime.Date{Time: parsedDateTo}
	}
	if req.Timezone != "" {
		q.Timezone = new(req.Timezone)
	}
	if req.ActiveSince != "" {
		parsedActiveSince, err := time.Parse(time.RFC3339, req.ActiveSince)
		if err != nil {
			return nil, err
		}
		q.ActiveSince = &parsedActiveSince
	}
	if req.Scope != "" {
		q.Scope = new(apiclient.GetAPIV1SearchContentQueryScope(req.Scope))
	}
	if req.IncludeChildren {
		q.IncludeChildren = new(true)
	}
	if req.IncludeAutomated {
		q.IncludeAutomated = new(true)
	}
	if req.IncludeOneShot {
		q.IncludeOneShot = new(true)
	}
	for _, id := range req.ExcludeSessionIDs {
		if id = strings.TrimSpace(id); id != "" {
			q.ExcludeSession = append(q.ExcludeSession, id)
		}
	}
	if req.Limit > 0 {
		q.Limit = new(int64(req.Limit))
	}
	if req.Cursor > 0 {
		q.Cursor = new(int64(req.Cursor))
	}
	if req.Context > 0 {
		q.Context = new(int64(req.Context))
	}
	var editors []runtime.RequestEditorFn
	if req.Mode == "semantic" || req.Mode == "hybrid" {
		editors = append(editors, func(_ context.Context, r *http.Request) error {
			r.Header.Set(service.SemanticSearchIntentHeader, service.SemanticSearchIntentValue)
			return nil
		})
	}

	api, err := b.apiClient(b.longRunningClient)
	if err != nil {
		return nil, err
	}
	response, err := api.GetAPIV1SearchContentWithResponse(ctx, &apiclient.GetAPIV1SearchContentRequestOptions{Query: q}, editors...)
	if response == nil {
		return nil, err
	}
	out := response.JSON200
	err = serviceResponseError(response.HTTPResponse, response.Body, err)
	if err != nil {
		if notImpl, ok := errors.AsType[*notImplementedBodyError](err); ok {
			return nil, wrapSemanticUnavailable(notImpl.message)
		}
		return nil, err
	}
	for i := range out.Matches {
		out.Matches[i].WebURL = b.sessionWebURL(out.Matches[i].SessionID)
	}
	return out, nil
}

// wrapSemanticUnavailable turns a search/content 501 response's error
// message into an error wrapping ErrSemanticUnavailable, preserving
// whatever cause-specific remediation text the server attached (e.g. "index
// is building: N% complete" or "... run 'agentsview embeddings build
// --full-rebuild'") instead of discarding it for the bare sentinel.
// errors.Is(result, ErrSemanticUnavailable) always holds. When message is
// empty or is exactly the sentinel's own text (no extra cause), the bare
// sentinel is returned rather than duplicating it.
func wrapSemanticUnavailable(message string) error {
	sentinel := service.ErrSemanticUnavailable.Error()
	if message == "" || message == sentinel {
		return service.ErrSemanticUnavailable
	}
	if cause, ok := strings.CutPrefix(message, sentinel); ok {
		return fmt.Errorf("%w%s", service.ErrSemanticUnavailable, cause)
	}
	if reason, ok := strings.CutPrefix(
		message, "semantic search not available: ",
	); ok {
		return db.NewSemanticUnavailableError(reason)
	}
	// An unexpected body shape (e.g. a differently worded 501) is still a
	// reasoned semantic-unavailable error: errors.Is holds without injecting
	// the sentinel's local-only setup guidance into the server's text.
	return db.NewSemanticUnavailableError(message)
}

func wrapSemanticTransient(message string) error {
	sentinel := db.ErrSemanticTransient.Error()
	if message == "" || message == sentinel {
		return db.ErrSemanticTransient
	}
	if cause, ok := strings.CutPrefix(message, sentinel); ok {
		return fmt.Errorf("%w%s", db.ErrSemanticTransient, cause)
	}
	return fmt.Errorf("%w: %s", db.ErrSemanticTransient, message)
}

func (b *httpBackend) UsageSummary(
	ctx context.Context, req service.UsageRequest,
) (*service.UsageSummaryResult, error) {
	q := &apiclient.GetAPIV1UsageSummaryQuery{}
	if req.From != "" {
		parsedFrom, err := time.Parse(time.DateOnly, req.From)
		if err != nil {
			return nil, err
		}
		q.From = &runtime.Date{Time: parsedFrom}
	}
	if req.To != "" {
		parsedTo, err := time.Parse(time.DateOnly, req.To)
		if err != nil {
			return nil, err
		}
		q.To = &runtime.Date{Time: parsedTo}
	}
	if req.Timezone != "" {
		q.Timezone = new(req.Timezone)
	}
	if req.Agent != "" {
		q.Agent = new(req.Agent)
	}
	if req.Project != "" {
		q.Project = new(req.Project)
	}
	if req.Machine != "" {
		q.Machine = new(req.Machine)
	}
	if req.GitBranch != "" {
		q.GitBranch = new(req.GitBranch)
	}
	if req.ExcludeProject != "" {
		q.ExcludeProject = new(req.ExcludeProject)
	}
	if req.ExcludeProjectKey != "" {
		q.ExcludeProjectKey = new(req.ExcludeProjectKey)
	}
	if req.ExcludeAgent != "" {
		q.ExcludeAgent = new(req.ExcludeAgent)
	}
	if req.ExcludeModel != "" {
		q.ExcludeModel = new(req.ExcludeModel)
	}
	if req.Model != "" {
		q.Model = new(req.Model)
	}
	if req.ActiveSince != "" {
		parsedActiveSince, err := time.Parse(time.RFC3339, req.ActiveSince)
		if err != nil {
			return nil, err
		}
		q.ActiveSince = &parsedActiveSince
	}
	if req.Termination != "" {
		q.Termination = new(req.Termination)
	}
	if req.MinUserMessages > 0 {
		q.MinUserMessages = new(int64(req.MinUserMessages))
	}
	if req.NoDefaultRange {
		q.NoDefaultRange = new(true)
	}
	if req.Breakdowns != nil {
		q.Breakdowns = new(*req.Breakdowns)
	}
	if req.SessionCounts != nil {
		q.SessionCounts = new(*req.SessionCounts)
	}
	// include_one_shot defaults to true on the server, so it must be sent
	// explicitly to transmit a false value; include_automated defaults to
	// false. Send both explicitly so the round-trip matches the direct
	// backend regardless of the daemon's defaults.
	q.IncludeOneShot = new(req.IncludeOneShot)
	q.IncludeAutomated = new(req.IncludeAutomated)

	api, err := b.apiClient(b.longRunningClient)
	if err != nil {
		return nil, err
	}
	response, err := api.GetAPIV1UsageSummaryWithResponse(ctx, &apiclient.GetAPIV1UsageSummaryRequestOptions{Query: q})
	if response == nil {
		return nil, err
	}
	err = serviceResponseError(response.HTTPResponse, response.Body, err)
	if errors.Is(err, errHTTPNotImplemented) {
		// A read-only daemon (pg serve) returns 501 for usage; surface
		// the shared sentinel so callers can errors.Is it.
		return nil, fmt.Errorf(
			"usage summary: daemon at %s: %w", b.baseURL, db.ErrReadOnly,
		)
	}
	if err != nil {
		return nil, err
	}
	wire := response.JSON200
	out := &service.UsageSummaryResult{
		Pricing: wire.Pricing, Projects: wire.Projects, From: wire.From, To: wire.To,
		Totals: wire.Totals, Daily: wire.Daily, ProjectTotals: wire.ProjectTotals,
		ModelTotals: wire.ModelTotals, AgentTotals: wire.AgentTotals,
		SessionCounts: wire.SessionCounts, CacheStats: wire.CacheStats,
		UnsupportedUsage: wire.UnsupportedUsage,
	}
	if wire.SchemaVersion != nil {
		out.SchemaVersion = int(*wire.SchemaVersion)
	}
	return out, nil
}

func (b *httpBackend) UsagePairwiseComparison(
	ctx context.Context, req service.UsagePairwiseComparisonRequest,
) (*service.UsagePairwiseComparisonResponse, error) {
	q := &apiclient.GetAPIV1UsagePairwiseComparisonQuery{}
	if req.From != "" {
		parsedFrom, err := time.Parse(time.DateOnly, req.From)
		if err != nil {
			return nil, err
		}
		q.From = &runtime.Date{Time: parsedFrom}
	}
	if req.To != "" {
		parsedTo, err := time.Parse(time.DateOnly, req.To)
		if err != nil {
			return nil, err
		}
		q.To = &runtime.Date{Time: parsedTo}
	}
	if req.Timezone != "" {
		q.Timezone = new(req.Timezone)
	}
	if req.Agent != "" {
		q.Agent = new(req.Agent)
	}
	if req.Project != "" {
		q.Project = new(req.Project)
	}
	if req.Machine != "" {
		q.Machine = new(req.Machine)
	}
	if req.GitBranch != "" {
		q.GitBranch = new(req.GitBranch)
	}
	if req.ExcludeProject != "" {
		q.ExcludeProject = new(req.ExcludeProject)
	}
	if req.ExcludeProjectKey != "" {
		q.ExcludeProjectKey = new(req.ExcludeProjectKey)
	}
	if req.ExcludeAgent != "" {
		q.ExcludeAgent = new(req.ExcludeAgent)
	}
	if req.ExcludeModel != "" {
		q.ExcludeModel = new(req.ExcludeModel)
	}
	if req.ActiveSince != "" {
		parsedActiveSince, err := time.Parse(time.RFC3339, req.ActiveSince)
		if err != nil {
			return nil, err
		}
		q.ActiveSince = &parsedActiveSince
	}
	if req.Termination != "" {
		q.Termination = new(req.Termination)
	}
	if req.LeftDimension != "" {
		q.LeftDimension = req.LeftDimension
	}
	if req.LeftValue != "" {
		q.LeftValue = req.LeftValue
	}
	if req.RightDimension != "" {
		q.RightDimension = req.RightDimension
	}
	if req.RightValue != "" {
		q.RightValue = req.RightValue
	}
	if req.MinUserMessages > 0 {
		q.MinUserMessages = new(int64(req.MinUserMessages))
	}
	if req.NoDefaultRange {
		q.NoDefaultRange = new(true)
	}
	// Include explicit booleans to preserve source defaults.
	q.IncludeOneShot = new(req.IncludeOneShot)
	q.IncludeAutomated = new(req.IncludeAutomated)
	if req.Model != "" {
		q.Model = new(req.Model)
	}

	api, err := b.apiClient(b.longRunningClient)
	if err != nil {
		return nil, err
	}
	response, err := api.GetAPIV1UsagePairwiseComparisonWithResponse(ctx, &apiclient.GetAPIV1UsagePairwiseComparisonRequestOptions{Query: q})
	if response == nil {
		return nil, err
	}
	out := response.JSON200
	err = serviceResponseError(response.HTTPResponse, response.Body, err)
	if errors.Is(err, errHTTPNotImplemented) {
		return nil, fmt.Errorf(
			"usage pairwise comparison: daemon at %s: %w", b.baseURL, db.ErrReadOnly,
		)
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (b *httpBackend) ListRecallEntries(
	ctx context.Context, f service.RecallFilter,
) (*service.RecallList, error) {
	if err := service.ValidateRecallEntryLimit(f.Limit); err != nil {
		return nil, err
	}
	q := recallFilterToQuery(f)
	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	response, err := api.GetAPIV1RecallEntriesWithResponse(ctx, &apiclient.GetAPIV1RecallEntriesRequestOptions{Query: q})
	if response == nil {
		return nil, err
	}
	err = serviceResponseError(response.HTTPResponse, response.Body, err)
	if err != nil {
		if errors.Is(err, errHTTPNotImplemented) {
			return nil, fmt.Errorf(
				"recall list: daemon at %s: %w", b.baseURL, db.ErrReadOnly,
			)
		}
		return nil, err
	}
	out := service.RecallList{RecallEntries: response.JSON200.Entries, TrustedOnly: response.JSON200.TrustedOnly}
	if out.RecallEntries == nil {
		out.RecallEntries = []db.RecallResult{}
	}
	if f.TrustedOnly {
		out.TrustedOnly = true
	}
	return &out, nil
}

func (b *httpBackend) GetRecallEntry(
	ctx context.Context, id string,
) (*db.RecallEntry, error) {
	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	response, err := api.GetAPIV1RecallEntriesIDWithResponse(ctx, &apiclient.GetAPIV1RecallEntriesIDRequestOptions{PathParams: &apiclient.GetAPIV1RecallEntriesIDPath{ID: url.PathEscape(id)}})
	if response == nil {
		return nil, err
	}
	out := response.JSON200
	err = serviceResponseError(response.HTTPResponse, response.Body, err)
	if errors.Is(err, errHTTPNotFound) {
		return nil, nil
	}
	if errors.Is(err, errHTTPNotImplemented) {
		return nil, fmt.Errorf(
			"recall get: daemon at %s: %w", b.baseURL, db.ErrReadOnly,
		)
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (b *httpBackend) QueryRecallEntries(
	ctx context.Context, req service.RecallQuery,
) (*service.RecallQueryResult, error) {
	if err := service.ValidateRecallEntryLimit(req.Limit); err != nil {
		return nil, err
	}
	if req.IncludeContext {
		if _, err := service.NormalizeRecallContextMaxBytes(req.ContextMaxBytes); err != nil {
			return nil, err
		}
	}
	if _, err := service.NormalizeRecallQuerySurface(req.Surface); err != nil {
		return nil, err
	}
	if req.StrictRecording {
		return nil, errors.New("strict recall recording requires a direct backend")
	}
	mode := db.NormalizeRecallQuery(db.RecallQuery{Mode: req.Mode}).Mode
	httpClient := b.client
	if mode == db.RecallQueryModeVector || mode == db.RecallQueryModeHybrid {
		httpClient = b.longRunningClient
	}
	api, err := b.apiClient(httpClient)
	if err != nil {
		return nil, err
	}
	response, err := api.PostAPIV1RecallQueryWithResponse(ctx, &apiclient.PostAPIV1RecallQueryRequestOptions{Body: &apiclient.PostAPIV1RecallQueryBody{Agent: new(req.Agent), ContextMaxBytes: new(int64(req.ContextMaxBytes)), Cwd: new(req.CWD), ExtractorMethod: new(req.ExtractorMethod), GitBranch: new(req.GitBranch), IncludeContext: new(req.IncludeContext), Limit: new(int64(req.Limit)), Mode: new(req.Mode), Project: new(req.Project), Query: req.Query, Scope: new(req.Scope), SkipRecording: new(req.SkipRecording), SourceEpisodeID: new(req.SourceEpisodeID), SourceRunID: new(req.SourceRunID), SourceSessionID: new(req.SourceSessionID), Status: new(req.Status), SupersededByEntryID: new(req.SupersededByEntryID), SupersedesEntryID: new(req.SupersedesEntryID), Surface: new(req.Surface), TrustedOnly: new(req.TrustedOnly), Type: new(req.Type)}})
	if response == nil {
		return nil, err
	}
	out := response.JSON200
	if err := serviceResponseError(response.HTTPResponse, response.Body, err); err != nil {
		if errors.Is(err, errHTTPNotImplemented) {
			notImpl, hasNotImpl := errors.AsType[*notImplementedBodyError](err)
			if (mode == db.RecallQueryModeVector ||
				mode == db.RecallQueryModeHybrid) &&
				hasNotImpl &&
				notImpl.message != "not available in remote mode" {
				return nil, wrapSemanticUnavailable(notImpl.message)
			}
			return nil, fmt.Errorf(
				"recall query: daemon at %s: %w", b.baseURL, db.ErrReadOnly,
			)
		}
		statusErr, hasStatusErr := errors.AsType[*httpStatusError](err)
		if (mode == db.RecallQueryModeVector ||
			mode == db.RecallQueryModeHybrid) &&
			hasStatusErr &&
			statusErr.statusCode == http.StatusServiceUnavailable {
			return nil, wrapSemanticTransient(statusErr.message())
		}
		return nil, err
	}
	if out.RecallEntries == nil {
		out.RecallEntries = []db.RecallResult{}
	}
	requestedMode := mode
	returnedMode := db.NormalizeRecallQuery(db.RecallQuery{Mode: out.Mode}).Mode
	if returnedMode != requestedMode {
		return nil, fmt.Errorf(
			"recall query: requested recall mode %s but daemon returned %s",
			requestedMode, returnedMode,
		)
	}
	out.Mode = returnedMode
	if req.TrustedOnly {
		out.TrustedOnly = true
	}
	if out.Summary == nil {
		out.Summary = service.BuildRecallQuerySummary(out.RecallEntries)
	}
	if out.ContextEntries == nil && out.ContextMeta != nil {
		out.ContextEntries = service.RecallContextResults(
			out.RecallEntries, out.ContextMeta,
		)
	}
	if err := service.ValidateRecallContextEntries(
		out.ContextEntries, out.ContextMeta,
	); err != nil {
		return nil, err
	}
	if out.ContextSummary == nil && out.ContextMeta != nil {
		out.ContextSummary = service.BuildRecallContextSummary(
			out.RecallEntries, out.ContextMeta,
		)
	}
	return out, nil
}

func (b *httpBackend) ImportRecallEntries(
	ctx context.Context, r io.Reader, opts db.RecallImportOptions,
) (*db.RecallImportResult, error) {
	if b.readOnly {
		// Surface the shared sentinel so callers can errors.Is it,
		// matching Sync/ScanSecrets instead of posting to a read-only
		// daemon and returning a bare endpoint error.
		return nil, fmt.Errorf(
			"import: daemon at %s is read-only: %w",
			b.baseURL, db.ErrReadOnly,
		)
	}
	q := &apiclient.PostAPIV1RecallImportQuery{}
	if opts.DryRun {
		q.DryRun = new(true)
	}
	if opts.RequireExistingSessions {
		q.RequireExistingSessions = new(true)
	} else {
		q.AllowPlaceholderSessions = new(true)
	}
	if opts.AllowProductionImport {
		q.AllowProductionImport = new(true)
	}
	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	response, err := api.PostAPIV1RecallImportWithResponse(ctx, &apiclient.PostAPIV1RecallImportRequestOptions{Query: q}, func(_ context.Context, request *http.Request) error {
		// Keep imports streaming instead of buffering the JSONL input.
		request.Body = io.NopCloser(r)
		request.ContentLength = -1
		return nil
	})
	if response == nil {
		return nil, err
	}
	out := response.JSON200
	if response.StatusCode == http.StatusNotImplemented {
		return nil, fmt.Errorf("daemon at %s is read-only: %w", b.baseURL, db.ErrReadOnly)
	}
	if err := serviceResponseError(response.HTTPResponse, response.Body, err); err != nil {
		return nil, err
	}

	return out, nil
}

func recallFilterToQuery(f service.RecallFilter) *apiclient.GetAPIV1RecallEntriesQuery {
	q := &apiclient.GetAPIV1RecallEntriesQuery{}
	if f.Query != "" {
		q.Q = new(f.Query)
	}
	if f.Project != "" {
		q.Project = new(f.Project)
	}
	if f.CWD != "" {
		q.Cwd = new(f.CWD)
	}
	if f.GitBranch != "" {
		q.GitBranch = new(f.GitBranch)
	}
	if f.Agent != "" {
		q.Agent = new(f.Agent)
	}
	if f.Type != "" {
		q.Type = new(f.Type)
	}
	if f.Scope != "" {
		q.Scope = new(f.Scope)
	}
	if f.Status != "" {
		q.Status = new(f.Status)
	}
	if f.ExtractorMethod != "" {
		q.ExtractorMethod = new(f.ExtractorMethod)
	}
	if f.SourceSessionID != "" {
		q.SourceSessionID = new(f.SourceSessionID)
	}
	if f.SourceEpisodeID != "" {
		q.SourceEpisodeID = new(f.SourceEpisodeID)
	}
	if f.SourceRunID != "" {
		q.SourceRunID = new(f.SourceRunID)
	}
	if f.SupersedesEntryID != "" {
		q.SupersedesEntryID = new(f.SupersedesEntryID)
	}
	if f.SupersededByEntryID != "" {
		q.SupersededByEntryID = new(f.SupersededByEntryID)
	}
	if f.Limit > 0 {
		q.Limit = new(f.Limit)
	}
	if f.TrustedOnly {
		q.TrustedOnly = new(true)
	}
	return q
}

func (b *httpBackend) ListSecrets(
	ctx context.Context, f service.SecretListFilter,
) (*service.SecretFindingList, error) {
	q := &apiclient.GetAPIV1SecretsQuery{}
	if f.Project != "" {
		q.Project = new(f.Project)
	}
	if f.Agent != "" {
		q.Agent = new(f.Agent)
	}
	if f.DateFrom != "" {
		parsedDateFrom, err := time.Parse(time.DateOnly, f.DateFrom)
		if err != nil {
			return nil, err
		}
		q.DateFrom = &runtime.Date{Time: parsedDateFrom}
	}
	if f.DateTo != "" {
		parsedDateTo, err := time.Parse(time.DateOnly, f.DateTo)
		if err != nil {
			return nil, err
		}
		q.DateTo = &runtime.Date{Time: parsedDateTo}
	}
	if f.Rule != "" {
		q.Rule = new(f.Rule)
	}
	if f.Confidence != "" {
		q.Confidence = new(f.Confidence)
	}
	if f.Reveal {
		q.Reveal = new(true)
	}
	if f.Limit > 0 {
		q.Limit = new(int64(f.Limit))
	}
	if f.Cursor > 0 {
		q.Cursor = new(int64(f.Cursor))
	}
	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	response, err := api.GetAPIV1SecretsWithResponse(ctx, &apiclient.GetAPIV1SecretsRequestOptions{Query: q})
	if response == nil {
		return nil, err
	}
	out := response.JSON200
	err = serviceResponseError(response.HTTPResponse, response.Body, err)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (b *httpBackend) ScanSecrets(
	ctx context.Context, in service.SecretScanInput,
	progress func(service.SecretScanProgress),
) (*service.SecretScanSummary, error) {
	if b.readOnly {
		return nil, fmt.Errorf("scan: daemon at %s is read-only: %w",
			b.baseURL, db.ErrReadOnly)
	}
	q := &apiclient.PostAPIV1SecretsScanQuery{}
	if in.Backfill {
		q.Backfill = new(true)
	}
	if in.Project != "" {
		q.Project = new(in.Project)
	}
	if in.Agent != "" {
		q.Agent = new(in.Agent)
	}
	if in.DateFrom != "" {
		parsedDateFrom, err := time.Parse(time.DateOnly, in.DateFrom)
		if err != nil {
			return nil, err
		}
		q.DateFrom = &runtime.Date{Time: parsedDateFrom}
	}
	if in.DateTo != "" {
		parsedDateTo, err := time.Parse(time.DateOnly, in.DateTo)
		if err != nil {
			return nil, err
		}
		q.DateTo = &runtime.Date{Time: parsedDateTo}
	}
	api, err := b.apiClient(b.longRunningClient)
	if err != nil {
		return nil, err
	}
	response, err := api.PostAPIV1SecretsScanStreamWithResponse(ctx, &apiclient.PostAPIV1SecretsScanRequestOptions{Query: q})
	if response != nil && response.StatusCode == http.StatusNotImplemented {
		return nil, fmt.Errorf("scan: daemon at %s: %w", b.baseURL, db.ErrReadOnly)
	}
	if err != nil {
		return nil, err
	}
	defer response.Stream200.Close()
	return parseScanStream(response.Stream200, progress)
}

// parseScanStream decodes the scan SSE stream: progress ticks invoke the
// callback, the summary event is the result, and an error event becomes an
// error. A stream that ends without a summary event (broken connection,
// canceled context, daemon crash) is reported as an error rather than a
// zero-value success.
func parseScanStream(
	stream *runtime.Stream[[]byte], progress func(service.SecretScanProgress),
) (*service.SecretScanSummary, error) {
	var summary service.SecretScanSummary
	var scanErr, decodeErr error
	var gotSummary bool
	for stream.Next() {
		ev := stream.Event()
		switch ev.Type {
		case "progress":
			var p service.SecretScanProgress
			if json.Unmarshal(ev.Data, &p) == nil && progress != nil {
				progress(p)
			}
		case "summary":
			if err := json.Unmarshal(ev.Data, &summary); err != nil {
				decodeErr = fmt.Errorf("scan: decoding summary: %w", err)
			} else {
				gotSummary = true
			}
		case "error":
			scanErr = fmt.Errorf("scan: %s", ev.Data)
		}
	}
	readErr := stream.Err()
	switch {
	case scanErr != nil:
		// The server explicitly reported failure; prefer that over a
		// trailing read error from the dropped connection.
		return nil, scanErr
	case gotSummary:
		// A complete summary arrived; any post-summary read noise is
		// irrelevant to the scan result.
		return &summary, nil
	case readErr != nil:
		return nil, fmt.Errorf("scan: reading stream: %w", readErr)
	case decodeErr != nil:
		return nil, decodeErr
	default:
		return nil, errors.New("scan: stream ended before summary")
	}
}

func (b *httpBackend) apiClient(client *http.Client) (*apiclient.Client, error) {
	return apiclient.NewHTTPClient(b.baseURL, b.token, client)
}

func serviceResponseError(response *http.Response, body []byte, err error) error {
	switch response.StatusCode {
	case http.StatusNotFound:
		return errHTTPNotFound
	case http.StatusNotImplemented:
		return &notImplementedBodyError{message: notImplementedMessage(body)}
	case http.StatusOK:
		if err != nil {
			return err
		}
		if len(body) == 0 {
			return io.ErrUnexpectedEOF
		}
		return nil
	default:
		return &httpStatusError{method: response.Request.Method, path: response.Request.URL.RequestURI(), statusCode: response.StatusCode, body: body}
	}
}

// sessionWebURL mirrors the browser router: agent prefix and opaque session ID
// are separate path segments. The selected HTTP backend owns these IDs.
func (b *httpBackend) sessionWebURL(id string) string {
	if id == "" {
		return ""
	}
	prefix, rest, found := strings.Cut(id, ":")
	path := url.PathEscape(prefix)
	if found {
		path += "/" + url.PathEscape(rest)
	}
	base, err := url.Parse(b.browserURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return ""
	}
	base.User = nil
	base.RawQuery = ""
	base.ForceQuery = false
	base.Fragment = ""
	base.RawFragment = ""
	return strings.TrimRight(base.String(), "/") + "/sessions/" + path
}
