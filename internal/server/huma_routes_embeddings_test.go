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

// newEmbeddingsTestServer builds a full Server (via testServer, so the SPA
// fallback and route registration match production) with m wired in as the
// embeddings manager. A nil m leaves the routes unregistered.
func newEmbeddingsTestServer(t *testing.T, m EmbeddingsManager) *Server {
	t.Helper()
	var opts []Option
	if m != nil {
		opts = append(opts, WithEmbeddingsManager(m))
	}
	return testServer(t, 0, opts...)
}

// TestEmbeddingsRoutesNotRegisteredWhenManagerNil confirms a nil manager
// leaves /api/v1/embeddings/... entirely unregistered on the mux (it falls
// through to the SPA catch-all pattern "/", same as any other unknown path)
// rather than serving with a "no manager" error. A live server (fake
// manager set) registers the exact pattern instead.
func TestEmbeddingsRoutesNotRegisteredWhenManagerNil(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/embeddings/status", nil)

	withoutManager := newEmbeddingsTestServer(t, nil)
	_, patternWithout := withoutManager.mux.Handler(req)
	assert.Equal(t, "/", patternWithout)

	withManager := newEmbeddingsTestServer(t, &fakeEmbeddingsManager{})
	_, patternWith := withManager.mux.Handler(req)
	assert.NotEqual(t, "/", patternWith)
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
}

func TestEmbeddingsBuildReturnsConflictWhenAlreadyRunning(t *testing.T) {
	fake := &fakeEmbeddingsManager{startBuildErr: vector.ErrBuildRunning}
	s := newEmbeddingsTestServer(t, fake)

	w := serveJSON(t, s.mux, http.MethodPost, "/api/v1/embeddings/build", vector.BuildRequest{})
	assertRecorderStatus(t, w, http.StatusConflict)
	assert.Contains(t, w.Body.String(), "already running")
}

func TestEmbeddingsStatusReturnsCurrentStatus(t *testing.T) {
	fake := &fakeEmbeddingsManager{status: vector.BuildStatus{
		Running: true, Phase: "embedding", Done: 3, Total: 10,
	}}
	s := newEmbeddingsTestServer(t, fake)

	w := serveGet(t, s, "/api/v1/embeddings/status")
	assertRecorderStatus(t, w, http.StatusOK)

	var status vector.BuildStatus
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &status))
	assert.Equal(t, fake.status, status)
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
