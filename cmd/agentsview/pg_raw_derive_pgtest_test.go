//go:build pgtest

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
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
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/agentsview/internal/service"
)

func hostedRuntimeConfig(t *testing.T) (config.Config, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("TEST_PG_URL")
	if dsn == "" {
		t.Skip("TEST_PG_URL not configured")
	}
	nonce := make([]byte, 16)
	_, err := rand.Read(nonce)
	require.NoError(t, err)
	suffix := hex.EncodeToString(nonce)
	schema := "runtime_test_" + suffix
	role := schema + "_role"
	admin, err := postgres.Open(dsn, schema, false)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE;DROP ROLE "`+role+`"`)
		assert.NoError(t, err)
		assert.NoError(t, admin.Close())
	})
	require.NoError(t, postgres.EnsureHostedTenant(t.Context(), admin, schema, "tenant-runtime"))
	password := make([]byte, 32)
	_, err = rand.Read(password)
	require.NoError(t, err)
	_, err = admin.Exec(`CREATE ROLE "` + role + `" LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT PASSWORD '` + hex.EncodeToString(password) + `';GRANT USAGE ON SCHEMA "` + schema + `" TO "` + role + `";GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA "` + schema + `" TO "` + role + `";GRANT USAGE ON ALL SEQUENCES IN SCHEMA "` + schema + `" TO "` + role + `"`)
	require.NoError(t, err)
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	u.User = url.UserPassword(role, hex.EncodeToString(password))
	cfg := config.Config{DataDir: t.TempDir(), Host: "127.0.0.1", Port: 0, RequireAuth: true, AuthToken: "synthetic-test-token", CursorSecret: "c3ludGhldGljLWN1cnNvci1zZWNyZXQ=", NoBrowser: true, PG: config.PGConfig{URL: u.String(), Schema: schema, RawTenant: "tenant-runtime"}}
	return cfg, admin
}

func TestHostedRuntimePreparationOffRetainsPublicReads(t *testing.T) {
	cfg, _ := hostedRuntimeConfig(t)
	// An occupied custody path makes any eager repository open fail decisively.
	require.NoError(t, os.WriteFile(filepath.Join(cfg.DataDir, pgRawSyncDataDirectory), []byte("occupied"), 0600))
	startup, err := preparePGServeImpl(cfg, "")
	require.NoError(t, err)
	t.Cleanup(startup.cleanup)
	require.Nil(t, startup.startWorker)
	startup.cleanup()
	startup.cleanup()
	store, closeStore, err := openPGReadStore(cfg, cfg.PG)
	require.NoError(t, err)
	defer closeStore()
	_, ok := store.(*postgres.HostedStore)
	require.True(t, ok)
	cfg.PG.RawTenant = ""
	_, err = preparePGServeImpl(cfg, "")
	require.ErrorContains(t, err, "raw_tenant")
}
func TestHostedRuntimePreparationSandboxAndIdle(t *testing.T) {
	cfg, _ := hostedRuntimeConfig(t)
	cfg.PG.RawDerivation = true
	p, err := rawderive.NewSubprocessParser(5 * time.Second)
	require.NoError(t, err)
	supported := p.Preflight(t.Context()) == nil
	if !supported && os.Getenv("RAW_SANDBOX_REQUIRED") == "1" {
		t.Fatal("mandatory sandbox unavailable")
	}
	require.NoError(t, os.WriteFile(filepath.Join(cfg.DataDir, pgRawSyncDataDirectory), []byte("occupied"), 0600))
	startup, err := preparePGServeImpl(cfg, "")
	if !supported {
		require.ErrorIs(t, err, rawderive.ErrSandboxUnavailable)
		return
	}
	require.NoError(t, err)
	defer startup.cleanup()
	require.NotNil(t, startup.startWorker)
	startup.startWorker()
	// This cancellation joins the real empty queue/maintenance pass. Occupied
	// custody proves startup and empty work have no repository dependency.
	startup.cleanup()
}

func TestHostedEmbeddingRuntimeStartsAfterReadinessAndWorkerOffRetainsReads(t *testing.T) {
	cfg, admin := hostedRuntimeConfig(t)
	var calls atomic.Int32
	encoder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request struct {
			Input []string `json:"input"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		data := make([]map[string]any, len(request.Input))
		for i := range request.Input {
			data[i] = map[string]any{"index": i, "embedding": []float32{1, 0, 0}}
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
	}))
	defer encoder.Close()

	cfg.HostedEmbeddings = hostedEmbeddingTestConfig(encoder.URL+"/v1", 1)
	profile, err := cfg.HostedEmbeddings.Profile("current")
	require.NoError(t, err)
	recipe, err := hostedEmbeddingRecipe("current", profile)
	require.NoError(t, err)
	u, err := url.Parse(cfg.PG.URL)
	require.NoError(t, err)
	generation, err := postgres.ProvisionHostedEmbeddings(t.Context(), admin, cfg.PG.Schema, cfg.PG.RawTenant, recipe, "initial", u.User.Username())
	require.NoError(t, err)
	runtimeDB, err := postgres.OpenHosted(cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
	require.NoError(t, err)
	_, err = runtimeDB.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,message_count,user_message_count) VALUES('source','project','machine','codex','legacy',2,2);INSERT INTO messages(session_id,ordinal,role,content) VALUES('source',0,'user','needle'),('source',1,'user','second turn')`)
	require.NoError(t, err)
	require.NoError(t, runtimeDB.Close())

	cfg.PG.HostedEmbeddingsEnabled = true
	cfg.PG.HostedEmbeddingsPollSeconds = 1
	startup, err := preparePGServeImpl(cfg, "")
	require.NoError(t, err)
	require.NotNil(t, startup.startWorker)
	assert.Zero(t, calls.Load(), "runtime preparation must not contact the encoder before HTTP readiness")
	startup.startWorker()
	observerDB, err := postgres.OpenHosted(cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
	require.NoError(t, err)
	observer, err := postgres.NewHostedEmbeddingStore(t.Context(), observerDB, postgres.HostedEmbeddingOptions{Schema: cfg.PG.Schema, Tenant: cfg.PG.RawTenant})
	require.NoError(t, err)
	activated := assert.Eventually(t, func() bool {
		active, activeErr := observer.Active(t.Context())
		return activeErr == nil && active != nil && active.ID == generation.ID
	}, 5*time.Second, 20*time.Millisecond)
	if !activated {
		status, statusErr := observer.Status(t.Context())
		t.Logf("embedding calls=%d status=%+v status_err=%v", calls.Load(), status, statusErr)
	}
	require.True(t, activated)
	startup.cleanup()
	require.NoError(t, observerDB.Close())
	buildCalls := calls.Load()
	require.Positive(t, buildCalls)

	cfg.PG.HostedEmbeddingsEnabled = false
	readService, closeRead, err := newPGReadService(cfg, cfg.PG)
	require.NoError(t, err)
	result, err := readService.SearchContent(t.Context(), service.ContentSearchRequest{Pattern: "needle", Mode: "semantic", Limit: 5})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotEmpty(t, result.Matches)
	assert.Equal(t, "source", result.Matches[0].SessionID)
	assert.Equal(t, 0, result.Matches[0].Ordinal)
	closeRead()
	assert.Equal(t, buildCalls+1, calls.Load(), "worker-off direct reads may encode only their query")

	beforeUsageOnly := calls.Load()
	cfg.ArchiveContent = config.ArchiveContentUsage
	cfg.PG.HostedEmbeddingsEnabled = true
	usageStartup, err := preparePGServeImpl(cfg, "")
	require.NoError(t, err)
	require.Nil(t, usageStartup.startWorker, "usage-only startup must disable the embedding worker")
	usageDB, err := postgres.OpenHosted(cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
	require.NoError(t, err)
	usageStore, err := postgres.NewHostedEmbeddingStore(t.Context(), usageDB, postgres.HostedEmbeddingOptions{Schema: cfg.PG.Schema, Tenant: cfg.PG.RawTenant})
	require.NoError(t, err)
	active, err := usageStore.Active(t.Context())
	require.NoError(t, err)
	assert.Nil(t, active, "usage-only ClearContent must invalidate active serving until a complete rebuild")
	status, err := usageStore.Status(t.Context())
	require.NoError(t, err)
	require.NotNil(t, status.Active, "usage-only cleanup preserves generation identity")
	assert.False(t, status.ActiveAvailable)
	var documents int
	require.NoError(t, usageDB.QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_embedding_documents`).Scan(&documents))
	assert.Zero(t, documents)
	require.NoError(t, usageDB.Close())
	usageStartup.cleanup()
	assert.Equal(t, beforeUsageOnly, calls.Load(), "usage-only startup must not call the encoder")
}

// This gate executes the actual hosted startup worker and parser child. It is
// mandatory on the Linux isolation runner; the local restricted host skips it.
func TestHostedRuntimeDerivesAcceptedSource(t *testing.T) {
	p, err := rawderive.NewSubprocessParser(5 * time.Second)
	require.NoError(t, err)
	if err = p.Preflight(t.Context()); err != nil {
		if os.Getenv("RAW_SANDBOX_REQUIRED") == "1" {
			t.Fatal(err)
		}
		t.Skip("kernel isolation unavailable")
	}
	cfg, _ := hostedRuntimeConfig(t)
	cfg.PG.RawDerivation = true
	cfg.PG.RawPollSeconds = 1
	startup, err := preparePGServeImpl(cfg, "")
	require.NoError(t, err)
	defer startup.cleanup()
	database, err := postgres.OpenHosted(cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
	require.NoError(t, err)
	defer database.Close()
	authStore, err := postgres.NewTenantRawDeviceAuthStore(database, cfg.PG.RawTenant)
	require.NoError(t, err)
	auth, err := rawsync.NewDeviceAuthService(authStore, time.Minute)
	require.NoError(t, err)
	enrolled, err := auth.EnrollDevice(t.Context(), cfg.PG.RawTenant, "synthetic device")
	require.NoError(t, err)
	request := func(method, path, token, kind string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://127.0.0.1"+path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", kind)
		req.Header.Set("X-AgentsView-Device-ID", enrolled.Identity.DeviceID)
		req.Header.Set("Upload-Offset", "0")
		rec := httptest.NewRecorder()
		startup.srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	tokenResponse := request("POST", "/api/v1/raw-sync/tokens", enrolled.Credential, "application/json", []byte(`{"scopes":["upload","commit"]}`))
	require.Equal(t, 200, tokenResponse.Code)
	var token struct{ Token string }
	require.NoError(t, json.Unmarshal(tokenResponse.Body.Bytes(), &token))
	body := hostedRuntimeClaudeFixture()
	digest := sha256.Sum256(body)
	ref := rawsync.ObjectRef{SHA256: hex.EncodeToString(digest[:]), Length: int64(len(body))}
	encoded, err := json.Marshal(map[string]any{"provider": "claude", "object": ref})
	require.NoError(t, err)
	uploadResponse := request("POST", "/api/v1/raw-sync/uploads", token.Token, "application/json", encoded)
	require.Contains(t, []int{200, 201}, uploadResponse.Code)
	var upload struct {
		UploadID string `json:"upload_id"`
	}
	require.NoError(t, json.Unmarshal(uploadResponse.Body.Bytes(), &upload))
	require.NotEmpty(t, upload.UploadID)
	appended := request("PATCH", "/api/v1/raw-sync/uploads/"+upload.UploadID, token.Token, "application/octet-stream", body)
	require.Equal(t, 200, appended.Code)
	manifest := rawsync.Manifest{SchemaVersion: rawsync.ManifestSchemaVersion, Provider: parser.AgentClaude, ConfiguredRootID: "root", SourceKey: "/canonical/project/runtime-session.jsonl", CaptureID: "runtime-capture", CapturedAt: time.Now().UTC(), Kind: rawsync.ManifestSnapshot, Entries: []rawsync.Entry{{Path: "project/runtime-session.jsonl", Type: "file", Length: ref.Length, Objects: []rawsync.ObjectRef{ref}}}}
	encoded, err = json.Marshal(manifest)
	require.NoError(t, err)
	accepted := request("POST", "/api/v1/raw-sync/manifests", token.Token, "application/json", encoded)
	require.Equal(t, 200, accepted.Code, accepted.Body.String())
	var attempts, generation int
	require.NoError(t, database.QueryRow(`SELECT attempt_count,projection_generation FROM raw_ingest_jobs`).Scan(&attempts, &generation))
	assert.Zero(t, attempts)
	assert.Equal(t, 1, generation)
	ctx, cancel := context.WithCancel(startup.ctx)
	startup.ctx = ctx
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- runPreparedPGServe(startup) }()
	defer func() {
		cancel()
		select {
		case err := <-finished:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Error("hosted runtime did not join")
		}
	}()
	require.Eventually(t, func() bool {
		var state string
		err := database.QueryRow(`SELECT state FROM raw_ingest_jobs`).Scan(&state)
		return err == nil && state == "complete"
	}, 20*time.Second, 50*time.Millisecond)
	store, err := postgres.NewHostedStore(cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
	require.NoError(t, err)
	defer store.Close()
	messages, err := store.GetMessages(t.Context(), "runtime-session", 0, 10, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "hello runtime", messages[0].Content)
	assert.Equal(t, "hello viewer", messages[1].Content)
}

func TestHostedRuntimeRejectsMissingPublicationPrivilegesBeforeReadiness(t *testing.T) {
	cfg, admin := hostedRuntimeConfig(t)
	endpoint, err := url.Parse(cfg.PG.URL)
	require.NoError(t, err)
	role := endpoint.User.Username()
	control, err := preparePGServeImpl(cfg, "")
	require.NoError(t, err)
	control.cleanup()
	cfg.PG.RawDerivation = true
	for _, tc := range []struct{ object, privilege, kind string }{
		{"session_sources", "DELETE", "TABLE"}, {"raw_session_links", "DELETE", "TABLE"}, {"tool_result_events", "INSERT", "TABLE"}, {"usage_events", "INSERT", "TABLE"}, {"secret_findings", "INSERT", "TABLE"}, {"starred_sessions", "INSERT", "TABLE"}, {"pinned_messages", "UPDATE", "TABLE"},
		{"tool_calls_id_seq", "USAGE", "SEQUENCE"}, {"tool_result_events_id_seq", "USAGE", "SEQUENCE"}, {"usage_events_id_seq", "USAGE", "SEQUENCE"}, {"pinned_messages_id_seq", "USAGE", "SEQUENCE"},
	} {
		t.Run(tc.object+"_"+tc.privilege, func(t *testing.T) {
			_, err := admin.Exec(`REVOKE ` + tc.privilege + ` ON ` + tc.kind + ` "` + tc.object + `" FROM "` + role + `"`)
			require.NoError(t, err)
			defer func() {
				_, err := admin.Exec(`GRANT ` + tc.privilege + ` ON ` + tc.kind + ` "` + tc.object + `" TO "` + role + `"`)
				require.NoError(t, err)
			}()
			startup, err := preparePGServeImpl(cfg, "")
			if err == nil {
				startup.cleanup()
			}
			require.ErrorContains(t, err, "privileges")
			require.Nil(t, startup.startWorker)
			var attempts int
			require.NoError(t, admin.QueryRow(`SELECT COALESCE(sum(attempt_count),0) FROM raw_ingest_jobs`).Scan(&attempts))
			assert.Zero(t, attempts)
		})
	}
}
