package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/server"
	agentsync "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestNewSyncCommandHandsTargetToRunner(t *testing.T) {
	var got SyncConfig
	cmd := newSyncCommandWithRunner(func(cfg SyncConfig) {
		got = cfg
	})
	cmd.SetArgs([]string{"--target", "shared-artifacts", "--full"})

	require.NoError(t, cmd.Execute())
	assert.Equal(t, "shared-artifacts", got.Target)
	assert.True(t, got.Full)
}

func TestNewSyncCommandRejectsTargetWithAdHocHost(t *testing.T) {
	called := false
	cmd := newSyncCommandWithRunner(func(SyncConfig) {
		called = true
	})
	cmd.SetArgs([]string{
		"--target", "shared-artifacts",
		"--host", "peer.example.test",
	})

	err := cmd.Execute()
	require.EqualError(t, err, "--target cannot be combined with --host")
	assert.False(t, called)
}

func TestRunArtifactFolderSyncPassesOnlyDistinctProtectedRoots(t *testing.T) {
	dataDir := t.TempDir()
	providerA := filepath.Join(t.TempDir(), "provider-a")
	providerB := filepath.Join(t.TempDir(), "provider-b")
	target := filepath.Join(t.TempDir(), "target")
	appCfg := config.Config{
		DataDir: dataDir,
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {
				providerA,
				filepath.Join(providerA, "."),
				"s3://archive-bucket/claude",
			},
			parser.AgentCodex:  {providerB, "s3://archive-bucket/codex"},
			parser.AgentGemini: {dataDir},
		},
	}
	database, err := db.Open(filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })

	original := runArtifactSyncCLI
	var got artifact.SyncOptions
	runArtifactSyncCLI = func(
		_ context.Context,
		_ *db.DB,
		opts artifact.SyncOptions,
	) (artifact.SyncResult, error) {
		got = opts
		return artifact.SyncResult{Origin: "node-a1b2c3"}, nil
	}
	t.Cleanup(func() { runArtifactSyncCLI = original })

	result, err := runArtifactFolderSync(
		t.Context(),
		appCfg,
		database,
		SyncConfig{Target: target, Full: true},
	)
	require.NoError(t, err)
	assert.Equal(t, "node-a1b2c3", result.Origin)
	assert.Equal(t, dataDir, got.DataDir)
	assert.Equal(t, target, got.Target)
	assert.Empty(t, got.Origin)
	assert.True(t, got.Full)
	assert.ElementsMatch(
		t,
		[]string{dataDir, providerA, providerB},
		got.ForbiddenRoots,
	)
	assert.Len(t, got.ForbiddenRoots, 3)
}

func TestRunArtifactFolderSyncRedactsTargetFromErrors(t *testing.T) {
	target := filepath.Join(t.TempDir(), "private-target")
	cause := fmt.Errorf("opening %s: permission denied", target)
	original := runArtifactSyncCLI
	runArtifactSyncCLI = func(
		context.Context,
		*db.DB,
		artifact.SyncOptions,
	) (artifact.SyncResult, error) {
		return artifact.SyncResult{}, cause
	}
	t.Cleanup(func() { runArtifactSyncCLI = original })

	_, err := runArtifactFolderSync(
		t.Context(),
		config.Config{DataDir: t.TempDir()},
		nil,
		SyncConfig{Target: target},
	)
	require.Error(t, err)
	assert.Equal(t, "artifact folder sync failed", err.Error())
	assert.NotContains(t, err.Error(), target)
	assert.ErrorIs(t, err, cause)
}

func TestPrintArtifactSyncSummaryReportsBoundedRerun(t *testing.T) {
	var output bytes.Buffer

	printArtifactSyncSummary(&output, artifact.SyncResult{
		ExportedSessions:   2,
		RejectedSessions:   1,
		ImportedSessions:   3,
		ImportedMessages:   7,
		Quarantined:        1,
		ReceivedArtifacts:  8,
		PublishedArtifacts: 9,
		More:               true,
	})

	assert.Equal(t,
		"Artifacts: exported 2 sessions; imported 3 sessions and 7 messages; "+
			"received 8 objects; published 9 objects; rejected 1 session; "+
			"quarantined 1 object\n"+
			"Artifact work remains; run the sync command again.\n",
		output.String(),
	)
}

func TestDoSyncWithoutTargetDoesNotCreateArtifactRepository(t *testing.T) {
	env := newSyncCLIEnv(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	isolateDirectCLISources(t)

	hadFailures := doSync(SyncConfig{})

	assert.False(t, hadFailures)
	assert.NoDirExists(t, filepath.Join(env.DataDir, "artifacts"))
}

func TestDoSyncRunsArtifactExchangeAfterLocalSync(t *testing.T) {
	env := newSyncCLIEnv(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	isolateDirectCLISources(t)
	sourceRoot := filepath.Join(t.TempDir(), "claude")
	require.NoError(t, os.MkdirAll(filepath.Join(sourceRoot, "project"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(sourceRoot, "project", "session.jsonl"),
		[]byte(testjsonl.NewSessionBuilder().
			AddClaudeUser("2026-07-12T00:00:00Z", "local before artifact").
			String()),
		0o600,
	))
	t.Setenv("CLAUDE_PROJECTS_DIR", sourceRoot)
	target := filepath.Join(t.TempDir(), "target")

	original := runArtifactSyncCLI
	called := false
	runArtifactSyncCLI = func(
		ctx context.Context,
		database *db.DB,
		opts artifact.SyncOptions,
	) (artifact.SyncResult, error) {
		called = true
		stats, err := database.GetStats(ctx, false, false)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.SessionCount)
		assert.Equal(t, target, opts.Target)
		return artifact.SyncResult{ExportedSessions: 1}, nil
	}
	t.Cleanup(func() { runArtifactSyncCLI = original })

	var hadFailures bool
	output := captureStdout(t, func() {
		hadFailures = doSync(SyncConfig{Target: target})
	})

	assert.False(t, hadFailures)
	assert.True(t, called)
	assert.Contains(t, output, "Artifacts: exported 1 session")
	assert.FileExists(t, env.DBPath)
}

func TestDoSyncRunsArtifactExchangeAfterConfiguredRemoteFanout(t *testing.T) {
	env := newSyncCLIEnv(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	isolateDirectCLISources(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(env.DataDir, "config.toml"),
		[]byte("[[remote_hosts]]\nhost = \"peer\"\n"),
		0o600,
	))
	var order []string
	originalRemotes := runConfiguredLocalAndRemotesCLI
	runConfiguredLocalAndRemotesCLI = func(
		context.Context,
		config.Config,
		*db.DB,
		[]config.RemoteHost,
		bool,
		agentsync.ProgressFunc,
	) (bool, []remoteHostFailure, error) {
		order = append(order, "remotes")
		return false, nil, nil
	}
	t.Cleanup(func() { runConfiguredLocalAndRemotesCLI = originalRemotes })
	originalArtifact := runArtifactSyncCLI
	runArtifactSyncCLI = func(
		context.Context,
		*db.DB,
		artifact.SyncOptions,
	) (artifact.SyncResult, error) {
		order = append(order, "artifact")
		return artifact.SyncResult{}, nil
	}
	t.Cleanup(func() { runArtifactSyncCLI = originalArtifact })

	hadFailures := doSync(SyncConfig{
		Target: filepath.Join(t.TempDir(), "target"),
	})

	assert.False(t, hadFailures)
	assert.Equal(t, []string{"remotes", "artifact"}, order)
}

func TestRunDaemonArtifactExchangeUsesAuthenticatedLoopbackEndpoint(
	t *testing.T,
) {
	target := filepath.Join(t.TempDir(), "archive")
	var got server.ArtifactExchangeRequest
	ts := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/api/v1/artifacts/exchange", r.URL.Path)
		require.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		require.Equal(t, tsURL(t, r), r.Header.Get("Origin"))
		require.NoError(t, json.UnmarshalRead(r.Body, &got))
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.MarshalWrite(w, artifact.SyncResult{
			Origin:             "node-a1b2c3",
			PublishedArtifacts: 3,
		}))
	}))
	t.Cleanup(ts.Close)

	result, err := runDaemonArtifactExchange(
		t.Context(),
		transport{Mode: transportHTTP, URL: ts.URL + "/"},
		"test-token",
		target,
		true,
	)

	require.NoError(t, err)
	assert.Equal(t, server.ArtifactExchangeRequest{
		Target: target,
		Full:   true,
	}, got)
	assert.Equal(t, "node-a1b2c3", result.Origin)
	assert.Equal(t, 3, result.PublishedArtifacts)
}

func TestDaemonArtifactExchangeNotifiesClientsAndSchedulersAfterImport(
	t *testing.T,
) {
	database, err := db.Open(filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })

	broadcaster := server.NewBroadcaster(0)
	events, unsubscribe := broadcaster.Subscribe()
	t.Cleanup(unsubscribe)

	embedManager := &fakeEmbedManager{}
	embedScheduler := newEmbedScheduler(
		embedManager,
		time.Millisecond,
		0,
		false,
		nil,
	)
	go embedScheduler.Run(t.Context())
	t.Cleanup(embedScheduler.Stop)

	recallNotified := make(chan struct{}, 1)
	emitter := wrapEmbeddingSyncEmitter(
		broadcaster,
		vectorServing{
			Scheduler: embedScheduler,
			RecallMutationNotify: func() {
				recallNotified <- struct{}{}
			},
		},
		true,
	)
	engine := agentsync.NewEngine(
		database,
		agentsync.EngineConfig{Emitter: emitter},
	)
	t.Cleanup(engine.Close)

	original := runArtifactSyncCLI
	runArtifactSyncCLI = func(
		context.Context,
		*db.DB,
		artifact.SyncOptions,
	) (artifact.SyncResult, error) {
		return artifact.SyncResult{
			ImportedSessions: 1,
			ImportedMessages: 2,
		}, nil
	}
	t.Cleanup(func() { runArtifactSyncCLI = original })

	runner := newDaemonArtifactExchangeRunner(
		config.Config{DataDir: t.TempDir()},
		database,
		engine,
		emitter,
	)
	result, err := runner(t.Context(), server.ArtifactExchangeRequest{
		Target: t.TempDir(),
	})

	require.NoError(t, err)
	assert.Equal(t, 1, result.ImportedSessions)
	select {
	case event := <-events:
		assert.Equal(t, "sessions", event.Scope)
	case <-time.After(time.Second):
		require.Fail(t, "artifact import did not notify SSE subscribers")
	}
	waitForSchedulerCondition(
		t,
		func() bool { return embedManager.callCount() == 1 },
		"artifact import did not notify the embedding scheduler",
	)
	select {
	case <-recallNotified:
	case <-time.After(time.Second):
		require.Fail(t, "artifact import did not notify the recall scheduler")
	}
}

func TestRunDaemonArtifactExchangeMakesRelativeTargetAbsolute(t *testing.T) {
	var got server.ArtifactExchangeRequest
	ts := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		require.NoError(t, json.UnmarshalRead(r.Body, &got))
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `{}`)
		require.NoError(t, err)
	}))
	t.Cleanup(ts.Close)

	relativeTarget := filepath.Join("relative", "artifact-target")
	wantTarget, err := filepath.Abs(relativeTarget)
	require.NoError(t, err)
	_, err = runDaemonArtifactExchange(
		t.Context(),
		transport{Mode: transportHTTP, URL: ts.URL},
		"",
		relativeTarget,
		false,
	)

	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(got.Target))
	assert.Equal(t, wantTarget, got.Target)
}

func TestRunDaemonArtifactExchangeAcceptsLocalhostEndpoint(t *testing.T) {
	var gotHost string
	ts := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		gotHost = r.Host
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `{}`)
		require.NoError(t, err)
	}))
	t.Cleanup(ts.Close)
	endpoint, err := url.Parse(ts.URL)
	require.NoError(t, err)
	endpoint.Host = "localhost:" + endpoint.Port()

	_, err = runDaemonArtifactExchange(
		t.Context(),
		transport{Mode: transportHTTP, URL: endpoint.String()},
		"",
		t.TempDir(),
		false,
	)

	require.NoError(t, err)
	assert.Equal(t, "localhost:"+endpoint.Port(), gotHost)
}

func TestRunDaemonArtifactExchangeConnectsToIPv6LocalhostListener(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback is unavailable: %v", err)
	}
	var gotHost string
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		gotHost = r.Host
		w.Header().Set("Content-Type", "application/json")
		_, writeErr := io.WriteString(w, `{}`)
		require.NoError(t, writeErr)
	}))
	ts.Listener = listener
	ts.Start()
	t.Cleanup(ts.Close)
	endpoint, err := url.Parse(ts.URL)
	require.NoError(t, err)
	endpoint.Host = "localhost:" + endpoint.Port()

	_, err = runDaemonArtifactExchange(
		t.Context(),
		transport{Mode: transportHTTP, URL: endpoint.String()},
		"",
		t.TempDir(),
		false,
	)

	require.NoError(t, err)
	assert.Equal(t, "localhost:"+endpoint.Port(), gotHost)
}

func TestRunLocalAndArtifactFolderSyncStopsAfterLocalFailure(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	localFailure := errors.New("provider discovery failed")
	originalCoordinator := coordinateLocalSyncRunner
	coordinateLocalSyncRunner = func(
		context.Context,
		config.Config,
		*db.DB,
		bool,
		agentsync.ProgressFunc,
		bool,
		func() (agentsync.RebuildOptions, agentsync.RebuildCleanup, error),
		func(bool, bool) error,
	) (bool, agentsync.SyncStats, error) {
		return false, agentsync.SyncStats{}, localFailure
	}
	t.Cleanup(func() { coordinateLocalSyncRunner = originalCoordinator })
	originalArtifact := runArtifactSyncCLI
	artifactCalled := false
	runArtifactSyncCLI = func(
		context.Context,
		*db.DB,
		artifact.SyncOptions,
	) (artifact.SyncResult, error) {
		artifactCalled = true
		return artifact.SyncResult{}, nil
	}
	t.Cleanup(func() { runArtifactSyncCLI = originalArtifact })

	_, err = runLocalAndArtifactFolderSync(
		t.Context(),
		config.Config{
			DataDir: t.TempDir(),
			DBPath:  database.Path(),
		},
		database,
		SyncConfig{Target: t.TempDir()},
	)

	require.ErrorIs(t, err, localFailure)
	assert.False(t, artifactCalled)
}

func TestRunDaemonArtifactExchangeRejectsUnsafeDaemonEndpoints(
	t *testing.T,
) {
	var redirected bool
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		redirected = true
	}))
	t.Cleanup(redirectTarget.Close)
	redirectSource := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
	}))
	t.Cleanup(redirectSource.Close)

	credentialURL, err := url.Parse(redirectSource.URL)
	require.NoError(t, err)
	credentialURL.User = url.UserPassword("private-user", "private-password")
	tests := []struct {
		name string
		url  string
	}{
		{name: "redirect", url: redirectSource.URL},
		{name: "credentials", url: credentialURL.String()},
		{name: "non-loopback", url: "http://192.0.2.1:8080"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			privateTarget := "/private/company/archive"

			_, err := runDaemonArtifactExchange(
				t.Context(),
				transport{Mode: transportHTTP, URL: tt.url},
				"test-token",
				privateTarget,
				false,
			)

			require.Error(t, err)
			assert.Equal(t, "daemon artifact exchange failed", err.Error())
			assert.NotContains(t, err.Error(), privateTarget)
			assert.NotContains(t, err.Error(), "private-password")
		})
	}
	assert.False(t, redirected)
}

func TestRunDaemonArtifactExchangeRedactsDaemonErrors(t *testing.T) {
	privateTarget := "/private/company/archive"
	ts := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		http.Error(
			w,
			"opening "+privateTarget+": permission denied",
			http.StatusBadGateway,
		)
	}))
	t.Cleanup(ts.Close)

	_, err := runDaemonArtifactExchange(
		t.Context(),
		transport{Mode: transportHTTP, URL: ts.URL},
		"",
		privateTarget,
		false,
	)

	require.Error(t, err)
	assert.Equal(t, "daemon artifact exchange failed", err.Error())
	assert.NotContains(t, err.Error(), privateTarget)
	assert.NotContains(t, err.Error(), "permission denied")
}

func TestDoSyncDelegatesArtifactExchangeAfterDaemonSync(t *testing.T) {
	env := newSyncCLIEnv(t)
	target := filepath.Join(t.TempDir(), "private-target")
	var order []string
	ts := daemonRouteTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/sync": func(w http.ResponseWriter, _ *http.Request) {
			order = append(order, "sync")
			writeDoneSSE(t, w, agentsync.SyncStats{Synced: 1})
		},
		"/api/v1/artifacts/exchange": func(
			w http.ResponseWriter,
			r *http.Request,
		) {
			order = append(order, "artifact")
			var request server.ArtifactExchangeRequest
			require.NoError(t, json.UnmarshalRead(r.Body, &request))
			assert.Equal(t, target, request.Target)
			w.Header().Set("Content-Type", "application/json")
			_, err := io.WriteString(
				w,
				`{"origin":"node-a1b2c3","exported_sessions":1}`,
			)
			require.NoError(t, err)
		},
	})
	registerSyncRouteTestRuntime(t, env.DataDir, ts.URL)

	var hadFailures bool
	output := captureStdout(t, func() {
		hadFailures = doSync(SyncConfig{Target: target})
	})

	assert.False(t, hadFailures)
	assert.Equal(t, []string{"sync", "artifact"}, order)
	assert.Contains(t, output, "Artifacts: exported 1 session")
	env.assertNoLocalDB(t)
}
