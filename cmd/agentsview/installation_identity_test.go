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
	type captureResult struct {
		data []byte
	}
	for _, tc := range []struct {
		name    string
		enabled string
	}{
		{name: "enabled", enabled: "1"},
		{name: "disabled", enabled: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(telemetry.EnabledEnv, tc.enabled)
			t.Setenv(telemetry.GenericEnabledEnv, "1")
			cfg := config.Config{DataDir: t.TempDir(), InstallationID: "install-one"}
			path := filepath.Join(cfg.DataDir, "telemetry-screen-views")
			require.NoError(t, os.WriteFile(path, []byte(legacy), 0o600))
			telemetryOptions(cfg)
			forwarded := make(chan captureResult, 1)
			handler, _ := telemetryCaptureHandler(cfg, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
				data, err := os.ReadFile(path)
				assert.NoError(t, err)
				forwarded <- captureResult{data: data}
			}))
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, legacy, string(data), "reporter options and handler setup must not migrate")
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/telemetry", nil))

			first := <-forwarded
			if tc.enabled == "0" {
				assert.Equal(t, legacy, string(first.data))
				return
			}
			assert.True(t, jsontext.Value(first.data).IsValid(), "claims must be JSON before forwarding the first capture")

			lock := flock.New(path + ".lock")
			require.NoError(t, lock.Lock())
			secondStarted, secondDone := make(chan struct{}), make(chan struct{})
			lockHeld := true
			defer func() {
				if lockHeld {
					_ = lock.Unlock()
				}
				<-secondDone
			}()
			go func() {
				close(secondStarted)
				handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/telemetry", nil))
				close(secondDone)
			}()
			select {
			case <-secondStarted:
			case <-time.After(5 * time.Second):
				require.FailNow(t, "second capture did not start")
			}
			var second captureResult
			select {
			case second = <-forwarded:
			case <-time.After(100 * time.Millisecond): //nolint:kennlint // timer verifies the one-time handler forwards while the claims lock is held
				require.FailNow(t, "second capture waited on the screen claims lock")
			}
			assert.Equal(t, first.data, second.data)
			require.NoError(t, lock.Unlock())
			lockHeld = false
			select {
			case <-secondDone:
			case <-time.After(5 * time.Second):
				require.FailNow(t, "second capture did not finish")
			}
		})
	}
}

func TestTelemetryScreenClaimsMigration(t *testing.T) {
	const legacy = "install-one 2026-10-08 sessions"
	type captureResult struct {
		data []byte
	}
	t.Run("startup converts claims without a capture", func(t *testing.T) {
		t.Setenv(telemetry.EnabledEnv, "1")
		cfg := config.Config{DataDir: t.TempDir(), InstallationID: "install-one"}
		path := filepath.Join(cfg.DataDir, "telemetry-screen-views")
		require.NoError(t, os.WriteFile(path, []byte(legacy), 0o600))
		_, startMigration := telemetryCaptureHandler(cfg, http.NotFoundHandler())
		lock := flock.New(path + ".lock")
		require.NoError(t, lock.Lock())
		started := make(chan struct{})
		go func() {
			startMigration()
			close(started)
		}()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			require.NoError(t, lock.Unlock())
			<-started
			require.FailNow(t, "starting migration waited for the screen claims lock")
		}
		require.NoError(t, lock.Unlock())
		require.Eventually(t, func() bool {
			data, err := os.ReadFile(path)
			return err == nil && jsontext.Value(data).IsValid()
		}, 5*time.Second, 10*time.Millisecond, "startup must convert claims without a capture")
		require.NoError(t, lock.Lock())
		require.NoError(t, lock.Unlock())
	})
	t.Run("capture arrives first and waits for migration", func(t *testing.T) {
		t.Setenv(telemetry.EnabledEnv, "1")
		cfg := config.Config{DataDir: t.TempDir(), InstallationID: "install-one"}
		path := filepath.Join(cfg.DataDir, "telemetry-screen-views")
		require.NoError(t, os.WriteFile(path, []byte(legacy), 0o600))
		forwarded := make(chan captureResult, 1)
		handler, _ := telemetryCaptureHandler(cfg, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			data, err := os.ReadFile(path)
			assert.NoError(t, err)
			forwarded <- captureResult{data: data}
		}))
		lock := flock.New(path + ".lock")
		require.NoError(t, lock.Lock())
		captureStarted, captureDone := make(chan struct{}), make(chan struct{})
		lockHeld := true
		defer func() {
			if lockHeld {
				_ = lock.Unlock()
			}
			<-captureDone
		}()
		go func() {
			close(captureStarted)
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/telemetry", nil))
			close(captureDone)
		}()
		select {
		case <-captureStarted:
		case <-time.After(5 * time.Second):
			require.FailNow(t, "capture did not start")
		}
		select {
		case <-forwarded:
			require.FailNow(t, "capture reached the forwarding handler while screen claims were locked")
		case <-time.After(100 * time.Millisecond): //nolint:kennlint // timer shows capture waits for migration to release the claims lock
		}
		require.NoError(t, lock.Unlock())
		lockHeld = false
		select {
		case <-captureDone:
		case <-time.After(5 * time.Second):
			require.FailNow(t, "capture did not finish after the screen claims lock was released")
		}
		result := <-forwarded
		assert.True(t, jsontext.Value(result.data).IsValid(), "claims must be JSON before forwarding capture")
	})
}
