package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/remotesync"
	agentsync "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
	"go.kenn.io/kit/daemon"
)

type fakeCLIPreparedHTTPRebuild struct {
	contributors  []agentsync.RebuildContributor
	closed        int
	released      bool
	closeReleased bool
}

type cliLifecycleError struct{ err error }

func (e *cliLifecycleError) Error() string { return "lifecycle: " + e.err.Error() }
func (e *cliLifecycleError) Unwrap() error { return e.err }

func (p *fakeCLIPreparedHTTPRebuild) BorrowRebuildContributors() (
	[]agentsync.RebuildContributor, func(), error,
) {
	return p.contributors, func() { p.released = true }, nil
}

func (p *fakeCLIPreparedHTTPRebuild) Close() error {
	p.closed++
	p.closeReleased = p.released
	return nil
}

func newDirectSyncFixture(t *testing.T) (config.Config, *db.DB) {
	t.Helper()
	dataDir := t.TempDir()
	localRoot := filepath.Join(dataDir, "local-claude")
	require.NoError(t, os.MkdirAll(filepath.Join(localRoot, "project"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(localRoot, "project", "session.jsonl"),
		[]byte(testjsonl.NewSessionBuilder().
			AddClaudeUser("2026-07-12T00:00:00Z", "local direct sync").
			String()),
		0o600,
	))
	cfg := config.Config{
		DataDir:          dataDir,
		DBPath:           filepath.Join(dataDir, "sessions.db"),
		LocalMachineName: "collector-host",
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {localRoot},
		},
	}
	database, err := db.Open(cfg.DBPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	return cfg, database
}

func TestRunRemoteSyncTransportWarnsOnceForDeprecatedSSH(t *testing.T) {
	logs := captureLogOutput(t)
	originalOnce := sshRemoteSyncDeprecationWarningOnce
	sshRemoteSyncDeprecationWarningOnce = new(stdsync.Once)
	t.Cleanup(func() { sshRemoteSyncDeprecationWarningOnce = originalOnce })

	originalSSH := runSSHRemoteSync
	runSSHRemoteSync = func(
		context.Context, config.Config, *db.DB, config.RemoteHost, bool,
	) (remotesync.SyncStats, error) {
		return remotesync.SyncStats{}, nil
	}
	t.Cleanup(func() { runSSHRemoteSync = originalSSH })

	for range 2 {
		_, err := runRemoteSyncTransport(
			context.Background(), config.Config{}, nil,
			config.RemoteHost{Host: "legacy-host"}, false,
		)
		require.NoError(t, err)
	}

	const warning = "SSH remote sync is deprecated"
	assert.Equal(t, 1, strings.Count(logs.String(), warning))
	assert.Contains(t, logs.String(), "use HTTP remote sync instead")
}

func isolateDirectCLISources(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	for _, def := range parser.Registry {
		if def.EnvVar != "" {
			t.Setenv(def.EnvVar, filepath.Join(root, string(def.Type)))
		}
		if def.DefaultRootEnvVar != "" {
			t.Setenv(def.DefaultRootEnvVar, root)
		}
	}
}

func TestDoSyncConfiguredFullUsesUnifiedHTTPContributorBeforeSSH(t *testing.T) {
	cfg, database := newDirectSyncFixture(t)
	httpHost := config.RemoteHost{
		Host: "http-box", Transport: config.RemoteTransportHTTP,
		URL: "http://127.0.0.1:1", Token: "token",
	}
	sshHost := config.RemoteHost{Host: "ssh-box"}
	var order []string
	prepared := &fakeCLIPreparedHTTPRebuild{contributors: []agentsync.RebuildContributor{{
		Name: "http-box",
		AfterSync: func(*agentsync.Engine, *db.DB) error {
			order = append(order, "http contributor")
			return nil
		},
	}}}
	originalPrepare := prepareHTTPRebuildCLI
	prepareHTTPRebuildCLI = func(
		context.Context, []remotesync.HTTPSync,
	) (preparedHTTPRebuildCLI, error) {
		order = append(order, "prepare")
		return prepared, nil
	}
	t.Cleanup(func() { prepareHTTPRebuildCLI = originalPrepare })
	originalSSH := runSSHRemoteSync
	runSSHRemoteSync = func(
		_ context.Context, _ config.Config, _ *db.DB,
		rh config.RemoteHost, full bool,
	) (remotesync.SyncStats, error) {
		order = append(order, "ssh")
		assert.Equal(t, sshHost, rh)
		assert.True(t, full)
		return remotesync.SyncStats{}, nil
	}
	t.Cleanup(func() { runSSHRemoteSync = originalSSH })

	didResync, failures, err := runConfiguredLocalAndRemotes(
		context.Background(), cfg, database,
		[]config.RemoteHost{sshHost, httpHost}, true, nil,
	)

	require.NoError(t, err)
	assert.True(t, didResync)
	assert.Empty(t, failures)
	assert.Equal(t, []string{"prepare", "http contributor", "ssh"}, order)
	assert.Equal(t, 1, prepared.closed)
	assert.True(t, prepared.closeReleased,
		"contributor borrow must release before prepared sources close")
}

func TestDoSyncConfiguredFullIgnoresSSHHistoryDuringUnifiedSafetyCheck(t *testing.T) {
	cfg, database := newDirectSyncFixture(t)
	for _, roots := range cfg.AgentDirs {
		for _, root := range roots {
			require.NoError(t, os.RemoveAll(root))
		}
	}
	missingPath := filepath.Join(t.TempDir(), "missing-ssh-session.jsonl")
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "preserved-ssh-session", Project: "archive", Machine: "ssh-box",
		Agent: "claude", FilePath: &missingPath, MessageCount: 1,
	}))
	httpRoot := t.TempDir()
	prepared := &fakeCLIPreparedHTTPRebuild{contributors: []agentsync.RebuildContributor{{
		Name: "http-box",
		Config: agentsync.EngineConfig{
			AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {httpRoot}},
			Machine:   "http-box", IDPrefix: "http-box~", Ephemeral: true,
		},
	}}}
	originalPrepare := prepareHTTPRebuildCLI
	prepareHTTPRebuildCLI = func(
		context.Context, []remotesync.HTTPSync,
	) (preparedHTTPRebuildCLI, error) {
		return prepared, nil
	}
	t.Cleanup(func() { prepareHTTPRebuildCLI = originalPrepare })
	sshCalls := 0
	originalSSH := runSSHRemoteSync
	runSSHRemoteSync = func(
		_ context.Context, _ config.Config, database *db.DB,
		rh config.RemoteHost, full bool,
	) (remotesync.SyncStats, error) {
		sshCalls++
		assert.Equal(t, "ssh-box", rh.Host)
		assert.True(t, full)
		preserved, err := database.GetSession(
			context.Background(), "preserved-ssh-session",
		)
		require.NoError(t, err)
		assert.NotNil(t, preserved,
			"SSH pass must observe its archived session after the unified swap")
		return remotesync.SyncStats{}, nil
	}
	t.Cleanup(func() { runSSHRemoteSync = originalSSH })

	didResync, failures, err := runConfiguredLocalAndRemotes(
		context.Background(), cfg, database,
		[]config.RemoteHost{
			{Host: "http-box", Transport: config.RemoteTransportHTTP, Token: "token"},
			{Host: "ssh-box"},
		}, true, nil,
	)

	require.NoError(t, err)
	assert.True(t, didResync)
	assert.Empty(t, failures)
	assert.Equal(t, 1, sshCalls,
		"SSH synchronization must run after an empty local/HTTP rebuild")
}

func TestDoSyncConfiguredFullSSHOnlyFallsBackBeforeRemoteImport(t *testing.T) {
	cfg, database := newDirectSyncFixture(t)
	for _, roots := range cfg.AgentDirs {
		for _, root := range roots {
			require.NoError(t, os.RemoveAll(root))
		}
	}
	missingPath := filepath.Join(t.TempDir(), "missing-remote.jsonl")
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "preserved-ssh-session", Project: "archive", Machine: "ssh-box",
		Agent: "claude", FilePath: &missingPath, MessageCount: 1,
	}))
	sshHost := config.RemoteHost{Host: "ssh-box"}
	sshCalls := 0
	originalSSH := runSSHRemoteSync
	runSSHRemoteSync = func(
		_ context.Context, _ config.Config, _ *db.DB,
		rh config.RemoteHost, full bool,
	) (remotesync.SyncStats, error) {
		sshCalls++
		assert.Equal(t, sshHost, rh)
		assert.True(t, full)
		return remotesync.SyncStats{}, nil
	}
	t.Cleanup(func() { runSSHRemoteSync = originalSSH })

	didResync, failures, err := runConfiguredLocalAndRemotes(
		context.Background(), cfg, database,
		[]config.RemoteHost{sshHost}, true, nil,
	)

	require.NoError(t, err)
	assert.True(t, didResync)
	assert.Empty(t, failures)
	assert.Equal(t, 1, sshCalls,
		"SSH import must run after the legacy local fallback")
	preserved, err := database.GetSession(context.Background(), "preserved-ssh-session")
	require.NoError(t, err)
	assert.NotNil(t, preserved)
}

func TestDoSyncAutomaticResyncUsesUnifiedHTTPContributor(t *testing.T) {
	cfg, database := newDirectSyncFixture(t)
	require.NoError(t, database.Close())
	raw, err := sql.Open("sqlite3", cfg.DBPath)
	require.NoError(t, err)
	_, err = raw.Exec("PRAGMA user_version = 0")
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	database, err = db.Open(cfg.DBPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	require.True(t, database.NeedsResync())

	prepared := &fakeCLIPreparedHTTPRebuild{}
	prepareCalls := 0
	originalPrepare := prepareHTTPRebuildCLI
	prepareHTTPRebuildCLI = func(
		context.Context, []remotesync.HTTPSync,
	) (preparedHTTPRebuildCLI, error) {
		prepareCalls++
		return prepared, nil
	}
	t.Cleanup(func() { prepareHTTPRebuildCLI = originalPrepare })

	didResync, failures, err := runConfiguredLocalAndRemotes(
		context.Background(), cfg, database,
		[]config.RemoteHost{{
			Host: "http-box", Transport: config.RemoteTransportHTTP,
			URL: "http://127.0.0.1:1", Token: "token",
		}}, false, nil,
	)

	require.NoError(t, err)
	assert.True(t, didResync)
	assert.Empty(t, failures)
	assert.Equal(t, 1, prepareCalls)
	assert.False(t, database.NeedsResync())
}

func TestDoSyncPreparationFailureMapsRemoteAndSkipsSSH(t *testing.T) {
	cfg, database := newDirectSyncFixture(t)
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "preserved", Project: "archive", Machine: "local", Agent: "codex",
	}))
	prepCause := errors.New("manifest unavailable")
	prepared := &fakeCLIPreparedHTTPRebuild{}
	originalPrepare := prepareHTTPRebuildCLI
	prepareHTTPRebuildCLI = func(
		context.Context, []remotesync.HTTPSync,
	) (preparedHTTPRebuildCLI, error) {
		return prepared, &remotesync.HostError{
			Host: "http-box", Operation: "prepare", Err: prepCause,
		}
	}
	t.Cleanup(func() { prepareHTTPRebuildCLI = originalPrepare })
	sshCalls := 0
	originalSSH := runSSHRemoteSync
	runSSHRemoteSync = func(
		context.Context, config.Config, *db.DB, config.RemoteHost, bool,
	) (remotesync.SyncStats, error) {
		sshCalls++
		return remotesync.SyncStats{}, nil
	}
	t.Cleanup(func() { runSSHRemoteSync = originalSSH })

	didResync, failures, err := runConfiguredLocalAndRemotes(
		context.Background(), cfg, database, []config.RemoteHost{
			{Host: "http-box", Transport: config.RemoteTransportHTTP,
				URL: "http://127.0.0.1:1", Token: "token"},
			{Host: "ssh-box"},
		}, true, nil,
	)

	require.NoError(t, err)
	assert.True(t, didResync)
	require.Len(t, failures, 1)
	assert.Equal(t, "http-box", failures[0].Host.Host)
	assert.ErrorIs(t, failures[0].Err, prepCause)
	assert.Equal(t, 0, sshCalls)
	assert.Equal(t, 1, prepared.closed,
		"partial preparation ownership must be closed on failure")
	preserved, getErr := database.GetSession(context.Background(), "preserved")
	require.NoError(t, getErr)
	assert.NotNil(t, preserved, "failed preparation must not swap")
}

func TestDoSyncContributorFailureMapsRemotePreservesCauseAndSkipsSSH(t *testing.T) {
	cfg, database := newDirectSyncFixture(t)
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "preserved", Project: "archive", Machine: "local", Agent: "codex",
	}))
	cause := errors.New("cache snapshot failed")
	prepared := &fakeCLIPreparedHTTPRebuild{contributors: []agentsync.RebuildContributor{{
		Name:      "http-box",
		AfterSync: func(*agentsync.Engine, *db.DB) error { return cause },
	}}}
	originalPrepare := prepareHTTPRebuildCLI
	prepareHTTPRebuildCLI = func(
		context.Context, []remotesync.HTTPSync,
	) (preparedHTTPRebuildCLI, error) {
		return prepared, nil
	}
	t.Cleanup(func() { prepareHTTPRebuildCLI = originalPrepare })
	sshCalls := 0
	originalSSH := runSSHRemoteSync
	runSSHRemoteSync = func(
		context.Context, config.Config, *db.DB, config.RemoteHost, bool,
	) (remotesync.SyncStats, error) {
		sshCalls++
		return remotesync.SyncStats{}, nil
	}
	t.Cleanup(func() { runSSHRemoteSync = originalSSH })

	didResync, failures, err := runConfiguredLocalAndRemotes(
		context.Background(), cfg, database, []config.RemoteHost{
			{Host: "http-box", Transport: config.RemoteTransportHTTP,
				URL: "http://127.0.0.1:1", Token: "token"},
			{Host: "ssh-box"},
		}, true, nil,
	)

	require.NoError(t, err)
	assert.True(t, didResync)
	require.Len(t, failures, 1)
	assert.Equal(t, "http-box", failures[0].Host.Host)
	assert.ErrorIs(t, failures[0].Err, cause)
	assert.Equal(t, 0, sshCalls)
	preserved, getErr := database.GetSession(context.Background(), "preserved")
	require.NoError(t, getErr)
	assert.NotNil(t, preserved, "failed contributor must not swap")
}

func TestDoSyncUnknownCoordinatorFailureRemainsLocalError(t *testing.T) {
	cfg, database := newDirectSyncFixture(t)
	cause := errors.New("temporary database unavailable")
	originalPrepare := prepareHTTPRebuildCLI
	prepareHTTPRebuildCLI = func(
		context.Context, []remotesync.HTTPSync,
	) (preparedHTTPRebuildCLI, error) {
		return nil, cause
	}
	t.Cleanup(func() { prepareHTTPRebuildCLI = originalPrepare })

	_, failures, err := runConfiguredLocalAndRemotes(
		context.Background(), cfg, database, []config.RemoteHost{{
			Host: "http-box", Transport: config.RemoteTransportHTTP,
			URL: "http://127.0.0.1:1", Token: "token",
		}}, true, nil,
	)

	assert.Empty(t, failures)
	assert.ErrorIs(t, err, cause)
}

func TestConfiguredHTTPCoordinatorFailureUsesPrimaryOperationOnly(t *testing.T) {
	hosts := []config.RemoteHost{
		{Host: "alpha", Transport: config.RemoteTransportHTTP},
		{Host: "cleanup", Transport: config.RemoteTransportHTTP},
	}
	localCause := errors.New("local database failed")
	prepCause := errors.New("alpha manifest failed")
	contributorCause := errors.New("alpha contributor failed")
	cleanupCause := errors.New("cleanup failed")
	cleanupHost := &remotesync.HostError{
		Host: "cleanup", Operation: "cleanup", Err: cleanupCause,
	}
	tests := []struct {
		name     string
		err      error
		wantHost string
		wantIs   error
		wantMap  bool
	}{
		{
			name: "pending cleanup wrapping prior host stays coordinator error",
			err: &remotesync.PendingCleanupError{Err: &remotesync.HostError{
				Host: "alpha", Operation: "prepare", Err: prepCause,
			}},
			wantIs: prepCause,
		},
		{
			name:    "secondary cleanup host cannot steal local failure",
			err:     errors.Join(localCause, cleanupHost),
			wantIs:  localCause,
			wantMap: false,
		},
		{
			name: "primary preparation host wins over cleanup host",
			err: &cliLifecycleError{err: errors.Join(
				&remotesync.HostError{
					Host: "alpha", Operation: "prepare", Err: prepCause,
				},
				cleanupHost,
			)},
			wantHost: "alpha",
			wantIs:   prepCause,
			wantMap:  true,
		},
		{
			name: "primary contributor wins over cleanup host",
			err: errors.Join(&agentsync.RebuildContributorError{
				Contributor: "alpha", Err: contributorCause,
			}, cleanupHost),
			wantHost: "alpha",
			wantIs:   contributorCause,
			wantMap:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failure, ok := configuredHTTPCoordinatorFailure(hosts, tt.err)
			assert.Equal(t, tt.wantMap, ok)
			assert.ErrorIs(t, tt.err, tt.wantIs)
			if tt.wantMap {
				assert.Equal(t, tt.wantHost, failure.Host.Host)
				assert.ErrorIs(t, failure.Err, tt.wantIs)
			}
		})
	}
}

func TestRunConfiguredLocalAndRemotesKeepsPendingCleanupBlocked(t *testing.T) {
	cfg, database := newDirectSyncFixture(t)
	pendingCause := errors.New("prior alpha cleanup")
	pending := &remotesync.PendingCleanupError{Err: &remotesync.HostError{
		Host: "http-box", Operation: "cleanup", Err: pendingCause,
	}}
	originalRun := runLocalSyncWithRebuildCLI
	runLocalSyncWithRebuildCLI = func(
		context.Context, config.Config, *db.DB, bool, agentsync.ProgressFunc,
		func() (agentsync.RebuildOptions, agentsync.RebuildCleanup, error),
		func(bool, bool) error,
	) (bool, error) {
		return true, pending
	}
	t.Cleanup(func() { runLocalSyncWithRebuildCLI = originalRun })

	_, failures, err := runConfiguredLocalAndRemotes(
		context.Background(), cfg, database, []config.RemoteHost{{
			Host: "http-box", Transport: config.RemoteTransportHTTP,
			URL: "http://127.0.0.1:1", Token: "token",
		}}, true, nil,
	)

	assert.Empty(t, failures)
	var gotPending *remotesync.PendingCleanupError
	require.ErrorAs(t, err, &gotPending)
	assert.ErrorIs(t, err, pendingCause)
}

func TestRunLocalSyncWithRebuildReturnsAbortedSentinelWithoutSummary(t *testing.T) {
	dataDir := t.TempDir()
	cfg := config.Config{DataDir: dataDir, DBPath: filepath.Join(dataDir, "sessions.db")}
	database, err := db.Open(cfg.DBPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	missingPath := filepath.Join(dataDir, "missing.jsonl")
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "old-file-session", Agent: "claude", Machine: "local",
		Project: "preserved", FilePath: &missingPath, MessageCount: 1,
	}))
	workCalls := 0
	var runErr error
	out := captureStdout(t, func() {
		_, runErr = runLocalSyncWithRebuild(
			context.Background(), cfg, database, true, nil,
			func() (agentsync.RebuildOptions, agentsync.RebuildCleanup, error) {
				return agentsync.RebuildOptions{Contributors: []agentsync.RebuildContributor{{
					Name: "http-box",
				}}}, nil, nil
			},
			func(bool, bool) error {
				workCalls++
				return nil
			},
		)
	})

	assert.ErrorIs(t, runErr, errUnifiedRebuildAborted)
	assert.Equal(t, 0, workCalls)
	assert.NotContains(t, out, "Sync complete")
	preserved, getErr := database.GetSession(context.Background(), "old-file-session")
	require.NoError(t, getErr)
	assert.NotNil(t, preserved)
}

func TestRunLocalSyncWithRebuildCancellationPreservesContextError(t *testing.T) {
	cfg, database := newDirectSyncFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var runErr error
	out := captureStdout(t, func() {
		_, runErr = runLocalSyncWithRebuild(
			ctx, cfg, database, true, nil,
			func() (agentsync.RebuildOptions, agentsync.RebuildCleanup, error) {
				return agentsync.RebuildOptions{Contributors: []agentsync.RebuildContributor{{
					Name: "http-box",
				}}}, nil, nil
			},
			func(bool, bool) error { return nil },
		)
	})

	assert.ErrorIs(t, runErr, context.Canceled)
	assert.NotErrorIs(t, runErr, errUnifiedRebuildAborted)
	assert.NotContains(t, out, "Sync complete")
}

func TestRunLocalSyncZeroContributorRetainsAbortFallback(t *testing.T) {
	dataDir := t.TempDir()
	cfg := config.Config{DataDir: dataDir, DBPath: filepath.Join(dataDir, "sessions.db")}
	database, err := db.Open(cfg.DBPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	missingPath := filepath.Join(dataDir, "missing.jsonl")
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "old-file-session", Agent: "claude", Machine: "local",
		Project: "preserved", FilePath: &missingPath, MessageCount: 1,
	}))

	didResync := runLocalSync(context.Background(), cfg, database, true)

	assert.True(t, didResync)
	preserved, getErr := database.GetSession(context.Background(), "old-file-session")
	require.NoError(t, getErr)
	assert.NotNil(t, preserved,
		"legacy zero-contributor fallback must keep the active archive")
}

func TestDoSyncConfiguredFullUnifiedHTTPUsesManifestDeltaAndOrderedProgress(
	t *testing.T,
) {
	cfg, database := newDirectSyncFixture(t)
	remoteRoot := filepath.Join(t.TempDir(), "remote-claude")
	require.NoError(t, os.MkdirAll(filepath.Join(remoteRoot, "project"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(remoteRoot, "project", "remote.jsonl"),
		[]byte(testjsonl.NewSessionBuilder().
			AddClaudeUser("2026-07-12T01:00:00Z", "remote direct sync").
			String()),
		0o600,
	))
	targets := remotesync.TargetSet{Dirs: map[parser.AgentType][]string{
		parser.AgentClaude: {remoteRoot},
	}}
	var archiveRequests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/remote-sync/targets":
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(targets); err != nil {
				http.Error(w, "encode targets", http.StatusInternalServerError)
			}
		case "/api/v1/remote-sync/manifest":
			var requested remotesync.TargetSet
			if err := json.NewDecoder(r.Body).Decode(&requested); err != nil {
				http.Error(w, "decode manifest request", http.StatusBadRequest)
				return
			}
			manifest, err := remotesync.BuildManifest(requested)
			if err != nil {
				http.Error(w, "build manifest", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(manifest); err != nil {
				http.Error(w, "encode manifest", http.StatusInternalServerError)
			}
		case "/api/v1/remote-sync/archive":
			archiveRequests.Add(1)
			var requested remotesync.ArchiveRequest
			if err := json.NewDecoder(r.Body).Decode(&requested); err != nil {
				http.Error(w, "decode archive request", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/x-tar")
			var err error
			if requested.DeltaFiles != nil {
				err = remotesync.WriteArchiveFiles(
					w, targets, requested.DeltaFiles,
				)
			} else {
				err = remotesync.WriteArchive(w, requested.TargetSet)
			}
			if err != nil {
				http.Error(w, "write archive", http.StatusInternalServerError)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(remote.Close)
	host := config.RemoteHost{
		Host: "http-box", Transport: config.RemoteTransportHTTP,
		URL: remote.URL, Token: "remote-token",
	}
	now := time.Date(2026, 7, 12, 1, 0, 0, 0, time.UTC)
	printer := newRemoteProgressPrinter(&bytes.Buffer{}, func() time.Time {
		now = now.Add(time.Millisecond)
		return now
	})
	var output bytes.Buffer
	printer.w = &output

	didResync, failures, err := runConfiguredLocalAndRemotes(
		context.Background(), cfg, database, []config.RemoteHost{host},
		true, printer.Print,
	)
	printer.Finish()
	require.NoError(t, err)
	assert.True(t, didResync)
	assert.Empty(t, failures)
	assert.Equal(t, int32(1), archiveRequests.Load())
	page, err := database.ListSessions(context.Background(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, page.Sessions, 2)

	progressOutput := output.String()
	labels := []string{
		"Downloading session archive from http-box",
		"Extracting session archive from http-box",
		"Processing sessions from http-box",
		"Rebuilding search index",
		"Swapping rebuilt database into place",
	}
	previous := -1
	for _, label := range labels {
		position := strings.Index(progressOutput, label)
		require.Greater(t, position, previous, "progress output: %s", progressOutput)
		previous = position
	}
	assert.Contains(t, progressOutput,
		"Swapping rebuilt database into place completed in")

	_, failures, err = runConfiguredLocalAndRemotes(
		context.Background(), cfg, database, []config.RemoteHost{host},
		true, nil,
	)
	require.NoError(t, err)
	assert.Empty(t, failures)
	assert.Equal(t, int32(1), archiveRequests.Load(),
		"unchanged full rebuild must not request a second archive")
}

func TestDoSyncIncrementalKeepsOrdinaryRemotePath(t *testing.T) {
	cfg, database := newDirectSyncFixture(t)
	host := config.RemoteHost{
		Host: "http-box", Transport: config.RemoteTransportHTTP,
		URL: "http://127.0.0.1:1", Token: "token",
	}
	prepareCalls := 0
	originalPrepare := prepareHTTPRebuildCLI
	prepareHTTPRebuildCLI = func(
		context.Context, []remotesync.HTTPSync,
	) (preparedHTTPRebuildCLI, error) {
		prepareCalls++
		return &fakeCLIPreparedHTTPRebuild{}, nil
	}
	t.Cleanup(func() { prepareHTTPRebuildCLI = originalPrepare })
	activeCalls := 0
	restore := stubHTTPRemoteSyncForTest(t, func(
		_ context.Context, got config.RemoteHost, full bool,
	) (remotesync.SyncStats, error) {
		activeCalls++
		assert.Equal(t, host, got)
		assert.False(t, full)
		return remotesync.SyncStats{}, nil
	})
	t.Cleanup(restore)

	didResync, failures, err := runConfiguredLocalAndRemotes(
		context.Background(), cfg, database, []config.RemoteHost{host}, false, nil,
	)

	require.NoError(t, err)
	assert.False(t, didResync)
	assert.Empty(t, failures)
	assert.Equal(t, 0, prepareCalls)
	assert.Equal(t, 1, activeCalls)
}

func TestDoSyncPreparationFailureReturnsRemoteFailureOutcome(t *testing.T) {
	env := newSyncCLIEnv(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	require.NoError(t, os.WriteFile(
		filepath.Join(env.DataDir, "config.toml"),
		[]byte(`[[remote_hosts]]
host = "http-box"
transport = "http"
url = "http://127.0.0.1:1"
token = "remote-token"
`),
		0o600,
	))
	originalPrepare := prepareHTTPRebuildCLI
	prepareHTTPRebuildCLI = func(
		context.Context, []remotesync.HTTPSync,
	) (preparedHTTPRebuildCLI, error) {
		return nil, &remotesync.HostError{
			Host: "http-box", Operation: "prepare", Err: errors.New("offline"),
		}
	}
	t.Cleanup(func() { prepareHTTPRebuildCLI = originalPrepare })

	hadRemoteFailures, err := doSync(SyncConfig{Full: true})

	require.NoError(t, err)
	assert.True(t, hadRemoteFailures)
}

func TestDoSyncContributorFailureReturnsRemoteFailureOutcome(t *testing.T) {
	env := newSyncCLIEnv(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	isolateDirectCLISources(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(env.DataDir, "config.toml"),
		[]byte(`[[remote_hosts]]
host = "http-box"
transport = "http"
url = "http://127.0.0.1:1"
token = "remote-token"
`),
		0o600,
	))
	cause := errors.New("persist remote cache")
	prepared := &fakeCLIPreparedHTTPRebuild{contributors: []agentsync.RebuildContributor{{
		Name:      "http-box",
		AfterSync: func(*agentsync.Engine, *db.DB) error { return cause },
	}}}
	originalPrepare := prepareHTTPRebuildCLI
	prepareHTTPRebuildCLI = func(
		context.Context, []remotesync.HTTPSync,
	) (preparedHTTPRebuildCLI, error) {
		return prepared, nil
	}
	t.Cleanup(func() { prepareHTTPRebuildCLI = originalPrepare })
	originalRegistry := httpRemoteCleanupRegistry
	httpRemoteCleanupRegistry = new(remotesync.CleanupRegistry)
	t.Cleanup(func() { httpRemoteCleanupRegistry = originalRegistry })

	hadRemoteFailures, err := doSync(SyncConfig{Full: true})

	require.NoError(t, err)
	assert.True(t, hadRemoteFailures)
	assert.Equal(t, 1, prepared.closed)
	assert.True(t, prepared.closeReleased)
}

func TestDoSyncAbortedUnifiedRebuildExitsNonZeroWithoutSuccessSummary(
	t *testing.T,
) {
	dataDir := t.TempDir()
	cmd := exec.Command(
		os.Args[0],
		"-test.run=^TestDoSyncAbortedUnifiedRebuildHelperProcess$",
	)
	cmd.Env = append(os.Environ(),
		"AGENTSVIEW_ABORTED_UNIFIED_HELPER=1",
		"AGENTSVIEW_NO_DAEMON=1",
		"AGENTSVIEW_DATA_DIR="+dataDir,
	)

	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 1, exitErr.ExitCode())
	assert.Contains(t, string(out), "fatal: local sync: "+errUnifiedRebuildAborted.Error())
	assert.NotContains(t, string(out), "Sync complete")
}

func TestDoSyncAbortedUnifiedRebuildHelperProcess(t *testing.T) {
	if os.Getenv("AGENTSVIEW_ABORTED_UNIFIED_HELPER") != "1" {
		return
	}
	dataDir := os.Getenv("AGENTSVIEW_DATA_DIR")
	require.NotEmpty(t, dataDir)
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "config.toml"),
		[]byte(`[[remote_hosts]]
host = "http-box"
transport = "http"
url = "http://127.0.0.1:1"
token = "remote-token"
`),
		0o600,
	))
	runConfiguredLocalAndRemotesCLI = func(
		context.Context, config.Config, *db.DB, []config.RemoteHost,
		bool, agentsync.ProgressFunc,
	) (bool, []remoteHostFailure, error) {
		return true, nil, errUnifiedRebuildAborted
	}
	doSync(SyncConfig{Full: true})
	os.Exit(0)
}

func TestDoSyncSingleHostFullStaysOnActiveArchivePath(t *testing.T) {
	newSyncCLIEnv(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	prepareCalls := 0
	originalPrepare := prepareHTTPRebuildCLI
	prepareHTTPRebuildCLI = func(
		context.Context, []remotesync.HTTPSync,
	) (preparedHTTPRebuildCLI, error) {
		prepareCalls++
		return nil, nil
	}
	t.Cleanup(func() { prepareHTTPRebuildCLI = originalPrepare })
	sshCalls := 0
	originalSSH := runSSHRemoteSync
	runSSHRemoteSync = func(
		_ context.Context, _ config.Config, _ *db.DB,
		rh config.RemoteHost, full bool,
	) (remotesync.SyncStats, error) {
		sshCalls++
		assert.Equal(t, "one-box", rh.Host)
		assert.True(t, full)
		return remotesync.SyncStats{}, nil
	}
	t.Cleanup(func() { runSSHRemoteSync = originalSSH })

	hadRemoteFailures, err := doSync(SyncConfig{Host: "one-box", Full: true})

	require.NoError(t, err)
	assert.False(t, hadRemoteFailures)
	assert.Equal(t, 0, prepareCalls)
	assert.Equal(t, 1, sshCalls)
}

func TestRunRemoteHosts_AttemptsAllAndCollectsFailures(t *testing.T) {
	hosts := []config.RemoteHost{
		{Host: "alpha"},
		{Host: "beta", User: "u", Port: 2222},
		{Host: "gamma"},
	}
	failBeta := errors.New("ssh down")

	var attempted []config.RemoteHost
	failures, blocked := runRemoteHosts(hosts, true, func(rh config.RemoteHost, full bool) error {
		attempted = append(attempted, rh)
		assert.True(t, full, "full flag should propagate to syncFn")
		if rh.Host == "beta" {
			return failBeta
		}
		return nil
	})
	require.NoError(t, blocked)

	// Every host attempted, in declared order, even after a failure.
	require.Equal(t, hosts, attempted)
	// Only beta failed; its full RemoteHost (user/port) is preserved.
	require.Len(t, failures, 1)
	assert.Equal(t, hosts[1], failures[0].Host)
	assert.Equal(t, failBeta, failures[0].Err)
}

func TestRunRemoteHosts_AllSucceedReturnsEmpty(t *testing.T) {
	hosts := []config.RemoteHost{{Host: "alpha"}, {Host: "beta"}}
	failures, blocked := runRemoteHosts(hosts, false, func(config.RemoteHost, bool) error {
		return nil
	})
	require.NoError(t, blocked)
	assert.Empty(t, failures)
}

func TestRunRemoteSyncOnceDispatchesHTTP(t *testing.T) {
	var called config.RemoteHost
	restore := stubHTTPRemoteSyncForTest(t, func(
		_ context.Context,
		rh config.RemoteHost,
		full bool,
	) (remotesync.SyncStats, error) {
		called = rh
		assert.True(t, full)
		return remotesync.SyncStats{SessionsSynced: 2}, nil
	})
	defer restore()

	err := runRemoteSyncOnce(config.Config{}, nil, config.RemoteHost{
		Host: "devbox", Transport: config.RemoteTransportHTTP,
		URL: "http://devbox:8080", Token: "remote-token",
	}, true)

	require.NoError(t, err)
	assert.Equal(t, "http://devbox:8080", called.URL)
}

func TestRunHTTPRemoteSyncRequiresExplicitHTTPToken(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	t.Cleanup(ts.Close)

	_, err := runHTTPRemoteSync(
		context.Background(),
		config.Config{AuthToken: "collector-token"},
		nil,
		config.RemoteHost{
			Host:      "devbox",
			Transport: config.RemoteTransportHTTP,
			URL:       ts.URL,
		},
		false,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "token is required")
	assert.False(t, called, "collector auth_token must not be sent to remote")
}

func stubHTTPRemoteSyncForTest(
	t *testing.T,
	fn func(context.Context, config.RemoteHost, bool) (remotesync.SyncStats, error),
) func() {
	t.Helper()
	orig := runHTTPRemoteSync
	runHTTPRemoteSync = func(
		ctx context.Context,
		_ config.Config,
		_ *db.DB,
		rh config.RemoteHost,
		full bool,
	) (remotesync.SyncStats, error) {
		return fn(ctx, rh, full)
	}
	return func() { runHTTPRemoteSync = orig }
}

func TestRunRemoteSyncTransportRetainsFailedHTTPCleanupUntilReleased(t *testing.T) {
	originalRegistry := httpRemoteCleanupRegistry
	httpRemoteCleanupRegistry = new(remotesync.CleanupRegistry)
	t.Cleanup(func() { httpRemoteCleanupRegistry = originalRegistry })

	owner := &syncTransportCleanupError{
		cause: errors.New("http sync failed"),
		results: []error{
			errors.New("cleanup still failed"),
			nil,
		},
	}
	runs := 0
	restore := stubHTTPRemoteSyncForTest(t, func(
		context.Context, config.RemoteHost, bool,
	) (remotesync.SyncStats, error) {
		runs++
		if runs == 1 {
			return remotesync.SyncStats{}, owner
		}
		return remotesync.SyncStats{SessionsSynced: 1}, nil
	})
	t.Cleanup(restore)
	rh := config.RemoteHost{Host: "alpha", Transport: config.RemoteTransportHTTP}

	_, err := runRemoteSyncTransport(
		context.Background(), config.Config{}, nil, rh, false,
	)
	require.Same(t, owner, err)
	assert.Equal(t, 1, owner.retries,
		"cleanup must be retried before the transport returns the error")
	assert.Equal(t, 1, runs)

	stats, err := runRemoteSyncTransport(
		context.Background(), config.Config{}, nil, rh, false,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	assert.Equal(t, 2, owner.retries)
	assert.Equal(t, 2, runs,
		"later HTTP work starts after retained cleanup releases")
}

func TestRunRemoteHostsStopsOnPendingHTTPCleanupWithoutMisattribution(t *testing.T) {
	originalRegistry := httpRemoteCleanupRegistry
	httpRemoteCleanupRegistry = new(remotesync.CleanupRegistry)
	t.Cleanup(func() { httpRemoteCleanupRegistry = originalRegistry })

	owner := &syncTransportCleanupError{
		cause: errors.New("alpha HTTP sync failed"),
		results: []error{
			errors.New("cleanup failed after alpha"),
			errors.New("cleanup still blocks beta"),
			errors.New("cleanup still blocks later call"),
			nil,
		},
	}
	var callbacks []string
	restore := stubHTTPRemoteSyncForTest(t, func(
		_ context.Context, rh config.RemoteHost, _ bool,
	) (remotesync.SyncStats, error) {
		callbacks = append(callbacks, rh.Host)
		if rh.Host == "alpha" {
			return remotesync.SyncStats{}, owner
		}
		return remotesync.SyncStats{SessionsSynced: 1}, nil
	})
	t.Cleanup(restore)
	run := func(rh config.RemoteHost, full bool) error {
		_, err := runRemoteSyncTransport(
			context.Background(), config.Config{}, nil, rh, full,
		)
		return err
	}
	httpHost := func(host string) config.RemoteHost {
		return config.RemoteHost{Host: host, Transport: config.RemoteTransportHTTP}
	}

	failures, blocked := runRemoteHosts([]config.RemoteHost{
		httpHost("alpha"), httpHost("beta"), httpHost("gamma"),
	}, false, run)
	require.Len(t, failures, 1)
	assert.Equal(t, "alpha", failures[0].Host.Host)
	assert.Same(t, owner, failures[0].Err)
	var pending *remotesync.PendingCleanupError
	require.ErrorAs(t, blocked, &pending)
	assert.ErrorIs(t, blocked, owner)
	assert.Equal(t, []string{"alpha"}, callbacks,
		"beta's callback is blocked and iteration stops before gamma")
	assert.Equal(t, 2, owner.retries)

	failures, blocked = runRemoteHosts(
		[]config.RemoteHost{httpHost("delta")}, false, run,
	)
	assert.Empty(t, failures)
	require.ErrorAs(t, blocked, &pending)
	assert.Equal(t, []string{"alpha"}, callbacks)
	assert.Equal(t, 3, owner.retries)

	failures, blocked = runRemoteHosts(
		[]config.RemoteHost{httpHost("epsilon")}, false, run,
	)
	assert.Empty(t, failures)
	require.NoError(t, blocked)
	assert.Equal(t, []string{"alpha", "epsilon"}, callbacks)
	assert.Equal(t, 4, owner.retries)
}

type syncTransportCleanupError struct {
	cause   error
	results []error
	retries int
}

func (e *syncTransportCleanupError) Error() string { return e.cause.Error() }

func (e *syncTransportCleanupError) Unwrap() error { return e.cause }

func (e *syncTransportCleanupError) RetryCleanup() error {
	result := e.results[e.retries]
	e.retries++
	return result
}

func TestSyncLocalAndRemotes_ResyncForcesRemoteFull(t *testing.T) {
	tests := []struct {
		name      string
		cfgFull   bool
		didResync bool
		wantFull  bool
	}{
		{"no full, no resync", false, false, false},
		{"automatic resync forces remote full", false, true, true},
		{"cli --full", true, false, true},
		{"both", true, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hosts := []config.RemoteHost{{Host: "alpha"}, {Host: "beta"}}
			localCalled := false
			var gotFull []bool
			failures, blocked := syncLocalAndRemotes(hosts, tt.cfgFull,
				func() bool { localCalled = true; return tt.didResync },
				func(_ config.RemoteHost, full bool) error {
					gotFull = append(gotFull, full)
					return nil
				})

			require.True(t, localCalled, "local sync must run")
			require.NoError(t, blocked)
			assert.Empty(t, failures)
			require.Len(t, gotFull, len(hosts))
			for _, full := range gotFull {
				assert.Equal(t, tt.wantFull, full)
			}
		})
	}
}

func TestUseDaemonForSync(t *testing.T) {
	tests := []struct {
		name     string
		readOnly bool
		want     bool
	}{
		{"skips read-only daemon", true, false},
		{"uses writable daemon", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			use := useDaemonForSync(transport{
				Mode:     transportHTTP,
				URL:      "http://127.0.0.1:8080",
				ReadOnly: tt.readOnly,
			})
			assert.Equal(t, tt.want, use)
		})
	}
}

func TestParseDaemonSyncSSEAllowsLargeDoneEvent(t *testing.T) {
	largeWarning := strings.Repeat("x", 70*1024)
	want := agentsync.SyncStats{
		TotalSessions: 12,
		Synced:        3,
		Warnings:      []string{largeWarning},
	}

	got, err := parseDaemonSyncSSE(doneSSE(t, want, true))
	require.NoError(t, err)
	assert.Equal(t, want.TotalSessions, got.TotalSessions)
	assert.Equal(t, want.Synced, got.Synced)
	require.Len(t, got.Warnings, 1)
	assert.Equal(t, largeWarning, got.Warnings[0])
}

func TestParseDaemonSyncSSEFlushesUnterminatedDoneEvent(t *testing.T) {
	want := agentsync.SyncStats{
		TotalSessions: 12,
		Synced:        3,
	}

	got, err := parseDaemonSyncSSE(doneSSE(t, want, false))
	require.NoError(t, err)
	assert.Equal(t, want.TotalSessions, got.TotalSessions)
	assert.Equal(t, want.Synced, got.Synced)
}

func TestParseDaemonSyncSSEReportsErrorEventPayload(t *testing.T) {
	_, err := parseDaemonSyncSSE(strings.NewReader(
		"event: error\n" +
			"data: remote sync failed\n" +
			"data: permission denied\n\n",
	))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "daemon sync error")
	assert.Contains(t, err.Error(), "remote sync failed\npermission denied")
}

func TestParseDaemonSyncSSEReportsProgressEvents(t *testing.T) {
	want := agentsync.SyncStats{
		TotalSessions: 12,
		Synced:        3,
	}
	var progress []agentsync.Progress

	got, err := parseDaemonSyncSSE(strings.NewReader(
		"event: progress\n"+
			"data: {\"phase\":\"rebuilding_search\",\"detail\":\"Rebuilding search index\",\"resync\":true}\n\n"+
			sseString(t, doneSSE(t, want, true)),
	), func(p agentsync.Progress) {
		progress = append(progress, p)
	})

	require.NoError(t, err)
	assert.Equal(t, want.Synced, got.Synced)
	require.Len(t, progress, 1)
	assert.Equal(t, agentsync.PhaseRebuildingSearch, progress[0].Phase)
	assert.Equal(t, "Rebuilding search index", progress[0].Detail)
	assert.True(t, progress[0].Resync)
}

func TestPrintSyncProgressClearsShorterOverwrites(t *testing.T) {
	out := captureStdout(t, func() {
		printSyncProgress(agentsync.Progress{
			Detail: "Rebuilding search index",
			Hint:   "Rebuilding the search index may take a while on large archives.",
		})
		printSyncProgress(agentsync.Progress{
			Detail: "Swapping rebuilt database into place",
		})
	})

	require.GreaterOrEqual(t, strings.Count(out, "\x1b[K"), 2,
		"each carriage-return progress line must clear stale text")
	assert.Contains(t, out, "\r  Swapping rebuilt database into place\x1b[K")
}

func TestResyncProgressPrinterWritesPhaseTimingsOnNewLines(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var out bytes.Buffer
	printer := newResyncProgressPrinter(&out, clock)

	printer.Print(agentsync.Progress{
		Phase:  agentsync.PhasePreparingResync,
		Detail: "Preparing full resync",
		Resync: true,
	})
	now = now.Add(150 * time.Millisecond)
	printer.Print(agentsync.Progress{
		Phase:           agentsync.PhaseSyncing,
		Detail:          "Syncing sessions into rebuilt database",
		SessionsTotal:   10,
		SessionsDone:    4,
		MessagesIndexed: 40,
		Resync:          true,
	})
	now = now.Add(2 * time.Second)
	printer.Print(agentsync.Progress{
		Phase:           agentsync.PhaseSyncing,
		Detail:          "Syncing sessions into rebuilt database",
		SessionsTotal:   10,
		SessionsDone:    10,
		MessagesIndexed: 100,
		Resync:          true,
	})
	now = now.Add(350 * time.Millisecond)
	printer.Print(agentsync.Progress{
		Phase:  agentsync.PhaseRebuildingSearch,
		Detail: "Rebuilding search index",
		Hint:   "Rebuilding the search index may take a while on large archives.",
		Resync: true,
	})
	now = now.Add(3 * time.Second)
	printer.Print(agentsync.Progress{
		Phase:  agentsync.PhaseSwappingDatabase,
		Detail: "Swapping rebuilt database into place",
		Resync: true,
	})

	got := out.String()
	assert.Contains(t, got, "  Preparing full resync...\n")
	assert.Contains(t, got, "  Preparing full resync completed in 150ms\n")
	assert.Contains(t, got, "\r  Syncing sessions into rebuilt database: 10/10 sessions (100%) · 100 messages\x1b[K")
	assert.Contains(t, got, "\n  Syncing sessions into rebuilt database completed in 2.35s\n")
	assert.Contains(t, got, "  Rebuilding search index - Rebuilding the search index may take a while on large archives...\n")
	assert.Contains(t, got, "  Rebuilding search index completed in 3s\n")
	assert.NotContains(t, got, "\r  Rebuilding search index",
		"non-session resync phases must not be overwritten in place")
}

func TestResyncProgressPrinterRendersDoneProgressBeforeCompletion(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var out bytes.Buffer
	printer := newResyncProgressPrinter(&out, clock)

	printer.Print(agentsync.Progress{
		Phase:           agentsync.PhaseSyncing,
		Detail:          "Syncing sessions into rebuilt database",
		SessionsTotal:   1,
		SessionsDone:    1,
		MessagesIndexed: 0,
		Resync:          true,
	})
	now = now.Add(time.Second)
	printer.Print(agentsync.Progress{
		Phase:           agentsync.PhaseDone,
		SessionsTotal:   1,
		SessionsDone:    1,
		MessagesIndexed: 42,
		Resync:          true,
	})

	got := out.String()
	assert.Contains(t, got,
		"\r  Syncing sessions into rebuilt database: 1/1 sessions (100%) · 42 messages\x1b[K")
	assert.Contains(t, got,
		"\n  Syncing sessions into rebuilt database completed in 1s\n")
}

func TestRemoteProgressPrinterWritesTimedStepLines(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var out bytes.Buffer
	printer := newRemoteProgressPrinter(&out, clock)

	printer.Print(agentsync.Progress{
		Detail: "Resolving agent directories on devbox",
	})
	now = now.Add(150 * time.Millisecond)
	printer.Print(agentsync.Progress{
		Detail: "Downloading session data from devbox (3 agents)",
	})
	now = now.Add(2 * time.Second)
	printer.Print(agentsync.Progress{
		Phase:           agentsync.PhaseSyncing,
		Detail:          "Processing sessions from devbox",
		SessionsTotal:   10,
		SessionsDone:    4,
		MessagesIndexed: 40,
	})
	now = now.Add(350 * time.Millisecond)
	printer.Print(agentsync.Progress{
		Phase:           agentsync.PhaseSyncing,
		Detail:          "Processing sessions from devbox",
		SessionsTotal:   10,
		SessionsDone:    10,
		MessagesIndexed: 100,
	})
	now = now.Add(3 * time.Second)
	printer.Print(agentsync.Progress{
		Detail: "Synced 10 sessions from devbox (1 unchanged)",
	})
	printer.Finish()

	got := out.String()
	assert.Contains(t, got, "  Resolving agent directories on devbox...\n")
	assert.Contains(t, got, "  Resolving agent directories on devbox completed in 150ms\n")
	assert.Contains(t, got, "  Downloading session data from devbox (3 agents)...\n")
	assert.Contains(t, got, "  Downloading session data from devbox (3 agents) completed in 2s\n")
	assert.Contains(t, got, "\r  Processing sessions from devbox: 10/10 sessions (100%) · 100 messages\x1b[K")
	assert.Contains(t, got, "\n  Processing sessions from devbox completed in 3.35s\n")
	assert.Contains(t, got, "  Synced 10 sessions from devbox (1 unchanged)\n")
	assert.True(t, strings.HasSuffix(got, "\n"), "remote progress should finish on a newline")
}

func TestRemoteProgressPrinterRendersByteProgressInPlace(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var out bytes.Buffer
	printer := newRemoteProgressPrinter(&out, clock)

	printer.Print(agentsync.Progress{
		Detail:     "Downloading session archive from devbox",
		BytesDone:  1 << 20,
		BytesTotal: 4 << 20,
	})
	now = now.Add(150 * time.Millisecond)
	printer.Print(agentsync.Progress{
		Detail:     "Downloading session archive from devbox",
		BytesDone:  4 << 20,
		BytesTotal: 4 << 20,
	})
	printer.Print(agentsync.Progress{
		Detail: "Extracting session archive from devbox",
	})
	now = now.Add(850 * time.Millisecond)
	printer.Finish()

	got := out.String()
	assert.Contains(t, got,
		"\r  Downloading session archive from devbox: 1.0 MB/4.0 MB (25%)\x1b[K")
	assert.Contains(t, got,
		"\r  Downloading session archive from devbox: 4.0 MB/4.0 MB (100%)\x1b[K")
	assert.Contains(t, got,
		"\n  Downloading session archive from devbox completed in 150ms\n")
	assert.Contains(t, got, "  Extracting session archive from devbox...\n")
	assert.Contains(t, got,
		"  Extracting session archive from devbox completed in 850ms\n")
}

func TestRemoteProgressPrinterRendersLocalSyncProgressWithoutDetail(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var out bytes.Buffer
	printer := newRemoteProgressPrinter(&out, clock)

	printer.Print(agentsync.Progress{
		Phase:           agentsync.PhaseSyncing,
		SessionsTotal:   10,
		SessionsDone:    4,
		MessagesIndexed: 40,
	})
	now = now.Add(250 * time.Millisecond)
	printer.Print(agentsync.Progress{
		Phase:           agentsync.PhaseDone,
		SessionsTotal:   10,
		SessionsDone:    10,
		MessagesIndexed: 100,
	})
	printer.Print(agentsync.Progress{
		Detail: "Resolving agent directories on devbox",
	})

	got := out.String()
	assert.Contains(t, got, "\r  Syncing local sessions: 4/10 sessions (40%) · 40 messages\x1b[K")
	assert.Contains(t, got, "\r  Syncing local sessions: 10/10 sessions (100%) · 100 messages\x1b[K")
	assert.Contains(t, got, "\n  Syncing local sessions completed in 250ms\n")
	assert.Contains(t, got, "  Resolving agent directories on devbox...\n")
}

func TestRemoteProgressPrinterKeepsResyncLabelOnDoneProgress(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var out bytes.Buffer
	printer := newRemoteProgressPrinter(&out, clock)

	printer.Print(agentsync.Progress{
		Phase:           agentsync.PhaseSyncing,
		Detail:          "Syncing sessions into rebuilt database",
		SessionsTotal:   10,
		SessionsDone:    4,
		MessagesIndexed: 40,
		Resync:          true,
	})
	now = now.Add(250 * time.Millisecond)
	printer.Print(agentsync.Progress{
		Phase:           agentsync.PhaseDone,
		SessionsTotal:   10,
		SessionsDone:    10,
		MessagesIndexed: 100,
		Resync:          true,
	})

	got := out.String()
	assert.Contains(t, got, "\r  Syncing sessions into rebuilt database: 10/10 sessions (100%) · 100 messages\x1b[K")
	assert.NotContains(t, got, "\r  Syncing local sessions: 10/10 sessions (100%) · 100 messages\x1b[K")
	assert.Contains(t, got, "\n  Syncing sessions into rebuilt database completed in 250ms\n")
}

func TestRunLocalSyncUsesCallerContextForResync(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "sessions.db")
	database, err := db.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	raw, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = raw.Exec("PRAGMA user_version = 0")
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	database, err = db.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })
	require.True(t, database.NeedsResync())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var didResync bool
	captureStdout(t, func() {
		didResync = runLocalSync(ctx, config.Config{
			DataDir: dataDir,
			DBPath:  dbPath,
		}, database, false)
	})

	assert.True(t, didResync)
	assert.True(t, database.NeedsResync())
}

func TestDoSyncUsesDaemonRouteWhenWritableDaemonRunning(t *testing.T) {
	env := newSyncCLIEnv(t)

	var syncCalled bool
	ts := syncRouteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/sync", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		syncCalled = true
		writeDoneSSE(t, w, agentsync.SyncStats{Synced: 7})
	})

	registerSyncRouteTestRuntime(t, env.DataDir, ts.URL)

	hadFailures, err := doSync(SyncConfig{})
	require.NoError(t, err)
	require.False(t, hadFailures)
	assert.True(t, syncCalled)
	env.assertNoLocalDB(t)
}

func TestDoSyncRoutesArtifactTargetThroughWritableDaemon(t *testing.T) {
	env := newSyncCLIEnv(t)
	target := t.TempDir()

	var syncCalled bool
	var exchangeCalled bool
	ts := daemonRouteTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/sync": func(w http.ResponseWriter, r *http.Request) {
			syncCalled = true
			writeDoneSSE(t, w, agentsync.SyncStats{Synced: 7})
		},
		"/api/v1/artifacts/exchange": func(w http.ResponseWriter, r *http.Request) {
			exchangeCalled = true
			var request struct {
				Target string `json:"target"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			assert.Equal(t, target, request.Target)
			w.Header().Set("Content-Type", "application/json")
			_, err := io.WriteString(w, `{"origin":"daemon-a1b2c3","exported_sessions":3}`)
			require.NoError(t, err)
		},
	})

	registerSyncRouteTestRuntime(t, env.DataDir, ts.URL)

	hadFailures, err := doSync(SyncConfig{ArtifactFolder: target})
	require.NoError(t, err)
	assert.False(t, hadFailures)
	assert.True(t, syncCalled)
	assert.True(t, exchangeCalled)
	env.assertNoLocalDB(t)
}

func TestSyncTransportArtifactTargetUsesDirectDBWithoutAutoStartingDaemon(t *testing.T) {
	env := newSyncCLIEnv(t)
	var autostartCalled bool
	stubStartBackgroundServeForTransport(t, func(
		context.Context, *config.Config, time.Duration,
	) (*DaemonRuntime, error) {
		autostartCalled = true
		return nil, errors.New("unexpected daemon autostart")
	})

	appCfg := config.Config{
		DataDir: env.DataDir,
	}
	tr, err := syncTransport(&appCfg, SyncConfig{
		ArtifactFolder: t.TempDir(),
	})
	require.NoError(t, err)
	assert.Equal(t, transportDirect, tr.Mode)
	assert.False(t, autostartCalled)
}

func TestDoSyncRefusesDirectArtifactOwnershipWhenDaemonIsUnreachable(t *testing.T) {
	env := newSyncCLIEnv(t)
	writeUnreachableDaemonRuntime(t, env.DataDir, false)

	hadFailures, err := doSync(SyncConfig{ArtifactFolder: t.TempDir()})

	require.Error(t, err)
	assert.False(t, hadFailures)
	assert.Contains(t, err.Error(), "daemon owns the SQLite archive")
	assert.Contains(t, err.Error(), "refusing to sync directly")
	env.assertNoLocalDB(t)
}

func TestDoSyncFullUsesDaemonResyncRoute(t *testing.T) {
	env := newSyncCLIEnv(t)

	var resyncCalled bool
	ts := syncRouteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/resync", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		resyncCalled = true
		w.Header().Set("Content-Type", "text/event-stream")
		_, err := io.WriteString(w,
			"event: progress\n"+
				"data: {\"phase\":\"rebuilding_search\",\"detail\":\"Rebuilding search index\",\"resync\":true}\n\n",
		)
		require.NoError(t, err)
		writeDoneSSE(t, w, agentsync.SyncStats{Synced: 9})
	})

	registerSyncRouteTestRuntime(t, env.DataDir, ts.URL)

	var hadFailures bool
	out := captureStdout(t, func() {
		var err error
		hadFailures, err = doSync(SyncConfig{Full: true})
		require.NoError(t, err)
	})
	require.False(t, hadFailures)
	assert.True(t, resyncCalled)
	assert.Contains(t, out, "Rebuilding search index")
	env.assertNoLocalDB(t)
}

func TestDoSyncPrintsStatusBeforeWaitingForDaemonStartup(t *testing.T) {
	env := newSyncCLIEnv(t)
	ts := syncRouteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/resync", r.URL.Path)
		writeDoneSSE(t, w, agentsync.SyncStats{})
	})
	endpoint := serverEndpoint(t, ts)
	startupEntered := make(chan struct{})
	releaseStartup := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseStartup:
		default:
			close(releaseStartup)
		}
	})
	stubStartBackgroundServeForTransport(t, func(
		context.Context, *config.Config, time.Duration,
	) (*DaemonRuntime, error) {
		close(startupEntered)
		<-releaseStartup
		return &DaemonRuntime{Host: endpoint.Host, Port: endpoint.Port}, nil
	})

	stdout := filepath.Join(t.TempDir(), "stdout")
	outFile, err := os.Create(stdout)
	require.NoError(t, err)
	oldStdout := os.Stdout
	os.Stdout = outFile
	t.Cleanup(func() {
		os.Stdout = oldStdout
		_ = outFile.Close()
	})

	type syncResult struct {
		hadFailures bool
		err         error
	}
	done := make(chan syncResult, 1)
	go func() {
		hadFailures, err := doSync(SyncConfig{Full: true})
		done <- syncResult{hadFailures: hadFailures, err: err}
	}()
	<-startupEntered
	require.NoError(t, outFile.Sync())
	output, err := os.ReadFile(stdout)
	require.NoError(t, err)
	assert.Contains(t, string(output), "Preparing full sync...")

	close(releaseStartup)
	result := <-done
	require.NoError(t, result.err)
	assert.False(t, result.hadFailures)
	require.NoError(t, outFile.Close())
	os.Stdout = oldStdout
	env.assertNoLocalDB(t)
}

func TestDoSyncFullSkipsRedundantDaemonInitialSync(t *testing.T) {
	env := newSyncCLIEnv(t)
	ts := syncRouteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/resync", r.URL.Path)
		writeDoneSSE(t, w, agentsync.SyncStats{})
	})
	endpoint := serverEndpoint(t, ts)
	var skipInitialSync bool
	stubStartBackgroundServeForTransport(t, func(
		_ context.Context, cfg *config.Config, _ time.Duration,
	) (*DaemonRuntime, error) {
		skipInitialSync = cfg.SkipInitialSync
		return &DaemonRuntime{Host: endpoint.Host, Port: endpoint.Port}, nil
	})

	hadFailures, err := doSync(SyncConfig{Full: true})

	require.NoError(t, err)
	assert.False(t, hadFailures)
	assert.True(t, skipInitialSync)
	env.assertNoLocalDB(t)
}

func TestDoSyncSkipsRedundantDaemonInitialSync(t *testing.T) {
	env := newSyncCLIEnv(t)
	ts := syncRouteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/sync", r.URL.Path)
		writeDoneSSE(t, w, agentsync.SyncStats{})
	})
	endpoint := serverEndpoint(t, ts)
	var skipInitialSync bool
	stubStartBackgroundServeForTransport(t, func(
		_ context.Context, cfg *config.Config, _ time.Duration,
	) (*DaemonRuntime, error) {
		skipInitialSync = cfg.SkipInitialSync
		return &DaemonRuntime{Host: endpoint.Host, Port: endpoint.Port}, nil
	})

	hadFailures, err := doSync(SyncConfig{})

	require.NoError(t, err)
	assert.False(t, hadFailures)
	assert.True(t, skipInitialSync)
	env.assertNoLocalDB(t)
}

func TestDoSyncRemoteHostKeepsDaemonInitialLocalSync(t *testing.T) {
	env := newSyncCLIEnv(t)
	got, handler := captureRemoteSyncRequest(t)
	ts := remoteSyncRouteTestServer(t, handler)
	endpoint := serverEndpoint(t, ts)
	var skipInitialSync bool
	stubStartBackgroundServeForTransport(t, func(
		_ context.Context, cfg *config.Config, _ time.Duration,
	) (*DaemonRuntime, error) {
		skipInitialSync = cfg.SkipInitialSync
		return &DaemonRuntime{Host: endpoint.Host, Port: endpoint.Port}, nil
	})

	hadFailures, err := doSync(SyncConfig{Host: "host-a.example"})

	require.NoError(t, err)
	assert.False(t, hadFailures)
	assert.False(t, skipInitialSync,
		"remote-only request needs the daemon startup local sync")
	assert.False(t, got.IncludeLocal,
		"the remote request must not duplicate the startup local sync")
	env.assertNoLocalDB(t)
}

func TestRunDaemonSyncTrimsBaseURLTrailingSlash(t *testing.T) {
	var syncCalled bool
	ts := syncRouteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/sync", r.URL.Path)
		require.Equal(t, strings.TrimSuffix(tsURL(t, r), "/"), r.Header.Get("Origin"))
		syncCalled = true
		writeDoneSSE(t, w, agentsync.SyncStats{Synced: 7})
	})

	stats, err := runDaemonSync(
		context.Background(),
		transport{URL: ts.URL + "/"},
		"",
		false,
		nil,
	)
	require.NoError(t, err)
	assert.True(t, syncCalled)
	assert.Equal(t, 7, stats.Synced)
}

func TestRunDaemonArtifactExchangeDoesNotFollowRedirects(t *testing.T) {
	var redirectedRequests atomic.Int32
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedRequests.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, receiver.URL+"/captured", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	_, err := runDaemonArtifactExchange(t.Context(), transport{
		Mode: transportHTTP, URL: redirector.URL,
		Runtime: &DaemonRuntime{Host: "127.0.0.1"},
	}, "daemon-secret", SyncConfig{ArtifactFolder: t.TempDir(), Token: "peer-secret"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 307")
	assert.Zero(t, redirectedRequests.Load(),
		"redirect receiver must get neither request body nor credentials")
}

func TestRunDaemonArtifactExchangeRejectsLANRuntimeBeforeRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	_, err := runDaemonArtifactExchange(t.Context(), transport{
		Mode: transportHTTP, URL: server.URL,
		Runtime: &DaemonRuntime{Host: "192.0.2.44"},
	}, "", SyncConfig{ArtifactFolder: t.TempDir()})
	require.ErrorContains(t, err, "loopback-bound writable daemon")
	assert.Zero(t, requests.Load(), "LAN refusal must happen before any HTTP request")
}

func TestRunDaemonArtifactExchangeRejectsSecretTargetWithoutDisclosure(t *testing.T) {
	const secret = "do-not-disclose"
	_, err := runDaemonArtifactExchange(t.Context(), transport{
		Mode: transportHTTP, URL: "http://127.0.0.1:1",
		Runtime: &DaemonRuntime{Host: "127.0.0.1"},
	}, "daemon-"+secret, SyncConfig{
		ArtifactFolder: "https://user:" + secret + "@example.invalid/archive?token=" + secret + "#" + secret,
		Token:          "peer-" + secret,
	})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
}

func TestDoSyncRemoteHostUsesDaemonRouteWhenWritableDaemonRunning(t *testing.T) {
	env := newSyncCLIEnv(t)

	got, handler := captureRemoteSyncRequest(t)
	ts := remoteSyncRouteTestServer(t, handler)
	registerSyncRouteTestRuntime(t, env.DataDir, ts.URL)

	hadFailures, err := doSync(SyncConfig{
		Host: "devbox",
		User: "alice",
		Port: 2222,
		Full: true,
	})

	require.NoError(t, err)
	require.False(t, hadFailures)
	assert.False(t, got.IncludeLocal)
	assert.True(t, got.Full)
	require.Len(t, got.Hosts, 1)
	assert.Equal(t, config.RemoteHost{
		Host: "devbox",
		User: "alice",
		Port: 2222,
	}, got.Hosts[0])
	env.assertNoLocalDB(t)
}

func TestDoSyncRemoteHostPrintsDaemonProgress(t *testing.T) {
	env := newSyncCLIEnv(t)

	ts := remoteSyncRouteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/sync/remotes", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		w.Header().Set("Content-Type", "text/event-stream")
		_, err := io.WriteString(w,
			"event: progress\n"+
				"data: {\"detail\":\"Resolving agent directories on devbox\"}\n\n"+
				"event: done\n"+
				"data: {\"failures\":[]}\n\n",
		)
		require.NoError(t, err)
	})
	registerSyncRouteTestRuntime(t, env.DataDir, ts.URL)

	var hadFailures bool
	var err error
	out := captureStdout(t, func() {
		hadFailures, err = doSync(SyncConfig{Host: "devbox"})
	})

	require.NoError(t, err)
	require.False(t, hadFailures)
	assert.Contains(t, out, "Running sync with remotes via daemon...")
	assert.Contains(t, out, "Resolving agent directories on devbox")
	assert.True(t, strings.HasSuffix(out, "\n"), "progress output should finish on a newline")
	env.assertNoLocalDB(t)
}

func TestRunDaemonRemoteSyncTrimsBaseURLTrailingSlash(t *testing.T) {
	ts := remoteSyncRouteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/sync/remotes", r.URL.Path)
		require.Equal(t, strings.TrimSuffix(tsURL(t, r), "/"), r.Header.Get("Origin"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"failures":[]}`)
	})

	failures, err := runDaemonRemoteSync(
		context.Background(),
		transport{URL: ts.URL + "/"},
		"",
		[]config.RemoteHost{{Host: "devbox"}},
		false,
		false,
		nil,
	)
	require.NoError(t, err)
	assert.Empty(t, failures)
}

func TestRunDaemonRemoteSyncReportsProgressEvents(t *testing.T) {
	ts := remoteSyncRouteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/sync/remotes", r.URL.Path)
		assert.Contains(t, r.Header.Get("Accept"), "text/event-stream")
		w.Header().Set("Content-Type", "text/event-stream")
		_, err := io.WriteString(w,
			"event: progress\n"+
				"data: {\"phase\":\"rebuilding_search\",\"detail\":\"Rebuilding search index\",\"resync\":true}\n\n"+
				"event: done\n"+
				"data: {\"failures\":[]}\n\n",
		)
		require.NoError(t, err)
	})
	var progress []agentsync.Progress

	failures, err := runDaemonRemoteSync(
		context.Background(),
		transport{URL: ts.URL},
		"",
		[]config.RemoteHost{{Host: "devbox"}},
		false,
		true,
		func(p agentsync.Progress) {
			progress = append(progress, p)
		},
	)

	require.NoError(t, err)
	assert.Empty(t, failures)
	require.Len(t, progress, 1)
	assert.Equal(t, agentsync.PhaseRebuildingSearch, progress[0].Phase)
}

func TestRunDaemonRemoteSyncJSONReturnsTopLevelErrorWithEarlierFailures(t *testing.T) {
	const blocked = "HTTP remote sync failed: pending cleanup still owns resources"
	ts := remoteSyncRouteTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `{
			"failures":[{"host":{"host":"alpha"},"error":"alpha failed"}],
			"error":"`+blocked+`"
		}`)
		require.NoError(t, err)
	})

	failures, err := runDaemonRemoteSync(
		context.Background(), transport{URL: ts.URL}, "",
		[]config.RemoteHost{{Host: "alpha"}, {Host: "beta"}},
		false, false, nil,
	)

	require.EqualError(t, err, blocked)
	require.Len(t, failures, 1)
	assert.Equal(t, "alpha", failures[0].Host.Host)
	assert.EqualError(t, failures[0].Err, "alpha failed")
}

func TestRunDaemonRemoteSyncJSONRejectsAbortedUnifiedRebuild(t *testing.T) {
	ts := remoteSyncRouteTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `{
			"local_stats":{"aborted":true},
			"failures":[],
			"error":"`+agentsync.ErrUnifiedRebuildAborted.Error()+`"
		}`)
		require.NoError(t, err)
	})

	failures, err := runDaemonRemoteSync(
		context.Background(), transport{URL: ts.URL}, "",
		[]config.RemoteHost{{Host: "alpha"}}, true, true, nil,
	)

	require.ErrorIs(t, err, agentsync.ErrUnifiedRebuildAborted)
	assert.Empty(t, failures)
}

func TestRunDaemonRemoteSyncSSEReturnsTopLevelErrorWithEarlierFailures(t *testing.T) {
	const blocked = "HTTP remote sync failed: pending cleanup still owns resources"
	ts := remoteSyncRouteTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, err := io.WriteString(w,
			"event: done\n"+
				`data: {"failures":[{"host":{"host":"alpha"},"error":"alpha failed"}],"error":"`+blocked+`"}`+
				"\n\n",
		)
		require.NoError(t, err)
	})

	failures, err := runDaemonRemoteSync(
		context.Background(), transport{URL: ts.URL}, "",
		[]config.RemoteHost{{Host: "alpha"}, {Host: "beta"}},
		false, false, nil,
	)

	require.EqualError(t, err, blocked)
	require.Len(t, failures, 1)
	assert.Equal(t, "alpha", failures[0].Host.Host)
	assert.EqualError(t, failures[0].Err, "alpha failed")
}

func TestRunDaemonRemoteSyncSSERejectsAbortedUnifiedRebuild(t *testing.T) {
	ts := remoteSyncRouteTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, err := io.WriteString(w,
			"event: done\n"+
				`data: {"local_stats":{"aborted":true},"failures":[],"error":"`+
				agentsync.ErrUnifiedRebuildAborted.Error()+`"}`+
				"\n\n",
		)
		require.NoError(t, err)
	})

	failures, err := runDaemonRemoteSync(
		context.Background(), transport{URL: ts.URL}, "",
		[]config.RemoteHost{{Host: "alpha"}}, true, true, nil,
	)

	require.ErrorIs(t, err, agentsync.ErrUnifiedRebuildAborted)
	assert.Empty(t, failures)
}

func TestDoSyncDaemonAbortedUnifiedRebuildExitsNonZero(t *testing.T) {
	dataDir := t.TempDir()
	cmd := exec.Command(
		os.Args[0],
		"-test.run=^TestDoSyncDaemonAbortedUnifiedRebuildHelperProcess$",
	)
	cmd.Env = append(os.Environ(),
		"AGENTSVIEW_DAEMON_ABORTED_UNIFIED_HELPER=1",
		"AGENTSVIEW_DATA_DIR="+dataDir,
	)

	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 1, exitErr.ExitCode())
	assert.Contains(t, string(out),
		"fatal: daemon remote sync: "+agentsync.ErrUnifiedRebuildAborted.Error())
	assert.NotContains(t, string(out), "Sync complete")
}

func TestDoSyncDaemonAbortedUnifiedRebuildHelperProcess(t *testing.T) {
	if os.Getenv("AGENTSVIEW_DAEMON_ABORTED_UNIFIED_HELPER") != "1" {
		return
	}
	dataDir := os.Getenv("AGENTSVIEW_DATA_DIR")
	require.NotEmpty(t, dataDir)
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "config.toml"),
		[]byte(`[[remote_hosts]]
host = "alpha"
transport = "http"
url = "http://127.0.0.1:1"
token = "remote-token"
`),
		0o600,
	))
	ts := remoteSyncRouteTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, err := io.WriteString(w,
			"event: done\n"+
				`data: {"local_stats":{"aborted":true},"failures":[],"error":"`+
				agentsync.ErrUnifiedRebuildAborted.Error()+`"}`+
				"\n\n",
		)
		require.NoError(t, err)
	})
	registerSyncRouteTestRuntime(t, dataDir, ts.URL)

	doSync(SyncConfig{Full: true})
	os.Exit(0)
}

func TestDoSyncConfiguredRemoteHostsUsesDaemonRouteWithLocalSync(
	t *testing.T,
) {
	env := newSyncCLIEnv(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(env.DataDir, "config.toml"),
		[]byte(`[[remote_hosts]]
host = "alpha"
user = "robot"
`),
		0o600,
	))

	got, handler := captureRemoteSyncRequest(t)
	ts := remoteSyncRouteTestServer(t, handler)
	registerSyncRouteTestRuntime(t, env.DataDir, ts.URL)

	hadFailures, err := doSync(SyncConfig{})

	require.NoError(t, err)
	require.False(t, hadFailures)
	assert.True(t, got.IncludeLocal)
	require.Len(t, got.Hosts, 1)
	assert.Equal(t, "alpha", got.Hosts[0].Host)
	assert.Equal(t, "robot", got.Hosts[0].User)
	env.assertNoLocalDB(t)
}

// syncCLIEnv is a daemon-backed CLI test environment: an isolated data dir
// exported via AGENTSVIEW_DATA_DIR with the global log writer restored on
// cleanup.
type syncCLIEnv struct {
	DataDir string
	DBPath  string
}

func newSyncCLIEnv(t *testing.T) syncCLIEnv {
	t.Helper()
	dataDir := testDataDir(t)
	restoreTestLogOutput(t)
	return syncCLIEnv{
		DataDir: dataDir,
		DBPath:  filepath.Join(dataDir, "sessions.db"),
	}
}

// assertNoLocalDB verifies the CLI deferred to the daemon instead of opening a
// local SQLite archive.
func (e syncCLIEnv) assertNoLocalDB(t *testing.T) {
	t.Helper()
	assert.NoFileExists(t, e.DBPath)
}

// remoteSyncRequest mirrors the JSON body the CLI POSTs to the daemon's
// /api/v1/sync/remotes route.
type remoteSyncRequest struct {
	Full         bool                `json:"full"`
	IncludeLocal bool                `json:"include_local"`
	Hosts        []config.RemoteHost `json:"hosts"`
}

// captureRemoteSyncRequest returns a handler that records the decoded remote
// sync request into the returned struct and replies with no failures.
func captureRemoteSyncRequest(t *testing.T) (*remoteSyncRequest, http.HandlerFunc) {
	t.Helper()
	got := &remoteSyncRequest{}
	return got, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.NoError(t, json.NewDecoder(r.Body).Decode(got))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"failures":[]}`)
	}
}

// doneSSE renders stats as a daemon sync "done" SSE event. When terminated is
// false the trailing blank line is omitted to exercise flush-on-EOF parsing.
func doneSSE(t *testing.T, stats agentsync.SyncStats, terminated bool) io.Reader {
	t.Helper()
	payload, err := json.Marshal(stats)
	require.NoError(t, err)
	suffix := "\n\n"
	if !terminated {
		suffix = "\n"
	}
	return strings.NewReader("event: done\ndata: " + string(payload) + suffix)
}

func sseString(t *testing.T, r io.Reader) string {
	t.Helper()
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(data)
}

// writeDoneSSE writes a terminated daemon sync "done" SSE event to w.
func writeDoneSSE(t *testing.T, w io.Writer, stats agentsync.SyncStats) {
	t.Helper()
	_, err := io.Copy(w, doneSSE(t, stats, true))
	require.NoError(t, err)
}

// daemonRouteTestServer starts an httptest server that answers daemon ping
// probes and dispatches the given routes by exact path.
func daemonRouteTestServer(
	t *testing.T,
	routes map[string]http.HandlerFunc,
) *httptest.Server {
	t.Helper()
	ping := daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: "test",
	})
	ts := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		if r.URL.Path == "/api/ping" {
			ping.ServeHTTP(w, r)
			return
		}
		if h, ok := routes[r.URL.Path]; ok {
			h(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func syncRouteTestServer(
	t *testing.T,
	syncHandler http.HandlerFunc,
) *httptest.Server {
	t.Helper()
	return daemonRouteTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/sync":   syncHandler,
		"/api/v1/resync": syncHandler,
	})
}

func remoteSyncRouteTestServer(
	t *testing.T,
	remoteHandler http.HandlerFunc,
) *httptest.Server {
	t.Helper()
	return daemonRouteTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/sync/remotes": remoteHandler,
	})
}

func registerSyncRouteTestRuntime(
	t *testing.T,
	dataDir string,
	rawURL string,
) {
	registerTestRuntime(t, dataDir, rawURL, false)
}

func registerTestRuntime(
	t *testing.T,
	dataDir string,
	rawURL string,
	readOnly bool,
) {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	host, portText, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	_, err = WriteDaemonRuntime(dataDir, host, port, "test", readOnly)
	require.NoError(t, err)
}

func tsURL(t *testing.T, r *http.Request) string {
	t.Helper()
	return "http://" + r.Host
}

func TestRemoteFailureDisplaySanitizesHTTPErrors(t *testing.T) {
	tests := []struct {
		name       string
		failure    remoteHostFailure
		want       string
		wantAbsent []string
	}{
		{
			name: "http status failure uses sanitized summary",
			failure: remoteHostFailure{
				Host: config.RemoteHost{
					Host:      "devbox",
					Transport: config.RemoteTransportHTTP,
				},
				Err: &remotesync.StatusError{
					Code:   401,
					Status: "401 Unauthorized",
					Detail: "bearer secret-token-123 rejected",
				},
			},
			want: "HTTP remote sync failed: remote daemon rejected " +
				"the sync token (401 Unauthorized); the token for " +
				"this host in [[remote_hosts]] must match the remote " +
				"daemon's auth_token",
			wantAbsent: []string{"secret-token-123"},
		},
		{
			name: "http transport collapses unknown raw errors",
			failure: remoteHostFailure{
				Host: config.RemoteHost{
					Host:      "devbox",
					Transport: config.RemoteTransportHTTP,
				},
				Err: errors.New(
					`Get "http://devbox.tailnet.ts.net:8080": token=abc rejected`,
				),
			},
			want:       "HTTP remote sync failed",
			wantAbsent: []string{"tailnet.ts.net", "token=abc"},
		},
		{
			name: "ssh transport keeps the raw error",
			failure: remoteHostFailure{
				Host: config.RemoteHost{Host: "buildbox"},
				Err:  errors.New("ssh: permission denied"),
			},
			want: "ssh: permission denied",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := remoteFailureDisplay(tt.failure)
			assert.Equal(t, tt.want, got)
			for _, absent := range tt.wantAbsent {
				assert.NotContains(t, got, absent)
			}
		})
	}
}
func TestRunHTTPRemoteSyncReachesMirrorPath(t *testing.T) {
	manifestRequests := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/remote-sync/targets":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		case "/api/v1/remote-sync/manifest":
			manifestRequests++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"files":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	database := dbtest.OpenTestDB(t)

	_, err := runHTTPRemoteSync(
		context.Background(),
		config.Config{DataDir: t.TempDir()},
		database,
		config.RemoteHost{
			Host:      "devbox",
			Transport: config.RemoteTransportHTTP,
			URL:       ts.URL,
			Token:     "remote-token",
		},
		false,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, manifestRequests,
		"configured DataDir must route HTTP sync through the manifest/mirror path")
}

func TestNewSyncCommandRegistersArtifactFolderFlag(t *testing.T) {
	cmd := newSyncCommand()
	flag := cmd.Flags().Lookup("artifact-folder")
	require.NotNil(t, flag)
	assert.Equal(t, "", flag.DefValue)
	assert.Contains(t, flag.Usage, "local-first sync artifacts")
	initFlag := cmd.Flags().Lookup("init")
	require.NotNil(t, initFlag)
	assert.Equal(t, "false", initFlag.DefValue)
	assert.Contains(t, initFlag.Usage, "Initialize artifact sync")
	assert.Contains(t, cmd.Long, "do not point this at the")
	assert.Contains(t, cmd.Long, "Use --init with an artifact folder")
	watchFlag := cmd.Flags().Lookup("watch")
	require.NotNil(t, watchFlag)
	assert.Equal(t, "false", watchFlag.DefValue)
	assert.Contains(t, watchFlag.Usage, "Run artifact folder sync continuously")
	assert.Equal(t, defaultWatchDebounce.String(), cmd.Flags().Lookup("debounce").DefValue)
	assert.Equal(t, defaultWatchInterval.String(), cmd.Flags().Lookup("interval").DefValue)
	assert.Contains(t, cmd.Long, "Use --watch with an artifact folder")
}

func TestApplySyncArtifactTargetUsesPositionalArgument(t *testing.T) {
	cfg := SyncConfig{}
	err := applySyncArtifactTarget(&cfg, []string{"/tmp/agentsview-share"}, false)
	require.NoError(t, err)
	assert.Equal(t, "/tmp/agentsview-share", cfg.ArtifactFolder)
}

func TestApplySyncArtifactTargetRejectsArgumentAndFlag(t *testing.T) {
	cfg := SyncConfig{ArtifactFolder: "/tmp/from-flag"}
	err := applySyncArtifactTarget(&cfg, []string{"/tmp/from-arg"}, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "both as an argument")
	assert.Equal(t, "/tmp/from-flag", cfg.ArtifactFolder)
}

func TestValidateSyncConfigInitRequiresArtifactFolder(t *testing.T) {
	err := validateSyncConfig(SyncConfig{Init: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--init requires")
}

func TestValidateSyncConfigInitRejectsHost(t *testing.T) {
	err := validateSyncConfig(SyncConfig{
		Init:           true,
		Host:           "remote",
		ArtifactFolder: "/tmp/agentsview-share",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be combined with --host")
}

func TestValidateSyncConfigInitAllowsArtifactFolder(t *testing.T) {
	err := validateSyncConfig(SyncConfig{
		Init:           true,
		ArtifactFolder: "/tmp/agentsview-share",
	})
	require.NoError(t, err)
}

func TestValidateSyncConfigWatchRequiresArtifactFolder(t *testing.T) {
	err := validateSyncConfig(SyncConfig{Watch: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--watch requires")
}

func TestValidateSyncConfigWatchRejectsHost(t *testing.T) {
	err := validateSyncConfig(SyncConfig{
		Watch:          true,
		Host:           "remote",
		ArtifactFolder: "/tmp/agentsview-share",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be combined with --host")
}

func TestValidateSyncConfigWatchAllowsArtifactFolder(t *testing.T) {
	err := validateSyncConfig(SyncConfig{
		Watch:          true,
		ArtifactFolder: "/tmp/agentsview-share",
		Debounce:       time.Second,
		Interval:       time.Minute,
	})
	require.NoError(t, err)
}

func TestValidateSyncConfigRejectsHostWithArtifactTarget(t *testing.T) {
	err := validateSyncConfig(SyncConfig{
		Host:           "remote",
		ArtifactFolder: "/tmp/agentsview-share",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--host cannot be combined with an artifact target")
}

func TestValidateSyncConfigAllowsHostWithoutArtifactTarget(t *testing.T) {
	require.NoError(t, validateSyncConfig(SyncConfig{Host: "remote"}))
}

func TestArtifactPeerTokenDoesNotReuseLocalAuthToken(t *testing.T) {
	got := artifactPeerToken(SyncConfig{
		ArtifactFolder: "https://peer.example.test",
	})
	assert.Empty(t, got)

	got = artifactPeerToken(SyncConfig{
		ArtifactFolder: "https://peer.example.test",
		Token:          "peer-secret",
	})
	assert.Equal(t, "peer-secret", got)
}

func TestSyncArtifactFolderPlumbsInsecurePeerOptIn(t *testing.T) {
	target, requests := insecureArtifactPeerTarget(t)
	dataDir := t.TempDir()
	database, err := db.Open(filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })

	_, err = syncArtifactFolder(
		context.Background(), config.Config{DataDir: dataDir}, database,
		target, "desk-a1b2c3", "", true, false, nil,
	)

	require.NoError(t, err)
	assert.Positive(t, requests.Load(),
		"one-shot sync must reach an explicitly allowed plaintext peer")
}

func insecureArtifactPeerTarget(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	var host net.IP
	addrs, err := net.InterfaceAddrs()
	require.NoError(t, err)
	for _, addr := range addrs {
		ip, _, parseErr := net.ParseCIDR(addr.String())
		if parseErr == nil && ip.To4() != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			host = ip
			break
		}
	}
	if host == nil {
		t.Skip("no non-loopback IPv4 interface available for plaintext peer plumbing test")
	}
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	require.NoError(t, err)
	var requests atomic.Int32
	peer := &httptest.Server{
		Listener: listener,
		Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/origins"):
				_, _ = io.WriteString(w, `{"origins":[]}`)
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/index"):
				_, _ = io.WriteString(w, `{"origin":"desk-a1b2c3"}`)
			case r.Method == http.MethodPost:
				w.WriteHeader(http.StatusCreated)
			default:
				http.NotFound(w, r)
			}
		})},
	}
	peer.Start()
	t.Cleanup(peer.Close)
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NO_PROXY", "*")
	port := listener.Addr().(*net.TCPAddr).Port
	return "http://" + net.JoinHostPort(host.String(), strconv.Itoa(port)), &requests
}

func TestSyncGCHelpDocumentsMaintenanceFlags(t *testing.T) {
	help, err := executeCommand(newRootCommand(), "sync", "gc", "--help")
	require.NoError(t, err)
	for _, want := range []string{
		"Retain logical artifacts", "--grace", "--quarantine-grace",
		"--max-objects", "--max-bytes", "--dry-run",
	} {
		assert.Contains(t, help, want)
	}
}

func TestSyncGCRejectsExternalFolderTarget(t *testing.T) {
	_, err := executeCommand(newRootCommand(), "sync", "gc", "https://example.test/artifacts")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown command")
}

func TestSyncGCDryRunPrintsSummary(t *testing.T) {
	t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
	out, err := executeCommand(newRootCommand(), "sync", "gc", "--dry-run")
	require.NoError(t, err)
	assert.Contains(t, out, "Artifact retention:")
	assert.Contains(t, out, "would trash 0 artifact(s)")
}

func TestArtifactFolderPusherPushExportsToTarget(t *testing.T) {
	dataDir := t.TempDir()
	target := t.TempDir()
	database, err := db.Open(filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	dbtest.SeedSession(t, database, "sess-1", "alpha", func(s *db.Session) {
		s.MessageCount = 1
		s.UserMessageCount = 1
	})
	require.NoError(t, database.ReplaceSessionMessages("sess-1", []db.Message{
		{SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
	}))

	pusher := &artifactFolderPusher{
		appCfg:   config.Config{DataDir: dataDir},
		database: database,
		target:   target,
		origin:   "desk-a1b2c3",
	}
	require.NoError(t, pusher.push(context.Background(), reasonChange))

	checkpoints, err := filepath.Glob(
		filepath.Join(target, "desk-a1b2c3", "checkpoints", "*.json"),
	)
	require.NoError(t, err)
	require.Len(t, checkpoints, 1)
	manifests, err := filepath.Glob(
		filepath.Join(target, "desk-a1b2c3", "manifests", "*.json.zst"),
	)
	require.NoError(t, err)
	assert.Len(t, manifests, 1)
}
