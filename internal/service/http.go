// ABOUTME: httpBackend implements SessionService by proxying HTTP
// ABOUTME: calls to a running agentsview daemon.
package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// errHTTPNotFound is returned by getJSON for 404 responses so callers
// can distinguish "no such resource" from other transport errors
// without string-matching the status code. Kept unexported since
// only Get currently consumes it; other paths map status codes
// explicitly below.
var errHTTPNotFound = errors.New("http: not found")

// errHTTPNotImplemented is returned (wrapped in *errNotImplementedBody) by
// getJSON for 501 responses so callers can map a capability-absent daemon
// (e.g. search with no FTS index) to a typed sentinel instead of
// string-matching the status.
var errHTTPNotImplemented = errors.New("http: not implemented")

// errNotImplementedBody wraps errHTTPNotImplemented with the 501 response's
// error message, so callers that need cause-specific detail — e.g.
// SearchContent's "index is building: N% complete" or "index is stale ...
// --full-rebuild" remediation — can recover it instead of seeing only the
// bare sentinel. errors.Is(err, errHTTPNotImplemented) still holds for every
// caller that only cares about the status.
type errNotImplementedBody struct {
	message string
}

func (e *errNotImplementedBody) Error() string { return errHTTPNotImplemented.Error() }
func (e *errNotImplementedBody) Unwrap() error { return errHTTPNotImplemented }

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
	client            *http.Client
	longRunningClient *http.Client
	readOnly          bool
	token             string
}

// NewHTTPBackend constructs a SessionService that proxies to a
// running agentsview daemon at baseURL. When readOnly is true,
// Sync returns a clear error without making the HTTP round-trip.
// token, when non-empty, is attached as `Authorization: Bearer ...`
// on every request so the backend works against daemons running
// with require_auth=true.
func NewHTTPBackend(baseURL, token string, readOnly bool) SessionService {
	return &httpBackend{
		baseURL:           strings.TrimSuffix(baseURL, "/"),
		client:            &http.Client{Timeout: 30 * time.Second},
		longRunningClient: &http.Client{Timeout: 0},
		readOnly:          readOnly,
		token:             token,
	}
}

func (b *httpBackend) Get(
	ctx context.Context, id string,
) (*SessionDetail, error) {
	var out SessionDetail
	path := "/api/v1/sessions/" + url.PathEscape(id)
	err := b.getJSON(ctx, path, &out)
	if errors.Is(err, errHTTPNotFound) {
		// Match directBackend.Get: absent session returns (nil, nil)
		// so transport swaps stay neutral.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (b *httpBackend) FindSessionIDsByPartial(
	ctx context.Context, partial string, limit int,
) ([]string, error) {
	q := url.Values{}
	q.Set("partial", partial)
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out struct {
		IDs []string `json:"ids"`
	}
	if err := b.getJSON(ctx, "/api/v1/session-ids/resolve?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return out.IDs, nil
}

func (b *httpBackend) List(
	ctx context.Context, f ListFilter,
) (*SessionList, error) {
	q := filterToQuery(f)
	var out SessionList
	if err := b.getJSON(ctx, "/api/v1/sessions?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// filterToQuery converts a ListFilter into the URL query params
// expected by handleListSessions. Field mapping mirrors the
// server-side parser in internal/server/sessions.go.
func filterToQuery(f ListFilter) url.Values {
	q := url.Values{}
	setIfNotEmpty := func(k, v string) {
		if v != "" {
			q.Set(k, v)
		}
	}
	setIfNotEmpty("project", f.Project)
	setIfNotEmpty("exclude_project", f.ExcludeProject)
	setIfNotEmpty("machine", f.Machine)
	setIfNotEmpty("git_branch", f.GitBranch)
	setIfNotEmpty("agent", f.Agent)
	setIfNotEmpty("date", f.Date)
	setIfNotEmpty("date_from", f.DateFrom)
	setIfNotEmpty("date_to", f.DateTo)
	setIfNotEmpty("active_since", f.ActiveSince)
	if f.MinMessages > 0 {
		q.Set("min_messages", strconv.Itoa(f.MinMessages))
	}
	if f.MaxMessages > 0 {
		q.Set("max_messages", strconv.Itoa(f.MaxMessages))
	}
	if f.MinUserMessages > 0 {
		q.Set("min_user_messages", strconv.Itoa(f.MinUserMessages))
	}
	if f.IncludeOneShot {
		q.Set("include_one_shot", "true")
	}
	if f.IncludeAutomated {
		q.Set("include_automated", "true")
	}
	if f.IncludeChildren {
		q.Set("include_children", "true")
	}
	setIfNotEmpty("outcome", f.Outcome)
	setIfNotEmpty("health_grade", f.HealthGrade)
	setIfNotEmpty("termination", f.Termination)
	if f.MinToolFailures != nil {
		q.Set("min_tool_failures", strconv.Itoa(*f.MinToolFailures))
	}
	if f.HasSecret {
		q.Set("has_secret", "true")
	}
	if f.Starred {
		q.Set("starred", "true")
	}
	setIfNotEmpty("cursor", f.Cursor)
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	setIfNotEmpty("order_by", f.OrderBy)
	if f.Descending != nil {
		q.Set("descending", strconv.FormatBool(*f.Descending))
	}
	return q
}

func (b *httpBackend) Messages(
	ctx context.Context, id string, f MessageFilter,
) (*MessageList, error) {
	q := url.Values{}
	if f.From != nil {
		q.Set("from", strconv.Itoa(*f.From))
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	if f.Direction != "" {
		q.Set("direction", f.Direction)
	}
	if f.Around != nil {
		q.Set("around", strconv.Itoa(*f.Around))
	}
	if f.Before != nil {
		q.Set("before", strconv.Itoa(*f.Before))
	}
	if f.After != nil {
		q.Set("after", strconv.Itoa(*f.After))
	}
	if len(f.Roles) > 0 {
		q.Set("roles", strings.Join(f.Roles, ","))
	}
	path := "/api/v1/sessions/" + url.PathEscape(id) +
		"/messages?" + q.Encode()
	var out MessageList
	if err := b.getJSON(ctx, path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (b *httpBackend) ToolCalls(
	ctx context.Context, id string,
) (*ToolCallList, error) {
	var out ToolCallList
	path := "/api/v1/sessions/" + url.PathEscape(id) + "/tool-calls"
	if err := b.getJSON(ctx, path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (b *httpBackend) Sync(
	ctx context.Context, in SyncInput,
) (*SessionDetail, error) {
	if b.readOnly {
		// Return the shared sentinel so callers can
		// errors.Is(err, db.ErrReadOnly) regardless of
		// transport.
		return nil, fmt.Errorf(
			"sync: daemon at %s is read-only: %w",
			b.baseURL, db.ErrReadOnly,
		)
	}
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost,
		b.baseURL+"/api/v1/sessions/sync",
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// The daemon's CSRF guard rejects mutating requests whose Origin
	// is not in the allowlist. Setting Origin to the daemon's own
	// baseURL satisfies that check for the CLI, which has no real
	// browser origin.
	req.Header.Set("Origin", b.baseURL)
	b.addAuth(req)
	resp, err := b.longRunningClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotImplemented {
		// Daemon is read-only (pg serve). Surface as the shared
		// sentinel so CLI callers can errors.Is it.
		return nil, fmt.Errorf(
			"sync: daemon at %s: %w", b.baseURL, db.ErrReadOnly,
		)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf(
			"sync: HTTP %d: %s", resp.StatusCode, msg,
		)
	}
	var detail SessionDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return nil, err
	}
	return &detail, nil
}

func (b *httpBackend) Watch(
	ctx context.Context, id string,
) (<-chan Event, error) {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet,
		b.baseURL+"/api/v1/sessions/"+url.PathEscape(id)+"/watch",
		nil,
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	b.addAuth(req)
	// Use a separate no-timeout client so long-lived streams do not
	// hit the 30s default on b.client.
	resp, err := b.longRunningClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, fmt.Errorf("watch: session not found: %s", id)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("watch: HTTP %d", resp.StatusCode)
	}

	out := make(chan Event)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		// A dropped live-watch stream is signalled to the consumer by
		// closing out; there is no error channel, so a read error here
		// is not actionable.
		_ = parseSSE(resp.Body, func(ev Event) bool {
			select {
			case out <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		})
	}()
	return out, nil
}

func (b *httpBackend) Stats(
	ctx context.Context, f StatsFilter,
) (*SessionStats, error) {
	q := url.Values{}
	setIfNotEmpty := func(k, v string) {
		if v != "" {
			q.Set(k, v)
		}
	}
	setIfNotEmpty("since", f.Since)
	setIfNotEmpty("until", f.Until)
	setIfNotEmpty("agent", f.Agent)
	setIfNotEmpty("timezone", f.Timezone)
	includeOneShot := f.IncludeOneShot
	includeAutomated := f.IncludeAutomated
	if !f.ApplyDefaultVisibility {
		includeOneShot = true
		includeAutomated = true
	}
	q.Set("include_one_shot", strconv.FormatBool(includeOneShot))
	q.Set("include_automated", strconv.FormatBool(includeAutomated))
	for _, p := range f.IncludeProjects {
		q.Add("include_project", p)
	}
	for _, p := range f.ExcludeProjects {
		q.Add("exclude_project", p)
	}
	q.Set("include_git_outcomes", strconv.FormatBool(f.IncludeGitOutcomes))
	q.Set("include_github_outcomes", strconv.FormatBool(f.IncludeGitHubOutcomes))

	var out SessionStats
	err := b.getJSON(ctx, "/api/v1/session-stats?"+q.Encode(), &out)
	if errors.Is(err, errHTTPNotImplemented) {
		return nil, fmt.Errorf(
			"stats: daemon at %s: %w", b.baseURL, db.ErrReadOnly,
		)
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (b *httpBackend) Search(
	ctx context.Context, req SearchRequest,
) (*SessionSearchResult, error) {
	q := url.Values{}
	q.Set("q", req.Query)
	if req.Project != "" {
		q.Set("project", req.Project)
	}
	if req.Sort != "" {
		q.Set("sort", req.Sort)
	}
	if req.Cursor > 0 {
		q.Set("cursor", strconv.Itoa(req.Cursor))
	}
	if req.Limit > 0 {
		q.Set("limit", strconv.Itoa(req.Limit))
	}
	// GET /api/v1/search responds with {query, results, count, next};
	// "next" is the int pagination cursor. Decode into a local shape and
	// map it onto SessionSearchResult so the wire format stays internal.
	var out struct {
		Results []db.SearchResult `json:"results"`
		Next    int               `json:"next"`
	}
	if err := b.getJSON(ctx, "/api/v1/search?"+q.Encode(), &out); err != nil {
		if errors.Is(err, errHTTPNotImplemented) {
			return nil, ErrSearchUnavailable
		}
		return nil, err
	}
	results := out.Results
	if results == nil {
		results = []db.SearchResult{}
	}
	return &SessionSearchResult{Results: results, NextCursor: out.Next}, nil
}

func (b *httpBackend) SearchContent(
	ctx context.Context, req ContentSearchRequest,
) (*ContentSearchResult, error) {
	q := url.Values{}
	q.Set("pattern", req.Pattern)
	if req.Mode != "" {
		q.Set("mode", req.Mode)
	}
	if len(req.Sources) > 0 {
		q.Set("in", strings.Join(req.Sources, ","))
	}
	if req.ExcludeSystem {
		q.Set("exclude_system", "true")
	}
	if req.Reveal {
		q.Set("reveal", "true")
	}
	for k, v := range map[string]string{
		"project":         req.Project,
		"exclude_project": req.ExcludeProject,
		"machine":         req.Machine,
		"git_branch":      req.GitBranch,
		"agent":           req.Agent,
		"date":            req.Date,
		"date_from":       req.DateFrom,
		"date_to":         req.DateTo,
		"active_since":    req.ActiveSince,
		"scope":           req.Scope,
	} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if req.IncludeChildren {
		q.Set("include_children", "true")
	}
	if req.IncludeAutomated {
		q.Set("include_automated", "true")
	}
	if req.IncludeOneShot {
		q.Set("include_one_shot", "true")
	}
	if req.Limit > 0 {
		q.Set("limit", strconv.Itoa(req.Limit))
	}
	if req.Cursor > 0 {
		q.Set("cursor", strconv.Itoa(req.Cursor))
	}
	if req.Context > 0 {
		q.Set("context", strconv.Itoa(req.Context))
	}
	var out ContentSearchResult
	var opts []func(*http.Request)
	if req.Mode == "semantic" || req.Mode == "hybrid" {
		opts = append(opts, func(r *http.Request) {
			r.Header.Set(SemanticSearchIntentHeader, SemanticSearchIntentValue)
		})
	}
	if err := b.getJSONLong(ctx, "/api/v1/search/content?"+q.Encode(), &out, opts...); err != nil {
		var notImpl *errNotImplementedBody
		if errors.As(err, &notImpl) {
			return nil, wrapSemanticUnavailable(notImpl.message)
		}
		return nil, err
	}
	return &out, nil
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
	sentinel := ErrSemanticUnavailable.Error()
	if message == "" || message == sentinel {
		return ErrSemanticUnavailable
	}
	if cause, ok := strings.CutPrefix(message, sentinel); ok {
		return fmt.Errorf("%w%s", ErrSemanticUnavailable, cause)
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

func (b *httpBackend) UsageSummary(
	ctx context.Context, req UsageRequest,
) (*UsageSummaryResult, error) {
	q := url.Values{}
	for k, v := range map[string]string{
		"from":                req.From,
		"to":                  req.To,
		"timezone":            req.Timezone,
		"agent":               req.Agent,
		"project":             req.Project,
		"machine":             req.Machine,
		"git_branch":          req.GitBranch,
		"exclude_project":     req.ExcludeProject,
		"exclude_project_key": req.ExcludeProjectKey,
		"exclude_agent":       req.ExcludeAgent,
		"exclude_model":       req.ExcludeModel,
		"model":               req.Model,
		"active_since":        req.ActiveSince,
		"termination":         req.Termination,
	} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if req.MinUserMessages > 0 {
		q.Set("min_user_messages", strconv.Itoa(req.MinUserMessages))
	}
	if req.NoDefaultRange {
		q.Set("no_default_range", "true")
	}
	if req.Breakdowns != nil {
		q.Set("breakdowns", strconv.FormatBool(*req.Breakdowns))
	}
	if req.SessionCounts != nil {
		q.Set("session_counts", strconv.FormatBool(*req.SessionCounts))
	}
	// include_one_shot defaults to true on the server, so it must be sent
	// explicitly to transmit a false value; include_automated defaults to
	// false. Send both explicitly so the round-trip matches the direct
	// backend regardless of the daemon's defaults.
	q.Set("include_one_shot", strconv.FormatBool(req.IncludeOneShot))
	q.Set("include_automated", strconv.FormatBool(req.IncludeAutomated))

	var out UsageSummaryResult
	err := b.getJSON(ctx, "/api/v1/usage/summary?"+q.Encode(), &out)
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
	return &out, nil
}

func (b *httpBackend) UsagePairwiseComparison(
	ctx context.Context, req UsagePairwiseComparisonRequest,
) (*UsagePairwiseComparisonResponse, error) {
	q := url.Values{}
	for k, v := range map[string]string{
		"from":                req.From,
		"to":                  req.To,
		"timezone":            req.Timezone,
		"agent":               req.Agent,
		"project":             req.Project,
		"machine":             req.Machine,
		"git_branch":          req.GitBranch,
		"exclude_project":     req.ExcludeProject,
		"exclude_project_key": req.ExcludeProjectKey,
		"exclude_agent":       req.ExcludeAgent,
		"exclude_model":       req.ExcludeModel,
		"active_since":        req.ActiveSince,
		"termination":         req.Termination,
	} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if req.LeftDimension != "" {
		q.Set("left_dimension", req.LeftDimension)
	}
	if req.LeftValue != "" {
		q.Set("left_value", req.LeftValue)
	}
	if req.RightDimension != "" {
		q.Set("right_dimension", req.RightDimension)
	}
	if req.RightValue != "" {
		q.Set("right_value", req.RightValue)
	}
	if req.MinUserMessages > 0 {
		q.Set("min_user_messages", strconv.Itoa(req.MinUserMessages))
	}
	if req.NoDefaultRange {
		q.Set("no_default_range", "true")
	}
	// Include explicit booleans to preserve source defaults.
	q.Set("include_one_shot", strconv.FormatBool(req.IncludeOneShot))
	q.Set("include_automated", strconv.FormatBool(req.IncludeAutomated))
	if req.Model != "" {
		q.Set("model", req.Model)
	}

	var out UsagePairwiseComparisonResponse
	err := b.getJSON(
		ctx,
		"/api/v1/usage/pairwise-comparison?"+q.Encode(),
		&out,
	)
	if errors.Is(err, errHTTPNotImplemented) {
		return nil, fmt.Errorf(
			"usage pairwise comparison: daemon at %s: %w", b.baseURL, db.ErrReadOnly,
		)
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (b *httpBackend) ListRecallEntries(
	ctx context.Context, f RecallFilter,
) (*RecallList, error) {
	if err := ValidateRecallEntryLimit(f.Limit); err != nil {
		return nil, err
	}
	q := recallFilterToQuery(f)
	var out RecallList
	if err := b.getJSON(ctx, "/api/v1/recall/entries?"+q.Encode(), &out); err != nil {
		if errors.Is(err, errHTTPNotImplemented) {
			return nil, fmt.Errorf(
				"recall list: daemon at %s: %w", b.baseURL, db.ErrReadOnly,
			)
		}
		return nil, err
	}
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
	var out db.RecallEntry
	path := "/api/v1/recall/entries/" + url.PathEscape(id)
	err := b.getJSON(ctx, path, &out)
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
	return &out, nil
}

func (b *httpBackend) QueryRecallEntries(
	ctx context.Context, req RecallQuery,
) (*RecallQueryResult, error) {
	if err := ValidateRecallEntryLimit(req.Limit); err != nil {
		return nil, err
	}
	if req.IncludeContext {
		if _, err := NormalizeRecallContextMaxBytes(req.ContextMaxBytes); err != nil {
			return nil, err
		}
	}
	if _, err := NormalizeRecallQuerySurface(req.Surface); err != nil {
		return nil, err
	}
	if req.StrictRecording {
		return nil, fmt.Errorf("strict recall recording requires a direct backend")
	}
	var out RecallQueryResult
	if err := b.postJSON(ctx, "/api/v1/recall/query", req, &out); err != nil {
		if errors.Is(err, errHTTPNotImplemented) {
			return nil, fmt.Errorf(
				"recall query: daemon at %s: %w", b.baseURL, db.ErrReadOnly,
			)
		}
		return nil, err
	}
	if out.RecallEntries == nil {
		out.RecallEntries = []db.RecallResult{}
	}
	if req.TrustedOnly {
		out.TrustedOnly = true
	}
	if out.Summary == nil {
		out.Summary = BuildRecallQuerySummary(out.RecallEntries)
	}
	if out.ContextEntries == nil && out.ContextMeta != nil {
		out.ContextEntries = RecallContextResults(
			out.RecallEntries, out.ContextMeta,
		)
	}
	if err := ValidateRecallContextEntries(
		out.ContextEntries, out.ContextMeta,
	); err != nil {
		return nil, err
	}
	if out.ContextSummary == nil && out.ContextMeta != nil {
		out.ContextSummary = BuildRecallContextSummary(
			out.RecallEntries, out.ContextMeta,
		)
	}
	return &out, nil
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
	var out db.RecallImportResult
	path := "/api/v1/recall/import"
	q := url.Values{}
	if opts.DryRun {
		q.Set("dry_run", "true")
	}
	if opts.RequireExistingSessions {
		q.Set("require_existing_sessions", "true")
	} else {
		q.Set("allow_placeholder_sessions", "true")
	}
	if opts.AllowProductionImport {
		q.Set("allow_production_import", "true")
	}
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	if err := b.postRaw(ctx, path, r, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func recallFilterToQuery(f RecallFilter) url.Values {
	q := url.Values{}
	for k, v := range map[string]string{
		"q":                      f.Query,
		"project":                f.Project,
		"cwd":                    f.CWD,
		"git_branch":             f.GitBranch,
		"agent":                  f.Agent,
		"type":                   f.Type,
		"scope":                  f.Scope,
		"status":                 f.Status,
		"extractor_method":       f.ExtractorMethod,
		"source_session_id":      f.SourceSessionID,
		"source_episode_id":      f.SourceEpisodeID,
		"source_run_id":          f.SourceRunID,
		"supersedes_entry_id":    f.SupersedesEntryID,
		"superseded_by_entry_id": f.SupersededByEntryID,
	} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	if f.TrustedOnly {
		q.Set("trusted_only", "true")
	}
	return q
}

func (b *httpBackend) ListSecrets(
	ctx context.Context, f SecretListFilter,
) (*SecretFindingList, error) {
	q := url.Values{}
	for k, v := range map[string]string{
		"project": f.Project, "agent": f.Agent,
		"date_from": f.DateFrom, "date_to": f.DateTo,
		"rule": f.Rule, "confidence": f.Confidence,
	} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if f.Reveal {
		q.Set("reveal", "true")
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	if f.Cursor > 0 {
		q.Set("cursor", strconv.Itoa(f.Cursor))
	}
	var out SecretFindingList
	if err := b.getJSON(ctx, "/api/v1/secrets?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (b *httpBackend) ScanSecrets(
	ctx context.Context, in SecretScanInput,
	progress func(SecretScanProgress),
) (*SecretScanSummary, error) {
	if b.readOnly {
		return nil, fmt.Errorf("scan: daemon at %s is read-only: %w",
			b.baseURL, db.ErrReadOnly)
	}
	q := url.Values{}
	if in.Backfill {
		q.Set("backfill", "true")
	}
	for k, v := range map[string]string{
		"project": in.Project, "agent": in.Agent,
		"date_from": in.DateFrom, "date_to": in.DateTo,
	} {
		if v != "" {
			q.Set(k, v)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		b.baseURL+"/api/v1/secrets/scan?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Origin", b.baseURL)
	b.addAuth(req)
	resp, err := (&http.Client{Timeout: 0}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotImplemented {
		return nil, fmt.Errorf("scan: daemon at %s: %w", b.baseURL, db.ErrReadOnly)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("scan: HTTP %d", resp.StatusCode)
	}
	return parseScanStream(resp.Body, progress)
}

// parseScanStream decodes the scan SSE stream: progress ticks invoke the
// callback, the summary event is the result, and an error event becomes an
// error. A stream that ends without a summary event (broken connection,
// canceled context, daemon crash) is reported as an error rather than a
// zero-value success.
func parseScanStream(
	r io.Reader, progress func(SecretScanProgress),
) (*SecretScanSummary, error) {
	var summary SecretScanSummary
	var scanErr, decodeErr error
	var gotSummary bool
	readErr := parseSSE(r, func(ev Event) bool {
		switch ev.Event {
		case "progress":
			var p SecretScanProgress
			if json.Unmarshal([]byte(ev.Data), &p) == nil && progress != nil {
				progress(p)
			}
		case "summary":
			if err := json.Unmarshal([]byte(ev.Data), &summary); err != nil {
				decodeErr = fmt.Errorf("scan: decoding summary: %w", err)
			} else {
				gotSummary = true
			}
		case "error":
			scanErr = fmt.Errorf("scan: %s", ev.Data)
		}
		return true
	})
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

// parseSSE reads a Server-Sent Events stream and invokes emit for
// each complete event. emit returns false to stop parsing (e.g. on
// context cancel). It returns any read error from the underlying
// stream, so callers can tell a truncated/broken stream apart from a
// clean end; a voluntary stop (emit returning false) returns nil.
func parseSSE(r io.Reader, emit func(Event) bool) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var event, data string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if event != "" {
				if !emit(Event{Event: event, Data: data}) {
					return nil
				}
			}
			event, data = "", ""
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			event = line[len("event: "):]
		} else if strings.HasPrefix(line, "data: ") {
			data = line[len("data: "):]
		}
	}
	return scanner.Err()
}

// addAuth attaches the bearer token to req when the backend was
// constructed with one. Safe to call on a request without a token
// configured (no-op).
func (b *httpBackend) addAuth(req *http.Request) {
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}
}

func (b *httpBackend) getJSON(
	ctx context.Context, path string, out any, opts ...func(*http.Request),
) error {
	return b.getJSONWithClient(ctx, b.client, path, out, opts...)
}

func (b *httpBackend) getJSONLong(
	ctx context.Context, path string, out any, opts ...func(*http.Request),
) error {
	return b.getJSONWithClient(ctx, b.longRunningClient, path, out, opts...)
}

func (b *httpBackend) getJSONWithClient(
	ctx context.Context,
	client *http.Client,
	path string,
	out any,
	opts ...func(*http.Request),
) error {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, b.baseURL+path, nil,
	)
	if err != nil {
		return err
	}
	b.addAuth(req)
	for _, opt := range opts {
		opt(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errHTTPNotFound
	}
	if resp.StatusCode == http.StatusNotImplemented {
		body, _ := io.ReadAll(resp.Body)
		return &errNotImplementedBody{message: notImplementedMessage(body)}
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf(
			"GET %s: HTTP %d: %s", path, resp.StatusCode, msg,
		)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (b *httpBackend) postJSON(
	ctx context.Context, path string, in any, out any,
) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, b.baseURL+path, bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", b.baseURL)
	b.addAuth(req)
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errHTTPNotFound
	}
	if resp.StatusCode == http.StatusNotImplemented {
		body, _ := io.ReadAll(resp.Body)
		return &errNotImplementedBody{message: notImplementedMessage(body)}
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf(
			"POST %s: HTTP %d: %s", path, resp.StatusCode, msg,
		)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (b *httpBackend) postRaw(
	ctx context.Context, path string, in io.Reader, out any,
) error {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, b.baseURL+path, in,
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("Origin", b.baseURL)
	b.addAuth(req)
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errHTTPNotFound
	}
	if resp.StatusCode == http.StatusNotImplemented {
		// A read-only (pg serve) daemon returns 501 for write endpoints.
		// Surface the shared sentinel so callers can errors.Is it, matching
		// Sync and ScanSecrets.
		return fmt.Errorf(
			"daemon at %s is read-only: %w", b.baseURL, db.ErrReadOnly,
		)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf(
			"POST %s: HTTP %d: %s", path, resp.StatusCode, msg,
		)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
