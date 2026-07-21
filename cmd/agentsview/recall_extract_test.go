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
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/recall/extract"
	"go.kenn.io/agentsview/internal/secrets"
)

// extractModelStub answers every /chat/completions call with one fact entry.
func extractModelStub(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			content := `{"entries":[{"type":"fact","title":"t",` +
				`"body":"b","entities":[]}]}`
			body := map[string]any{
				"choices": []map[string]any{{
					"finish_reason": "stop",
					"message": map[string]any{
						"role": "assistant", "content": content,
					},
				}},
				"usage": map[string]any{
					"prompt_tokens": 5, "completion_tokens": 2,
				},
			}
			_ = json.NewEncoder(w).Encode(body)
		}))
	t.Cleanup(server.Close)
	return server
}

// writeExtractConfig writes a config.toml enabling extraction against url.
func writeExtractConfig(t *testing.T, dataDir, url string) {
	t.Helper()
	content := fmt.Sprintf(`[recall.extract]
enabled = true
model = "test-model"
quiet_period = "0s"

[recall.extract.servers.local]
endpoint = %q
`, url)
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "config.toml"), []byte(content), 0o644))
}

// seedExtractCLISession stores one ended, extractable session.
func seedExtractCLISession(t *testing.T, dataDir string) {
	t.Helper()
	d, err := db.Open(filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	defer d.Close()
	ended := time.Now().Add(-time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	require.NoError(t, d.UpsertSession(db.Session{
		ID:           "extract-session",
		Project:      "proj",
		Machine:      "local",
		Agent:        "claude",
		EndedAt:      &ended,
		MessageCount: 2,
	}))
	require.NoError(t, d.InsertMessages([]db.Message{
		{SessionID: "extract-session", Ordinal: 0, Role: "user",
			Content: "fix the flaky test"},
		{SessionID: "extract-session", Ordinal: 1, Role: "assistant",
			Content: "pinned the clock in the scheduler test"},
	}))
	require.NoError(t, d.ReplaceSessionSecretFindings(
		"extract-session", nil, 0, secrets.RulesVersion()))
}

func TestRecallExtractCommandsRejectRemoteServer(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	server := extractModelStub(t)
	writeExtractConfig(t, dataDir, server.URL)

	for _, sub := range [][]string{
		{"recall", "extract", "run"},
		{"recall", "extract", "status"},
		{"recall", "extract", "activate"},
		{"recall", "extract", "retire", "fp"},
		{"recall", "extract", "doctor"},
	} {
		args := append(append([]string{}, sub...),
			"--server", "http://remote:8080")
		_, err := executeCommand(newRootCommand(), args...)
		require.Error(t, err, "%v must not silently ignore --server", sub)
		assert.Contains(t, err.Error(), "--server",
			"%v error must name the unsupported flag", sub)
	}
}

func TestRecallExtractRunAndStatusEndToEnd(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	server := extractModelStub(t)
	writeExtractConfig(t, dataDir, server.URL)
	seedExtractCLISession(t, dataDir)

	out, err := executeCommand(newRootCommand(),
		"recall", "extract", "run", "--format", "json")
	require.NoError(t, err, "run output: %s", out)
	var run struct {
		Sessions int  `json:"sessions"`
		Failed   int  `json:"failed"`
		Entries  int  `json:"entries"`
		Units    int  `json:"units"`
		Active   bool `json:"activated"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &run), "stdout: %q", out)
	assert.Equal(t, 1, run.Sessions)
	assert.Equal(t, 2, run.Entries)
	assert.True(t, run.Active)

	out, err = executeCommand(newRootCommand(),
		"recall", "extract", "status", "--format", "json")
	require.NoError(t, err, "status output: %s", out)
	var status extract.Status
	require.NoError(t, json.Unmarshal([]byte(out), &status), "stdout: %q", out)
	assert.Equal(t, 1, status.Stats.Done)
	assert.Equal(t, 2, status.Stats.Entries)
	assert.NotEmpty(t, status.Fingerprint)
}

// TestSetupExtractReconcileOnlyWhenDisabled pins that retraction runs even
// with extraction disabled: a generation activated while it was enabled
// keeps serving, so setup returns a reconcile-only scheduler when a
// generation exists and nil when none does.
func TestSetupExtractReconcileOnlyWhenDisabled(t *testing.T) {
	ctx := context.Background()

	t.Run("nil without a generation", func(t *testing.T) {
		d, err := db.Open(filepath.Join(t.TempDir(), "sessions.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = d.Close() })
		sched, err := setupRecallExtraction(config.Config{}, d, nil)
		require.NoError(t, err)
		assert.Nil(t, sched,
			"a disabled daemon with no generation starts no scheduler")
	})

	t.Run("reconciles an ineligible session's entries", func(t *testing.T) {
		d, err := db.Open(filepath.Join(t.TempDir(), "sessions.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = d.Close() })
		_, err = d.EnsureExtractGeneration(ctx, db.ExtractGeneration{
			Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
		})
		require.NoError(t, err)
		require.NoError(t, d.UpsertSession(db.Session{
			ID: "sess-gone", Project: "p", Machine: "m", Agent: "claude",
		}))
		_, err = d.InsertExtractedRecallEntries(ctx, []db.RecallEntry{{
			ID: "e-gone", Type: "fact", ReviewState: "unreviewed_auto",
			Status: "accepted", Title: "t", Body: "b",
			SourceSessionID: "sess-gone", SourceRunID: "fp-a",
			ProvenanceOK: true,
		}})
		require.NoError(t, err)
		require.NoError(t, d.SoftDeleteSession("sess-gone"))

		sched, err := setupRecallExtraction(config.Config{}, d, nil)
		require.NoError(t, err)
		require.NotNil(t, sched,
			"a disabled daemon with a generation runs reconciliation")

		runCtx, cancel := context.WithCancel(ctx)
		go sched.Run(runCtx)
		require.Eventually(t, func() bool {
			entry, err := d.GetRecallEntry(ctx, "e-gone")
			return err == nil && entry == nil
		}, 5*time.Second, 10*time.Millisecond,
			"the startup reconcile pass must retract the ineligible entry")
		cancel()
		sched.Stop()
	})
}

func TestRecallExtractRunRefusesWhenDisabled(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

	_, err := executeCommand(newRootCommand(), "recall", "extract", "run")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "recall.extract")
}

// TestRecallExtractRunRejectsNegativeLimit pins that a negative --limit is
// refused at the CLI boundary: the DB reads "<= 0" as unlimited, so a
// negative would scan the whole eligible archive — a surprise model-usage
// burst. The check runs before config load, so it needs no valid config.
func TestRecallExtractRunRejectsNegativeLimit(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

	_, err := executeCommand(newRootCommand(),
		"recall", "extract", "run", "--limit", "-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "limit")
}

func TestRecallExtractRetireRequiresForceForActive(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	server := extractModelStub(t)
	writeExtractConfig(t, dataDir, server.URL)
	seedExtractCLISession(t, dataDir)

	_, err := executeCommand(newRootCommand(), "recall", "extract", "run")
	require.NoError(t, err)

	out, err := executeCommand(newRootCommand(),
		"recall", "extract", "status", "--format", "json")
	require.NoError(t, err)
	var status extract.Status
	require.NoError(t, json.Unmarshal([]byte(out), &status))

	_, err = executeCommand(newRootCommand(),
		"recall", "extract", "retire", status.Fingerprint)
	require.Error(t, err, "retiring the active generation needs --force")

	_, err = executeCommand(newRootCommand(),
		"recall", "extract", "retire", status.Fingerprint, "--force")
	require.NoError(t, err)
}

func TestRecallExtractDoctorProbesTheModel(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	server := extractModelStub(t)
	writeExtractConfig(t, dataDir, server.URL)

	out, err := executeCommand(newRootCommand(), "recall", "extract", "doctor")
	require.NoError(t, err, "doctor output: %s", out)
	assert.Contains(t, out, "Fingerprint:")
	assert.Contains(t, out, "test-model")
	assert.Contains(t, out, "probe: ok")
}

// TestExtractProbeTimeoutExceedsConfiguredRequestTimeout pins that the
// doctor probe's context deadline is derived from the configured per-request
// timeout rather than fixed: a fixed deadline shorter than the configured
// timeout fails slow-model configurations during diagnostics even though
// they work during normal extraction.
func TestExtractProbeTimeoutExceedsConfiguredRequestTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{time.Second, 10 * time.Minute} {
		assert.Greater(t, extractProbeTimeout(timeout), timeout,
			"the context deadline must leave the configured request "+
				"timeout as the bound that fires for %s", timeout)
	}
}

func TestRecallExtractDoctorSanitizesEndpointErrors(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	// A malicious endpoint answers the probe with terminal control
	// sequences in its error body: an OSC 8 hyperlink and a CSI clear.
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(
				"denied \x1b]8;;http://evil.example\x07click here" +
					"\x1b]8;;\x07 \x1b[2Jwiped"))
		}))
	t.Cleanup(server.Close)
	writeExtractConfig(t, dataDir, server.URL)

	_, err := executeCommand(newRootCommand(), "recall", "extract", "doctor")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "probe:")
	assert.NotContains(t, err.Error(), "\x1b",
		"an endpoint error body must not reach the terminal with escape "+
			"sequences intact")
}

func TestRecallExtractPreviewSubcommandBuildsChunks(t *testing.T) {
	dataDir := t.TempDir()
	setRecallTestEnv(t, dataDir)
	seedRecallEntryFixture(t, dataDir)

	out, err := executeCommand(
		newRootCommand(),
		"recall", "extract", "preview",
		"--session", "recall-session",
		"--chunk-max-chars", "120",
		"--format", "json",
	)
	require.NoError(t, err)
	var got struct {
		SessionID  string `json:"session_id"`
		ChunkCount int    `json:"chunk_count"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got),
		"stdout should be valid JSON: %q", out)
	assert.Equal(t, "recall-session", got.SessionID)
	assert.GreaterOrEqual(t, got.ChunkCount, 2)
}

func TestResolveExtractDistillationAppliesOverrides(t *testing.T) {
	temp := 0.3
	cfg := config.RecallExtractConfig{
		Enabled:          true,
		Model:            "qwen3.5-27b",
		Deployment:       "gpu-a",
		MaxWindowChars:   40000,
		MaxTokens:        512,
		QuietPeriod:      "30m",
		BackstopInterval: "1h",
		FailureBackoff:   "2h",
		Servers: map[string]config.RecallExtractServerConfig{
			"local": {Endpoint: "http://127.0.0.1:30000/v1", Timeout: "120s"},
		},
		Request: config.RecallExtractRequestConfig{
			Temperature: &temp,
			ExtraBody:   map[string]any{"custom": true},
		},
	}
	dist, err := resolveExtractDistillation(cfg)
	require.NoError(t, err)
	assert.Equal(t, "local", dist.Server)
	assert.Equal(t, "qwen", dist.Profile,
		"model prefix must select the qwen profile")
	assert.Equal(t, "http://127.0.0.1:30000/v1", dist.Client.BaseURL)
	assert.Equal(t, 0.3, dist.Client.Request.Temperature)
	assert.Equal(t, 512, dist.Client.Request.MaxTokens)
	assert.Equal(t, map[string]any{"custom": true},
		dist.Client.Request.ExtraBody,
		"configured extra_body replaces the profile's")
	assert.Equal(t, 40000, dist.Segmenter.MaxWindowChars)
	assert.Equal(t,
		extract.ModelIdentity{Model: "qwen3.5-27b", Deployment: "gpu-a"},
		dist.Identity)
	assert.Equal(t, 30*time.Minute, dist.Quiet)
	assert.Equal(t, 2*time.Hour, dist.Backoff)
	assert.Equal(t, time.Hour, dist.Backstop)

	cfg.Enabled = false
	_, err = resolveExtractDistillation(cfg)
	require.Error(t, err, "disabled extraction cannot resolve")

	cfg.Enabled = true
	cfg.Prompts.Profile = "nonexistent"
	_, err = resolveExtractDistillation(cfg)
	require.Error(t, err, "unknown profile must surface")
}

func TestResolveExtractDistillationLoadsPromptDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "intent.txt"), []byte("custom intent\n"), 0o644))
	cfg := config.RecallExtractConfig{
		Enabled:          true,
		Model:            "m",
		MaxWindowChars:   50000,
		QuietPeriod:      "30m",
		BackstopInterval: "1h",
		FailureBackoff:   "1h",
		Servers: map[string]config.RecallExtractServerConfig{
			"local": {Endpoint: "http://127.0.0.1:30000/v1", Timeout: "120s"},
		},
		Prompts: config.RecallExtractPromptsConfig{Dir: dir},
	}
	dist, err := resolveExtractDistillation(cfg)
	require.NoError(t, err)
	assert.Equal(t, "custom intent", dist.Prompts[extract.RoleIntent])
	assert.NotEmpty(t, dist.Prompts[extract.RoleAction],
		"roles without override files keep profile prompts")
}

func TestRecallExtractRunRefusesWhileOfflineWriterHoldsLock(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	server := extractModelStub(t)
	writeExtractConfig(t, dataDir, server.URL)
	seedExtractCLISession(t, dataDir)
	holdWriteOwnerLockForTest(t, dataDir)

	_, err := executeCommand(newRootCommand(), "recall", "extract", "run")
	require.Error(t, err,
		"an extraction pass is a multi-step write and must not overlap "+
			"another offline writer or a resync database swap")
	assert.Contains(t, err.Error(), "lock")
}

func TestResolveExtractDistillationRefusesAllRedirects(t *testing.T) {
	cfg := config.RecallExtractConfig{
		Enabled:          true,
		Model:            "m",
		MaxWindowChars:   50000,
		QuietPeriod:      "30m",
		BackstopInterval: "1h",
		FailureBackoff:   "1h",
		Servers: map[string]config.RecallExtractServerConfig{
			"remote": {Endpoint: "https://build-box:30000/v1", Timeout: "120s"},
		},
	}
	dist, err := resolveExtractDistillation(cfg)
	require.NoError(t, err)
	redirect := dist.Client.HTTPClient.CheckRedirect
	require.NotNil(t, redirect,
		"the model client must refuse redirects: a compliant endpoint "+
			"must not steer the extraction POST anywhere")

	// A 307/308 replays the POST — transcript content included — to
	// whatever destination the endpoint names. A name-based same-origin
	// allowance is not enough: the redirect target is re-resolved, so a
	// rebinding hostname passes the string check while the connection
	// lands on a loopback or LAN service that trusts local callers. No
	// redirect is followed at all, same-origin included.
	for name, target := range map[string]string{
		"same-origin":    "https://build-box:30000/v1/chat/completions",
		"plaintext":      "http://build-box:30000/v1/x",
		"cross-origin":   "https://other-box/v1/x",
		"different port": "https://build-box:30001/v1/x",
		"loopback":       "http://127.0.0.1:9/v1/x",
	} {
		req := &http.Request{URL: mustParseURL(t, target)}
		require.Error(t, redirect(req, nil),
			"a %s redirect must be refused", name)
	}

	// The refusal message must not echo credentials the redirect target
	// carries: it reaches stderr and stored failure rows.
	credentialed := &http.Request{URL: mustParseURL(t,
		"https://tester:hunter2@build-box:30000/v1?sig=sekret")}
	err = redirect(credentialed, nil)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "hunter2")
	assert.NotContains(t, err.Error(), "sekret")
	assert.NotContains(t, err.Error(), "tester:")
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

// TestRecallExtractDoctorRedactsEndpointCredentials pins that doctor output
// never shows URL userinfo: endpoints may carry Basic-auth credentials, and
// doctor output lands in terminals, CI logs, and pasted diagnostics.
func TestRecallExtractDoctorRedactsEndpointCredentials(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	server := extractModelStub(t)
	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	writeExtractConfig(t, dataDir,
		"http://tester:hunter2@"+parsed.Host)

	out, cmdErr := executeCommand(newRootCommand(), "recall", "extract", "doctor")
	require.NoError(t, cmdErr, "doctor output: %s", out)
	assert.Contains(t, out, parsed.Host,
		"the endpoint host stays visible for diagnostics")
	assert.NotContains(t, out, "hunter2",
		"a Basic-auth password must never reach the terminal")
	assert.NotContains(t, out, "tester:",
		"URL userinfo must be dropped, not just the password")
}
