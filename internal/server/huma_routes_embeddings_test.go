package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/vector"
)

// activateCall records one Activate or Retire invocation on
// fakeEmbeddingsManager, for assertions on what the route handlers passed
// through.
type activateCall struct {
	id    int64
	force bool
}

// fakeEmbeddingsManager is a test double for EmbeddingsManager: each method's
// return value is scripted via the corresponding field, and calls are
// recorded for assertions. Safe for concurrent use since huma may invoke
// handlers from more than one goroutine.
type fakeEmbeddingsManager struct {
	mu sync.Mutex

	startBuildErr   error
	startBuildCalls []vector.BuildRequest

	status vector.BuildStatus

	generations    []vector.GenerationInfo
	generationsErr error

	activateErr   error
	activateCalls []activateCall

	retireErr   error
	retireCalls []activateCall

	waitForBuild <-chan struct{}
}

func (f *fakeEmbeddingsManager) StartBuild(req vector.BuildRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startBuildCalls = append(f.startBuildCalls, req)
	return f.startBuildErr
}

func (f *fakeEmbeddingsManager) Status() vector.BuildStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeEmbeddingsManager) Generations(_ context.Context) ([]vector.GenerationInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.generations, f.generationsErr
}

func (f *fakeEmbeddingsManager) Activate(_ context.Context, id int64, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activateCalls = append(f.activateCalls, activateCall{id: id, force: force})
	return f.activateErr
}

func (f *fakeEmbeddingsManager) Retire(_ context.Context, id int64, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retireCalls = append(f.retireCalls, activateCall{id: id, force: force})
	return f.retireErr
}

func (f *fakeEmbeddingsManager) Wait() {
	if f.waitForBuild != nil {
		<-f.waitForBuild
	}
}

// newEmbeddingsTestServer builds a full Server (via testServer, so the SPA
// fallback and route registration match production) with m wired in as the
// embeddings manager. A nil m leaves the routes registered but unavailable.
func newEmbeddingsTestServer(t *testing.T, m EmbeddingsManager) *Server {
	t.Helper()
	var opts []Option
	if m != nil {
		opts = append(opts, WithEmbeddingsManager(m))
	}
	return testServer(t, 0, opts...)
}

func TestEmbeddingsRoutesRegisteredWhenManagerNil(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/embeddings/status", nil)

	withoutManager := newEmbeddingsTestServer(t, nil)
	_, patternWithout := withoutManager.mux.Handler(req)
	assert.NotEqual(t, "/", patternWithout)

	w := serveGet(t, withoutManager, "/api/v1/embeddings/status")
	assertRecorderStatus(t, w, http.StatusNotImplemented)

	withManager := newEmbeddingsTestServer(t, &fakeEmbeddingsManager{})
	_, patternWith := withManager.mux.Handler(req)
	assert.NotEqual(t, "/", patternWith)
}

// TestEmbeddingsUnavailableReasonReplacesGeneric501 pins that a recorded
// unavailability reason (e.g. vector serving disabled at startup because
// vectors.write.lock was held) reaches the 501 body, so CLI users see why
// the daemon cannot build and how to recover instead of the generic message.
func TestEmbeddingsUnavailableReasonReplacesGeneric501(t *testing.T) {
	reason := "vector serving is disabled for this daemon run: restart the daemon"
	srv := testServer(t, 0, WithEmbeddingsUnavailableReason(reason))

	w := serveGet(t, srv, "/api/v1/embeddings/status")
	assertRecorderStatus(t, w, http.StatusNotImplemented)
	assert.Contains(t, w.Body.String(), reason)

	generic := serveGet(t, newEmbeddingsTestServer(t, nil), "/api/v1/embeddings/status")
	assertRecorderStatus(t, generic, http.StatusNotImplemented)
	assert.Contains(t, generic.Body.String(), "embeddings manager not available")
}

func TestOpenAPIDocumentsEmbeddingsRoutesWithoutManager(t *testing.T) {
	spec := readOpenAPISpec(t, testServer(t, 0).Handler())

	for _, tt := range []struct {
		method string
		path   string
	}{
		{method: "post", path: "/api/v1/embeddings/build"},
		{method: "get", path: "/api/v1/embeddings/status"},
		{method: "get", path: "/api/v1/embeddings/generations"},
		{method: "post", path: "/api/v1/embeddings/generations/{id}/activate"},
		{method: "post", path: "/api/v1/embeddings/generations/{id}/retire"},
	} {
		requireOpenAPIOperation(t, spec, tt.method, tt.path)
	}
}

func TestEmbeddingsBuildReturnsAcceptedAndStartsBuild(t *testing.T) {
	fake := &fakeEmbeddingsManager{}
	s := newEmbeddingsTestServer(t, fake)

	w := serveJSON(t, s.mux, http.MethodPost, "/api/v1/embeddings/build",
		vector.BuildRequest{FullRebuild: true})
	assertRecorderStatus(t, w, http.StatusAccepted)

	var body struct {
		Started bool `json:"started"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.True(t, body.Started)

	require.Len(t, fake.startBuildCalls, 1)
	assert.True(t, fake.startBuildCalls[0].FullRebuild)
	assert.False(t, fake.startBuildCalls[0].RepairInvalid)
}

func TestEmbeddingsBuildHoldsIdleLeaseUntilManagerCompletes(t *testing.T) {
	buildDone := make(chan struct{})
	var completeOnce sync.Once
	complete := func() { completeOnce.Do(func() { close(buildDone) }) }
	defer complete()
	fake := &fakeEmbeddingsManager{waitForBuild: buildDone}
	idled := make(chan struct{}, 1)
	tracker := NewIdleTracker(20*time.Millisecond, func() { idled <- struct{}{} })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := testServer(t, 0, WithEmbeddingsManager(fake), WithIdleTracker(tracker))
	tracker.Touch()
	go tracker.Run(ctx)

	w := serveJSON(t, s.mux, http.MethodPost, "/api/v1/embeddings/build",
		vector.BuildRequest{})
	assertRecorderStatus(t, w, http.StatusAccepted)

	select {
	case <-idled:
		require.Fail(t, "daemon idled while an API-started embedding build was active")
	case <-time.After(100 * time.Millisecond):
	}
	complete()
	select {
	case <-idled:
	case <-time.After(time.Second):
		require.Fail(t, "daemon did not idle after the API-started build completed")
	}
}

func TestEmbeddingsRoutesSelectRecallStoreManager(t *testing.T) {
	messages := &fakeEmbeddingsManager{status: vector.BuildStatus{Done: 1}}
	recall := &fakeEmbeddingsManager{status: vector.BuildStatus{Done: 2}}
	s := testServer(t, 0,
		WithEmbeddingsManager(messages),
		WithEmbeddingsStoreManager(vector.RecallIndexSpec().Name, recall),
	)

	w := serveJSON(t, s.mux, http.MethodPost, "/api/v1/embeddings/build",
		map[string]any{
			"store": "recall", "full_rebuild": true,
			"include_automated": true,
		})
	assertRecorderStatus(t, w, http.StatusAccepted)
	assert.Empty(t, messages.startBuildCalls)
	require.Len(t, recall.startBuildCalls, 1)
	assert.True(t, recall.startBuildCalls[0].FullRebuild)
	assert.False(t, recall.startBuildCalls[0].IncludeAutomated,
		"Recall has no automated-session scope and must normalize it")

	statusResponse := serveGet(t, s, "/api/v1/embeddings/status?store=recall")
	assertRecorderStatus(t, statusResponse, http.StatusOK)
	var status vector.BuildStatus
	require.NoError(t, json.Unmarshal(statusResponse.Body.Bytes(), &status))
	assert.Equal(t, int64(2), status.Done)
}

// TestEmbeddingsBuildIncludeAutomatedDefaulting pins the tri-state contract:
// a request omitting include_automated builds with the daemon's configured
// [vector].include_automated scope (matching scheduler and CLI builds), and
// an explicit value overrides that scope in either direction.
func TestEmbeddingsBuildIncludeAutomatedDefaulting(t *testing.T) {
	tests := []struct {
		name             string
		configured       bool
		body             string
		wantIncludeAutom bool
	}{
		{"omitted uses configured true", true, `{}`, true},
		{"omitted uses configured false", false, `{}`, false},
		{"explicit false overrides configured true", true,
			`{"include_automated":false}`, false},
		{"explicit true overrides configured false", false,
			`{"include_automated":true}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeEmbeddingsManager{}
			s := testServer(t, 0,
				WithEmbeddingsManager(fake),
				WithEmbeddingsIncludeAutomatedDefault(tt.configured),
			)

			req := httptest.NewRequest(http.MethodPost, "/api/v1/embeddings/build",
				strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, req)
			assertRecorderStatus(t, w, http.StatusAccepted)

			require.Len(t, fake.startBuildCalls, 1)
			assert.Equal(t, tt.wantIncludeAutom, fake.startBuildCalls[0].IncludeAutomated)
		})
	}
}

func TestEmbeddingsBuildInvalidRequestReturnsBadRequest(t *testing.T) {
	tests := []struct {
		name string
		req  vector.BuildRequest
	}{
		{
			name: "full rebuild with repair",
			req:  vector.BuildRequest{FullRebuild: true, RepairInvalid: true},
		},
		{
			name: "backstop with repair",
			req:  vector.BuildRequest{Backstop: true, RepairInvalid: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeEmbeddingsManager{startBuildErr: fmt.Errorf(
				"%w: mutually exclusive build modes", vector.ErrInvalidBuildRequest)}
			s := newEmbeddingsTestServer(t, fake)

			w := serveJSON(t, s.mux, http.MethodPost, "/api/v1/embeddings/build", tt.req)
			assertRecorderStatus(t, w, http.StatusBadRequest)
			assert.Contains(t, w.Body.String(), "mutually exclusive")
			require.Len(t, fake.startBuildCalls, 1)
			assert.Equal(t, tt.req, fake.startBuildCalls[0])
		})
	}
}

func TestEmbeddingsBuildReturnsConflictWhenAlreadyRunning(t *testing.T) {
	fake := &fakeEmbeddingsManager{startBuildErr: vector.ErrBuildRunning}
	s := newEmbeddingsTestServer(t, fake)

	w := serveJSON(t, s.mux, http.MethodPost, "/api/v1/embeddings/build", vector.BuildRequest{})
	assertRecorderStatus(t, w, http.StatusConflict)
	assert.Contains(t, w.Body.String(), "already running")
}

func TestEmbeddingsBuildUnknownServerReturnsBadRequest(t *testing.T) {
	fake := &fakeEmbeddingsManager{startBuildErr: fmt.Errorf(
		"resolve encoder: %w", vector.ErrUnknownServer)}
	s := newEmbeddingsTestServer(t, fake)

	w := serveJSON(t, s.mux, http.MethodPost, "/api/v1/embeddings/build",
		vector.BuildRequest{Using: "nope"})
	assertRecorderStatus(t, w, http.StatusBadRequest)
	assert.Contains(t, w.Body.String(), "unknown embeddings server",
		"the response must carry the manager's actionable message, not a generic 500")
}

func TestEmbeddingsStatusReturnsCurrentStatus(t *testing.T) {
	fake := &fakeEmbeddingsManager{status: vector.BuildStatus{
		Running: true, Phase: "embedding", Done: 10, Total: 10,
		EstimateReady: true, RatePerSecond: 50, ETAMilliseconds: 0,
	}}
	s := newEmbeddingsTestServer(t, fake)

	w := serveGet(t, s, "/api/v1/embeddings/status")
	assertRecorderStatus(t, w, http.StatusOK)

	var status vector.BuildStatus
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &status))
	assert.Equal(t, fake.status, status)
	assert.Contains(t, w.Body.String(), `"eta_milliseconds":0`,
		"a ready zero-second ETA must remain distinguishable from a missing estimate")
}

func TestEmbeddingsGenerationsReturnsWrappedList(t *testing.T) {
	fake := &fakeEmbeddingsManager{generations: []vector.GenerationInfo{
		{ID: 1, State: "active", Model: "m", Dimension: 3, Fingerprint: "fp1", Embedded: 5},
	}}
	s := newEmbeddingsTestServer(t, fake)

	w := serveGet(t, s, "/api/v1/embeddings/generations")
	assertRecorderStatus(t, w, http.StatusOK)

	var body struct {
		Generations []vector.GenerationInfo `json:"generations"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Generations, 1)
	assert.Equal(t, fake.generations[0], body.Generations[0])
}

func TestEmbeddingsGenerationsPropagatesError(t *testing.T) {
	fake := &fakeEmbeddingsManager{generationsErr: errors.New("boom")}
	s := newEmbeddingsTestServer(t, fake)

	w := serveGet(t, s, "/api/v1/embeddings/generations")
	assertRecorderStatus(t, w, http.StatusInternalServerError)
}

func TestEmbeddingsActivateReturnsNoContentAndPassesForce(t *testing.T) {
	fake := &fakeEmbeddingsManager{}
	s := newEmbeddingsTestServer(t, fake)

	w := serveJSON(t, s.mux, http.MethodPost, "/api/v1/embeddings/generations/5/activate",
		map[string]bool{"force": true})
	assertRecorderStatus(t, w, http.StatusNoContent)

	require.Len(t, fake.activateCalls, 1)
	assert.Equal(t, activateCall{id: 5, force: true}, fake.activateCalls[0])
}

func TestEmbeddingsActivateRefusalReturnsConflict(t *testing.T) {
	fake := &fakeEmbeddingsManager{
		activateErr: fmt.Errorf("%w: generation 5 still has 2 messages needing embedding; use --force",
			vector.ErrGenerationRefused),
	}
	s := newEmbeddingsTestServer(t, fake)

	w := serveJSON(t, s.mux, http.MethodPost, "/api/v1/embeddings/generations/5/activate",
		map[string]bool{"force": false})
	assertRecorderStatus(t, w, http.StatusConflict)
	assert.Contains(t, w.Body.String(), "still has 2 messages needing embedding")
}

func TestEmbeddingsActivateBuildRunningReturnsConflict(t *testing.T) {
	fake := &fakeEmbeddingsManager{activateErr: vector.ErrBuildRunning}
	s := newEmbeddingsTestServer(t, fake)

	w := serveJSON(t, s.mux, http.MethodPost, "/api/v1/embeddings/generations/5/activate",
		map[string]bool{"force": false})
	assertRecorderStatus(t, w, http.StatusConflict)
}

func TestEmbeddingsActivateUnknownGenerationReturnsNotFound(t *testing.T) {
	fake := &fakeEmbeddingsManager{
		activateErr: fmt.Errorf("generation %d: %w", 999, vector.ErrGenerationNotFound),
	}
	s := newEmbeddingsTestServer(t, fake)

	w := serveJSON(t, s.mux, http.MethodPost, "/api/v1/embeddings/generations/999/activate",
		map[string]bool{"force": false})
	assertRecorderStatus(t, w, http.StatusNotFound)
	assert.Contains(t, w.Body.String(), "999")
}

func TestEmbeddingsActivateOtherErrorReturnsInternalError(t *testing.T) {
	fake := &fakeEmbeddingsManager{activateErr: errors.New("boom")}
	s := newEmbeddingsTestServer(t, fake)

	w := serveJSON(t, s.mux, http.MethodPost, "/api/v1/embeddings/generations/5/activate",
		map[string]bool{"force": false})
	assertRecorderStatus(t, w, http.StatusInternalServerError)
}

func TestEmbeddingsRetireReturnsNoContentAndPassesForce(t *testing.T) {
	fake := &fakeEmbeddingsManager{}
	s := newEmbeddingsTestServer(t, fake)

	w := serveJSON(t, s.mux, http.MethodPost, "/api/v1/embeddings/generations/7/retire",
		map[string]bool{"force": true})
	assertRecorderStatus(t, w, http.StatusNoContent)

	require.Len(t, fake.retireCalls, 1)
	assert.Equal(t, activateCall{id: 7, force: true}, fake.retireCalls[0])
}

func TestEmbeddingsRetireRefusalReturnsConflict(t *testing.T) {
	fake := &fakeEmbeddingsManager{
		retireErr: fmt.Errorf("%w: generation 7 is active; use --force to retire it",
			vector.ErrGenerationRefused),
	}
	s := newEmbeddingsTestServer(t, fake)

	w := serveJSON(t, s.mux, http.MethodPost, "/api/v1/embeddings/generations/7/retire",
		map[string]bool{"force": false})
	assertRecorderStatus(t, w, http.StatusConflict)
	assert.True(t, strings.Contains(w.Body.String(), "is active"))
}

func TestEmbeddingsRetireUnknownGenerationReturnsNotFound(t *testing.T) {
	fake := &fakeEmbeddingsManager{
		retireErr: fmt.Errorf("generation %d: %w", 999, vector.ErrGenerationNotFound),
	}
	s := newEmbeddingsTestServer(t, fake)

	w := serveJSON(t, s.mux, http.MethodPost, "/api/v1/embeddings/generations/999/retire",
		map[string]bool{"force": false})
	assertRecorderStatus(t, w, http.StatusNotFound)
	assert.Contains(t, w.Body.String(), "999")
}
