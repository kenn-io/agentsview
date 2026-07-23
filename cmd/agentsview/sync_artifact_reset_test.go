package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func TestArtifactResetCommandIsExplicitAndTakesNoTarget(t *testing.T) {
	cmd := newSyncArtifactResetCommand()
	assert.Equal(t, "artifact-reset", cmd.Use)
	require.Error(t, cmd.Args(cmd, []string{"some-vault"}))
}

func TestArtifactResetCLIUsesAuthenticatedDaemonRoute(t *testing.T) {
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/artifacts/reset", r.URL.Path)
		assert.Equal(t, "Bearer daemon-secret", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(syncArtifactResetResponse{
			RepositoryResetResult: artifact.RepositoryResetResult{
				VaultRoot: "/tmp/fresh", DiagnosticRoot: "/tmp/moved",
				Export: artifact.ExportResult{ExportedSessions: 3, CheckpointSequence: 7},
			},
			ManualCleanup:    artifact.ArtifactResetManualCleanupWarning,
			ForeignArtifacts: artifact.ArtifactResetForeignRelayWarning,
		}))
	}))
	defer server.Close()
	cmd := &cobra.Command{}
	var output bytes.Buffer
	cmd.SetOut(&output)
	direct := false

	err := runSyncArtifactResetWith(cmd, config.Config{
		DataDir: t.TempDir(), AuthToken: "daemon-secret",
	}, syncArtifactResetDependencies{
		findDaemon: func(string, ...string) *DaemonRuntime {
			return daemonRuntimeFromTestURL(t, server.URL)
		},
		localDaemonActive: func(string, ...string) bool { return true },
		openDirect: func(context.Context, config.Config) (*db.DB, func(), error) {
			direct = true
			return nil, nil, errors.New("direct reset must not run")
		},
	})

	require.NoError(t, err)
	assert.True(t, called)
	assert.False(t, direct)
	assert.Contains(t, output.String(), "/tmp/moved")
	assert.Contains(t, output.String(), artifact.ArtifactResetManualCleanupWarning)
	assert.Contains(t, output.String(), artifact.ArtifactResetForeignRelayWarning)
}

func TestArtifactResetCLINeverFallsBackFromDaemonOwner(t *testing.T) {
	for _, tt := range []struct {
		name    string
		runtime *DaemonRuntime
		active  bool
	}{
		{name: "read only daemon", runtime: &DaemonRuntime{ReadOnly: true}},
		{name: "unreachable writable daemon", active: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			direct := false
			cmd := &cobra.Command{}
			err := runSyncArtifactResetWith(cmd, config.Config{DataDir: t.TempDir()},
				syncArtifactResetDependencies{
					findDaemon:        func(string, ...string) *DaemonRuntime { return tt.runtime },
					localDaemonActive: func(string, ...string) bool { return tt.active },
					openDirect: func(context.Context, config.Config) (*db.DB, func(), error) {
						direct = true
						return nil, nil, errors.New("unexpected direct reset")
					},
				})
			require.Error(t, err)
			assert.False(t, direct)
		})
	}
}

func TestArtifactResetCLIDirectModeUsesSQLiteWriteOwnership(t *testing.T) {
	dataDir := t.TempDir()
	cfg := config.Config{DataDir: dataDir, DBPath: filepath.Join(dataDir, "sessions.db")}
	database, err := db.Open(cfg.DBPath)
	require.NoError(t, err)
	require.NoError(t, artifact.AdoptOrigin(database, "desktop-d4e5f6"))
	startedAt := "2026-06-14T01:02:03Z"
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "local-session", Machine: "local", Agent: "codex", Project: "project-a",
		StartedAt: &startedAt, CreatedAt: startedAt,
	}))
	database.Close()
	repository, err := artifact.OpenRepository(t.Context(), dataDir)
	require.NoError(t, err)
	require.NoError(t, repository.Close())
	cmd := &cobra.Command{}
	var output bytes.Buffer
	cmd.SetOut(&output)

	err = runSyncArtifactResetWith(cmd, cfg, syncArtifactResetDependencies{
		findDaemon:        func(string, ...string) *DaemonRuntime { return nil },
		localDaemonActive: func(string, ...string) bool { return false },
		openDirect:        openArtifactResetDirect,
		resetRepository:   artifact.ResetRepository,
	})

	require.NoError(t, err)
	assert.Contains(t, output.String(), "Artifact vault reset complete")
	assert.Contains(t, output.String(), artifact.ArtifactResetManualCleanupWarning)
	assert.Contains(t, output.String(), artifact.ArtifactResetForeignRelayWarning)
	assert.DirExists(t, filepath.Join(dataDir, "artifacts"))
	matches, err := filepath.Glob(filepath.Join(dataDir, "artifacts.reset-*"))
	require.NoError(t, err)
	assert.Len(t, matches, 1)
	reopened, err := artifact.OpenRepository(t.Context(), dataDir)
	require.NoError(t, err)
	originIterator, err := reopened.Content().Origins(t.Context())
	require.NoError(t, err)
	origins, nextErr := originIterator.Next(t.Context(), 10)
	require.ErrorIs(t, nextErr, io.EOF)
	require.NoError(t, originIterator.Close())
	assert.Equal(t, []string{"desktop-d4e5f6"}, origins)
	require.NoError(t, reopened.Close())
}

func TestArtifactResetCLIDirectLockFailureDoesNotTouchVault(t *testing.T) {
	lockErr := errors.New("SQLite write ownership is held")
	resetCalled := false
	cmd := &cobra.Command{}
	err := runSyncArtifactResetWith(cmd, config.Config{DataDir: t.TempDir()},
		syncArtifactResetDependencies{
			findDaemon:        func(string, ...string) *DaemonRuntime { return nil },
			localDaemonActive: func(string, ...string) bool { return false },
			openDirect: func(context.Context, config.Config) (*db.DB, func(), error) {
				return nil, nil, lockErr
			},
			resetRepository: func(context.Context, string, *db.DB, string, *artifact.Repository) (*artifact.Repository, artifact.RepositoryResetResult, error) {
				resetCalled = true
				return nil, artifact.RepositoryResetResult{}, nil
			},
		})

	require.ErrorIs(t, err, lockErr)
	assert.False(t, resetCalled)
}
