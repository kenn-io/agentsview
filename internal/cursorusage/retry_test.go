package cursorusage

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// apiResponse is one canned reply from the fake Admin API.
type apiResponse struct {
	// err, when set, fails the request in transport instead of answering.
	err        error
	status     int
	retryAfter string
	// events is the number of usage events on a 200 page.
	events int
	total  int
}

// apiRequest records when a page was requested, on the synctest clock.
type apiRequest struct {
	page int
	at   time.Time
}

// fakeAPI serves filtered-usage-events from memory so retry and pacing waits
// can run on the synctest clock without a network listener.
type fakeAPI struct {
	t       *testing.T
	respond func(call int) apiResponse

	mu       sync.Mutex
	requests []apiRequest
}

func (f *fakeAPI) RoundTrip(r *http.Request) (*http.Response, error) {
	var body struct {
		Page int `json:"page"`
	}
	if !assert.NoError(f.t, json.UnmarshalRead(r.Body, &body)) {
		return nil, errors.New("undecodable request")
	}
	f.mu.Lock()
	call := len(f.requests)
	f.requests = append(f.requests, apiRequest{page: body.Page, at: time.Now()})
	f.mu.Unlock()

	reply := f.respond(call)
	if reply.err != nil {
		return nil, reply.err
	}
	header := http.Header{}
	if reply.retryAfter != "" {
		header.Set("Retry-After", reply.retryAfter)
	}
	var payload bytes.Buffer
	if reply.status == http.StatusOK {
		events := make([]map[string]any, reply.events)
		for i := range events {
			events[i] = map[string]any{
				"timestamp": strconv.Itoa(1748700000000 + i),
				"model":     "gpt-5.5-high",
				"kind":      "USAGE_EVENT_KIND_USAGE_BASED",
			}
		}
		require.NoError(f.t, json.MarshalWrite(&payload, map[string]any{
			"totalUsageEventsCount": reply.total,
			"usageEvents":           events,
		}))
	}
	return &http.Response{
		StatusCode: reply.status,
		Status:     strconv.Itoa(reply.status) + " " + http.StatusText(reply.status),
		Header:     header,
		Body:       io.NopCloser(&payload),
		Request:    r,
	}, nil
}

func (f *fakeAPI) recorded() []apiRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]apiRequest(nil), f.requests...)
}

// gaps returns the time between the starts of consecutive requests.
func gaps(requests []apiRequest) []time.Duration {
	out := make([]time.Duration, 0, len(requests))
	for i := 1; i < len(requests); i++ {
		out = append(out, requests[i].at.Sub(requests[i-1].at))
	}
	return out
}

// retryNotice is one OnRetry call.
type retryNotice struct {
	page int
	err  error
	wait time.Duration
}

func newFakeAPIClient(
	t *testing.T, respond func(call int) apiResponse,
) (*Client, *fakeAPI, *[]retryNotice) {
	t.Helper()
	api := &fakeAPI{t: t, respond: respond}
	client := NewClientWithBaseURL("https://cursor.test", "cursor-key")
	client.HTTPClient = &http.Client{Transport: api}
	var notices []retryNotice
	client.OnRetry = func(page int, err error, wait time.Duration) {
		notices = append(notices, retryNotice{page: page, err: err, wait: wait})
	}
	return client, api, &notices
}

func onePageQuery() Query {
	return Query{
		StartDate: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		EndDate:   time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC),
		PageSize:  10,
	}
}

// waitRange bounds one wait between a failed attempt and its retry.
type waitRange struct{ min, max time.Duration }

func TestFetchAllUsageEventsRetriesTransientFailures(t *testing.T) {
	tests := []struct {
		name       string
		failure    apiResponse
		wantStatus int
		// waits bounds each wait before a retry; the page fails once per
		// entry and then succeeds.
		waits []waitRange
	}{
		{
			name: "rate limit honors Retry-After",
			failure: apiResponse{
				status: http.StatusTooManyRequests, retryAfter: "60",
			},
			wantStatus: http.StatusTooManyRequests,
			waits:      []waitRange{{60 * time.Second, 60 * time.Second}},
		},
		{
			name: "excessive Retry-After is capped",
			failure: apiResponse{
				status: http.StatusTooManyRequests, retryAfter: "3600",
			},
			wantStatus: http.StatusTooManyRequests,
			waits:      []waitRange{{maxRetryDelay, maxRetryDelay}},
		},
		{
			name: "server error honors Retry-After",
			failure: apiResponse{
				status: http.StatusServiceUnavailable, retryAfter: "5",
			},
			wantStatus: http.StatusServiceUnavailable,
			waits:      []waitRange{{5 * time.Second, 5 * time.Second}},
		},
		{
			// Without Retry-After the waits grow from about 2s to about
			// 4s, each randomized by half, so the second wait is longer
			// than the one-second request interval alone would give.
			name:       "rate limit without Retry-After backs off",
			failure:    apiResponse{status: http.StatusTooManyRequests},
			wantStatus: http.StatusTooManyRequests,
			waits: []waitRange{
				{time.Second, 3 * time.Second},
				{2 * time.Second, 6 * time.Second},
			},
		},
		{
			name:       "server error without Retry-After backs off",
			failure:    apiResponse{status: http.StatusServiceUnavailable},
			wantStatus: http.StatusServiceUnavailable,
			waits: []waitRange{
				{time.Second, 3 * time.Second},
				{2 * time.Second, 6 * time.Second},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client, api, notices := newFakeAPIClient(t, func(call int) apiResponse {
					if call < len(tc.waits) {
						return tc.failure
					}
					return apiResponse{status: http.StatusOK, events: 2, total: 2}
				})

				events, err := client.FetchAllUsageEvents(t.Context(), onePageQuery())
				require.NoError(t, err)
				assert.Len(t, events, 2)

				requests := api.recorded()
				require.Len(t, requests, len(tc.waits)+1)
				for _, req := range requests {
					assert.Equal(t, 1, req.page, "the failed page is requested again")
				}
				require.Len(t, *notices, len(tc.waits))
				for i, gap := range gaps(requests) {
					assert.GreaterOrEqual(t, gap, tc.waits[i].min, "wait %d", i)
					assert.LessOrEqual(t, gap, tc.waits[i].max, "wait %d", i)

					notice := (*notices)[i]
					assert.Equal(t, 1, notice.page)
					assert.Equal(t, gap, notice.wait)
					statusErr, ok := errors.AsType[*statusError](notice.err)
					require.True(t, ok, "OnRetry receives the API status error")
					assert.Equal(t, tc.wantStatus, statusErr.statusCode)
				}
			})
		})
	}
}

func TestFetchAllUsageEventsFailsFastOnPermanentStatus(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
	} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client, api, notices := newFakeAPIClient(t, func(int) apiResponse {
					return apiResponse{status: status}
				})

				_, err := client.FetchAllUsageEvents(t.Context(), onePageQuery())
				require.Error(t, err)
				statusErr, ok := errors.AsType[*statusError](err)
				require.True(t, ok)
				assert.Equal(t, status, statusErr.statusCode)
				assert.Len(t, api.recorded(), 1)
				assert.Empty(t, *notices)
			})
		})
	}
}

func TestFetchAllUsageEventsDoesNotRetryTransportErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		errConnection := errors.New("connection reset by peer")
		client, api, notices := newFakeAPIClient(t, func(int) apiResponse {
			return apiResponse{err: errConnection}
		})

		_, err := client.FetchAllUsageEvents(t.Context(), onePageQuery())
		require.ErrorIs(t, err, errConnection)
		assert.Len(t, api.recorded(), 1)
		assert.Empty(t, *notices)
	})
}

func TestFetchAllUsageEventsGivesUpAfterMaxAttempts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, api, notices := newFakeAPIClient(t, func(int) apiResponse {
			return apiResponse{
				status: http.StatusTooManyRequests, retryAfter: "60",
			}
		})
		start := time.Now()

		_, err := client.FetchAllUsageEvents(t.Context(), onePageQuery())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "page 1")
		assert.Contains(t, err.Error(), "giving up after 5 attempts")
		statusErr, ok := errors.AsType[*statusError](err)
		require.True(t, ok)
		assert.Equal(t, http.StatusTooManyRequests, statusErr.statusCode)

		assert.Len(t, api.recorded(), maxPageAttempts)
		assert.Len(t, *notices, maxPageAttempts-1)
		assert.Equal(t,
			time.Duration(maxPageAttempts-1)*60*time.Second, time.Since(start),
		)
	})
}

func TestFetchAllUsageEventsStopsWaitingWhenCanceled(t *testing.T) {
	tests := []struct {
		name string
		// respond keeps the fetch waiting after its first request.
		respond  func(call int) apiResponse
		query    func() Query
		cancelAt time.Duration
	}{
		{
			name: "during a Retry-After wait",
			respond: func(int) apiResponse {
				return apiResponse{
					status: http.StatusTooManyRequests, retryAfter: "60",
				}
			},
			query:    onePageQuery,
			cancelAt: 10 * time.Second,
		},
		{
			name: "during the wait between pages",
			respond: func(int) apiResponse {
				return apiResponse{status: http.StatusOK, events: 1, total: 3}
			},
			query: func() Query {
				q := onePageQuery()
				q.PageSize = 1
				return q
			},
			cancelAt: defaultRequestInterval / 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client, api, _ := newFakeAPIClient(t, tc.respond)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				go func() {
					time.Sleep(tc.cancelAt)
					cancel()
				}()
				start := time.Now()

				_, err := client.FetchAllUsageEvents(ctx, tc.query())
				require.ErrorIs(t, err, context.Canceled)
				assert.Equal(t, tc.cancelAt, time.Since(start))
				assert.Len(t, api.recorded(), 1)
			})
		})
	}
}

func TestFetchAllUsageEventsPacesPageRequests(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		wantGap  time.Duration
	}{
		{name: "default interval", interval: defaultRequestInterval, wantGap: time.Second},
		{name: "pacing disabled", interval: 0, wantGap: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client, api, _ := newFakeAPIClient(t, func(int) apiResponse {
					return apiResponse{status: http.StatusOK, events: 1, total: 3}
				})
				client.RequestInterval = tc.interval
				q := onePageQuery()
				q.PageSize = 1

				events, err := client.FetchAllUsageEvents(t.Context(), q)
				require.NoError(t, err)
				assert.Len(t, events, 3)

				requests := api.recorded()
				require.Len(t, requests, 3)
				for i, gap := range gaps(requests) {
					assert.Equal(t, i+2, requests[i+1].page)
					assert.Equal(t, tc.wantGap, gap)
				}
			})
		})
	}
}

func TestFetchAllUsageEventsPacesThePageAfterARetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, api, _ := newFakeAPIClient(t, func(call int) apiResponse {
			if call == 0 {
				return apiResponse{
					status: http.StatusTooManyRequests, retryAfter: "60",
				}
			}
			return apiResponse{status: http.StatusOK, events: 1, total: 2}
		})
		q := onePageQuery()
		q.PageSize = 1

		events, err := client.FetchAllUsageEvents(t.Context(), q)
		require.NoError(t, err)
		assert.Len(t, events, 2)

		requests := api.recorded()
		require.Len(t, requests, 3)
		assert.Equal(t, []int{1, 1, 2},
			[]int{requests[0].page, requests[1].page, requests[2].page})
		assert.Equal(t,
			[]time.Duration{60 * time.Second, defaultRequestInterval},
			gaps(requests),
			"page 2 waits a full interval after page 1's retry",
		)
	})
}
