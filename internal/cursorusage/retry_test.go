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

// retryNotice is one OnRetry call.
type retryNotice struct {
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
	client.OnRetry = func(err error, wait time.Duration) {
		notices = append(notices, retryNotice{err: err, wait: wait})
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

func TestFetchAllUsageEventsRetriesTransientFailures(t *testing.T) {
	tests := []struct {
		name       string
		failure    apiResponse
		wantStatus int
		// The wait before the retry falls in [minWait, maxWait].
		minWait time.Duration
		maxWait time.Duration
	}{
		{
			name: "rate limit honors Retry-After",
			failure: apiResponse{
				status: http.StatusTooManyRequests, retryAfter: "60",
			},
			wantStatus: http.StatusTooManyRequests,
			minWait:    60 * time.Second,
			maxWait:    60 * time.Second,
		},
		{
			name: "excessive Retry-After is capped",
			failure: apiResponse{
				status: http.StatusTooManyRequests, retryAfter: "3600",
			},
			wantStatus: http.StatusTooManyRequests,
			minWait:    maxRetryDelay,
			maxWait:    maxRetryDelay,
		},
		{
			name:       "rate limit without Retry-After backs off",
			failure:    apiResponse{status: http.StatusTooManyRequests},
			wantStatus: http.StatusTooManyRequests,
			minWait:    time.Second,
			maxWait:    3 * time.Second,
		},
		{
			name:       "server error backs off",
			failure:    apiResponse{status: http.StatusServiceUnavailable},
			wantStatus: http.StatusServiceUnavailable,
			minWait:    time.Second,
			maxWait:    3 * time.Second,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client, api, notices := newFakeAPIClient(t, func(call int) apiResponse {
					if call == 0 {
						return tc.failure
					}
					return apiResponse{status: http.StatusOK, events: 2, total: 2}
				})

				events, err := client.FetchAllUsageEvents(t.Context(), onePageQuery())
				require.NoError(t, err)
				assert.Len(t, events, 2)

				requests := api.recorded()
				require.Len(t, requests, 2)
				assert.Equal(t, 1, requests[0].page)
				assert.Equal(t, 1, requests[1].page, "the failed page is requested again")
				gap := requests[1].at.Sub(requests[0].at)
				assert.GreaterOrEqual(t, gap, tc.minWait)
				assert.LessOrEqual(t, gap, tc.maxWait)

				require.Len(t, *notices, 1)
				notice := (*notices)[0]
				assert.Equal(t, gap, notice.wait)
				statusErr, ok := errors.AsType[*StatusError](notice.err)
				require.True(t, ok, "OnRetry receives the API status error")
				assert.Equal(t, tc.wantStatus, statusErr.StatusCode)
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
				statusErr, ok := errors.AsType[*StatusError](err)
				require.True(t, ok)
				assert.Equal(t, status, statusErr.StatusCode)
				assert.Len(t, api.recorded(), 1)
				assert.Empty(t, *notices)
			})
		})
	}
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
		statusErr, ok := errors.AsType[*StatusError](err)
		require.True(t, ok)
		assert.Equal(t, http.StatusTooManyRequests, statusErr.StatusCode)

		assert.Len(t, api.recorded(), maxPageAttempts)
		assert.Len(t, *notices, maxPageAttempts-1)
		assert.Equal(t,
			time.Duration(maxPageAttempts-1)*60*time.Second, time.Since(start),
		)
	})
}

func TestFetchAllUsageEventsStopsWaitingWhenCanceled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, api, _ := newFakeAPIClient(t, func(int) apiResponse {
			return apiResponse{
				status: http.StatusTooManyRequests, retryAfter: "60",
			}
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() {
			time.Sleep(10 * time.Second)
			cancel()
		}()
		start := time.Now()

		_, err := client.FetchAllUsageEvents(ctx, onePageQuery())
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, 10*time.Second, time.Since(start))
		assert.Len(t, api.recorded(), 1)
	})
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
				for i := 1; i < len(requests); i++ {
					assert.Equal(t, i+1, requests[i].page)
					assert.Equal(t, tc.wantGap, requests[i].at.Sub(requests[i-1].at))
				}
			})
		})
	}
}
