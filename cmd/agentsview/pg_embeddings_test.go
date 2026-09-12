package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/vector"
)

type hostedEmbeddingFakeStore struct {
	mu              sync.Mutex
	leases          []postgres.HostedEmbeddingLease
	snapshots       map[string]*postgres.HostedEmbeddingSnapshot
	reused          map[string][]postgres.HostedEmbeddingVector
	published       map[string][]postgres.HostedEmbeddingVector
	failed          map[string]string
	retryable       map[string]bool
	activated       []int64
	autoActivated   []int64
	desired         *postgres.HostedEmbeddingGeneration
	publishFn       func(context.Context, *postgres.HostedEmbeddingSnapshot, []postgres.HostedEmbeddingVector) error
	heartbeats      atomic.Int32
	failHasDeadline bool
	activateErr     error
	reconcileFn     func(context.Context, int) (postgres.HostedEmbeddingReconcileResult, error)
}

func (s *hostedEmbeddingFakeStore) Reconcile(ctx context.Context, limit int) (postgres.HostedEmbeddingReconcileResult, error) {
	if s.reconcileFn != nil {
		return s.reconcileFn(ctx, limit)
	}
	return postgres.HostedEmbeddingReconcileResult{}, nil
}
func (s *hostedEmbeddingFakeStore) Desired(context.Context) (*postgres.HostedEmbeddingGeneration, error) {
	if s.desired != nil {
		return s.desired, nil
	}
	for _, snapshot := range s.snapshots {
		generation := snapshot.Generation
		return &generation, nil
	}
	return nil, nil
}
func (s *hostedEmbeddingFakeStore) Claim(_ context.Context, _ string, limit int, _ time.Duration) ([]postgres.HostedEmbeddingLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit = min(limit, len(s.leases))
	out := append([]postgres.HostedEmbeddingLease(nil), s.leases[:limit]...)
	s.leases = s.leases[limit:]
	return out, nil
}
func (s *hostedEmbeddingFakeStore) ReadSession(_ context.Context, lease postgres.HostedEmbeddingLease) (*postgres.HostedEmbeddingSnapshot, error) {
	return s.snapshots[lease.SessionID], nil
}
func (s *hostedEmbeddingFakeStore) ReusableVectors(_ context.Context, snap *postgres.HostedEmbeddingSnapshot) ([]postgres.HostedEmbeddingVector, error) {
	return append([]postgres.HostedEmbeddingVector(nil), s.reused[snap.Lease.SessionID]...), nil
}
func (s *hostedEmbeddingFakeStore) Publish(ctx context.Context, snap *postgres.HostedEmbeddingSnapshot, vectors []postgres.HostedEmbeddingVector) error {
	if s.publishFn != nil {
		return s.publishFn(ctx, snap, vectors)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.published[snap.Lease.SessionID] = append([]postgres.HostedEmbeddingVector(nil), vectors...)
	return nil
}
func (s *hostedEmbeddingFakeStore) Heartbeat(_ context.Context, lease postgres.HostedEmbeddingLease, ttl time.Duration) (postgres.HostedEmbeddingLease, error) {
	s.heartbeats.Add(1)
	lease.ExpiresAt = time.Now().Add(ttl)
	return lease, nil
}
func (s *hostedEmbeddingFakeStore) Fail(ctx context.Context, lease postgres.HostedEmbeddingLease, code string, retryable bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed[lease.SessionID] = code
	if s.retryable != nil {
		s.retryable[lease.SessionID] = retryable
	}
	_, s.failHasDeadline = ctx.Deadline()
	return nil
}
func (s *hostedEmbeddingFakeStore) Activate(_ context.Context, id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activated = append(s.activated, id)
	return s.activateErr == nil, s.activateErr
}

func (s *hostedEmbeddingFakeStore) ActivateAutomatic(_ context.Context, id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.autoActivated = append(s.autoActivated, id)
	return s.activateErr == nil, s.activateErr
}

func hostedEmbeddingTestConfig(endpoint string, transportConcurrency int) config.HostedEmbeddingsConfig {
	return config.HostedEmbeddingsConfig{Profiles: map[string]config.HostedEmbeddingProfile{
		"current": {
			Embeddings: config.VectorEmbeddingsConfig{
				Model: "model-a", Dimension: 3, MaxInputChars: 100,
				DocumentPrefix: "doc:", QueryPrefix: "query:", InputSuffix: ":end",
				RequestDimensions: true, DefaultServer: "primary",
				Servers: map[string]config.VectorEmbeddingsServerConfig{
					"primary": {Endpoint: endpoint, BatchSize: 2, Concurrency: transportConcurrency, Timeout: "2s", MaxRetries: 1},
				},
			},
		},
	}}
}

func hostedEmbeddingTestGeneration(t *testing.T, cfg config.HostedEmbeddingsConfig, id int64) postgres.HostedEmbeddingGeneration {
	t.Helper()
	profile, err := cfg.Profile("current")
	require.NoError(t, err)
	recipe, err := hostedEmbeddingRecipe("current", profile)
	require.NoError(t, err)
	return postgres.HostedEmbeddingGeneration{ID: id, InstanceKey: "instance", Recipe: recipe}
}

func TestHostedEmbeddingWorkerPublishesRawChunksWithOriginalIndices(t *testing.T) {
	var requests []struct {
		Model      string   `json:"model"`
		Input      []string `json:"input"`
		Dimensions int      `json:"dimensions"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model      string   `json:"model"`
			Input      []string `json:"input"`
			Dimensions int      `json:"dimensions"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		requests = append(requests, request)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{0, 1, 0}}}}))
	}))
	defer server.Close()

	cfg := hostedEmbeddingTestConfig(server.URL+"/v1", 1)
	generation := hostedEmbeddingTestGeneration(t, cfg, 7)
	lease := postgres.HostedEmbeddingLease{GenerationID: 7, SessionID: "source-a", Revision: 1}
	store := &hostedEmbeddingFakeStore{
		leases: []postgres.HostedEmbeddingLease{lease},
		snapshots: map[string]*postgres.HostedEmbeddingSnapshot{"source-a": {
			Generation: generation, Lease: lease, Eligible: true,
			Documents: []postgres.HostedEmbeddingDocument{{Key: "doc-a", Chunks: []postgres.HostedEmbeddingChunk{
				{Index: 0, Text: "alpha", InputHash: "cached"},
				{Index: 2, Text: "beta", InputHash: "missing"},
			}}},
		}},
		reused:    map[string][]postgres.HostedEmbeddingVector{"source-a": {{DocumentKey: "doc-a", ChunkIndex: 0, Values: []float32{1, 0, 0}}}},
		published: map[string][]postgres.HostedEmbeddingVector{}, failed: map[string]string{},
	}
	runtime := newHostedEmbeddingRuntime(t.Context(), store, newHostedEmbeddingResolver(cfg), hostedEmbeddingRuntimeOptions{SourceConcurrency: 1, AttemptTimeout: time.Second})
	require.NoError(t, runtime.processBatch(t.Context()))

	require.Len(t, requests, 1)
	assert.Equal(t, "model-a", requests[0].Model)
	assert.Equal(t, []string{"doc:beta:end"}, requests[0].Input)
	assert.Equal(t, 3, requests[0].Dimensions)
	assert.ElementsMatch(t, []postgres.HostedEmbeddingVector{
		{DocumentKey: "doc-a", ChunkIndex: 0, Values: []float32{1, 0, 0}},
		{DocumentKey: "doc-a", ChunkIndex: 2, Values: []float32{0, 1, 0}},
	}, store.published["source-a"])
	assert.Equal(t, []int64{7}, store.autoActivated)
	assert.Empty(t, store.activated)
}

func TestHostedEmbeddingWorkerActivatesEmptyDesiredOnlyWithExactAvailableProfile(t *testing.T) {
	nextConfig := hostedEmbeddingTestConfig("http://127.0.0.1:1/v1", 1)
	nextConfig.Profiles["next"] = nextConfig.Profiles["current"]
	delete(nextConfig.Profiles, "current")
	profile, err := nextConfig.Profile("next")
	require.NoError(t, err)
	recipe, err := hostedEmbeddingRecipe("next", profile)
	require.NoError(t, err)
	generation := postgres.HostedEmbeddingGeneration{ID: 9, InstanceKey: "next", Recipe: recipe}
	store := &hostedEmbeddingFakeStore{
		desired: &generation, snapshots: map[string]*postgres.HostedEmbeddingSnapshot{},
		reused: map[string][]postgres.HostedEmbeddingVector{}, published: map[string][]postgres.HostedEmbeddingVector{}, failed: map[string]string{},
	}
	currentOnly := hostedEmbeddingTestConfig("http://127.0.0.1:1/v1", 1)
	runtime := newHostedEmbeddingRuntime(t.Context(), store, newHostedEmbeddingResolver(currentOnly), hostedEmbeddingRuntimeOptions{})
	require.NoError(t, runtime.processBatch(t.Context()))
	assert.Empty(t, store.autoActivated, "an unavailable desired profile must retain the current active generation")

	runtime = newHostedEmbeddingRuntime(t.Context(), store, newHostedEmbeddingResolver(nextConfig), hostedEmbeddingRuntimeOptions{})
	require.NoError(t, runtime.processBatch(t.Context()))
	assert.Equal(t, []int64{9}, store.autoActivated)
	assert.Empty(t, store.activated)
}

func TestHostedEmbeddingResolverRequiresSelectedSecretOnly(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	cfg := hostedEmbeddingTestConfig(server.URL+"/v1", 1)
	current := cfg.Profiles["current"]
	selected := current.Embeddings.Servers["primary"]
	selected.APIKeyEnv = "HOSTED_SELECTED_KEY"
	current.Embeddings.Servers["primary"] = selected
	cfg.Profiles["current"] = current
	cfg.Profiles["unused"] = current
	unused := cfg.Profiles["unused"]
	unusedServer := selected
	unusedServer.APIKeyEnv = "HOSTED_UNUSED_KEY"
	unused.Embeddings.Servers = map[string]config.VectorEmbeddingsServerConfig{"primary": unusedServer}
	cfg.Profiles["unused"] = unused
	t.Setenv("HOSTED_SELECTED_KEY", "")
	t.Setenv("HOSTED_UNUSED_KEY", "")

	generation := hostedEmbeddingTestGeneration(t, cfg, 1)
	_, err := newHostedEmbeddingResolver(cfg).document(generation)
	require.ErrorContains(t, err, "selected API key reference is missing or empty")
	assert.Zero(t, calls.Load())

	t.Setenv("HOSTED_SELECTED_KEY", "selected-value")
	_, err = newHostedEmbeddingResolver(cfg).document(generation)
	require.NoError(t, err)
	assert.Zero(t, calls.Load(), "resolving a profile must not call its endpoint")
}

func TestHostedEmbeddingWorkerCapsHTTPConcurrencyAcrossSources(t *testing.T) {
	var inFlight, peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := inFlight.Add(1)
		for {
			prior := peak.Load()
			if current <= prior || peak.CompareAndSwap(prior, current) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		inFlight.Add(-1)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{1, 0, 0}}}}))
	}))
	defer server.Close()
	cfg := hostedEmbeddingTestConfig(server.URL+"/v1", 1)
	generation := hostedEmbeddingTestGeneration(t, cfg, 1)
	store := &hostedEmbeddingFakeStore{snapshots: map[string]*postgres.HostedEmbeddingSnapshot{}, reused: map[string][]postgres.HostedEmbeddingVector{}, published: map[string][]postgres.HostedEmbeddingVector{}, failed: map[string]string{}}
	for _, source := range []string{"a", "b"} {
		lease := postgres.HostedEmbeddingLease{GenerationID: 1, SessionID: source, Revision: 1}
		store.leases = append(store.leases, lease)
		store.snapshots[source] = &postgres.HostedEmbeddingSnapshot{Generation: generation, Lease: lease, Eligible: true, Documents: []postgres.HostedEmbeddingDocument{{Key: "doc-" + source, Chunks: []postgres.HostedEmbeddingChunk{{Index: 0, Text: source, InputHash: source}}}}}
	}
	runtime := newHostedEmbeddingRuntime(t.Context(), store, newHostedEmbeddingResolver(cfg), hostedEmbeddingRuntimeOptions{SourceConcurrency: 2, AttemptTimeout: time.Second})
	require.NoError(t, runtime.processBatch(t.Context()))
	assert.Equal(t, int32(1), peak.Load())
	assert.Len(t, store.published, 2)
}

func TestHostedEmbeddingWorkerRejectsPartialEncoderResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": []any{}}))
	}))
	defer server.Close()
	cfg := hostedEmbeddingTestConfig(server.URL+"/v1", 1)
	generation := hostedEmbeddingTestGeneration(t, cfg, 1)
	lease := postgres.HostedEmbeddingLease{GenerationID: 1, SessionID: "partial", Revision: 1}
	store := &hostedEmbeddingFakeStore{
		leases: []postgres.HostedEmbeddingLease{lease},
		snapshots: map[string]*postgres.HostedEmbeddingSnapshot{"partial": {
			Generation: generation, Lease: lease, Eligible: true,
			Documents: []postgres.HostedEmbeddingDocument{{Key: "doc", Chunks: []postgres.HostedEmbeddingChunk{{Index: 3, Text: "must be present", InputHash: "hash"}}}},
		}},
		reused: map[string][]postgres.HostedEmbeddingVector{}, published: map[string][]postgres.HostedEmbeddingVector{}, failed: map[string]string{},
	}
	runtime := newHostedEmbeddingRuntime(t.Context(), store, newHostedEmbeddingResolver(cfg), hostedEmbeddingRuntimeOptions{SourceConcurrency: 1, AttemptTimeout: time.Second})
	require.NoError(t, runtime.processBatch(t.Context()))
	assert.Empty(t, store.published, "a partial response must not publish incomplete coverage")
	assert.Equal(t, "encoder_unavailable", store.failed["partial"], "the structural response failure must be recorded without provider content")
}

func hostedEmbeddingSingleLeaseStore(generation postgres.HostedEmbeddingGeneration, session, text string) *hostedEmbeddingFakeStore {
	lease := postgres.HostedEmbeddingLease{GenerationID: generation.ID, SessionID: session, Revision: 1}
	return &hostedEmbeddingFakeStore{
		leases: []postgres.HostedEmbeddingLease{lease},
		snapshots: map[string]*postgres.HostedEmbeddingSnapshot{session: {
			Generation: generation, Lease: lease, Eligible: true,
			Documents: []postgres.HostedEmbeddingDocument{{Key: "doc", Chunks: []postgres.HostedEmbeddingChunk{{Index: 0, Text: text, InputHash: "hash"}}}},
		}},
		reused: map[string][]postgres.HostedEmbeddingVector{}, published: map[string][]postgres.HostedEmbeddingVector{},
		failed: map[string]string{}, retryable: map[string]bool{},
	}
}

func TestHostedEmbeddingWorkerRetriesTransientDecodeFailureAfterRestart(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if requests.Add(1) == 1 {
			_, _ = io.WriteString(w, `{"data":`)
			return
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{1, 0, 0}}}}))
	}))
	defer server.Close()
	cfg := hostedEmbeddingTestConfig(server.URL+"/v1", 1)
	generation := hostedEmbeddingTestGeneration(t, cfg, 1)
	store := hostedEmbeddingSingleLeaseStore(generation, "transient", "retry me")
	runtime := newHostedEmbeddingRuntime(t.Context(), store, newHostedEmbeddingResolver(cfg), hostedEmbeddingRuntimeOptions{AttemptTimeout: time.Second})
	require.NoError(t, runtime.processBatch(t.Context()))
	assert.Equal(t, "encoder_unavailable", store.failed["transient"])
	assert.True(t, store.retryable["transient"])

	store.leases = []postgres.HostedEmbeddingLease{store.snapshots["transient"].Lease}
	runtime = newHostedEmbeddingRuntime(t.Context(), store, newHostedEmbeddingResolver(cfg), hostedEmbeddingRuntimeOptions{AttemptTimeout: time.Second})
	require.NoError(t, runtime.processBatch(t.Context()))
	assert.Contains(t, store.published, "transient")
}

func TestHostedEmbeddingWorkerBoundsRateLimitAttempt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	cfg := hostedEmbeddingTestConfig(server.URL+"/v1", 1)
	generation := hostedEmbeddingTestGeneration(t, cfg, 1)
	store := hostedEmbeddingSingleLeaseStore(generation, "limited", "retry later")
	runtime := newHostedEmbeddingRuntime(t.Context(), store, newHostedEmbeddingResolver(cfg), hostedEmbeddingRuntimeOptions{AttemptTimeout: 80 * time.Millisecond, HeartbeatInterval: 10 * time.Millisecond})

	started := time.Now()
	require.NoError(t, runtime.processBatch(t.Context()))
	assert.Less(t, time.Since(started), time.Second)
	assert.Equal(t, "encoder_timeout", store.failed["limited"])
	assert.True(t, store.retryable["limited"])
}

func TestHostedEmbeddingWorkerPermanentlyRejectsInputWithoutLeakingDiagnostics(t *testing.T) {
	const bodyMarker = "PROVIDER_BODY_MARKER"
	const promptMarker = "PROMPT_MARKER"
	const keyMarker = "KEY_MARKER"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, bodyMarker+" input exceeds context length")
	}))
	defer server.Close()
	cfg := hostedEmbeddingTestConfig(server.URL+"/v1", 1)
	profile := cfg.Profiles["current"]
	selected := profile.Embeddings.Servers["primary"]
	selected.APIKeyEnv = "HOSTED_PERMANENT_TEST_KEY"
	profile.Embeddings.Servers["primary"] = selected
	cfg.Profiles["current"] = profile
	t.Setenv("HOSTED_PERMANENT_TEST_KEY", keyMarker)
	generation := hostedEmbeddingTestGeneration(t, cfg, 1)
	store := hostedEmbeddingSingleLeaseStore(generation, "permanent", promptMarker)
	store.activateErr = errors.New("synthetic activation failure")
	runtime := newHostedEmbeddingRuntime(t.Context(), store, newHostedEmbeddingResolver(cfg), hostedEmbeddingRuntimeOptions{PollInterval: time.Hour, AttemptTimeout: time.Second})
	stderr := captureHostedEmbeddingRuntimeStderr(t, runtime)

	assert.Equal(t, "invalid_results", store.failed["permanent"])
	assert.False(t, store.retryable["permanent"])
	diagnostics, err := json.Marshal(store.failed)
	require.NoError(t, err)
	assert.Contains(t, stderr, "hosted embedding worker: batch failed")
	for _, marker := range []string{bodyMarker, promptMarker, keyMarker} {
		assert.NotContains(t, string(diagnostics), marker)
		assert.NotContains(t, stderr, marker)
	}
}

func TestHostedEmbeddingWorkerDoesNotLeakSelectedSecretFailureDiagnostics(t *testing.T) {
	const secretReferenceMarker = "HOSTED_SECRET_REFERENCE_MARKER"
	const promptMarker = "SECRET_FAILURE_PROMPT_MARKER"
	cfg := hostedEmbeddingTestConfig("http://127.0.0.1:1/v1", 1)
	profile := cfg.Profiles["current"]
	selected := profile.Embeddings.Servers["primary"]
	selected.APIKeyEnv = secretReferenceMarker
	profile.Embeddings.Servers["primary"] = selected
	cfg.Profiles["current"] = profile
	t.Setenv(secretReferenceMarker, "")
	generation := hostedEmbeddingTestGeneration(t, cfg, 1)
	store := hostedEmbeddingSingleLeaseStore(generation, "secret", promptMarker)
	runtime := newHostedEmbeddingRuntime(t.Context(), store, newHostedEmbeddingResolver(cfg), hostedEmbeddingRuntimeOptions{PollInterval: time.Hour, AttemptTimeout: time.Second})
	stderr := captureHostedEmbeddingRuntimeStderr(t, runtime)

	assert.Equal(t, "encoder_unavailable", store.failed["secret"])
	assert.False(t, store.retryable["secret"])
	diagnostics, err := json.Marshal(store.failed)
	require.NoError(t, err)
	for _, marker := range []string{secretReferenceMarker, promptMarker} {
		assert.NotContains(t, string(diagnostics), marker)
		assert.NotContains(t, stderr, marker)
	}
}

type hostedEmbeddingStderrCapture struct {
	buffer bytes.Buffer
	ready  chan struct{}
	once   sync.Once
}

func (c *hostedEmbeddingStderrCapture) Write(p []byte) (int, error) {
	n, err := c.buffer.Write(p)
	if n > 0 {
		c.once.Do(func() { close(c.ready) })
	}
	return n, err
}

func captureHostedEmbeddingRuntimeStderr(t *testing.T, runtime *hostedEmbeddingRuntime) string {
	t.Helper()
	stderrReader, stderrWriter, err := os.Pipe()
	require.NoError(t, err)
	originalStderr := os.Stderr
	os.Stderr = stderrWriter
	captured := &hostedEmbeddingStderrCapture{ready: make(chan struct{})}
	readerDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(captured, stderrReader)
		readerDone <- copyErr
	}()
	runtime.Start()
	select {
	case <-captured.ready:
	case <-time.After(time.Second):
	}
	runtime.Stop()
	os.Stderr = originalStderr
	require.NoError(t, stderrWriter.Close())
	require.NoError(t, <-readerDone)
	require.NoError(t, stderrReader.Close())
	return captured.buffer.String()
}

func TestCaptureHostedEmbeddingRuntimeStderrDrainsLaterLines(t *testing.T) {
	const laterMarker = "SYNTHETIC_LATER_DIAGNOSTIC"
	store := &hostedEmbeddingFakeStore{reconcileFn: func(context.Context, int) (postgres.HostedEmbeddingReconcileResult, error) {
		_, _ = fmt.Fprintln(os.Stderr, "first diagnostic")
		_, _ = fmt.Fprintln(os.Stderr, laterMarker)
		return postgres.HostedEmbeddingReconcileResult{}, errors.New("synthetic reconcile failure")
	}}
	runtime := newHostedEmbeddingRuntime(t.Context(), store, newHostedEmbeddingResolver(config.HostedEmbeddingsConfig{}), hostedEmbeddingRuntimeOptions{PollInterval: time.Hour})

	stderr := captureHostedEmbeddingRuntimeStderr(t, runtime)
	assert.Contains(t, stderr, "first diagnostic")
	assert.Contains(t, stderr, laterMarker)
}

func TestHostedEmbeddingWorkerKeepsHeartbeatThroughBoundedPublication(t *testing.T) {
	cfg := hostedEmbeddingTestConfig("http://127.0.0.1:1/v1", 1)
	generation := hostedEmbeddingTestGeneration(t, cfg, 1)
	store := hostedEmbeddingSingleLeaseStore(generation, "blocked", "already encoded")
	store.reused["blocked"] = []postgres.HostedEmbeddingVector{{DocumentKey: "doc", ChunkIndex: 0, Values: []float32{1, 0, 0}}}
	store.publishFn = func(ctx context.Context, _ *postgres.HostedEmbeddingSnapshot, _ []postgres.HostedEmbeddingVector) error {
		<-ctx.Done()
		return ctx.Err()
	}
	runtime := newHostedEmbeddingRuntime(t.Context(), store, newHostedEmbeddingResolver(cfg), hostedEmbeddingRuntimeOptions{AttemptTimeout: 80 * time.Millisecond, HeartbeatInterval: 10 * time.Millisecond})

	started := time.Now()
	require.NoError(t, runtime.processBatch(t.Context()))
	assert.Less(t, time.Since(started), time.Second)
	assert.Positive(t, store.heartbeats.Load())
	assert.Equal(t, "encoder_timeout", store.failed["blocked"])
	assert.True(t, store.retryable["blocked"])
	assert.True(t, store.failHasDeadline)
}

func TestHostedEmbeddingRuntimeStopCancelsAndJoinsEncoder(t *testing.T) {
	requestStarted := make(chan struct{})
	requestDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		require.NoError(t, err)
		close(requestStarted)
		<-r.Context().Done()
		close(requestDone)
	}))
	defer server.Close()
	cfg := hostedEmbeddingTestConfig(server.URL+"/v1", 1)
	generation := hostedEmbeddingTestGeneration(t, cfg, 1)
	lease := postgres.HostedEmbeddingLease{GenerationID: 1, SessionID: "a", Revision: 1}
	store := &hostedEmbeddingFakeStore{leases: []postgres.HostedEmbeddingLease{lease}, snapshots: map[string]*postgres.HostedEmbeddingSnapshot{"a": {Generation: generation, Lease: lease, Eligible: true, Documents: []postgres.HostedEmbeddingDocument{{Key: "doc-a", Chunks: []postgres.HostedEmbeddingChunk{{Index: 0, Text: "a", InputHash: "a"}}}}}}, reused: map[string][]postgres.HostedEmbeddingVector{}, published: map[string][]postgres.HostedEmbeddingVector{}, failed: map[string]string{}}
	runtime := newHostedEmbeddingRuntime(context.Background(), store, newHostedEmbeddingResolver(cfg), hostedEmbeddingRuntimeOptions{PollInterval: time.Hour, SourceConcurrency: 1, AttemptTimeout: time.Minute})
	runtime.Start()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		require.Fail(t, "encoder request did not start")
	}
	runtime.Stop()
	require.Eventually(t, func() bool {
		select {
		case <-requestDone:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond, "Stop must cancel the encoder HTTP request")
}

type hostedEmbeddingFakeSearchStore struct {
	active      *postgres.HostedEmbeddingGeneration
	activeCalls int
	searched    int64
	resolved    int64
	vector      []float32
}

func (s *hostedEmbeddingFakeSearchStore) Active(context.Context) (*postgres.HostedEmbeddingGeneration, error) {
	s.activeCalls++
	return s.active, nil
}
func (s *hostedEmbeddingFakeSearchStore) SearchGeneration(_ context.Context, generation postgres.HostedEmbeddingGeneration, vector []float32, _ int) ([]db.VectorHit, error) {
	s.searched = generation.ID
	s.vector = append([]float32(nil), vector...)
	return []db.VectorHit{{SessionID: "physical", Ordinal: 2, Score: 1}}, nil
}
func (s *hostedEmbeddingFakeSearchStore) ResolveGenerationUnits(_ context.Context, generation postgres.HostedEmbeddingGeneration, _ []db.MessageRef) ([]db.UnitRef, error) {
	s.resolved = generation.ID
	return []db.UnitRef{{DocKey: "doc", SessionID: "physical"}}, nil
}

func TestHostedEmbeddingSearcherBindsOneGenerationAcrossRequestLegs(t *testing.T) {
	cfg := hostedEmbeddingTestConfig("http://127.0.0.1:1/v1", 1)
	generation := hostedEmbeddingTestGeneration(t, cfg, 11)
	store := &hostedEmbeddingFakeSearchStore{active: &generation}
	searcher := newHostedEmbeddingSearcher(store, newHostedEmbeddingResolver(cfg))

	bound, err := searcher.BindVectorSearch(t.Context())
	require.NoError(t, err)
	next := generation
	next.ID = 12
	store.active = &next
	_, err = bound.ResolveMessageUnits(t.Context(), []db.MessageRef{{SessionID: "physical", Ordinal: 0}})
	require.NoError(t, err)

	assert.Equal(t, 1, store.activeCalls)
	assert.Equal(t, int64(11), store.resolved)
}

func TestHostedEmbeddingSearcherPinsActiveGenerationAndExactQueryRecipe(t *testing.T) {
	var input []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		input = append(input, request.Input...)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{1, 0, 0}}}}))
	}))
	defer server.Close()
	cfg := hostedEmbeddingTestConfig(server.URL+"/v1", 1)
	generation := hostedEmbeddingTestGeneration(t, cfg, 11)
	store := &hostedEmbeddingFakeSearchStore{active: &generation}
	searcher := newHostedEmbeddingSearcher(store, newHostedEmbeddingResolver(cfg))

	empty := newHostedEmbeddingSearcher(&hostedEmbeddingFakeSearchStore{}, newHostedEmbeddingResolver(cfg))
	available, err := empty.SemanticAvailable(t.Context())
	require.NoError(t, err)
	assert.False(t, available)
	_, err = empty.SemanticSearch(t.Context(), "needle", 5)
	assert.ErrorIs(t, err, db.ErrSemanticUnavailable)

	available, err = searcher.SemanticAvailable(t.Context())
	require.NoError(t, err)
	assert.True(t, available)
	hits, err := searcher.SemanticSearch(t.Context(), "needle", 5)
	require.NoError(t, err)
	assert.Equal(t, []string{"query:needle:end"}, input)
	assert.Equal(t, int64(11), store.searched)
	assert.Equal(t, []float32{1, 0, 0}, store.vector)
	assert.Equal(t, []db.VectorHit{{SessionID: "physical", Ordinal: 2, Score: 1}}, hits)

	changed := cfg.Profiles["current"]
	changed.Embeddings.Model = "changed-in-place"
	searcher = newHostedEmbeddingSearcher(store, newHostedEmbeddingResolver(config.HostedEmbeddingsConfig{Profiles: map[string]config.HostedEmbeddingProfile{"current": changed}}))
	available, err = searcher.SemanticAvailable(t.Context())
	require.NoError(t, err)
	assert.False(t, available)
	_, err = searcher.SemanticSearch(t.Context(), "needle", 5)
	assert.ErrorIs(t, err, db.ErrSemanticUnavailable)
}

func TestPGEmbeddingsProvisionRejectsEmptySelectedURLBeforeOpeningDatabase(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	t.Setenv("MISSING_OWNER_DATABASE_URL", "")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`
default_pg = "owner"

[pg.owner]
url = "${MISSING_OWNER_DATABASE_URL}"
schema = "hosted"
raw_tenant = "tenant"

[hosted_embeddings.profiles.current.embeddings]
model = "model-a"
dimension = 3
default_server = "primary"

[hosted_embeddings.profiles.current.embeddings.servers.primary]
endpoint = "https://embeddings.example.test/v1"
`), 0o600))

	cmd := newPGEmbeddingsCommand()
	cmd.SetArgs([]string{"provision", "owner", "--profile", "current", "--runtime-role", "runtime", "--instance-key", "initial"})
	err := cmd.Execute()
	require.ErrorContains(t, err, "selected PostgreSQL target has no usable url")
}

func TestHostedEmbeddingStoreFailuresKeepValidationPermanent(t *testing.T) {
	for _, fallback := range []string{"source_read", "invalid_results"} {
		for _, tc := range []struct {
			name string
			err  error
			code string
		}{
			{"work_limit", postgres.ErrHostedEmbeddingWorkLimit, "work_limit"},
			{"structural_results", postgres.ErrHostedEmbeddingInvalidResults, "invalid_results"},
			{"invalid_vector", &vector.InvalidEmbeddingError{}, "invalid_results"},
			{"permanent_http", &vector.HTTPStatusError{Status: 400, Body: "input exceeds context length"}, "invalid_results"},
		} {
			t.Run(fallback+"/"+tc.name, func(t *testing.T) {
				store := &hostedEmbeddingFakeStore{failed: map[string]string{}, retryable: map[string]bool{}}
				runtime := newHostedEmbeddingRuntime(t.Context(), store, nil, hostedEmbeddingRuntimeOptions{})
				require.NoError(t, runtime.recordFailure(t.Context(), postgres.HostedEmbeddingLease{SessionID: "s"}, tc.err, fallback))
				assert.Equal(t, tc.code, store.failed["s"])
				assert.False(t, store.retryable["s"])
			})
		}
	}
}

func TestHostedEmbeddingWorkerProfileFailuresStayPermanent(t *testing.T) {
	for _, fault := range []string{"missing_profile", "changed_recipe", "missing_selected_secret"} {
		t.Run(fault, func(t *testing.T) {
			var requests atomic.Int32
			endpoint := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
			defer endpoint.Close()
			cfg := hostedEmbeddingTestConfig(endpoint.URL+"/v1", 1)
			generation := hostedEmbeddingTestGeneration(t, cfg, 1)
			switch fault {
			case "missing_profile":
				delete(cfg.Profiles, "current")
			case "changed_recipe":
				profile := cfg.Profiles["current"]
				profile.Embeddings.Model = "different"
				cfg.Profiles["current"] = profile
			case "missing_selected_secret":
				profile := cfg.Profiles["current"]
				server := profile.Embeddings.Servers["primary"]
				server.APIKeyEnv = "HOSTED_MISSING_TEST_SECRET"
				t.Setenv(server.APIKeyEnv, "")
				profile.Embeddings.Servers["primary"] = server
				cfg.Profiles["current"] = profile
			}
			store := hostedEmbeddingSingleLeaseStore(generation, "s", "synthetic")
			runtime := newHostedEmbeddingRuntime(t.Context(), store, newHostedEmbeddingResolver(cfg), hostedEmbeddingRuntimeOptions{})
			require.NoError(t, runtime.processBatch(t.Context()))
			assert.Equal(t, "encoder_unavailable", store.failed["s"])
			assert.False(t, store.retryable["s"])
			assert.Empty(t, store.published)
			assert.Zero(t, requests.Load())
		})
	}
}
