package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestRecallExtractRunRefusesWhenDisabled(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

	_, err := executeCommand(newRootCommand(), "recall", "extract", "run")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "recall.extract")
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
