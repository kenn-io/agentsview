package main

import (
	"encoding/json/jsontext"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/telemetry"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestSessionSyncUsesInstallationIdentityAndPreservesHistoricalNames(t *testing.T) {
	isolateDirectCLISources(t)
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	t.Setenv("AGENTSVIEW_TELEMETRY_ENABLED", "0")
	const id = "0123456789abcdef0123456789abcdef"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "telemetry-install-id"), []byte(id), 0o600))
	root := filepath.Join(dir, "claude", "project")
	require.NoError(t, os.MkdirAll(root, 0o700))
	path := filepath.Join(root, "current.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(testjsonl.NewSessionBuilder().
		AddClaudeUser("2026-07-12T00:00:00Z", "installation identity check").String()), 0o600))

	for _, name := range []string{"Laptop", "Renamed Laptop"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"),
			[]byte("local_machine_name = \""+name+"\"\n"), 0o600))
		cfg, err := config.LoadMinimal()
		require.NoError(t, err)
		cfg.AgentDirs = map[parser.AgentType][]string{parser.AgentClaude: {filepath.Dir(root)}}
		svc, closeService, err := syncService(t.Context(), cfg, transport{Mode: transportDirect})
		require.NoError(t, err)
		detail, err := svc.Sync(t.Context(), service.SyncInput{Path: path})
		closeService()
		require.NoError(t, err)
		assert.Equal(t, id, detail.Machine)

		database, err := openDB(t.Context(), cfg)
		require.NoError(t, err)
		if name == "Laptop" {
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{
				ID: "historical", Project: "project", Agent: "claude", Machine: "old-host",
			}))
		}
		labels, err := database.GetMachineLabels(t.Context())
		require.NoError(t, err)
		assert.Equal(t, map[string]string{id: name}, labels)
		historical, err := database.GetSession(t.Context(), "historical")
		require.NoError(t, err)
		require.NotNil(t, historical)
		assert.Equal(t, "old-host", historical.Machine)
		require.NoError(t, database.Close())
	}
}

func TestTelemetryOptionsAllowCoreActionValues(t *testing.T) {
	t.Setenv(telemetry.EnabledEnv, "0")
	reporter, err := telemetry.NewReporter(telemetryOptions(config.Config{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reporter.Close()) })

	for _, tc := range []struct {
		event, property, value string
	}{
		{telemetry.EventSessionViewed, "agent", "codex"},
		{telemetry.EventSessionViewed, "agent", "freebuff"},
		{telemetry.EventInsightGenerated, "kind", "daily_activity"},
		{telemetry.EventInsightGenerated, "kind", "agent_analysis"},
		{telemetry.EventInsightGenerated, "kind", "prompt_maturity_review"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			props, err := reporter.SanitizeProperties(tc.event, map[string]any{tc.property: tc.value})
			require.NoError(t, err)
			assert.Equal(t, tc.value, props[tc.property])
		})
	}
}

func TestTelemetryOptionsPassInstallationAge(t *testing.T) {
	const existingID = "0123456789abcdef0123456789abcdef"
	tests := []struct {
		name       string
		existingID string
		wantRecent bool
	}{
		{name: "fresh install", wantRecent: true},
		{name: "existing ID without a recorded time", existingID: existingID},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("AGENTSVIEW_DATA_DIR", dir)
			t.Setenv("AGENTSVIEW_TELEMETRY_ENABLED", "0")
			if tc.existingID != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "telemetry-install-id"), []byte(tc.existingID+"\n"), 0o600))
			}

			cfg, err := config.LoadMinimal()
			require.NoError(t, err)
			opts := telemetryOptions(cfg)
			assert.Equal(t, cfg.InstallationID, opts.InstallationID)
			assert.Equal(t, filepath.Join(dir, "telemetry-screen-views"), opts.ScreenClaimsPath)
			if tc.wantRecent {
				assert.WithinDuration(t, time.Now(), opts.InstalledAt, time.Minute)
			} else {
				assert.Equal(t, tc.existingID, opts.InstallationID)
				assert.True(t, opts.InstalledAt.IsZero(), "existing installs report without an install age")
			}

			restarted, err := config.LoadMinimal()
			require.NoError(t, err)
			restartedOpts := telemetryOptions(restarted)
			assert.Equal(t, opts.InstallationID, restartedOpts.InstallationID)
			assert.True(t, opts.InstalledAt.Equal(restartedOpts.InstalledAt), "restart keeps the recorded creation time")
		})
	}
}

func TestTelemetryCaptureMigratesClaimsOnceBeforeForwarding(t *testing.T) {
	const legacy = "install-one 2026-10-08 sessions"
	for _, tc := range []struct {
		name, enabled  string
		startWithLock  bool
		wantMigrations bool
	}{
		{"capture before start", "1", false, true},
		{"start after server ready", "1", true, true},
		{"disabled", "0", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(telemetry.EnabledEnv, tc.enabled)
			t.Setenv(telemetry.GenericEnabledEnv, "1")
			cfg := config.Config{DataDir: t.TempDir(), InstallationID: "install-one"}
			path := filepath.Join(cfg.DataDir, "telemetry-screen-views")
			require.NoError(t, os.WriteFile(path, []byte(legacy), 0o600))
			telemetryOptions(cfg)
			wantLegacy := !tc.wantMigrations
			forwarded := 0
			handler, startMigration := telemetryCaptureHandler(cfg, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
				forwarded++
				data, err := os.ReadFile(path)
				assert.NoError(t, err)
				if wantLegacy {
					assert.Equal(t, legacy, string(data))
				} else {
					assert.True(t, jsontext.Value(data).IsValid(), "claims must be JSON before forwarding capture")
				}
			}))
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, legacy, string(data), "reporter options and handler setup must not migrate")
			if tc.startWithLock {
				lock := flock.New(path + ".lock")
				require.NoError(t, lock.Lock())
				started := make(chan struct{})
				go func() {
					startMigration()
					close(started)
				}()
				select {
				case <-started:
				case <-time.After(time.Second):
					require.NoError(t, lock.Unlock())
					require.FailNow(t, "starting migration waited for the screen claims lock")
				}
				require.NoError(t, lock.Unlock())
				if tc.wantMigrations {
					assert.Eventually(t, func() bool {
						data, err := os.ReadFile(path)
						return err == nil && jsontext.Value(data).IsValid()
					}, 5*time.Second, 10*time.Millisecond, "start must convert claims without a capture")
				}
			}
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/telemetry", nil))
			require.Equal(t, 1, forwarded)
			if tc.wantMigrations {
				// A later request must leave claims alone, even if they change.
				require.NoError(t, os.WriteFile(path, []byte(legacy), 0o600))
				wantLegacy = true
				handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/telemetry", nil))
				assert.Equal(t, 2, forwarded)
			}
		})
	}
}
