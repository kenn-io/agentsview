//go:build pgtest

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/service"
)

type acceptanceEmbeddingRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions"`
}

type acceptanceEncoder struct {
	mu       sync.Mutex
	vectors  map[string][]float32
	models   map[string]int
	requests []acceptanceEmbeddingRequest
	errors   []string
	holds    map[string]*acceptanceEncoderHold
}

type acceptanceEncoderHold struct {
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (e *acceptanceEncoder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request acceptanceEmbeddingRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		e.reject(w, fmt.Sprintf("decode request: %v", err))
		return
	}
	wantDimensions, ok := e.models[request.Model]
	if r.Method != http.MethodPost || r.URL.Path != "/v1/embeddings" || !ok || request.Dimensions != wantDimensions {
		e.reject(w, fmt.Sprintf("unexpected request method=%s path=%s model=%q dimensions=%d", r.Method, r.URL.Path, request.Model, request.Dimensions))
		return
	}
	e.mu.Lock()
	e.requests = append(e.requests, request)
	e.mu.Unlock()
	for _, input := range request.Input {
		if hold := e.holds[request.Model+"\x00"+input]; hold != nil {
			hold.once.Do(func() { close(hold.started) })
			select {
			case <-hold.release:
			case <-r.Context().Done():
				return
			}
		}
	}
	data := make([]map[string]any, len(request.Input))
	for i, input := range request.Input {
		vector, found := e.vectors[request.Model+"\x00"+input]
		if !found {
			e.reject(w, fmt.Sprintf("unexpected transformed input for %s: %q", request.Model, input))
			return
		}
		data[i] = map[string]any{"index": i, "embedding": vector}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func (e *acceptanceEncoder) reject(w http.ResponseWriter, message string) {
	e.mu.Lock()
	e.errors = append(e.errors, message)
	e.mu.Unlock()
	http.Error(w, "synthetic request rejected", http.StatusUnprocessableEntity)
}

func (e *acceptanceEncoder) snapshot() ([]acceptanceEmbeddingRequest, []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	requests := make([]acceptanceEmbeddingRequest, len(e.requests))
	for i := range e.requests {
		requests[i] = e.requests[i]
		requests[i].Input = slices.Clone(e.requests[i].Input)
	}
	return requests, slices.Clone(e.errors)
}

func acceptanceProfile(endpoint string) config.HostedEmbeddingsConfig {
	return hostedEmbeddingTestConfig(endpoint, 1)
}

func acceptanceClaudeFixture() []byte {
	return []byte(`{"type":"user","timestamp":"2026-08-13T12:00:00Z","uuid":"u1","sessionId":"runtime-session","message":{"content":"hello runtime"},"cwd":"/work/project"}` + "\n" +
		`{"type":"assistant","timestamp":"2026-08-13T12:00:01Z","uuid":"a1","parentUuid":"u1","sessionId":"runtime-session","message":{"content":"hello viewer"}}` + "\n" +
		`{"type":"user","timestamp":"2026-08-13T12:00:02Z","uuid":"u2","parentUuid":"a1","sessionId":"runtime-session","message":{"content":"keep context"}}` + "\n" +
		`{"type":"assistant","timestamp":"2026-08-13T12:00:03Z","uuid":"a2","parentUuid":"u2","sessionId":"runtime-session","message":{"content":"ready for search"}}` + "\n")
}

type acceptanceHostedRuntime struct {
	cfg        config.Config
	startup    *pgServeStartup
	store      *postgres.HostedStore
	embeddings *postgres.HostedEmbeddingStore
	admin      *sql.DB
	generation postgres.HostedEmbeddingGeneration
}

func startAcceptanceHostedRuntime(t *testing.T, profiles config.HostedEmbeddingsConfig) *acceptanceHostedRuntime {
	t.Helper()
	requireHostedSandbox(t)
	cfg, admin := hostedRuntimeConfig(t)
	cfg.PG.RawDerivation = true
	cfg.PG.RawPollSeconds = 1
	cfg.PG.RawMaxAttempts = 2
	cfg.PG.HostedEmbeddingsEnabled = true
	cfg.PG.HostedEmbeddingsPollSeconds = 1
	cfg.PG.HostedEmbeddingsAttemptSeconds = 5
	cfg.PG.HostedEmbeddingsMaxAttempts = 3
	cfg.HostedEmbeddings = profiles
	cfg.DBPath = filepath.Join(cfg.DataDir, "sessions.db")
	require.NoError(t, os.Mkdir(cfg.DBPath, 0700))
	sentinel := filepath.Join(cfg.DBPath, "sentinel")
	require.NoError(t, os.WriteFile(sentinel, []byte("hosted runtime must not open SQLite"), 0600))

	profile, err := profiles.Profile("current")
	require.NoError(t, err)
	recipe, err := hostedEmbeddingRecipe("current", profile)
	require.NoError(t, err)
	runtimeURL, err := url.Parse(cfg.PG.URL)
	require.NoError(t, err)
	generation, err := postgres.ProvisionHostedEmbeddings(t.Context(), admin, cfg.PG.Schema, cfg.PG.RawTenant, recipe, "acceptance-current", runtimeURL.User.Username())
	require.NoError(t, err)

	startup, err := preparePGServeImpl(cfg, "")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(startup.ctx)
	startup.ctx = ctx
	finished := make(chan error, 1)
	go func() { finished <- runPreparedPGServe(startup) }()
	t.Cleanup(func() {
		cancel()
		select {
		case runErr := <-finished:
			assert.NoError(t, runErr)
		case <-time.After(10 * time.Second):
			t.Error("configured hosted runtime failed to join")
		}
		startup.cleanup()
		body, readErr := os.ReadFile(sentinel)
		assert.NoError(t, readErr)
		assert.Equal(t, "hosted runtime must not open SQLite", string(body))
	})

	store, err := postgres.NewHostedStore(cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, store.Close()) })
	observerDB, err := postgres.OpenHosted(cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, observerDB.Close()) })
	embeddings, err := postgres.NewHostedEmbeddingStore(t.Context(), observerDB, postgres.HostedEmbeddingOptions{Schema: cfg.PG.Schema, Tenant: cfg.PG.RawTenant})
	require.NoError(t, err)
	return &acceptanceHostedRuntime{cfg: cfg, startup: &startup, store: store, embeddings: embeddings, admin: admin, generation: generation}
}

func acceptanceSearch(t *testing.T, runtime *acceptanceHostedRuntime, pattern, mode string) *service.ContentSearchResult {
	t.Helper()
	path := "/api/v1/search/content?pattern=" + url.QueryEscape(pattern) + "&mode=" + mode + "&limit=1"
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+path, nil)
	req.Header.Set("Authorization", "Bearer "+runtime.cfg.AuthToken)
	req.Header.Set(service.SemanticSearchIntentHeader, service.SemanticSearchIntentValue)
	rec := httptest.NewRecorder()
	runtime.startup.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var result service.ContentSearchResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	return &result
}

func waitAcceptanceHit(t *testing.T, runtime *acceptanceHostedRuntime, pattern, mode string) db.ContentMatch {
	t.Helper()
	var match db.ContentMatch
	require.Eventually(t, func() bool {
		result := acceptanceSearch(t, runtime, pattern, mode)
		if len(result.Matches) != 1 {
			return false
		}
		match = result.Matches[0]
		return true
	}, 20*time.Second, 100*time.Millisecond)
	return match
}

// Removing the raw projection, hosted worker, active-generation searcher, or
// HTTP wiring breaks this one production-path acceptance fixture.
func TestHostedEmbeddingCapturedSourceReachesPublicSearch(t *testing.T) {
	encoder := &acceptanceEncoder{
		models: map[string]int{"model-a": 3},
		vectors: map[string][]float32{
			"model-a\x00doc:hello runtime:end":      {1, 0, 0},
			"model-a\x00doc:hello viewer:end":       {0, 1, 0},
			"model-a\x00doc:keep context:end":       {0, 1, 0},
			"model-a\x00doc:ready for search:end":   {0, 1, 0},
			"model-a\x00query:runtime meaning:end":  {1, 0, 0},
			"model-a\x00query:hello runtime:end":    {1, 0, 0},
			"model-a\x00doc:novel delta:end":        {0, 0, 1},
			"model-a\x00doc:delta acknowledged:end": {0, 0, 1},
			"model-a\x00query:delta concept:end":    {0, 0, 1},
			"model-a\x00query:removed concept:end":  {0, 0, 1},
			"model-a\x00query:restored concept:end": {0, 0, 1},
			"model-a\x00query:trashed concept:end":  {0, 0, 1},
			"model-a\x00query:restored trash:end":   {0, 0, 1},
		},
	}
	server := httptest.NewServer(encoder)
	defer server.Close()
	runtime := startAcceptanceHostedRuntime(t, acceptanceProfile(server.URL+"/v1"))
	root := t.TempDir()
	path := filepath.Join(root, "project", "runtime-session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, acceptanceClaudeFixture(), 0600))
	client := newHostedCaptureClient(t, runtime.startup, runtime.store, parser.AgentClaude, root)
	initial := client.capture(t)
	waitCaptureJob(t, runtime.admin, initial.ManifestID, "complete", 1)
	require.Eventually(t, func() bool {
		active, activeErr := runtime.embeddings.Active(t.Context())
		return activeErr == nil && active != nil && active.ID == runtime.generation.ID
	}, 20*time.Second, 100*time.Millisecond)

	semantic := waitAcceptanceHit(t, runtime, "runtime meaning", "semantic")
	assert.Equal(t, "runtime-session", semantic.SessionID)
	assert.Equal(t, 0, semantic.Ordinal)
	assert.Equal(t, [2]int{0, 0}, semantic.OrdinalRange)
	assert.Equal(t, "hello runtime", semantic.Snippet)
	assert.Equal(t, "message", semantic.Location)
	assert.Equal(t, "user", semantic.Role)
	hybrid := waitAcceptanceHit(t, runtime, "hello runtime", "hybrid")
	assert.Equal(t, "runtime-session", hybrid.SessionID)
	assert.Equal(t, 0, hybrid.Ordinal)
	assert.Equal(t, [2]int{0, 0}, hybrid.OrdinalRange)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	require.NoError(t, err)
	_, err = f.WriteString(`{"type":"user","timestamp":"2026-08-13T12:01:00Z","uuid":"u3","parentUuid":"a2","sessionId":"runtime-session","message":{"content":"novel delta"}}` + "\n" + `{"type":"assistant","timestamp":"2026-08-13T12:01:01Z","uuid":"a3","parentUuid":"u3","sessionId":"runtime-session","message":{"content":"delta acknowledged"}}` + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	appended := client.capture(t)
	waitCaptureJob(t, runtime.admin, appended.ManifestID, "complete", 1)
	delta := waitAcceptanceHit(t, runtime, "delta concept", "semantic")
	assert.Equal(t, "runtime-session", delta.SessionID)
	assert.Equal(t, 4, delta.Ordinal)
	assert.Equal(t, [2]int{4, 4}, delta.OrdinalRange)
	assert.Equal(t, "novel delta", delta.Snippet)

	requests, requestErrors := encoder.snapshot()
	assert.Empty(t, requestErrors)
	var documentInputs []string
	for _, request := range requests {
		for _, input := range request.Input {
			if len(input) >= 4 && input[:4] == "doc:" {
				documentInputs = append(documentInputs, input)
			}
		}
	}
	assert.Equal(t, []string{
		"doc:hello runtime:end", "doc:hello viewer:end", "doc:keep context:end", "doc:ready for search:end",
		"doc:novel delta:end", "doc:delta acknowledged:end",
	}, documentInputs, "physical source replacement must reuse unchanged transformed chunks")

	_, queued, err := client.checkpoint.QueueTombstone(t.Context(), rawcheckpoint.SourceIdentity{
		Provider: client.last.Provider, ConfiguredRootID: client.last.ConfiguredRootID,
		SourceKey: client.last.SourceKey,
	})
	require.NoError(t, err)
	require.True(t, queued)
	removed := client.flush(t)
	waitCaptureJob(t, runtime.admin, removed.ManifestID, "complete", 1)
	assert.Empty(t, acceptanceSearch(t, runtime, "removed concept", "semantic").Matches)

	restored := client.capture(t)
	waitCaptureJob(t, runtime.admin, restored.ManifestID, "complete", 1)
	restoredHit := waitAcceptanceHit(t, runtime, "restored concept", "semantic")
	assert.Equal(t, "runtime-session", restoredHit.SessionID)
	requests, requestErrors = encoder.snapshot()
	assert.Empty(t, requestErrors)
	documentInputs = documentInputs[:0]
	for _, request := range requests {
		for _, input := range request.Input {
			if len(input) >= 4 && input[:4] == "doc:" {
				documentInputs = append(documentInputs, input)
			}
		}
	}
	assert.Len(t, documentInputs, 6, "source restoration must rebuild coverage from source-free vector values")

	req := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1/api/v1/sessions/runtime-session", nil)
	req.Header.Set("Authorization", "Bearer "+runtime.cfg.AuthToken)
	rec := httptest.NewRecorder()
	runtime.startup.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Empty(t, acceptanceSearch(t, runtime, "trashed concept", "semantic").Matches)
	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/sessions/runtime-session/restore", nil)
	req.Header.Set("Authorization", "Bearer "+runtime.cfg.AuthToken)
	rec = httptest.NewRecorder()
	runtime.startup.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	restoredTrash := waitAcceptanceHit(t, runtime, "restored trash", "semantic")
	assert.Equal(t, "runtime-session", restoredTrash.SessionID)
}
