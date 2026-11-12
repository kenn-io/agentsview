package cursorusage

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v7"

	"go.kenn.io/agentsview/internal/httputil"
	"go.kenn.io/agentsview/internal/money"
)

const defaultBaseURL = "https://api.cursor.com"

// Cursor limits /teams/filtered-usage-events to 60 requests per minute per
// team, shared by every caller using that team's admin keys. Spacing page
// requests a second apart keeps one fetch inside that limit on its own;
// retries cover the requests other callers spend from the same budget.
const defaultRequestInterval = time.Second

// A rate-limited Admin API response carries Retry-After: 60, the length of
// Cursor's limit window. Longer server-requested waits are capped here so a
// malformed header cannot stall a fetch, and backoff growth stops at the same
// ceiling.
const maxRetryDelay = 60 * time.Second

// maxPageAttempts bounds how many times one page is requested before the fetch
// gives up. With Retry-After: 60, five attempts wait out four limit windows.
const maxPageAttempts = 5

// Client talks to the Cursor Admin API.
type Client struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client

	// RequestInterval is the minimum time between the starts of consecutive
	// page requests in FetchAllUsageEvents. NewClient sets it to one second;
	// zero disables pacing.
	RequestInterval time.Duration

	// OnRetry, when set, is called before FetchAllUsageEvents waits to retry
	// a page after a rate-limit or server error, with that error and the wait.
	OnRetry func(err error, wait time.Duration)
}

// StatusError reports a non-2xx response from the Cursor Admin API.
type StatusError struct {
	StatusCode int
	Status     string
	// RetryAfter is the server's requested wait from the Retry-After header,
	// or zero when the response did not include one.
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	return "cursor admin API returned " + e.Status
}

// Retryable reports whether the same request may succeed later: a rate limit
// clears when Cursor's window rolls over, and a server error may be transient.
// Other statuses, such as a rejected key or bad request, will not change.
func (e *StatusError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests ||
		e.StatusCode >= http.StatusInternalServerError
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
		RequestInterval: defaultRequestInterval,
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
		return Page{}, errors.New("nil client")
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return Page{}, errors.New("missing Cursor admin API key")
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
		return Page{}, &StatusError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			RetryAfter: httputil.ParseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	var decoded usageEventsEnvelope
	if err := json.UnmarshalRead(resp.Body, &decoded); err != nil {
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

// FetchAllUsageEvents requests every page of q, starting at q.Page. Page
// requests start at least RequestInterval apart. A page that fails with a
// rate limit or server error is requested again after the server's
// Retry-After wait, or after an exponential backoff when the response has
// none, up to maxPageAttempts times; any other failure ends the fetch.
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
	var lastRequest time.Time
	for {
		if err := c.waitForRequestSlot(ctx, lastRequest); err != nil {
			return nil, err
		}
		lastRequest = time.Now()
		resp, err := c.listUsageEventsWithRetry(ctx, Query{
			StartDate: q.StartDate,
			EndDate:   q.EndDate,
			Page:      page,
			PageSize:  pageSize,
			Email:     q.Email,
			UserID:    q.UserID,
		})
		if err != nil {
			return nil, fmt.Errorf("fetching Cursor usage page %d: %w", page, err)
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

// waitForRequestSlot blocks until RequestInterval has passed since
// lastRequest. A zero lastRequest means no request has been made yet.
func (c *Client) waitForRequestSlot(
	ctx context.Context, lastRequest time.Time,
) error {
	if lastRequest.IsZero() || c.RequestInterval <= 0 {
		return nil
	}
	wait := c.RequestInterval - time.Since(lastRequest)
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// listUsageEventsWithRetry requests one page, retrying a rate limit or server
// error as FetchAllUsageEvents describes.
func (c *Client) listUsageEventsWithRetry(
	ctx context.Context, q Query,
) (Page, error) {
	page, err := backoff.Retry(ctx, func() (Page, error) {
		page, err := c.ListUsageEvents(ctx, q)
		if err == nil {
			return page, nil
		}
		statusErr, ok := errors.AsType[*StatusError](err)
		if !ok || !statusErr.Retryable() {
			return Page{}, backoff.Permanent(err)
		}
		if statusErr.RetryAfter > 0 {
			return Page{}, backoff.RetryAfter(
				min(statusErr.RetryAfter, maxRetryDelay), err,
			)
		}
		return Page{}, err
	},
		backoff.WithBackOff(newRetryBackOff()),
		backoff.WithMaxTries(maxPageAttempts),
		// Attempts and the context bound the loop; an elapsed-time limit
		// would cut off a run of honored 60-second Retry-After waits.
		backoff.WithMaxElapsedTime(0),
		backoff.WithNotify(func(err error, wait time.Duration) {
			if c.OnRetry == nil {
				return
			}
			if statusErr, ok := errors.AsType[*StatusError](err); ok {
				err = statusErr
			}
			c.OnRetry(err, wait)
		}),
	)
	retryErr := backoff.AsRetryError(err)
	if retryErr == nil {
		return page, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Page{}, ctxErr
	}
	if errors.Is(retryErr.Cause, backoff.ErrExhausted) {
		return Page{}, fmt.Errorf(
			"giving up after %d attempts: %w", maxPageAttempts, retryErr.LastErr,
		)
	}
	return Page{}, retryErr.LastErr
}

// newRetryBackOff paces retries of a failed page when the server gave no
// Retry-After: about 2s, 4s, 8s and 16s, randomized so concurrent callers on
// the same team do not retry in lockstep.
func newRetryBackOff() *backoff.ExponentialBackOff {
	return &backoff.ExponentialBackOff{
		InitialInterval:     2 * time.Second,
		RandomizationFactor: 0.5,
		Multiplier:          2,
		MaxInterval:         maxRetryDelay,
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
	Timestamp      string         `json:"timestamp"`
	Model          string         `json:"model"`
	Kind           string         `json:"kind"`
	TokenUsage     tokenUsage     `json:"tokenUsage"`
	ChargedCents   jsontext.Value `json:"chargedCents"`
	CursorTokenFee jsontext.Value `json:"cursorTokenFee"`
	UserID         string         `json:"userId"`
	UserEmail      string         `json:"userEmail"`
	IsHeadless     bool           `json:"isHeadless"`
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

func parseOptionalCents(value jsontext.Value) (money.Money, error) {
	if len(value) == 0 || value.Kind() == jsontext.KindNull {
		return money.Money{}, nil
	}
	parsed, err := money.ParseCents(string(value))
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
		return time.Time{}, errors.New("empty timestamp")
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
