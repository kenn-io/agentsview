package cursorusage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/money"
)

const defaultBaseURL = "https://api.cursor.com"

// Client talks to the Cursor Admin API.
type Client struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

// TokenUsage mirrors the per-request usage payload.
type TokenUsage struct {
	InputTokens      int `json:"inputTokens"`
	OutputTokens     int `json:"outputTokens"`
	CacheWriteTokens int `json:"cacheWriteTokens"`
	CacheReadTokens  int `json:"cacheReadTokens"`
}

// UsageEvent is a parsed Cursor usage event.
type UsageEvent struct {
	Timestamp      time.Time
	Model          string
	Kind           string
	TokenUsage     TokenUsage
	Charged        money.Money
	CursorTokenFee money.Money
	UserID         string
	UserEmail      string
	IsHeadless     bool
}

// Query controls one filtered usage-event page request.
type Query struct {
	StartDate time.Time
	EndDate   time.Time
	Page      int
	PageSize  int
	Email     string
	UserID    string
}

// Page is one paginated response from the API.
type Page struct {
	TotalCount int
	Events     []UsageEvent
}

func NewClient(apiKey string) *Client {
	return &Client{
		BaseURL: defaultBaseURL,
		APIKey:  apiKey,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func NewClientWithBaseURL(baseURL, apiKey string) *Client {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultBaseURL
	}
	c := NewClient(apiKey)
	c.BaseURL = strings.TrimRight(baseURL, "/")
	return c
}

func (c *Client) httpClient() *http.Client {
	if c != nil && c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Client) baseURL() string {
	if c == nil || strings.TrimSpace(c.BaseURL) == "" {
		return defaultBaseURL
	}
	return strings.TrimRight(c.BaseURL, "/")
}

func (c *Client) ListUsageEvents(
	ctx context.Context, q Query,
) (Page, error) {
	if c == nil {
		return Page{}, fmt.Errorf("nil client")
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return Page{}, fmt.Errorf("missing Cursor admin API key")
	}
	if q.Page <= 0 {
		q.Page = 1
	}
	if q.PageSize <= 0 {
		q.PageSize = 100
	}

	userID, err := requestUserID(q.UserID)
	if err != nil {
		return Page{}, err
	}

	body := map[string]any{
		"startDate": timestampMillis(q.StartDate),
		"endDate":   timestampMillis(q.EndDate),
		"page":      q.Page,
		"pageSize":  q.PageSize,
	}
	if strings.TrimSpace(q.Email) != "" {
		body["email"] = strings.TrimSpace(q.Email)
	}
	if strings.TrimSpace(q.UserID) != "" {
		body["userId"] = userID
	}

	data, err := json.Marshal(body)
	if err != nil {
		return Page{}, fmt.Errorf("encoding request: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.baseURL()+"/teams/filtered-usage-events",
		bytes.NewReader(data),
	)
	if err != nil {
		return Page{}, fmt.Errorf("creating request: %w", err)
	}
	req.SetBasicAuth(c.APIKey, "")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return Page{}, fmt.Errorf("calling filtered usage events: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Page{}, fmt.Errorf("cursor admin API returned %s", resp.Status)
	}

	var decoded usageEventsEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return Page{}, fmt.Errorf("decoding filtered usage events: %w", err)
	}

	rawEvents := decoded.usageEvents()
	events := make([]UsageEvent, 0, len(rawEvents))
	for _, raw := range rawEvents {
		ev, err := parseUsageEvent(raw)
		if err != nil {
			return Page{}, err
		}
		events = append(events, ev)
	}

	return Page{
		TotalCount: decoded.TotalCount,
		Events:     events,
	}, nil
}

func timestampMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixMilli()
}

func requestUserID(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	userID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid Cursor user ID %q", raw)
	}
	return userID, nil
}

func (c *Client) FetchAllUsageEvents(
	ctx context.Context, q Query,
) ([]UsageEvent, error) {
	page := q.Page
	if page <= 0 {
		page = 1
	}
	pageSize := q.PageSize
	if pageSize <= 0 {
		pageSize = 100
	}

	var out []UsageEvent
	for {
		resp, err := c.ListUsageEvents(ctx, Query{
			StartDate: q.StartDate,
			EndDate:   q.EndDate,
			Page:      page,
			PageSize:  pageSize,
			Email:     q.Email,
			UserID:    q.UserID,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, resp.Events...)
		if len(resp.Events) == 0 {
			return out, nil
		}
		if resp.TotalCount > 0 && page*pageSize >= resp.TotalCount {
			return out, nil
		}
		if len(resp.Events) < pageSize {
			return out, nil
		}
		page++
	}
}

type usageEventsEnvelope struct {
	TotalCount int             `json:"totalUsageEventsCount"`
	Usage      []rawUsageEvent `json:"usageEvents"`
	Display    []rawUsageEvent `json:"usageEventsDisplay"`
}

func (e usageEventsEnvelope) usageEvents() []rawUsageEvent {
	if len(e.Usage) > 0 {
		return e.Usage
	}
	return e.Display
}

type rawUsageEvent struct {
	Timestamp      string      `json:"timestamp"`
	Model          string      `json:"model"`
	Kind           string      `json:"kind"`
	TokenUsage     tokenUsage  `json:"tokenUsage"`
	ChargedCents   json.Number `json:"chargedCents"`
	CursorTokenFee json.Number `json:"cursorTokenFee"`
	UserID         string      `json:"userId"`
	UserEmail      string      `json:"userEmail"`
	IsHeadless     bool        `json:"isHeadless"`
}

type tokenUsage struct {
	InputTokens      int `json:"inputTokens"`
	OutputTokens     int `json:"outputTokens"`
	CacheWriteTokens int `json:"cacheWriteTokens"`
	CacheReadTokens  int `json:"cacheReadTokens"`
}

func parseUsageEvent(raw rawUsageEvent) (UsageEvent, error) {
	t, err := parseCursorTimestamp(raw.Timestamp)
	if err != nil {
		return UsageEvent{}, fmt.Errorf("parsing timestamp: %w", err)
	}
	charged, err := parseOptionalCents(raw.ChargedCents)
	if err != nil {
		return UsageEvent{}, fmt.Errorf("parsing charged cents: %w", err)
	}
	tokenFee, err := parseOptionalCents(raw.CursorTokenFee)
	if err != nil {
		return UsageEvent{}, fmt.Errorf("parsing Cursor token fee: %w", err)
	}
	return UsageEvent{
		Timestamp: t,
		Model:     strings.TrimSpace(raw.Model),
		Kind:      strings.TrimSpace(raw.Kind),
		TokenUsage: TokenUsage{
			InputTokens:      raw.TokenUsage.InputTokens,
			OutputTokens:     raw.TokenUsage.OutputTokens,
			CacheWriteTokens: raw.TokenUsage.CacheWriteTokens,
			CacheReadTokens:  raw.TokenUsage.CacheReadTokens,
		},
		Charged:        charged,
		CursorTokenFee: tokenFee,
		UserID:         strings.TrimSpace(raw.UserID),
		UserEmail:      strings.TrimSpace(raw.UserEmail),
		IsHeadless:     raw.IsHeadless,
	}, nil
}

func parseOptionalCents(value json.Number) (money.Money, error) {
	if value == "" {
		return money.Money{}, nil
	}
	parsed, err := money.ParseCents(value.String())
	if err != nil {
		return money.Money{}, err
	}
	if parsed.Microdollars < 0 {
		return money.Money{}, money.ErrNegative
	}
	return parsed, nil
}

func parseCursorTimestamp(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	if ms, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Unix(0, ms*int64(time.Millisecond)).UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("invalid timestamp %q", raw)
}
