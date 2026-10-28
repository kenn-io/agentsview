package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/clickhouse"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/stacktest"
)

// Run the CLI in a child so serve's fatal startup errors and real stderr are
// observable without starting a daemon in the test runner.
func TestScopedConfigProcess(t *testing.T) {
	if os.Getenv("AGENTSVIEW_TEST_SCOPED_CONFIG") != "1" {
		return
	}
	i := slices.Index(os.Args, "--")
	require.Positive(t, i)
	if err := executeCLIWithLegacyFlagCompat(os.Args[i+1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func runScopedConfigCLI(t *testing.T, env []string, args ...string) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestScopedConfigProcess$", "--"}, args...)...)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "AGENTSVIEW_") {
			child.Env = append(child.Env, entry)
		}
	}
	child.Env = append(child.Env, "AGENTSVIEW_TEST_SCOPED_CONFIG=1", "AGENTSVIEW_NO_DAEMON=1",
		"AGENTSVIEW_DATA_DIR="+os.Getenv("AGENTSVIEW_DATA_DIR"))
	child.Env = append(child.Env, env...)
	var stdout, stderr bytes.Buffer
	child.Stdout, child.Stderr = &stdout, &stderr
	err := child.Run()
	if ctx.Err() != nil {
		stacktest.Dump(t)
	}
	require.NoError(t, ctx.Err(), "CLI must finish without waiting for a server")
	return stdout.String(), stderr.String(), err
}

func seedScopedConfigArchive(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "AGENTSVIEW_") || strings.HasPrefix(name, "AGENT_VIEWER_") {
			t.Setenv(name, "")
			require.NoError(t, os.Unsetenv(name))
		}
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	seedExportSessionsArchiveAt(t, filepath.Join(dir, "sessions.db"))
}

func TestScopedConfigExportIgnoresUnusedSections(t *testing.T) {
	for _, tt := range []struct{ section, data string }{
		{"insights", "[insights]\nendpoint = 'http://models.example/v1'\nmodel = 'remote'\n"},
		{"vector", "[vector]\nenabled = true\n"},
		{"proxy", "[proxy]\nmode = 'invalid'\n"},
		{"public_url", "public_url = 'not-a-url'\n"},
		{"public_origins", "public_origins = ['not-an-origin']\n"},
		{"recall.extract", "[recall.extract]\nenabled = true\nmodel = 'local'\n"},
		{"daemon_idle_timeout", "daemon_idle_timeout = '-1s'\n"},
		{"chart_palette", "chart_palette = 'invalid'\n"},
		{"zoom_level", "zoom_level = 12\n"},
		{"pg", "[pg.all]\nurl = 'postgres://db.example/archive'\n"},
		{"clickhouse", "[clickhouse.all]\nurl = 'clickhouse://db.example/archive'\n"},
	} {
		t.Run(tt.section, func(t *testing.T) {
			seedScopedConfigArchive(t)
			path := filepath.Join(os.Getenv("AGENTSVIEW_DATA_DIR"), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte("cursor_secret = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='\n"+tt.data), 0o600))
			stdout, stderr, err := runScopedConfigCLI(t, nil, "export", "sessions", "--format", "json")
			require.NoError(t, err, stderr)
			assert.Len(t, decodeExportSessionsDocument(t, stdout).Sessions, 2)
			assert.Contains(t, stderr, "warning:")
			assert.Contains(t, stderr, tt.section)
			persisted, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Contains(t, string(persisted), strings.TrimSpace(tt.data), "loading must preserve invalid unused settings")

			_, stderr, err = runScopedConfigCLI(t, nil, "serve", "--no-browser", "--no-sync")
			require.Error(t, err, "server startup must reject the same config")
			assert.Contains(t, stderr, "loading config")
		})
	}
}

func TestScopedConfigDeploymentOnlyAffectsServer(t *testing.T) {
	for _, tt := range []struct {
		name, data, want string
		env              []string
	}{
		{"missing token file", "", "AGENTSVIEW_AUTH_TOKEN_FILE", []string{"AGENTSVIEW_MODE=serve", "AGENTSVIEW_AUTH_TOKEN_FILE=/nonexistent/scoped-config-token"}},
		{"embeddings conflict", "[vector]\nenabled = false\n", "AGENTSVIEW_EMBEDDINGS_*", []string{"AGENTSVIEW_MODE=serve", "AGENTSVIEW_EMBEDDINGS_ENDPOINT=http://127.0.0.1:30000/v1"}},
		{"invalid mode", "", "AGENTSVIEW_MODE", []string{"AGENTSVIEW_MODE=invalid"}},
		{"invalid server boolean", "", "AGENTSVIEW_REQUIRE_AUTH", []string{"AGENTSVIEW_MODE=serve", "AGENTSVIEW_REQUIRE_AUTH=invalid"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			seedScopedConfigArchive(t)
			require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("AGENTSVIEW_DATA_DIR"), "config.toml"), []byte(tt.data), 0o600))
			stdout, stderr, err := runScopedConfigCLI(t, tt.env, "export", "sessions", "--format", "json")
			require.NoError(t, err, stderr)
			assert.Len(t, decodeExportSessionsDocument(t, stdout).Sessions, 2)
			_, stderr, err = runScopedConfigCLI(t, tt.env, "serve", "--no-browser", "--no-sync")
			require.Error(t, err)
			assert.Contains(t, stderr, tt.want)
		})
	}
}

func TestScopedConfigParseErrorsAreGlobal(t *testing.T) {
	for _, data := range []string{"[insights\n", "[vector]\nunknown_key = true\n", "[pg]\nunknown_key = true\n"} {
		t.Run(data, func(t *testing.T) {
			seedScopedConfigArchive(t)
			require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("AGENTSVIEW_DATA_DIR"), "config.toml"), []byte(data), 0o600))
			for _, args := range [][]string{{"export", "sessions"}, {"serve", "--no-browser", "--no-sync"}} {
				_, stderr, err := runScopedConfigCLI(t, nil, args...)
				require.Error(t, err)
				assert.Contains(t, stderr, "loading config")
			}
		})
	}
}

func TestScopedConfigWarnsForEveryInvalidUnusedSection(t *testing.T) {
	seedScopedConfigArchive(t)
	data := "public_url='invalid'\n[vector]\nenabled=true\n[insights]\nmodel='incomplete'\n"
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("AGENTSVIEW_DATA_DIR"), "config.toml"), []byte(data), 0o600))
	stdout, stderr, err := runScopedConfigCLI(t, nil, "export", "sessions", "--format", "json")
	require.NoError(t, err, stderr)
	assert.Len(t, decodeExportSessionsDocument(t, stdout).Sessions, 2)
	for _, section := range []string{"public_url", "[vector]", "[insights]"} {
		assert.Contains(t, stderr, section)
	}
	assert.Equal(t, 3, strings.Count(stderr, "warning: invalid "), "one warning per invalid section:\n%s", stderr)
	_, stderr, err = runScopedConfigCLI(t, nil, "daemon", "status")
	require.NoError(t, err, stderr)
}

func TestScopedConfigRecallVectorModes(t *testing.T) {
	for _, command := range []string{"query", "brief"} {
		for _, mode := range []string{"vector", "hybrid", " VeCtOr ", " HYBRID "} {
			t.Run(command+"/"+mode, func(t *testing.T) {
				seedScopedConfigArchive(t)
				t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
				data := "[vector]\nenabled=true\nrecall_max_revision_lag=-1\n"
				require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("AGENTSVIEW_DATA_DIR"), "config.toml"), []byte(data), 0o600))
				root := newRootCommand()
				root.SetArgs([]string{"recall", command, "task", "--mode", mode})
				err := root.Execute()
				require.ErrorContains(t, err, "recall_max_revision_lag must be 0 or greater")
			})
		}
	}
}

func TestScopedConfigDaemonTransportUsesDeploymentAuth(t *testing.T) {
	seedScopedConfigArchive(t)
	t.Setenv("AGENTSVIEW_MODE", "serve")
	t.Setenv("AGENTSVIEW_AUTH_TOKEN_FILE", filepath.Join(os.Getenv("HOME"), "missing-token"))
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	cfg, err := config.LoadMinimalFor(config.ScopeLocal)
	require.NoError(t, err)
	_, err = ensureTransport(&cfg, transportIntentRead, 0)
	require.ErrorContains(t, err, "AGENTSVIEW_AUTH_TOKEN_FILE")
}

func TestScopedConfigReplicaTargetCanDisableVectors(t *testing.T) {
	for _, tt := range []struct {
		backend storage.Replica
		url     string
	}{{pgReplica{}, "postgres://db.example/archive"}, {clickhouse.Backend{}, "clickhouse://db.example/archive"}} {
		backend := tt.backend
		t.Run(backend.Name(), func(t *testing.T) {
			seedScopedConfigArchive(t)
			data := "[" + backend.Name() + "]\nurl='" + tt.url + "'\npush_vectors=false\n[vector]\nenabled=true\n"
			require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("AGENTSVIEW_DATA_DIR"), "config.toml"), []byte(data), 0o600))
			push := ReplicaPushConfig{}
			cfg, err := config.LoadMinimalFor(replicaConfigScope(backend, &push))
			require.NoError(t, err)
			refs, err := storage.SelectTargets(backend, cfg, "", false)
			require.NoError(t, err)
			target, err := backend.ResolveTarget(cfg, refs[0])
			require.NoError(t, err)
			assert.Nil(t, replicaVectorPushSource(cfg, target, push))
			require.NoError(t, validateReplicaPushVectors(cfg, push, target.Target.PushVectors))
			require.ErrorContains(t, validateReplicaPushVectors(cfg, push, true), "model is required")
			push.NoVectors = true
			require.NoError(t, validateReplicaPushVectors(cfg, push, true))
		})
	}
}

func TestScopedConfigPGDeploymentConsumers(t *testing.T) {
	for _, tt := range []struct {
		name, insecure, want string
		args                 []string
	}{
		{"vectors", "invalid", "AGENTSVIEW_PG_ALLOW_INSECURE must be a boolean", []string{"pg", "vectors", "list"}},
		{"raw reparse", "true", "hosted raw projection schema is invalid", []string{"pg", "raw-reparse", "--run-id", "scoped"}},
		{"PG read", "true", "hosted raw projection schema is invalid", []string{"session", "list", "--pg"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			seedScopedConfigArchive(t)
			dir := os.Getenv("AGENTSVIEW_DATA_DIR")
			data := "cursor_secret = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='\n[vector]\nenabled=false\n[pg.primary]\nurl='postgres://db.example/archive?sslmode=disable'\nschema='bad-name'\nraw_tenant='tenant'\n"
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(data), 0o600))
			env := []string{
				"AGENTSVIEW_MODE=serve", "AGENTSVIEW_REQUIRE_AUTH=true", "AGENTSVIEW_PG_ALLOW_INSECURE=" + tt.insecure,
				"AGENTSVIEW_AUTH_TOKEN_FILE=" + filepath.Join(dir, "missing-token"), "AGENTSVIEW_EMBEDDINGS_ENDPOINT=http://127.0.0.1:30000/v1",
			}
			_, stderr, err := runScopedConfigCLI(t, env, tt.args...)
			require.Error(t, err)
			assert.Contains(t, stderr, tt.want, "PG settings must apply before connecting, without unrelated deployment settings")
		})
	}
}

// Every case bypasses ensureTransportContext. Dropping narrow deployment auth
// from one of these paths must prevent it from reaching the authenticated API.
func TestScopedConfigDirectDaemonClientsUseDeploymentAuth(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"label", []string{"session", "label", "session-id", "role=reviewer"}, "authenticated API reached"},
		{"parent", []string{"session", "parent", "session-id", "parent-id"}, "authenticated API reached"},
		{"embeddings list", []string{"embeddings", "list"}, "authenticated API reached"},
		{"embeddings build", []string{"embeddings", "build"}, "authenticated API reached"},
		{"embeddings activate", []string{"embeddings", "activate", "1"}, "authenticated API reached"},
		{"embeddings retire", []string{"embeddings", "retire", "1"}, "authenticated API reached"},
		{"compact", []string{"db", "compact", "--yes"}, "authenticated API reached"},
		{"classifier", []string{"classifier", "rebuild"}, "local daemon is serving on"},
		{"chrome", nil, "authenticated API reached"},
		{"conversation initialization", nil, ""},
		{"recall extraction", nil, "a local daemon is running and owns the archive"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			seedScopedConfigArchive(t)
			dir := os.Getenv("AGENTSVIEW_DATA_DIR")
			writeEmbeddingsTestConfig(t, dir, "http://127.0.0.1:1")
			if tt.name == "parent" {
				path := filepath.Join(dir, "config.toml")
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, append([]byte("auth_token = 'stale-token'\n"), data...), 0o600))
			}
			tokenFile := filepath.Join(dir, "deployment-token")
			require.NoError(t, os.WriteFile(tokenFile, []byte("deployment-secret\n"), 0o600))
			t.Setenv("AGENTSVIEW_MODE", "serve")
			t.Setenv("AGENTSVIEW_AUTH_TOKEN_FILE", tokenFile)
			// These settings are irrelevant to daemon clients, including a
			// deployment embeddings transport conflicting with [vector].
			t.Setenv("AGENTSVIEW_HOST", "")
			t.Setenv("AGENTSVIEW_EMBEDDINGS_ENDPOINT", "http://127.0.0.1:1/v1")
			ping := daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: "test"})
			var initialized atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer deployment-secret" {
					http.Error(w, "wrong token", http.StatusUnauthorized)
					return
				}
				if r.URL.Path == "/api/v1/export/conversations/initialize" {
					initialized.Store(true)
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{}`)
					return
				}
				if r.URL.Path == "/api/ping" {
					ping.ServeHTTP(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"error":"authenticated API reached"}`)
			}))
			t.Cleanup(server.Close)
			endpoint := serverEndpoint(t, server)
			writeDaemonRuntimeForTest(t, dir, endpoint.Host, endpoint.Port, "test", false)
			var err error
			switch tt.name {
			case "chrome":
				err = syncClaudeAIChrome(t.Context())
			case "conversation initialization":
				cmd := parseCommand(t, []string{"export", "conversations", "changes"})
				cmd.SetContext(t.Context())
				err = initializeConversationExport(cmd)
			case "recall extraction":
				cfg, loadErr := config.LoadMinimalFor(config.ScopeRecall)
				require.NoError(t, loadErr)
				var database *db.DB
				var lock *writeOwnerLock
				database, lock, err = openWritableExtractDB(t.Context(), cfg)
				if database != nil {
					closeWriteDB(database, lock)
				}
			default:
				_, err = executeCommand(newRootCommand(), tt.args...)
			}
			if tt.name == "conversation initialization" {
				require.NoError(t, err)
				assert.True(t, initialized.Load(), "cold export must use the authenticated daemon's initialization endpoint")
			} else {
				require.ErrorContains(t, err, tt.want)
			}
		})
	}
}

func TestScopedConfigReplacementRejectsInvalidServerConfigBeforeStop(t *testing.T) {
	for _, tt := range []struct {
		name, data, want string
		env              map[string]string
	}{
		{"insights", "[insights]\nmodel='incomplete'\n", "endpoint and model", nil},
		{"vector", "[vector]\nenabled=true\n", "model is required", nil},
		{"palette", "chart_palette='invalid'\n", "chart_palette", nil},
		{"deployment embeddings", "[vector]\nenabled=false\n", "AGENTSVIEW_EMBEDDINGS_*", map[string]string{
			"AGENTSVIEW_MODE": "serve", "AGENTSVIEW_EMBEDDINGS_ENDPOINT": "http://127.0.0.1:30000/v1",
		}},
		{"deployment mode", "", "AGENTSVIEW_MODE must", map[string]string{"AGENTSVIEW_MODE": "invalid"}},
		{"deployment bind", "", "exposes the API beyond", map[string]string{
			"AGENTSVIEW_MODE": "serve", "AGENTSVIEW_HOST": "0.0.0.0", "AGENTSVIEW_REQUIRE_AUTH": "false",
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			seedScopedConfigArchive(t)
			dir := os.Getenv("AGENTSVIEW_DATA_DIR")
			writeTestConfig(t, dir, tt.data)
			for name, value := range tt.env {
				t.Setenv(name, value)
			}
			cfg, err := config.LoadMinimalFor(config.ScopeLocal)
			require.NoError(t, err, "the one-shot client does not use the invalid section")
			// The re-exec child uses cfg.DataDir, even if its parent environment
			// selects a different archive. Validate that same file before stop.
			t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
			setTestVersion(t, "1.1.0")
			host, port := testPingServer(t)
			writeDaemonRuntimeForTest(t, dir, host, port, "1.0.0", false)
			stops, starts := 0, 0
			stubStopDaemonRuntimeForUpgrade(t, func(context.Context, config.Config, *DaemonRuntime) error {
				stops++
				return errors.New("stop reached before strict validation")
			})
			oldStart := startServeBackgroundProcessForEnsure
			startServeBackgroundProcessForEnsure = func(context.Context, config.Config, []string) (*exec.Cmd, string, error) {
				starts++
				return nil, "", errors.New("unexpected child start")
			}
			t.Cleanup(func() { startServeBackgroundProcessForEnsure = oldStart })
			_, err = ensureBackgroundServe(t.Context(), &cfg, time.Second, true)
			require.ErrorContains(t, err, tt.want)
			assert.Zero(t, stops, "invalid replacement config must preserve the incumbent")
			assert.Zero(t, starts)
			runtime := FindDaemonRuntime(dir, cfg.AuthToken)
			require.NotNil(t, runtime, "the incumbent must remain reachable")
			assert.Equal(t, "1.0.0", runtime.Record.Version)
		})
	}
}

func TestScopedConfigDeploymentHostControlsDaemonLaunch(t *testing.T) {
	for _, tt := range []struct {
		name, daemonVersion, mode, host string
		launch                          bool
	}{
		{"start", "", "serve", "127.0.0.1", true},
		{"replace", "1.0.0", "serve", "127.0.0.1", true},
		{"reuse", "1.1.0", "invalid", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			seedScopedConfigArchive(t)
			dir := os.Getenv("AGENTSVIEW_DATA_DIR")
			writeTestConfig(t, dir, "host='0.0.0.0'\n")
			t.Setenv("AGENTSVIEW_MODE", tt.mode)
			t.Setenv("AGENTSVIEW_HOST", tt.host)
			setTestVersion(t, "1.1.0")
			if tt.daemonVersion != "" {
				host, port := testPingServer(t)
				writeDaemonRuntimeForTest(t, dir, host, port, tt.daemonVersion, false)
			}
			cfg, err := config.LoadMinimalFor(config.ScopeLocal)
			require.NoError(t, err)
			stubStartBackgroundServeForTransport(t, ensureBackgroundServe)
			stops, starts := 0, 0
			stubStopDaemonRuntimeForUpgrade(t, func(context.Context, config.Config, *DaemonRuntime) error {
				stops++
				RemoveDaemonRuntime(dir)
				return nil
			})
			launchReached := errors.New("child launch reached")
			oldStart := startServeBackgroundProcessForEnsure
			startServeBackgroundProcessForEnsure = func(context.Context, config.Config, []string) (*exec.Cmd, string, error) {
				starts++
				return nil, "", launchReached
			}
			t.Cleanup(func() { startServeBackgroundProcessForEnsure = oldStart })
			tr, err := ensureTransportContext(t.Context(), &cfg, transportIntentRead, time.Second)
			if tt.launch {
				require.ErrorIs(t, err, launchReached, "safe deployment host must pass both startup and replacement guards")
				assert.Equal(t, 1, starts)
				if tt.daemonVersion == "" {
					assert.Zero(t, stops)
				} else {
					assert.Equal(t, 1, stops)
				}
			} else {
				require.NoError(t, err, "current-daemon discovery must ignore unused deployment settings")
				assert.Equal(t, transportHTTP, tr.Mode)
				assert.Zero(t, starts)
				assert.Zero(t, stops)
			}
		})
	}
}
