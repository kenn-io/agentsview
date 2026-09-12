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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/postgres"
)

func writePGEmbeddingCutoverConfig(t *testing.T, cfg config.Config, defaultTarget, endpoint, nextModel, nextKeyEnv string) []byte {
	t.Helper()
	body := fmt.Sprintf(`default_pg = %q

[pg.runtime]
url = %q
schema = %q
raw_tenant = %q

[pg.unused]
url = "postgres://unused@127.0.0.1:1/unused?sslmode=disable"
schema = "unused"
raw_tenant = "unused"

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
api_key_env = "CUTOVER_EMBEDDING_KEY"

[hosted_embeddings.profiles.next.embeddings]
model = %q
dimension = 3
max_input_chars = 100
document_prefix = "doc:"
query_prefix = "query:"
input_suffix = ":end"
request_dimensions = true
default_server = "primary"

[hosted_embeddings.profiles.next.embeddings.servers.primary]
endpoint = %q
api_key_env = %q
`, defaultTarget, cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, endpoint, nextModel, endpoint, nextKeyEnv)
	require.NoError(t, os.WriteFile(filepath.Join(cfg.DataDir, "config.toml"), []byte(body), 0o600))
	return []byte(body)
}

func runPGEmbeddingCutoverTestCommand(t *testing.T, args ...string) (string, error) {
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

func processHostedEmbeddingUntil(t *testing.T, runtime *hostedEmbeddingRuntime, predicate func() bool) {
	t.Helper()
	require.Eventually(t, func() bool {
		if err := runtime.processBatch(t.Context()); err != nil {
			return false
		}
		return predicate()
	}, 10*time.Second, 20*time.Millisecond)
}

// Removing the manual worker hold, selected-profile validation, atomic store
// readiness gate, or public active-generation binding breaks this fixture.
func TestPGEmbeddingManualCutoverChangesPublicSearchOnlyAfterExplicitActivation(t *testing.T) {
	encoder := &acceptanceEncoder{
		models: map[string]int{"model-a": 3, "model-b": 3},
		vectors: map[string][]float32{
			"model-a\x00doc:alpha anchor:end":     {1, 0, 0},
			"model-a\x00doc:alpha context:end":    {1, 0, 0},
			"model-a\x00doc:beta anchor:end":      {0, 1, 0},
			"model-a\x00doc:beta context:end":     {0, 1, 0},
			"model-a\x00query:cutover choice:end": {1, 0, 0},
			"model-b\x00doc:alpha anchor:end":     {1, 0, 0},
			"model-b\x00doc:alpha context:end":    {1, 0, 0},
			"model-b\x00doc:beta anchor:end":      {0, 1, 0},
			"model-b\x00doc:beta context:end":     {0, 1, 0},
			"model-b\x00query:cutover choice:end": {0, 1, 0},
		},
	}
	endpoint := httptest.NewServer(encoder)
	defer endpoint.Close()

	profiles := acceptanceProfile(endpoint.URL + "/v1")
	next := profiles.Profiles["current"]
	next.Embeddings.Model = "model-b"
	profiles.Profiles["next"] = next
	cfg, admin := hostedRuntimeConfig(t)
	cfg.HostedEmbeddings = profiles
	t.Setenv("AGENTSVIEW_DATA_DIR", cfg.DataDir)
	t.Setenv("CUTOVER_EMBEDDING_KEY", "synthetic-key")
	t.Setenv("CUTOVER_MISSING_KEY", "")
	writePGEmbeddingCutoverConfig(t, cfg, "unused", endpoint.URL+"/v1", "model-b", "CUTOVER_EMBEDDING_KEY")
	sqlitePath := filepath.Join(cfg.DataDir, "sessions.db")

	runtimeURL, err := url.Parse(cfg.PG.URL)
	require.NoError(t, err)
	runtimeRole := runtimeURL.User.Username()
	currentProfile, err := profiles.Profile("current")
	require.NoError(t, err)
	currentRecipe, err := hostedEmbeddingRecipe("current", currentProfile)
	require.NoError(t, err)
	generationA, err := postgres.ProvisionHostedEmbeddingsWithOptions(t.Context(), admin, cfg.PG.Schema, cfg.PG.RawTenant, currentRecipe, "cutover-a", runtimeRole, postgres.HostedEmbeddingProvisionOptions{ActivationMode: postgres.HostedEmbeddingActivationAutomatic})
	require.NoError(t, err)
	database, err := postgres.OpenHosted(cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, database.Close()) })
	_, err = database.ExecContext(t.Context(), `
INSERT INTO sessions(id,project,machine,agent,provenance_kind,message_count,user_message_count)
 VALUES ('alpha-session','project','machine','codex','legacy',2,2),
        ('beta-session','project','machine','codex','legacy',2,2);
INSERT INTO messages(session_id,ordinal,role,content) VALUES
 ('alpha-session',0,'user','alpha anchor'),('alpha-session',1,'user','alpha context'),
 ('beta-session',0,'user','beta anchor'),('beta-session',1,'user','beta context')`)
	require.NoError(t, err)
	store, err := postgres.NewHostedEmbeddingStore(t.Context(), database, postgres.HostedEmbeddingOptions{Schema: cfg.PG.Schema, Tenant: cfg.PG.RawTenant})
	require.NoError(t, err)
	workerA := newHostedEmbeddingRuntime(t.Context(), store, newHostedEmbeddingResolver(profiles), hostedEmbeddingRuntimeOptions{SourceConcurrency: 4, AttemptTimeout: time.Second})
	processHostedEmbeddingUntil(t, workerA, func() bool {
		active, activeErr := store.Active(t.Context())
		return activeErr == nil && active != nil && active.ID == generationA.ID
	})

	nextProfile, err := profiles.Profile("next")
	require.NoError(t, err)
	nextRecipe, err := hostedEmbeddingRecipe("next", nextProfile)
	require.NoError(t, err)
	generationB, err := postgres.ProvisionHostedEmbeddingsWithOptions(t.Context(), admin, cfg.PG.Schema, cfg.PG.RawTenant, nextRecipe, "cutover-b", runtimeRole, postgres.HostedEmbeddingProvisionOptions{ActivationMode: postgres.HostedEmbeddingActivationManual})
	require.NoError(t, err)

	for _, tc := range []struct {
		name       string
		generation int64
		want       string
	}{
		{name: "unknown", generation: generationB.ID + 100, want: errPGEmbeddingActivationSelection.Error()},
		{name: "non desired", generation: generationA.ID, want: errPGEmbeddingActivationSelection.Error()},
		{name: "incomplete", generation: generationB.ID, want: errPGEmbeddingActivationNotReady.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, commandErr := runPGEmbeddingCutoverTestCommand(t, "activate", "runtime", "--generation", fmt.Sprint(tc.generation))
			require.EqualError(t, commandErr, tc.want)
			active, activeErr := store.Active(t.Context())
			require.NoError(t, activeErr)
			require.NotNil(t, active)
			assert.Equal(t, generationA.ID, active.ID)
		})
	}

	workerB := newHostedEmbeddingRuntime(t.Context(), store, newHostedEmbeddingResolver(profiles), hostedEmbeddingRuntimeOptions{SourceConcurrency: 4, AttemptTimeout: time.Second})
	processHostedEmbeddingUntil(t, workerB, func() bool {
		status, statusErr := store.Status(t.Context())
		return statusErr == nil && status.ActivationReady && status.Active != nil && status.Active.Generation.ID == generationA.ID && status.Desired != nil && status.Desired.Generation.ID == generationB.ID && status.Desired.Generation.ActivationMode == postgres.HostedEmbeddingActivationManual
	})
	restartedWorker := newHostedEmbeddingRuntime(t.Context(), store, newHostedEmbeddingResolver(profiles), hostedEmbeddingRuntimeOptions{SourceConcurrency: 4, AttemptTimeout: time.Second})
	require.NoError(t, restartedWorker.processBatch(t.Context()))
	active, err := store.Active(t.Context())
	require.NoError(t, err)
	require.NotNil(t, active)
	assert.Equal(t, generationA.ID, active.ID, "a restarted worker must preserve the manual hold")
	statusOutput, commandErr := runPGEmbeddingCutoverTestCommand(t, "status", "runtime", "--format", "json")
	require.NoError(t, commandErr)
	var statusDTO hostedEmbeddingStatusDTO
	require.NoError(t, json.Unmarshal([]byte(statusOutput), &statusDTO))
	require.NotNil(t, statusDTO.Active)
	require.NotNil(t, statusDTO.Desired)
	assert.Equal(t, postgres.HostedEmbeddingActivationAutomatic, statusDTO.Active.ActivationMode)
	assert.Equal(t, postgres.HostedEmbeddingActivationManual, statusDTO.Desired.ActivationMode)
	assert.True(t, statusDTO.ActivationReady)

	requestsBeforeRefusals, encoderErrors := encoder.snapshot()
	require.Empty(t, encoderErrors)
	var documentsBefore int
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_embedding_documents`).Scan(&documentsBefore))

	_, err = database.ExecContext(t.Context(), `UPDATE hosted_embedding_requirements SET state='failed',error_code='invalid_results' WHERE generation_id=$1 AND session_id='alpha-session'`, generationB.ID)
	require.NoError(t, err)
	_, commandErr = runPGEmbeddingCutoverTestCommand(t, "activate", "runtime", "--generation", fmt.Sprint(generationB.ID))
	require.EqualError(t, commandErr, errPGEmbeddingActivationNotReady.Error())
	_, err = database.ExecContext(t.Context(), `UPDATE hosted_embedding_requirements SET state='complete',completed_revision=required_revision,error_code='' WHERE generation_id=$1 AND session_id='alpha-session'`, generationB.ID)
	require.NoError(t, err)

	_, err = database.ExecContext(t.Context(), `UPDATE hosted_embedding_sources SET dirty=true WHERE tenant_id=current_setting('agentsview.tenant_id')`)
	require.NoError(t, err)
	_, commandErr = runPGEmbeddingCutoverTestCommand(t, "activate", "runtime", "--generation", fmt.Sprint(generationB.ID))
	require.EqualError(t, commandErr, errPGEmbeddingActivationNotReady.Error())
	_, err = database.ExecContext(t.Context(), `UPDATE hosted_embedding_sources SET dirty=false WHERE tenant_id=current_setting('agentsview.tenant_id')`)
	require.NoError(t, err)

	mismatchBody := writePGEmbeddingCutoverConfig(t, cfg, "unused", endpoint.URL+"/v1", "model-mismatch", "CUTOVER_EMBEDDING_KEY")
	_, commandErr = runPGEmbeddingCutoverTestCommand(t, "activate", "runtime", "--generation", fmt.Sprint(generationB.ID))
	require.EqualError(t, commandErr, errPGEmbeddingActivationAvailability.Error())
	for _, forbidden := range []string{endpoint.URL, "CUTOVER_MISSING_KEY", "alpha-session", cfg.PG.RawTenant} {
		assert.NotContains(t, commandErr.Error(), forbidden)
	}
	afterMismatch, err := os.ReadFile(filepath.Join(cfg.DataDir, "config.toml"))
	require.NoError(t, err)
	assert.Equal(t, mismatchBody, afterMismatch)

	missingBody := writePGEmbeddingCutoverConfig(t, cfg, "unused", endpoint.URL+"/v1", "model-b", "CUTOVER_MISSING_KEY")
	_, commandErr = runPGEmbeddingCutoverTestCommand(t, "activate", "runtime", "--generation", fmt.Sprint(generationB.ID))
	require.EqualError(t, commandErr, errPGEmbeddingActivationAvailability.Error())
	afterMissing, err := os.ReadFile(filepath.Join(cfg.DataDir, "config.toml"))
	require.NoError(t, err)
	assert.Equal(t, missingBody, afterMissing)

	requestsAfterRefusals, encoderErrors := encoder.snapshot()
	require.Empty(t, encoderErrors)
	assert.Len(t, requestsAfterRefusals, len(requestsBeforeRefusals), "activation refusals must not contact the encoder")
	active, err = store.Active(t.Context())
	require.NoError(t, err)
	require.NotNil(t, active)
	assert.Equal(t, generationA.ID, active.ID)
	var documentsAfterRefusals int
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_embedding_documents`).Scan(&documentsAfterRefusals))
	assert.Equal(t, documentsBefore, documentsAfterRefusals)

	namedConfigBody := writePGEmbeddingCutoverConfig(t, cfg, "unused", endpoint.URL+"/v1", "model-b", "CUTOVER_EMBEDDING_KEY")
	readerCfg := cfg
	readerCfg.PG.HostedEmbeddingsEnabled = false
	reader := startAcceptanceRunningServer(t, readerCfg)
	code, result := acceptanceSearchCode(t, readerCfg, &reader.startup, "cutover choice", "semantic")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, result.Matches, 1)
	assert.Equal(t, "alpha-session", result.Matches[0].SessionID)
	requestsBeforeActivation, encoderErrors := encoder.snapshot()
	require.Empty(t, encoderErrors)

	output, commandErr := runPGEmbeddingCutoverTestCommand(t, "activate", "runtime", "--generation", fmt.Sprint(generationB.ID), "--format", "json")
	require.NoError(t, commandErr)
	assert.Equal(t, fmt.Sprintf("{\"generation_id\":%d,\"activated\":true}\n", generationB.ID), output)
	afterNamedActivation, err := os.ReadFile(filepath.Join(cfg.DataDir, "config.toml"))
	require.NoError(t, err)
	assert.Equal(t, namedConfigBody, afterNamedActivation, "named-target activation must load configuration read-only")
	requestsAfterActivation, encoderErrors := encoder.snapshot()
	require.Empty(t, encoderErrors)
	assert.Len(t, requestsAfterActivation, len(requestsBeforeActivation), "activation must not contact the encoder")

	code, result = acceptanceSearchCode(t, readerCfg, &reader.startup, "cutover choice", "semantic")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, result.Matches, 1)
	assert.Equal(t, "beta-session", result.Matches[0].SessionID)
	requestsBeforeRepeat, encoderErrors := encoder.snapshot()
	require.Empty(t, encoderErrors)
	defaultConfigBody := writePGEmbeddingCutoverConfig(t, cfg, "runtime", endpoint.URL+"/v1", "model-b", "CUTOVER_EMBEDDING_KEY")
	output, commandErr = runPGEmbeddingCutoverTestCommand(t, "activate", "--generation", fmt.Sprint(generationB.ID), "--format", "json")
	require.NoError(t, commandErr)
	assert.Equal(t, fmt.Sprintf("{\"generation_id\":%d,\"activated\":true}\n", generationB.ID), output)
	requestsAfterRepeat, encoderErrors := encoder.snapshot()
	require.Empty(t, encoderErrors)
	assert.Len(t, requestsAfterRepeat, len(requestsBeforeRepeat), "repeated activation must not contact the encoder")

	finalConfig, err := os.ReadFile(filepath.Join(cfg.DataDir, "config.toml"))
	require.NoError(t, err)
	assert.Equal(t, defaultConfigBody, finalConfig, "default-target activation must load configuration read-only")
	_, statErr := os.Stat(sqlitePath)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "activation must not create a SQLite archive")
	active, err = store.Active(t.Context())
	require.NoError(t, err)
	require.NotNil(t, active)
	assert.Equal(t, generationB.ID, active.ID)
}
