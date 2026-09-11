//go:build pgtest

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/postgres"
)

// This is the configured-runtime seam between transport validation and the
// durable PostgreSQL requirement state. Store-level tests cover every invalid
// float shape; worker unit tests cover rate limits, deadlines, and cancellation.
func TestHostedEmbeddingConfiguredRuntimeRejectsInvalidEncoderCoverage(t *testing.T) {
	tests := []struct {
		name, response, errorCode string
	}{
		{"wrong_dimension", `{"data":[{"index":0,"embedding":[1,0]}]}`, "encoder_unavailable"},
		{"zero_vector", `{"data":[{"index":0,"embedding":[0,0,0]}]}`, "invalid_results"},
		{"partial_response", `{"data":[]}`, "encoder_unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var requests []acceptanceEmbeddingRequest
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request acceptanceEmbeddingRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					http.Error(w, "invalid synthetic request", http.StatusBadRequest)
					return
				}
				mu.Lock()
				requests = append(requests, request)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.response))
			}))
			defer endpoint.Close()

			cfg, admin := hostedRuntimeConfig(t)
			cfg.HostedEmbeddings = acceptanceProfile(endpoint.URL + "/v1")
			cfg.PG.HostedEmbeddingsEnabled = true
			cfg.PG.HostedEmbeddingsPollSeconds = 1
			cfg.PG.HostedEmbeddingsAttemptSeconds = 2
			cfg.PG.HostedEmbeddingsMaxAttempts = 1
			profile, err := cfg.HostedEmbeddings.Profile("current")
			require.NoError(t, err)
			recipe, err := hostedEmbeddingRecipe("current", profile)
			require.NoError(t, err)
			runtimeURL, err := url.Parse(cfg.PG.URL)
			require.NoError(t, err)
			generation, err := postgres.ProvisionHostedEmbeddings(t.Context(), admin, cfg.PG.Schema, cfg.PG.RawTenant, recipe, "invalid-coverage", runtimeURL.User.Username())
			require.NoError(t, err)
			database, err := postgres.OpenHosted(cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, database.Close()) })
			_, err = database.ExecContext(t.Context(), `
INSERT INTO sessions(id,project,machine,agent,provenance_kind,message_count,user_message_count)
 VALUES('invalid-source','synthetic','fixture','codex','legacy',1,1);
INSERT INTO messages(session_id,ordinal,role,content)
 VALUES('invalid-source',0,'user','invalid source')`)
			require.NoError(t, err)
			observer, err := postgres.NewHostedEmbeddingStore(t.Context(), database, postgres.HostedEmbeddingOptions{Schema: cfg.PG.Schema, Tenant: cfg.PG.RawTenant, MaxAttempts: 1})
			require.NoError(t, err)

			running := startAcceptanceRunningServer(t, cfg)
			require.Eventually(t, func() bool {
				status, statusErr := observer.Status(t.Context())
				return statusErr == nil && status.Desired != nil && status.Desired.Failed == 1 && status.Desired.Errors[tt.errorCode] == 1
			}, 10*time.Second, 50*time.Millisecond)
			running.stop(t)

			status, err := observer.Status(t.Context())
			require.NoError(t, err)
			assert.False(t, status.ActivationReady)
			assert.Nil(t, status.Active)
			var documents int
			require.NoError(t, database.QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_embedding_documents WHERE generation_id=$1`, generation.ID).Scan(&documents))
			assert.Zero(t, documents)
			mu.Lock()
			gotRequests := append([]acceptanceEmbeddingRequest(nil), requests...)
			mu.Unlock()
			require.NotEmpty(t, gotRequests)
			for _, request := range gotRequests {
				assert.Equal(t, acceptanceEmbeddingRequest{Model: "model-a", Input: []string{"doc:invalid source:end"}, Dimensions: 3}, request)
			}
		})
	}
}
