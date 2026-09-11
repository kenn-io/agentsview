//go:build pgtest

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/postgres"
)

type activatingHostedSearchStore struct {
	delegate *postgres.HostedEmbeddingStore
	activate func() error
	once     sync.Once
	mu       sync.Mutex
	searched []int64
	resolved []int64
}

func (s *activatingHostedSearchStore) Active(ctx context.Context) (*postgres.HostedEmbeddingGeneration, error) {
	return s.delegate.Active(ctx)
}

func (s *activatingHostedSearchStore) SearchGeneration(ctx context.Context, generation postgres.HostedEmbeddingGeneration, vector []float32, limit int) ([]db.VectorHit, error) {
	s.mu.Lock()
	s.searched = append(s.searched, generation.ID)
	s.mu.Unlock()
	hits, err := s.delegate.SearchGeneration(ctx, generation, vector, limit)
	if err == nil {
		s.once.Do(func() { err = s.activate() })
	}
	return hits, err
}

func (s *activatingHostedSearchStore) ResolveGenerationUnits(ctx context.Context, generation postgres.HostedEmbeddingGeneration, refs []db.MessageRef) ([]db.UnitRef, error) {
	s.mu.Lock()
	s.resolved = append(s.resolved, generation.ID)
	s.mu.Unlock()
	return s.delegate.ResolveGenerationUnits(ctx, generation, refs)
}

func publishHostedTestGeneration(t *testing.T, store *postgres.HostedEmbeddingStore, generation postgres.HostedEmbeddingGeneration) {
	t.Helper()
	for range 3 {
		_, err := store.Reconcile(t.Context(), 64)
		require.NoError(t, err)
	}
	leases, err := store.Claim(t.Context(), "test-worker", 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	snapshot, err := store.ReadSession(t.Context(), leases[0])
	require.NoError(t, err)
	var vectors []postgres.HostedEmbeddingVector
	for _, document := range snapshot.Documents {
		for _, chunk := range document.Chunks {
			values := make([]float32, generation.Recipe.Dimensions)
			values[0] = 1
			vectors = append(vectors, postgres.HostedEmbeddingVector{DocumentKey: document.Key, ChunkIndex: chunk.Index, Values: values})
		}
	}
	require.NoError(t, store.Publish(t.Context(), snapshot, vectors))
}

func TestPGHostedHybridPinsGenerationAcrossConcurrentActivation(t *testing.T) {
	cfg, admin := hostedRuntimeConfig(t)
	encoder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Dimensions int `json:"dimensions"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		values := make([]float32, request.Dimensions)
		values[0] = 1
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": values}}}))
	}))
	defer encoder.Close()

	embeddings := hostedEmbeddingTestConfig(encoder.URL+"/v1", 1)
	profileA := embeddings.Profiles["current"]
	profileB := profileA
	profileB.Embeddings.Model = "model-b"
	profileB.Embeddings.Dimension = 4
	embeddings.Profiles["next"] = profileB
	profileARecipe, err := hostedEmbeddingRecipe("current", profileA)
	require.NoError(t, err)
	profileBRecipe, err := hostedEmbeddingRecipe("next", profileB)
	require.NoError(t, err)
	runtimeURL, err := url.Parse(cfg.PG.URL)
	require.NoError(t, err)
	runtimeRole := runtimeURL.User.Username()
	generationA, err := postgres.ProvisionHostedEmbeddings(t.Context(), admin, cfg.PG.Schema, cfg.PG.RawTenant, profileARecipe, "a", runtimeRole)
	require.NoError(t, err)
	runtimeDB, err := postgres.OpenHosted(cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
	require.NoError(t, err)
	_, err = runtimeDB.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,message_count,user_message_count) VALUES('physical','project','machine','codex','legacy',2,2); INSERT INTO messages(session_id,ordinal,role,content) VALUES('physical',0,'user','needle'),('physical',1,'user','second turn')`)
	require.NoError(t, err)
	embeddingStore, err := postgres.NewHostedEmbeddingStore(t.Context(), runtimeDB, postgres.HostedEmbeddingOptions{Schema: cfg.PG.Schema, Tenant: cfg.PG.RawTenant})
	require.NoError(t, err)
	publishHostedTestGeneration(t, embeddingStore, generationA)
	activated, err := embeddingStore.Activate(t.Context(), generationA.ID)
	require.NoError(t, err)
	require.True(t, activated)

	generationB, err := postgres.ProvisionHostedEmbeddings(t.Context(), admin, cfg.PG.Schema, cfg.PG.RawTenant, profileBRecipe, "b", runtimeRole)
	require.NoError(t, err)
	publishHostedTestGeneration(t, embeddingStore, generationB)

	readStore, err := postgres.NewHostedStore(cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
	require.NoError(t, err)
	defer readStore.Close()
	tracking := &activatingHostedSearchStore{delegate: embeddingStore, activate: func() error {
		ok, activateErr := embeddingStore.Activate(t.Context(), generationB.ID)
		if activateErr == nil && !ok {
			return fmt.Errorf("generation B was not activation ready")
		}
		return activateErr
	}}
	readStore.SetVectorSearcher(newHostedEmbeddingSearcher(tracking, newHostedEmbeddingResolver(config.HostedEmbeddingsConfig{Profiles: embeddings.Profiles})))

	page, err := readStore.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "needle", Mode: "hybrid", Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, page.Matches)
	assert.Equal(t, "physical", page.Matches[0].SessionID)
	assert.Equal(t, []int64{generationA.ID}, tracking.searched)
	assert.Equal(t, []int64{generationA.ID}, tracking.resolved)
	active, err := embeddingStore.Active(t.Context())
	require.NoError(t, err)
	require.NotNil(t, active)
	assert.Equal(t, generationB.ID, active.ID)
}

func TestPGHostedWorkerResumesDurableTransientFailureAfterRestart(t *testing.T) {
	cfg, admin := hostedRuntimeConfig(t)
	var requests atomic.Int32
	encoder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if requests.Add(1) == 1 {
			_, _ = io.WriteString(w, `{"data":`)
			return
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{1, 0, 0}}}}))
	}))
	defer encoder.Close()
	embeddings := hostedEmbeddingTestConfig(encoder.URL+"/v1", 1)
	profile, err := embeddings.Profile("current")
	require.NoError(t, err)
	recipe, err := hostedEmbeddingRecipe("current", profile)
	require.NoError(t, err)
	runtimeURL, err := url.Parse(cfg.PG.URL)
	require.NoError(t, err)
	generation, err := postgres.ProvisionHostedEmbeddings(t.Context(), admin, cfg.PG.Schema, cfg.PG.RawTenant, recipe, "initial", runtimeURL.User.Username())
	require.NoError(t, err)

	openStore := func() (*sql.DB, *postgres.HostedEmbeddingStore) {
		database, openErr := postgres.OpenHosted(cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
		require.NoError(t, openErr)
		store, storeErr := postgres.NewHostedEmbeddingStore(t.Context(), database, postgres.HostedEmbeddingOptions{Schema: cfg.PG.Schema, Tenant: cfg.PG.RawTenant, MaxAttempts: 3, RetryDelay: 5 * time.Millisecond})
		require.NoError(t, storeErr)
		return database, store
	}
	database, store := openStore()
	_, err = database.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,message_count,user_message_count) VALUES('retry-source','project','machine','codex','legacy',1,1); INSERT INTO messages(session_id,ordinal,role,content) VALUES('retry-source',0,'user','retry me')`)
	require.NoError(t, err)
	first := newHostedEmbeddingRuntime(t.Context(), store, newHostedEmbeddingResolver(embeddings), hostedEmbeddingRuntimeOptions{AttemptTimeout: time.Second})
	require.NoError(t, first.processBatch(t.Context()))
	status, err := store.Status(t.Context())
	require.NoError(t, err)
	require.NotNil(t, status.Desired)
	assert.Equal(t, int64(1), status.Desired.Retry)
	assert.Equal(t, int32(1), requests.Load())
	require.NoError(t, database.Close())

	database, store = openStore()
	defer database.Close()
	second := newHostedEmbeddingRuntime(t.Context(), store, newHostedEmbeddingResolver(embeddings), hostedEmbeddingRuntimeOptions{AttemptTimeout: time.Second})
	require.Eventually(t, func() bool {
		if batchErr := second.processBatch(t.Context()); batchErr != nil {
			return false
		}
		active, activeErr := store.Active(t.Context())
		return activeErr == nil && active != nil && active.ID == generation.ID
	}, time.Second, 10*time.Millisecond)
	assert.Equal(t, int32(2), requests.Load(), "each worker attempt must use its bounded encoder budget")
}

func TestPGEmbeddingsCommandsSeparateOwnerProvisionFromReadOnlyStatus(t *testing.T) {
	cfg, admin := hostedRuntimeConfig(t)
	ownerURL := os.Getenv("TEST_PG_URL")
	runtimeURL, err := url.Parse(cfg.PG.URL)
	require.NoError(t, err)
	runtimeRole := runtimeURL.User.Username()

	var embeddingCalls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		embeddingCalls.Add(1)
	}))
	defer endpoint.Close()

	configPath := filepath.Join(cfg.DataDir, "config.toml")
	configBody := fmt.Sprintf(`default_pg = "runtime"

[pg.owner]
url = %q
schema = %q
raw_tenant = %q

[pg.runtime]
url = %q
schema = %q
raw_tenant = %q

[pg.unused]
url = "${UNUSED_OWNER_DATABASE_URL}"
schema = "unused"
raw_tenant = "unused"

[hosted_embeddings.profiles.current.embeddings]
model = "synthetic"
dimension = 3
max_input_chars = 64
default_server = "primary"

[hosted_embeddings.profiles.current.embeddings.servers.primary]
endpoint = %q
api_key_env = "CURRENT_EMBEDDING_API_KEY"

[hosted_embeddings.profiles.unused.embeddings]
model = "unused"
dimension = 3
default_server = "primary"

[hosted_embeddings.profiles.unused.embeddings.servers.primary]
endpoint = "https://unused.example.test/v1"
api_key_env = "UNUSED_EMBEDDING_API_KEY"
`, ownerURL, cfg.PG.Schema, cfg.PG.RawTenant, cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, endpoint.URL)
	require.NoError(t, os.WriteFile(configPath, []byte(configBody), 0o600))
	t.Setenv("AGENTSVIEW_DATA_DIR", cfg.DataDir)
	t.Setenv("UNUSED_OWNER_DATABASE_URL", "")
	t.Setenv("CURRENT_EMBEDDING_API_KEY", "")
	t.Setenv("UNUSED_EMBEDDING_API_KEY", "")

	run := func(args ...string) (string, error) {
		t.Helper()
		cmd := newPGEmbeddingsCommand()
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return output.String(), err
	}

	before, err := os.ReadFile(configPath)
	require.NoError(t, err)
	output, err := run("status", "runtime", "--json")
	require.NoError(t, err)
	assert.JSONEq(t, `{"provisioned":false,"activation_ready":false,"active_available":false}`, output)
	var exists bool
	require.NoError(t, admin.QueryRowContext(t.Context(), `SELECT to_regclass(format('%I.hosted_embedding_generations',$1::text)) IS NOT NULL`, cfg.PG.Schema).Scan(&exists))
	assert.False(t, exists, "status must not provision embedding tables")
	after, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, before, after, "read-only status must not initialize config secrets")

	_, err = run("provision", "runtime", "--profile", "current", "--runtime-role", runtimeRole, "--instance-key", "initial")
	require.Error(t, err, "the restricted runtime role must not provision owner objects")
	require.NoError(t, admin.QueryRowContext(t.Context(), `SELECT to_regclass(format('%I.hosted_embedding_generations',$1::text)) IS NOT NULL`, cfg.PG.Schema).Scan(&exists))
	assert.False(t, exists, "failed owner provisioning must roll back optional embedding state")

	output, err = run("provision", "owner", "--profile", "current", "--runtime-role", runtimeRole, "--instance-key", "initial")
	require.NoError(t, err)
	assert.Contains(t, output, "Generation 1")
	assert.NotContains(t, output, "status owner")
	assert.Contains(t, output, "status RUNTIME_TARGET")
	output, err = run("provision", "owner", "--profile", "current", "--runtime-role", runtimeRole, "--instance-key", "initial")
	require.NoError(t, err)
	assert.Contains(t, output, "Generation 1", "the same instance key must preserve its generation")
	_, err = admin.ExecContext(t.Context(), `UPDATE `+cfg.PG.Schema+`.hosted_embedding_generations SET backfill_finished=true WHERE tenant_id=$1 AND id=1`, cfg.PG.RawTenant)
	require.NoError(t, err)
	_, err = run("rebuild", "owner", "--profile", "current", "--runtime-role", runtimeRole, "--instance-key", "initial")
	require.ErrorContains(t, err, "requires a new instance key")

	output, err = run("rebuild", "owner", "--profile", "current", "--runtime-role", runtimeRole, "--instance-key", "after-provider-fix")
	require.NoError(t, err)
	assert.Contains(t, output, "Generation 2", "a fresh rebuild key must select a fresh generation")
	assert.Zero(t, embeddingCalls.Load(), "owner commands must not contact the embedding provider")

	var generations int
	var desired int64
	require.NoError(t, admin.QueryRowContext(t.Context(), `SELECT count(*) FROM `+cfg.PG.Schema+`.hosted_embedding_generations`).Scan(&generations))
	require.NoError(t, admin.QueryRowContext(t.Context(), `SELECT desired_generation_id FROM `+cfg.PG.Schema+`.hosted_embedding_state WHERE tenant_id=$1`, cfg.PG.RawTenant).Scan(&desired))
	assert.Equal(t, 2, generations)
	assert.Equal(t, int64(2), desired)

	output, err = run("status", "runtime", "--json")
	require.NoError(t, err)
	var status map[string]any
	require.NoError(t, json.Unmarshal([]byte(output), &status))
	assert.Equal(t, true, status["provisioned"])
	desiredStatus, ok := status["desired"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, float64(2), desiredStatus["id"])
	assert.Contains(t, desiredStatus, "ready")
	assert.Contains(t, desiredStatus, "retry")
	assert.Contains(t, desiredStatus, "failed")
	for _, forbidden := range []string{"backfill_after_session_id", cfg.PG.RawTenant, runtimeRole, "UNUSED_EMBEDDING_API_KEY", endpoint.URL} {
		assert.NotContains(t, strings.ToLower(output), strings.ToLower(forbidden))
	}
	var generationsAfter int
	require.NoError(t, admin.QueryRowContext(t.Context(), `SELECT count(*) FROM `+cfg.PG.Schema+`.hosted_embedding_generations`).Scan(&generationsAfter))
	assert.Equal(t, generations, generationsAfter, "status must not mutate embedding generations")
	assert.Zero(t, embeddingCalls.Load(), "status must not initialize or call an embedding provider")
}
