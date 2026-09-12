//go:build pgtest

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/postgres"
)

func TestPGEmbeddingRecoveryConfiguredRuntime(t *testing.T) {
	cfg, admin := hostedRuntimeConfig(t)
	var encoderRequests atomic.Int32
	encoder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encoderRequests.Add(1)
		var request acceptanceEmbeddingRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		data := make([]map[string]any, len(request.Input))
		for i := range request.Input {
			data[i] = map[string]any{"index": i, "embedding": []float32{1, 0, 0}}
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
	}))
	defer encoder.Close()

	profiles := hostedEmbeddingTestConfig(encoder.URL+"/v1", 1)
	profile := profiles.Profiles["current"]
	server := profile.Embeddings.Servers["primary"]
	server.APIKeyEnv = "RECOVERY_SENTINEL_KEY"
	profile.Embeddings.Servers["primary"] = server
	profiles.Profiles["current"] = profile
	recipe, err := hostedEmbeddingRecipe("current", profile)
	require.NoError(t, err)
	runtimeURL, err := url.Parse(cfg.PG.URL)
	require.NoError(t, err)
	generation, err := postgres.ProvisionHostedEmbeddings(t.Context(), admin, cfg.PG.Schema, cfg.PG.RawTenant, recipe, "recovery-current", runtimeURL.User.Username())
	require.NoError(t, err)
	database, err := postgres.OpenHosted(cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, database.Close()) })
	store, err := postgres.NewHostedEmbeddingStore(t.Context(), database, postgres.HostedEmbeddingOptions{Schema: cfg.PG.Schema, Tenant: cfg.PG.RawTenant, MaxAttempts: 1})
	require.NoError(t, err)
	_, err = database.ExecContext(t.Context(), `
INSERT INTO sessions(id,project,machine,agent,provenance_kind,message_count,user_message_count)
 VALUES('recovery-source','synthetic','fixture','codex','legacy',2,2);
INSERT INTO messages(session_id,ordinal,role,content)
	VALUES('recovery-source',0,'user','original searchable text'),
	      ('recovery-source',1,'user','supporting context')`)
	require.NoError(t, err)
	publishHostedTestGeneration(t, store, generation)
	activated, err := store.Activate(t.Context(), generation.ID)
	require.NoError(t, err)
	require.True(t, activated)

	countQuery := fmt.Sprintf(`SELECT
 (SELECT count(*) FROM hosted_embedding_documents WHERE generation_id=$1),
 (SELECT count(*) FROM hosted_embedding_chunks_g%d WHERE generation_id=$1),
 (SELECT count(*) FROM hosted_embedding_values_g%d WHERE generation_id=$1)`, generation.ID, generation.ID)
	var documentsBefore, chunksBefore, valuesBefore int
	require.NoError(t, database.QueryRowContext(t.Context(), countQuery, generation.ID).Scan(&documentsBefore, &chunksBefore, &valuesBefore))
	_, err = database.ExecContext(t.Context(), `UPDATE messages SET content='recovered searchable text' WHERE session_id='recovery-source' AND ordinal=0`)
	require.NoError(t, err)
	t.Setenv("RECOVERY_SENTINEL_KEY", "")
	failedRuntime := newHostedEmbeddingRuntime(t.Context(), store, newHostedEmbeddingResolver(profiles), hostedEmbeddingRuntimeOptions{SourceConcurrency: 1, AttemptTimeout: time.Second})
	require.NoError(t, failedRuntime.processBatch(t.Context()))
	status, err := store.Status(t.Context())
	require.NoError(t, err)
	require.NotNil(t, status.Desired)
	require.Equal(t, int64(1), status.Desired.Failed)
	assert.Equal(t, map[string]int64{"encoder_unavailable": 1}, status.Desired.Errors)
	assert.Zero(t, encoderRequests.Load())

	configPath := filepath.Join(cfg.DataDir, "config.toml")
	configBody := fmt.Sprintf(`default_pg = "runtime"

[pg.runtime]
url = %q
schema = %q
raw_tenant = %q

[hosted_embeddings.profiles.current.embeddings]
model = "model-a"
dimension = 3
max_input_chars = 100
document_prefix = "doc:"
query_prefix = "query:"
input_suffix = ":end"
request_dimensions = true
default_server = "primary"

[hosted_embeddings.profiles.current.embeddings.servers.primary]
endpoint = %q
api_key_env = "RECOVERY_SENTINEL_KEY"
`, cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, encoder.URL+"/v1")
	require.NoError(t, os.WriteFile(configPath, []byte(configBody), 0o600))
	t.Setenv("AGENTSVIEW_DATA_DIR", cfg.DataDir)
	sqlitePath := filepath.Join(cfg.DataDir, "sessions.db")
	_, statErr := os.Stat(sqlitePath)
	require.ErrorIs(t, statErr, os.ErrNotExist)
	configBefore, err := os.ReadFile(configPath)
	require.NoError(t, err)

	run := func(args ...string) (string, error) {
		t.Helper()
		cmd := newPGEmbeddingsCommand()
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return output.String(), err
	}

	output, err := run("retry-failed", "runtime", "--generation", fmt.Sprint(generation.ID), "--batch-size", "1", "--format", "json")
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("{\"generation_id\":%d,\"examined\":1,\"retried\":1,\"skipped\":0}\n", generation.ID), output)
	output, err = run("retry-failed", "runtime", "--generation", fmt.Sprint(generation.ID), "--batch-size", "1", "--format", "json")
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("{\"generation_id\":%d,\"examined\":0,\"retried\":0,\"skipped\":0}\n", generation.ID), output)

	status, err = store.Status(t.Context())
	require.NoError(t, err)
	require.NotNil(t, status.Active)
	require.NotNil(t, status.Desired)
	assert.Equal(t, generation.ID, status.Active.Generation.ID)
	assert.Equal(t, generation.ID, status.Desired.Generation.ID)
	assert.Equal(t, int64(1), status.Desired.Ready, "a disabled worker leaves the recovered requirement queued")
	assert.Zero(t, status.Desired.Failed)
	var documentsAfter, chunksAfter, valuesAfter int
	require.NoError(t, database.QueryRowContext(t.Context(), countQuery, generation.ID).Scan(&documentsAfter, &chunksAfter, &valuesAfter))
	assert.Equal(t, []int{documentsBefore, chunksBefore, valuesBefore}, []int{documentsAfter, chunksAfter, valuesAfter})
	assert.Zero(t, encoderRequests.Load(), "retry must not resolve credentials or contact the encoder")
	configAfter, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, configBefore, configAfter, "retry must load configuration read-only")
	_, statErr = os.Stat(sqlitePath)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "retry must not create a SQLite archive")

	t.Setenv("RECOVERY_SENTINEL_KEY", "synthetic-key")
	cfg.HostedEmbeddings = profiles
	cfg.PG.HostedEmbeddingsEnabled = true
	cfg.PG.HostedEmbeddingsPollSeconds = 1
	cfg.PG.HostedEmbeddingsAttemptSeconds = 5
	running := startAcceptanceRunningServer(t, cfg)
	require.Eventually(t, func() bool {
		current, statusErr := store.Status(t.Context())
		return statusErr == nil && current.ActiveAvailable && current.Active != nil && current.Active.Generation.ID == generation.ID && current.Desired != nil && current.Desired.Complete == 1 && current.Desired.Ready == 0 && current.Desired.Failed == 0
	}, 10*time.Second, 50*time.Millisecond)
	code, result := acceptanceSearchCode(t, cfg, &running.startup, "recovered searchable text", "semantic")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, result.Matches, 1)
	assert.Equal(t, "recovery-source", result.Matches[0].SessionID)
	assert.Equal(t, "recovered searchable text", result.Matches[0].Snippet)
	assert.Positive(t, encoderRequests.Load())
	running.stop(t)
	_, statErr = os.Stat(sqlitePath)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestPGEmbeddingRecoverySanitizesQueryFailure(t *testing.T) {
	cfg, admin := hostedRuntimeConfig(t)
	profile := hostedEmbeddingTestConfig("http://127.0.0.1:1/v1", 1).Profiles["current"]
	recipe, err := hostedEmbeddingRecipe("current", profile)
	require.NoError(t, err)
	runtimeURL, err := url.Parse(cfg.PG.URL)
	require.NoError(t, err)
	generation, err := postgres.ProvisionHostedEmbeddings(t.Context(), admin, cfg.PG.Schema, cfg.PG.RawTenant, recipe, "query-failure", runtimeURL.User.Username())
	require.NoError(t, err)
	database, err := postgres.OpenHosted(cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, database.Close()) })
	store, err := postgres.NewHostedEmbeddingStore(t.Context(), database, postgres.HostedEmbeddingOptions{Schema: cfg.PG.Schema, Tenant: cfg.PG.RawTenant})
	require.NoError(t, err)
	_, err = database.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent) VALUES('boundary-source','synthetic','fixture','codex'); INSERT INTO messages(session_id,ordinal,role,content) VALUES('boundary-source',0,'user','boundary')`)
	require.NoError(t, err)
	for range 3 {
		_, err = store.Reconcile(t.Context(), 64)
		require.NoError(t, err)
	}
	leases, err := store.Claim(t.Context(), "boundary-worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.NoError(t, store.Fail(t.Context(), leases[0], "encoder_unavailable", false))
	_, err = admin.ExecContext(t.Context(), `REVOKE UPDATE ON `+cfg.PG.Schema+`.hosted_embedding_requirements FROM "`+runtimeURL.User.Username()+`"`)
	require.NoError(t, err)

	configBody := fmt.Sprintf(`default_pg = "runtime"

[pg.runtime]
url = %q
schema = %q
raw_tenant = %q
`, cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant)
	require.NoError(t, os.WriteFile(filepath.Join(cfg.DataDir, "config.toml"), []byte(configBody), 0o600))
	t.Setenv("AGENTSVIEW_DATA_DIR", cfg.DataDir)
	cmd := newPGEmbeddingsCommand()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"retry-failed", "runtime", "--generation", fmt.Sprint(generation.ID)})
	err = cmd.Execute()
	require.EqualError(t, err, "retrying failed hosted embeddings failed")
	for _, forbidden := range []string{"permission denied", "hosted_embedding_requirements", "boundary-source", runtimeURL.User.Username()} {
		assert.NotContains(t, fmt.Sprint(err), forbidden)
	}
}
