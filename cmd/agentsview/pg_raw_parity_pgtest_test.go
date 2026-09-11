//go:build pgtest

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
