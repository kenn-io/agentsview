package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/insight"
	"go.kenn.io/agentsview/internal/server"
)

type listInsightsResponse struct {
	Insights []db.Insight `json:"insights"`
}

type failFirstWriteRecorder struct {
	header  http.Header
	writes  int
	status  int
	flushed bool
}

func newFailFirstWriteRecorder() *failFirstWriteRecorder {
	return &failFirstWriteRecorder{
		header: make(http.Header),
		status: http.StatusOK,
	}
}

func (f *failFirstWriteRecorder) Header() http.Header {
	return f.header
}

func (f *failFirstWriteRecorder) WriteHeader(statusCode int) {
	f.status = statusCode
}

func (f *failFirstWriteRecorder) Write(b []byte) (int, error) {
	f.writes++
	if f.writes == 1 {
		return 0, io.ErrClosedPipe
	}
	return len(b), nil
}

func (f *failFirstWriteRecorder) Flush() {
	f.flushed = true
}

func TestListInsights(t *testing.T) {
	tests := []struct {
		name       string
		seed       func(t *testing.T, te *testEnv)
		path       string
		wantStatus int
		wantCount  int
		wantBody   string
	}{
		{
			name:       "Empty",
			seed:       func(t *testing.T, te *testEnv) {},
			path:       "/api/v1/insights",
			wantStatus: http.StatusOK,
			wantCount:  0,
		},
		{
			name: "WithData",
			seed: func(t *testing.T, te *testEnv) {
				te.seedInsight(t, "daily_activity", "2025-01-15", new("my-app"))
				te.seedInsight(t, "daily_activity", "2025-01-15", new("other-app"))
				te.seedInsight(t, "agent_analysis", "2025-01-15", nil)
			},
			path:       "/api/v1/insights",
			wantStatus: http.StatusOK,
			wantCount:  3,
		},
		{
			name: "TypeFilter",
			seed: func(t *testing.T, te *testEnv) {
				te.seedInsight(t, "daily_activity", "2025-01-15", new("my-app"))
				te.seedInsight(t, "agent_analysis", "2025-01-15", nil)
			},
			path:       "/api/v1/insights?type=daily_activity",
			wantStatus: http.StatusOK,
			wantCount:  1,
		},
		{
			name: "ReturnsAll",
			seed: func(t *testing.T, te *testEnv) {
				te.seedInsight(t, "daily_activity", "2025-01-15", new("my-app"))
				te.seedInsight(t, "daily_activity", "2025-01-16", new("my-app"))
			},
			path:       "/api/v1/insights",
			wantStatus: http.StatusOK,
			wantCount:  2,
		},
		{
			name:       "InvalidType",
			seed:       func(t *testing.T, te *testEnv) {},
			path:       "/api/v1/insights?type=invalid",
			wantStatus: http.StatusBadRequest,
			wantBody:   "invalid type",
		},
		{
			name:       "ReversedDateRange",
			seed:       func(t *testing.T, te *testEnv) {},
			path:       "/api/v1/insights?date_from=2026-06-17&date_to=2026-06-16",
			wantStatus: http.StatusBadRequest,
			wantBody:   "date_from must not be after date_to",
		},
		{
			// A non-date value is rejected with 400 before the handler runs
			// (huma's format:"date" query validation), so only the status is
			// asserted; the body message is owned by the framework.
			name:       "InvalidDateFrom",
			seed:       func(t *testing.T, te *testEnv) {},
			path:       "/api/v1/insights?date_from=not-a-date",
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			te := setup(t)
			tt.seed(t, te)

			w := te.get(t, tt.path)
			assertStatus(t, w, tt.wantStatus)

			if tt.wantBody != "" {
				assertBodyContains(t, w, tt.wantBody)
			}

			if tt.wantStatus == http.StatusOK {
				r := decode[listInsightsResponse](t, w)
				require.Len(t, r.Insights, tt.wantCount)
			}
		})
	}
}

func TestGetInsight_Found(t *testing.T) {
	te := setup(t)

	id := te.seedInsight(t, "daily_activity", "2025-01-15",
		new("my-app"))

	w := te.get(t, fmt.Sprintf("/api/v1/insights/%d", id))
	assertStatus(t, w, http.StatusOK)

	r := decode[db.Insight](t, w)
	require.Equal(t, id, r.ID)
	assert.Equal(t, "daily_activity", r.Type)
}

func TestGenerateInsight_Validation(t *testing.T) {
	tests := []struct {
		name     string
		payload  string
		wantBody string
	}{
		{"InvalidType", `{"type":"bad","date_from":"2025-01-15","date_to":"2025-01-15"}`, ""},
		{"InvalidDateFrom", `{"type":"daily_activity","date_from":"bad","date_to":"2025-01-15"}`, "date_from"},
		{"InvalidDateTo", `{"type":"daily_activity","date_from":"2025-01-15","date_to":"bad"}`, "date_to"},
		{"DateToBeforeDateFrom", `{"type":"daily_activity","date_from":"2025-01-16","date_to":"2025-01-15"}`, "date_to must be"},
		{"InvalidJSON", `{bad json`, ""},
		{"InvalidAgent", `{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15","agent":"gpt"}`, "invalid agent"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			te := setup(t)
			w := te.post(t, "/api/v1/insights/generate", tt.payload)

			assertStatus(t, w, http.StatusBadRequest)
			if tt.wantBody != "" {
				assertBodyContains(t, w, tt.wantBody)
			}
		})
	}
}

func TestGenerateInsight_DefaultAgent(t *testing.T) {
	stubGen := func(
		_ context.Context, agent, _ string,
	) (insight.Result, error) {
		assert.Equal(t, "claude", agent, "expected default agent claude")
		return insight.Result{}, fmt.Errorf("stub: no CLI")
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateFunc(stubGen),
	})

	w := te.post(t, "/api/v1/insights/generate",
		`{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15"}`)
	assertStatus(t, w, http.StatusOK)
	assertBodyContains(t, w, "event: error")
	assertBodyContains(t, w, "stub: no CLI")
}

func TestGenerateInsight_ErrorMessageStripsStderr(t *testing.T) {
	stubGen := func(
		_ context.Context, _, _ string,
	) (insight.Result, error) {
		return insight.Result{}, fmt.Errorf(
			"claude CLI failed: exit status 1\nstderr: some debug output",
		)
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateFunc(stubGen),
	})

	w := te.post(t, "/api/v1/insights/generate",
		`{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15"}`)
	assertStatus(t, w, http.StatusOK)
	body := w.Body.String()
	require.Contains(t, body, "claude CLI failed: exit status 1")
	require.NotContains(t, body, "some debug output",
		"expected stderr to be stripped from client message")
}

func TestGenerateInsight_ErrorMessageStripsRaw(t *testing.T) {
	stubGen := func(
		_ context.Context, _, _ string,
	) (insight.Result, error) {
		return insight.Result{}, fmt.Errorf(
			"claude returned empty result\nraw: {\"type\":\"result\",\"result\":\"\"}",
		)
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateFunc(stubGen),
	})

	w := te.post(t, "/api/v1/insights/generate",
		`{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15"}`)
	assertStatus(t, w, http.StatusOK)
	body := w.Body.String()
	require.Contains(t, body, "claude returned empty result")
	require.NotContains(t, body, `"type":"result"`,
		"expected raw payload to be stripped from client message")
}

func TestGenerateInsight_InitialStatusWriteFailureSkipsGeneration(t *testing.T) {
	var called atomic.Bool
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateStreamFunc(func(
			_ context.Context, _ string, _ string, _ insight.LogFunc,
		) (insight.Result, error) {
			called.Store(true)
			return insight.Result{Content: "should not run"}, nil
		}),
	})

	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/insights/generate",
		strings.NewReader(
			`{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15","agent":"claude"}`,
		),
	)
	req.Header.Set("Content-Type", "application/json")

	w := newFailFirstWriteRecorder()
	te.handler.ServeHTTP(w, req)

	require.False(t, called.Load(),
		"generation should not run when initial SSE status write fails")
}

func TestGenerateInsight_StreamsLogs(t *testing.T) {
	stubGen := func(
		_ context.Context, _ string, _ string, onLog insight.LogFunc,
	) (insight.Result, error) {
		onLog(insight.LogEvent{
			Stream: "stdout",
			Line:   `{"type":"system","status":"ready"}`,
		})
		onLog(insight.LogEvent{
			Stream: "stderr",
			Line:   "rate limit warning",
		})
		return insight.Result{
			Content: "# Insight",
			Agent:   "claude",
			Model:   "test-model",
		}, nil
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateStreamFunc(stubGen),
	})

	w := te.post(t, "/api/v1/insights/generate",
		`{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15","agent":"claude"}`)
	assertStatus(t, w, http.StatusOK)

	events := parseSSE(w.Body.String())
	require.GreaterOrEqual(t, len(events), 4, "expected >=4 SSE events: %s", w.Body.String())
	require.Equal(t, "status", events[0].Event, "first event")
	require.Equal(t, "log", events[1].Event, "events: %#v", events)
	require.Equal(t, "log", events[2].Event, "events: %#v", events)
	require.Equal(t, "done", events[len(events)-1].Event, "last event")

	var log1 insight.LogEvent
	require.NoError(t, json.Unmarshal([]byte(events[1].Data), &log1))
	require.Equal(t, "stdout", log1.Stream)

	var log2 insight.LogEvent
	require.NoError(t, json.Unmarshal([]byte(events[2].Data), &log2))
	require.Equal(t, "stderr", log2.Stream)
}

type slowFlushRecorder struct {
	*httptest.ResponseRecorder
	delay time.Duration
	mu    sync.Mutex
}

func (f *slowFlushRecorder) Write(
	b []byte,
) (int, error) {
	time.Sleep(f.delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ResponseRecorder.Write(b)
}

func (f *slowFlushRecorder) Flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ResponseRecorder.Flush()
}

func (f *slowFlushRecorder) BodyString() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Body.String()
}

type slowLogRecorder struct {
	*httptest.ResponseRecorder
	delay time.Duration
	mu    sync.Mutex
}

func (f *slowLogRecorder) Write(
	b []byte,
) (int, error) {
	if strings.HasPrefix(string(b), "event: log\n") {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ResponseRecorder.Write(b)
}

func (f *slowLogRecorder) Flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ResponseRecorder.Flush()
}

func (f *slowLogRecorder) BodyString() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Body.String()
}

type blockingLogRecorder struct {
	*httptest.ResponseRecorder
	release <-chan struct{}
	mu      sync.Mutex
}

func (f *blockingLogRecorder) Write(
	b []byte,
) (int, error) {
	if strings.HasPrefix(string(b), "event: log\n") {
		<-f.release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ResponseRecorder.Write(b)
}

func (f *blockingLogRecorder) Flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ResponseRecorder.Flush()
}

func (f *blockingLogRecorder) BodyString() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Body.String()
}

type firstLogDelayRecorder struct {
	*httptest.ResponseRecorder
	delay time.Duration
	once  sync.Once
	mu    sync.Mutex
}

func (f *firstLogDelayRecorder) Write(
	b []byte,
) (int, error) {
	if strings.HasPrefix(string(b), "event: log\n") {
		f.once.Do(func() {
			time.Sleep(f.delay)
		})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ResponseRecorder.Write(b)
}

func (f *firstLogDelayRecorder) Flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ResponseRecorder.Flush()
}

func (f *firstLogDelayRecorder) BodyString() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Body.String()
}

type deadlineAwareBlockingLogRecorder struct {
	*httptest.ResponseRecorder
	handlerReturned     <-chan struct{}
	postReturnWrites    atomic.Int32
	postReturnAttempted chan struct{}
	deadlineUpdates     chan struct{}
	mu                  sync.Mutex
	writeDeadline       time.Time
}

func newDeadlineAwareBlockingLogRecorder(
	handlerReturned <-chan struct{},
) *deadlineAwareBlockingLogRecorder {
	return &deadlineAwareBlockingLogRecorder{
		ResponseRecorder:    httptest.NewRecorder(),
		handlerReturned:     handlerReturned,
		postReturnAttempted: make(chan struct{}, 1),
		deadlineUpdates:     make(chan struct{}, 1),
	}
}

func (f *deadlineAwareBlockingLogRecorder) SetWriteDeadline(t time.Time) error {
	f.mu.Lock()
	f.writeDeadline = t
	f.mu.Unlock()
	select {
	case f.deadlineUpdates <- struct{}{}:
	default:
	}
	return nil
}

func (f *deadlineAwareBlockingLogRecorder) Write(
	b []byte,
) (int, error) {
	if f.handlerReturned != nil {
		select {
		case <-f.handlerReturned:
			f.postReturnWrites.Add(1)
			select {
			case f.postReturnAttempted <- struct{}{}:
			default:
			}
		default:
		}
	}

	if strings.HasPrefix(string(b), "event: log\n") {
		for {
			f.mu.Lock()
			deadline := f.writeDeadline
			f.mu.Unlock()
			if !deadline.IsZero() && !deadline.After(time.Now()) {
				return 0, os.ErrDeadlineExceeded
			}
			<-f.deadlineUpdates
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ResponseRecorder.Write(b)
}

func (f *deadlineAwareBlockingLogRecorder) Flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ResponseRecorder.Flush()
}

func (f *deadlineAwareBlockingLogRecorder) PostReturnWrites() int32 {
	return f.postReturnWrites.Load()
}

func (f *deadlineAwareBlockingLogRecorder) PostReturnAttempted() <-chan struct{} {
	return f.postReturnAttempted
}

func (f *deadlineAwareBlockingLogRecorder) BodyString() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Body.String()
}

func TestGenerateInsight_LogDropSummaryAndCompletion(t *testing.T) {
	stubGen := func(
		_ context.Context, _ string, _ string, onLog insight.LogFunc,
	) (insight.Result, error) {
		for i := range 5000 {
			onLog(insight.LogEvent{
				Stream: "stdout",
				Line:   fmt.Sprintf("line-%d", i),
			})
		}
		return insight.Result{
			Content: "# Insight",
			Agent:   "claude",
		}, nil
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateStreamFunc(stubGen),
	})

	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/insights/generate",
		strings.NewReader(
			`{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15","agent":"claude"}`,
		),
	)
	req.Header.Set("Content-Type", "application/json")
	w := &slowFlushRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		delay:            4 * time.Millisecond,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		te.handler.ServeHTTP(w, req)
	}()

	select {
	case <-done:
	case <-time.After(8 * time.Second):
		require.Fail(t, "timed out waiting for generate handler")
	}

	assertStatus(t, w.ResponseRecorder, http.StatusOK)
	events := parseSSE(w.BodyString())

	foundDone := false
	foundDropSummary := false
	for _, ev := range events {
		if ev.Event == "done" {
			foundDone = true
		}
		if ev.Event != "log" {
			continue
		}
		var line insight.LogEvent
		if json.Unmarshal([]byte(ev.Data), &line) != nil {
			continue
		}
		if line.Stream == "stderr" &&
			strings.Contains(line.Line, "dropped ") &&
			strings.Contains(line.Line, "slow client") {
			foundDropSummary = true
		}
	}
	require.True(t, foundDropSummary,
		"expected dropped-log summary event, got %d events", len(events))
	require.True(t, foundDone, "expected done event")
}

func TestGenerateInsight_LogDrainTimeoutReturnsWithoutHang(t *testing.T) {
	stubGen := func(
		_ context.Context, _ string, _ string, onLog insight.LogFunc,
	) (insight.Result, error) {
		for i := range 10 {
			onLog(insight.LogEvent{
				Stream: "stdout",
				Line:   fmt.Sprintf("slow-line-%d", i),
			})
		}
		return insight.Result{
			Content: "# Insight",
			Agent:   "claude",
		}, nil
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateStreamFunc(stubGen),
	})

	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/insights/generate",
		strings.NewReader(
			`{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15","agent":"claude"}`,
		),
	)
	req.Header.Set("Content-Type", "application/json")
	w := &slowLogRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		delay:            5 * time.Second,
	}

	started := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		te.handler.ServeHTTP(w, req)
	}()

	select {
	case <-done:
	case <-time.After(12 * time.Second):
		require.Fail(t, "timed out waiting for generate handler completion")
	}
	require.LessOrEqual(t, time.Since(started), 7*time.Second,
		"handler should return within bounded timeout handling")

	assertStatus(t, w.ResponseRecorder, http.StatusOK)
	events := parseSSE(w.BodyString())
	for _, ev := range events {
		require.NotEqual(t, "done", ev.Event,
			"did not expect done event when timeout path is triggered")
	}
}

func TestGenerateInsight_LogDrainTimeoutReportsBufferedDrops(t *testing.T) {
	stubGen := func(
		_ context.Context, _ string, _ string, onLog insight.LogFunc,
	) (insight.Result, error) {
		for i := range 10 {
			onLog(insight.LogEvent{
				Stream: "stdout",
				Line:   fmt.Sprintf("slow-line-%d", i),
			})
		}
		return insight.Result{
			Content: "# Insight",
			Agent:   "claude",
		}, nil
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateStreamFunc(stubGen),
	})

	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/insights/generate",
		strings.NewReader(
			`{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15","agent":"claude"}`,
		),
	)
	req.Header.Set("Content-Type", "application/json")
	w := &firstLogDelayRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		delay:            2200 * time.Millisecond,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		te.handler.ServeHTTP(w, req)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		require.Fail(t, "timed out waiting for generate handler completion")
	}

	assertStatus(t, w.ResponseRecorder, http.StatusOK)
	events := parseSSE(w.BodyString())
	foundTimeoutError := false
	foundDropSummary := false
	for _, ev := range events {
		require.NotEqual(t, "done", ev.Event,
			"did not expect done event when timeout path is triggered")
		if ev.Event == "error" &&
			strings.Contains(ev.Data, "timed out before completion") {
			foundTimeoutError = true
		}
		if ev.Event != "log" {
			continue
		}
		var line insight.LogEvent
		if json.Unmarshal([]byte(ev.Data), &line) != nil {
			continue
		}
		if line.Stream != "stderr" ||
			!strings.HasPrefix(line.Line, "dropped ") ||
			!strings.Contains(line.Line, "log stream timeout") {
			continue
		}
		parts := strings.SplitN(line.Line, " ", 3)
		if len(parts) < 3 {
			continue
		}
		dropped, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		// 10 events were enqueued; timeout truncation should account
		// for most buffered entries that were never flushed.
		require.GreaterOrEqual(t, dropped, 8,
			"expected timeout drop summary >=8 (%q)", line.Line)
		foundDropSummary = true
	}
	require.True(t, foundTimeoutError,
		"expected timeout error event, got %d events", len(events))
	require.True(t, foundDropSummary,
		"expected timeout-aware drop summary, got %d events", len(events))
}

func TestGenerateInsight_LogDrainTimeoutBoundedWhenWriterStuck(t *testing.T) {
	stubGen := func(
		_ context.Context, _ string, _ string, onLog insight.LogFunc,
	) (insight.Result, error) {
		onLog(insight.LogEvent{Stream: "stdout", Line: "stuck-line"})
		return insight.Result{Content: "# Insight", Agent: "claude"}, nil
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateStreamFunc(stubGen),
	})

	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/insights/generate",
		strings.NewReader(
			`{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15","agent":"claude"}`,
		),
	)
	req.Header.Set("Content-Type", "application/json")
	release := make(chan struct{})
	w := &blockingLogRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		release:          release,
	}

	started := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		te.handler.ServeHTTP(w, req)
	}()

	select {
	case <-done:
	case <-time.After(7 * time.Second):
		require.Fail(t, "timed out waiting for bounded timeout behavior")
	}
	elapsed := time.Since(started)
	require.LessOrEqual(t, elapsed, 6*time.Second,
		"handler returned too slowly for stuck writer path")
	close(release)

	assertStatus(t, w.ResponseRecorder, http.StatusOK)
	events := parseSSE(w.BodyString())
	for _, ev := range events {
		require.NotEqual(t, "done", ev.Event,
			"did not expect done event on stuck writer timeout path")
	}
}

func TestGenerateInsight_LogDrainTimeoutForceUnblocksAndNoPostReturnWrites(t *testing.T) {
	stubGen := func(
		_ context.Context, _ string, _ string, onLog insight.LogFunc,
	) (insight.Result, error) {
		onLog(insight.LogEvent{Stream: "stdout", Line: "force-unblock-line"})
		return insight.Result{Content: "# Insight", Agent: "claude"}, nil
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateStreamFunc(stubGen),
	})

	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/insights/generate",
		strings.NewReader(
			`{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15","agent":"claude"}`,
		),
	)
	req.Header.Set("Content-Type", "application/json")
	handlerReturned := make(chan struct{})
	w := newDeadlineAwareBlockingLogRecorder(handlerReturned)

	done := make(chan struct{})
	go func() {
		defer close(done)
		te.handler.ServeHTTP(w, req)
		close(handlerReturned)
	}()

	select {
	case <-done:
	case <-time.After(8 * time.Second):
		require.Fail(t, "timed out waiting for forced-unblock completion")
	}

	select {
	case <-w.PostReturnAttempted():
		require.Fail(t, "expected no writes after handler return")
	case <-time.After(300 * time.Millisecond):
	}
	require.Zero(t, w.PostReturnWrites(), "expected no writes after handler return")

	assertStatus(t, w.ResponseRecorder, http.StatusOK)
	events := parseSSE(w.BodyString())
	foundTimeoutError := false
	for _, ev := range events {
		require.NotEqual(t, "done", ev.Event,
			"did not expect done event on forced-unblock timeout path")
		if ev.Event == "error" &&
			strings.Contains(ev.Data, "timed out before completion") {
			foundTimeoutError = true
		}
	}
	require.True(t, foundTimeoutError, "expected timeout error event")
}

func TestDeleteInsight_Found(t *testing.T) {
	te := setup(t)

	id := te.seedInsight(t, "daily_activity", "2025-01-15",
		new("my-app"))

	w := te.del(t, fmt.Sprintf("/api/v1/insights/%d", id))
	assertStatus(t, w, http.StatusNoContent)

	// Verify it's gone.
	w = te.get(t, fmt.Sprintf("/api/v1/insights/%d", id))
	assertStatus(t, w, http.StatusNotFound)
}

func TestInsight_ResourceErrors(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		status int
	}{
		{"Get_NotFound", http.MethodGet, "/api/v1/insights/99999", http.StatusNotFound},
		{"Get_InvalidID", http.MethodGet, "/api/v1/insights/abc", http.StatusBadRequest},
		{"Delete_NotFound", http.MethodDelete, "/api/v1/insights/99999", http.StatusNotFound},
		{"Delete_InvalidID", http.MethodDelete, "/api/v1/insights/abc", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			te := setup(t)
			if tt.method == http.MethodGet {
				w := te.get(t, tt.path)
				assertStatus(t, w, tt.status)
			} else {
				w := te.del(t, tt.path)
				assertStatus(t, w, tt.status)
			}
		})
	}
}

// --- helpers ---

func (te *testEnv) seedInsight(
	t *testing.T,
	typ, date string,
	project *string,
) int64 {
	t.Helper()
	id, err := te.db.InsertInsight(db.Insight{
		Type:     typ,
		DateFrom: date,
		DateTo:   date,
		Project:  project,
		Agent:    "claude",
		Content:  "Test insight content",
	})
	require.NoError(t, err)
	return id
}
