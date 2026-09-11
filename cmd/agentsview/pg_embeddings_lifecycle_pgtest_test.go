//go:build pgtest

package main

import (
	"context"
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
	"go.kenn.io/agentsview/internal/service"
)

type acceptanceRunningServer struct {
	startup  pgServeStartup
	cancel   context.CancelFunc
	finished chan error
	stopped  bool
}

func startAcceptanceRunningServer(t *testing.T, cfg config.Config) *acceptanceRunningServer {
	t.Helper()
	startup, err := preparePGServeImpl(cfg, "")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(startup.ctx)
	startup.ctx = ctx
	running := &acceptanceRunningServer{startup: startup, cancel: cancel, finished: make(chan error, 1)}
	go func() { running.finished <- runPreparedPGServe(running.startup) }()
	t.Cleanup(func() { running.stop(t) })
	return running
}

func (r *acceptanceRunningServer) stop(t *testing.T) {
	t.Helper()
	if r.stopped {
		return
	}
	r.stopped = true
	r.cancel()
	select {
	case err := <-r.finished:
		assert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Error("configured hosted runtime failed to join")
	}
	r.startup.cleanup()
}

func acceptanceSearchCode(t *testing.T, cfg config.Config, startup *pgServeStartup, pattern, mode string) (int, service.ContentSearchResult) {
	t.Helper()
	path := "/api/v1/search/content?pattern=" + url.QueryEscape(pattern) + "&mode=" + mode + "&limit=1"
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+path, nil)
	req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	if mode == "semantic" || mode == "hybrid" {
		req.Header.Set(service.SemanticSearchIntentHeader, service.SemanticSearchIntentValue)
	}
	rec := httptest.NewRecorder()
	startup.srv.Handler().ServeHTTP(rec, req)
	var result service.ContentSearchResult
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	}
	return rec.Code, result
}

func waitAcceptanceGeneration(t *testing.T, store *postgres.HostedEmbeddingStore, id int64) {
	t.Helper()
	require.Eventually(t, func() bool {
		active, err := store.Active(t.Context())
		return err == nil && active != nil && active.ID == id
	}, 20*time.Second, 50*time.Millisecond)
}

// Switching activation before B is complete, using B's dimensions with A's
// model, or treating cleared content as covered breaks an observable phase.
func TestHostedEmbeddingConfiguredMigrationRestartAndUsageOnlyRebuild(t *testing.T) {
	releaseA := make(chan struct{})
	releaseB := make(chan struct{})
	holdA := &acceptanceEncoderHold{started: make(chan struct{}), release: releaseA}
	holdB := &acceptanceEncoderHold{started: make(chan struct{}), release: releaseB}
	encoder := &acceptanceEncoder{
		models: map[string]int{"model-a": 3, "model-b": 4},
		vectors: map[string][]float32{
			"model-a\x00doc:alpha topic:end":         {1, 0, 0},
			"model-a\x00doc:alpha revised:end":       {1, 1, 0},
			"model-a\x00doc:alpha response:end":      {0, 1, 0},
			"model-a\x00doc:background:end":          {0, 0, 1},
			"model-a\x00doc:background response:end": {0, 0, 1},
			"model-a\x00doc:control topic:end":       {.1, 1, 1},
			"model-a\x00doc:control response:end":    {0, 1, 0},
			"model-a\x00doc:control context:end":     {0, 1, 0},
			"model-a\x00doc:control ready:end":       {0, 1, 0},
			"model-a\x00query:alpha search:end":      {1, 0, 0},
			"model-a\x00query:revised search:end":    {1, 1, 0},
			"model-a\x00query:control search:end":    {.1, 1, 1},
			"model-b\x00doc:alpha revised:end":       {0, 1, 1, 0},
			"model-b\x00doc:alpha response:end":      {1, 0, 0, 0},
			"model-b\x00doc:background:end":          {0, 0, 1, 0},
			"model-b\x00doc:background response:end": {0, 0, 0, 1},
			"model-b\x00doc:control topic:end":       {0, 0, 1, 0},
			"model-b\x00doc:control response:end":    {0, 0, 0, 1},
			"model-b\x00doc:control context:end":     {0, 0, 1, 0},
			"model-b\x00doc:control ready:end":       {0, 0, 0, 1},
			"model-b\x00query:migration search:end":  {0, 1, 1, 0},
			"model-b\x00query:rebuilt search:end":    {0, 1, 1, 0},
		},
		holds: map[string]*acceptanceEncoderHold{
			"model-a\x00doc:alpha revised:end": holdA,
			"model-b\x00doc:alpha revised:end": holdB,
		},
	}
	endpoint := httptest.NewServer(encoder)
	defer endpoint.Close()
	profiles := acceptanceProfile(endpoint.URL + "/v1")
	next := profiles.Profiles["current"]
	next.Embeddings.Model = "model-b"
	next.Embeddings.Dimension = 4
	profiles.Profiles["next"] = next

	cfg, admin := hostedRuntimeConfig(t)
	cfg.HostedEmbeddings = profiles
	cfg.PG.HostedEmbeddingsEnabled = true
	cfg.PG.HostedEmbeddingsPollSeconds = 1
	cfg.PG.HostedEmbeddingsAttemptSeconds = 5
	cfg.PG.HostedEmbeddingsMaxAttempts = 3
	cfg.DBPath = filepath.Join(cfg.DataDir, "sessions.db")
	require.NoError(t, os.Mkdir(cfg.DBPath, 0700))
	sentinel := filepath.Join(cfg.DBPath, "sentinel")
	require.NoError(t, os.WriteFile(sentinel, []byte("no SQLite archive"), 0600))
	t.Cleanup(func() {
		body, err := os.ReadFile(sentinel)
		assert.NoError(t, err)
		assert.Equal(t, "no SQLite archive", string(body))
	})
	runtimeURL, err := url.Parse(cfg.PG.URL)
	require.NoError(t, err)
	profileA, err := profiles.Profile("current")
	require.NoError(t, err)
	recipeA, err := hostedEmbeddingRecipe("current", profileA)
	require.NoError(t, err)
	generationA, err := postgres.ProvisionHostedEmbeddings(t.Context(), admin, cfg.PG.Schema, cfg.PG.RawTenant, recipeA, "migration-a", runtimeURL.User.Username())
	require.NoError(t, err)
	database, err := postgres.OpenHosted(cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, database.Close()) })
	_, err = database.ExecContext(t.Context(), `
INSERT INTO sessions(id,project,machine,agent,provenance_kind,message_count,user_message_count)
 VALUES
 ('migration-source','project','machine','codex','legacy',4,2),
 ('control-source','project','machine','codex','legacy',4,2);
INSERT INTO messages(session_id,ordinal,role,content) VALUES
 ('migration-source',0,'user','alpha topic'),
 ('migration-source',1,'assistant','alpha response'),
 ('migration-source',2,'user','background'),
 ('migration-source',3,'assistant','background response'),
 ('control-source',0,'user','control topic'),
 ('control-source',1,'assistant','control response'),
 ('control-source',2,'user','control context'),
 ('control-source',3,'assistant','control ready')`)
	require.NoError(t, err)
	observer, err := postgres.NewHostedEmbeddingStore(t.Context(), database, postgres.HostedEmbeddingOptions{Schema: cfg.PG.Schema, Tenant: cfg.PG.RawTenant, MaxAttempts: 3})
	require.NoError(t, err)

	runA := startAcceptanceRunningServer(t, cfg)
	waitAcceptanceGeneration(t, observer, generationA.ID)
	readerCfg := cfg
	readerCfg.PG.HostedEmbeddingsEnabled = false
	reader := startAcceptanceRunningServer(t, readerCfg)
	require.Nil(t, reader.startup.startWorker)
	code, result := acceptanceSearchCode(t, readerCfg, &reader.startup, "alpha search", "semantic")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, result.Matches, 1)
	assert.Equal(t, "migration-source", result.Matches[0].SessionID)
	assert.Equal(t, 0, result.Matches[0].Ordinal)

	_, err = database.ExecContext(t.Context(), `UPDATE messages SET content='alpha revised' WHERE session_id='migration-source' AND ordinal=0`)
	require.NoError(t, err)
	select {
	case <-holdA.started:
	case <-time.After(10 * time.Second):
		t.Fatal("edited legacy source did not reach the controlled encoder barrier")
	}
	code, result = acceptanceSearchCode(t, readerCfg, &reader.startup, "alpha search", "semantic")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, result.Matches, 1)
	assert.Equal(t, "control-source", result.Matches[0].SessionID, "edited source must hide all prior vectors before replacement is published")
	code, result = acceptanceSearchCode(t, readerCfg, &reader.startup, "control search", "semantic")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, result.Matches, 1)
	assert.Equal(t, "control-source", result.Matches[0].SessionID, "an unrelated active source must remain searchable")
	close(releaseA)
	require.Eventually(t, func() bool {
		code, result = acceptanceSearchCode(t, readerCfg, &reader.startup, "revised search", "semantic")
		return code == http.StatusOK && len(result.Matches) == 1 && result.Matches[0].SessionID == "migration-source" && result.Matches[0].Ordinal == 0 && result.Matches[0].Snippet == "alpha revised"
	}, 10*time.Second, 50*time.Millisecond)

	profileB, err := profiles.Profile("next")
	require.NoError(t, err)
	recipeB, err := hostedEmbeddingRecipe("next", profileB)
	require.NoError(t, err)
	generationB, err := postgres.ProvisionHostedEmbeddings(t.Context(), admin, cfg.PG.Schema, cfg.PG.RawTenant, recipeB, "migration-b", runtimeURL.User.Username())
	require.NoError(t, err)
	select {
	case <-holdB.started:
	case <-time.After(10 * time.Second):
		t.Fatal("model B document encoding did not reach the controlled barrier")
	}
	active, err := observer.Active(t.Context())
	require.NoError(t, err)
	require.NotNil(t, active)
	assert.Equal(t, generationA.ID, active.ID)
	code, result = acceptanceSearchCode(t, readerCfg, &reader.startup, "revised search", "semantic")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, result.Matches, 1)
	assert.Equal(t, "migration-source", result.Matches[0].SessionID)
	assert.Equal(t, "alpha revised", result.Matches[0].Snippet)

	runA.stop(t)
	close(releaseB)
	var interruptedState string
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT state FROM hosted_embedding_requirements WHERE generation_id=$1 AND session_id='migration-source'`, generationB.ID).Scan(&interruptedState))
	assert.Equal(t, "leased", interruptedState, "a crashed worker leaves its fenced lease for takeover")
	_, err = admin.ExecContext(t.Context(), `UPDATE hosted_embedding_requirements SET lease_expires_at='2000-01-01' WHERE generation_id=$1 AND session_id='migration-source'`, generationB.ID)
	require.NoError(t, err)
	runB := startAcceptanceRunningServer(t, cfg)
	waitAcceptanceGeneration(t, observer, generationB.ID)
	requestsBeforeBQuery, _ := encoder.snapshot()
	code, result = acceptanceSearchCode(t, readerCfg, &reader.startup, "migration search", "semantic")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, result.Matches, 1)
	assert.Equal(t, "migration-source", result.Matches[0].SessionID)
	assert.Equal(t, 0, result.Matches[0].Ordinal)
	requestsAfterBQuery, _ := encoder.snapshot()
	require.Len(t, requestsAfterBQuery, len(requestsBeforeBQuery)+1)
	assert.Equal(t, acceptanceEmbeddingRequest{Model: "model-b", Input: []string{"query:migration search:end"}, Dimensions: 4}, requestsAfterBQuery[len(requestsBeforeBQuery)])
	runB.stop(t)

	req := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1/api/v1/sessions/migration-source", nil)
	req.Header.Set("Authorization", "Bearer "+readerCfg.AuthToken)
	rec := httptest.NewRecorder()
	reader.startup.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	code, result = acceptanceSearchCode(t, readerCfg, &reader.startup, "migration search", "semantic")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, result.Matches, 1)
	assert.Equal(t, "control-source", result.Matches[0].SessionID, "trash must hide active vectors without an embedding worker reconciliation")
	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/sessions/migration-source/restore", nil)
	req.Header.Set("Authorization", "Bearer "+readerCfg.AuthToken)
	rec = httptest.NewRecorder()
	reader.startup.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	reader.stop(t)

	missing := cfg
	missing.HostedEmbeddings = acceptanceProfile(endpoint.URL + "/v1")
	missing.PG.HostedEmbeddingsEnabled = false
	unavailable := startAcceptanceRunningServer(t, missing)
	code, _ = acceptanceSearchCode(t, missing, &unavailable.startup, "migration search", "semantic")
	assert.Equal(t, http.StatusNotImplemented, code)
	code, result = acceptanceSearchCode(t, missing, &unavailable.startup, "alpha", "fts")
	require.Equal(t, http.StatusOK, code)
	require.NotEmpty(t, result.Matches)
	assert.Equal(t, "migration-source", result.Matches[0].SessionID)
	unavailable.stop(t)

	usage := cfg
	usage.ArchiveContent = config.ArchiveContentUsage
	usageStartup, err := preparePGServeImpl(usage, "")
	require.NoError(t, err)
	require.Nil(t, usageStartup.startWorker)
	usageStartup.cleanup()
	active, err = observer.Active(t.Context())
	require.NoError(t, err)
	assert.Nil(t, active)
	var documents, cachedA, cachedB int
	require.NoError(t, database.QueryRowContext(t.Context(), fmt.Sprintf(`SELECT
 (SELECT count(*) FROM hosted_embedding_documents),
 (SELECT count(*) FROM hosted_embedding_values_g%d),
 (SELECT count(*) FROM hosted_embedding_values_g%d)`, generationA.ID, generationB.ID)).Scan(&documents, &cachedA, &cachedB))
	assert.Zero(t, documents)
	assert.Zero(t, cachedA)
	assert.Zero(t, cachedB)

	rebuilt := startAcceptanceRunningServer(t, cfg)
	waitAcceptanceGeneration(t, observer, generationB.ID)
	code, result = acceptanceSearchCode(t, cfg, &rebuilt.startup, "rebuilt search", "semantic")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, result.Matches, 1)
	assert.Equal(t, "migration-source", result.Matches[0].SessionID)

	requests, requestErrors := encoder.snapshot()
	assert.Empty(t, requestErrors)
	var aQueries, bQueries, bDocumentRequests int
	for _, request := range requests {
		for _, input := range request.Input {
			switch {
			case request.Model == "model-a" && input == "query:alpha search:end":
				aQueries++
			case request.Model == "model-b" && (input == "query:migration search:end" || input == "query:rebuilt search:end"):
				bQueries++
			case request.Model == "model-b" && len(input) >= 4 && input[:4] == "doc:":
				bDocumentRequests++
			}
		}
	}
	assert.Equal(t, 2, aQueries, "A must continue serving while B is incomplete")
	assert.Equal(t, 3, bQueries, "the persistent reader and rebuilt runtime must use B after activation")
	assert.Equal(t, 18, bDocumentRequests, "restart must finish the canceled source and usage-only rebuild must request complete eight-document coverage")
}
