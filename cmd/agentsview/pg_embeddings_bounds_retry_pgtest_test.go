//go:build pgtest

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/postgres"
)

// Inject only a store operation failure; coordination and durable failure
// recording continue through the real PostgreSQL implementation.
type hostedEmbeddingStoreFault struct {
	hostedEmbeddingWorkStore
	operation string
}

func (s hostedEmbeddingStoreFault) ReadSession(ctx context.Context, lease postgres.HostedEmbeddingLease) (*postgres.HostedEmbeddingSnapshot, error) {
	if s.operation == "read" {
		return nil, errors.New("synthetic transient store failure")
	}
	return s.hostedEmbeddingWorkStore.ReadSession(ctx, lease)
}
func (s hostedEmbeddingStoreFault) ReusableVectors(ctx context.Context, snapshot *postgres.HostedEmbeddingSnapshot) ([]postgres.HostedEmbeddingVector, error) {
	if s.operation == "reuse" {
		return nil, errors.New("synthetic transient store failure")
	}
	return s.hostedEmbeddingWorkStore.ReusableVectors(ctx, snapshot)
}
func (s hostedEmbeddingStoreFault) Publish(ctx context.Context, snapshot *postgres.HostedEmbeddingSnapshot, vectors []postgres.HostedEmbeddingVector) error {
	if s.operation == "publish" {
		return errors.New("synthetic transient store failure")
	}
	return s.hostedEmbeddingWorkStore.Publish(ctx, snapshot, vectors)
}

func TestHostedEmbeddingWorkerVectorBudgetBeforeEncoding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limit  int64
		cached bool
		failed bool
	}{
		{"exact", 24, false, false}, {"over", 23, false, true}, {"cached_and_new_over", 23, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, admin := hostedRuntimeConfig(t)
			var requests atomic.Int32
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				var input struct {
					Input []string `json:"input"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
				var data []map[string]any
				for i := range input.Input {
					data = append(data, map[string]any{"index": i, "embedding": []float32{1, 0, 0}})
				}
				w.Header().Set("Content-Type", "application/json")
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
			}))
			defer endpoint.Close()
			embeddings := hostedEmbeddingTestConfig(endpoint.URL+"/v1", 1)
			recipe := hostedEmbeddingTestGeneration(t, embeddings, 1).Recipe
			target, err := url.Parse(cfg.PG.URL)
			require.NoError(t, err)
			generation, err := postgres.ProvisionHostedEmbeddings(t.Context(), admin, cfg.PG.Schema, cfg.PG.RawTenant, recipe, "initial", target.User.Username())
			require.NoError(t, err)
			database, err := postgres.OpenHosted(cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
			require.NoError(t, err)
			defer database.Close()
			store, err := postgres.NewHostedEmbeddingStore(t.Context(), database, postgres.HostedEmbeddingOptions{Schema: cfg.PG.Schema, Tenant: cfg.PG.RawTenant, Limits: postgres.HostedEmbeddingLimits{MaxVectorBytes: tc.limit}})
			require.NoError(t, err)
			_, err = database.Exec(`INSERT INTO sessions(id,project,machine,agent) VALUES('s','synthetic','fixture','codex'); INSERT INTO messages(session_id,ordinal,role,content) VALUES('s',0,'user','cached')`)
			require.NoError(t, err)
			if tc.cached {
				publishHostedTestGeneration(t, store, generation)
			}
			_, err = database.Exec(`INSERT INTO messages(session_id,ordinal,role,content) VALUES('s',1,'user','new')`)
			require.NoError(t, err)
			runtime := newHostedEmbeddingRuntime(t.Context(), store, newHostedEmbeddingResolver(embeddings), hostedEmbeddingRuntimeOptions{})
			require.NoError(t, runtime.processBatch(t.Context()))
			status, err := store.Status(t.Context())
			require.NoError(t, err)
			require.NotNil(t, status.Desired)
			if tc.failed {
				assert.Equal(t, int64(1), status.Desired.Failed)
				assert.Equal(t, map[string]int64{"work_limit": 1}, status.Desired.Errors)
				assert.Zero(t, requests.Load(), "reject the full result before any uncached encoder request")
			} else {
				assert.Equal(t, int64(1), status.Desired.Complete)
				assert.Equal(t, int32(1), requests.Load())
			}
		})
	}
}

func TestHostedEmbeddingWorkerStoreFailureRetriesDurably(t *testing.T) {
	for _, operation := range []string{"read", "reuse", "publish"} {
		t.Run(operation, func(t *testing.T) {
			cfg, admin := hostedRuntimeConfig(t)
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0,0]}]}`))
			}))
			defer endpoint.Close()
			embeddings := hostedEmbeddingTestConfig(endpoint.URL+"/v1", 1)
			recipe := hostedEmbeddingTestGeneration(t, embeddings, 1).Recipe
			target, err := url.Parse(cfg.PG.URL)
			require.NoError(t, err)
			generation, err := postgres.ProvisionHostedEmbeddings(t.Context(), admin, cfg.PG.Schema, cfg.PG.RawTenant, recipe, "initial", target.User.Username())
			require.NoError(t, err)
			database, err := postgres.OpenHosted(cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
			require.NoError(t, err)
			defer database.Close()
			options := postgres.HostedEmbeddingOptions{Schema: cfg.PG.Schema, Tenant: cfg.PG.RawTenant, MaxAttempts: 2, RetryDelay: time.Millisecond}
			store, err := postgres.NewHostedEmbeddingStore(t.Context(), database, options)
			require.NoError(t, err)
			_, err = database.Exec(`INSERT INTO sessions(id,project,machine,agent) VALUES('s','synthetic','fixture','codex'); INSERT INTO messages(session_id,ordinal,role,content) VALUES('s',0,'user','recover')`)
			require.NoError(t, err)
			fault := hostedEmbeddingStoreFault{store, operation}
			first := newHostedEmbeddingRuntime(t.Context(), fault, newHostedEmbeddingResolver(embeddings), hostedEmbeddingRuntimeOptions{})
			require.NoError(t, first.processBatch(t.Context()))
			status, err := store.Status(t.Context())
			require.NoError(t, err)
			require.NotNil(t, status.Desired)
			require.Equal(t, int64(1), status.Desired.Retry)
			assert.Zero(t, status.Desired.Failed)
			// Reopen the durable store and recover on the same generation and revision.
			require.NoError(t, database.Close())
			database, err = postgres.OpenHosted(cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
			require.NoError(t, err)
			defer database.Close()
			store, err = postgres.NewHostedEmbeddingStore(t.Context(), database, options)
			require.NoError(t, err)
			second := newHostedEmbeddingRuntime(t.Context(), store, newHostedEmbeddingResolver(embeddings), hostedEmbeddingRuntimeOptions{})
			require.Eventually(t, func() bool {
				if second.processBatch(t.Context()) != nil {
					return false
				}
				active, e := store.Active(t.Context())
				return e == nil && active != nil && active.ID == generation.ID
			}, time.Second, 10*time.Millisecond)
			var attempts int
			require.NoError(t, database.QueryRow(`SELECT attempts FROM hosted_embedding_requirements WHERE session_id='s'`).Scan(&attempts))
			assert.Equal(t, 2, attempts)
			// A new source still exhausts the same finite ceiling if the fault persists.
			_, err = database.Exec(`INSERT INTO sessions(id,project,machine,agent) VALUES('exhaust','synthetic','fixture','codex'); INSERT INTO messages(session_id,ordinal,role,content) VALUES('exhaust',0,'user','exhaust')`)
			require.NoError(t, err)
			exhausted := newHostedEmbeddingRuntime(t.Context(), hostedEmbeddingStoreFault{store, operation}, newHostedEmbeddingResolver(embeddings), hostedEmbeddingRuntimeOptions{})
			require.Eventually(t, func() bool {
				if exhausted.processBatch(t.Context()) != nil {
					return false
				}
				var state string
				if database.QueryRow(`SELECT state,attempts FROM hosted_embedding_requirements WHERE session_id='exhaust'`).Scan(&state, &attempts) != nil {
					return false
				}
				return state == "failed"
			}, time.Second, 10*time.Millisecond)
			assert.Equal(t, 2, attempts)
			require.NoError(t, exhausted.processBatch(t.Context()))
			require.NoError(t, database.QueryRow(`SELECT attempts FROM hosted_embedding_requirements WHERE session_id='exhaust'`).Scan(&attempts))
			assert.Equal(t, 2, attempts)
		})
	}
}
