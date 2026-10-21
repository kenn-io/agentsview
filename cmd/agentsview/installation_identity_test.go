package main

import (
	"context"
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
	t.Run("screen claims path", func(t *testing.T) {
		cfg := config.Config{DataDir: t.TempDir()}
		assert.Equal(t, cfg.TelemetryScreenClaimsPath(), telemetryOptions(cfg).ScreenClaimsPath, "the reporter must read the file the handler migrates")
	})

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

func TestTelemetryCaptureHandlerScreenClaimsMigration(t *testing.T) {
	const legacy = "install-one 2026-10-08 sessions"
	setup := func(t *testing.T) (config.Config, chan []byte, http.Handler) {
		t.Helper()
		cfg := config.Config{DataDir: t.TempDir(), InstallationID: "install-one"}
		path := cfg.TelemetryScreenClaimsPath()
		require.NoError(t, os.WriteFile(path, []byte(legacy), 0o600))
		forwarded := make(chan []byte, 1)
		next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			data, err := os.ReadFile(path)
			assert.NoError(t, err)
			forwarded <- data
		})
		return cfg, forwarded, next
	}

	t.Run("enabled", func(t *testing.T) {
		cfg, forwarded, next := setup(t)
		path := cfg.TelemetryScreenClaimsPath()
		handler, _ := telemetryCaptureHandler(cfg, true, next)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, legacy, string(data), "handler setup must not migrate")
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
		<-captureStarted
		select {
		case <-forwarded:
			require.FailNow(t, "capture reached the forwarding handler while screen claims were locked")
		case <-captureDone:
			require.FailNow(t, "capture returned while migration was locked")
		case <-time.After(100 * time.Millisecond): //nolint:kennlint // timer shows capture waits for migration to release the claims lock
		}
		require.NoError(t, lock.Unlock())
		lockHeld = false
		select {
		case <-captureDone:
		case <-time.After(5 * time.Second):
			require.FailNow(t, "capture did not finish after the screen claims lock was released")
		}
		first := <-forwarded
		assert.True(t, jsontext.Value(first).IsValid(), "claims must be JSON before forwarding capture")

		require.NoError(t, lock.Lock())
		secondStarted, secondDone := make(chan struct{}), make(chan struct{})
		lockHeld = true
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
		<-secondStarted
		var second []byte
		select {
		case second = <-forwarded:
		case <-time.After(2 * time.Second): // timer verifies forwarding before the 3s migration lock timeout
			require.FailNow(t, "second capture waited on the screen claims lock")
		}
		assert.Equal(t, first, second)
		require.NoError(t, lock.Unlock())
		lockHeld = false
		select {
		case <-secondDone:
		case <-time.After(5 * time.Second):
			require.FailNow(t, "second capture did not finish")
		}
	})

	t.Run("disabled", func(t *testing.T) {
		cfg, forwarded, next := setup(t)
		handler, _ := telemetryCaptureHandler(cfg, false, next)
		lock := flock.New(cfg.TelemetryScreenClaimsPath() + ".lock")
		require.NoError(t, lock.Lock())
		captureDone := make(chan struct{})
		defer func() {
			_ = lock.Unlock()
			<-captureDone
		}()
		go func() {
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/telemetry", nil))
			close(captureDone)
		}()
		select {
		case <-captureDone:
		case <-time.After(5 * time.Second):
			require.FailNow(t, "disabled capture waited on the screen claims lock")
		}
		assert.Equal(t, legacy, string(<-forwarded))
	})

	t.Run("canceled capture leaves migration running", func(t *testing.T) {
		cfg, forwarded, next := setup(t)
		path := cfg.TelemetryScreenClaimsPath()
		handler, _ := telemetryCaptureHandler(cfg, true, next)
		lock := flock.New(path + ".lock")
		require.NoError(t, lock.Lock())
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		captureStarted, captureDone := make(chan struct{}), make(chan struct{})
		defer func() {
			_ = lock.Unlock()
			<-captureDone
		}()
		go func() {
			close(captureStarted)
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/telemetry", nil))
			close(captureDone)
		}()
		<-captureStarted
		select {
		case <-forwarded:
			require.FailNow(t, "capture reached the forwarding handler while screen claims were locked")
		case <-captureDone:
			require.FailNow(t, "capture returned while migration was locked")
		case <-time.After(100 * time.Millisecond): //nolint:kennlint // timer shows a live capture waits while the migration lock is held
		}
		cancel()
		select {
		case <-captureDone:
		case <-time.After(5 * time.Second):
			require.FailNow(t, "canceled capture did not return while migration was locked")
		}
		assert.Empty(t, forwarded, "canceled capture must not reach the forwarding handler")
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, legacy, string(data), "migration must still be waiting for the held lock")
		require.NoError(t, lock.Unlock())
		require.Eventually(t, func() bool {
			data, err := os.ReadFile(path)
			return err == nil && jsontext.Value(data).IsValid()
		}, 5*time.Second, 10*time.Millisecond, "migration must finish after its only capture was canceled")
		require.NoError(t, lock.Lock())
		require.NoError(t, lock.Unlock())
	})

	t.Run("startup migrates without a capture", func(t *testing.T) {
		cfg, _, next := setup(t)
		path := cfg.TelemetryScreenClaimsPath()
		_, startMigration := telemetryCaptureHandler(cfg, true, next)
		lock := flock.New(path + ".lock")
		require.NoError(t, lock.Lock())
		started := make(chan struct{})
		defer func() {
			_ = lock.Unlock()
			<-started
		}()
		go func() {
			startMigration()
			close(started)
		}()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			require.FailNow(t, "starting migration waited for the screen claims lock")
		}
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, legacy, string(data), "migration must still be waiting for the held lock")
		require.NoError(t, lock.Unlock())
		require.Eventually(t, func() bool {
			data, err := os.ReadFile(path)
			return err == nil && jsontext.Value(data).IsValid()
		}, 5*time.Second, 10*time.Millisecond, "startup must convert claims without a capture")
		require.NoError(t, lock.Lock())
		require.NoError(t, lock.Unlock())
	})
}
