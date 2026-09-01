package tui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/vector"
)

// APIError reports a non-success response from the agentsview daemon.
type APIError struct {
	Status  int
	Message string
	Headers http.Header
}

func (e *APIError) Error() string {
	return fmt.Sprintf("daemon returned HTTP %d: %s", e.Status, e.Message)
}

type mutationSSEError struct {
	code    string
	message string
}

func (e *mutationSSEError) Error() string {
	return e.message
}

// Settings is the daemon settings payload used by the terminal settings page.
type Settings struct {
	AgentDirs        map[string][]string `json:"agent_dirs"`
	Terminal         TerminalConfig      `json:"terminal"`
	AuthToken        string              `json:"auth_token,omitempty"`
	GithubConfigured bool                `json:"github_configured"`
	Host             string              `json:"host"`
	Port             int                 `json:"port"`
	RequireAuth      bool                `json:"require_auth"`
	ReadOnly         bool                `json:"read_only"`
}

type TerminalConfig struct {
	Mode       string `json:"mode"`
	CustomBin  string `json:"custom_bin,omitempty"`
	CustomArgs string `json:"custom_args,omitempty"`
}

type WorktreeMappings struct {
	Machine  string                      `json:"machine"`
	Mappings []db.WorktreeProjectMapping `json:"mappings"`
}

type EmbeddingGenerations struct {
	Generations []vector.GenerationInfo `json:"generations"`
}

type PinList struct {
	Pins []db.PinnedMessage `json:"pins"`
}

type InsightList struct {
	Insights []db.Insight `json:"insights"`
}

type TrashList struct {
	Sessions []db.Session `json:"sessions"`
}

type PublishResult struct {
	GistURL string `json:"gist_url"`
	ViewURL string `json:"view_url"`
}

type UsageComparison struct {
	PriorFrom      string      `json:"priorFrom"`
	PriorTo        string      `json:"priorTo"`
	PriorTotalCost money.Money `json:"priorTotalCost"`
	DeltaPct       float64     `json:"deltaPct"`
}

type OrdinalsResponse struct {
	Ordinals []int `json:"ordinals"`
}

// SessionExtras contains the detail panels loaded with a transcript.
type SessionExtras struct {
	Activity *db.SessionActivityResponse
	Timing   *db.SessionTimingSummary
	Usage    *db.SessionUsage
}

// PageData holds the typed response for a non-session page.
type PageData struct {
	Analytics        *db.AnalyticsSummary
	AnalyticsSeries  *db.ActivityResponse
	Heatmap          *db.HeatmapResponse
	Projects         *db.ProjectsAnalyticsResponse
	HourOfWeek       *db.HourOfWeekResponse
	SessionShape     *db.SessionShapeResponse
	Velocity         *db.VelocityResponse
	Tools            *db.ToolsAnalyticsResponse
	Skills           *db.SkillsAnalyticsResponse
	TopSessions      *db.TopSessionsResponse
	Signals          *db.SignalsAnalyticsResponse
	Usage            *service.UsageSummaryResult
	UsageComparison  *UsageComparison
	UsagePairwise    *service.UsagePairwiseComparisonResponse
	UsageTopSessions []db.TopSessionEntry
	Activity         *activity.Report
	Trends           *db.TrendsTermsResponse
	Insights         []db.Insight
	Pins             []db.PinnedMessage
	Trash            []db.Session
	RecentEdits      *db.RecentEditsResult
	Settings         *Settings
	Mappings         *WorktreeMappings
	Embeddings       *EmbeddingGenerations
}

type pageUpdate struct {
	Data PageData
	Err  error
	Done bool
}

type progressiveDataClient interface {
	LoadPageUpdates(context.Context, Page, PageQuery) (<-chan pageUpdate, bool)
}

// DataClient is the TUI's daemon seam. Tests can replace it with a focused fake.
type DataClient interface {
	ListSessions(context.Context, service.ListFilter) (*service.SessionList, error)
	GetSession(context.Context, string) (*service.SessionDetail, error)
	SessionExtras(context.Context, string) (SessionExtras, error)
	Messages(context.Context, string, service.MessageFilter) (*service.MessageList, error)
	FindSession(context.Context, string, string) ([]int, error)
	Search(context.Context, service.SearchRequest) (*service.SessionSearchResult, error)
	LoadPage(context.Context, Page, PageQuery) (PageData, error)
	Mutate(context.Context, Mutation) (string, error)
	WatchEvents(context.Context) (<-chan ServerEvent, error)
}

type PageQuery struct {
	From, To, Timezone, Project, Agent, Machine, Terms, Search   string
	Model, GitBranch, ExcludeProject, ExcludeAgent, ExcludeModel string
	ActiveSince, Termination                                     string
	CompareDimension, CompareLeft, CompareRight                  string
	ActivityPreset, ActivityDate, ActivityBucket                 string
	ActivityAutomation, TrendGranularity, InsightType            string
	MinUserMessages, Offset                                      int
	IncludeOneShot, IncludeAutomated                             bool
}

type Mutation struct {
	Kind      string
	SessionID string
	MessageID int64
	InsightID int64
	Value     string
	Flag      bool
}

type ServerEvent struct {
	Event string
	Data  string
}

// Client connects the TUI to an agentsview daemon.
type Client struct {
	baseURL  string
	token    string
	readOnly atomic.Bool
	http     *http.Client
	stream   *http.Client
	sessions service.SessionService
}

func NewClient(baseURL, token string, readOnly bool) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	client := &Client{
		baseURL:  baseURL,
		token:    token,
		http:     &http.Client{Timeout: 30 * time.Second},
		stream:   &http.Client{},
		sessions: service.NewHTTPBackend(baseURL, token, readOnly),
	}
	client.readOnly.Store(readOnly)
	return client
}

func (c *Client) Settings(ctx context.Context) (*Settings, error) {
	var settings Settings
	if err := c.get(ctx, "/api/v1/settings", nil, &settings); err != nil {
		return nil, err
	}
	c.readOnly.Store(settings.ReadOnly)
	return &settings, nil
}

func (c *Client) ListSessions(ctx context.Context, f service.ListFilter) (*service.SessionList, error) {
	return c.sessions.List(ctx, f)
}

func (c *Client) GetSession(ctx context.Context, id string) (*service.SessionDetail, error) {
	return c.sessions.Get(ctx, id)
}

func (c *Client) SessionExtras(ctx context.Context, id string) (SessionExtras, error) {
	sid := url.PathEscape(id)
	var activityResponse db.SessionActivityResponse
	var timing db.SessionTimingSummary
	var usage db.SessionUsage
	var activityErr, timingErr, usageErr error
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		activityErr = c.get(ctx, "/api/v1/sessions/"+sid+"/activity", nil, &activityResponse)
	}()
	go func() {
		defer wg.Done()
		timingErr = c.get(ctx, "/api/v1/sessions/"+sid+"/timing-summary", nil, &timing)
	}()
	go func() {
		defer wg.Done()
		usageErr = c.get(ctx, "/api/v1/sessions/"+sid+"/usage", url.Values{"breakdown": {"false"}}, &usage)
	}()
	wg.Wait()
	if activityErr != nil {
		return SessionExtras{}, activityErr
	}
	if timingErr != nil {
		return SessionExtras{}, timingErr
	}
	if usageErr != nil {
		return SessionExtras{}, usageErr
	}
	return SessionExtras{Activity: &activityResponse, Timing: &timing, Usage: &usage}, nil
}

func (c *Client) Messages(ctx context.Context, id string, f service.MessageFilter) (*service.MessageList, error) {
	return c.sessions.Messages(ctx, id, f)
}

func (c *Client) FindSession(ctx context.Context, id, query string) ([]int, error) {
	values := url.Values{"q": {query}}
	var out OrdinalsResponse
	err := c.get(ctx, "/api/v1/sessions/"+url.PathEscape(id)+"/search", values, &out)
	return out.Ordinals, err
}

func (c *Client) Search(ctx context.Context, req service.SearchRequest) (*service.SessionSearchResult, error) {
	return c.sessions.Search(ctx, req)
}

func (c *Client) LoadPage(ctx context.Context, page Page, q PageQuery) (PageData, error) {
	values := reportValues(q)
	switch page {
	case PageDashboard:
		return c.loadDashboard(ctx, values)
	case PageUsage:
		return c.loadUsage(ctx, q, values)
	case PageActivity:
		preset := q.ActivityPreset
		if preset == "" {
			preset = "week"
		}
		values.Set("preset", preset)
		if q.ActivityDate != "" {
			values.Set("date", q.ActivityDate)
		}
		if q.ActivityBucket != "" {
			values.Set("bucket", q.ActivityBucket)
		}
		if q.ActivityAutomation != "" {
			values.Set("automation", q.ActivityAutomation)
		}
		var out activity.Report
		err := c.get(ctx, "/api/v1/activity/report", values, &out)
		return PageData{Activity: &out}, err
	case PageTrends:
		terms := splitTerms(q.Terms)
		if len(terms) == 0 {
			return PageData{}, nil
		}
		granularity := q.TrendGranularity
		if granularity == "" {
			granularity = "week"
		}
		values.Set("granularity", granularity)
		for _, term := range terms {
			values.Add("term", term)
		}
		var out db.TrendsTermsResponse
		err := c.get(ctx, "/api/v1/trends/terms", values, &out)
		return PageData{Trends: &out}, err
	case PageInsights:
		values.Del("from")
		values.Del("to")
		if q.From != "" {
			values.Set("date_from", q.From)
		}
		if q.To != "" {
			values.Set("date_to", q.To)
		}
		if q.InsightType != "" {
			values.Set("type", q.InsightType)
		}
		var out InsightList
		err := c.get(ctx, "/api/v1/insights", values, &out)
		return PageData{Insights: out.Insights}, err
	case PagePinned:
		var out PinList
		err := c.get(ctx, "/api/v1/pins", values, &out)
		return PageData{Pins: out.Pins}, err
	case PageTrash:
		var out TrashList
		err := c.get(ctx, "/api/v1/trash", nil, &out)
		return PageData{Trash: out.Sessions}, err
	case PageRecentEdits:
		values.Set("limit", "50")
		values.Set("offset", strconv.Itoa(q.Offset))
		if q.Search != "" {
			values.Set("search", q.Search)
		}
		var out db.RecentEditsResult
		err := c.get(ctx, "/api/v1/recent-edits", values, &out)
		return PageData{RecentEdits: &out}, err
	case PageSettings:
		var settings Settings
		if err := c.get(ctx, "/api/v1/settings", nil, &settings); err != nil {
			return PageData{}, err
		}
		data := PageData{Settings: &settings}
		if !settings.ReadOnly {
			var mappings WorktreeMappings
			if err := c.get(ctx, "/api/v1/settings/worktree-mappings", nil, &mappings); err == nil {
				data.Mappings = &mappings
			}
		}
		var generations EmbeddingGenerations
		if err := c.get(ctx, "/api/v1/embeddings/generations", nil, &generations); err == nil {
			data.Embeddings = &generations
		}
		return data, nil
	default:
		return PageData{}, fmt.Errorf("unsupported page %q", page)
	}
}

func (c *Client) LoadPageUpdates(
	ctx context.Context, page Page, q PageQuery,
) (<-chan pageUpdate, bool) {
	if page != PageDashboard {
		if page == PageUsage {
			return c.loadUsageUpdates(ctx, q, reportValues(q)), true
		}
		return nil, false
	}
	return c.loadDashboardUpdates(ctx, reportValues(q)), true
}

func (c *Client) loadUsage(ctx context.Context, q PageQuery, values url.Values) (PageData, error) {
	var data PageData
	for update := range c.loadUsageUpdates(ctx, q, values) {
		data = update.Data
		if update.Err != nil {
			return data, update.Err
		}
	}
	if err := ctx.Err(); err != nil {
		return data, err
	}
	return data, nil
}

func (c *Client) loadUsageUpdates(
	ctx context.Context, q PageQuery, values url.Values,
) <-chan pageUpdate {
	usageRequest := service.UsageRequest{
		From: q.From, To: q.To, Timezone: q.Timezone, Project: q.Project,
		Agent: q.Agent, Machine: q.Machine, Model: q.Model,
		GitBranch: q.GitBranch, ExcludeProject: q.ExcludeProject,
		ExcludeAgent: q.ExcludeAgent, ExcludeModel: q.ExcludeModel,
		ActiveSince: q.ActiveSince, Termination: q.Termination,
		MinUserMessages: q.MinUserMessages, IncludeOneShot: q.IncludeOneShot,
		IncludeAutomated: q.IncludeAutomated,
	}
	topValues := cloneURLValues(values)
	topValues.Set("include_one_shot", strconv.FormatBool(q.IncludeOneShot))
	topValues.Set("include_automated", strconv.FormatBool(q.IncludeAutomated))
	topValues.Set("limit", "20")

	type result struct {
		kind       string
		summary    *service.UsageSummaryResult
		top        []db.TopSessionEntry
		comparison *UsageComparison
		pairwise   *service.UsagePairwiseComparisonResponse
		err        error
	}
	results := make(chan result, 4)
	updates := make(chan pageUpdate, 1)
	loadCtx, cancel := context.WithCancel(ctx)
	go func() {
		summary, err := c.sessions.UsageSummary(loadCtx, usageRequest)
		results <- result{kind: "summary", summary: summary, err: err}
	}()
	go func() {
		var top []db.TopSessionEntry
		err := c.get(loadCtx, "/api/v1/usage/top-sessions", topValues, &top)
		results <- result{kind: "top", top: top, err: err}
	}()
	go func() {
		defer close(updates)
		defer cancel()
		data, pending := PageData{}, 2
		for pending > 0 {
			var loaded result
			select {
			case loaded = <-results:
			case <-loadCtx.Done():
				return
			}
			pending--
			if loaded.err != nil {
				select {
				case updates <- pageUpdate{Data: data, Err: loaded.err, Done: true}:
				case <-loadCtx.Done():
				}
				return
			}
			switch loaded.kind {
			case "summary":
				data.Usage = loaded.summary
				comparisonValues := cloneURLValues(values)
				comparisonValues.Set("current_microdollars", strconv.FormatInt(
					loaded.summary.Totals.TotalCost.Microdollars, 10,
				))
				pending++
				go func() {
					var comparison UsageComparison
					err := c.get(loadCtx, "/api/v1/usage/comparison", comparisonValues, &comparison)
					results <- result{kind: "comparison", comparison: &comparison, err: err}
				}()
				if q.CompareDimension != "" && q.CompareLeft != "" && q.CompareRight != "" {
					pending++
					go func() {
						pairwise, err := c.sessions.UsagePairwiseComparison(loadCtx, service.UsagePairwiseComparisonRequest{
							UsageRequest:  usageRequest,
							LeftDimension: q.CompareDimension, LeftValue: q.CompareLeft,
							RightDimension: q.CompareDimension, RightValue: q.CompareRight,
						})
						results <- result{kind: "pairwise", pairwise: pairwise, err: err}
					}()
				}
			case "top":
				data.UsageTopSessions = loaded.top
			case "comparison":
				data.UsageComparison = loaded.comparison
			case "pairwise":
				data.UsagePairwise = loaded.pairwise
			}
			select {
			case updates <- pageUpdate{Data: data, Done: pending == 0}:
			case <-loadCtx.Done():
				return
			}
		}
	}()
	return updates
}

func (c *Client) loadDashboard(ctx context.Context, values url.Values) (PageData, error) {
	var data PageData
	for update := range c.loadDashboardUpdates(ctx, values) {
		data = update.Data
		if update.Err != nil {
			return data, update.Err
		}
	}
	if err := ctx.Err(); err != nil {
		return data, err
	}
	return data, nil
}

func (c *Client) loadDashboardUpdates(
	ctx context.Context, values url.Values,
) <-chan pageUpdate {
	updates := make(chan pageUpdate, 1)
	loads := []struct {
		path string
		out  any
	}{
		{"/api/v1/analytics/summary", new(db.AnalyticsSummary)},
		{"/api/v1/analytics/activity", new(db.ActivityResponse)},
		{"/api/v1/analytics/heatmap", new(db.HeatmapResponse)},
		{"/api/v1/analytics/projects", new(db.ProjectsAnalyticsResponse)},
		{"/api/v1/analytics/hour-of-week", new(db.HourOfWeekResponse)},
		{"/api/v1/analytics/sessions", new(db.SessionShapeResponse)},
		{"/api/v1/analytics/velocity", new(db.VelocityResponse)},
		{"/api/v1/analytics/tools", new(db.ToolsAnalyticsResponse)},
		{"/api/v1/analytics/skills", new(db.SkillsAnalyticsResponse)},
		{"/api/v1/analytics/top-sessions", new(db.TopSessionsResponse)},
		{"/api/v1/analytics/signals", new(db.SignalsAnalyticsResponse)},
	}
	type result struct {
		index int
		err   error
	}
	results := make(chan result, len(loads))
	for index := range loads {
		go func() {
			results <- result{index: index, err: c.get(
				ctx, loads[index].path, values, loads[index].out,
			)}
		}()
	}
	go func() {
		defer close(updates)
		var data PageData
		for completed := 1; completed <= len(loads); completed++ {
			var loaded result
			select {
			case loaded = <-results:
			case <-ctx.Done():
				return
			}
			if loaded.err != nil {
				select {
				case updates <- pageUpdate{Data: data, Err: loaded.err, Done: true}:
				case <-ctx.Done():
				}
				return
			}
			setDashboardPart(&data, loaded.index, loads[loaded.index].out)
			select {
			case updates <- pageUpdate{Data: data, Done: completed == len(loads)}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return updates
}

func setDashboardPart(data *PageData, index int, value any) {
	switch index {
	case 0:
		data.Analytics = value.(*db.AnalyticsSummary)
	case 1:
		data.AnalyticsSeries = value.(*db.ActivityResponse)
	case 2:
		data.Heatmap = value.(*db.HeatmapResponse)
	case 3:
		data.Projects = value.(*db.ProjectsAnalyticsResponse)
	case 4:
		data.HourOfWeek = value.(*db.HourOfWeekResponse)
	case 5:
		data.SessionShape = value.(*db.SessionShapeResponse)
	case 6:
		data.Velocity = value.(*db.VelocityResponse)
	case 7:
		data.Tools = value.(*db.ToolsAnalyticsResponse)
	case 8:
		data.Skills = value.(*db.SkillsAnalyticsResponse)
	case 9:
		data.TopSessions = value.(*db.TopSessionsResponse)
	case 10:
		data.Signals = value.(*db.SignalsAnalyticsResponse)
	}
}

func reportValues(q PageQuery) url.Values {
	v := url.Values{}
	set := func(k, value string) {
		if value != "" {
			v.Set(k, value)
		}
	}
	set("from", q.From)
	set("to", q.To)
	set("timezone", q.Timezone)
	set("project", q.Project)
	set("agent", q.Agent)
	set("machine", q.Machine)
	set("model", q.Model)
	set("git_branch", q.GitBranch)
	set("exclude_project", q.ExcludeProject)
	set("exclude_agent", q.ExcludeAgent)
	set("exclude_model", q.ExcludeModel)
	set("active_since", q.ActiveSince)
	set("termination", q.Termination)
	if q.MinUserMessages > 0 {
		v.Set("min_user_messages", strconv.Itoa(q.MinUserMessages))
	}
	if q.IncludeOneShot {
		v.Set("include_one_shot", "true")
	}
	if q.IncludeAutomated {
		v.Set("include_automated", "true")
	}
	return v
}

func cloneURLValues(values url.Values) url.Values {
	cloned := make(url.Values, len(values))
	for key, entries := range values {
		cloned[key] = append([]string(nil), entries...)
	}
	return cloned
}

func splitTerms(raw string) []string {
	var terms []string
	for term := range strings.SplitSeq(raw, ",") {
		if term = strings.TrimSpace(term); term != "" {
			terms = append(terms, term)
		}
	}
	return terms
}

func (c *Client) Mutate(ctx context.Context, m Mutation) (string, error) {
	if c.readOnly.Load() {
		return "", errors.New("daemon is read-only")
	}
	sid := url.PathEscape(m.SessionID)
	switch m.Kind {
	case "rename":
		return "renamed session", c.do(ctx, http.MethodPatch, "/api/v1/sessions/"+sid+"/rename", map[string]any{"display_name": nullableString(m.Value)}, nil)
	case "delete":
		return "moved session to trash", c.do(ctx, http.MethodDelete, "/api/v1/sessions/"+sid, nil, nil)
	case "restore":
		return "restored session", c.do(ctx, http.MethodPost, "/api/v1/sessions/"+sid+"/restore", map[string]any{}, nil)
	case "delete-permanent":
		return "permanently deleted session", c.do(ctx, http.MethodDelete, "/api/v1/sessions/"+sid+"/permanent", nil, nil)
	case "star", "unstar":
		method := http.MethodPut
		if m.Kind == "unstar" {
			method = http.MethodDelete
		}
		return m.Kind + "red session", c.do(ctx, method, "/api/v1/sessions/"+sid+"/star", nil, nil)
	case "pin", "unpin":
		method := http.MethodPost
		if m.Kind == "unpin" {
			method = http.MethodDelete
		}
		path := fmt.Sprintf("/api/v1/sessions/%s/messages/%d/pin", sid, m.MessageID)
		return m.Kind + "ned message", c.do(ctx, method, path, map[string]any{"note": nullableString(m.Value)}, nil)
	case "sync", "resync":
		err := c.do(
			ctx, http.MethodPost, "/api/v1/"+m.Kind, map[string]any{}, nil,
		)
		return syncMutationResult(m.Kind+" complete", err)
	case "startup-sync":
		err := c.do(ctx, http.MethodPost, "/api/v1/sync", map[string]any{}, nil)
		var apiErr *APIError
		if errors.As(err, &apiErr) &&
			apiErr.Headers.Get("X-Agentsview-Resync-Required") != "" {
			err = c.do(
				ctx, http.MethodPost, "/api/v1/resync", map[string]any{}, nil,
			)
		}
		return syncMutationResult("sync complete", err)
	case "publish-session":
		var out PublishResult
		path := "/api/v1/sessions/" + sid + "/publish?secret=" + strconv.FormatBool(m.Flag)
		err := c.do(ctx, http.MethodPost, path, map[string]any{}, &out)
		if out.ViewURL != "" {
			return out.ViewURL, err
		}
		return out.GistURL, err
	case "publish-insight":
		var out PublishResult
		path := fmt.Sprintf("/api/v1/insights/%d/publish?secret=%t", m.InsightID, m.Flag)
		err := c.do(ctx, http.MethodPost, path, map[string]any{}, &out)
		if out.ViewURL != "" {
			return out.ViewURL, err
		}
		return out.GistURL, err
	case "delete-insight":
		path := fmt.Sprintf("/api/v1/insights/%d", m.InsightID)
		return "deleted insight", c.do(ctx, http.MethodDelete, path, nil, nil)
	case "empty-trash":
		return "emptied trash", c.do(ctx, http.MethodDelete, "/api/v1/trash", nil, nil)
	case "embeddings-build":
		return "started embeddings build", c.do(ctx, http.MethodPost, "/api/v1/embeddings/build", map[string]any{}, nil)
	case "embeddings-activate", "embeddings-retire":
		action := strings.TrimPrefix(m.Kind, "embeddings-")
		path := fmt.Sprintf("/api/v1/embeddings/generations/%d/%s", m.InsightID, action)
		return action + "d embedding generation", c.do(ctx, http.MethodPost, path, map[string]any{"force": m.Flag}, nil)
	case "worktrees-apply":
		return "applied worktree mappings", c.do(ctx, http.MethodPost, "/api/v1/settings/worktree-mappings/apply", map[string]any{}, nil)
	case "worktree-add":
		parts := strings.SplitN(m.Value, "|", 4)
		if len(parts) < 3 {
			return "", errors.New("worktree-add requires layout|path|project[|enabled]")
		}
		enabled := true
		if len(parts) == 4 {
			enabled = parseMutationBool(parts[3], true)
		}
		body := map[string]any{"layout": strings.TrimSpace(parts[0]), "path_prefix": strings.TrimSpace(parts[1]), "project": strings.TrimSpace(parts[2]), "enabled": enabled}
		return "created worktree mapping", c.do(ctx, http.MethodPost, "/api/v1/settings/worktree-mappings", body, nil)
	case "worktree-update":
		parts := strings.SplitN(m.Value, "|", 5)
		if len(parts) != 5 {
			return "", errors.New("worktree-update requires id|layout|path|project|enabled")
		}
		id, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		if err != nil || id <= 0 {
			return "", errors.New("worktree mapping ID must be a positive number")
		}
		body := map[string]any{"layout": strings.TrimSpace(parts[1]), "path_prefix": strings.TrimSpace(parts[2]), "project": strings.TrimSpace(parts[3]), "enabled": parseMutationBool(parts[4], true)}
		path := fmt.Sprintf("/api/v1/settings/worktree-mappings/%d", id)
		return "updated worktree mapping", c.do(ctx, http.MethodPut, path, body, nil)
	case "worktree-delete":
		path := fmt.Sprintf("/api/v1/settings/worktree-mappings/%d", m.InsightID)
		return "deleted worktree mapping", c.do(ctx, http.MethodDelete, path, nil, nil)
	case "terminal-mode":
		parts := strings.SplitN(m.Value, "|", 3)
		config := TerminalConfig{Mode: strings.TrimSpace(parts[0])}
		if len(parts) > 1 {
			config.CustomBin = strings.TrimSpace(parts[1])
		}
		if len(parts) > 2 {
			config.CustomArgs = strings.TrimSpace(parts[2])
		}
		return "updated terminal mode", c.do(ctx, http.MethodPost, "/api/v1/config/terminal", config, nil)
	case "github-token":
		return "saved GitHub token", c.do(ctx, http.MethodPost, "/api/v1/config/github", map[string]any{"token": m.Value}, nil)
	case "require-auth":
		var settings Settings
		err := c.do(ctx, http.MethodPut, "/api/v1/settings", map[string]any{"require_auth": m.Flag}, &settings)
		if settings.AuthToken != "" {
			c.token = settings.AuthToken
		}
		return "updated remote authentication", err
	case "sync-remote":
		parts := strings.SplitN(m.Value, "|", 3)
		host := map[string]any{"host": strings.TrimSpace(parts[0])}
		if len(parts) == 3 {
			host["url"], host["token"] = strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2])
		}
		body := map[string]any{"full": m.Flag, "hosts": []map[string]any{host}}
		return "remote sync complete", c.do(ctx, http.MethodPost, "/api/v1/sync/remotes", body, nil)
	case "import-claude", "import-chatgpt":
		kind := strings.TrimPrefix(m.Kind, "import-")
		if kind == "claude" {
			kind = "claude-ai"
		}
		return c.importArchive(ctx, kind, m.Value)
	case "export-html", "export-markdown":
		return c.exportSession(ctx, m.SessionID, m.Kind, m.Value)
	case "export-insight-html", "export-insight-markdown":
		suffix, route := ".html", "/export"
		if m.Kind == "export-insight-markdown" {
			suffix, route = ".md", "/md"
		}
		path := fmt.Sprintf("/api/v1/insights/%d%s", m.InsightID, route)
		return c.exportDocument(ctx, path, suffix, m.Value)
	case "generate-insight":
		parts := strings.SplitN(m.Value, "|", 4)
		if len(parts) < 3 {
			return "", errors.New("generate-insight requires type|from|to|project")
		}
		body := map[string]any{"type": strings.TrimSpace(parts[0]), "date_from": strings.TrimSpace(parts[1]), "date_to": strings.TrimSpace(parts[2]), "timezone": time.Now().Location().String()}
		if len(parts) == 4 {
			body["project"] = strings.TrimSpace(parts[3])
		}
		return "generated insight", c.do(ctx, http.MethodPost, "/api/v1/insights/generate", body, nil)
	case "open-session":
		return "opened session directory", c.do(ctx, http.MethodPost, "/api/v1/sessions/"+sid+"/open", map[string]any{}, nil)
	case "resume-session":
		return "resumed session", c.do(ctx, http.MethodPost, "/api/v1/sessions/"+sid+"/resume", map[string]any{}, nil)
	default:
		return "", fmt.Errorf("unsupported mutation %q", m.Kind)
	}
}

func parseMutationBool(value string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "on", "yes", "1":
		return true
	case "false", "off", "no", "0":
		return false
	default:
		return fallback
	}
}

func (c *Client) importArchive(ctx context.Context, kind, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, file); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/import/"+kind, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Origin", c.baseURL)
	c.authorize(req)
	resp, err := c.stream.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return "", &APIError{Status: resp.StatusCode, Message: apiMessage(b)}
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		if err := consumeMutationSSE(resp.Body); err != nil {
			return "", err
		}
		return "imported " + kind + " archive", nil
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return "", err
	}
	return "imported " + kind + " archive", nil
}

func (c *Client) exportSession(ctx context.Context, sessionID, kind, target string) (string, error) {
	suffix, route := ".html", "/export"
	if kind == "export-markdown" {
		suffix, route = ".md", "/md"
	}
	path := "/api/v1/sessions/" + url.PathEscape(sessionID) + route
	return c.exportDocument(ctx, path, suffix, target)
}

func (c *Client) exportDocument(ctx context.Context, path, suffix, target string) (string, error) {
	if target == "" {
		file, err := os.CreateTemp("", "agentsview-session-*"+suffix)
		if err != nil {
			return "", err
		}
		target = file.Name()
		if err := file.Close(); err != nil {
			return "", err
		}
		_ = os.Remove(target)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return "", err
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return "", &APIError{Status: resp.StatusCode, Message: apiMessage(b)}
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create export without overwriting: %w", err)
	}
	_, copyErr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(target)
		return "", copyErr
	}
	if closeErr != nil {
		_ = os.Remove(target)
		return "", closeErr
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return target, nil
	}
	return abs, nil
}

func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func syncMutationResult(message string, err error) (string, error) {
	var streamErr *mutationSSEError
	if errors.As(err, &streamErr) && streamErr.code == "sync_in_progress" {
		return streamErr.message, nil
	}
	return message, err
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return c.do(ctx, http.MethodGet, path, nil, out)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Origin", c.baseURL)
	}
	c.authorize(req)
	client := c.http
	if path == "/api/v1/sync" || path == "/api/v1/resync" ||
		path == "/api/v1/sync/remotes" || path == "/api/v1/insights/generate" {
		client = c.stream
		req.Header.Set("Accept", "text/event-stream")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return &APIError{
			Status: resp.StatusCode, Message: apiMessage(b),
			Headers: resp.Header.Clone(),
		}
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return consumeMutationSSE(resp.Body)
	}
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func consumeMutationSSE(reader io.Reader) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	var event string
	var data strings.Builder
	done := false
	consume := func() error {
		if event == "" && data.Len() == 0 {
			return nil
		}
		raw := data.String()
		switch event {
		case "error":
			return newMutationSSEError(raw)
		case "done":
			done = true
			var result struct {
				Error    string `json:"error"`
				Failures []struct {
					Error string `json:"error"`
				} `json:"failures"`
			}
			if json.Unmarshal([]byte(raw), &result) == nil {
				if result.Error != "" {
					return errors.New(result.Error)
				}
				if len(result.Failures) > 0 {
					messages := make([]string, 0, len(result.Failures))
					for _, failure := range result.Failures {
						if failure.Error != "" {
							messages = append(messages, failure.Error)
						}
					}
					if len(messages) == 0 {
						return errors.New("remote sync failed")
					}
					return errors.New(strings.Join(messages, "; "))
				}
			}
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := consume(); err != nil {
				return err
			}
			event = ""
			data.Reset()
			continue
		}
		if value, ok := strings.CutPrefix(line, "event: "); ok {
			event = value
			continue
		}
		if value, ok := strings.CutPrefix(line, "data: "); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if event != "" || data.Len() > 0 {
		if err := consume(); err != nil {
			return err
		}
	}
	if !done {
		return errors.New("daemon response ended before completion")
	}
	return nil
}

func newMutationSSEError(raw string) error {
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal([]byte(raw), &body)
	return &mutationSSEError{code: body.Code, message: sseErrorMessage(raw)}
}

func sseErrorMessage(raw string) string {
	var body struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(raw), &body) == nil {
		if body.Message != "" {
			return body.Message
		}
		if body.Error != "" {
			return body.Error
		}
	}
	if raw = strings.TrimSpace(raw); raw != "" {
		return raw
	}
	return "daemon operation failed"
}

func apiMessage(body []byte) string {
	var v struct {
		Error   any    `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &v) == nil {
		if v.Message != "" {
			return v.Message
		}
		switch e := v.Error.(type) {
		case string:
			if e != "" {
				return e
			}
		case map[string]any:
			if msg, ok := e["message"].(string); ok {
				return msg
			}
		}
	}
	if s := strings.TrimSpace(string(body)); s != "" {
		return s
	}
	return http.StatusText(http.StatusInternalServerError)
}

func (c *Client) authorize(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

func (c *Client) WatchEvents(ctx context.Context) (<-chan ServerEvent, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/events", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	c.authorize(req)
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, &APIError{Status: resp.StatusCode, Message: apiMessage(b)}
	}
	out := make(chan ServerEvent)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		scanSSE(ctx, resp.Body, out)
	}()
	return out, nil
}

func scanSSE(ctx context.Context, r io.Reader, out chan<- ServerEvent) {
	scanner := bufio.NewScanner(r)
	var event string
	var data []string
	flush := func() bool {
		if event == "" && len(data) == 0 {
			return true
		}
		msg := ServerEvent{Event: event, Data: strings.Join(data, "\n")}
		event, data = "", nil
		select {
		case out <- msg:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if !flush() {
				return
			}
			continue
		}
		if value, ok := strings.CutPrefix(line, "event:"); ok {
			event = strings.TrimSpace(value)
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimSpace(value))
		}
	}
	flush()
}
