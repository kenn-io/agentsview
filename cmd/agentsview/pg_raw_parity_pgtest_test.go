//go:build pgtest

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/rawcapture"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/agentsview/internal/rawtest"
)

type hostedCaptureClient struct {
	checkpoint    *rawcheckpoint.Store
	provider      parser.Provider
	device, token string
	startup       *pgServeStartup
	last          rawsync.Manifest
}

type directCapturedGeneration struct {
	manifest rawsync.Manifest
	commit   rawsync.CommitResult
}

type directParityProjectionState struct {
	sourceID string
	active   int
	physical int
}

type directParityCapture struct {
	checkpoint *rawcheckpoint.Store
	provider   parser.Provider
	identity   rawsync.AuthIdentity
	custody    *pgRawSyncCustody
}

func newDirectParityCapture(t *testing.T, database *sql.DB, custody *pgRawSyncCustody, agent parser.AgentType, root, device string) *directParityCapture {
	t.Helper()
	provider, ok := parser.NewProvider(agent, parser.ProviderConfig{Roots: []string{root}, Machine: "parity-fixture"})
	require.True(t, ok)
	identity, err := rawsync.NewAuthIdentity("tenant-runtime", device)
	require.NoError(t, err)
	registerDirectParityDevice(t, database, identity)
	dir := t.TempDir()
	checkpoint, err := rawcheckpoint.OpenWithOptions(t.Context(), filepath.Join(dir, "checkpoint.db"), rawcheckpoint.Options{SpoolDir: filepath.Join(dir, "spool"), MaxOutboxBytes: 8 << 20})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, checkpoint.Close()) })
	require.NoError(t, checkpoint.SetDevice(t.Context(), identity.DeviceID))
	return &directParityCapture{checkpoint: checkpoint, provider: provider, identity: identity, custody: custody}
}

func registerDirectParityDevice(t *testing.T, database *sql.DB, identity rawsync.AuthIdentity) {
	t.Helper()
	_, err := database.ExecContext(t.Context(), `INSERT INTO raw_devices(device_id,display_name,credential_sha256,created_at) VALUES($1,'parity fixture device',$2,clock_timestamp()) ON CONFLICT(device_id) DO NOTHING`, identity.DeviceID, make([]byte, 32))
	require.NoError(t, err)
}

func (c *directParityCapture) captureSource(t *testing.T, source parser.SourceRef) directCapturedGeneration {
	t.Helper()
	result, err := rawcapture.New(c.checkpoint).Capture(t.Context(), c.provider, source)
	require.NoError(t, err)
	require.Equal(t, rawcapture.StatusCaptured, result.Status)
	manifest, found, err := c.checkpoint.FinalizeNextManifest(t.Context(), c.identity.DeviceID)
	require.NoError(t, err)
	require.True(t, found)
	for _, entry := range manifest.Entries {
		for _, ref := range entry.Objects {
			missing, err := c.custody.MissingObjects(t.Context(), c.identity, manifest.Provider, []rawsync.ObjectRef{ref})
			require.NoError(t, err)
			if len(missing) == 0 {
				continue
			}
			payload, err := os.ReadFile(c.checkpoint.ObjectPath(ref))
			require.NoError(t, err)
			_, err = c.custody.FinalizeObject(t.Context(), c.identity, manifest.Provider, ref, bytes.NewReader(payload))
			require.NoError(t, err)
		}
	}
	commit, err := c.custody.CommitManifest(t.Context(), c.identity, manifest)
	require.NoError(t, err)
	require.NoError(t, c.checkpoint.BindFinalizedCommit(t.Context(), c.identity.DeviceID, manifest.CaptureID, commit))
	_, err = c.checkpoint.AcknowledgeGeneration(t.Context(), c.identity.DeviceID, manifest.CaptureID, commit)
	require.NoError(t, err)
	return directCapturedGeneration{manifest: manifest, commit: commit}
}

func (c *directParityCapture) captureAll(t *testing.T) []directCapturedGeneration {
	t.Helper()
	discovery, err := parser.DiscoverRawCaptureSources(t.Context(), c.provider)
	require.NoError(t, err)
	require.True(t, discovery.Complete)
	slices.SortFunc(discovery.Sources, func(a, b parser.SourceRef) int { return strings.Compare(a.Key, b.Key) })
	result := make([]directCapturedGeneration, 0, len(discovery.Sources))
	for _, source := range discovery.Sources {
		result = append(result, c.captureSource(t, source))
	}
	return result
}

func replicateDirectParityGenerations(t *testing.T, source, target *directParityCapture, generations []directCapturedGeneration) []directCapturedGeneration {
	t.Helper()
	require.Equal(t, source.identity, target.identity)
	result := make([]directCapturedGeneration, 0, len(generations))
	parents := make(map[string]string)
	for _, generation := range generations {
		replica := generation.manifest
		if replica.ExpectedParentReceipt != "" {
			parent, ok := parents[replica.SourceKey]
			require.True(t, ok, "replicated history must begin with generation one")
			replica.ExpectedParentReceipt = parent
		}
		for _, entry := range replica.Entries {
			for _, ref := range entry.Objects {
				missing, err := target.custody.MissingObjects(t.Context(), target.identity, replica.Provider, []rawsync.ObjectRef{ref})
				require.NoError(t, err)
				if len(missing) == 0 {
					continue
				}
				var payload bytes.Buffer
				_, err = source.custody.CopyObject(t.Context(), source.identity.TenantID, ref, &payload)
				require.NoError(t, err)
				_, err = target.custody.FinalizeObject(t.Context(), target.identity, replica.Provider, ref, bytes.NewReader(payload.Bytes()))
				require.NoError(t, err)
			}
		}
		commit, err := target.custody.CommitManifest(t.Context(), target.identity, replica)
		require.NoError(t, err)
		require.Equal(t, generation.commit.Generation, commit.Generation)
		parents[replica.SourceKey] = commit.Receipt
		result = append(result, directCapturedGeneration{manifest: replica, commit: commit})
	}
	return result
}

func newDirectParityCustody(t *testing.T, cfg config.Config, database *sql.DB) *pgRawSyncCustody {
	t.Helper()
	metadata, err := postgres.NewHostedRawIngestStore(database, cfg.PG.RawTenant, rawProcessingVersion())
	require.NoError(t, err)
	custody := &pgRawSyncCustody{dataDir: cfg.DataDir, tenant: cfg.PG.RawTenant, metadata: metadata, limits: rawsync.DefaultManifestLimits(), version: rawProcessingVersion()}
	t.Cleanup(func() { require.NoError(t, custody.Close()) })
	return custody
}

func quarantineDirectParityObject(t *testing.T, custody *pgRawSyncCustody, tenant string, object rawsync.ObjectRef) {
	t.Helper()
	_, err := custody.openService(t.Context())
	require.NoError(t, err)
	originDigest := sha256.Sum256([]byte(tenant))
	ref, err := artifact.NewRef("tenant-"+hex.EncodeToString(originDigest[:]), artifact.KindRaw, object.SHA256)
	require.NoError(t, err)
	require.NoError(t, custody.repository.Content().Quarantine(t.Context(), ref, "isolated missing-object parity fixture"))
	_, err = custody.CopyObject(t.Context(), tenant, object, io.Discard)
	require.ErrorIs(t, err, rawsync.ErrNotFound, "production custody must observe the quarantined object as missing")
}

func restrictedDirectParityBaseline(t *testing.T, cfg config.Config, admin *sql.DB) config.Config {
	t.Helper()
	role := cfg.PG.Schema + "_parity_reader"
	password := cfg.PG.Schema + "_reader_password"
	_, err := admin.ExecContext(t.Context(), `CREATE ROLE "`+role+`" LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT PASSWORD '`+password+`'; GRANT USAGE ON SCHEMA "`+cfg.PG.Schema+`" TO "`+role+`"; GRANT SELECT ON ALL TABLES IN SCHEMA "`+cfg.PG.Schema+`" TO "`+role+`"`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := admin.ExecContext(context.Background(), `DROP OWNED BY "`+role+`"; DROP ROLE IF EXISTS "`+role+`"`)
		assert.NoError(t, cleanupErr)
	})
	u, err := url.Parse(cfg.PG.URL)
	require.NoError(t, err)
	u.User = url.UserPassword(role, password)
	cfg.PG.URL = u.String()
	reader, err := postgres.OpenHosted(cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
	require.NoError(t, err)
	defer reader.Close()
	var canSelect, canUpdate bool
	require.NoError(t, reader.QueryRowContext(t.Context(), `SELECT has_table_privilege(current_user,'sessions','SELECT'),has_table_privilege(current_user,'sessions','UPDATE')`).Scan(&canSelect, &canUpdate))
	assert.True(t, canSelect)
	assert.False(t, canUpdate)
	return cfg
}

func runDirectRawDerivation(t *testing.T, cfg config.Config, database *sql.DB, custody *pgRawSyncCustody, expected int) {
	t.Helper()
	isolated, err := rawderive.NewSubprocessParser(20 * time.Second)
	require.NoError(t, err)
	require.NoError(t, isolated.Preflight(t.Context()))
	retry := rawderive.RetryPolicy{Base: time.Millisecond, Maximum: time.Second, MaxAttempts: 2}
	sink, err := postgres.NewRawProjectionStore(database, hostedRawProjectionOptions(cfg, cfg.PG.RawTenant, retry))
	require.NoError(t, err)
	queue, ok := custody.metadata.(rawderive.JobQueue)
	require.True(t, ok)
	worker, err := rawderive.NewWorker(rawderive.WorkerConfig{
		Queue: queue, Manifests: rawderive.ManifestLoader{Store: custody, Limits: custody.limits},
		Materializer: rawderive.Materializer{Store: custody, BaseDir: t.TempDir(), MaxTotalBytes: 8 << 20}, Parser: isolated, Projection: sink,
		Owner: "parity-fixture-raw", BatchSize: 8, LeaseDuration: time.Minute, HeartbeatInterval: time.Second,
		AttemptTimeout: 20 * time.Second, RetryBase: retry.Base, RetryMax: retry.Maximum, MaxAttempts: retry.MaxAttempts,
	})
	require.NoError(t, err)
	total := 0
	for {
		batch, err := worker.RunBatch(t.Context())
		require.NoError(t, err)
		total += batch.Succeeded
		if batch.Claimed == 0 {
			break
		}
		require.Zero(t, batch.Retried)
		require.Zero(t, batch.Failed)
	}
	require.Equal(t, expected, total)
}

func requireHostedSandbox(t *testing.T) {
	t.Helper()
	p, err := rawderive.NewSubprocessParser(5 * time.Second)
	require.NoError(t, err)
	if err = p.Preflight(t.Context()); err != nil {
		if os.Getenv("RAW_SANDBOX_REQUIRED") == "1" {
			t.Fatal(err)
		}
		t.Skip("kernel isolation unavailable")
	}
}
func startParityRuntime(t *testing.T, policy config.ArchiveContent) (*pgServeStartup, *postgres.HostedStore, *sql.DB) {
	t.Helper()
	requireHostedSandbox(t)
	cfg, admin := hostedRuntimeConfig(t)
	cfg.PG.RawDerivation = true
	cfg.PG.RawPollSeconds = 1
	cfg.PG.RawMaxAttempts = 2
	cfg.ArchiveContent = policy
	// An occupied archive path makes an accidental hosted archive open fail.
	cfg.DBPath = filepath.Join(cfg.DataDir, "sessions.db")
	require.NoError(t, os.Mkdir(cfg.DBPath, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(cfg.DBPath, "sentinel"), []byte("not an archive"), 0600))
	startup, err := preparePGServeImpl(cfg, "")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(startup.ctx)
	startup.ctx = ctx
	finished := make(chan error, 1)
	go func() { finished <- runPreparedPGServe(startup) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-finished:
			assert.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Error("runtime failed to join")
		}
		startup.cleanup()
		body, err := os.ReadFile(filepath.Join(cfg.DBPath, "sentinel"))
		assert.NoError(t, err)
		assert.Equal(t, "not an archive", string(body))
	})
	store, err := postgres.NewHostedStore(cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return &startup, store, admin
}
func newHostedCaptureClient(t *testing.T, startup *pgServeStartup, store *postgres.HostedStore, agent parser.AgentType, root string) *hostedCaptureClient {
	t.Helper()
	authStore, err := postgres.NewTenantRawDeviceAuthStore(store.DB(), "tenant-runtime")
	require.NoError(t, err)
	auth, err := rawsync.NewDeviceAuthService(authStore, time.Minute)
	require.NoError(t, err)
	enrolled, err := auth.EnrollDevice(t.Context(), "tenant-runtime", "synthetic capture device")
	require.NoError(t, err)
	provider, ok := parser.NewProvider(agent, parser.ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	dir := t.TempDir()
	cp, err := rawcheckpoint.OpenWithOptions(t.Context(), filepath.Join(dir, "checkpoint.db"), rawcheckpoint.Options{SpoolDir: filepath.Join(dir, "spool"), MaxOutboxBytes: 8 << 20})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cp.Close()) })
	require.NoError(t, cp.SetDevice(t.Context(), enrolled.Identity.DeviceID))
	c := &hostedCaptureClient{checkpoint: cp, provider: provider, device: enrolled.Identity.DeviceID, startup: startup}
	reply := c.request("POST", "/api/v1/raw-sync/tokens", enrolled.Credential, "application/json", []byte(`{"scopes":["upload","commit"]}`))
	require.Equal(t, 200, reply.Code, reply.Body.String())
	var token struct{ Token string }
	require.NoError(t, json.Unmarshal(reply.Body.Bytes(), &token))
	c.token = token.Token
	return c
}
func (c *hostedCaptureClient) request(method, path, token, kind string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://127.0.0.1"+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", kind)
	req.Header.Set("X-AgentsView-Device-ID", c.device)
	req.Header.Set("Upload-Offset", "0")
	rec := httptest.NewRecorder()
	c.startup.srv.Handler().ServeHTTP(rec, req)
	return rec
}
func (c *hostedCaptureClient) commit(t *testing.T, m rawsync.Manifest) rawsync.CommitResult {
	t.Helper()
	body, err := json.Marshal(m)
	require.NoError(t, err)
	rec := c.request("POST", "/api/v1/raw-sync/manifests", c.token, "application/json", body)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var result struct {
		ManifestID string `json:"manifest_id"`
		Receipt    string `json:"receipt"`
		Generation int64  `json:"generation"`
		Created    bool   `json:"created"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	return rawsync.CommitResult{ManifestID: result.ManifestID, Receipt: result.Receipt, Generation: result.Generation, Created: result.Created}
}
func (c *hostedCaptureClient) flush(t *testing.T) rawsync.CommitResult {
	t.Helper()
	m, found, err := c.checkpoint.FinalizeNextManifest(t.Context(), c.device)
	require.NoError(t, err)
	require.True(t, found)
	for _, entry := range m.Entries {
		for _, ref := range entry.Objects {
			body, err := json.Marshal(map[string]any{"provider": m.Provider, "object": ref})
			require.NoError(t, err)
			rec := c.request("POST", "/api/v1/raw-sync/uploads", c.token, "application/json", body)
			require.Contains(t, []int{200, 201}, rec.Code, rec.Body.String())
			var upload struct {
				UploadID string `json:"upload_id"`
				Complete bool   `json:"complete"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &upload))
			if !upload.Complete {
				require.NotEmpty(t, upload.UploadID)
				payload, err := os.ReadFile(c.checkpoint.ObjectPath(ref))
				require.NoError(t, err)
				rec = c.request("PATCH", "/api/v1/raw-sync/uploads/"+upload.UploadID, c.token, "application/octet-stream", payload)
				require.Equal(t, 200, rec.Code, rec.Body.String())
			}
		}
	}
	result := c.commit(t, m)
	require.NoError(t, c.checkpoint.BindFinalizedCommit(t.Context(), c.device, m.CaptureID, result))
	_, err = c.checkpoint.AcknowledgeGeneration(t.Context(), c.device, m.CaptureID, result)
	require.NoError(t, err)
	c.last = m
	return result
}
func (c *hostedCaptureClient) capture(t *testing.T) rawsync.CommitResult {
	t.Helper()
	sources, err := parser.DiscoverRawCaptureSources(t.Context(), c.provider)
	require.NoError(t, err)
	require.True(t, sources.Complete)
	require.Len(t, sources.Sources, 1)
	result, err := rawcapture.New(c.checkpoint).Capture(t.Context(), c.provider, sources.Sources[0])
	require.NoError(t, err)
	require.Equal(t, rawcapture.StatusCaptured, result.Status)
	return c.flush(t)
}
func waitCaptureJob(t *testing.T, pg *sql.DB, manifest, state string, attempts int) {
	t.Helper()
	require.Eventually(t, func() bool {
		var got string
		var count int
		err := pg.QueryRow(`SELECT state,attempt_count FROM raw_ingest_jobs WHERE manifest_id=$1`, manifest).Scan(&got, &count)
		return err == nil && got == state && count == attempts
	}, 30*time.Second, 50*time.Millisecond)
}

func configureDirectParityOwner(t *testing.T, runtimeCfg, baselineCfg config.Config, runtimeDB *sql.DB, custody *pgRawSyncCustody) (*pgMigrationParityOwner, *postgres.MigrationParityStore, rawderive.ParityRequest) {
	t.Helper()
	baselineDB, err := postgres.OpenHosted(baselineCfg.PG.URL, baselineCfg.PG.Schema, baselineCfg.PG.RawTenant, false)
	require.NoError(t, err)
	defer baselineDB.Close()
	var baselineID string
	require.NoError(t, baselineDB.QueryRowContext(t.Context(), `SELECT target_id::text FROM migration_parity_identity WHERE singleton=1`).Scan(&baselineID))
	runtimePG := runtimeCfg.PG
	runtimePG.ParityEnabled = true
	runtimePG.ParityPollSeconds = 1
	runtimePG.ParityAttemptSeconds = 20
	runtimePG.ParitySnapshotSeconds = 20
	runtimePG.ParityBaselines = map[string]config.PGParityBaseline{"before": {Target: "baseline", Identity: baselineID}}
	runtimeCfg.DefaultPG = "runtime"
	runtimeCfg.PGTargets = map[string]config.PGConfig{"runtime": runtimePG, "baseline": baselineCfg.PG}
	owner, err := newPGMigrationParityOwner(t.Context(), runtimeCfg, runtimePG, runtimeDB, custody)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, owner.parser.Close()) })
	store, err := postgres.NewMigrationParityStore(t.Context(), runtimeDB, postgres.MigrationParityOptions{Schema: runtimePG.Schema, Tenant: runtimePG.RawTenant})
	require.NoError(t, err)
	var runtimeID, device, provider, root string
	require.NoError(t, runtimeDB.QueryRowContext(t.Context(), `SELECT target_id::text FROM migration_parity_identity WHERE singleton=1`).Scan(&runtimeID))
	require.NoError(t, runtimeDB.QueryRowContext(t.Context(), `SELECT min(device_id),min(provider),min(configured_root_id) FROM raw_source_heads`).Scan(&device, &provider, &root))
	request := rawderive.ParityRequest{RunID: "00000000-0000-4000-8000-000000000137", RuntimeID: runtimeID, BaselineProfile: "before", Cohort: rawderive.ParityCohort{DeviceID: device, Provider: parser.AgentType(provider), RootID: root}}
	return owner, store, request
}

func writeParityClaudeForkFixture(t *testing.T, root string) {
	t.Helper()
	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(project, 0o755))
	original := strings.Join([]string{
		`{"type":"system","subtype":"local_command","timestamp":"2026-01-01T09:59:59Z","sessionId":"orig-1111","content":"<command-name>/rename</command-name>\n<command-args>Original retained title</command-args>"}`,
		`{"type":"user","uuid":"u1","parentUuid":null,"timestamp":"2026-01-01T10:00:00Z","sessionId":"orig-1111","cwd":"/work/project","message":{"content":"first question"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-01-01T10:00:05Z","sessionId":"orig-1111","message":{"id":"msg_a1","content":[{"type":"text","text":"first answer"}],"usage":{"input_tokens":11,"output_tokens":7}}}`,
	}, "\n") + "\n"
	fork := strings.ReplaceAll(strings.ReplaceAll(original, "orig-1111", "fork-2222"), `"sessionId":"fork-2222",`, `"sessionId":"fork-2222","sessionKind":"bg",`)
	require.NoError(t, os.WriteFile(filepath.Join(project, "orig-1111.jsonl"), []byte(original), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(project, "fork-2222.jsonl"), []byte(fork), 0o644))
}

func writeParityClaudeCompanionFixture(t *testing.T, root string) (string, string) {
	t.Helper()
	project := filepath.Join(root, "project")
	sessionID := "object-loss-session"
	transcript := filepath.Join(project, sessionID+".jsonl")
	sidecar := filepath.Join(project, sessionID, "tool-results", "retained.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(sidecar), 0o755))
	require.NoError(t, os.WriteFile(sidecar, []byte("generation one retained companion\n"), 0o644))
	persisted := "<persisted-output>\nFull output saved to: " + sidecar + "\n</persisted-output>"
	body := strings.Join([]string{
		`{"type":"system","subtype":"local_command","timestamp":"2026-01-02T09:59:59Z","sessionId":"` + sessionID + `","content":"<command-name>/rename</command-name>\n<command-args>Object loss title</command-args>"}`,
		`{"type":"user","uuid":"u1","parentUuid":null,"timestamp":"2026-01-02T10:00:00Z","sessionId":"` + sessionID + `","cwd":"/work/project","message":{"content":"inspect retained object"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-01-02T10:00:01Z","sessionId":"` + sessionID + `","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"fixture"}}],"usage":{"input_tokens":13,"output_tokens":5}}}`,
		`{"type":"user","uuid":"u2","parentUuid":"a1","timestamp":"2026-01-02T10:00:02Z","sessionId":"` + sessionID + `","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` + strconv.Quote(persisted) + `,"is_error":false}]},"toolUseResult":{"persistedOutputPath":` + strconv.Quote(sidecar) + `,"persistedOutputSize":34}}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(transcript, []byte(body), 0o644))
	return transcript, sidecar
}

func generationObjectAtPath(t *testing.T, generation directCapturedGeneration, suffix string) rawsync.ObjectRef {
	t.Helper()
	for _, entry := range generation.manifest.Entries {
		if strings.HasSuffix(entry.Path, suffix) {
			require.Len(t, entry.Objects, 1)
			return entry.Objects[0]
		}
	}
	t.Fatalf("captured generation has no entry ending in %q", suffix)
	return rawsync.ObjectRef{}
}

func logDirectParityEvidence(t *testing.T, database *sql.DB, runID string) {
	t.Helper()
	rows, err := database.QueryContext(t.Context(), `SELECT source_kind,evidence_generation,candidate_complete,COALESCE(verdict,''),code,count(*)
		FROM migration_parity_sources WHERE run_id=$1 GROUP BY source_kind,evidence_generation,candidate_complete,verdict,code ORDER BY source_kind,evidence_generation,candidate_complete,verdict,code`, runID)
	require.NoError(t, err)
	for rows.Next() {
		var kind, verdict, code string
		var generation, count int64
		var complete bool
		require.NoError(t, rows.Scan(&kind, &generation, &complete, &verdict, &code, &count))
		t.Logf("parity source evidence: kind=%s generation=%d complete=%t verdict=%s code=%s count=%d", kind, generation, complete, verdict, code, count)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	rows, err = database.QueryContext(t.Context(), `SELECT s.source_kind,m.kind,COALESCE(m.verdict,''),m.inventory_kind,m.required,(m.candidate_fingerprint IS NOT NULL),count(*)
		FROM migration_parity_members m JOIN migration_parity_sources s USING(tenant_id,run_id,init_epoch,source_id)
		WHERE m.run_id=$1 GROUP BY s.source_kind,m.kind,m.verdict,m.inventory_kind,m.required,(m.candidate_fingerprint IS NOT NULL)
		ORDER BY s.source_kind,m.kind,m.verdict,m.inventory_kind,m.required,(m.candidate_fingerprint IS NOT NULL)`, runID)
	require.NoError(t, err)
	for rows.Next() {
		var sourceKind, memberKind, verdict, inventory string
		var required, candidate bool
		var count int64
		require.NoError(t, rows.Scan(&sourceKind, &memberKind, &verdict, &inventory, &required, &candidate, &count))
		t.Logf("parity member evidence: source_kind=%s kind=%s verdict=%s inventory=%s required=%t candidate=%t count=%d", sourceKind, memberKind, verdict, inventory, required, candidate, count)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
}

func directParityProjectionStates(t *testing.T, database *sql.DB) []directParityProjectionState {
	t.Helper()
	rows, err := database.QueryContext(t.Context(), `SELECT p.source_id,
		count(DISTINCT b.branch_id) FILTER (WHERE b.active),count(DISTINCT ss.branch_id)
		FROM raw_source_projections p
		LEFT JOIN raw_session_branches b ON b.source_id=p.source_id
		LEFT JOIN session_sources ss ON ss.source_id=p.source_id
		GROUP BY p.source_id ORDER BY p.source_id`)
	require.NoError(t, err)
	defer rows.Close()
	var states []directParityProjectionState
	for rows.Next() {
		var state directParityProjectionState
		require.NoError(t, rows.Scan(&state.sourceID, &state.active, &state.physical))
		states = append(states, state)
	}
	require.NoError(t, rows.Err())
	return states
}

func TestParityConfiguredOwnerProcessesBatchOneAcrossRestartWithRealBaselineAndExclusion(t *testing.T) {
	runtimeCfg, _ := hostedRuntimeConfig(t)
	baselineCfg, baselineAdmin := hostedRuntimeConfig(t)
	runtimeDB, err := postgres.OpenHosted(runtimeCfg.PG.URL, runtimeCfg.PG.Schema, runtimeCfg.PG.RawTenant, false)
	require.NoError(t, err)
	defer runtimeDB.Close()
	baselineDB, err := postgres.OpenHosted(baselineCfg.PG.URL, baselineCfg.PG.Schema, baselineCfg.PG.RawTenant, false)
	require.NoError(t, err)
	defer baselineDB.Close()
	runtimeCustody := newDirectParityCustody(t, runtimeCfg, runtimeDB)
	baselineCustody := newDirectParityCustody(t, baselineCfg, baselineDB)
	root := t.TempDir()
	writeParityClaudeForkFixture(t, root)
	device := "00000000-0000-4000-8000-000000000138"
	runtimeCapture := newDirectParityCapture(t, runtimeDB, runtimeCustody, parser.AgentClaude, root, device)
	runtimeGenerations := runtimeCapture.captureAll(t)
	require.Len(t, runtimeGenerations, 2)
	registerDirectParityDevice(t, baselineDB, runtimeCapture.identity)
	baselineCapture := &directParityCapture{identity: runtimeCapture.identity, custody: baselineCustody}
	baselineGenerations := replicateDirectParityGenerations(t, runtimeCapture, baselineCapture, runtimeGenerations)
	require.Len(t, baselineGenerations, 2)
	baselineReadCfg := restrictedDirectParityBaseline(t, baselineCfg, baselineAdmin)
	requireHostedSandbox(t)
	runDirectRawDerivation(t, runtimeCfg, runtimeDB, runtimeCustody, 2)
	runDirectRawDerivation(t, baselineCfg, baselineDB, baselineCustody, 2)
	runtimeProjection := directParityProjectionStates(t, runtimeDB)
	baselineProjection := directParityProjectionStates(t, baselineDB)
	require.Len(t, runtimeProjection, 2)
	require.Len(t, baselineProjection, len(runtimeProjection))
	sameIdentities := true
	for index := range runtimeProjection {
		sameIdentities = sameIdentities && runtimeProjection[index].sourceID == baselineProjection[index].sourceID
		assert.Equal(t, runtimeProjection[index].active, baselineProjection[index].active)
		assert.Equal(t, runtimeProjection[index].physical, baselineProjection[index].physical)
	}
	require.True(t, sameIdentities, "runtime and baseline custody must project the same retained source identities")
	t.Logf("real projection census: sources=%d active=%d/%d physical=%d/%d", len(baselineProjection), baselineProjection[0].active, baselineProjection[1].active, baselineProjection[0].physical, baselineProjection[1].physical)
	_, err = baselineDB.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,message_count,user_message_count,data_version,quality_signal_version,secrets_rules_version)
		SELECT 'baseline-independent-sentinel','sentinel','baseline','claude','legacy',1,1,data_version,quality_signal_version,secrets_rules_version FROM sessions WHERE provenance_kind='raw' LIMIT 1;
		INSERT INTO messages(session_id,ordinal,role,content) VALUES('baseline-independent-sentinel',0,'user','baseline secret sentinel unrelated to raw')`)
	require.NoError(t, err)

	owner, store, request := configureDirectParityOwner(t, runtimeCfg, baselineReadCfg, runtimeDB, runtimeCustody)
	_, err = store.CreateOrResumeParity(t.Context(), request, 1)
	require.NoError(t, err)
	require.NoError(t, owner.runOne(t.Context()))
	first, err := store.ReadParityRequestReport(t.Context(), request.RunID, 1)
	require.NoError(t, err)
	assert.Equal(t, "complete", first.State)
	assert.EqualValues(t, 1, first.PendingSources)
	var attempted, completed int
	require.NoError(t, runtimeDB.QueryRowContext(t.Context(), `SELECT count(*) FILTER (WHERE evidence_generation=1),count(*) FILTER (WHERE candidate_complete) FROM migration_parity_sources WHERE run_id=$1 AND source_kind='raw'`, request.RunID).Scan(&attempted, &completed))
	assert.Equal(t, 1, attempted)
	assert.Equal(t, 1, completed)
	var firstBinding []byte
	var firstObserved time.Time
	require.NoError(t, runtimeDB.QueryRowContext(t.Context(), `SELECT binding_digest,observed_at FROM migration_parity_runs WHERE run_id=$1`, request.RunID).Scan(&firstBinding, &firstObserved))
	require.NotEmpty(t, firstBinding)
	require.False(t, firstObserved.IsZero())

	shutdown := &pgHostedRawShutdown{parityParser: owner.parser, custody: runtimeCustody}
	shutdown.Close()
	runtimeCustody = newDirectParityCustody(t, runtimeCfg, runtimeDB)
	owner, store, restartedRequest := configureDirectParityOwner(t, runtimeCfg, baselineReadCfg, runtimeDB, runtimeCustody)
	require.Equal(t, request, restartedRequest)

	_, err = store.CreateOrResumeParity(t.Context(), restartedRequest, 1)
	require.NoError(t, err)
	var restartedBinding []byte
	var restartedObserved time.Time
	require.NoError(t, runtimeDB.QueryRowContext(t.Context(), `SELECT binding_digest,observed_at FROM migration_parity_runs WHERE run_id=$1`, request.RunID).Scan(&restartedBinding, &restartedObserved))
	assert.Equal(t, firstBinding, restartedBinding)
	assert.Equal(t, firstObserved, restartedObserved)
	require.NoError(t, owner.runOne(t.Context()))
	second, err := store.ReadParityRequestReport(t.Context(), request.RunID, 2)
	require.NoError(t, err)
	assert.Equal(t, "complete", second.State)
	assert.Zero(t, second.PendingSources)
	assert.True(t, second.BaselineSealed)
	assert.False(t, second.Passing, "provider exclusion without independent baseline proof remains partial")
	logDirectParityEvidence(t, runtimeDB, request.RunID)
	require.NoError(t, runtimeDB.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_sources WHERE run_id=$1 AND source_kind='raw' AND candidate_complete`, request.RunID).Scan(&completed))
	assert.Equal(t, 2, completed)
	require.NoError(t, runtimeDB.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_sources WHERE run_id=$1 AND source_kind='raw' AND evidence_generation=2`, request.RunID).Scan(&attempted))
	assert.Equal(t, 1, attempted, "reconstructed owner must attempt only the source left pending by generation one")
	var exclusions, unavailable, matchedSessions int
	require.NoError(t, runtimeDB.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_members WHERE run_id=$1 AND kind='exclusion' AND verdict='partial_unsupported'`, request.RunID).Scan(&exclusions))
	assert.Positive(t, exclusions, "the real background fork must retain a partial exclusion member when the sealed baseline has no provider proof")
	require.NoError(t, runtimeDB.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_sources WHERE run_id=$1 AND source_kind='raw' AND code='exclusion_provenance_unavailable'`, request.RunID).Scan(&unavailable))
	assert.Positive(t, unavailable)
	require.NoError(t, runtimeDB.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_members WHERE run_id=$1 AND kind='session' AND verdict='matched'`, request.RunID).Scan(&matchedSessions))
	assert.Positive(t, matchedSessions, "the non-excluded real source must complete against the sealed baseline")
	var sentinel string
	require.NoError(t, baselineDB.QueryRowContext(t.Context(), `SELECT content FROM messages WHERE session_id='baseline-independent-sentinel'`).Scan(&sentinel))
	assert.Equal(t, "baseline secret sentinel unrelated to raw", sentinel)
}

func TestParityConfiguredOwnerMissingGenerationObjectDoesNotBorrowRealBaseline(t *testing.T) {
	runtimeCfg, _ := hostedRuntimeConfig(t)
	baselineCfg, baselineAdmin := hostedRuntimeConfig(t)
	runtimeDB, err := postgres.OpenHosted(runtimeCfg.PG.URL, runtimeCfg.PG.Schema, runtimeCfg.PG.RawTenant, false)
	require.NoError(t, err)
	defer runtimeDB.Close()
	baselineDB, err := postgres.OpenHosted(baselineCfg.PG.URL, baselineCfg.PG.Schema, baselineCfg.PG.RawTenant, false)
	require.NoError(t, err)
	defer baselineDB.Close()
	runtimeCustody := newDirectParityCustody(t, runtimeCfg, runtimeDB)
	baselineCustody := newDirectParityCustody(t, baselineCfg, baselineDB)
	root := t.TempDir()
	transcript, sidecar := writeParityClaudeCompanionFixture(t, root)
	device := "00000000-0000-4000-8000-000000000139"
	runtimeCapture := newDirectParityCapture(t, runtimeDB, runtimeCustody, parser.AgentClaude, root, device)
	first := runtimeCapture.captureAll(t)
	require.Len(t, first, 1)
	missingObject := generationObjectAtPath(t, first[0], "tool-results/retained.txt")
	require.NoError(t, os.Remove(sidecar))
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString(`{"type":"user","uuid":"u3","parentUuid":"u2","timestamp":"2026-01-02T10:00:03Z","sessionId":"object-loss-session","cwd":"/work/project","message":{"content":"generation two complete"}}` + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	second := runtimeCapture.captureAll(t)
	require.Len(t, second, 1)
	for _, entry := range second[0].manifest.Entries {
		assert.NotContains(t, entry.Objects, missingObject, "generation-one-only companion must be absent from generation two")
	}
	registerDirectParityDevice(t, baselineDB, runtimeCapture.identity)
	baselineCapture := &directParityCapture{identity: runtimeCapture.identity, custody: baselineCustody}
	baseline := replicateDirectParityGenerations(t, runtimeCapture, baselineCapture, append(append([]directCapturedGeneration{}, first...), second...))
	require.Len(t, baseline, 2)
	baselineReadCfg := restrictedDirectParityBaseline(t, baselineCfg, baselineAdmin)
	requireHostedSandbox(t)
	runDirectRawDerivation(t, runtimeCfg, runtimeDB, runtimeCustody, 1)
	runDirectRawDerivation(t, baselineCfg, baselineDB, baselineCustody, 1)
	_, err = baselineDB.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,message_count,user_message_count,data_version,quality_signal_version,secrets_rules_version)
		SELECT 'baseline-independent-sentinel','sentinel','baseline','claude','legacy',1,1,data_version,quality_signal_version,secrets_rules_version FROM sessions WHERE provenance_kind='raw' LIMIT 1;
		INSERT INTO messages(session_id,ordinal,role,content) VALUES('baseline-independent-sentinel',0,'user','baseline secret sentinel unrelated to raw')`)
	require.NoError(t, err)

	quarantineDirectParityObject(t, runtimeCustody, runtimeCapture.identity.TenantID, missingObject)
	owner, store, request := configureDirectParityOwner(t, runtimeCfg, baselineReadCfg, runtimeDB, runtimeCustody)
	_, err = store.CreateOrResumeParity(t.Context(), request, 1)
	require.NoError(t, err)
	require.NoError(t, owner.runOne(t.Context()))
	report, err := store.ReadParityRequestReport(t.Context(), request.RunID, 1)
	require.NoError(t, err)
	assert.Equal(t, "complete", report.State)
	assert.False(t, report.Passing)
	assert.Positive(t, report.PendingSources)
	var code string
	var complete bool
	require.NoError(t, runtimeDB.QueryRowContext(t.Context(), `SELECT code,candidate_complete FROM migration_parity_sources WHERE run_id=$1 AND source_kind='raw'`, request.RunID).Scan(&code, &complete))
	assert.Equal(t, "missing_object", code)
	assert.False(t, complete)
	var candidateFingerprints int
	require.NoError(t, runtimeDB.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_members m JOIN migration_parity_sources s USING(tenant_id,run_id,init_epoch,source_id) WHERE m.run_id=$1 AND s.source_kind='raw' AND m.candidate_fingerprint IS NOT NULL`, request.RunID).Scan(&candidateFingerprints))
	assert.Zero(t, candidateFingerprints, "baseline fingerprints must not become candidate evidence")
	loader := rawderive.ManifestLoader{Store: runtimeCustody, Limits: runtimeCustody.limits}
	_, err = loader.LoadManifest(t.Context(), runtimeCapture.identity, first[0].commit.ManifestID)
	require.NoError(t, err, "generation-one manifest remains present")
	_, err = loader.LoadManifest(t.Context(), runtimeCapture.identity, second[0].commit.ManifestID)
	require.NoError(t, err, "generation-two manifest remains present")
	var sentinel string
	require.NoError(t, baselineDB.QueryRowContext(t.Context(), `SELECT content FROM messages WHERE session_id='baseline-independent-sentinel'`).Scan(&sentinel))
	assert.Equal(t, "baseline secret sentinel unrelated to raw", sentinel)
}

// Losing any captured companion, parser wire field, configured policy or
// publication child row must diverge from the independently synced SQLite rows.
func TestHostedRuntimeCapturedParity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		agent  parser.AgentType
		policy config.ArchiveContent
	}{
		{"claude", parser.AgentClaude, config.ArchiveContentFull}, {"claude_transcript", parser.AgentClaude, config.ArchiveContentTranscripts}, {"zcode", parser.AgentZCode, config.ArchiveContentFull}, {"codex_tools", parser.AgentCodex, config.ArchiveContentFull},
	} {
		t.Run(tc.name, func(t *testing.T) {
			startup, store, admin := startParityRuntime(t, tc.policy)
			root := t.TempDir()
			ids := []string{rawtest.ClaudeID}
			var path string
			if tc.agent == parser.AgentClaude {
				path = rawtest.Claude(t, root)
			} else if tc.agent == parser.AgentZCode {
				rawtest.ZCode(t, root)
				ids = []string{rawtest.ZCodeID, rawtest.ZCodeBillableID}
			} else {
				rawtest.CodexTools(t, root)
				ids = []string{"codex:" + rawtest.CodexToolsID}
			}
			oracle, engine := rawtest.Oracle(t, tc.agent, root, tc.policy, "")
			require.Equal(t, len(ids), engine.SyncAll(t.Context(), nil).Synced)
			client := newHostedCaptureClient(t, startup, store, tc.agent, root)
			receipt := client.capture(t)
			waitCaptureJob(t, admin, receipt.ManifestID, "complete", 1)
			rawtest.EqualStored(t, t.Context(), oracle, store, ids...)
			core, err := postgres.NewRawProjectionStore(store.DB(), postgres.RawProjectionOptions{Tenant: "tenant-runtime"})
			require.NoError(t, err)
			for _, id := range ids {
				resolved, err := core.Resolve(t.Context(), id)
				require.NoError(t, err)
				rawtest.EqualUsageEvents(t, t.Context(), oracle, store.DB(), id, resolved.SessionID)
			}
			if tc.agent == parser.AgentCodex {
				return
			}
			if tc.agent == parser.AgentZCode {
				messages, err := store.GetAllMessages(t.Context(), rawtest.ZCodeBillableID)
				require.NoError(t, err)
				assert.Empty(t, messages)
				usage, err := store.GetSessionUsage(t.Context(), rawtest.ZCodeBillableID, true)
				require.NoError(t, err)
				require.NotNil(t, usage)
				assert.Equal(t, 17, usage.TotalOutputTokens)
				assert.True(t, usage.HasCost)
				return
			}
			if tc.policy == config.ArchiveContentFull {
				require.Greater(t, len(client.last.Entries), 1, "capture must include persisted tool output")
			}
			require.NoError(t, store.RenameSession(rawtest.ClaudeID, new("Curated title")))
			require.NoError(t, oracle.RenameSession(rawtest.ClaudeID, new("Curated title")))
			_, err = store.StarSession(rawtest.ClaudeID)
			require.NoError(t, err)
			_, err = store.PinMessage(rawtest.ClaudeID, 0, new("Review this prompt"))
			require.NoError(t, err)
			sources, err := parser.DiscoverRawCaptureSources(t.Context(), client.provider)
			require.NoError(t, err)
			require.Len(t, sources.Sources, 1)
			unchanged, err := rawcapture.New(client.checkpoint).Capture(t.Context(), client.provider, sources.Sources[0])
			require.NoError(t, err)
			assert.Equal(t, rawcapture.StatusUnchanged, unchanged.Status)
			var revision int64
			require.NoError(t, admin.QueryRow(`SELECT corpus_revision FROM raw_corpus_state`).Scan(&revision))
			replay := client.commit(t, client.last)
			assert.False(t, replay.Created)
			var after int64
			require.NoError(t, admin.QueryRow(`SELECT corpus_revision FROM raw_corpus_state`).Scan(&after))
			assert.Equal(t, revision, after)
			waitCaptureJob(t, admin, receipt.ManifestID, "complete", 1)
			rawtest.AppendClaude(t, path)
			require.Equal(t, 1, engine.SyncAllForceParse(t.Context(), nil).Synced)
			appended := client.capture(t)
			waitCaptureJob(t, admin, appended.ManifestID, "complete", 1)
			rawtest.EqualStored(t, t.Context(), oracle, store, ids...)
			pins, err := store.ListPinnedMessages(t.Context(), rawtest.ClaudeID, "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			assert.Zero(t, pins[0].Ordinal)
			require.NotNil(t, pins[0].Note)
			assert.Equal(t, "Review this prompt", *pins[0].Note)
			stars, err := store.ListStarredSessionIDs(t.Context())
			require.NoError(t, err)
			assert.Equal(t, []string{rawtest.ClaudeID}, stars)
		})
	}
}

// Equal device content coalesces, divergent content becomes explicitly
// ambiguous, and a captured tombstone removes only that device's proof.
func TestHostedRuntimeCapturedConflictAndRemoval(t *testing.T) {
	startup, store, admin := startParityRuntime(t, config.ArchiveContentFull)
	rootA, rootB := t.TempDir(), t.TempDir()
	rawtest.Claude(t, rootA)
	pathB := rawtest.Claude(t, rootB)
	a := newHostedCaptureClient(t, startup, store, parser.AgentClaude, rootA)
	b := newHostedCaptureClient(t, startup, store, parser.AgentClaude, rootB)
	first := a.capture(t)
	waitCaptureJob(t, admin, first.ManifestID, "complete", 1)
	second := b.capture(t)
	waitCaptureJob(t, admin, second.ManifestID, "complete", 1)
	session, err := store.GetSession(t.Context(), rawtest.ClaudeID)
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NoError(t, store.RenameSession(rawtest.ClaudeID, new("Shared curation")))
	rawtest.AppendClaude(t, pathB)
	divergent := b.capture(t)
	waitCaptureJob(t, admin, divergent.ManifestID, "complete", 1)
	_, err = store.GetSession(t.Context(), rawtest.ClaudeID)
	var conflict *db.SessionIdentityError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, "ambiguous", conflict.State)
	require.Len(t, conflict.Variants, 2)
	var contents []string
	for _, id := range conflict.Variants {
		messages, err := store.GetAllMessages(t.Context(), id)
		require.NoError(t, err)
		require.NotEmpty(t, messages)
		contents = append(contents, messages[len(messages)-1].Content)
	}
	assert.ElementsMatch(t, []string{"The build failed.", "Recorded the failure."}, contents)
	_, queued, err := b.checkpoint.QueueTombstone(t.Context(), rawcheckpoint.SourceIdentity{Provider: b.last.Provider, ConfiguredRootID: b.last.ConfiguredRootID, SourceKey: b.last.SourceKey})
	require.NoError(t, err)
	require.True(t, queued)
	removed := b.flush(t)
	waitCaptureJob(t, admin, removed.ManifestID, "complete", 1)
	session, err = store.GetSession(t.Context(), rawtest.ClaudeID)
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotNil(t, session.DisplayName)
	assert.Equal(t, "Shared curation", *session.DisplayName)
	messages, err := store.GetAllMessages(t.Context(), rawtest.ClaudeID)
	require.NoError(t, err)
	require.NotEmpty(t, messages)
	assert.Equal(t, "The build failed.", messages[len(messages)-1].Content)
	_, queued, err = a.checkpoint.QueueTombstone(t.Context(), rawcheckpoint.SourceIdentity{Provider: a.last.Provider, ConfiguredRootID: a.last.ConfiguredRootID, SourceKey: a.last.SourceKey})
	require.NoError(t, err)
	require.True(t, queued)
	removed = a.flush(t)
	waitCaptureJob(t, admin, removed.ManifestID, "complete", 1)
	session, err = store.GetSession(t.Context(), rawtest.ClaudeID)
	require.NoError(t, err)
	assert.Nil(t, session)
}

// A missing fork parent is a real provider partial result: publish its visible
// messages, retry finitely, and recover when capture includes the parent.
func TestHostedRuntimeCapturedPartialRetryExhaustion(t *testing.T) {
	startup, store, admin := startParityRuntime(t, config.ArchiveContentFull)
	root := t.TempDir()
	rawtest.CodexFork(t, root)
	client := newHostedCaptureClient(t, startup, store, parser.AgentCodex, root)
	partial := client.capture(t)
	waitCaptureJob(t, admin, partial.ManifestID, "failed", 2)
	id := "codex:" + rawtest.CodexChildID
	messages, err := store.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "Inspect the fork.", messages[0].Content)
	assert.Equal(t, "Fork inspected.", messages[1].Content)
	require.NoError(t, store.RenameSession(id, new("Partial curation")))
	var revision int64
	require.NoError(t, admin.QueryRow(`SELECT corpus_revision FROM raw_corpus_state`).Scan(&revision))
	replay := client.commit(t, client.last)
	assert.False(t, replay.Created)
	core, err := postgres.NewRawProjectionStore(store.DB(), postgres.RawProjectionOptions{Tenant: "tenant-runtime"})
	require.NoError(t, err)
	rollout, err := core.ScheduleCurrentHeads(t.Context(), "same-version-replay", rawProcessingVersion(), 64)
	require.NoError(t, err)
	assert.True(t, rollout.Done)
	// Three worker polls after failure must not claim an exhausted generation.
	require.Never(t, func() bool {
		var attempts int
		err := admin.QueryRow(`SELECT attempt_count FROM raw_ingest_jobs WHERE manifest_id=$1`, partial.ManifestID).Scan(&attempts)
		return err != nil || attempts != 2
	}, 3200*time.Millisecond, 100*time.Millisecond)
	var after int64
	require.NoError(t, admin.QueryRow(`SELECT corpus_revision FROM raw_corpus_state`).Scan(&after))
	assert.Equal(t, revision, after)
	rawtest.CodexParent(t, root)
	sources, err := parser.DiscoverRawCaptureSources(t.Context(), client.provider)
	require.NoError(t, err)
	var captured bool
	for _, source := range sources.Sources {
		if source.Key == rawtest.CodexChildID || strings.Contains(source.DisplayPath, rawtest.CodexChildID) {
			result, err := rawcapture.New(client.checkpoint).Capture(t.Context(), client.provider, source)
			require.NoError(t, err)
			require.Equal(t, rawcapture.StatusCaptured, result.Status)
			captured = true
		}
	}
	require.True(t, captured)
	recovered := client.flush(t)
	require.Greater(t, len(client.last.Entries), 1, "the parent must be captured as a companion")
	waitCaptureJob(t, admin, recovered.ManifestID, "complete", 1)
	oracle, engine := rawtest.Oracle(t, parser.AgentCodex, root, "", "")
	require.Positive(t, engine.SyncAll(t.Context(), nil).Synced)
	require.NoError(t, oracle.RenameSession(id, new("Partial curation")))
	rawtest.EqualStored(t, t.Context(), oracle, store, id)
}

func TestHostedRuntimeCapturedRelationships(t *testing.T) {
	for _, parentFirst := range []bool{true, false} {
		name := "child_first"
		if parentFirst {
			name = "parent_first"
		}
		t.Run(name, func(t *testing.T) {
			startup, store, admin := startParityRuntime(t, config.ArchiveContentFull)
			root := t.TempDir()
			rawtest.Claude(t, root)
			rawtest.ClaudeChild(t, root)
			oracle, engine := rawtest.Oracle(t, parser.AgentClaude, root, "", "")
			require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
			client := newHostedCaptureClient(t, startup, store, parser.AgentClaude, root)
			sources, err := parser.DiscoverRawCaptureSources(t.Context(), client.provider)
			require.NoError(t, err)
			require.Len(t, sources.Sources, 2)
			slices.SortFunc(sources.Sources, func(a, b parser.SourceRef) int {
				aChild := strings.Contains(a.DisplayPath, rawtest.ClaudeChildID)
				bChild := strings.Contains(b.DisplayPath, rawtest.ClaudeChildID)
				if aChild == bChild {
					return 0
				}
				if aChild == parentFirst {
					return 1
				}
				return -1
			})
			require.Equal(t, !parentFirst, strings.Contains(sources.Sources[0].DisplayPath, rawtest.ClaudeChildID))
			for _, source := range sources.Sources {
				result, err := rawcapture.New(client.checkpoint).Capture(t.Context(), client.provider, source)
				require.NoError(t, err)
				require.Equal(t, rawcapture.StatusCaptured, result.Status)
				receipt := client.flush(t)
				waitCaptureJob(t, admin, receipt.ManifestID, "complete", 1)
			}
			rawtest.EqualStored(t, t.Context(), oracle, store, rawtest.ClaudeID, rawtest.ClaudeChildID)
			children, err := store.GetChildSessions(t.Context(), rawtest.ClaudeID)
			require.NoError(t, err)
			require.Len(t, children, 1)
			assert.Equal(t, rawtest.ClaudeChildID, children[0].ID)

		})
	}
}
