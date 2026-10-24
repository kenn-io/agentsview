package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/skills"
	"go.kenn.io/agentsview/internal/storage"
)

func TestMemorySessionStartUsesPluginEnvironment(t *testing.T) {
	original := runMemorySessionStart
	t.Cleanup(func() { runMemorySessionStart = original })
	t.Setenv("AGENTSVIEW_MEMORY_MODE", "hosted-contributor")
	t.Setenv("AGENTSVIEW_MEMORY_TARGET", "team")

	var got memorySessionStartRequest
	runMemorySessionStart = func(
		_ context.Context, req memorySessionStartRequest,
	) error {
		got = req
		return nil
	}

	_, err := executeCommand(newRootCommand(), "memory", "session-start")
	require.NoError(t, err)
	assert.Equal(t, memorySessionStartRequest{
		Mode: memoryModeHostedContributor, Target: "team",
	}, got)
}

func TestMemoryContributorIgnoresMCPReadTargetEnvironment(t *testing.T) {
	original := runMemorySessionStart
	t.Cleanup(func() { runMemorySessionStart = original })
	t.Setenv("AGENTSVIEW_MEMORY_MODE", "hosted-contributor")
	t.Setenv("AGENTSVIEW_MEMORY_TARGET", "team")
	t.Setenv("AGENTSVIEW_MEMORY_PG", "true")

	var got memorySessionStartRequest
	runMemorySessionStart = func(
		_ context.Context, req memorySessionStartRequest,
	) error {
		got = req
		return nil
	}

	_, err := executeCommand(newRootCommand(), "memory", "session-start")
	require.NoError(t, err)
	assert.Equal(t, memorySessionStartRequest{
		Mode: memoryModeHostedContributor, Target: "team",
	}, got)
}

func TestMemorySessionStartExplicitFlagsOverridePluginEnvironment(t *testing.T) {
	original := runMemorySessionStart
	t.Cleanup(func() { runMemorySessionStart = original })
	t.Setenv("AGENTSVIEW_MEMORY_MODE", "hosted-reader")
	t.Setenv("AGENTSVIEW_MEMORY_SERVER", "https://wrong.example")

	var got memorySessionStartRequest
	runMemorySessionStart = func(
		_ context.Context, req memorySessionStartRequest,
	) error {
		got = req
		return nil
	}

	_, err := executeCommand(newRootCommand(), "memory", "session-start",
		"--mode", "local")
	require.NoError(t, err)
	assert.Equal(t, memorySessionStartRequest{Mode: memoryModeLocal}, got)
}

func TestMemorySessionStartHookReportsFailureWithoutBlocking(t *testing.T) {
	original := runMemorySessionStart
	t.Cleanup(func() { runMemorySessionStart = original })
	runMemorySessionStart = func(
		context.Context, memorySessionStartRequest,
	) error {
		return errors.New("remote unavailable")
	}

	cmd := newRootCommand()
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"memory", "session-start", "--hook"})
	require.NoError(t, cmd.Execute())
	assert.Contains(t, stderr.String(), "remote unavailable")
}

func TestMemoryPluginConflictDiagnosticPreservesStandaloneSkill(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	pkg, err := skills.RenderPackage(skills.HarnessClaude, "dev", skills.Remote{})
	require.NoError(t, err)
	path := filepath.Join(home, pkg[0].RelativePath)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(pkg[0].Content), 0o644))

	pluginRoot := t.TempDir()
	pluginSkill := filepath.Join(pluginRoot, "skills",
		"agentsview-finding-history", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(pluginSkill), 0o755))
	require.NoError(t, os.WriteFile(pluginSkill, []byte(pkg[0].Content), 0o644))

	var diagnostics bytes.Buffer
	reportMemoryPluginConflicts(&diagnostics, pluginRoot)
	assert.Contains(t, diagnostics.String(), "duplicates managed standalone skill")
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, pkg[0].Content, string(body))

	modified := pkg[0].Content + "\nlocal edit\n"
	require.NoError(t, os.WriteFile(path, []byte(modified), 0o644))
	diagnostics.Reset()
	reportMemoryPluginConflicts(&diagnostics, pluginRoot)
	assert.Contains(t, diagnostics.String(), "conflicts with user-managed skill")
	body, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, modified, string(body))
}

func TestMemorySessionStartUsesBoundedContext(t *testing.T) {
	original := runMemorySessionStart
	t.Cleanup(func() { runMemorySessionStart = original })

	var called bool
	runMemorySessionStart = func(
		ctx context.Context, req memorySessionStartRequest,
	) error {
		called = true
		assert.Equal(t, memoryModeLocal, req.Mode)
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		remaining := time.Until(deadline)
		assert.Positive(t, remaining)
		assert.LessOrEqual(t, remaining, memorySessionStartTimeout)
		return nil
	}

	_, err := executeCommand(newRootCommand(), "memory", "session-start")
	require.NoError(t, err)
	assert.True(t, called)
}

func TestMemorySessionStartDisableSwitchSkipsRequest(t *testing.T) {
	original := runMemorySessionStart
	t.Cleanup(func() { runMemorySessionStart = original })
	t.Setenv("AGENTSVIEW_DISABLE_AUTO_SYNC", "1")

	runMemorySessionStart = func(
		context.Context, memorySessionStartRequest,
	) error {
		require.Fail(t, "disabled automatic sync must not request a refresh")
		return nil
	}

	_, err := executeCommand(newRootCommand(), "memory", "session-start")
	require.NoError(t, err)
}

func TestMemorySessionStartDispatchesHostedModes(t *testing.T) {
	original := runMemorySessionStart
	t.Cleanup(func() { runMemorySessionStart = original })
	for _, tc := range []struct {
		name            string
		args            []string
		memoryEnv       bool
		remoteTokenFile bool
		want            memorySessionStartRequest
		wantClient      bool
		wantError       string
	}{
		{name: "explicit contributor", args: []string{"--mode", "hosted-contributor", "--target", "team"}, want: memorySessionStartRequest{Mode: memoryModeHostedContributor, Target: "team"}},
		{name: "explicit reader", args: []string{"--mode", "hosted-reader", "--server", "https://memory.example"}, want: memorySessionStartRequest{Mode: memoryModeHostedReader, Server: "https://memory.example", ServerToken: "reader-token"}},
		{name: "local bypass", args: []string{"--mode", "local"}, want: memorySessionStartRequest{Mode: memoryModeLocal}},
		{name: "local override", args: []string{"--mode", "hosted-reader", "--local"}, want: memorySessionStartRequest{Mode: memoryModeLocal}},
		{name: "memory environment", memoryEnv: true, want: memorySessionStartRequest{Mode: memoryModeHostedReader, Server: "https://memory.example", ServerToken: "reader-token"}},
		{name: "configured reader", args: []string{"--mode", "hosted-reader"}, remoteTokenFile: true, want: memorySessionStartRequest{Mode: memoryModeHostedReader, Server: "https://hub.example", ServerToken: "remote-only"}, wantClient: true},
		{name: "unavailable configured credential", args: []string{"--mode", "hosted-reader"}, wantError: "remote https://hub.example request failed; try --local"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
			t.Setenv("AGENTSVIEW_SERVER_TOKEN", "reader-token")
			t.Setenv("AGENTSVIEW_REMOTE_URL", "https://hub.example")
			tokenPath := filepath.Join(t.TempDir(), "token")
			if tc.remoteTokenFile {
				require.NoError(t, os.WriteFile(tokenPath, []byte("remote-only"), 0o600))
			}
			t.Setenv("AGENTSVIEW_REMOTE_TOKEN_FILE", tokenPath)
			if tc.memoryEnv {
				t.Setenv("AGENTSVIEW_MEMORY_MODE", "hosted-reader")
				t.Setenv("AGENTSVIEW_MEMORY_SERVER", "https://memory.example")
			}
			var got []memorySessionStartRequest
			runMemorySessionStart = func(_ context.Context, req memorySessionStartRequest) error { got = append(got, req); return nil }
			_, err := executeCommand(newRootCommand(), append([]string{"memory", "session-start"}, tc.args...)...)
			if tc.wantError != "" {
				require.EqualError(t, err, tc.wantError)
				require.Empty(t, got)
				return
			}
			require.NoError(t, err)
			require.Len(t, got, 1)
			if tc.wantClient {
				require.NotNil(t, got[0].HTTPClient)
			} else {
				require.Nil(t, got[0].HTTPClient)
			}
			got[0].HTTPClient = nil
			require.Equal(t, tc.want, got[0])
		})
	}
}

func TestMemorySessionStartRejectsInvalidModeFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"ambiguous reader", []string{"--mode", "hosted-reader", "--server", "https://memory.example", "--pg"}, "exactly one of --server or --pg"},
		{"reader missing target", []string{"--mode", "hosted-reader"}, "exactly one of --server or --pg"},
		{"reader token without server", []string{"--mode", "hosted-reader", "--pg", "--server-token-file", "token"}, "--server-token-file requires --server"},
		{"reader named remote", []string{"--mode", "hosted-reader", "--server", "https://memory.example", "--target", "team"}, "--target requires --pg"},
		{"contributor remote", []string{"--mode", "hosted-contributor", "--server", "https://memory.example"}, "hosted-contributor mode accepts only --target"},
		{"local hosted flag", []string{"--target", "team"}, "local mode does not accept hosted target flags"},
		{"unknown mode", []string{"--mode", "other"}, "unknown --mode"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original := runMemorySessionStart
			t.Cleanup(func() { runMemorySessionStart = original })
			runMemorySessionStart = func(
				context.Context, memorySessionStartRequest,
			) error {
				require.Fail(t, "invalid mode flags must fail before dispatch")
				return nil
			}

			args := append([]string{"memory", "session-start"}, test.args...)
			_, err := executeCommand(newRootCommand(), args...)
			require.Error(t, err)
			assert.ErrorContains(t, err, test.want)
		})
	}
}

func TestRunMemorySessionStartUsesRoleOwner(t *testing.T) {
	originalLocal := requestMemoryLocalRefresh
	originalContributor := requestMemoryContributorRefresh
	originalReader := checkMemoryHostedReader
	t.Cleanup(func() {
		requestMemoryLocalRefresh = originalLocal
		requestMemoryContributorRefresh = originalContributor
		checkMemoryHostedReader = originalReader
	})

	var calls []string
	requestMemoryLocalRefresh = func(context.Context) error {
		calls = append(calls, "local")
		return nil
	}
	requestMemoryContributorRefresh = func(_ context.Context, target string) error {
		calls = append(calls, "contributor:"+target)
		return nil
	}
	checkMemoryHostedReader = func(
		_ context.Context, req memorySessionStartRequest,
	) error {
		calls = append(calls, "reader:"+req.Server)
		return nil
	}

	require.NoError(t, executeMemorySessionStart(t.Context(),
		memorySessionStartRequest{Mode: memoryModeLocal}))
	require.NoError(t, executeMemorySessionStart(t.Context(),
		memorySessionStartRequest{
			Mode: memoryModeHostedContributor, Target: "team",
		}))
	require.NoError(t, executeMemorySessionStart(t.Context(),
		memorySessionStartRequest{
			Mode: memoryModeHostedReader, Server: "https://memory.example",
		}))

	assert.Equal(t, []string{
		"local", "contributor:team", "reader:https://memory.example",
	}, calls)
}

func TestHostedContributorNotifiesConfiguredPGOwner(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`
[pg]
url = "postgres://db.example/archive?sslmode=require"
schema = "agentsview"
machine_name = "laptop"
`), 0o600))
	original := notifyMemoryReplicaWatch
	t.Cleanup(func() { notifyMemoryReplicaWatch = original })

	notifyMemoryReplicaWatch = func(
		_ context.Context, dataDir, backend, target string,
	) error {
		assert.Equal(t, dir, dataDir)
		assert.Equal(t, "pg", backend)
		assert.Empty(t, target)
		return nil
	}

	require.NoError(t, requestHostedContributorRefresh(t.Context(), ""))
}

func TestHostedReaderChecksConfiguredPGWithoutLocalDaemon(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`
[pg]
url = "postgres://db.example/archive?sslmode=require"
schema = "agentsview"
machine_name = "laptop"
`), 0o600))
	original := probeMemoryHostedReaderPostgreSQL
	t.Cleanup(func() { probeMemoryHostedReaderPostgreSQL = original })

	called := false
	probeMemoryHostedReaderPostgreSQL = func(
		_ context.Context, target storage.ReplicaTarget,
	) error {
		called = true
		assert.Equal(t,
			"postgres://db.example/archive?sslmode=require",
			target.URL)
		assert.Equal(t, "agentsview", target.Schema)
		return nil
	}

	require.NoError(t, checkHostedReaderAvailability(t.Context(),
		memorySessionStartRequest{Mode: memoryModeHostedReader, PG: true}))
	assert.True(t, called)
}

func TestHostedReaderChecksExplicitServerWithToken(t *testing.T) {
	original := probeMemoryHostedReaderHTTP
	t.Cleanup(func() { probeMemoryHostedReaderHTTP = original })

	probeMemoryHostedReaderHTTP = func(
		_ context.Context, server, token string,
	) error {
		assert.Equal(t, "https://memory.example/base", server)
		assert.Equal(t, "reader-token", token)
		return nil
	}

	require.NoError(t, checkHostedReaderAvailability(t.Context(),
		memorySessionStartRequest{
			Mode: memoryModeHostedReader, Server: "https://memory.example/base",
			ServerToken: "reader-token",
		}))
}

func TestMemoryRefreshSchedulerCoalescesBurst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		queue := newMemoryRefreshQueue()
		var runs atomic.Int32
		go runMemoryRefreshScheduler(ctx, queue.requests, time.Second, func() {
			runs.Add(1)
		})

		queue.Notify()
		queue.Notify()
		queue.Notify()
		time.Sleep(time.Second)
		synctest.Wait()

		assert.Equal(t, int32(1), runs.Load())
	})
}

func TestPostMemoryRefreshTargetsAuthenticatedQueueRoute(t *testing.T) {
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/memory-base/api/v1/memory/refresh", r.URL.Path)
		assert.Equal(t, serverURL, r.Header.Get("Origin"))
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusAccepted)
	}))
	serverURL = server.URL
	t.Cleanup(server.Close)

	require.NoError(t, postMemoryRefresh(
		t.Context(), server.URL+"/memory-base/", "test-token",
	))
}

func TestMemoryReaderRejectsOrphanCredentialBeforeDefault(t *testing.T) {
	for _, credential := range []string{"available", "missing"} {
		t.Run(credential, func(t *testing.T) {
			t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
			calls := 0
			ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
			defer ts.Close()
			t.Setenv("AGENTSVIEW_REMOTE_URL", ts.URL)
			t.Setenv("AGENTSVIEW_REMOTE_TOKEN_FILE", "")
			if credential == "missing" {
				t.Setenv("AGENTSVIEW_REMOTE_TOKEN_FILE", filepath.Join(t.TempDir(), "missing"))
			}
			original := runMemorySessionStart
			t.Cleanup(func() { runMemorySessionStart = original })
			dispatches := 0
			runMemorySessionStart = func(context.Context, memorySessionStartRequest) error { dispatches++; return nil }
			_, err := executeCommand(newRootCommand(), "memory", "session-start", "--mode", "hosted-reader", "--server-token-file", filepath.Join(t.TempDir(), "explicit-token"))
			require.ErrorContains(t, err, "memory session-start: --server-token-file requires --server")
			require.Zero(t, calls)
			require.Zero(t, dispatches)
		})
	}
}
